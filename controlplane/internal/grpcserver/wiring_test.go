package grpcserver

import (
	"context"
	"testing"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/storage"
	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── Mock DispatcherClient ─────────────────────────────────────────────────────

type mockDispatcher struct {
	synced []string
	err    error
}

func (m *mockDispatcher) SyncRulesOnConnect(_ context.Context, agentID string) error {
	m.synced = append(m.synced, agentID)
	return m.err
}

// ── Mock IndexerClient ────────────────────────────────────────────────────────

type mockIndexer struct {
	called bool
	err    error
}

func (m *mockIndexer) HandleUploadResult(_ context.Context, _ uuid.UUID, _ uuid.UUID, _ *agentv1.UploadResult) error {
	m.called = true
	return m.err
}

// ── Mock STSManagerClient ─────────────────────────────────────────────────────

type mockSTSMgr struct {
	creds *agentv1.CredentialsPayload
	err   error
	// gotAgentID / gotBuckets record the last call so tests can assert on the
	// scope actually requested, not merely that a call happened.
	gotAgentID string
	gotBuckets []storage.BucketAccess
	calls      int
}

func (m *mockSTSMgr) IssueCredentials(_ context.Context, agentID string, buckets []storage.BucketAccess) (*agentv1.CredentialsPayload, error) {
	m.calls++
	m.gotAgentID = agentID
	m.gotBuckets = buckets
	return m.creds, m.err
}

// testAgentID is the authenticated agent used by the credential tests.
var testAgentID = uuid.NewString()

// agentCtx returns a context carrying verified JWT claims for agentID, matching
// what the gRPC interceptor installs on a real call.
func agentCtx(agentID string) context.Context {
	return context.WithValue(context.Background(), claimsContextKey,
		&auth.Claims{RegisteredClaims: jwt.RegisteredClaims{Subject: agentID}})
}

// ── Mock CredentialDB ─────────────────────────────────────────────────────────

type mockCredDB struct {
	rule     *db.CollectionRule
	ruleErr  error
	bucket   *db.Bucket
	buckErr  error
	rules    []*db.CollectionRule
	rulesErr error
}

func (m *mockCredDB) GetCollectionRuleByID(_ context.Context, _ uuid.UUID) (*db.CollectionRule, error) {
	return m.rule, m.ruleErr
}

func (m *mockCredDB) GetBucketByID(_ context.Context, _ uuid.UUID) (*db.Bucket, error) {
	return m.bucket, m.buckErr
}

func (m *mockCredDB) ListCollectionRulesByAgent(_ context.Context, _ uuid.UUID) ([]*db.CollectionRule, error) {
	return m.rules, m.rulesErr
}

// ── RefreshCredentials ────────────────────────────────────────────────────────

func TestRefreshCredentials_NoSTSMgr_ReturnsUnimplemented(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)

	_, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{
		AgentId: testAgentID,
		RuleId:  uuid.New().String(),
	})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestRefreshCredentials_InvalidRuleID_ReturnsInvalidArgument(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	srv.WithExtraDeps(nil, nil, &mockSTSMgr{}, &mockCredDB{})

	_, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{
		AgentId: testAgentID,
		RuleId:  "not-a-uuid",
	})
	require.Error(t, err)
	assert.Equal(t, codes.InvalidArgument, status.Code(err))
}

func TestRefreshCredentials_RuleNotFound_ReturnsNotFound(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	credDB := &mockCredDB{ruleErr: assert.AnError}
	srv.WithExtraDeps(nil, nil, &mockSTSMgr{}, credDB)

	_, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{
		AgentId: testAgentID,
		RuleId:  uuid.New().String(),
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestRefreshCredentials_BucketNotFound_ReturnsNotFound(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	bucketID := uuid.New()
	credDB := &mockCredDB{
		rule:    &db.CollectionRule{ID: uuid.New(), AgentID: uuid.MustParse(testAgentID), BucketID: bucketID},
		buckErr: assert.AnError,
	}
	srv.WithExtraDeps(nil, nil, &mockSTSMgr{}, credDB)

	_, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{
		AgentId: testAgentID,
		RuleId:  uuid.New().String(),
	})
	require.Error(t, err)
	assert.Equal(t, codes.NotFound, status.Code(err))
}

func TestRefreshCredentials_STSError_ReturnsInternal(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	bucketID := uuid.New()
	credDB := &mockCredDB{
		rule:   &db.CollectionRule{ID: uuid.New(), AgentID: uuid.MustParse(testAgentID), BucketID: bucketID},
		bucket: &db.Bucket{ID: bucketID, Name: "data-sensor"},
	}
	stsMgr := &mockSTSMgr{err: assert.AnError}
	srv.WithExtraDeps(nil, nil, stsMgr, credDB)

	_, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{
		AgentId: testAgentID,
		RuleId:  uuid.New().String(),
	})
	require.Error(t, err)
	assert.Equal(t, codes.Internal, status.Code(err))
}

func TestRefreshCredentials_Success(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	bucketID := uuid.New()
	creds := &agentv1.CredentialsPayload{AccessKey: "AKID", SecretKey: "SECRET"}
	credDB := &mockCredDB{
		rule:   &db.CollectionRule{ID: uuid.New(), AgentID: uuid.MustParse(testAgentID), BucketID: bucketID},
		bucket: &db.Bucket{ID: bucketID, Name: "data-sensor"},
	}
	stsMgr := &mockSTSMgr{creds: creds}
	srv.WithExtraDeps(nil, nil, stsMgr, credDB)

	resp, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{
		AgentId: testAgentID,
		RuleId:  uuid.New().String(),
	})
	require.NoError(t, err)
	require.NotNil(t, resp.Credentials)
	assert.Equal(t, "AKID", resp.Credentials.AccessKey)
}

// ── handleUploadResult with Indexer ──────────────────────────────────────────

func TestHandleUploadResult_WithIndexer_CallsIndexer(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	ix := &mockIndexer{}
	agentUUID := uuid.New()
	srv.WithExtraDeps(nil, ix, nil, nil)

	// Inject valid agent_id via claims in context.
	claims := &auth.Claims{}
	claims.Subject = agentUUID.String()
	claims.OrgID = "00000000-0000-0000-0000-000000000001"
	ctx := context.WithValue(context.Background(), claimsContextKey, claims)

	srv.handleUploadResult(ctx, agentUUID.String(), &agentv1.UploadResult{
		StoragePath: "uploads/file.txt",
		Success:     true,
	})

	assert.True(t, ix.called)
}

func TestHandleUploadResult_InvalidAgentID_DoesNotCallIndexer(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	ix := &mockIndexer{}
	srv.WithExtraDeps(nil, ix, nil, nil)

	srv.handleUploadResult(context.Background(), "not-a-uuid", &agentv1.UploadResult{})
	assert.False(t, ix.called)
}

func TestHandleUploadResult_NilIndexer_DoesNotPanic(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	// no indexer injected

	assert.NotPanics(t, func() {
		srv.handleUploadResult(context.Background(), uuid.New().String(), &agentv1.UploadResult{
			StoragePath: "path/file.txt",
			Success:     true,
		})
	})
}

// ── extractOrgID ──────────────────────────────────────────────────────────────

func TestExtractOrgID_ValidClaims(t *testing.T) {
	orgID := uuid.New()
	claims := &auth.Claims{}
	claims.OrgID = orgID.String()
	ctx := context.WithValue(context.Background(), claimsContextKey, claims)

	got := extractOrgID(ctx)
	assert.Equal(t, orgID, got)
}

func TestExtractOrgID_EmptyContext_ReturnsDefault(t *testing.T) {
	got := extractOrgID(context.Background())
	assert.Equal(t, defaultOrgID, got)
}

func TestExtractOrgID_InvalidOrgID_ReturnsDefault(t *testing.T) {
	claims := &auth.Claims{}
	claims.OrgID = "not-a-uuid"
	ctx := context.WithValue(context.Background(), claimsContextKey, claims)

	got := extractOrgID(ctx)
	assert.Equal(t, defaultOrgID, got)
}

// ── WithExtraDeps ─────────────────────────────────────────────────────────────

func TestWithExtraDeps_SetsAllFields(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)

	d := &mockDispatcher{}
	ix := &mockIndexer{}
	stsMgr := &mockSTSMgr{}
	credDB := &mockCredDB{}

	srv.WithExtraDeps(d, ix, stsMgr, credDB)

	assert.NotNil(t, srv.dispatcher)
	assert.NotNil(t, srv.indexer)
	assert.NotNil(t, srv.stsMgr)
	assert.NotNil(t, srv.credDB)
}

// ── RefreshCredentials: identity is taken from the token, never the body ──────

// An approved agent must not be able to mint credentials for a different agent
// by naming it in the request body. Under the bucket-wide policy of D-030 §8
// that would mean full write access to another agent's buckets.
func TestRefreshCredentials_ForeignAgentID_ReturnsPermissionDenied(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	stsMgr := &mockSTSMgr{creds: &agentv1.CredentialsPayload{AccessKey: "AKID"}}
	srv.WithExtraDeps(nil, nil, stsMgr, &mockCredDB{})

	_, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{
		AgentId: uuid.NewString(), // some other agent
	})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Zero(t, stsMgr.calls, "no credential may be issued for a foreign agent_id")
}

// Naming another agent's rule must be refused too: fixing only agent_id would
// leave this second path open.
func TestRefreshCredentials_ForeignRuleID_ReturnsPermissionDenied(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	bucketID := uuid.New()
	stsMgr := &mockSTSMgr{creds: &agentv1.CredentialsPayload{AccessKey: "AKID"}}
	credDB := &mockCredDB{
		rule:   &db.CollectionRule{ID: uuid.New(), AgentID: uuid.New(), BucketID: bucketID},
		bucket: &db.Bucket{ID: bucketID, Name: "someone-elses-bucket"},
	}
	srv.WithExtraDeps(nil, nil, stsMgr, credDB)

	_, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{
		RuleId: uuid.NewString(),
	})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Zero(t, stsMgr.calls)
}

func TestRefreshCredentials_NoClaims_ReturnsUnauthenticated(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	srv.WithExtraDeps(nil, nil, &mockSTSMgr{}, &mockCredDB{})

	_, err := srv.RefreshCredentials(context.Background(), &agentv1.RefreshCredentialsRequest{
		AgentId: testAgentID,
	})
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

// ── RefreshCredentials: rule_id omitted (the shape the agent actually uses) ───

func TestRefreshCredentials_NoRuleID_CoversAllActiveRuleBuckets(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	bucketA, bucketB, bucketC := uuid.New(), uuid.New(), uuid.New()
	agentUUID := uuid.MustParse(testAgentID)
	credDB := &multiBucketCredDB{
		rules: []*db.CollectionRule{
			{ID: uuid.New(), AgentID: agentUUID, BucketID: bucketA, Status: db.RuleStatusActive},
			{ID: uuid.New(), AgentID: agentUUID, BucketID: bucketB, Status: db.RuleStatusActive},
			// duplicate bucket: two rules may target the same one
			{ID: uuid.New(), AgentID: agentUUID, BucketID: bucketA, Status: db.RuleStatusActive},
			// Inactive rules contribute nothing. Its bucket must be resolvable,
			// otherwise the assertion below would pass even without the status
			// filter — the row would be dropped by the bucket lookup instead.
			{ID: uuid.New(), AgentID: agentUUID, BucketID: bucketC, Status: db.RuleStatusInactive},
		},
		buckets: map[uuid.UUID]*db.Bucket{
			bucketA: {ID: bucketA, Name: "bucket-a"},
			bucketB: {ID: bucketB, Name: "bucket-b"},
			bucketC: {ID: bucketC, Name: "bucket-c"},
		},
	}
	stsMgr := &mockSTSMgr{creds: &agentv1.CredentialsPayload{AccessKey: "AKID"}}
	srv.WithExtraDeps(nil, nil, stsMgr, credDB)

	resp, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{})
	require.NoError(t, err)
	assert.Equal(t, "AKID", resp.Credentials.AccessKey)

	assert.Equal(t, testAgentID, stsMgr.gotAgentID)
	names := make([]string, 0, len(stsMgr.gotBuckets))
	for _, b := range stsMgr.gotBuckets {
		names = append(names, b.BucketName)
	}
	assert.ElementsMatch(t, []string{"bucket-a", "bucket-b"}, names,
		"duplicates collapse and inactive rules are excluded")
	assert.NotContains(t, names, "bucket-c", "an inactive rule must not widen the session")
}

func TestRefreshCredentials_NoActiveRules_ReturnsFailedPrecondition(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	credDB := &multiBucketCredDB{}
	stsMgr := &mockSTSMgr{}
	srv.WithExtraDeps(nil, nil, stsMgr, credDB)

	_, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.FailedPrecondition, status.Code(err))
	assert.Zero(t, stsMgr.calls)
}

// multiBucketCredDB resolves each rule's bucket independently, which the single
// -bucket mockCredDB cannot express.
type multiBucketCredDB struct {
	rules   []*db.CollectionRule
	buckets map[uuid.UUID]*db.Bucket
}

func (m *multiBucketCredDB) GetCollectionRuleByID(context.Context, uuid.UUID) (*db.CollectionRule, error) {
	return nil, assert.AnError
}

func (m *multiBucketCredDB) GetBucketByID(_ context.Context, id uuid.UUID) (*db.Bucket, error) {
	if b, ok := m.buckets[id]; ok {
		return b, nil
	}
	return nil, assert.AnError
}

func (m *multiBucketCredDB) ListCollectionRulesByAgent(context.Context, uuid.UUID) ([]*db.CollectionRule, error) {
	return m.rules, nil
}

// ── pushCredentials ──────────────────────────────────────────────────────────

func TestPushCredentials_DeliversOverStream(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	registry := NewAgentRegistry()
	srv.WithDeps(registry, nil, nil, nil, nil)

	bucketID := uuid.New()
	credDB := &multiBucketCredDB{
		rules: []*db.CollectionRule{
			{ID: uuid.New(), AgentID: uuid.MustParse(testAgentID), BucketID: bucketID, Status: db.RuleStatusActive},
		},
		buckets: map[uuid.UUID]*db.Bucket{bucketID: {ID: bucketID, Name: "data-sensor"}},
	}
	stsMgr := &mockSTSMgr{creds: &agentv1.CredentialsPayload{AccessKey: "AKID"}}
	srv.WithExtraDeps(nil, nil, stsMgr, credDB)

	conn := registry.Register(testAgentID, nil, func() {})
	srv.pushCredentials(context.Background(), testAgentID)

	select {
	case msg := <-conn.SendCh:
		payload, ok := msg.Payload.(*agentv1.ServerMessage_Credentials)
		require.True(t, ok, "expected a Credentials payload, got %T", msg.Payload)
		assert.Equal(t, "AKID", payload.Credentials.AccessKey)
	default:
		t.Fatal("no credentials delivered to the agent stream")
	}
}

// A freshly approved agent has no rules yet; that is normal and must not be
// treated as an error, the refresh RPC supplies credentials once rules arrive.
func TestPushCredentials_NoRules_IsQuietNoOp(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	registry := NewAgentRegistry()
	srv.WithDeps(registry, nil, nil, nil, nil)
	stsMgr := &mockSTSMgr{}
	srv.WithExtraDeps(nil, nil, stsMgr, &multiBucketCredDB{})

	conn := registry.Register(testAgentID, nil, func() {})
	assert.NotPanics(t, func() { srv.pushCredentials(context.Background(), testAgentID) })
	assert.Zero(t, stsMgr.calls)
	assert.Empty(t, conn.SendCh)
}

// ── liveness gate: a revoked agent's token must stop working ─────────────────

// Agent JWTs live for 30 days by default and RevokeAgent never invalidated
// them, so revocation only bites if the persisted status is consulted at use
// time. Without this the "management" control that D-030 §8 leans on is inert.
func TestRefreshCredentials_RevokedAgent_ReturnsPermissionDenied(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	stsMgr := &mockSTSMgr{creds: &agentv1.CredentialsPayload{AccessKey: "AKID"}}
	bucketID := uuid.New()
	credDB := &multiBucketCredDB{
		rules: []*db.CollectionRule{
			{ID: uuid.New(), AgentID: uuid.MustParse(testAgentID), BucketID: bucketID, Status: db.RuleStatusActive},
		},
		buckets: map[uuid.UUID]*db.Bucket{bucketID: {ID: bucketID, Name: "data-sensor"}},
	}
	srv.WithExtraDeps(nil, nil, stsMgr, credDB)
	srv.WithStateDB(&mockStateDB{agentStatus: db.AgentStatusRevoked})

	_, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Zero(t, stsMgr.calls, "a revoked agent must not receive credentials")
}

func TestRefreshCredentials_ApprovedAgent_PassesLivenessGate(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	bucketID := uuid.New()
	for _, st := range []db.AgentStatus{db.AgentStatusApproved, db.AgentStatusOnline, db.AgentStatusOffline} {
		stsMgr := &mockSTSMgr{creds: &agentv1.CredentialsPayload{AccessKey: "AKID"}}
		credDB := &multiBucketCredDB{
			rules: []*db.CollectionRule{
				{ID: uuid.New(), AgentID: uuid.MustParse(testAgentID), BucketID: bucketID, Status: db.RuleStatusActive},
			},
			buckets: map[uuid.UUID]*db.Bucket{bucketID: {ID: bucketID, Name: "data-sensor"}},
		}
		srv.WithExtraDeps(nil, nil, stsMgr, credDB)
		srv.WithStateDB(&mockStateDB{agentStatus: st})
		_, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{})
		require.NoError(t, err, "status %s must be allowed", st)
	}
}

// An agent row that cannot be read must not be trusted.
func TestRefreshCredentials_AgentLookupFails_ReturnsPermissionDenied(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	stsMgr := &mockSTSMgr{}
	srv.WithExtraDeps(nil, nil, stsMgr, &multiBucketCredDB{})
	srv.WithStateDB(&mockStateDB{agentErr: assert.AnError})

	_, err := srv.RefreshCredentials(agentCtx(testAgentID), &agentv1.RefreshCredentialsRequest{})
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err))
	assert.Zero(t, stsMgr.calls)
}

// ── dry-run results must belong to the reporting agent ───────────────────────

// The rule id travels in the message body, so without an ownership check any
// connected agent could deliver a fabricated preview against someone else's
// rule and the operator would see it as genuine (IC-BUG-24).
func TestHandleDryRunResult_ForeignRule_IsDiscarded(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	store := &mockDryRunStore{}
	credDB := &mockCredDB{
		rule: &db.CollectionRule{ID: uuid.New(), AgentID: uuid.New()}, // owned by someone else
	}
	srv.WithExtraDeps(nil, nil, nil, credDB)
	srv.WithDryRunStore(store)

	srv.handleDryRunResult(context.Background(), testAgentID,
		&agentv1.DryRunResult{RuleId: uuid.NewString()})

	assert.Zero(t, store.delivered, "a foreign rule's result must not reach the store")
}

func TestHandleDryRunResult_OwnRule_IsDelivered(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	store := &mockDryRunStore{}
	credDB := &mockCredDB{
		rule: &db.CollectionRule{ID: uuid.New(), AgentID: uuid.MustParse(testAgentID)},
	}
	srv.WithExtraDeps(nil, nil, nil, credDB)
	srv.WithDryRunStore(store)

	srv.handleDryRunResult(context.Background(), testAgentID,
		&agentv1.DryRunResult{RuleId: uuid.NewString()})

	assert.Equal(t, 1, store.delivered)
}

// A lookup failure must not be treated as ownership.
func TestHandleDryRunResult_LookupFails_IsDiscarded(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	store := &mockDryRunStore{}
	srv.WithExtraDeps(nil, nil, nil, &mockCredDB{ruleErr: assert.AnError})
	srv.WithDryRunStore(store)

	srv.handleDryRunResult(context.Background(), testAgentID,
		&agentv1.DryRunResult{RuleId: uuid.NewString()})

	assert.Zero(t, store.delivered)
}

type mockDryRunStore struct{ delivered int }

func (m *mockDryRunStore) Deliver(_ string, _ *agentv1.DryRunResult) { m.delivered++ }
