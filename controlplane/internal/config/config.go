// Package config provides environment-variable-based configuration loading for
// the Control Plane server. All required fields must be present in the
// environment or Load returns an error, causing the process to exit.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

// Config holds the complete runtime configuration for the Control Plane.
// Values are loaded exclusively from environment variables; no hard-coded
// defaults exist for security-sensitive fields.
type Config struct {
	// HTTP REST API server port (default 8080).
	HTTPPort int
	// gRPC server port (default 9090).
	GRPCPort int

	// DatabaseURL is the PostgreSQL connection string (required).
	// Example: postgres://user:pass@localhost:5432/fileagent?sslmode=disable
	DatabaseURL string

	// MigrationsPath is the directory that contains golang-migrate SQL files.
	// Defaults to "migrations" (relative to working directory).
	MigrationsPath string

	// RedisURL is the Redis connection URL (required).
	// Example: redis://:password@localhost:6379/0
	RedisURL string

	// JWTSecret is the HMAC-SHA256 signing secret for JWT tokens (required).
	JWTSecret string
	// JWTAccessTokenTTL is the lifetime of an Access Token (default 2h).
	JWTAccessTokenTTL time.Duration
	// JWTRefreshTokenTTL is the lifetime of a Refresh Token (default 720h / 30 days).
	JWTRefreshTokenTTL time.Duration
	// AgentTokenTTL is the lifetime of the JWT issued to an Agent at approval
	// (default 720h / 30 days). Agents hold a long-lived gRPC connection and
	// reuse this token across reconnects, so a short TTL (e.g. the 2h access
	// TTL) would cause an Agent to be permanently rejected after any reconnect
	// past expiry. See system-design.md §4.7 and appendix C.1 (AGENT_TOKEN_TTL).
	AgentTokenTTL time.Duration

	// MinIO configuration.
	MinIOEndpoint  string // required, e.g. "minio.internal:9000"
	MinIOAccessKey string // required
	MinIOSecretKey string // required
	MinIOUseSSL    bool   // default false
	// MinIORoleARN is the STS AssumeRole ARN used when issuing Agent credentials.
	MinIORoleARN string

	// NATSURL is the NATS JetStream connection URL (required).
	// Example: nats://localhost:4222
	NATSURL string

	// LogLevel controls zap log verbosity. Valid values: debug, info, warn, error.
	// Defaults to "info".
	LogLevel string

	// APIRateLimitPerMinute is the maximum number of authenticated API requests
	// a single user may make per minute (fixed window, system-design.md §5.1).
	// A value <= 0 disables rate limiting. Default 600 (10 req/s per user).
	APIRateLimitPerMinute int

	// InternalWebhookSecret is the shared secret for MinIO → Control Plane events
	// (MinIO notify_webhook auth_token). When empty, the /internal/minio-event
	// endpoint fails closed and rejects every request, since it mutates the file
	// index from an external source and must be authenticated.
	InternalWebhookSecret string

	// BootstrapAdminUsername is the username used for first-start admin creation.
	// Default: "admin".
	BootstrapAdminUsername string
	// BootstrapAdminPassword optionally sets the bootstrap admin password.
	// If empty and first-start bootstrap is needed, a random password is generated.
	BootstrapAdminPassword string
	// BootstrapAdminForceReset forces resetting the bootstrap admin password on
	// startup when BootstrapAdminPassword is set.
	BootstrapAdminForceReset bool
	// BootstrapAdminCredentialsFile is the file path where first-start generated
	// admin credentials are written with 0600 permissions.
	BootstrapAdminCredentialsFile string
}

// Load reads configuration from environment variables and returns a validated
// *Config. Missing required variables are collected and returned as a single
// error so the caller can log all problems at once before exiting.
func Load() (*Config, error) {
	var missing []string

	cfg := &Config{}

	// ── ports ────────────────────────────────────────────────────────────────
	cfg.HTTPPort = envInt("HTTP_PORT", 8080)
	cfg.GRPCPort = envInt("GRPC_PORT", 9090)

	// ── database ─────────────────────────────────────────────────────────────
	cfg.DatabaseURL = os.Getenv("DATABASE_URL")
	if cfg.DatabaseURL == "" {
		missing = append(missing, "DATABASE_URL")
	}
	cfg.MigrationsPath = envString("MIGRATIONS_PATH", "migrations")

	// ── redis ─────────────────────────────────────────────────────────────────
	cfg.RedisURL = os.Getenv("REDIS_URL")
	if cfg.RedisURL == "" {
		missing = append(missing, "REDIS_URL")
	}

	// ── jwt ──────────────────────────────────────────────────────────────────
	cfg.JWTSecret = os.Getenv("JWT_SECRET")
	if cfg.JWTSecret == "" {
		missing = append(missing, "JWT_SECRET")
	}
	cfg.JWTAccessTokenTTL = envDuration("JWT_ACCESS_TOKEN_TTL", 2*time.Hour)
	cfg.JWTRefreshTokenTTL = envDuration("JWT_REFRESH_TOKEN_TTL", 720*time.Hour)
	cfg.AgentTokenTTL = envDuration("AGENT_TOKEN_TTL", 720*time.Hour)

	// ── minio ─────────────────────────────────────────────────────────────────
	cfg.MinIOEndpoint = os.Getenv("MINIO_ENDPOINT")
	if cfg.MinIOEndpoint == "" {
		missing = append(missing, "MINIO_ENDPOINT")
	}
	cfg.MinIOAccessKey = os.Getenv("MINIO_ACCESS_KEY")
	if cfg.MinIOAccessKey == "" {
		missing = append(missing, "MINIO_ACCESS_KEY")
	}
	cfg.MinIOSecretKey = os.Getenv("MINIO_SECRET_KEY")
	if cfg.MinIOSecretKey == "" {
		missing = append(missing, "MINIO_SECRET_KEY")
	}
	cfg.MinIOUseSSL = envBool("MINIO_USE_SSL", false)
	cfg.MinIORoleARN = envString("MINIO_ROLE_ARN", "arn:aws:iam:::role/agent-role")

	// ── nats ──────────────────────────────────────────────────────────────────
	cfg.NATSURL = os.Getenv("NATS_URL")
	if cfg.NATSURL == "" {
		missing = append(missing, "NATS_URL")
	}

	// ── misc ─────────────────────────────────────────────────────────────────
	cfg.LogLevel = envString("LOG_LEVEL", "info")
	cfg.APIRateLimitPerMinute = envInt("API_RATE_LIMIT_PER_MINUTE", 600)
	cfg.InternalWebhookSecret = os.Getenv("INTERNAL_WEBHOOK_SECRET")
	cfg.BootstrapAdminUsername = envString("BOOTSTRAP_ADMIN_USERNAME", "admin")
	cfg.BootstrapAdminPassword = os.Getenv("BOOTSTRAP_ADMIN_PASSWORD")
	cfg.BootstrapAdminForceReset = envBool("BOOTSTRAP_ADMIN_FORCE_RESET", false)
	cfg.BootstrapAdminCredentialsFile = envString("BOOTSTRAP_ADMIN_CREDENTIALS_FILE", "bootstrap_admin_credentials.txt")

	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s",
			strings.Join(missing, ", "))
	}
	return cfg, nil
}

// MustLoad calls Load and panics if the configuration is invalid. It is
// intended for use inside main() where a missing variable is a fatal error.
func MustLoad() *Config {
	cfg, err := Load()
	if err != nil {
		panic(err)
	}
	return cfg
}

// Validate performs additional semantic validation on a loaded config.
func (c *Config) Validate() error {
	if c.HTTPPort <= 0 || c.HTTPPort > 65535 {
		return errors.New("HTTP_PORT must be between 1 and 65535")
	}
	if c.GRPCPort <= 0 || c.GRPCPort > 65535 {
		return errors.New("GRPC_PORT must be between 1 and 65535")
	}
	validLevels := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !validLevels[c.LogLevel] {
		return fmt.Errorf("LOG_LEVEL must be one of debug/info/warn/error, got %q", c.LogLevel)
	}
	if c.BootstrapAdminUsername == "" {
		return errors.New("BOOTSTRAP_ADMIN_USERNAME cannot be empty")
	}
	if c.BootstrapAdminForceReset && c.BootstrapAdminPassword == "" {
		return errors.New("BOOTSTRAP_ADMIN_PASSWORD is required when BOOTSTRAP_ADMIN_FORCE_RESET=true")
	}
	if c.BootstrapAdminCredentialsFile == "" {
		return errors.New("BOOTSTRAP_ADMIN_CREDENTIALS_FILE cannot be empty")
	}
	if c.AgentTokenTTL <= 0 {
		return errors.New("AGENT_TOKEN_TTL must be a positive duration")
	}
	return nil
}

// ── helpers ──────────────────────────────────────────────────────────────────

func envString(key, defaultVal string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return defaultVal
}

func envInt(key string, defaultVal int) int {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		return defaultVal
	}
	return n
}

func envBool(key string, defaultVal bool) bool {
	v := strings.ToLower(os.Getenv(key))
	if v == "" {
		return defaultVal
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return defaultVal
	}
	return b
}

func envDuration(key string, defaultVal time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return defaultVal
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		return defaultVal
	}
	return d
}
