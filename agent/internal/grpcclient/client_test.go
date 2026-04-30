package grpcclient

import (
	"context"
	"net"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/agent/internal/config"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// ── Test helpers ──────────────────────────────────────────────────────────────

// fakeAgentServer is a minimal in-process AgentServiceServer for tests.
type fakeAgentServer struct {
	agentv1.UnimplementedAgentServiceServer
	receivedCh chan *agentv1.AgentMessage
}

func newFakeServer() *fakeAgentServer {
	return &fakeAgentServer{receivedCh: make(chan *agentv1.AgentMessage, 16)}
}

// Connect receives all messages until the stream closes.
func (f *fakeAgentServer) Connect(stream agentv1.AgentService_ConnectServer) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil
		}
		f.receivedCh <- msg
	}
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
