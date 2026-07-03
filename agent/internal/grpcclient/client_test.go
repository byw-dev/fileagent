package grpcclient

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/agent/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── Test helpers ──────────────────────────────────────────────────────────────

// fakeAgentServer is a minimal in-process AgentServiceServer for tests.
type fakeAgentServer struct {
	agentv1.UnimplementedAgentServiceServer
	receivedCh chan *agentv1.AgentMessage
	// connectErr, when non-nil, is returned immediately from Connect so tests
	// can simulate the Control Plane rejecting the stream (e.g. Unauthenticated).
	connectErr error
	// pollStatus / pollToken shape the PollApproval response used by the reauth
	// path tests.
	pollStatus string
	pollToken  string
}

func newFakeServer() *fakeAgentServer {
	return &fakeAgentServer{receivedCh: make(chan *agentv1.AgentMessage, 16)}
}

// Connect receives all messages until the stream closes, or returns connectErr
// immediately when configured.
func (f *fakeAgentServer) Connect(stream agentv1.AgentService_ConnectServer) error {
	if f.connectErr != nil {
		return f.connectErr
	}
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil
		}
		f.receivedCh <- msg
	}
}

// PollApproval returns a response shaped by pollStatus/pollToken, for exercising
// the reauth self-heal path.
func (f *fakeAgentServer) PollApproval(_ context.Context, _ *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
	return &agentv1.PollApprovalResponse{Status: f.pollStatus, AuthToken: f.pollToken}, nil
}

// startFakeServer starts an in-process gRPC server and returns its address.
func startFakeServer(t *testing.T) (addr string, srv *fakeAgentServer) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv = newFakeServer()
	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, srv)

	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(func() { grpcSrv.Stop() })

	return lis.Addr().String(), srv
}

// buildTestConfig returns a config that points to addr and disables TLS.
func buildTestConfig(addr string) *config.Config {
	return &config.Config{
		Server: config.ServerConfig{Endpoint: addr},
		Upload: config.UploadConfig{Concurrency: 1, PartSizeMB: 64, QueueMaxSize: 100, RetryMax: 3},
		Metrics: config.MetricsConfig{Enabled: false, Port: 9100},
		Log:     config.LogConfig{Level: "info", Output: "/dev/null", MaxSizeMB: 1, MaxBackups: 1},
	}
}

// newInsecureClient builds a Client that dials without TLS for in-process tests.
func newInsecureClient(cfg *config.Config, logger *zap.Logger) *Client {
	return &Client{
		cfg:    cfg,
		logger: logger,
	}
}

// connectInsecure dials with insecure credentials (test only).
func connectInsecure(ctx context.Context, c *Client) error {
	conn, err := grpc.NewClient(c.cfg.Server.Endpoint, InsecureDialOpts()...)
	if err != nil {
		return err
	}
	c.mu.Lock()
	c.conn = conn
	c.svc = agentv1.NewAgentServiceClient(conn)
	c.mu.Unlock()
	go c.runLoop(ctx)
	return nil
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestClient_ConnectAndHeartbeat(t *testing.T) {
	addr, fakeServer := startFakeServer(t)
	logger := zap.NewNop()
	cfg := buildTestConfig(addr)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	client := newInsecureClient(cfg, logger)
	require.NoError(t, connectInsecure(ctx, client))
	defer func() { _ = client.Close() }()

	// Give the runLoop time to open the stream.
	require.Eventually(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.stream != nil
	}, 3*time.Second, 50*time.Millisecond, "stream should be established")

	// Send a heartbeat manually.
	hb := &agentv1.Heartbeat{AgentId: "test-agent", UptimeSeconds: 42}
	require.NoError(t, client.SendHeartbeat(hb))

	// The fake server should receive the message.
	select {
	case msg := <-fakeServer.receivedCh:
		require.NotNil(t, msg.GetHeartbeat())
		assert.Equal(t, "test-agent", msg.GetHeartbeat().AgentId)
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive heartbeat within timeout")
	}
}

func TestClient_SendHeartbeat_NoStream(t *testing.T) {
	logger := zap.NewNop()
	cfg := buildTestConfig("localhost:0")
	client := New(cfg, logger)

	err := client.SendHeartbeat(&agentv1.Heartbeat{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no active stream")
}

func TestClient_Close(t *testing.T) {
	addr, _ := startFakeServer(t)
	logger := zap.NewNop()
	cfg := buildTestConfig(addr)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	client := newInsecureClient(cfg, logger)
	require.NoError(t, connectInsecure(ctx, client))
	assert.NoError(t, client.Close())
}

func TestBackoffDelay(t *testing.T) {
	cases := []struct {
		attempt int
		want    time.Duration
	}{
		{1, 1 * time.Second},
		{2, 2 * time.Second},
		{3, 4 * time.Second},
		{4, 8 * time.Second},
		{7, 60 * time.Second}, // capped
		{10, 60 * time.Second},
	}
	for _, tc := range cases {
		got := backoffDelay(tc.attempt)
		assert.Equal(t, tc.want, got, "attempt=%d", tc.attempt)
	}
}

func TestClient_ReconnectOnStreamClose(t *testing.T) {
	addr, fakeServer := startFakeServer(t)
	logger := zap.NewNop()
	cfg := buildTestConfig(addr)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	client := newInsecureClient(cfg, logger)
	require.NoError(t, connectInsecure(ctx, client))
	defer func() { _ = client.Close() }()

	// Wait for first stream.
	require.Eventually(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.stream != nil
	}, 3*time.Second, 50*time.Millisecond)

	// Send first heartbeat.
	require.NoError(t, client.SendHeartbeat(&agentv1.Heartbeat{AgentId: "reconnect-test"}))
	select {
	case msg := <-fakeServer.receivedCh:
		assert.Equal(t, "reconnect-test", msg.GetHeartbeat().GetAgentId())
	case <-time.After(3 * time.Second):
		t.Fatal("timeout waiting for first heartbeat")
	}
}

// TestClient_ReauthOnUnauthenticated verifies the self-heal path (G-2): when the
// Control Plane rejects the token with Unauthenticated, the client invokes the
// reauth callback and installs the fresh token for the next reconnect, instead
// of looping forever on the stale credential.
func TestClient_ReauthOnUnauthenticated(t *testing.T) {
	addr, fakeServer := startFakeServer(t)
	fakeServer.connectErr = status.Error(codes.Unauthenticated, "token expired")

	logger := zap.NewNop()
	client := newInsecureClient(buildTestConfig(addr), logger)
	client.SetToken("stale-token")

	reauthCalled := make(chan struct{}, 1)
	client.SetReauthFunc(func(context.Context) (string, error) {
		select {
		case reauthCalled <- struct{}{}:
		default:
		}
		return "fresh-token", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, connectInsecure(ctx, client))
	defer func() { _ = client.Close() }()

	select {
	case <-reauthCalled:
	case <-time.After(5 * time.Second):
		t.Fatal("reauth was not triggered on Unauthenticated")
	}

	require.Eventually(t, func() bool {
		client.mu.Lock()
		defer client.mu.Unlock()
		return client.token == "fresh-token"
	}, 3*time.Second, 20*time.Millisecond, "refreshed token should be installed")
}

// TestClient_NoReauthOnNonAuthError verifies the reauth path is gated strictly on
// Unauthenticated: a transient non-auth error must NOT burn a reauth attempt.
func TestClient_NoReauthOnNonAuthError(t *testing.T) {
	addr, fakeServer := startFakeServer(t)
	fakeServer.connectErr = status.Error(codes.Unavailable, "temporary outage")

	logger := zap.NewNop()
	client := newInsecureClient(buildTestConfig(addr), logger)
	client.SetToken("some-token")

	var reauthCalls int32
	client.SetReauthFunc(func(context.Context) (string, error) {
		atomic.AddInt32(&reauthCalls, 1)
		return "should-not-be-used", nil
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, connectInsecure(ctx, client))
	defer func() { _ = client.Close() }()

	// Give the run loop time to fail and (wrongly) attempt reauth if misgated.
	time.Sleep(1500 * time.Millisecond)
	assert.Equal(t, int32(0), atomic.LoadInt32(&reauthCalls),
		"reauth must not fire for non-Unauthenticated errors")
	client.mu.Lock()
	tok := client.token
	client.mu.Unlock()
	assert.Equal(t, "some-token", tok, "token must be untouched on non-auth errors")
}

// TestClient_ReauthEmptyTokenIgnored guards the defensive check: if the reauth
// callback returns an empty token, the client must keep its existing credential
// rather than dropping the Bearer header and reconnecting anonymously.
func TestClient_ReauthEmptyTokenIgnored(t *testing.T) {
	addr, fakeServer := startFakeServer(t)
	fakeServer.connectErr = status.Error(codes.Unauthenticated, "token expired")

	client := newInsecureClient(buildTestConfig(addr), zap.NewNop())
	client.SetToken("original-token")

	var calls int32
	client.SetReauthFunc(func(context.Context) (string, error) {
		atomic.AddInt32(&calls, 1)
		return "", nil // buggy/unexpected empty token with nil error
	})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	require.NoError(t, connectInsecure(ctx, client))
	defer func() { _ = client.Close() }()

	// Wait until reauth has actually been invoked at least once.
	require.Eventually(t, func() bool {
		return atomic.LoadInt32(&calls) > 0
	}, 5*time.Second, 20*time.Millisecond, "reauth should have been attempted")

	client.mu.Lock()
	tok := client.token
	client.mu.Unlock()
	assert.Equal(t, "original-token", tok, "empty reauth token must not overwrite the credential")
}

// dialTestService dials the in-process fake server and returns an AgentService
// client for direct RPC-level tests.
func dialTestService(t *testing.T, addr string) agentv1.AgentServiceClient {
	t.Helper()
	conn, err := grpc.NewClient(addr, InsecureDialOpts()...)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	return agentv1.NewAgentServiceClient(conn)
}

func TestReAuthenticate_ApprovedReturnsToken(t *testing.T) {
	addr, srv := startFakeServer(t)
	srv.pollStatus = "approved"
	srv.pollToken = "fresh-token-123"

	tok, err := ReAuthenticate(context.Background(), dialTestService(t, addr), "agent-1", "fp-1", zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "fresh-token-123", tok)
}

func TestReAuthenticate_NotApproved(t *testing.T) {
	addr, srv := startFakeServer(t)
	srv.pollStatus = "revoked"

	_, err := ReAuthenticate(context.Background(), dialTestService(t, addr), "agent-1", "fp-1", zap.NewNop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not approved")
}

// TestReAuthenticate_ApprovedButNoToken locks the split error case: an approved
// status with an empty token reports a distinct, accurate message rather than
// the misleading "not approved (status=approved)".
func TestReAuthenticate_ApprovedButNoToken(t *testing.T) {
	addr, srv := startFakeServer(t)
	srv.pollStatus = "approved"
	srv.pollToken = ""

	_, err := ReAuthenticate(context.Background(), dialTestService(t, addr), "agent-1", "fp-1", zap.NewNop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no token")
	assert.NotContains(t, err.Error(), "not approved")
}
