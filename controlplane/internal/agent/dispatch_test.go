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
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
	// sendFails simulates a full SendCh / disconnected agent: Send reports
	// failure (IC-BUG-30 rules_sync + IC-BUG-31 DispatchRule propagation).
	sendFails bool
}

func newMockRegistry() *mockRegistry {
	return &mockRegistry{online: make(map[string]bool)}
}

func (r *mockRegistry) IsOnline(agentID string) bool { return r.online[agentID] }
func (r *mockRegistry) Send(agentID string, msg *agentv1.ServerMessage) bool {
	if r.sendFails {
		return false
	}
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

	active := &db.CollectionRule{ID: uuid.New(), AgentID: agentID, Name: "rule-1", Status: db.RuleStatusActive}
	inactive := &db.CollectionRule{ID: uuid.New(), AgentID: agentID, Name: "rule-2", Status: db.RuleStatusInactive}
	dispDB.rules = []*db.CollectionRule{active, inactive}

	err := d.SyncRulesOnConnect(context.Background(), agentID.String())
	require.NoError(t, err)
	// The sync is ONE snapshot message (D-033 snapshot form): per-rule pushes
	// burst-overflow the bounded send buffer at >32 rules.
	require.Len(t, reg.sent, 1)
	syncMsg := reg.sent[0]
	require.NotNil(t, syncMsg.GetRulesSync(), "the sync must be a single snapshot message")
	rules := syncMsg.GetRulesSync().GetRules()
	require.Len(t, rules, 2)
	snapshot := map[string]bool{}
	for _, r := range rules {
		snapshot[r.GetRuleId()] = r.GetEnabled()
	}
	assert.True(t, snapshot[active.ID.String()], "active rule in snapshot, enabled")
	assert.False(t, snapshot[inactive.ID.String()],
		"inactive rule in snapshot too — the agent stops it via applyRule's Enabled=false branch")
}

// The snapshot must be hole-free: a rule whose bucket cannot be resolved must
// fail the whole sync (connection ends, agent reconnects and resyncs) rather
// than be omitted — an omitted rule exists in the DB, and the agent would stop
// a rule that actually still runs.
func TestSyncRulesOnConnect_BucketLookupError_FailsSync(t *testing.T) {
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
	err := d.SyncRulesOnConnect(context.Background(), agentID.String())
	require.Error(t, err, "a hole-free snapshot must not silently drop a DB-existing rule")
	assert.Empty(t, reg.sent, "no snapshot may be delivered with holes")
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

// 硬化 1 / D-033：快照 Send 失败必须区分「缓冲满」与「agent 已断连」。
// 「断连重试」的安全性建立在「这条消息失败是罕见的」之上——若快照大到缓冲
// 装不下，每次重连都会必然失败，退化为重连循环。日志必须一眼分得出。
func TestSyncRulesOnConnect_SnapshotSendFailed_ChannelFull_LogsCause(t *testing.T) {
	core, logs := observer.New(zapcore.ErrorLevel)
	logger := zap.New(core)
	agentID := uuid.New()
	dispDB := &mockDispatchDB{
		rules: []*db.CollectionRule{
			{ID: uuid.New(), AgentID: agentID, Status: db.RuleStatusActive},
		},
	}
	reg := newMockRegistry()
	reg.online[agentID.String()] = true
	reg.sendFails = true
	d := NewDispatcher(dispDB, newMockBucketQuerier(), newMockDispatchCache(), reg, logger)

	err := d.SyncRulesOnConnect(context.Background(), agentID.String())
	require.Error(t, err)
	entries := logs.All()
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Message, "send channel full")
	assert.NotContains(t, entries[0].Message, "agent disconnected")
}

func TestSyncRulesOnConnect_SnapshotSendFailed_Disconnected_LogsCause(t *testing.T) {
	core, logs := observer.New(zapcore.ErrorLevel)
	logger := zap.New(core)
	agentID := uuid.New()
	dispDB := &mockDispatchDB{
		rules: []*db.CollectionRule{
			{ID: uuid.New(), AgentID: agentID, Status: db.RuleStatusActive},
		},
	}
	reg := newMockRegistry()
	reg.online[agentID.String()] = false
	reg.sendFails = true
	d := NewDispatcher(dispDB, newMockBucketQuerier(), newMockDispatchCache(), reg, logger)

	err := d.SyncRulesOnConnect(context.Background(), agentID.String())
	require.Error(t, err)
	entries := logs.All()
	require.Len(t, entries, 1)
	assert.Contains(t, entries[0].Message, "agent disconnected")
}

// IC-BUG-20 边界：bucket 集合算不出来（DB 列规则失败）时，只告警不报错——
// DispatchRule 本体已成功，凭据重推是尽力而为。
func TestDispatchRule_BucketSetComputationError_NoPush_NoError(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	agentID := uuid.New()
	reg := newMockRegistry()
	reg.online[agentID.String()] = true
	d := NewDispatcher(&errDispatchDB{err: assert.AnError}, newMockBucketQuerier(), newMockDispatchCache(), reg, logger)
	pusher := &mockCredentialPusher{}
	d.SetCredentialPusher(pusher)

	rule := &db.CollectionRule{ID: uuid.New(), AgentID: agentID, BucketID: uuid.New(), Status: db.RuleStatusActive}
	require.NoError(t, d.DispatchRule(context.Background(), rule))
	require.Len(t, reg.sent, 1)
	assert.Empty(t, pusher.agentIDs, "uncomputable bucket set must not re-push credentials")
}

func TestOtherRulesCoverBucket_DBError(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	d := NewDispatcher(&errDispatchDB{err: assert.AnError}, newMockBucketQuerier(), newMockDispatchCache(), newMockRegistry(), logger)
	covers, err := d.otherRulesCoverBucket(context.Background(), uuid.NewString(), &db.CollectionRule{ID: uuid.New()})
	require.Error(t, err)
	assert.False(t, covers)
}

func TestOtherRulesCoverBucket_InvalidAgentID(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	d := NewDispatcher(&mockDispatchDB{}, newMockBucketQuerier(), newMockDispatchCache(), newMockRegistry(), logger)
	covers, err := d.otherRulesCoverBucket(context.Background(), "not-a-uuid", &db.CollectionRule{ID: uuid.New()})
	require.Error(t, err)
	assert.False(t, covers)
}
