package handler_test

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// IC-4a rework — PoC gate RED tests for B1 / B2 / B3 (PR #110 review).
//
// Each test below pins the CORRECT (post-fix) behaviour and fails against the
// current implementation:
//
//	B1: a broken counter backend must NOT stall the feed forever — a bounded
//	    in-process fallback must keep the counter advancing so the cap stays
//	    reachable (currently: 5xx forever, counter never advances, no dead letter).
//	B2: when the dead-letter sink fails to persist, the event must NOT be
//	    answered 200 (that loses it silently) — the response must be 5xx and
//	    the counter must be kept so the next round retries persistence
//	    (currently: sink error swallowed, counter cleared, 200 returned).
//	B3: decode failures must flow through the same counted state machine —
//	    under the cap 5xx, over the cap dead-letter + 200 (currently: a dead
//	    letter is written on every attempt yet the response stays 5xx forever).
// ─────────────────────────────────────────────────────────────────────────────

// flakySink is a DeadLetterSink whose failure can be toggled mid-test.
type flakySink struct {
	mu       sync.Mutex
	fail     bool
	rows     []handler.DeadLetter
	attempts int
}

func (s *flakySink) DeadLetter(_ context.Context, dl handler.DeadLetter) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.fail {
		return assert.AnError
	}
	s.rows = append(s.rows, dl)
	return nil
}

func (s *flakySink) attemptsMade() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.attempts
}

func (s *flakySink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

// lastRow returns the most recently persisted dead letter (no locking guard
// beyond the sink mutex; tests call it single-threaded).
func (s *flakySink) lastRow() handler.DeadLetter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[len(s.rows)-1]
}

// ── B2: dead-letter persistence failure must NOT answer 200 ──────────────────

// TestRED_SinkFailure_Returns5xx_KeepsCounter: once past the cap, if the
// dead-letter sink fails to persist, the event must be answered 5xx (MinIO
// redelivers) and the counter must be kept — otherwise a PG outage as long as
// the cap window silently deletes events (the queue head is freed with no
// record anywhere). Current behaviour: 200 + counter cleared = silent loss.
func TestRED_SinkFailure_Returns5xx_KeepsCounter(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	fails := newCountingFailStore()
	dead := &flakySink{fail: true} // sink (PG) is down
	h := ic4aHandlerDead(ix, fails, dead, 1)

	// First delivery: count=1, not > 1 → 5xx (fine).
	code1 := postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "sinkdown", 1)).Code
	require.Equal(t, http.StatusInternalServerError, code1)

	// Second delivery: count=2 > 1 → dead-letter verdict reached, but the
	// sink fails. The event must NOT be lost: expect 5xx again.
	code2 := postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "sinkdown", 1)).Code
	assert.Equal(t, http.StatusInternalServerError, code2,
		"B2: dead-letter persistence failure must answer 5xx, not 200 (a 200 makes MinIO drop the event with no record)")

	// The counter must be kept so a later round can still reach a durable
	// verdict (persist → 200).
	n, err := fails.FailCount(context.Background(), redisKeyOf("b/k/sinkdown"))
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "B2: the counter must survive a failed dead-letter persist")
	assert.Equal(t, 1, dead.count(), "B2: nothing was durably recorded")
}

// TestRED_SinkRecovers_NextRedeliveryLandsDeadLetter: after the sink comes
// back, the very next redelivery persists the dead letter and returns 200.
func TestRED_SinkRecovers_NextRedeliveryLandsDeadLetter(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	fails := newCountingFailStore()
	dead := &flakySink{fail: true}
	h := ic4aHandlerDead(ix, fails, dead, 1)

	// Sink down: two rounds, both 5xx.
	_ = postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "recover", 1)).Code
	_ = postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "recover", 1)).Code

	// Sink (PG) recovers.
	dead.fail = false
	code := postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "recover", 1)).Code
	assert.Equal(t, http.StatusOK, code,
		"B2: once the dead letter persists, the queue must be freed with 200")
	require.Equal(t, 1, dead.count())
	assert.Equal(t, int64(3), dead.lastRow().FailCount)
	// Counter cleared after durable verdict.
	n, err := fails.FailCount(context.Background(), redisKeyOf("b/k/recover"))
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

// ── B1: broken Redis must not stall the feed forever ─────────────────────────

// TestRED_CounterBackendDown_FallbackKeepsCounting: with the Redis counter
// unavailable, the bounded in-process fallback must keep the count advancing
// so the cap stays reachable; the exhausted event then dead-letters (200) and
// the feed keeps moving. Currently: 5xx forever, no dead letter ever.
func TestRED_CounterBackendDown_FallbackKeepsCount(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	fails := newCountingFailStore()
	fails.incrErr = assert.AnError // Redis down for the whole test
	dead := &recordingSink{}
	h := ic4aHandler(ix, fails, dead, 2)

	var last int
	for i := 0; i < 4; i++ {
		last = postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "reddown", 1)).Code
	}
	assert.Equal(t, http.StatusOK, last,
		"B1: with the Redis counter down, the fallback counter must still reach the cap and dead-letter instead of 5xx forever")
	assert.Equal(t, 1, dead.count(), "B1: the exhausted event must reach the dead-letter store, not be retried eternally")
}

// ── B3: decode failures must reach a durable terminal state ──────────────────

// TestRED_DecodeFailure_UnderCap_Retries: a decode failure below the cap is
// answered 5xx and — crucially — NOT dead-lettered yet (the old code wrote a
// dead letter on every attempt while still returning 5xx, which is both a
// premature verdict and an infinite retry).
func TestRED_DecodeFailure_UnderCap_Retries(t *testing.T) {
	ix := &mockIndexerClient{}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	h := ic4aHandler(ix, fails, dead, 3)

	w := postIC4A(t, h, minioEventBody(t, "s3:ObjectCreated:Put", "data-sensor", "a%2Gb.csv"))
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"B3: under the cap a decode failure must be retried (5xx)")
	assert.Equal(t, 0, dead.count(),
		"B3: no dead letter before the cap — writing one per attempt yet retrying forever is contradictory")
	assert.False(t, ix.called, "an undecodable key must not reach the index")
}

// TestRED_DecodeFailure_OverCap_DeadLetter200: the same undecodable payload,
// delivered past the cap, must land a dead letter and be answered 200 so the
// queue head is freed. Currently: 5xx on every delivery, cap unreachable.
func TestRED_DecodeFailure_OverCap_DeadLetter200(t *testing.T) {
	ix := &mockIndexerClient{}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	h := ic4aHandlerDead(ix, fails, dead, 1)

	// First delivery: count=1, not > 1 → 5xx.
	code := postIC4A(t, h, minioEventBody(t, "s3:ObjectCreated:Put", "data-sensor", "a%2Gb.csv")).Code
	require.Equal(t, http.StatusInternalServerError, code)

	// Second delivery: count=2 > 1 → dead letter + 200.
	code = postIC4A(t, h, minioEventBody(t, "s3:ObjectCreated:Put", "data-sensor", "a%2Gb.csv")).Code
	assert.Equal(t, http.StatusOK, code,
		"B3: an exhausted decode failure must reach the durable terminal state (dead letter + 200), not 5xx forever")
	require.Equal(t, 1, dead.count())
	dl := dead.last()
	assert.Equal(t, "data-sensor/a%2Gb.csv/", dl.DedupKey,
		"decode failures are counted by RAW key + sequencer (empty sequencer in this payload)")
	assert.True(t, dl.Removed || dl.EventName == "s3:ObjectCreated:Put")
}

var _ = time.Now // retained for tag-stability of the test file