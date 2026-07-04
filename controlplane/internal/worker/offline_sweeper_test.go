package worker

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// ── mocks ───────────────────────────────────────────────────────────────────

type mockStatusDB struct {
	agents       []*db.Agent
	listErr      error
	markErr      error
	markZeroRows bool // simulate "already offline" (0 rows affected → no transition)
	markedIDs    []uuid.UUID
	markCalls    int
}

func (m *mockStatusDB) ListAgentsByStatus(_ context.Context, _ uuid.UUID, _ db.AgentStatus) ([]*db.Agent, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.agents, nil
}

func (m *mockStatusDB) MarkAgentOfflineIfOnline(_ context.Context, id uuid.UUID) (int64, error) {
	m.markCalls++
	if m.markErr != nil {
		return 0, m.markErr
	}
	if m.markZeroRows {
		return 0, nil // already offline elsewhere
	}
	m.markedIDs = append(m.markedIDs, id)
	return 1, nil
}

// mockCache returns queued Exists results in order; when exhausted it repeats the
// last value. This lets a test simulate "expired, then reappeared" for the recheck.
type mockCache struct {
	results []int64
	err     error
	calls   int32
}

func (m *mockCache) Exists(_ context.Context, _ ...string) (int64, error) {
	m.calls++
	if m.err != nil {
		return 0, m.err
	}
	i := int(m.calls) - 1
	if i >= len(m.results) {
		i = len(m.results) - 1
	}
	return m.results[i], nil
}

type mockPublisher struct {
	subjects []string
	payloads [][]byte
	err      error
}

func (m *mockPublisher) Publish(subject string, data []byte) error {
	m.subjects = append(m.subjects, subject)
	m.payloads = append(m.payloads, data)
	return m.err
}

func onlineAgent() *db.Agent {
	return &db.Agent{ID: uuid.New(), Status: db.AgentStatusOnline}
}

func newSweeper(sdb StatusDB, c PresenceCache, p EventPublisher) *OfflineSweeper {
	return NewOfflineSweeper(sdb, c, p, uuid.New(), zap.NewNop())
}

// ── tests ───────────────────────────────────────────────────────────────────

func TestSweep_MarksOfflineWhenPresenceExpired(t *testing.T) {
	a := onlineAgent()
	sdb := &mockStatusDB{agents: []*db.Agent{a}}
	c := &mockCache{results: []int64{0}} // key gone (both checks)
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	assert.Equal(t, 1, n)
	assert.Equal(t, []uuid.UUID{a.ID}, sdb.markedIDs)
	require.Len(t, pub.subjects, 1)
	assert.Equal(t, "events.agent.offline", pub.subjects[0])
	assert.JSONEq(t, `{"agent_id":"`+a.ID.String()+`"}`, string(pub.payloads[0]))
}

func TestSweep_SkipsWhenStillPresent(t *testing.T) {
	a := onlineAgent()
	sdb := &mockStatusDB{agents: []*db.Agent{a}}
	c := &mockCache{results: []int64{1}} // key still present
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	assert.Equal(t, 0, n)
	assert.Zero(t, sdb.markCalls)
	assert.Empty(t, pub.subjects)
}

func TestSweep_NoOnlineAgents(t *testing.T) {
	sdb := &mockStatusDB{agents: nil}
	c := &mockCache{results: []int64{0}}
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	assert.Equal(t, 0, n)
	assert.Zero(t, sdb.markCalls)
	assert.Empty(t, pub.subjects)
}

func TestSweep_ListErrorReturnsZero(t *testing.T) {
	sdb := &mockStatusDB{listErr: errors.New("db down")}
	c := &mockCache{results: []int64{0}}
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	assert.Equal(t, 0, n)
	assert.Zero(t, sdb.markCalls)
}

func TestSweep_MarkErrorContinuesToNextAgent(t *testing.T) {
	a1, a2 := onlineAgent(), onlineAgent()
	sdb := &mockStatusDB{agents: []*db.Agent{a1, a2}, markErr: errors.New("update failed")}
	c := &mockCache{results: []int64{0}} // all expired
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	// Both fail to update, but the loop must attempt both and never publish.
	assert.Equal(t, 0, n)
	assert.Equal(t, 2, sdb.markCalls)
	assert.Empty(t, pub.subjects)
}

// When the gRPC disconnect path already marked the agent offline, the conditional
// update affects 0 rows and the sweeper must NOT publish a duplicate offline event.
func TestSweep_NoDuplicateWhenAlreadyOffline(t *testing.T) {
	a := onlineAgent()
	sdb := &mockStatusDB{agents: []*db.Agent{a}, markZeroRows: true}
	c := &mockCache{results: []int64{0}} // presence expired
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	assert.Equal(t, 0, n)
	assert.Equal(t, 1, sdb.markCalls, "sweeper attempts the conditional transition")
	assert.Empty(t, pub.subjects, "no duplicate event when another path already set offline")
}

func TestSweep_PublishErrorStillCountsAndContinues(t *testing.T) {
	a1, a2 := onlineAgent(), onlineAgent()
	sdb := &mockStatusDB{agents: []*db.Agent{a1, a2}}
	c := &mockCache{results: []int64{0}}
	pub := &mockPublisher{err: errors.New("nats down")}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	// Status was updated for both; publish failures are logged, not fatal.
	assert.Equal(t, 2, n)
	assert.Len(t, sdb.markedIDs, 2)
	assert.Len(t, pub.subjects, 2)
}

func TestSweep_StopsEarlyOnContextCancel(t *testing.T) {
	sdb := &mockStatusDB{agents: []*db.Agent{onlineAgent(), onlineAgent()}}
	c := &mockCache{results: []int64{0}}
	pub := &mockPublisher{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // already cancelled: the loop must exit before touching any agent

	n := newSweeper(sdb, c, pub).Sweep(ctx)

	assert.Equal(t, 0, n)
	assert.Zero(t, sdb.markCalls, "no agents processed once ctx is cancelled")
}

func TestSweep_ListErrorQuietOnCancel(t *testing.T) {
	sdb := &mockStatusDB{listErr: context.Canceled}
	c := &mockCache{results: []int64{0}}
	pub := &mockPublisher{}

	core, logs := observer.New(zapcore.WarnLevel)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	s := NewOfflineSweeper(sdb, c, pub, uuid.New(), zap.New(core))
	assert.Equal(t, 0, s.Sweep(ctx))
	assert.Zero(t, logs.Len(), "a cancelled-context list error during shutdown must not warn")
}

func TestSweep_CacheErrorFailsSafe(t *testing.T) {
	a := onlineAgent()
	sdb := &mockStatusDB{agents: []*db.Agent{a}}
	c := &mockCache{err: errors.New("redis down")}
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	assert.Equal(t, 0, n, "must not mark offline when presence is unknown")
	assert.Zero(t, sdb.markCalls)
}

func TestRun_StopsOnContextCancel(t *testing.T) {
	sdb := &mockStatusDB{}
	c := &mockCache{results: []int64{1}}
	pub := &mockPublisher{}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		newSweeper(sdb, c, pub).Run(ctx, 10*time.Millisecond)
		close(done)
	}()

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancel")
	}
}

func TestRun_SweepsOnTick(t *testing.T) {
	a := onlineAgent()
	sdb := &mockStatusDB{agents: []*db.Agent{a}}
	c := &mockCache{results: []int64{0}}
	var published atomic.Int32
	pub := &countingPublisher{n: &published}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		newSweeper(sdb, c, pub).Run(ctx, 10*time.Millisecond)
		close(done)
	}()

	require.Eventually(t, func() bool {
		return published.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond, "sweeper should mark the expired agent offline on tick")

	// Stop the loop and wait for it to exit so no goroutine leaks into later tests.
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not stop after context cancel")
	}
}

// countingPublisher counts publishes without racing on a slice.
type countingPublisher struct{ n *atomic.Int32 }

func (p *countingPublisher) Publish(_ string, _ []byte) error {
	p.n.Add(1)
	return nil
}
