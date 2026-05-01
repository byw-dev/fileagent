package grpcserver

import (
	"context"
	"net"
	"testing"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
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

const bufSize = 1024 * 1024

// newTestServer starts an in-process gRPC server using bufconn and returns a
// client connected to it. The server and connection are automatically closed
// at test cleanup.
func newTestServer(t *testing.T) agentv1.AgentServiceClient {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	srv := New(logger)

	lis := bufconn.Listen(bufSize)
	grpcSrv := srv.GRPCServer()

	go func() { _ = grpcSrv.Serve(lis) }()

	t.Cleanup(func() { grpcSrv.GracefulStop() })

	conn, err := grpc.NewClient("passthrough://bufnet",
		grpc.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return lis.DialContext(ctx)
		}),
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	require.NoError(t, err)
	t.Cleanup(func() { conn.Close() })

	return agentv1.NewAgentServiceClient(conn)
}

func TestServer_Register_ReturnsUnimplemented(t *testing.T) {
	client := newTestServer(t)

	_, err := client.Register(context.Background(), &agentv1.RegisterRequest{
		Fingerprint: "fp-test",
		Hostname:    "host",
		OsType:      "linux",
	})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestServer_PollApproval_ReturnsUnimplemented(t *testing.T) {
	client := newTestServer(t)

	_, err := client.PollApproval(context.Background(), &agentv1.PollApprovalRequest{
		AgentId: "agent-1",
	})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestServer_Connect_ReturnsUnimplemented(t *testing.T) {
	client := newTestServer(t)

	// Connect requires a valid Bearer token in the metadata.
	md := metadata.Pairs("authorization", "Bearer valid-token")
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	stream, err := client.Connect(ctx)
	require.NoError(t, err)

	// The server should close the stream immediately with Unimplemented.
	_, err = stream.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

func TestServer_Connect_MissingToken_ReturnsUnauthenticated(t *testing.T) {
	client := newTestServer(t)

	// No authorization header — should be rejected by the JWT interceptor.
	stream, err := client.Connect(context.Background())
	require.NoError(t, err)

	_, err = stream.Recv()
	require.Error(t, err)
	assert.Equal(t, codes.Unauthenticated, status.Code(err))
}

func TestServer_RefreshCredentials_ReturnsUnimplemented(t *testing.T) {
	client := newTestServer(t)

	md := metadata.Pairs("authorization", "Bearer valid-token")
	ctx := metadata.NewOutgoingContext(context.Background(), md)

	_, err := client.RefreshCredentials(ctx, &agentv1.RefreshCredentialsRequest{
		AgentId: "agent-1",
	})
	require.Error(t, err)
	assert.Equal(t, codes.Unimplemented, status.Code(err))
}

