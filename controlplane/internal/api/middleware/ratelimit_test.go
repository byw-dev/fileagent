package middleware_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// fakeRateStore is an in-memory fixed-window counter for testing the limiter
// without Redis. errOnce, when set, is returned by the next IncrWithWindow.
type fakeRateStore struct {
	mu      sync.Mutex
	counts  map[string]int64
	failNow bool
}

func newFakeRateStore() *fakeRateStore {
	return &fakeRateStore{counts: map[string]int64{}}
}

func (f *fakeRateStore) IncrWithWindow(_ context.Context, key string, _ time.Duration) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failNow {
		return 0, errors.New("redis down")
	}
	f.counts[key]++
	return f.counts[key], nil
}

// rateLimitRouter builds a router that injects claims for userID (empty = none)
// then applies the rate-limit middleware in front of a 200 handler.
func rateLimitRouter(store middleware.RateLimitStore, perMinute int, userID string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	// Inject JWT claims with the desired Subject so GetClaims returns a non-nil
	// value the limiter can key on. Empty userID leaves claims absent.
	r.Use(func(c *gin.Context) {
		if userID != "" {
			claims := &auth.Claims{}
			claims.Subject = userID
			c.Set("jwt_claims", claims)
		}
		c.Next()
	})
	r.Use(middleware.RateLimit(store, perMinute, zap.NewNop()))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })
	return r
}

func doGet(r *gin.Engine) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	return w
}

func TestRateLimit_UnderLimit_Allows(t *testing.T) {
	store := newFakeRateStore()
	r := rateLimitRouter(store, 3, "user-1")

	for i := 1; i <= 3; i++ {
		w := doGet(r)
		require.Equal(t, http.StatusOK, w.Code, "request %d should pass", i)
		assert.Equal(t, "3", w.Header().Get("X-RateLimit-Limit"))
		assert.Equal(t, strconv.Itoa(3-i), w.Header().Get("X-RateLimit-Remaining"))
	}
}

func TestRateLimit_OverLimit_Returns429(t *testing.T) {
	store := newFakeRateStore()
	r := rateLimitRouter(store, 2, "user-1")

	doGet(r)      // 1
	doGet(r)      // 2 (at limit)
	w := doGet(r) // 3 (over)

	require.Equal(t, http.StatusTooManyRequests, w.Code)
	assert.Equal(t, "0", w.Header().Get("X-RateLimit-Remaining"))
	assert.NotEmpty(t, w.Header().Get("Retry-After"))

	var body middleware.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "RATE_LIMITED", body.Error.Code)
	assert.NotEmpty(t, body.RequestID)
}

func TestRateLimit_PerUserIsolation(t *testing.T) {
	store := newFakeRateStore()
	limit := 1

	// user-a exhausts its budget; user-b must still be allowed.
	require.Equal(t, http.StatusOK, doGet(rateLimitRouter(store, limit, "user-a")).Code)
	require.Equal(t, http.StatusTooManyRequests, doGet(rateLimitRouter(store, limit, "user-a")).Code)
	require.Equal(t, http.StatusOK, doGet(rateLimitRouter(store, limit, "user-b")).Code)
}

func TestRateLimit_NoClaims_FailsOpen(t *testing.T) {
	store := newFakeRateStore()
	r := rateLimitRouter(store, 1, "") // no user injected

	// Many requests, none counted because there is no user to key on.
	for i := 0; i < 5; i++ {
		require.Equal(t, http.StatusOK, doGet(r).Code)
	}
	assert.Empty(t, store.counts, "requests without claims must not be counted")
}

func TestRateLimit_RedisError_FailsOpen(t *testing.T) {
	store := newFakeRateStore()
	store.failNow = true
	r := rateLimitRouter(store, 1, "user-1")

	// Redis failing must not lock the user out.
	require.Equal(t, http.StatusOK, doGet(r).Code)
}
