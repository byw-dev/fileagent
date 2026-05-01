package grpcserver

import (
	"context"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Mock AgentManager ─────────────────────────────────────────────────────────

type mockAgentMgr struct {
	registerResp   *agentv1.RegisterResponse
	registerErr    error
	pollResp       *agentv1.PollApprovalResponse
	pollErr        error
}

func (m *mockAgentMgr) Register(ctx context.Context, req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	return m.registerResp, m.registerErr
}

func (m *mockAgentMgr) PollApproval(ctx context.Context, req *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
	return m.pollResp, m.pollErr
}

// ── Mock CacheClient ──────────────────────────────────────────────────────────

type mockCache struct {
	sets map[string]string
	dels []string
}

func newMockCache() *mockCache {
	return &mockCache{sets: make(map[string]string)}
}

func (m *mockCache) Set(_ context.Context, key string, value interface{}, _ time.Duration) error {
	m.sets[key] = ""
	return nil
}

func (m *mockCache) Del(_ context.Context, keys ...string) error {
	m.dels = append(m.dels, keys...)
	return nil
}

// ── Mock NATSPublisher ────────────────────────────────────────────────────────

type mockNATS struct {
	published []string
}

func (m *mockNATS) Publish(subject string, _ []byte) error {
	m.published = append(m.published, subject)
	return nil
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestRegister_WithAgentMgr_Delegates(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	mgr := &mockAgentMgr{
		registerResp: &agentv1.RegisterResponse{AgentId: "agent-1", Status: "pending"},
	}
	srv.WithDeps(nil, nil, nil, nil, mgr)

	resp, err := srv.Register(context.Background(), &agentv1.RegisterRequest{
		Fingerprint: "fp-1",
		Hostname:    "host-1",
	})
	require.NoError(t, err)
	assert.Equal(t, "agent-1", resp.GetAgentId())
}

func TestRegister_AgentMgr_Error(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	mgr := &mockAgentMgr{registerErr: assert.AnError}
	srv.WithDeps(nil, nil, nil, nil, mgr)

	_, err := srv.Register(context.Background(), &agentv1.RegisterRequest{Fingerprint: "fp-2"})
	require.Error(t, err)
}

func TestPollApproval_WithAgentMgr_Delegates(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	mgr := &mockAgentMgr{
		pollResp: &agentv1.PollApprovalResponse{Status: "approved", AuthToken: "tok123"},
	}
	srv.WithDeps(nil, nil, nil, nil, mgr)

	resp, err := srv.PollApproval(context.Background(), &agentv1.PollApprovalRequest{AgentId: "agent-1"})
	require.NoError(t, err)
	assert.Equal(t, "approved", resp.GetStatus())
}

func TestPollApproval_AgentMgr_Error(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	mgr := &mockAgentMgr{pollErr: assert.AnError}
	srv.WithDeps(nil, nil, nil, nil, mgr)

	_, err := srv.PollApproval(context.Background(), &agentv1.PollApprovalRequest{AgentId: "a1"})
	require.Error(t, err)
}

func TestHandleHeartbeat_UpdatesCache(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	c := newMockCache()
	srv.WithDeps(nil, c, nil, nil, nil)

	srv.handleHeartbeat(context.Background(), "agent-abc",
		&agentv1.Heartbeat{UptimeSeconds: 120})

	onlineKey := cache.AgentOnlineKey("agent-abc")
	_, ok := c.sets[onlineKey]
	assert.True(t, ok)
}

func TestHandleHeartbeat_NilCache(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	srv.WithDeps(nil, nil, nil, nil, nil)

	// Should not panic when cache is nil
	assert.NotPanics(t, func() {
		srv.handleHeartbeat(context.Background(), "agent-xyz",
			&agentv1.Heartbeat{UptimeSeconds: 10})
	})
}

func TestHandleUploadResult_Logs(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)

	assert.NotPanics(t, func() {
		srv.handleUploadResult(context.Background(), "agent-1", &agentv1.UploadResult{
			StoragePath: "uploads/test.log",
			Success:     true,
		})
	})
}

func TestHandleAgentMessage_Heartbeat(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	c := newMockCache()
	srv.WithDeps(nil, c, nil, nil, nil)

	msg := &agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_Heartbeat{
			Heartbeat: &agentv1.Heartbeat{UptimeSeconds: 30},
		},
	}
	srv.handleAgentMessage(context.Background(), "agent-1", msg)

	_, ok := c.sets[cache.AgentOnlineKey("agent-1")]
	assert.True(t, ok)
}

func TestHandleAgentMessage_UploadResult(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	srv.WithDeps(nil, nil, nil, nil, nil)

	msg := &agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_UploadResult{
			UploadResult: &agentv1.UploadResult{StoragePath: "path/file.txt", Success: true},
		},
	}
	assert.NotPanics(t, func() {
		srv.handleAgentMessage(context.Background(), "agent-2", msg)
	})
}

func TestHandleAgentMessage_UnknownMessage(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)

	msg := &agentv1.AgentMessage{
		MessageId: "msg-1",
		Payload:   nil,
	}
	assert.NotPanics(t, func() {
		srv.handleAgentMessage(context.Background(), "agent-3", msg)
	})
}

func TestPublishEvent_WithNATS(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	nats := &mockNATS{}
	srv.WithDeps(nil, nil, nil, nats, nil)

	srv.publishEvent("events.agent.online", "agent-1")
	assert.Contains(t, nats.published, "events.agent.online")
}

func TestPublishEvent_NilNATS(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)

	assert.NotPanics(t, func() {
		srv.publishEvent("events.agent.online", "agent-1")
	})
}

func TestExtractAgentID_FromContext(t *testing.T) {
	// inject claims into context using the claimsContextKey
	claims := &auth.Claims{}
	claims.Subject = "agent-uuid-123"
	ctx := context.WithValue(context.Background(), claimsContextKey, claims)

	agentID := extractAgentID(ctx)
	assert.Equal(t, "agent-uuid-123", agentID)
}

func TestExtractAgentID_EmptyContext(t *testing.T) {
	agentID := extractAgentID(context.Background())
	assert.Empty(t, agentID)
}

func TestExtractAgentID_WrongType(t *testing.T) {
	ctx := context.WithValue(context.Background(), claimsContextKey, "not-claims")
	agentID := extractAgentID(ctx)
	assert.Empty(t, agentID)
}

func TestWithDeps_SetsFields(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	srv := New(logger)
	registry := NewAgentRegistry()
	c := newMockCache()
	nats := &mockNATS{}
	mgr := &mockAgentMgr{}

	srv.WithDeps(registry, c, nil, nats, mgr)

	assert.NotNil(t, srv.registry)
	assert.NotNil(t, srv.cache)
	assert.NotNil(t, srv.nats)
	assert.NotNil(t, srv.agentMgr)
}
