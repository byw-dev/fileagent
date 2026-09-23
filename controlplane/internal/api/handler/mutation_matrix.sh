#!/bin/bash
# IC-4a mutation matrix — each guard is broken, the build must succeed
# (BUILD_OK), and the paired test must FAIL (the mutation is killed).
# Run from controlplane/: bash controlplane/internal/api/handler/mutation_matrix.sh
set -u
# repo root assumed; use absolute-safe cd
cd "$(cd "$(dirname "$0")" && git rev-parse --show-toplevel)"

PASS=0; FAIL=0

mutate_and_test() {
  local name="$1" file="$2" old="$3" new="$4" testpattern="$5"
  cp "$file" "$file.bak"
  python3 -c "
import sys
path, old, new = sys.argv[1], sys.argv[2], sys.argv[3]
src = open(path).read()
assert src.count(old) >= 1, 'mutation target not found in ' + path
open(path, 'w').write(src.replace(old, new, 1))
" "$file" "$old" "$new" || { echo "$name: MUTATION_APPLY_FAILED"; mv "$file.bak" "$file" 2>/dev/null; FAIL=$((FAIL+1)); return; }
  if ! go build ./... >/dev/null 2>&1; then
    echo "$name: BUILD_FAILED (not a valid mutation — ignored)"; mv "$file.bak" "$file"; return
  fi
  echo "$name: BUILD_OK"
  if go test ./controlplane/internal/api/handler/ -run "$testpattern" -count=1 >/dev/null 2>&1; then
    echo "$name: MUTANT SURVIVED ❌"; FAIL=$((FAIL+1))
  else
    echo "$name: KILLED ✅"; PASS=$((PASS+1))
  fi
  mv "$file.bak" "$file"
}

# M1: cap decision `>` → `>=` (dead-letter AT the cap instead of past it)
mutate_and_test "M1_shouldDeadLetter_ge" controlplane/internal/api/handler/webhook_policy.go \
  "return count > limit" "return count >= limit" \
  "TestIC4A_Boundary_AtCapStillRetries|TestIC4A_MutationMatrix"

# M2: default cap 600 → 1 (poison pill dead-letters on first hiccup)
mutate_and_test "M2_default_limit" controlplane/internal/api/handler/webhook_policy.go \
  "const DefaultWebhookFailLimit int64 = 600" "const DefaultWebhookFailLimit int64 = 1" \
  "TestIC4A_DefaultFloorAndBoundary|TestIC4A_MutationMatrix"

# M3: identity drops the sequencer (retries of distinct events share a counter)
mutate_and_test "M3_identity_no_seq" controlplane/internal/api/handler/webhook_policy.go \
  'return bucket + "/" + key + "/" + sequencer' 'return bucket + "/" + key' \
  "TestIC4A_DistinctEventsIndependentCounters|TestIC4A_MutationMatrix"

# M4: redis key becomes random (retries never share a counter → cap unreachable)
mutate_and_test "M4_redis_key_nondeterministic" controlplane/internal/api/handler/webhook_policy.go \
  'sum := sha256.Sum256([]byte(identity))' \
  'sum := sha256.Sum256([]byte(identity + time.Now().String()))' \
  "TestIC4A_RetriesShareOneCounter|TestIC4A_MutationMatrix"

# M5: success does not clear the counter (stale counts dead-letter early)
mutate_and_test "M5_no_clear_on_success" controlplane/internal/api/handler/events.go \
  'h.clearFailCount(c.Request.Context(), bucket, key, rec)
	}
	if failed {' '	}
	if failed {' \
  "TestIC4A_SuccessClearsCounter"

# M6: failure answered 200 (the original IC-BUG-6 bug — the headline regression)
mutate_and_test "M6_5xx_back_to_200" controlplane/internal/api/handler/events.go \
  'c.Status(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusOK)' 'c.Status(http.StatusOK)
		return
	}
	c.Status(http.StatusOK)' \
  "TestRED_HandlerReturns5xxWhenIndexingFails|TestIC4A_FailureUnderCap_Returns5xx"

# M7: failure counter not persisted (in-memory semantics — retries never reach cap)
mutate_and_test "M7_no_count" controlplane/internal/api/handler/events.go \
  'count, err := h.fails.IncrFailCount(ctx, DeadLetterRedisKey(identity))' \
  'count, err := int64(1), error(nil)' \
  "TestIC4A_FailureOverCap_DeadLetters200|TestIC4A_CounterPersistsAcrossStoreInstances"

# M8 (B2): dead-letter persist failure must keep 5xx + counter. Mutating
# deadLetter() to swallow the error would be killed by the B2 red tests.
mutate_and_test "M8_deadLetter_swallow" controlplane/internal/api/handler/events.go \
  'return err
	}
	logDeadLetter(h.logger, dl)' \
  'return nil
	}
	logDeadLetter(h.logger, dl)' \
  "TestRED_SinkFailure_Returns5xx_KeepsCounter|TestRED_SinkRecovers_NextRedeliveryLandsDeadLetter"

# M9 (B1): the in-process fallback is the guard against counter-backend
# outages; removing it would be killed by the B1 red test.
# M9 (B2, oversized variant): the oversized dead letter must be gated on
# durable persistence too — swallowing the sink error would answer 200 and
# silently lose the event. Killed by TestOversizedPayload_SinkFailure_Still5xx.
mutate_and_test "M9_oversized_persist_gate" controlplane/internal/api/handler/events.go \
  '		if err := h.deadLetter(c.Request.Context(), dl); err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusInternalServerError)
		return' \
  '		c.Status(http.StatusOK)
		return' \
  "TestOversizedPayload_SinkFailure_Still5xx"

mutate_and_test "M10_decode_bypass_counting" controlplane/internal/api/handler/events.go \
  'if h.handleIndexFailure(c.Request.Context(), bucket, rec.S3.Object.Key, rec, err) {
				failed = true
			}' \
  'failed = true' \
  "TestRED_DecodeFailure_OverCap_DeadLetter200|TestRED_DecodeFailure_UnderCap_Retries"

# M11 (S2): the payload hash must be content-derived; a fixed key would make
# distinct bad payloads overwrite each other's dead letter.
mutate_and_test "M11_fixed_hash" controlplane/internal/api/handler/events.go \
  '"unparseable:" + HashPayload(raw)' \
  '"unparseable:fixed"' \
  "TestIC4A_UnparseablePayload_DistinctPayloadsDoNotOverwrite|TestIC4A_UnparseablePayload_DeadLetter200"

# M12 (B-OLD-1): the PG fallback is the durable counter layer. Mutating
# IncrFailCount to return an error when Redis fails (i.e. dropping the PG
# path) would be killed by the B-OLD-1 test.
mutate_and_test "M12_pg_fallback_dropped" controlplane/internal/api/handler/webhook_policy.go \
  '	pgCount, pgErr := s.pg.IncrWebhookFailCounter(ctx, DeadLetterRedisKey(identity))' \
  '	return 0, err' \
  "TestRED_CounterBackendDown_FallbackKeepsCounting"

# M13 (B-NEW-2): oversized detection must exist — removing the limit+1 check
# (silently truncating) would be killed by the valid-large-payload test.
mutate_and_test "M13_oversized_detection" controlplane/internal/api/handler/webhook_policy.go \
  '	if int64(len(full)) > cap {
		return full[:cap], true, nil
	}' \
  '	if false {
		return full[:cap], true, nil
	}' \
  "TestOversizedPayload_DeadLetterAnd5xx"

mutate_and_test "M14_merge_max" controlplane/internal/api/handler/webhook_policy.go \
  '	merged := pgCount' \
  '	merged := redisCount' \
  "TestMergeRecovered_TakesMax"

echo "----"
echo "killed=$PASS survived=$FAIL"
[ "$FAIL" -eq 0 ] && echo "MUTATION_MATRIX_ALL_KILLED" || exit 1
