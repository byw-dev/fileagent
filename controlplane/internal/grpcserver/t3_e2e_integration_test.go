//go:build integration

package grpcserver

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/test/bufconn"
)

type integrationTestMockAgentManager struct {
	mu     sync.Mutex
	status map[string]string
	token  map[string]string
	jwtSvc auth.Service
}

func newIntegrationTestMockAgentManager(jwtSvc auth.Service) *integrationTestMockAgentManager {
	return &integrationTestMockAgentManager{
		status: map[string]string{},
		token:  map[string]string{},
		jwtSvc: jwtSvc,
	}
}

func (m *integrationTestMockAgentManager) Register(_ context.Context, _ *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id := uuid.New().String()
	m.status[id] = "pending"
	return &agentv1.RegisterResponse{
		AgentId: id,
		Status:  "pending",
		Message: "awaiting approval",
	}, nil
}

func (m *integrationTestMockAgentManager) PollApproval(_ context.Context, req *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := m.status[req.GetAgentId()]
	if state == "" {
		state = "pending"
	}
	resp := &agentv1.PollApprovalResponse{Status: state}
	if state == "approved" {
		resp.AuthToken = m.token[req.GetAgentId()]
	}
	return resp, nil
}

func (m *integrationTestMockAgentManager) approve(t *testing.T, agentID string) string {
	t.Helper()
	token, err := m.jwtSvc.GenerateAccessToken(
		agentID,
		"00000000-0000-0000-0000-000000000001",
		"agent",
		"e2e-agent",
		time.Hour,
	)
	require.NoError(t, err)
	m.mu.Lock()
	m.status[agentID] = "approved"
	m.token[agentID] = token
	m.mu.Unlock()
	return token
}

type integrationTestMockDispatcher struct {
	registry *AgentRegistry
}

func (d *integrationTestMockDispatcher) SyncRulesOnConnect(_ context.Context, agentID string) error {
	d.registry.Send(agentID, &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_PushRule{
			PushRule: &agentv1.PushRuleCommand{
				Rule: &agentv1.CollectionRule{
					RuleId:           "rule-e2e-1",
					Name:             "watch-log",
					Mode:             "watch",
					BasePath:         "/var/log",
					PathPattern:      "*.log",
					DestPathTemplate: "agents/logs",
					Enabled:          true,
				},
			},
		},
	})
	return nil
}

type integrationTestMockIndexer struct {
	mu      sync.Mutex
	uploads []*agentv1.UploadResult
}

func (i *integrationTestMockIndexer) HandleUploadResult(_ context.Context, _ uuid.UUID, _ uuid.UUID, result *agentv1.UploadResult) error {
	i.mu.Lock()
	defer i.mu.Unlock()
	i.uploads = append(i.uploads, result)
	return nil
}

func (i *integrationTestMockIndexer) count() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return len(i.uploads)
}

func TestControlPlaneAgent_EndToEnd_Integration(t *testing.T) {
	miniRedis, err := miniredis.Run()
	require.NoError(t, err)
	defer miniRedis.Close()

	redisClient, err := cache.New("redis://"+miniRedis.Addr(), zap.NewNop())
	require.NoError(t, err)
	defer func() { _ = redisClient.Close() }()

	jwtSvc := auth.New("test-secret-32-bytes-padded!!!!", redisClient)
	manager := newIntegrationTestMockAgentManager(jwtSvc)
	registry := NewAgentRegistry()
	indexer := &integrationTestMockIndexer{}
	dispatcher := &integrationTestMockDispatcher{registry: registry}

	srv := New(zap.NewNop())
	srv.WithDeps(registry, redisClient, jwtSvc, &mockNATS{}, manager)
	srv.WithExtraDeps(dispatcher, indexer, nil, nil)

	listener := bufconn.Listen(1024 * 1024)
	grpcSrv := srv.GRPCServer()
	go func() { _ = grpcSrv.Serve(listener) }()
	defer grpcSrv.GracefulStop()

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return listener.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	defer func() { _ = conn.Close() }()
	client := agentv1.NewAgentServiceClient(conn)

	// 1) Register -> PollApproval(pending) -> approve -> PollApproval(approved+token).
	registerResp, err := client.Register(context.Background(), &agentv1.RegisterRequest{
		Fingerprint: "fp-t3-e2e",
		Hostname:    "host-t3",
	})
	require.NoError(t, err)
	require.NotEmpty(t, registerResp.GetAgentId())
	assert.Equal(t, "pending", registerResp.GetStatus())

	pendingResp, err := client.PollApproval(context.Background(), &agentv1.PollApprovalRequest{
		AgentId: registerResp.GetAgentId(),
	})
	require.NoError(t, err)
	assert.Equal(t, "pending", pendingResp.GetStatus())

	token := manager.approve(t, registerResp.GetAgentId())
	approvedResp, err := client.PollApproval(context.Background(), &agentv1.PollApprovalRequest{
		AgentId: registerResp.GetAgentId(),
	})
	require.NoError(t, err)
	assert.Equal(t, "approved", approvedResp.GetStatus())
	assert.Equal(t, token, approvedResp.GetAuthToken())

	// 2) Connect and verify push_rule on connect.
	authCtx1, cancel1 := context.WithTimeout(
		metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token)),
		5*time.Second,
	)
	defer cancel1()
	stream1, err := client.Connect(authCtx1)
	require.NoError(t, err)

	msg1, err := stream1.Recv()
	require.NoError(t, err)
	require.NotNil(t, msg1.GetPushRule())
	assert.Equal(t, "rule-e2e-1", msg1.GetPushRule().GetRule().GetRuleId())

	// 3) Simulate network interruption and reconnect, then "replay" queued result.
	require.NoError(t, stream1.CloseSend())
	authCtx2, cancel2 := context.WithTimeout(
		metadata.NewOutgoingContext(context.Background(), metadata.Pairs("authorization", "Bearer "+token)),
		5*time.Second,
	)
	defer cancel2()
	stream2, err := client.Connect(authCtx2)
	require.NoError(t, err)

	msg2, err := stream2.Recv()
	require.NoError(t, err)
	require.NotNil(t, msg2.GetPushRule())
	assert.Equal(t, "rule-e2e-1", msg2.GetPushRule().GetRule().GetRuleId())

	require.NoError(t, stream2.Send(&agentv1.AgentMessage{
		MessageId: "upload-replay-1",
		Payload: &agentv1.AgentMessage_UploadResult{
			UploadResult: &agentv1.UploadResult{
				RuleId:      "rule-e2e-1",
				Bucket:      "data-sensor",
				StoragePath: "agents/replay.log",
				SizeBytes:   123,
				Sha256:      "abc123",
				Success:     true,
			},
		},
	}))
	require.Eventually(t, func() bool { return indexer.count() == 1 }, 2*time.Second, 20*time.Millisecond)

	// 4) Deliver revoke command and ensure connected agent receives it.
	ok := registry.Send(registerResp.GetAgentId(), &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_Revoke{
			Revoke: &agentv1.RevokeCommand{Reason: "revoked_by_admin"},
		},
	})
	require.True(t, ok)

	revokeMsg, err := stream2.Recv()
	require.NoError(t, err)
	require.NotNil(t, revokeMsg.GetRevoke())
	assert.Equal(t, "revoked_by_admin", revokeMsg.GetRevoke().GetReason())
}
