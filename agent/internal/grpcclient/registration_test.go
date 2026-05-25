package grpcclient

import (
	"context"
	"encoding/base64"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/config"
	"github.com/byw-dev/fileagent/agent/internal/credential"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// ── fake server helpers ───────────────────────────────────────────────────────

type fakeRegistrationServer struct {
	agentv1.UnimplementedAgentServiceServer
	registerFn     func(*agentv1.RegisterRequest) (*agentv1.RegisterResponse, error)
	pollApprovalFn func(*agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error)
	pollCallCount  int
}

func (f *fakeRegistrationServer) Register(_ context.Context, req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
	if f.registerFn != nil {
		return f.registerFn(req)
	}
	return &agentv1.RegisterResponse{AgentId: "agent-001", Status: "pending"}, nil
}

func (f *fakeRegistrationServer) PollApproval(_ context.Context, req *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
	f.pollCallCount++
	if f.pollApprovalFn != nil {
		return f.pollApprovalFn(req)
	}
	return &agentv1.PollApprovalResponse{Status: "approved", AuthToken: "tok-xyz"}, nil
}

func startRegistrationServer(t *testing.T, srv *fakeRegistrationServer) (agentv1.AgentServiceClient, func()) {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)

	grpcSrv := grpc.NewServer()
	agentv1.RegisterAgentServiceServer(grpcSrv, srv)
	go func() { _ = grpcSrv.Serve(lis) }()

	conn, err := grpc.NewClient(lis.Addr().String(), InsecureDialOpts()...)
	require.NoError(t, err)

	client := agentv1.NewAgentServiceClient(conn)
	cleanup := func() {
		_ = conn.Close()
		grpcSrv.Stop()
	}
	return client, cleanup
}

func setRegisterRetryDelays(t *testing.T, initial, max time.Duration) {
	t.Helper()
	prevInitial := registerRetryInitialDelay
	prevMax := registerRetryMaxDelay
	registerRetryInitialDelay = initial
	registerRetryMaxDelay = max
	t.Cleanup(func() {
		registerRetryInitialDelay = prevInitial
		registerRetryMaxDelay = prevMax
	})
}

// ── LoadOrCreateFingerprint tests ─────────────────────────────────────────────

func TestLoadOrCreateFingerprint_NewFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fingerprint.txt")

	fp, err := LoadOrCreateFingerprint(path)
	require.NoError(t, err)
	assert.NotEmpty(t, fp)

	// Second call must return the same value.
	fp2, err := LoadOrCreateFingerprint(path)
	require.NoError(t, err)
	assert.Equal(t, fp, fp2)
}

func TestLoadOrCreateFingerprint_ExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "fingerprint.txt")
	require.NoError(t, os.WriteFile(path, []byte("my-existing-fp\n"), 0o600))

	fp, err := LoadOrCreateFingerprint(path)
	require.NoError(t, err)
	assert.Equal(t, "my-existing-fp", fp)
}

func TestLoadOrCreateFingerprint_InvalidDir(t *testing.T) {
	_, err := LoadOrCreateFingerprint("/nonexistent/dir/fp.txt")
	require.Error(t, err)
}

// ── Register tests ────────────────────────────────────────────────────────────

func TestRegister_HappyPath(t *testing.T) {
	srv := &fakeRegistrationServer{
		registerFn: func(req *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
			assert.Equal(t, "test-fp", req.GetFingerprint())
			return &agentv1.RegisterResponse{AgentId: "agent-42", Status: "pending"}, nil
		},
	}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	cfg := &config.Config{}
	agentID, _, err := Register(context.Background(), svc, cfg, "test-fp", zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "agent-42", agentID)
}

func TestRegister_Rejected(t *testing.T) {
	srv := &fakeRegistrationServer{
		registerFn: func(_ *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
			return &agentv1.RegisterResponse{Status: "rejected", Message: "banned"}, nil
		},
	}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	_, _, err := Register(context.Background(), svc, &config.Config{}, "fp", zap.NewNop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rejected")
}

func TestRegister_RPCError(t *testing.T) {
	srv := &fakeRegistrationServer{
		registerFn: func(_ *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
			return nil, fmt.Errorf("internal error")
		},
	}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	_, _, err := Register(context.Background(), svc, &config.Config{}, "fp", zap.NewNop())
	require.Error(t, err)
}

// ── PollApproval tests ────────────────────────────────────────────────────────

func TestPollApproval_ImmediateApproval(t *testing.T) {
	srv := &fakeRegistrationServer{
		pollApprovalFn: func(_ *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
			return &agentv1.PollApprovalResponse{Status: "approved", AuthToken: "tok-abc"}, nil
		},
	}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	tok, _, err := PollApproval(context.Background(), svc, "agent-1", "fp", 10*time.Millisecond, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "tok-abc", tok)
}

func TestPollApproval_ContextCancelled(t *testing.T) {
	srv := &fakeRegistrationServer{
		pollApprovalFn: func(_ *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
			return &agentv1.PollApprovalResponse{Status: "pending"}, nil
		},
	}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	_, _, err := PollApproval(ctx, svc, "agent-1", "fp", 10*time.Millisecond, zap.NewNop())
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}

func TestPollApproval_Rejected(t *testing.T) {
	srv := &fakeRegistrationServer{
		pollApprovalFn: func(_ *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
			return &agentv1.PollApprovalResponse{Status: "rejected", Message: "not allowed"}, nil
		},
	}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	_, _, err := PollApproval(context.Background(), svc, "agent-1", "fp", 10*time.Millisecond, zap.NewNop())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "rejected")
}

func TestPollApproval_PendingThenApproved(t *testing.T) {
	callCount := 0
	srv := &fakeRegistrationServer{
		pollApprovalFn: func(_ *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
			callCount++
			if callCount < 3 {
				return &agentv1.PollApprovalResponse{Status: "pending"}, nil
			}
			return &agentv1.PollApprovalResponse{Status: "approved", AuthToken: "final-tok"}, nil
		},
	}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	tok, _, err := PollApproval(context.Background(), svc, "agent-1", "fp", 10*time.Millisecond, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, "final-tok", tok)
	assert.Equal(t, 3, callCount)
}

// ── RegistrationStateMachine tests ───────────────────────────────────────────

func TestStateMachine_Transitions(t *testing.T) {
	sm := NewRegistrationStateMachine()
	assert.Equal(t, StateInit, sm.Current())

	require.NoError(t, sm.Transition(StatePending))
	assert.Equal(t, StatePending, sm.Current())

	require.NoError(t, sm.Transition(StateApproved))
	assert.Equal(t, StateApproved, sm.Current())

	require.NoError(t, sm.Transition(StateRunning))
	assert.Equal(t, StateRunning, sm.Current())

	require.NoError(t, sm.Transition(StateOffline))
	require.NoError(t, sm.Transition(StateRevoked))
}

func TestState_String(t *testing.T) {
	assert.Equal(t, "INIT", StateInit.String())
	assert.Equal(t, "PENDING", StatePending.String())
	assert.Equal(t, "APPROVED", StateApproved.String())
	assert.Equal(t, "RUNNING", StateRunning.String())
	assert.Equal(t, "OFFLINE", StateOffline.String())
	assert.Equal(t, "REVOKED", StateRevoked.String())
}

// ── Lifecycle tests ───────────────────────────────────────────────────────────

func TestLifecycle_Start_TokenAlreadyExists(t *testing.T) {
	dir := t.TempDir()
	fpFile := filepath.Join(dir, "fp.txt")
	tokFile := filepath.Join(dir, "token.enc")

	// Create a syntactically valid JWT (unsigned) with future expiry.
	now := time.Now()
	exp := now.Add(2 * time.Hour).Unix()
	iat := now.Unix()
	header := base64Encode(`{"alg":"HS256","typ":"JWT"}`)
	payload := base64Encode(fmt.Sprintf(`{"exp":%d,"iat":%d}`, exp, iat))
	validJWT := header + "." + payload + ".fakesig"

	tm := credential.NewTokenManager(tokFile, "machine-id")
	require.NoError(t, tm.Save(validJWT))

	sm := credential.NewSTSManager()
	lc := NewLifecycle(tm, sm)

	cfg := &config.Config{
		Agent: config.AgentConfig{FingerprintFile: fpFile, TokenFile: tokFile},
	}

	// A fresh TokenManager (simulating the process restarting) must load from disk.
	tm2 := credential.NewTokenManager(tokFile, "machine-id")
	lc2 := NewLifecycle(tm2, sm)

	srv := &fakeRegistrationServer{}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	err := lc2.Start(context.Background(), svc, cfg, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, StateApproved, lc2.StateMachine.Current())
	assert.Equal(t, 0, srv.pollCallCount, "PollApproval should not be called when token is valid")
	_ = lc // suppress unused warning
}

// base64Encode returns the base64url (no-padding) encoding of s.
func base64Encode(s string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(s))
}

func TestLifecycle_Start_HappyPath(t *testing.T) {
	dir := t.TempDir()
	fpFile := filepath.Join(dir, "fp.txt")
	tokFile := filepath.Join(dir, "token.enc")

	srv := &fakeRegistrationServer{
		registerFn: func(_ *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
			return &agentv1.RegisterResponse{AgentId: "lc-agent-1", Status: "pending", AgentName: "host-1"}, nil
		},
		pollApprovalFn: func(_ *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
			return &agentv1.PollApprovalResponse{Status: "approved", AuthToken: "lifecycle-tok", AgentName: "host-1"}, nil
		},
	}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	tm := credential.NewTokenManager(tokFile, "machine-id")
	sm := credential.NewSTSManager()
	lc := NewLifecycle(tm, sm)

	cfg := &config.Config{
		Agent: config.AgentConfig{FingerprintFile: fpFile, TokenFile: tokFile},
	}

	err := lc.Start(context.Background(), svc, cfg, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, StateApproved, lc.StateMachine.Current())
	assert.Equal(t, "lc-agent-1", lc.AgentID)
	assert.Equal(t, "host-1", lc.AgentName)
}

func TestRegister_RetryOnTransientError(t *testing.T) {
	setRegisterRetryDelays(t, 10*time.Millisecond, 20*time.Millisecond)

	dir := t.TempDir()
	fpFile := filepath.Join(dir, "fp.txt")
	tokFile := filepath.Join(dir, "token.enc")
	attempt := 0

	srv := &fakeRegistrationServer{
		registerFn: func(_ *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
			attempt++
			if attempt < 3 {
				return nil, status.Error(codes.Unavailable, "temporary unavailable")
			}
			return &agentv1.RegisterResponse{AgentId: "retry-agent", Status: "pending"}, nil
		},
		pollApprovalFn: func(_ *agentv1.PollApprovalRequest) (*agentv1.PollApprovalResponse, error) {
			return &agentv1.PollApprovalResponse{Status: "approved", AuthToken: "retry-token"}, nil
		},
	}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	lc := NewLifecycle(credential.NewTokenManager(tokFile, "machine-id"), credential.NewSTSManager())
	cfg := &config.Config{Agent: config.AgentConfig{FingerprintFile: fpFile, TokenFile: tokFile}}

	err := lc.Start(context.Background(), svc, cfg, zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, 3, attempt)
	assert.Equal(t, "retry-agent", lc.AgentID)
}

func TestRegister_StopOnRejected(t *testing.T) {
	setRegisterRetryDelays(t, 10*time.Millisecond, 20*time.Millisecond)

	dir := t.TempDir()
	fpFile := filepath.Join(dir, "fp.txt")
	tokFile := filepath.Join(dir, "token.enc")
	attempt := 0

	srv := &fakeRegistrationServer{
		registerFn: func(_ *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
			attempt++
			return nil, status.Error(codes.PermissionDenied, "agent rejected")
		},
	}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	lc := NewLifecycle(credential.NewTokenManager(tokFile, "machine-id"), credential.NewSTSManager())
	cfg := &config.Config{Agent: config.AgentConfig{FingerprintFile: fpFile, TokenFile: tokFile}}

	err := lc.Start(context.Background(), svc, cfg, zap.NewNop())
	require.Error(t, err)
	assert.Equal(t, 1, attempt)
}

func TestRegister_StopOnContextCancel(t *testing.T) {
	setRegisterRetryDelays(t, 10*time.Millisecond, 20*time.Millisecond)

	dir := t.TempDir()
	fpFile := filepath.Join(dir, "fp.txt")
	tokFile := filepath.Join(dir, "token.enc")

	srv := &fakeRegistrationServer{
		registerFn: func(_ *agentv1.RegisterRequest) (*agentv1.RegisterResponse, error) {
			return nil, status.Error(codes.Unavailable, "temporary unavailable")
		},
	}
	svc, cleanup := startRegistrationServer(t, srv)
	defer cleanup()

	lc := NewLifecycle(credential.NewTokenManager(tokFile, "machine-id"), credential.NewSTSManager())
	cfg := &config.Config{Agent: config.AgentConfig{FingerprintFile: fpFile, TokenFile: tokFile}}

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := lc.Start(ctx, svc, cfg, zap.NewNop())
	require.Error(t, err)
	assert.ErrorIs(t, err, context.DeadlineExceeded)
}
