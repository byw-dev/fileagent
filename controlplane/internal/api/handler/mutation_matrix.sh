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
  "TestIC4A_DefaultFloor|TestIC4A_MutationMatrix"

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

echo "----"
echo "killed=$PASS survived=$FAIL"
[ "$FAIL" -eq 0 ] && echo "MUTATION_MATRIX_ALL_KILLED" || exit 1
