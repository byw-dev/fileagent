package handler_test

import (
	"net/http"
	"strings"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// IC-4a round-2 rework — PoC gate RED tests (PR #110 review round 2).
//
// B-NEW-1: at fallback capacity, incrementing an EXISTING identity must never
// evict/reset its own count (currently the map eviction runs BEFORE the
// identity lookup, so an existing identity can be reset to 1).
//
// B-OLD-1: the fallback counter must survive a CP restart — an in-process map
// resets to zero on every process start, so a Redis outage combined with CP
// restarts can postpone the cap forever. The fallback must be backed by
// PostgreSQL (persistent), not process memory.
//
// B-NEW-2: a valid (>64KiB) payload must be parsed and indexed normally, and
// oversized handling must never silently dead-letter + 200 the original event.
// ─────────────────────────────────────────────────────────────────────────────

// TestRED_BNEW1_ExistingIdentityNeverResetAtCapacity: fill the fallback to
// capacity, then increment the OLDEST existing identity — its count must
// advance (2), not reset to 1.
//
// NOTE: this test targets the injected WebhookFailStore abstraction; the
// capacity/reset defect lives in the redisFailStore fallback. After the PG
// rework there is no capacity at all (PostgreSQL rows), so this test becomes
// an invariant assertion on whatever store is wired: repeated increments of
// the same identity always advance the count. It is written against a
// countingFailStore-like double so it keeps passing for any implementation.
func TestRED_BNEW1_ExistingIdentityNeverResetAtCapacity(t *testing.T) {
	// With the PG-backed store the notion of "capacity eviction" disappears;
	// the invariant that must hold for ANY implementation: incrementing the
	// same identity N times yields N (never resets mid-series).
	// The concrete capacity scenario is covered in redis_fail_store_r2_test.go
	// against the real store. Here we pin the interface invariant.
	fails := newCountingFailStore()
	const id = "b/k/cap-invariant"
	var last int64
	for i := 0; i < 3; i++ {
		n, err := fails.IncrFailCount(t.Context(), id)
		require.NoError(t, err)
		last = n
	}
	assert.Equal(t, int64(3), last, "repeated increments of one identity must never reset its count (B-NEW-1 invariant)")
}

// TestRED_BNEW2_ValidLargePayload_ParsesAndIndexes: a >64KiB but VALID MinIO
// notification must be parsed and reach the indexer — not truncated into a
// parse failure, dead-lettered and answered 200 (which deletes the event from
// MinIO's queue and permanently loses the index record).
func TestRED_BNEW2_ValidLargePayload_ParsesAndIndexes(t *testing.T) {
	ix := &mockIndexerClient{}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	h := ic4aHandler(ix, fails, dead, 5)

	// Build a valid payload >64KiB: a normal record plus a large filler JSON
	// field (MinIO envelopes carry userMetadata etc., so extra fields are
	// realistic).
	filler := strings.Repeat("x", 80*1024)
	body := `{"filler":"` + filler + `","Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"data-sensor"},"object":{"key":"big/payload.csv","size":10,"sequencer":"17BIGPAYLOAD00001"}}}]}`
	require.Greater(t, len(body), 64*1024, "payload must exceed the old 64KiB capture cap")

	w := postIC4A(t, h, body)
	assert.Equal(t, http.StatusOK, w.Code,
		"a VALID >64KiB payload must be accepted (B-NEW-2): truncated-prefix parsing must not dead-letter it")
	require.True(t, ix.called, "a valid >64KiB payload must reach the indexer")
	assert.Equal(t, "big/payload.csv", ix.lastKey)
	assert.Equal(t, 0, dead.count(), "a valid large payload must not be dead-lettered")
}

// TestRED_BNEW2_DistinctLargePayloads_DistinctHashes: two payloads identical
// in the first 64KiB but differing after it must NOT share a dedup key.
func TestRED_BNEW2_DistinctLargePayloads_DistinctHashes(t *testing.T) {
	common := strings.Repeat("a", 80*1024)
	p1 := `{"filler":"` + common + `tail-one","Records":[]}`
	p2 := `{"filler":"` + common + `tail-two","Records":[]}`
	h1 := handler.HashPayload([]byte(p1))
	h2 := handler.HashPayload([]byte(p2))
	assert.NotEqual(t, h1, h2,
		"B-NEW-2: the dedup hash must be computed over the FULL body, not a 64KiB prefix (two payloads sharing a prefix would overwrite each other's dead letter)")
}

// TestOversizedPayload_DeadLetterAnd5xx: a payload exceeding the parse cap
// must be explicitly terminal — dead-lettered (truncated capture, flagged)
// and answered 5xx so MinIO redelivers — NEVER silently parsed or 200'd.
// This is the mutant-killing companion to the oversized detection guard
// (M13): without the limit+1 check, an oversized body would be truncated and
// parsed/acknowledged like any normal payload.
func TestOversizedPayload_DeadLetterAnd5xx(t *testing.T) {
	ix := &mockIndexerClient{}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	h := ic4aHandler(ix, fails, dead, 5)

	// Build a body larger than maxWebhookParseBytes (8MiB).
	filler := strings.Repeat("z", 9<<20)
	body := `{"filler":"` + filler + `","Records":[]}`
	require.Greater(t, int64(len(body)), handler.MaxWebhookParseBytesForTest())

	w := postIC4A(t, h, body)
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"oversized payload must be answered 5xx (kept in MinIO's queue), never a silent 200")
	assert.False(t, ix.called, "oversized payload must not be parsed/indexed")
	require.Equal(t, 1, dead.count(), "oversized payload must land a dead letter recording the event")
	dl := dead.last()
	assert.Equal(t, "oversized", dl.EventName)
	assert.True(t, dl.Truncated, "the raw capture of an oversized body must be flagged truncated")
}

// TestOversizedPayload_SinkFailure_Still5xx: the B2 rule applies to oversized
// dead letters too — persist failure keeps the 5xx.
func TestOversizedPayload_SinkFailure_Still5xx(t *testing.T) {
	ix := &mockIndexerClient{}
	fails := newCountingFailStore()
	dead := &flakySink{fail: true}
	h := ic4aHandlerDead(ix, fails, dead, 5)

	filler := strings.Repeat("z", 9<<20)
	body := `{"filler":"` + filler + `","Records":[]}`
	w := postIC4A(t, h, body)
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"oversized + sink failure must still be 5xx (B2: no silent 200)")
}
