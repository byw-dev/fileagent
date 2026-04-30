package cache

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newTestClient starts an in-process miniredis server and returns a *Client
// connected to it. The server is automatically stopped at test cleanup.
func newTestClient(t *testing.T) *Client {
	t.Helper()
	mr := miniredis.RunT(t)
	logger, _ := zap.NewDevelopment()
	c, err := New("redis://"+mr.Addr(), logger)
	require.NoError(t, err)
	t.Cleanup(func() { c.Close() })
	return c
}

func TestPing(t *testing.T) {
	c := newTestClient(t)
	assert.NoError(t, c.Ping(context.Background()))
}

func TestSetAndGet(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	require.NoError(t, c.Set(ctx, "foo", "bar", 0))
	val, err := c.Get(ctx, "foo")
	require.NoError(t, err)
	assert.Equal(t, "bar", val)
}

func TestGet_KeyNotFound_ReturnsErrNil(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	_, err := c.Get(ctx, "nonexistent")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrNil))
}

func TestDel(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	require.NoError(t, c.Set(ctx, "to-delete", "v", 0))
	require.NoError(t, c.Del(ctx, "to-delete"))

	_, err := c.Get(ctx, "to-delete")
	assert.True(t, errors.Is(err, ErrNil))
}

func TestDel_NonExistentKey_NoError(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)
	assert.NoError(t, c.Del(ctx, "ghost"))
}

func TestSetNX_KeyAbsent_SetsAndReturnsTrue(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	ok, err := c.SetNX(ctx, "nxkey", "value", 0)
	require.NoError(t, err)
	assert.True(t, ok)

	val, _ := c.Get(ctx, "nxkey")
	assert.Equal(t, "value", val)
}

func TestSetNX_KeyPresent_ReturnsFalse(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	require.NoError(t, c.Set(ctx, "existing", "original", 0))
	ok, err := c.SetNX(ctx, "existing", "new", 0)
	require.NoError(t, err)
	assert.False(t, ok)

	val, _ := c.Get(ctx, "existing")
	assert.Equal(t, "original", val)
}

func TestExpire(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	require.NoError(t, c.Set(ctx, "ttlkey", "v", 0))
	ok, err := c.Expire(ctx, "ttlkey", 100*time.Millisecond)
	require.NoError(t, err)
	assert.True(t, ok)

	// Key should still exist immediately after setting the TTL.
	val, err := c.Get(ctx, "ttlkey")
	require.NoError(t, err)
	assert.Equal(t, "v", val)
}

func TestExpire_NonExistentKey_ReturnsFalse(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	ok, err := c.Expire(ctx, "no-such-key", time.Minute)
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestExists(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	require.NoError(t, c.Set(ctx, "present", "1", 0))

	n, err := c.Exists(ctx, "present")
	require.NoError(t, err)
	assert.Equal(t, int64(1), n)

	n, err = c.Exists(ctx, "absent")
	require.NoError(t, err)
	assert.Equal(t, int64(0), n)
}

func TestHSetAndHGetAll(t *testing.T) {
	ctx := context.Background()
	c := newTestClient(t)

	require.NoError(t, c.HSet(ctx, "myhash", "field1", "val1", "field2", "val2"))
	m, err := c.HGetAll(ctx, "myhash")
	require.NoError(t, err)
	assert.Equal(t, map[string]string{"field1": "val1", "field2": "val2"}, m)
}

// ── Key helper tests ──────────────────────────────────────────────────────────

func TestAgentOnlineKey(t *testing.T) {
	assert.Equal(t, "agent:abc-123:online", AgentOnlineKey("abc-123"))
}

func TestAgentSTSKey(t *testing.T) {
	assert.Equal(t, "agent:abc-123:sts", AgentSTSKey("abc-123"))
}

func TestJWTBlacklistKey(t *testing.T) {
	assert.Equal(t, "jwt:jti:some-jti", JWTBlacklistKey("some-jti"))
}

func TestRateLimitKey(t *testing.T) {
	assert.Equal(t, "ratelimit:api:user-1", RateLimitKey("user-1"))
}

func TestLockTaskKey(t *testing.T) {
	assert.Equal(t, "lock:task:rule-99", LockTaskKey("rule-99"))
}

func TestSessionKey(t *testing.T) {
	assert.Equal(t, "session:tok", SessionKey("tok"))
}
