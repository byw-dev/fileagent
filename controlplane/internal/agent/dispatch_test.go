package agent

import (
	"context"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Mock implementations ────────────────────────────────────────────────────

type mockDispatchDB struct {
	rules []*db.CollectionRule
}

func (m *mockDispatchDB) ListCollectionRulesByAgent(_ context.Context, _ uuid.UUID) ([]*db.CollectionRule, error) {
	return m.rules, nil
}

type mockBucketQuerier struct {
	buckets map[uuid.UUID]*db.Bucket
}

func newMockBucketQuerier() *mockBucketQuerier {
	return &mockBucketQuerier{buckets: make(map[uuid.UUID]*db.Bucket)}
}

func (m *mockBucketQuerier) GetBucketByID(_ context.Context, id uuid.UUID) (*db.Bucket, error) {
	if b, ok := m.buckets[id]; ok {
		return b, nil
	}
	return &db.Bucket{ID: id, Name: "test-bucket"}, nil
}

type mockDispatchCache struct {
	keys map[string]bool
}

func newMockDispatchCache() *mockDispatchCache {
	return &mockDispatchCache{keys: make(map[string]bool)}
}

func (m *mockDispatchCache) SetNX(_ context.Context, key string, _ interface{}, _ time.Duration) (bool, error) {
	if m.keys[key] {
		return false, nil
	}
	m.keys[key] = true
	return true, nil
}

func (m *mockDispatchCache) Del(_ context.Context, keys ...string) error {
	for _, k := range keys {
		delete(m.keys, k)
	}
	return nil
}

type mockRegistry struct {
	online map[string]bool
	sent   []*agentv1.ServerMessage
}

func newMockRegistry() *mockRegistry {
	return &mockRegistry{online: make(map[string]bool)}
}

func (r *mockRegistry) IsOnline(agentID string) bool { return r.online[agentID] }
func (r *mockRegistry) Send(agentID string, msg *agentv1.ServerMessage) bool {
	r.sent = append(r.sent, msg)
	return true
}

// mockCredentialPusher records PushCredentials invocations made by the
// Dispatcher when an agent's bucket set changes (IC-BUG-20).
type mockCredentialPusher struct {
	agentIDs []string
}

func (p *mockCredentialPusher) PushCredentials(_ context.Context, agentID string) {
	p.agentIDs = append(p.agentIDs, agentID)
}

// ── Tests ────────────────────────────────────────────────────────────────────

func newTestDispatcher(t *testing.T) (*Dispatcher, *mockDispatchDB, *mockDispatchCache, *mockRegistry) {
	t.Helper()
	dispDB := &mockDispatchDB{}
	dispCache := newMockDispatchCache()
	reg := newMockRegistry()
	buckets := newMockBucketQuerier()
	logger, _ := zap.NewDevelopment()
	d := NewDispatcher(dispDB, buckets, dispCache, reg, logger)
	return d, dispDB, dispCache, reg
}

func TestDispatchRule_AgentOffline(t *testing.T) {
	d, _, _, reg := newTestDispatcher(t)
	reg.online["agent-1"] = false

	rule := &db.CollectionRule{
		ID:      uuid.New(),
		AgentID: uuid.New(),
		Status:  db.RuleStatusActive,
	}

	err := d.DispatchRule(context.Background(), rule)
	require.NoError(t, err)
	assert.Empty(t, reg.sent)
}

func TestDispatchRule_AgentOnline(t *testing.T) {
	d, _, _, reg := newTestDispatcher(t)
	agentID := uuid.New()
	reg.online[agentID.String()] = true

	rule := &db.CollectionRule{
		ID:      uuid.New(),
		AgentID: agentID,
		Status:  db.RuleStatusActive,
		Name:    "my-rule",
	}

	err := d.DispatchRule(context.Background(), rule)
	require.NoError(t, err)
	require.Len(t, reg.sent, 1)
	assert.NotNil(t, reg.sent[0].GetPushRule())
	// BUG-2 regression: upload_bucket must be non-empty.
	assert.NotEmpty(t, reg.sent[0].GetPushRule().GetRule().GetUploadBucket())
}

func TestDispatchRuleCancel(t *testing.T) {
	d, _, _, reg := newTestDispatcher(t)
	agentID := uuid.New()
	reg.online[agentID.String()] = true

	err := d.DispatchRuleCancel(context.Background(), uuid.New().String(), agentID.String())
	require.NoError(t, err)
	require.Len(t, reg.sent, 1)
	assert.NotNil(t, reg.sent[0].GetCancelRule())
}

func TestSyncRulesOnConnect(t *testing.T) {
	d, dispDB, _, reg := newTestDispatcher(t)
	agentID := uuid.New()
	reg.online[agentID.String()] = true

	dispDB.rules = []*db.CollectionRule{
		{ID: uuid.New(), AgentID: agentID, Name: "rule-1", Status: db.RuleStatusActive},
		{ID: uuid.New(), AgentID: agentID, Name: "rule-2", Status: db.RuleStatusInactive},
	}

	err := d.SyncRulesOnConnect(context.Background(), agentID.String())
	require.NoError(t, err)
	// Only the active rule should be sent.
	assert.Len(t, reg.sent, 1)
}

func TestDispatchRule_LockAlreadyHeld(t *testing.T) {
	d, _, cache, reg := newTestDispatcher(t)
	agentID := uuid.New()
	reg.online[agentID.String()] = true
	rule := &db.CollectionRule{
		ID:      uuid.New(),
		AgentID: agentID,
		Status:  db.RuleStatusActive,
	}

	// First dispatch acquires the lock
	require.NoError(t, d.DispatchRule(context.Background(), rule))
	assert.Len(t, reg.sent, 1)

	// Manually re-add the lock key so the second call finds it held
	cache.keys["lock:rule_dispatch:"+rule.ID.String()] = true
	require.NoError(t, d.DispatchRule(context.Background(), rule))
	// Still only 1 message sent (second call was skipped)
	assert.Len(t, reg.sent, 1)
}

func TestDispatchRuleCancel_AgentOffline(t *testing.T) {
	d, _, _, reg := newTestDispatcher(t)
	reg.online["agent-1"] = false
	err := d.DispatchRuleCancel(context.Background(), uuid.New().String(), "agent-1")
	require.NoError(t, err)
	assert.Empty(t, reg.sent)
}

// IC-BUG-20: dispatching a rule whose bucket is not yet covered by the agent's
// other active rules must re-push credentials, otherwise the agent keeps
// putting 403s for up to ~50 minutes on the old STS session.
func TestDispatchRule_NewBucket_TriggersCredentialPush(t *testing.T) {
	d, dispDB, _, reg := newTestDispatcher(t)
	pusher := &mockCredentialPusher{}
	d.SetCredentialPusher(pusher)
	agentID := uuid.New()
	bucketA, bucketB := uuid.New(), uuid.New()
	reg.online[agentID.String()] = true
	dispDB.rules = []*db.CollectionRule{
		{ID: uuid.New(), AgentID: agentID, BucketID: bucketA, Status: db.RuleStatusActive},
	}

	rule := &db.CollectionRule{
		ID:       uuid.New(),
		AgentID:  agentID,
		BucketID: bucketB,
		Status:   db.RuleStatusActive,
	}
	require.NoError(t, d.DispatchRule(context.Background(), rule))
	require.Len(t, reg.sent, 1)
	require.Len(t, pusher.agentIDs, 1, "a rule pointing at a new bucket must trigger a credentials re-push")
	assert.Equal(t, agentID.String(), pusher.agentIDs[0])
}

// IC-BUG-20 coupling check: a rule whose bucket is already covered by the
// agent's other active rules must NOT re-push credentials — the held session
// already covers it and re-issuing on every rule change would churn STS.
func TestDispatchRule_SameBucket_NoCredentialPush(t *testing.T) {
	d, dispDB, _, reg := newTestDispatcher(t)
	pusher := &mockCredentialPusher{}
	d.SetCredentialPusher(pusher)
	agentID := uuid.New()
	bucket := uuid.New()
	reg.online[agentID.String()] = true
	dispDB.rules = []*db.CollectionRule{
		{ID: uuid.New(), AgentID: agentID, BucketID: bucket, Status: db.RuleStatusActive},
	}

	rule := &db.CollectionRule{
		ID:       uuid.New(),
		AgentID:  agentID,
		BucketID: bucket,
		Status:   db.RuleStatusActive,
	}
	require.NoError(t, d.DispatchRule(context.Background(), rule))
	require.Len(t, reg.sent, 1)
	assert.Empty(t, pusher.agentIDs, "covered bucket must not trigger a credentials re-push")
}

// IC-BUG-20: the re-push only makes sense for rules that add a bucket, i.e.
// active ones; inactive rules are cancelled, not dispatched.
func TestDispatchRule_InactiveRule_NoCredentialPush(t *testing.T) {
	d, _, _, reg := newTestDispatcher(t)
	pusher := &mockCredentialPusher{}
	d.SetCredentialPusher(pusher)
	agentID := uuid.New()
	reg.online[agentID.String()] = true

	rule := &db.CollectionRule{
		ID:       uuid.New(),
		AgentID:  agentID,
		BucketID: uuid.New(),
		Status:   db.RuleStatusInactive,
	}
	require.NoError(t, d.DispatchRule(context.Background(), rule))
	assert.Empty(t, pusher.agentIDs)
}

func TestSyncRulesOnConnect_InvalidAgentID(t *testing.T) {
	d, _, _, _ := newTestDispatcher(t)
	err := d.SyncRulesOnConnect(context.Background(), "not-a-uuid")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid agent_id")
}

type errDispatchDB struct{ err error }

func (e *errDispatchDB) ListCollectionRulesByAgent(_ context.Context, _ uuid.UUID) ([]*db.CollectionRule, error) {
	return nil, e.err
}

func TestSyncRulesOnConnect_DBError(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	d := NewDispatcher(&errDispatchDB{err: assert.AnError}, newMockBucketQuerier(), newMockDispatchCache(), newMockRegistry(), logger)
	err := d.SyncRulesOnConnect(context.Background(), uuid.New().String())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "list rules")
}

type errBucketQuerier struct{ err error }

func (e *errBucketQuerier) GetBucketByID(_ context.Context, _ uuid.UUID) (*db.Bucket, error) {
	return nil, e.err
}

func TestDispatchRule_BucketLookupError(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	agentID := uuid.New()
	reg := newMockRegistry()
	reg.online[agentID.String()] = true
	d := NewDispatcher(&mockDispatchDB{}, &errBucketQuerier{err: assert.AnError}, newMockDispatchCache(), reg, logger)

	rule := &db.CollectionRule{
		ID:      uuid.New(),
		AgentID: agentID,
		Status:  db.RuleStatusActive,
	}
	err := d.DispatchRule(context.Background(), rule)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "lookup bucket")
	assert.Empty(t, reg.sent)
}

func TestSyncRulesOnConnect_BucketLookupError_SkipsRule(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	agentID := uuid.New()
	reg := newMockRegistry()
	reg.online[agentID.String()] = true
	dispDB := &mockDispatchDB{
		rules: []*db.CollectionRule{
			{ID: uuid.New(), AgentID: agentID, Status: db.RuleStatusActive},
		},
	}
	d := NewDispatcher(dispDB, &errBucketQuerier{err: assert.AnError}, newMockDispatchCache(), reg, logger)
	// Should not return an error; rules with failed bucket lookup are skipped.
	err := d.SyncRulesOnConnect(context.Background(), agentID.String())
	require.NoError(t, err)
	assert.Empty(t, reg.sent)
}
