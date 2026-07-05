// Package cache provides a thin Redis wrapper used throughout the Control Plane.
// It exposes the common key–value operations required by the system design
// (§3.5) and keeps all key-naming conventions in one place (keys.go).
package cache

import (
	"context"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
	"go.uber.org/zap"
)

// Client wraps a go-redis client with helper methods. All methods accept a
// context so callers can enforce timeouts and cancellation.
type Client struct {
	rdb    *redis.Client
	logger *zap.Logger
}

// New parses redisURL and returns a connected *Client. The connection is
// verified with a PING before returning. Supported URL formats:
//
//	redis://[:password@]host[:port][/db]
//	rediss://...  (TLS)
func New(redisURL string, logger *zap.Logger) (*Client, error) {
	opts, err := redis.ParseURL(redisURL)
	if err != nil {
		return nil, fmt.Errorf("cache: parse redis URL: %w", err)
	}

	rdb := redis.NewClient(opts)
	c := &Client{rdb: rdb, logger: logger}
	if err := c.ping(context.Background()); err != nil {
		_ = rdb.Close()
		return nil, err
	}

	logger.Info("redis connection established", zap.String("addr", opts.Addr))
	return c, nil
}

// Close closes the underlying Redis connection.
func (c *Client) Close() error {
	return c.rdb.Close()
}

// Ping checks that Redis is reachable.
func (c *Client) Ping(ctx context.Context) error {
	return c.ping(ctx)
}

func (c *Client) ping(ctx context.Context) error {
	if err := c.rdb.Ping(ctx).Err(); err != nil {
		return fmt.Errorf("cache: ping failed: %w", err)
	}
	return nil
}

// Get returns the string value stored at key. Returns ("", ErrNil) when the
// key does not exist.
func (c *Client) Get(ctx context.Context, key string) (string, error) {
	val, err := c.rdb.Get(ctx, key).Result()
	if err != nil {
		return "", err
	}
	return val, nil
}

// Set stores value at key with the given TTL. A zero TTL means the key never
// expires.
func (c *Client) Set(ctx context.Context, key string, value interface{}, ttl time.Duration) error {
	return c.rdb.Set(ctx, key, value, ttl).Err()
}

// Del deletes the given keys. It is not an error if a key does not exist.
func (c *Client) Del(ctx context.Context, keys ...string) error {
	return c.rdb.Del(ctx, keys...).Err()
}

// SetNX sets key to value only if the key does not already exist (SET … NX).
// Returns true if the key was set, false if it already existed.
func (c *Client) SetNX(ctx context.Context, key string, value interface{}, ttl time.Duration) (bool, error) {
	ok, err := c.rdb.SetNX(ctx, key, value, ttl).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

// Expire updates the expiry of an existing key. Returns true if the key
// exists and the timeout was set, false if the key does not exist.
func (c *Client) Expire(ctx context.Context, key string, ttl time.Duration) (bool, error) {
	ok, err := c.rdb.Expire(ctx, key, ttl).Result()
	if err != nil {
		return false, err
	}
	return ok, nil
}

// Exists reports whether all of the given keys exist in Redis. The returned
// integer is the number of keys that actually exist (duplicates counted once).
func (c *Client) Exists(ctx context.Context, keys ...string) (int64, error) {
	return c.rdb.Exists(ctx, keys...).Result()
}

// fixedWindowScript atomically increments a counter and, only on the first
// increment (when the key is created), sets its expiry to the window. Doing
// both in one Lua script prevents a crash between INCR and EXPIRE from leaving
// a TTL-less key that would rate-limit a user forever.
var fixedWindowScript = redis.NewScript(`
local count = redis.call("INCR", KEYS[1])
if count == 1 then
	redis.call("EXPIRE", KEYS[1], ARGV[1])
end
return count
`)

// IncrWithWindow atomically increments the fixed-window counter at key and
// returns its new value. On the first increment of a window it sets the key to
// expire after window, so subsequent increments within the window share the
// same expiry (fixed-window rate limiting, system-design.md §5.1 /
// ratelimit:api:{user_id}).
func (c *Client) IncrWithWindow(ctx context.Context, key string, window time.Duration) (int64, error) {
	return fixedWindowScript.Run(ctx, c.rdb, []string{key}, int(window.Seconds())).Int64()
}

// MGet returns the values at the given keys in order; a missing key yields a nil
// element. It batches many presence lookups into a single round-trip.
func (c *Client) MGet(ctx context.Context, keys ...string) ([]interface{}, error) {
	if len(keys) == 0 {
		return nil, nil
	}
	return c.rdb.MGet(ctx, keys...).Result()
}

// HSet sets field in the hash stored at key.
func (c *Client) HSet(ctx context.Context, key string, values ...interface{}) error {
	return c.rdb.HSet(ctx, key, values...).Err()
}

// HGetAll returns all fields and values of the hash stored at key.
func (c *Client) HGetAll(ctx context.Context, key string) (map[string]string, error) {
	return c.rdb.HGetAll(ctx, key).Result()
}

// HExpire sets the TTL for the entire hash key (Redis 7.4+; falls back to
// regular key-level Expire on older servers).
func (c *Client) HExpire(ctx context.Context, key string, ttl time.Duration) error {
	return c.rdb.Expire(ctx, key, ttl).Err()
}

// ErrNil is re-exported from go-redis so callers can import only this package.
var ErrNil = redis.Nil
