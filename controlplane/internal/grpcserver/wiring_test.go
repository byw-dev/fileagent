package grpcserver

import (
	"context"
	"testing"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/storage"
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
}

func (m *mockSTSMgr) IssueCredentials(_ context.Context, _ string, _ []storage.BucketAccess) (*agentv1.CredentialsPayload, error) {
	return m.creds, m.err
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

	_, err := srv.RefreshCredentials(context.Background(), &agentv1.RefreshCredentialsRequest{
		AgentId: "agent-1",
		RuleId:  uuid.New().String(),
	})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestRefreshCredentials_InvalidRuleID_ReturnsInvalidArgument(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	srv.WithExtraDeps(nil, nil, &mockSTSMgr{}, &mockCredDB{})

	_, err := srv.RefreshCredentials(context.Background(), &agentv1.RefreshCredentialsRequest{
		AgentId: "agent-1",
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

	_, err := srv.RefreshCredentials(context.Background(), &agentv1.RefreshCredentialsRequest{
		AgentId: "agent-1",
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
		rule:    &db.CollectionRule{ID: uuid.New(), BucketID: bucketID},
		buckErr: assert.AnError,
	}
	srv.WithExtraDeps(nil, nil, &mockSTSMgr{}, credDB)

	_, err := srv.RefreshCredentials(context.Background(), &agentv1.RefreshCredentialsRequest{
		AgentId: "agent-1",
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
		rule:   &db.CollectionRule{ID: uuid.New(), BucketID: bucketID},
		bucket: &db.Bucket{ID: bucketID, Name: "data-sensor"},
	}
	stsMgr := &mockSTSMgr{err: assert.AnError}
	srv.WithExtraDeps(nil, nil, stsMgr, credDB)

	_, err := srv.RefreshCredentials(context.Background(), &agentv1.RefreshCredentialsRequest{
		AgentId: "agent-1",
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
		rule:   &db.CollectionRule{ID: uuid.New(), BucketID: bucketID},
		bucket: &db.Bucket{ID: bucketID, Name: "data-sensor"},
	}
	stsMgr := &mockSTSMgr{creds: creds}
	srv.WithExtraDeps(nil, nil, stsMgr, credDB)

	resp, err := srv.RefreshCredentials(context.Background(), &agentv1.RefreshCredentialsRequest{
		AgentId: "agent-1",
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
