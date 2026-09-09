package grpcserver

import (
	"context"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

// newFullServer creates a test gRPC server with all deps wired (registry,
// mock cache, mock NATS, real JWT service backed by miniredis).
// Returns the client and the bearer token to use.
func newFullServer(t *testing.T) (agentv1.AgentServiceClient, string) {
	t.Helper()

	// Start miniredis for JWT revocation checks.
	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	redisClient, err := cache.New("redis://"+mr.Addr(), zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisClient.Close() })

	// Create the JWT service and mint a token for "agent-test".
	jwtSvc := auth.New("test-secret-32-bytes-padded!!!!", redisClient)
	token, err := jwtSvc.GenerateAccessToken(
		"agent-test", "org-1", "agent", "agent-test", time.Hour)
	require.NoError(t, err)

	logger, _ := zap.NewDevelopment()
	registry := NewAgentRegistry()
	c := newMockCache()

	srv := New(logger)
	srv.WithDeps(registry, c, jwtSvc, &mockNATS{}, nil)

	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := srv.GRPCServer()

	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.GracefulStop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return agentv1.NewAgentServiceClient(conn), "Bearer " + token
}

// A revoked agent must not be able to open a stream. Connect is the entry point
// that pushes credentials automatically, so leaving it ungated while
// RefreshCredentials is gated would keep the whole attack chain intact: connect
// once, receive credentials, done.
//
// The extra assertions are deliberate. A status-code-only test would still pass
// if the gate were moved further down Connect, where the agent would already
// have been registered, marked online and handed credentials before being
// refused. IsOnline alone is not enough either — the deferred Unregister resets
// it — so the online write is what actually pins the gate's position.
func TestServer_Connect_RevokedAgent_IsRejectedBeforeRegistration(t *testing.T) {
	agentID := "22222222-2222-2222-2222-222222222222"
	stateDB := &mockStateDB{agentStatus: db.AgentStatusRevoked}
	client, bearer, registry := newFullServerForAgent(t, agentID, stateDB)

	// Bounded: if the gate is ever removed the server accepts the stream and
	// Recv would block forever, turning a regression into a hung suite instead
	// of a failing test.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err, "the stream opens; the rejection arrives on first Recv")

	_, err = stream.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.PermissionDenied, status.Code(err),
		"a stream that merely times out means the gate is gone")
	assert.False(t, registry.IsOnline(agentID),
		"a rejected agent must never enter the registry")
	assert.Zero(t, stateDB.markOnlineUsableCalls,
		"the gate must run before the online transition, not after it")
}

func TestServer_Connect_ApprovedAgent_IsAccepted(t *testing.T) {
	agentID := "33333333-3333-3333-3333-333333333333"
	client, bearer, registry := newFullServerForAgent(t, agentID,
		&mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.NoError(t, stream.Send(&agentv1.AgentMessage{MessageId: "m1"}))

	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		2*time.Second, 20*time.Millisecond, "an approved agent must be registered")
}

// newFullServerForAgent is newFullServer with a caller-chosen agent id and state
// DB, so the liveness gate can be exercised.
func newFullServerForAgent(t *testing.T, agentID string, stateDB AgentStateDB) (agentv1.AgentServiceClient, string, *AgentRegistry) {
	t.Helper()

	mr, err := miniredis.Run()
	require.NoError(t, err)
	t.Cleanup(mr.Close)

	redisClient, err := cache.New("redis://"+mr.Addr(), zap.NewNop())
	require.NoError(t, err)
	t.Cleanup(func() { _ = redisClient.Close() })

	jwtSvc := auth.New("test-secret-32-bytes-padded!!!!", redisClient)
	token, err := jwtSvc.GenerateAccessToken(agentID, "org-1", "agent", agentID, time.Hour)
	require.NoError(t, err)

	logger, _ := zap.NewDevelopment()
	registry := NewAgentRegistry()
	srv := New(logger)
	srv.WithDeps(registry, newMockCache(), jwtSvc, &mockNATS{}, nil)
	srv.WithStateDB(stateDB)

	lis := bufconn.Listen(1024 * 1024)
	grpcSrv := srv.GRPCServer()
	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(grpcSrv.GracefulStop)

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return agentv1.NewAgentServiceClient(conn), "Bearer " + token, registry
}

func TestServer_Connect_WithRegistry_ReceivesAndDisconnects(t *testing.T) {
	client, bearerToken := newFullServer(t)

	md := metadata.Pairs("authorization", bearerToken)
	ctx, cancel := context.WithTimeout(
		metadata.NewOutgoingContext(context.Background(), md),
		3*time.Second,
	)
	defer cancel()

	stream, err := client.Connect(ctx)
	require.NoError(t, err)

	// Send a heartbeat to the server.
	err = stream.Send(&agentv1.AgentMessage{
		MessageId: "hb-1",
		Payload: &agentv1.AgentMessage_Heartbeat{
			Heartbeat: &agentv1.Heartbeat{UptimeSeconds: 10},
		},
	})
	require.NoError(t, err)

	// Close the send side so the server exits the receive loop.
	require.NoError(t, stream.CloseSend())

	// After client closes, the server Recv will return EOF which causes Connect
	// to return. The client will get EOF or a status error.
	_, recvErr := stream.Recv()
	// We expect EOF or a stream-closed error (not an application-level failure).
	if recvErr != nil {
		code := status.Code(recvErr)
		assert.NotEqual(t, codes.Unauthenticated, code)
		assert.NotEqual(t, codes.Internal, code)
	}
}

func TestServer_Connect_WithRegistry_Unauthenticated_NoToken(t *testing.T) {
	client, _ := newFullServer(t)

	// No authorization header — JWT interceptor should reject.
	stream, err := client.Connect(context.Background())
	require.NoError(t, err)

	_, err = stream.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

// Revocation has to actually end the RPC, not merely cancel a context nobody
// observes. The first attempt at IC-BUG-25 cancelled a context derived from
// stream.Context(), which the blocking Recv never looks at: the handler kept
// running, its registry entry stayed, and the agent went on sending.
//
// The assertion is therefore about the stream ending and the registry emptying,
// not about Disconnect returning true.
func TestServer_Connect_Disconnect_EndsStreamAndUnregisters(t *testing.T) {
	agentID := "44444444-4444-4444-4444-444444444444"
	client, bearer, registry := newFullServerForAgent(t, agentID,
		&mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.NoError(t, stream.Send(&agentv1.AgentMessage{MessageId: "hello"}))
	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		2*time.Second, 20*time.Millisecond)

	require.True(t, registry.Disconnect(agentID))

	// The client's Recv must return rather than block forever.
	recvErr := make(chan error, 1)
	go func() { _, e := stream.Recv(); recvErr <- e }()
	select {
	case e := <-recvErr:
		require.Error(t, e)
		assert.Equal(t, codes.PermissionDenied, status.Code(e))
	case <-time.After(5 * time.Second):
		t.Fatal("stream still open 5s after Disconnect — the handler never returned")
	}

	// The handler's deferred Unregister only runs once it returns.
	assert.Eventually(t, func() bool { return !registry.IsOnline(agentID) },
		3*time.Second, 20*time.Millisecond,
		"registry entry leaked: the handler goroutine is still alive")
}

// A revoked agent has no reason to keep reading the stream, and once it stops
// the send goroutine parks inside stream.Send — where cancelling the context
// cannot reach it. An earlier version waited for that goroutine before
// returning, which reproduced the very leak IC-BUG-25 is about: the handler
// never returned, the registry entry stayed, IsOnline stayed true.
//
// The agent here deliberately never calls Recv.
func TestServer_Connect_Disconnect_WhileSendBlocked(t *testing.T) {
	agentID := "55555555-5555-5555-5555-555555555555"
	client, bearer, registry := newFullServerForAgent(t, agentID,
		&mockStateDB{agentStatus: db.AgentStatusApproved})

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	stream, err := client.Connect(metadata.AppendToOutgoingContext(ctx, "authorization", bearer))
	require.NoError(t, err)
	require.NoError(t, stream.Send(&agentv1.AgentMessage{MessageId: "hello"}))
	require.Eventually(t, func() bool { return registry.IsOnline(agentID) },
		3*time.Second, 10*time.Millisecond)

	// Fill the flow-control window so the send goroutine is parked in Send.
	// SendCh refusing a message is the signal that it has stopped draining.
	big := strings.Repeat("x", 1<<20)
	for i := 0; i < 200; i++ {
		if !registry.Send(agentID, &agentv1.ServerMessage{
			Payload: &agentv1.ServerMessage_PushRule{
				PushRule: &agentv1.PushRuleCommand{
					Rule: &agentv1.CollectionRule{RuleId: "r", BasePath: big},
				},
			},
		}) {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}

	require.True(t, registry.Disconnect(agentID))

	assert.Eventually(t, func() bool { return !registry.IsOnline(agentID) },
		8*time.Second, 50*time.Millisecond,
		"handler did not return while the send path was blocked — registry and goroutine leaked")
}
