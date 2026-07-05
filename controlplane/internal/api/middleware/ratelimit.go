package middleware

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// rateLimitWindow is the fixed window over which requests are counted. The Redis
// counter key (ratelimit:api:{user_id}) expires after this window, matching the
// 1-minute TTL in system-design.md §3.5.
const rateLimitWindow = time.Minute

// RateLimitStore is the minimal Redis operation the rate-limiter needs. The
// production *cache.Client satisfies it; tests supply a fake.
type RateLimitStore interface {
	// IncrWithWindow atomically increments the counter at key and returns the
	// new value, setting key's expiry to window on the first increment.
	IncrWithWindow(ctx context.Context, key string, window time.Duration) (int64, error)
}

// RateLimit returns a Gin middleware that enforces a per-user fixed-window API
// rate limit of perMinute requests. It must be installed after the JWT
// middleware so the caller's user ID is available in the context; requests
// without JWT claims are not counted (fail open).
//
// The counter lives in Redis under ratelimit:api:{user_id} with a 1-minute TTL
// (system-design.md §5.1). Every response carries X-RateLimit-Limit and
// X-RateLimit-Remaining headers. When the limit is exceeded the request is
// rejected with 429 RATE_LIMITED and a Retry-After header.
//
// The limiter fails open: if Redis is unavailable the request is allowed and
// the error is logged, so a Redis blip cannot lock every user out of the API.
func RateLimit(store RateLimitStore, perMinute int, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		claims := GetClaims(c)
		if claims == nil || claims.Subject == "" {
			// No authenticated user to key on — cannot rate-limit per user.
			c.Next()
			return
		}

		key := cache.RateLimitKey(claims.Subject)
		count, err := store.IncrWithWindow(c.Request.Context(), key, rateLimitWindow)
		if err != nil {
			logger.Warn("rate limit: redis error, failing open",
				zap.String("user_id", claims.Subject), zap.Error(err))
			c.Next()
			return
		}

		c.Header("X-RateLimit-Limit", strconv.Itoa(perMinute))
		remaining := perMinute - int(count)
		if remaining < 0 {
			remaining = 0
		}
		c.Header("X-RateLimit-Remaining", strconv.Itoa(remaining))

		if int(count) > perMinute {
			c.Header("Retry-After", strconv.Itoa(int(rateLimitWindow.Seconds())))
			AbortWithError(c, http.StatusTooManyRequests, "RATE_LIMITED",
				"API rate limit exceeded, retry later", nil)
			return
		}

		c.Next()
	}
}
