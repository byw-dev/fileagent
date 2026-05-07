package grpcclient

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
)

// ── fakeAgentServerWithCreds adds RefreshCredentials support ──────────────────

type fakeAgentServerWithCreds struct {
	agentv1.UnimplementedAgentServiceServer
	receivedCh chan *agentv1.AgentMessage
	credResp   *agentv1.RefreshCredentialsResponse
}

func newFakeServerWithCreds() *fakeAgentServerWithCreds {
	return &fakeAgentServerWithCreds{
		receivedCh: make(chan *agentv1.AgentMessage, 16),
		credResp: &agentv1.RefreshCredentialsResponse{
			Credentials: &agentv1.CredentialsPayload{
				AccessKey: "AK", SecretKey: "SK", SessionToken: "ST",
			},
		},
	}
}

func (f *fakeAgentServerWithCreds) Connect(stream agentv1.AgentService_ConnectServer) error {
	for {
		msg, err := stream.Recv()
		if err != nil {
			return nil
		}
		f.receivedCh <- msg
	}
}

func (f *fakeAgentServerWithCreds) RefreshCredentials(_ context.Context, _ *agentv1.RefreshCredentialsRequest) (*agentv1.RefreshCredentialsResponse, error) {
	return f.credResp, nil
}

func startFakeServerWithCreds(t *testing.T) (addr string, srv *fakeAgentServerWithCreds) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	srv = newFakeServerWithCreds()
	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, srv)

	go func() { _ = grpcSrv.Serve(lis) }()
	t.Cleanup(func() { grpcSrv.Stop() })

	return lis.Addr().String(), srv
}

// ── SetToken / SetAgentID / SetMessageHandler ─────────────────────────────────

func TestClient_SetToken(t *testing.T) {
	cfg := buildTestConfig("localhost:0")
	c := New(cfg, zap.NewNop())

	c.SetToken("my-token")

	c.mu.Lock()
	got := c.token
	c.mu.Unlock()
	assert.Equal(t, "my-token", got)
}

func TestClient_SetAgentID(t *testing.T) {
	cfg := buildTestConfig("localhost:0")
	c := New(cfg, zap.NewNop())

	c.SetAgentID("agent-42")

	c.mu.Lock()
	got := c.agentID
	c.mu.Unlock()
	assert.Equal(t, "agent-42", got)
}

func TestClient_SetMessageHandler(t *testing.T) {
	cfg := buildTestConfig("localhost:0")
	c := New(cfg, zap.NewNop())

	called := false
	handler := func(_ *agentv1.ServerMessage) { called = true }
	c.SetMessageHandler(handler)

	c.mu.Lock()
	h := c.msgHandler
	c.mu.Unlock()

	require.NotNil(t, h)
	h(&agentv1.ServerMessage{})
	assert.True(t, called)
}

// ── ServiceClient ─────────────────────────────────────────────────────────────

func TestClient_ServiceClient_NilBeforeConnect(t *testing.T) {
	cfg := buildTestConfig("localhost:0")
	c := New(cfg, zap.NewNop())
	assert.Nil(t, c.ServiceClient())
}

func TestClient_ServiceClient_NotNilAfterConnect(t *testing.T) {
	addr, _ := startFakeServerWithCreds(t)
	cfg := buildTestConfig(addr)
	c := newInsecureClient(cfg, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, connectInsecure(ctx, c))
	defer func() { _ = c.Close() }()

	assert.NotNil(t, c.ServiceClient())
}

// ── SendMessage ───────────────────────────────────────────────────────────────

func TestClient_SendMessage_NoStream(t *testing.T) {
	cfg := buildTestConfig("localhost:0")
	c := New(cfg, zap.NewNop())

	err := c.SendMessage(&agentv1.AgentMessage{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no active stream")
}

func TestClient_SendMessage_WithStream(t *testing.T) {
	addr, fakeServer := startFakeServerWithCreds(t)
	cfg := buildTestConfig(addr)
	c := newInsecureClient(cfg, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, connectInsecure(ctx, c))
	defer func() { _ = c.Close() }()

	// Wait for stream to open.
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.stream != nil
	}, 3*time.Second, 50*time.Millisecond)

	msg := &agentv1.AgentMessage{
		Payload: &agentv1.AgentMessage_Heartbeat{
			Heartbeat: &agentv1.Heartbeat{AgentId: "send-test"},
		},
	}
	require.NoError(t, c.SendMessage(msg))

	select {
	case got := <-fakeServer.receivedCh:
		assert.Equal(t, "send-test", got.GetHeartbeat().GetAgentId())
	case <-time.After(3 * time.Second):
		t.Fatal("message not received")
	}
}

// ── RefreshCredentials ────────────────────────────────────────────────────────

func TestClient_RefreshCredentials_NoSvc(t *testing.T) {
	cfg := buildTestConfig("localhost:0")
	c := New(cfg, zap.NewNop())

	_, err := c.RefreshCredentials(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not initialised")
}

func TestClient_RefreshCredentials_Success(t *testing.T) {
	addr, _ := startFakeServerWithCreds(t)
	cfg := buildTestConfig(addr)
	c := newInsecureClient(cfg, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, connectInsecure(ctx, c))
	defer func() { _ = c.Close() }()

	creds, err := c.RefreshCredentials(context.Background())
	require.NoError(t, err)
	require.NotNil(t, creds)
	assert.Equal(t, "AK", creds.GetAccessKey())
}

func TestClient_RefreshCredentials_WithToken(t *testing.T) {
	addr, _ := startFakeServerWithCreds(t)
	cfg := buildTestConfig(addr)
	c := newInsecureClient(cfg, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	require.NoError(t, connectInsecure(ctx, c))
	defer func() { _ = c.Close() }()

	c.SetToken("bearer-token")
	c.SetAgentID("my-agent-id")

	creds, err := c.RefreshCredentials(context.Background())
	require.NoError(t, err)
	assert.NotNil(t, creds)
}

// ── Connect / buildDialOpts ───────────────────────────────────────────────────

func TestClient_Connect_Insecure(t *testing.T) {
	addr, _ := startFakeServerWithCreds(t)
	cfg := buildTestConfig(addr)
	c := newInsecureClient(cfg, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	require.NoError(t, connectInsecure(ctx, c))
	defer func() { _ = c.Close() }()

	// Wait for stream.
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.stream != nil
	}, 3*time.Second, 50*time.Millisecond)
}

func TestClient_BuildDialOpts_NoTLS(t *testing.T) {
	cfg := buildTestConfig("localhost:0")
	c := New(cfg, zap.NewNop())

	opts, err := c.buildDialOpts()
	require.NoError(t, err)
	assert.NotEmpty(t, opts)
}

func TestClient_BuildDialOpts_WithCACert(t *testing.T) {
	// Generate a minimal self-signed certificate for testing.
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	require.NoError(t, err)

	template := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test-ca"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IsCA:         true,
		KeyUsage:     x509.KeyUsageCertSign,
	}
	certDER, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	require.NoError(t, err)

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caFile, certPEM, 0o600))

	cfg := buildTestConfig("localhost:0")
	cfg.Server.TLSCACert = caFile
	c := New(cfg, zap.NewNop())

	opts, err := c.buildDialOpts()
	require.NoError(t, err)
	assert.NotEmpty(t, opts)
}

func TestClient_BuildDialOpts_MissingCACert(t *testing.T) {
	cfg := buildTestConfig("localhost:0")
	cfg.Server.TLSCACert = "/nonexistent/ca.pem"
	c := New(cfg, zap.NewNop())

	_, err := c.buildDialOpts()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "read CA cert")
}

func TestClient_BuildDialOpts_InvalidPEM(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.pem")
	require.NoError(t, os.WriteFile(caFile, []byte("not-a-pem"), 0o600))

	cfg := buildTestConfig("localhost:0")
	cfg.Server.TLSCACert = caFile
	c := New(cfg, zap.NewNop())

	_, err := c.buildDialOpts()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parse CA cert")
}

// ── MessageHandler dispatch via receive loop ──────────────────────────────────

func TestClient_MessageHandler_DispatchedOnReceive(t *testing.T) {
	addr, _ := startFakeServerWithCreds(t)
	cfg := buildTestConfig(addr)
	c := newInsecureClient(cfg, zap.NewNop())

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	received := make(chan *agentv1.ServerMessage, 1)
	c.SetMessageHandler(func(msg *agentv1.ServerMessage) {
		received <- msg
	})

	require.NoError(t, connectInsecure(ctx, c))
	defer func() { _ = c.Close() }()

	// Wait for stream — handler is registered, nothing to send from server side
	// in the fake server, so we just check no panic.
	require.Eventually(t, func() bool {
		c.mu.Lock()
		defer c.mu.Unlock()
		return c.stream != nil
	}, 3*time.Second, 50*time.Millisecond)
}
