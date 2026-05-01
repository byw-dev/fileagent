package grpcserver

import (
	"context"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/grpc/metadata"
)

// ── mock JWT service ──────────────────────────────────────────────────────────

type mockJWTSvc struct {
	claims   *auth.Claims
	err      error
	revoked  bool
	revokeErr error
}

func (m *mockJWTSvc) GenerateAccessToken(subject, orgID, role, username string, ttl time.Duration) (string, error) {
	return "mock-token", nil
}
func (m *mockJWTSvc) GenerateRefreshToken(subject, orgID, role, username string, ttl time.Duration) (string, error) {
	return "mock-refresh", nil
}
func (m *mockJWTSvc) ValidateToken(token string) (*auth.Claims, error) {
	return m.claims, m.err
}
func (m *mockJWTSvc) RevokeToken(ctx context.Context, tokenStr string) error {
	return m.revokeErr
}
func (m *mockJWTSvc) IsRevoked(ctx context.Context, jti string) (bool, error) {
	return m.revoked, m.revokeErr
}

// ── helpers ───────────────────────────────────────────────────────────────────

func ctxWithAuth(token string) context.Context {
	md := metadata.Pairs("authorization", "Bearer "+token)
	return metadata.NewIncomingContext(context.Background(), md)
}

func validClaims() *auth.Claims {
	c := &auth.Claims{}
	c.RegisteredClaims = jwt.RegisteredClaims{
		Subject:   "agent-123",
		ID:        "jti-abc",
		ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
	}
	return c
}

// ── authenticateGRPC tests ────────────────────────────────────────────────────

func TestAuthenticateGRPC_NoMetadata(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	_, err := authenticateGRPC(context.Background(), logger, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing metadata")
}

func TestAuthenticateGRPC_NoAuthHeader(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	md := metadata.Pairs("x-other", "value")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err := authenticateGRPC(ctx, logger, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "authorization")
}

func TestAuthenticateGRPC_NonBearerScheme(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	md := metadata.Pairs("authorization", "Basic abc123")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err := authenticateGRPC(ctx, logger, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Bearer")
}

func TestAuthenticateGRPC_EmptyToken(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	md := metadata.Pairs("authorization", "Bearer ")
	ctx := metadata.NewIncomingContext(context.Background(), md)
	_, err := authenticateGRPC(ctx, logger, nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "empty token")
}

func TestAuthenticateGRPC_NilJWTSvc_AcceptsToken(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	ctx := ctxWithAuth("some-valid-looking-token")
	newCtx, err := authenticateGRPC(ctx, logger, nil)
	require.NoError(t, err)
	assert.NotNil(t, newCtx)
}

func TestAuthenticateGRPC_WithJWTSvc_ValidToken(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	svc := &mockJWTSvc{claims: validClaims()}
	ctx := ctxWithAuth("real-jwt-token")
	newCtx, err := authenticateGRPC(ctx, logger, svc)
	require.NoError(t, err)
	// Claims should be stored in context.
	claims, ok := newCtx.Value(claimsContextKey).(*auth.Claims)
	require.True(t, ok)
	assert.Equal(t, "agent-123", claims.Subject)
}

func TestAuthenticateGRPC_WithJWTSvc_InvalidToken(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	svc := &mockJWTSvc{err: assert.AnError}
	ctx := ctxWithAuth("bad-token")
	_, err := authenticateGRPC(ctx, logger, svc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid")
}

func TestAuthenticateGRPC_WithJWTSvc_RevokedToken(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	svc := &mockJWTSvc{claims: validClaims(), revoked: true}
	ctx := ctxWithAuth("revoked-token")
	_, err := authenticateGRPC(ctx, logger, svc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "revoked")
}

func TestAuthenticateGRPC_WithJWTSvc_RevocationCheckError(t *testing.T) {
	logger, _ := zap.NewDevelopment()
	svc := &mockJWTSvc{claims: validClaims(), revokeErr: assert.AnError}
	ctx := ctxWithAuth("some-token")
	_, err := authenticateGRPC(ctx, logger, svc)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "internal")
}

// ── wrappedStream.Context ─────────────────────────────────────────────────────

type fakeStream struct{}

func (f *fakeStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeStream) SetTrailer(metadata.MD)       {}
func (f *fakeStream) Context() context.Context     { return context.Background() }
func (f *fakeStream) SendMsg(m interface{}) error  { return nil }
func (f *fakeStream) RecvMsg(m interface{}) error  { return nil }

func TestWrappedStream_ContextIsInjected(t *testing.T) {
	type ctxKey string
	customCtx := context.WithValue(context.Background(), ctxKey("test"), "value")
	ws := &wrappedStream{ServerStream: &fakeStream{}, ctx: customCtx}
	assert.Equal(t, customCtx, ws.Context())
}
