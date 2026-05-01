package grpcserver

import (
	"context"
	"net"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/alicebob/miniredis/v2"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
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
