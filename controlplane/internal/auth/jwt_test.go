package auth

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// mockRedis is a simple in-memory Redis mock for unit tests.
type mockRedis struct {
	store map[string]string
}

func newMockRedis() *mockRedis {
	return &mockRedis{store: make(map[string]string)}
}

func (m *mockRedis) Set(_ context.Context, key string, value interface{}, _ time.Duration) error {
	m.store[key] = value.(string)
	return nil
}

func (m *mockRedis) Exists(_ context.Context, keys ...string) (int64, error) {
	var count int64
	for _, k := range keys {
		if _, ok := m.store[k]; ok {
			count++
		}
	}
	return count, nil
}

const testSecret = "test-secret-for-jwt-service"

func TestGenerateAndValidateAccessToken(t *testing.T) {
	svc := New(testSecret, nil)

	token, err := svc.GenerateAccessToken("user-1", "org-1", "org_admin", "alice", time.Hour)
	require.NoError(t, err)
	require.NotEmpty(t, token)

	claims, err := svc.ValidateToken(token)
	require.NoError(t, err)
	assert.Equal(t, "user-1", claims.Subject)
	assert.Equal(t, "org-1", claims.OrgID)
	assert.Equal(t, "org_admin", claims.Role)
	assert.Equal(t, "alice", claims.Username)
	assert.Equal(t, "access", claims.TokenType)
	assert.NotEmpty(t, claims.ID) // jti
}

func TestGenerateAndValidateRefreshToken(t *testing.T) {
	svc := New(testSecret, nil)

	token, err := svc.GenerateRefreshToken("user-1", "org-1", "org_admin", "alice", 24*time.Hour)
	require.NoError(t, err)

	claims, err := svc.ValidateToken(token)
	require.NoError(t, err)
	assert.Equal(t, "refresh", claims.TokenType)
}

func TestValidateToken_Expired(t *testing.T) {
	svc := New(testSecret, nil)

	token, err := svc.GenerateAccessToken("user-1", "org-1", "org_admin", "alice", -time.Second)
	require.NoError(t, err)

	_, err = svc.ValidateToken(token)
	require.Error(t, err)
}

func TestValidateToken_WrongSecret(t *testing.T) {
	svc := New(testSecret, nil)
	svcOther := New("different-secret", nil)

	token, err := svc.GenerateAccessToken("user-1", "org-1", "org_admin", "alice", time.Hour)
	require.NoError(t, err)

	_, err = svcOther.ValidateToken(token)
	require.Error(t, err)
}

func TestRevokeToken_IsRevoked(t *testing.T) {
	redis := newMockRedis()
	svc := New(testSecret, redis)
	ctx := context.Background()

	token, err := svc.GenerateAccessToken("user-1", "org-1", "org_admin", "alice", time.Hour)
	require.NoError(t, err)

	claims, err := svc.ValidateToken(token)
	require.NoError(t, err)

	// Not yet revoked.
	revoked, err := svc.IsRevoked(ctx, claims.ID)
	require.NoError(t, err)
	assert.False(t, revoked)

	// Revoke.
	err = svc.RevokeToken(ctx, token)
	require.NoError(t, err)

	// Now it should be revoked.
	revoked, err = svc.IsRevoked(ctx, claims.ID)
	require.NoError(t, err)
	assert.True(t, revoked)
}

func TestRevokeToken_NilRedis(t *testing.T) {
	svc := New(testSecret, nil)
	ctx := context.Background()

	token, err := svc.GenerateAccessToken("user-1", "org-1", "org_admin", "alice", time.Hour)
	require.NoError(t, err)

	// Should be a no-op, not panic.
	err = svc.RevokeToken(ctx, token)
	require.NoError(t, err)

	claims, _ := svc.ValidateToken(token)
	revoked, err := svc.IsRevoked(ctx, claims.ID)
	require.NoError(t, err)
	assert.False(t, revoked)
}
