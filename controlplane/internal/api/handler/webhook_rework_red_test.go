package handler_test

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"sync"
	"testing"

	"github.com/alicebob/miniredis/v2"
	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
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
	n, err := fails.FailCount(context.Background(), "b/k/sinkdown")
	require.NoError(t, err)
	assert.Equal(t, int64(2), n, "B2: the counter must survive a failed dead-letter persist")
	assert.Equal(t, 0, dead.count(), "B2: nothing was durably recorded while the sink fails")
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

// TestCounterSeries_MonotonicAcrossRedisLoss: the round-3 model — PG is the
// authoritative monotonic counter, Redis only a cache. When the Redis
// instance dies mid-series (data loss), the count must NOT regress: PG holds
// the full series and every subsequent failure keeps advancing it. The
// round-2 max-merge model returned 1 here (fresh Redis), swallowing all
// prior failures.
func TestCounterSeries_MonotonicAcrossRedisLoss(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://fileagent:fileagent@127.0.0.1:5432/fileagent?sslmode=disable"
	}
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("no PostgreSQL available: %v", err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Skipf("no PostgreSQL available: %v", err)
	}
	q := db.New(conn)
	// The handler's identity comes from bucket/key/sequencer of the payload —
	// derive the test's key from the SAME identity so pre/cleanup target the
	// row the handler actually writes.
	identity := handler.DeadLetterIdentity("b", "k", "redisloss")
	dk := handler.DeadLetterRedisKey(identity)
	// Clean BOTH ends: stale rows from earlier failed runs must not seed this
	// run's counter (a leftover count ≥ cap would dead-letter on delivery 1).
	_, _ = conn.Exec("DELETE FROM webhook_fail_counters WHERE dedup_key=$1", dk)
	t.Cleanup(func() { _ = q.ClearWebhookFailCounter(context.Background(), dk) })

	mr := miniredis.RunT(t)
	logger, _ := zap.NewDevelopment()
	client, err := cache.New("redis://"+mr.Addr(), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	store := handler.NewRedisWebhookFailStore(client, q)

	ix := &mockIndexerClient{err: assert.AnError}
	dead := &recordingSink{}
	h := handler.NewMinioEventHandlerWithPolicy(ix, testWebhookSecret, store, dead, 2, newTestLogger())

	// Two failed deliveries with Redis up: the authoritative count is 2.
	code1 := postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "redisloss", 1)).Code
	if code1 != http.StatusInternalServerError {
		t.Fatalf("DELIVERY1: expected 500 (count=1 < cap 2), got %d", code1)
	}
	code2 := postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "redisloss", 1)).Code
	if code2 != http.StatusInternalServerError {
		t.Fatalf("DELIVERY2: expected 500 (count=2 = cap, strict >), got %d", code2)
	}
	pgAfterTwo, err := q.GetWebhookFailCounter(context.Background(), dk)
	require.NoError(t, err)
	require.Equal(t, int64(2), pgAfterTwo)

	// Redis dies completely (instance loss). The next delivery must still
	// advance the series (3), reach the cap (3 > 2), land the dead letter,
	// and return 200. The round-2 max model reset to 1 here (2 < 3 = never
	// reached the cap, infinite 5xx).
	mr.Close()
	code := postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "redisloss", 1)).Code
	assert.Equal(t, http.StatusOK, code,
		"the monotonic series must survive Redis instance loss: count 3 > cap 2 → dead letter + 200 (round-2 max model reset to 1 and never reached the cap)")
	require.Equal(t, 1, dead.count())

	// The authoritative count must reflect all three failures before the
	// durable verdict cleared it.
	var n int
	require.NoError(t, conn.QueryRow("SELECT count(*) FROM webhook_fail_counters WHERE dedup_key=$1", dk).Scan(&n))
	assert.Equal(t, 0, n, "the PG row is cleared after the durable verdict")
}
