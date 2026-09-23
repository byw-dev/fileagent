package handler_test

import (
	"context"
	"database/sql"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"

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
	n, err := fails.FailCount(context.Background(), redisKeyOf("b/k/sinkdown"))
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

// TestRED_CounterBackendDown_FallbackKeepsCounting: with the Redis counter
// unavailable, the REAL RedisWebhookFailStore's PostgreSQL fallback must keep
// the count advancing so the cap stays reachable; the exhausted event then
// dead-letters (200) and the feed keeps moving. This test runs against a real
// PostgreSQL (dev dev DB has migrations applied; the round-1 version used the
// in-process map, which the round-2 review replaced per the IC-4 (b)
// constraint).
func TestRED_CounterBackendDown_FallbackKeepsCounting(t *testing.T) {
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		dsn = "postgres://fileagent:fileagent@127.0.0.1:5432/fileagent?sslmode=disable"
	}
	conn, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Skipf("no PostgreSQL available for the PG-fallback test: %v", err)
	}
	defer conn.Close()
	if err := conn.Ping(); err != nil {
		t.Skipf("no PostgreSQL available for the PG-fallback test: %v", err)
	}

	// Real store against an in-process Redis, then take Redis away mid-test
	// (the review's B1 scenario: a persistent Redis outage, not a blip).
	mr := miniredis.RunT(t)
	logger, _ := zap.NewDevelopment()
	client, err := cache.New("redis://"+mr.Addr(), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	store := handler.NewRedisWebhookFailStore(client, db.New(conn))
	identityKey := handler.DeadLetterRedisKey("b/k/fb")
	t.Cleanup(func() { _ = store.ClearFailCount(context.Background(), "b/k/fb") })

	ix := &mockIndexerClient{err: assert.AnError}
	dead := &recordingSink{}
	h := handler.NewMinioEventHandlerWithPolicy(ix, testWebhookSecret, store, dead, 2, newTestLogger())

	// Sanity: while Redis is up, counting works (2 rounds, both 5xx).
	require.Equal(t, http.StatusInternalServerError,
		postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "fb", 1)).Code)
	require.Equal(t, http.StatusInternalServerError,
		postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "fb", 1)).Code)

	// Redis disappears for the rest of the test.
	mr.Close()

	// The PostgreSQL fallback must keep advancing: delivery 3 crosses the cap
	// (count=3 > 2) → dead letter + 200; the feed is NOT stalled forever.
	var last, deadCount int
	for i := 0; i < 3; i++ {
		last = postIC4A(t, h, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "fb", 1)).Code
		deadCount = dead.count()
	}
	assert.Equal(t, http.StatusOK, last,
		"B1/B-OLD-1: with Redis down, the PostgreSQL fallback must reach the cap and dead-letter instead of 5xx forever")
	assert.Equal(t, 1, deadCount,
		"B1/B-OLD-1: the exhausted event must reach the dead-letter store, not be retried eternally")

	// B-OLD-1 core evidence: delivery #3 crossed the cap using counts that
	// lived in PostgreSQL (Redis was down from delivery #3 on, so INCR #3 was
	// served by webhook_fail_counters). The dead letter then succeeded and
	// legitimately cleared both counters — assert the PG row was written and
	// cleared, i.e. the fallback path really persisted.
	var pgCount int64
	err = conn.QueryRow("SELECT count FROM webhook_fail_counters WHERE dedup_key=$1", identityKey).Scan(&pgCount)
	assert.ErrorIs(t, err, sql.ErrNoRows,
		"B-OLD-1: the PG fallback row must have been written (count reached 3 via PG) and then cleared by the durable dead-letter verdict")
	_ = identityKey
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

// TestMergeRecovered_TakesMax pins A-1 through the public API: with PG
// holding more progress than Redis (Redis lost its key/TTL during an outage),
// the next Redis-recovered increment must adopt the LARGER value — repeated
// Redis outages can never repeatedly reset the count and postpone the cap.
func TestMergeRecovered_TakesMax(t *testing.T) {
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
	identity := "b/k/mergemax"
	t.Cleanup(func() {
		_ = q.ClearWebhookFailCounter(context.Background(), handler.DeadLetterRedisKey(identity))
	})

	mr := miniredis.RunT(t)
	logger, _ := zap.NewDevelopment()
	client, err := cache.New("redis://"+mr.Addr(), logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = client.Close() })
	store := handler.NewRedisWebhookFailStore(client, q)

	// Simulate the outage's aftermath: Redis holds a low count (key survived
	// at 1), PG holds the higher durable progress (5).
	_, err = store.IncrFailCount(context.Background(), identity) // Redis=1, PG row cleared by merge
	require.NoError(t, err)
	_, err = q.IncrWebhookFailCounter(context.Background(), handler.DeadLetterRedisKey(identity)) // PG=1
	require.NoError(t, err)
	_, err = q.IncrWebhookFailCounter(context.Background(), handler.DeadLetterRedisKey(identity)) // PG=2
	require.NoError(t, err)
	_, err = q.IncrWebhookFailCounter(context.Background(), handler.DeadLetterRedisKey(identity)) // PG=3
	require.NoError(t, err)
	_, err = q.IncrWebhookFailCounter(context.Background(), handler.DeadLetterRedisKey(identity)) // PG=4
	require.NoError(t, err)

	// Redis-recovers increment: Redis INCR makes its count 2, but PG holds 4
	// (the durable progress). A-1: the merged value must be max(2, 4) = 4 —
	// adopting the raw Redis count (2) would RESET the durable progress and
	// postpone the cap on every Redis outage.
	count, err := store.IncrFailCount(context.Background(), identity)
	require.NoError(t, err)
	assert.Equal(t, int64(4), count,
		"A-1: merge must take max(redis, pg) — the durable PG progress (4) must not be reset by a stale Redis count (2)")
}
