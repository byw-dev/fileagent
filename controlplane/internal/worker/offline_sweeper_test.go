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
)

// ── mocks ───────────────────────────────────────────────────────────────────

type mockStatusDB struct {
	agents      []*db.Agent
	listErr     error
	updateErr   error
	updatedIDs  []uuid.UUID
	updateCalls int
}

func (m *mockStatusDB) ListAgentsByStatus(_ context.Context, _ uuid.UUID, _ db.AgentStatus) ([]*db.Agent, error) {
	if m.listErr != nil {
		return nil, m.listErr
	}
	return m.agents, nil
}

func (m *mockStatusDB) UpdateAgentStatus(_ context.Context, id uuid.UUID, _ db.AgentStatus) (*db.Agent, error) {
	m.updateCalls++
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	m.updatedIDs = append(m.updatedIDs, id)
	return &db.Agent{ID: id, Status: db.AgentStatusOffline}, nil
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
	assert.Equal(t, []uuid.UUID{a.ID}, sdb.updatedIDs)
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
	assert.Zero(t, sdb.updateCalls)
	assert.Empty(t, pub.subjects)
}

func TestSweep_NoOnlineAgents(t *testing.T) {
	sdb := &mockStatusDB{agents: nil}
	c := &mockCache{results: []int64{0}}
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	assert.Equal(t, 0, n)
	assert.Zero(t, sdb.updateCalls)
	assert.Empty(t, pub.subjects)
}

func TestSweep_ListErrorReturnsZero(t *testing.T) {
	sdb := &mockStatusDB{listErr: errors.New("db down")}
	c := &mockCache{results: []int64{0}}
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	assert.Equal(t, 0, n)
	assert.Zero(t, sdb.updateCalls)
}

func TestSweep_UpdateErrorContinuesToNextAgent(t *testing.T) {
	a1, a2 := onlineAgent(), onlineAgent()
	sdb := &mockStatusDB{agents: []*db.Agent{a1, a2}, updateErr: errors.New("update failed")}
	c := &mockCache{results: []int64{0}} // all expired
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	// Both fail to update, but the loop must attempt both and never publish.
	assert.Equal(t, 0, n)
	assert.Equal(t, 2, sdb.updateCalls)
	assert.Empty(t, pub.subjects)
}

func TestSweep_PublishErrorStillCountsAndContinues(t *testing.T) {
	a1, a2 := onlineAgent(), onlineAgent()
	sdb := &mockStatusDB{agents: []*db.Agent{a1, a2}}
	c := &mockCache{results: []int64{0}}
	pub := &mockPublisher{err: errors.New("nats down")}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	// Status was updated for both; publish failures are logged, not fatal.
	assert.Equal(t, 2, n)
	assert.Len(t, sdb.updatedIDs, 2)
	assert.Len(t, pub.subjects, 2)
}

func TestSweep_RecheckRaceKeepsReconnectedAgentOnline(t *testing.T) {
	a := onlineAgent()
	sdb := &mockStatusDB{agents: []*db.Agent{a}}
	// First check: expired (0). Recheck: reappeared (1) → agent reconnected.
	c := &mockCache{results: []int64{0, 1}}
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	assert.Equal(t, 0, n)
	assert.Zero(t, sdb.updateCalls, "reconnected agent must not be marked offline")
	assert.Empty(t, pub.subjects)
}

func TestSweep_CacheErrorFailsSafe(t *testing.T) {
	a := onlineAgent()
	sdb := &mockStatusDB{agents: []*db.Agent{a}}
	c := &mockCache{err: errors.New("redis down")}
	pub := &mockPublisher{}

	n := newSweeper(sdb, c, pub).Sweep(context.Background())

	assert.Equal(t, 0, n, "must not mark offline when presence is unknown")
	assert.Zero(t, sdb.updateCalls)
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
	defer cancel()
	go newSweeper(sdb, c, pub).Run(ctx, 10*time.Millisecond)

	require.Eventually(t, func() bool {
		return published.Load() >= 1
	}, 2*time.Second, 10*time.Millisecond, "sweeper should mark the expired agent offline on tick")
}

// countingPublisher counts publishes without racing on a slice.
type countingPublisher struct{ n *atomic.Int32 }

func (p *countingPublisher) Publish(_ string, _ []byte) error {
	p.n.Add(1)
	return nil
}
