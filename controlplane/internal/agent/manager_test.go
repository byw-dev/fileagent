package agent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Mock implementations ────────────────────────────────────────────────────

type mockAgentDB struct {
	agents map[string]*db.Agent
}

func newMockAgentDB() *mockAgentDB {
	return &mockAgentDB{agents: make(map[string]*db.Agent)}
}

func (m *mockAgentDB) CreateAgent(_ context.Context, arg db.CreateAgentParams) (*db.Agent, error) {
	a := &db.Agent{
		ID:          uuid.New(),
		OrgID:       arg.OrgID,
		Name:        arg.Name,
		Fingerprint: arg.Fingerprint,
		Status:      db.AgentStatusPending,
		OsInfo:      arg.OsInfo,
		IpAddress:   arg.IpAddress,
	}
	m.agents[a.ID.String()] = a
	m.agents["fp:"+arg.Fingerprint] = a
	return a, nil
}

func (m *mockAgentDB) GetAgentByFingerprint(_ context.Context, fingerprint string) (*db.Agent, error) {
	if a, ok := m.agents["fp:"+fingerprint]; ok {
		return a, nil
	}
	return &db.Agent{}, sql.ErrNoRows
}

func (m *mockAgentDB) GetAgentByID(_ context.Context, id uuid.UUID) (*db.Agent, error) {
	if a, ok := m.agents[id.String()]; ok {
		return a, nil
	}
	return nil, sql.ErrNoRows
}

func (m *mockAgentDB) UpdateAgentStatus(_ context.Context, id uuid.UUID, status db.AgentStatus) (*db.Agent, error) {
	if a, ok := m.agents[id.String()]; ok {
		a.Status = status
		return a, nil
	}
	return nil, sql.ErrNoRows
}

func (m *mockAgentDB) UpdateAgentAuthToken(_ context.Context, id uuid.UUID, hash sql.NullString, exp sql.NullTime) (*db.Agent, error) {
	if a, ok := m.agents[id.String()]; ok {
		a.AuthTokenHash = hash
		a.TokenExpiresAt = exp
		return a, nil
	}
	return nil, sql.ErrNoRows
}

type mockCache struct{}

func (m *mockCache) Set(_ context.Context, _ string, _ interface{}, _ time.Duration) error {
	return nil
}
func (m *mockCache) Del(_ context.Context, _ ...string) error { return nil }

type mockNATS struct {
	published []string
}

func (m *mockNATS) Publish(subject string, _ []byte) error {
	m.published = append(m.published, subject)
	return nil
}

// ── Tests ────────────────────────────────────────────────────────────────────

func newTestManager(t *testing.T) (*Manager, *mockAgentDB, *mockNATS) {
	t.Helper()
	agentDB := newMockAgentDB()
	nats := &mockNATS{}
	logger, _ := zap.NewDevelopment()
	jwtSvc := auth.New("test-secret", nil)
	m := NewManager(agentDB, &mockCache{}, jwtSvc, nats, logger, 24*time.Hour)
	return m, agentDB, nats
}

func TestRegister_FirstTime_ReturnsRealUUID(t *testing.T) {
	agentDB := newMockAgentDB()
	nats := &mockNATS{}
	logger, _ := zap.NewDevelopment()

	m := NewManager(agentDB, &mockCache{}, nil, nats, logger, 24*time.Hour)

	resp, err := m.Register(context.Background(), &agentv1.RegisterRequest{
		Fingerprint:  "fp-abc123",
		Hostname:     "edge-host-01",
		OsType:       "linux",
		OsVersion:    "ubuntu-22.04",
		Arch:         "amd64",
		AgentVersion: "1.0.0",
		IpAddress:    "192.168.1.100",
	})
	require.NoError(t, err)
	assert.NotEmpty(t, resp.AgentId)
	assert.NotEqual(t, uuid.Nil.String(), resp.AgentId)
	assert.Equal(t, "pending", resp.Status)
	assert.Contains(t, resp.Message, "Awaiting approval")
}

func TestRegister_ExistingAgent_ReturnsSameUUID(t *testing.T) {
	agentDB := newMockAgentDB()
	logger, _ := zap.NewDevelopment()

	// Pre-seed agent.
	existing := &db.Agent{
		ID:          uuid.New(),
		OrgID:       defaultOrgID,
		Name:        "edge-host-01",
		Fingerprint: "fp-abc123",
		Status:      db.AgentStatusPending,
		IpAddress:   pqtype.Inet{},
	}
	agentDB.agents[existing.ID.String()] = existing
	agentDB.agents["fp:fp-abc123"] = existing

	m := NewManager(agentDB, &mockCache{}, nil, nil, logger, 24*time.Hour)

	resp, err := m.Register(context.Background(), &agentv1.RegisterRequest{
		Fingerprint: "fp-abc123",
		Hostname:    "edge-host-01",
	})
	require.NoError(t, err)
	assert.Equal(t, existing.ID.String(), resp.AgentId)
}

func TestPollApproval_PendingStatus(t *testing.T) {
	agentDB := newMockAgentDB()
	logger, _ := zap.NewDevelopment()

	agentID := uuid.New()
	agentDB.agents[agentID.String()] = &db.Agent{
		ID:     agentID,
		Status: db.AgentStatusPending,
	}

	m := NewManager(agentDB, &mockCache{}, nil, nil, logger, 24*time.Hour)

	resp, err := m.PollApproval(context.Background(), &agentv1.PollApprovalRequest{
		AgentId: agentID.String(),
	})
	require.NoError(t, err)
	assert.Equal(t, "pending", resp.Status)
	assert.Empty(t, resp.AuthToken)
}

func TestPollApproval_ApprovedReturnsToken(t *testing.T) {
	m, agentDB, _ := newTestManager(t)

	agentID := uuid.New()
	agentDB.agents[agentID.String()] = &db.Agent{
		ID:     agentID,
		OrgID:  defaultOrgID,
		Name:   "approved-agent",
		Status: db.AgentStatusApproved,
	}

	resp, err := m.PollApproval(context.Background(), &agentv1.PollApprovalRequest{
		AgentId: agentID.String(),
	})
	require.NoError(t, err)
	assert.Equal(t, "approved", resp.Status)
	assert.NotEmpty(t, resp.AuthToken)

	claims, err := m.jwtSvc.ValidateToken(resp.AuthToken)
	require.NoError(t, err)
	assert.Equal(t, "agent", claims.Role)

	sum := sha256.Sum256([]byte(resp.AuthToken))
	expectedHash := hex.EncodeToString(sum[:])
	assert.Equal(t, expectedHash, agentDB.agents[agentID.String()].AuthTokenHash.String)
}

// TestPollApproval_TokenUsesAgentTokenTTL guards against the regression where
// agents were issued a short (2h access) token: because agents reuse the token
// across gRPC reconnects, a short TTL permanently locks them out after expiry.
// The issued token — both its stored expiry and its JWT exp claim — must reflect
// the long agentTokenTTL configured on the Manager.
func TestPollApproval_TokenUsesAgentTokenTTL(t *testing.T) {
	agentDB := newMockAgentDB()
	logger, _ := zap.NewDevelopment()
	jwtSvc := auth.New("test-secret", nil)
	const ttl = 720 * time.Hour
	m := NewManager(agentDB, &mockCache{}, jwtSvc, &mockNATS{}, logger, ttl)

	agentID := uuid.New()
	agentDB.agents[agentID.String()] = &db.Agent{
		ID:     agentID,
		OrgID:  defaultOrgID,
		Name:   "approved-agent",
		Status: db.AgentStatusApproved,
	}

	before := time.Now()
	resp, err := m.PollApproval(context.Background(), &agentv1.PollApprovalRequest{
		AgentId: agentID.String(),
	})
	require.NoError(t, err)
	require.NotEmpty(t, resp.AuthToken)

	// Stored expiry reflects the long TTL (allow a generous skew for slow CI).
	storedExp := agentDB.agents[agentID.String()].TokenExpiresAt
	require.True(t, storedExp.Valid)
	assert.WithinDuration(t, before.Add(ttl), storedExp.Time, time.Minute)

	// The JWT itself carries the long expiry, not the 2h access TTL.
	claims, err := jwtSvc.ValidateToken(resp.AuthToken)
	require.NoError(t, err)
	require.NotNil(t, claims.ExpiresAt)
	assert.WithinDuration(t, before.Add(ttl), claims.ExpiresAt.Time, time.Minute)
}

func TestPollApproval_InvalidID(t *testing.T) {
	agentDB := newMockAgentDB()
	logger, _ := zap.NewDevelopment()
	m := NewManager(agentDB, &mockCache{}, nil, nil, logger, 24*time.Hour)

	_, err := m.PollApproval(context.Background(), &agentv1.PollApprovalRequest{
		AgentId: "not-a-uuid",
	})
	require.Error(t, err)
}

func TestRevokeAgent_UpdatesStatus(t *testing.T) {
	agentDB := newMockAgentDB()
	nats := &mockNATS{}
	logger, _ := zap.NewDevelopment()

	agentID := uuid.New()
	agentDB.agents[agentID.String()] = &db.Agent{
		ID:     agentID,
		Name:   "test-agent",
		Status: db.AgentStatusApproved,
	}

	m := NewManager(agentDB, &mockCache{}, nil, nats, logger, 24*time.Hour)

	err := m.RevokeAgent(context.Background(), agentID, uuid.New())
	require.NoError(t, err)
	assert.Equal(t, db.AgentStatusRevoked, agentDB.agents[agentID.String()].Status)
	assert.Contains(t, nats.published, "events.agent.revoked")
}

func TestApproveAgent_GeneratesToken(t *testing.T) {
	m, agentDB, nats := newTestManager(t)

	agentID := uuid.New()
	agentDB.agents[agentID.String()] = &db.Agent{
		ID:     agentID,
		Name:   "test-agent",
		OrgID:  defaultOrgID,
		Status: db.AgentStatusPending,
	}

	rawToken, err := m.ApproveAgent(context.Background(), agentID, uuid.New())
	require.NoError(t, err)
	assert.NotEmpty(t, rawToken)

	agent := agentDB.agents[agentID.String()]
	assert.Equal(t, db.AgentStatusApproved, agent.Status)
	assert.True(t, agent.AuthTokenHash.Valid)
	assert.Contains(t, nats.published, "events.agent.approved")
}

func TestStatusMessage_AllStatuses(t *testing.T) {
	cases := []struct {
		status db.AgentStatus
		want   string
	}{
		{db.AgentStatusPending, "Awaiting approval"},
		{db.AgentStatusApproved, "Approved"},
		{db.AgentStatusOnline, "Online"},
		{db.AgentStatusOffline, "Offline"},
		{db.AgentStatusRevoked, "Revoked"},
		{db.AgentStatus("unknown_status"), "unknown_status"},
	}
	for _, tc := range cases {
		t.Run(string(tc.status), func(t *testing.T) {
			assert.Equal(t, tc.want, statusMessage(tc.status))
		})
	}
}

type errNATS struct{ err error }

func (e *errNATS) Publish(_ string, _ []byte) error { return e.err }

func TestPublishEvent_NATSError(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	agentDB := newMockAgentDB()
	jwtSvc := auth.New("test-secret", nil)
	m := NewManager(agentDB, &mockCache{}, jwtSvc, &errNATS{err: assert.AnError}, logger, 24*time.Hour)

	// Should not panic even when NATS returns an error
	assert.NotPanics(t, func() {
		m.publishEvent("events.agent.test", map[string]string{"key": "value"})
	})
}

func TestPublishEvent_NilNATS(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	agentDB := newMockAgentDB()
	jwtSvc := auth.New("test-secret", nil)
	m := NewManager(agentDB, &mockCache{}, jwtSvc, nil, logger, 24*time.Hour)

	assert.NotPanics(t, func() {
		m.publishEvent("events.agent.test", map[string]string{"key": "value"})
	})
}
