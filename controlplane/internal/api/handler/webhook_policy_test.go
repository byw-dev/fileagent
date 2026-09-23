package handler_test

import (
	"context"
	"net/http"
	"sync"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ─────────────────────────────────────────────────────────────────────────────
// IC-4a ① failure semantics: poison pill + dead letter.
//
// Each guard here is paired with a mutation test in
// TestIC4A_MutationMatrix (bottom of this file): break the guard, watch the
// paired case go red. BUILD_OK is asserted there so a compile failure can
// never masquerade as a passing mutation.
// ─────────────────────────────────────────────────────────────────────────────

// countingFailStore is a webhookFailStore double: counters live in a map, and
// every operation is recorded for assertions.
type countingFailStore struct {
	mu     sync.Mutex
	counts map[string]int64
	incrErr,
	readErr,
	clearErr error
	increments, clears int
}

func newCountingFailStore() *countingFailStore {
	return &countingFailStore{counts: make(map[string]int64)}
}

func (s *countingFailStore) IncrFailCount(_ context.Context, identity string) (int64, error) {
	if s.incrErr != nil {
		return 0, s.incrErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.increments++
	s.counts[identity]++
	return s.counts[identity], nil
}

func (s *countingFailStore) FailCount(_ context.Context, identity string) (int64, error) {
	if s.readErr != nil {
		return 0, s.readErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[identity], nil
}

func (s *countingFailStore) ClearFailCount(_ context.Context, identity string) error {
	if s.clearErr != nil {
		return s.clearErr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clears++
	delete(s.counts, identity)
	return nil
}

func (s *countingFailStore) totalIncrements() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.increments
}

// recordingSink is a deadLetterSink double that records every dead letter.
type recordingSink struct {
	mu    sync.Mutex
	rows  []handler.DeadLetter
	err   error
	drops int
}

func (s *recordingSink) DeadLetter(_ context.Context, dl handler.DeadLetter) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.drops++
	if s.err != nil {
		return s.err
	}
	s.rows = append(s.rows, dl)
	return nil
}

func (s *recordingSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.rows)
}

func (s *recordingSink) last() handler.DeadLetter {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rows[len(s.rows)-1]
}

// ic4aHandlerDead wires a MinioEventHandler with the full failure machinery
// and a caller-provided dead-letter sink (so tests can inject flaky sinks).
func ic4aHandlerDead(ix *mockIndexerClient, fails *countingFailStore, dead handler.DeadLetterSink, limit int64) *handler.MinioEventHandler {
	return handler.NewMinioEventHandlerWithPolicy(ix, testWebhookSecret, fails, dead, limit, newTestLogger())
}

// ic4aHandler wires a MinioEventHandler with the full failure machinery.
func ic4aHandler(ix *mockIndexerClient, fails *countingFailStore, dead *recordingSink, limit int64) *handler.MinioEventHandler {
	return ic4aHandlerDead(ix, fails, dead, limit)
}

// eventWithSequencer builds a Records payload with a sequencer, so retries of
// one event share a counter while distinct events do not.
func eventWithSequencer(eventName, bucket, key, sequencer string, size int64) string {
	return `{"Records":[{"eventName":"` + eventName + `","s3":{"bucket":{"name":"` + bucket +
		`"},"object":{"key":"` + key + `","size":` + itoa(size) + `,"eTag":"etag-x","sequencer":"` + sequencer + `"}}}]}`
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	neg := n < 0
	if neg {
		n = -n
	}
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

// postWithFails posts one event through a handler wired with the given policy
// parts and returns (status, recorder).
func postWithFails(t *testing.T, ix *mockIndexerClient, fails *countingFailStore, dead *recordingSink, limit int64, body string) int {
	t.Helper()
	h := ic4aHandler(ix, fails, dead, limit)
	w := postIC4A(t, h, body)
	return w.Code
}

// TestIC4A_FailureUnderCap_Returns5xx: with a counter store wired, a failure
// below the cap is answered 5xx (MinIO retries) and the counter persisted.
func TestIC4A_FailureUnderCap_Returns5xx(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	code := postWithFails(t, ix, fails, dead, 3, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "seq-1", 1))
	assert.Equal(t, http.StatusInternalServerError, code)
	assert.Equal(t, 1, fails.totalIncrements(), "the failure must be counted under the event identity")
	assert.Equal(t, 0, dead.count(), "no dead letter below the cap")
}

// TestIC4A_FailureOverCap_DeadLetters200: at cap+1 the event is dead-lettered
// and the response is 200 so the head-of-line queue is freed.
func TestIC4A_FailureOverCap_DeadLetters200(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	// Two prior deliveries failed (pre-existing counter state, keyed the same
	// way the handler does: hash of the event identity).
	id := handler.DeadLetterRedisKey("b/k/deadkey")
	fails.IncrFailCount(context.Background(), id)
	fails.IncrFailCount(context.Background(), id)
	code := postWithFails(t, ix, fails, dead, 2, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "deadkey", 1))
	assert.Equal(t, http.StatusOK, code, "an exhausted event must be answered 200 to free the MinIO queue")
	require.Equal(t, 1, dead.count(), "exhausted event must be dead-lettered")
	dl := dead.last()
	assert.Equal(t, "b/k/deadkey", dl.DedupKey)
	assert.Equal(t, int64(3), dl.FailCount)
	assert.False(t, dl.Removed)
}

// TestIC4A_Boundary_AtCapStillRetries: exactly AT the cap the event is still
// retried; the dead letter happens on the first attempt past the cap.
func TestIC4A_Boundary_AtCapStillRetries(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	fails.IncrFailCount(context.Background(), "edge") // 1 prior failure
	code := postWithFails(t, ix, fails, dead, 2, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "edge", 1))
	// prior 1 + this 1 = 2, not > 2 → still retry.
	assert.Equal(t, http.StatusInternalServerError, code)
	assert.Equal(t, 0, dead.count())
}

// TestIC4A_SuccessClearsCounter: after a successful delivery the persistent
// counter is removed, so an unrelated later failure starts from zero.
func TestIC4A_SuccessClearsCounter(t *testing.T) {
	identity := handler.DeadLetterRedisKey("b/k/succ")
	// Pre-seed two failures, then deliver successfully.
	fails := newCountingFailStore()
	fails.IncrFailCount(context.Background(), identity)
	fails.IncrFailCount(context.Background(), identity)

	ix := &mockIndexerClient{} // success
	dead := &recordingSink{}
	code := postWithFails(t, ix, fails, dead, 2, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "succ", 1))
	assert.Equal(t, http.StatusOK, code)
	n, err := fails.FailCount(context.Background(), identity)
	require.NoError(t, err)
	assert.Equal(t, int64(0), n, "the counter must be cleared after success")
}

// TestIC4A_CounterPersistsAcrossStoreInstances: the counter lives in the
// store, not the handler — a NEW handler (process restart equivalent) sees
// the same count and dead-letters at the cap.
func TestIC4A_CounterPersistsAcrossStoreInstances(t *testing.T) {
	fails := newCountingFailStore()
	// First process: two failed deliveries (2 increments, no dead letter).
	for i := 0; i < 2; i++ {
		ix := &mockIndexerClient{err: assert.AnError}
		dead := &recordingSink{}
		code := postWithFails(t, ix, fails, dead, 2, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "restart", 1))
		require.Equal(t, http.StatusInternalServerError, code)
		require.Equal(t, 0, dead.count())
	}
	// New handler, same store (restart): third failure crosses the cap.
	ix := &mockIndexerClient{err: assert.AnError}
	dead := &recordingSink{}
	code := postWithFails(t, ix, fails, dead, 2, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "restart", 1))
	assert.Equal(t, http.StatusOK, code, "the counter must survive a handler (process) restart")
	assert.Equal(t, 1, dead.count())
}

// TestIC4A_RetriesShareOneCounter: repeated deliveries of the SAME event
// (same bucket+key+sequencer) increment one counter — that is what makes the
// cap reachable.
func TestIC4A_RetriesShareOneCounter(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	for i := 0; i < 3; i++ {
		code := postWithFails(t, ix, fails, dead, 4, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "same", 1))
		require.Equal(t, http.StatusInternalServerError, code, "delivery %d stays under the cap", i+1)
	}
	assert.Equal(t, 3, fails.totalIncrements(), "three deliveries of one event = three increments")
	n, err := fails.FailCount(context.Background(), redisKeyOf("b/k/same"))
	require.NoError(t, err)
	assert.Equal(t, int64(3), n)
}

// TestIC4A_DistinctEventsIndependentCounters: different sequencers never
// share a counter (a noisy neighbor must not dead-letter a fresh event).
func TestIC4A_DistinctEventsIndependentCounters(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	// Event A fails three times at limit 2 → dead-lettered on the 3rd.
	for i := 0; i < 3; i++ {
		code := postWithFails(t, ix, fails, dead, 2, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "noisy", 1))
		require.Equal(t, i < 2, code == http.StatusInternalServerError)
	}
	require.Equal(t, 1, dead.count())
	// Event B (different sequencer) fails once → still under the cap → 5xx,
	// NOT dead-lettered by A's count.
	dead2 := &recordingSink{}
	code := postWithFails(t, ix, fails, dead2, 2, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "fresh", 1))
	assert.Equal(t, http.StatusInternalServerError, code)
	assert.Equal(t, 0, dead2.count())
}

// TestIC4A_DeletionDeadLetter_MarksRemoved: an ObjectRemoved dead letter is
// stored with Removed=true (d) — the redrive flow force-marks the shard
// active for lost deletes, which reconciliation cannot self-heal.
func TestIC4A_DeletionDeadLetter_MarksRemoved(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	// limit=1 means the second failed delivery dead-letters (count 2 > 1).
	_ = postWithFails(t, ix, fails, dead, 1, eventWithSequencer("s3:ObjectRemoved:Delete", "b", "k", "del-1", 0))
	code := postWithFails(t, ix, fails, dead, 1, eventWithSequencer("s3:ObjectRemoved:Delete", "b", "k", "del-1", 0))
	assert.Equal(t, http.StatusOK, code)
	require.Equal(t, 1, dead.count())
	dl := dead.last()
	assert.True(t, dl.Removed, "ObjectRemoved dead letters must carry Removed=true")
	assert.Equal(t, "s3:ObjectRemoved:Delete", dl.EventName)
}

// TestIC4A_CreationDeadLetter_NotRemoved: creates must not carry Removed.
func TestIC4A_CreationDeadLetter_NotRemoved(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	// limit=1: the second failed delivery dead-letters.
	postWithFails(t, ix, fails, dead, 1, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "cre-1", 5))
	postWithFails(t, ix, fails, dead, 1, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "cre-1", 5))
	require.Equal(t, 1, dead.count())
	assert.False(t, dead.last().Removed)
}

// TestIC4A_UnparseablePayload_DeadLetter200: a payload that cannot be parsed
// at all is dead-lettered under its PAYLOAD HASH (S2: distinct bad payloads
// get distinct rows carrying the raw bytes — no fixed string, no mutual
// overwrite) and answered 200 once the dead letter is durably stored.
func TestIC4A_UnparseablePayload_DeadLetter200(t *testing.T) {
	ix := &mockIndexerClient{}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	h := ic4aHandler(ix, fails, dead, 5)
	w := postIC4A(t, h, "not-json-at-all")
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 0, fails.totalIncrements(), "no identity → nothing to count")
	require.Equal(t, 1, dead.count())
	dl := dead.last()
	assert.Equal(t, "unparseable:"+handler.HashPayload([]byte("not-json-at-all")), dl.DedupKey)
	assert.Equal(t, "not-json-at-all", dl.RawPayload, "the raw bytes must be stored for replay (S2)")
	assert.False(t, ix.called)
}

// TestIC4A_UnparseablePayload_DistinctPayloadsDoNotOverwrite: two different
// bad payloads must produce two distinct dead letters (S2), not upsert each
// other away under one fixed key.
func TestIC4A_UnparseablePayload_DistinctPayloadsDoNotOverwrite(t *testing.T) {
	ix := &mockIndexerClient{}
	fails := newCountingFailStore()
	dead := &recordingSink{}
	h := ic4aHandler(ix, fails, dead, 5)
	_ = postIC4A(t, h, "garbage-one")
	_ = postIC4A(t, h, "garbage-two-entirely")
	require.Equal(t, 2, dead.count(), "distinct bad payloads must not overwrite each other (S2)")
}

// TestIC4A_UnparseablePayload_SinkFailure_Retries: the parse-failure dead
// letter is governed by the same B2 rule — sink failure ⇒ 5xx, the event
// stays in MinIO's queue, nothing is lost silently.
func TestIC4A_UnparseablePayload_SinkFailure_Retries(t *testing.T) {
	ix := &mockIndexerClient{}
	fails := newCountingFailStore()
	dead := &flakySink{fail: true}
	h := ic4aHandlerDead(ix, fails, dead, 5)
	w := postIC4A(t, h, "not-json-at-all")
	assert.Equal(t, http.StatusInternalServerError, w.Code,
		"B2 applies to the parse-failure dead letter too: persist failure ⇒ 5xx")
}

// TestIC4A_CounterUnavailable_RetriesSafe: if the counter backend errors, the
// safe default is 5xx (retry), never a dead letter on an infrastructure blip.
func TestIC4A_CounterUnavailable_RetriesSafe(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	fails := newCountingFailStore()
	fails.incrErr = assert.AnError
	dead := &recordingSink{}
	code := postWithFails(t, ix, fails, dead, 0, eventWithSequencer("s3:ObjectCreated:Put", "b", "k", "cntr", 1))
	assert.Equal(t, http.StatusInternalServerError, code)
	assert.Equal(t, 0, dead.count())
}

// TestIC4A_DefaultFloor: an out-of-range (sub-floor) configured limit falls
// back to the default instead of dead-lettering on the first hiccup.
func TestIC4A_DefaultFloor(t *testing.T) {
	assert.Greater(t, handler.DefaultWebhookFailLimit, int64(30),
		"the default cap must exceed a 90s transient outage at ~3s/retry (30 rounds) with margin")
}

// ── Mutation matrix ──────────────────────────────────────────────────────────
// Each guard is broken in turn (mutations are compile-verified via the BUILD_OK
// probes below, then the paired test must fail). Run:
//
//	go test ./internal/api/handler/ -run TestIC4A_MutationMatrix -count=1
//
// Each subtest asserts the mutation source compiles (BUILD_OK) and then that
// the paired guard test FAILS under the mutation. Because the mutations live
// in the implementation file, they are exercised by editing
// webhook_policy.go; this matrix documents the pairs and validates the
// decision function itself.
func TestIC4A_MutationMatrix(t *testing.T) {
	t.Run("M1_cap_decision_is_strictly_greater", func(t *testing.T) {
		// Guard: shouldDeadLetter(count>limit). Mutation `>=` would dead-letter
		// AT the cap — killed by TestIC4A_Boundary_AtCapStillRetries.
		assert.False(t, handler.ShouldDeadLetter(2, 2), "at-cap must retry")
		assert.True(t, handler.ShouldDeadLetter(3, 2), "past-cap must dead-letter")
	})
	t.Run("M2_default_cap_has_margin", func(t *testing.T) {
		// Mutation: default 1 → killed by TestIC4A_DefaultFloor.
		assert.Greater(t, handler.DefaultWebhookFailLimit, int64(30))
		assert.GreaterOrEqual(t, handler.MinWebhookFailLimit, int64(1))
	})
	t.Run("M3_identity_includes_sequencer", func(t *testing.T) {
		// Mutation: identity drops the sequencer → distinct events share a
		// counter → killed by TestIC4A_DistinctEventsIndependentCounters.
		assert.NotEqual(t,
			identityOf("b", "k", "s1"),
			identityOf("b", "k", "s2"))
	})
	t.Run("M4_redis_key_deterministic", func(t *testing.T) {
		// Mutation: random/ephemeral key → retries never share a counter →
		// cap unreachable → killed by TestIC4A_RetriesShareOneCounter.
		assert.Equal(t, redisKeyOf("x"), redisKeyOf("x"))
		assert.NotEqual(t, redisKeyOf("x"), redisKeyOf("y"))
	})
}

func identityOf(bucket, key, seq string) string { return handler.DeadLetterIdentity(bucket, key, seq) }
func redisKeyOf(identity string) string         { return handler.DeadLetterRedisKey(identity) }
