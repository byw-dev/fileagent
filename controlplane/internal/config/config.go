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

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
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
	//
	// MinIOEndpoint is the INTERNAL endpoint the Control Plane itself dials for
	// CP↔MinIO calls (STS AssumeRole + bucket-create admin). On a docker network
	// this is the in-cluster service name, e.g. "minio:9000".
	MinIOEndpoint  string // required, e.g. "minio:9000"
	MinIOAccessKey string // required
	MinIOSecretKey string // required
	MinIOUseSSL    bool   // default false
	// MinIOPublicEndpoint is the client-facing endpoint handed to agents in the
	// STS CredentialsPayload and used as the host that presigned URLs are signed
	// for. It must be reachable by browsers/agents (unlike the internal endpoint).
	// Defaults to MinIOEndpoint when MINIO_PUBLIC_ENDPOINT is unset (backward
	// compatible with single-endpoint deployments). See DECISIONS.md D-024.
	MinIOPublicEndpoint string
	MinIOPublicUseSSL   bool // defaults to MinIOUseSSL
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

	// WebhookMaxParseBytes is the IC-4a request-body parse cap for
	// /internal/minio-event (round-3 B-2-3): bodies above it are oversized
	// (dead letter + 5xx, never a silent 200). Default 8MiB; must stay at or
	// above the 64KiB dead-letter capture floor — a near-zero cap turns every
	// normal notification into a permanent 5xx (self-inflicted config), so
	// config.Validate rejects sub-floor values.
	WebhookMaxParseBytes int64
	// WebhookFailLimit is the IC-4a poison-pill retry cap (IC-BUG-6): after
	// this many failed deliveries of ONE webhook event (identified by
	// bucket+key+sequencer), the event is dead-lettered into
	// webhook_dead_letters and answered 200 so MinIO's head-of-line blocking
	// queue is freed. Default 600: the webhook queue retries ~every 3s, so 600
	// attempts ≈ 30 minutes of tolerated outage — long enough to ride out a PG
	// restart/failover (the acceptance requires surviving a PG outage), short
	// enough that a true poison pill cannot stall the feed indefinitely.
	WebhookFailLimit int64

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

	// StorageEventSource selects which adapter feeds the file index (D-037
	// rule 2 / PR3b D6): "minio_webhook" (default, the transitional MinIO
	// S3-event webhook, unchanged behavior) or "seaweedfs_filer" (the
	// SeaweedFS filer metadata subscription). Only seaweedfs_filer starts the
	// subscription worker; the webhook route stays registered either way
	// (registering it is harmless and keeps the two paths symmetrical).
	StorageEventSource string

	// SeaweedfsFilerGRPCAddr is the filer gRPC address (host:port) the
	// subscription dials. REQUIRED when StorageEventSource is
	// seaweedfs_filer (Validate rejects the combination without it).
	SeaweedfsFilerGRPCAddr string

	// SeaweedfsFilerInsecure is the explicit plaintext opt-in. Filer gRPC has
	// NO token auth — only mTLS (D-037 known constraint) — and mTLS lands in
	// B7; until then plaintext must be enabled deliberately per deployment,
	// never by a silent default. Validate refuses seaweedfs_filer without
	// SEAWEEDFS_FILER_INSECURE=true.
	SeaweedfsFilerInsecure bool

	// SeaweedfsResumeMargin is M, the rewind applied on every (re)subscribe to
	// ride out filer clock rollback across restarts (D5; default 60s). The
	// safety assumption: total clock rollback < M (filer host runs NTP slew).
	SeaweedfsResumeMargin time.Duration

	// SeaweedfsClientName/SeaweedfsClientID identify the subscriber to the
	// filer. Both must be STABLE across CP restarts (the filer re-attaches a
	// returning client to its backlog), hence the fixed defaults and the
	// expectation that overrides never change across restarts.
	SeaweedfsClientName string
	SeaweedfsClientID   int32

	// SeaweedfsApplyMaxRetries caps the in-place retries of one change's
	// read-back+Apply before the subscription stream is abandoned and
	// resubscribed from ResumeFrom(cursor, M). The change is never skipped.
	SeaweedfsApplyMaxRetries int
	// SeaweedfsApplyRetryBackoff is the wait between those in-place retries.
	SeaweedfsApplyRetryBackoff time.Duration

	// SeaweedfsSourceIDOverride optionally replaces the derived cursor/mark
	// identity ("seaweedfs:<addr>") in storage_event_cursors/marks. Keep it
	// stable: changing it orphans the old cursor (a full replay follows).
	SeaweedfsSourceIDOverride string
}

// Storage-event-source selector values (D6).
const (
	// StorageEventSourceMinioWebhook keeps the transitional MinIO webhook as
	// the only storage event source (the historical default).
	StorageEventSourceMinioWebhook = "minio_webhook"
	// StorageEventSourceSeaweedFSFiler enables the SeaweedFS filer
	// metadata-subscription adapter (D-037 target backend).
	StorageEventSourceSeaweedFSFiler = "seaweedfs_filer"
)

// SeaweedfsSourceID returns the storage_event_cursors/marks identity for this
// deployment: the SEAWEEDFS_SOURCE_ID override when set, else
// "seaweedfs:<filer addr>".
func (c *Config) SeaweedfsSourceID() string {
	if c.SeaweedfsSourceIDOverride != "" {
		return c.SeaweedfsSourceIDOverride
	}
	return "seaweedfs:" + c.SeaweedfsFilerGRPCAddr
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
	// Public (client-facing) endpoint falls back to the internal values so
	// single-endpoint deployments keep working unchanged (D-024). Read these
	// after MinIOEndpoint/MinIOUseSSL so the fallback observes them.
	cfg.MinIOPublicEndpoint = envString("MINIO_PUBLIC_ENDPOINT", cfg.MinIOEndpoint)
	cfg.MinIOPublicUseSSL = envBool("MINIO_PUBLIC_USE_SSL", cfg.MinIOUseSSL)
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
	cfg.WebhookFailLimit = int64(envInt("WEBHOOK_FAIL_LIMIT", 600))
	// Default 8MiB (see handler.maxWebhookParseBytes for the sizing rationale).
	cfg.WebhookMaxParseBytes = int64(envInt("WEBHOOK_MAX_PARSE_BYTES", int(handler.MaxWebhookParseBytesForTest())))
	cfg.BootstrapAdminUsername = envString("BOOTSTRAP_ADMIN_USERNAME", "admin")
	cfg.BootstrapAdminPassword = os.Getenv("BOOTSTRAP_ADMIN_PASSWORD")
	cfg.BootstrapAdminForceReset = envBool("BOOTSTRAP_ADMIN_FORCE_RESET", false)
	cfg.BootstrapAdminCredentialsFile = envString("BOOTSTRAP_ADMIN_CREDENTIALS_FILE", "bootstrap_admin_credentials.txt")

	// ── storage event source (PR3b D6) ──────────────────────────────────────
	// The raw value is loaded here and JUDGED in Validate: an unknown value
	// must fail startup loudly, not silently fall back to the webhook.
	cfg.StorageEventSource = envString("STORAGE_EVENT_SOURCE", StorageEventSourceMinioWebhook)
	cfg.SeaweedfsFilerGRPCAddr = os.Getenv("SEAWEEDFS_FILER_GRPC_ADDR")
	cfg.SeaweedfsFilerInsecure = envBool("SEAWEEDFS_FILER_INSECURE", false)
	cfg.SeaweedfsResumeMargin = envDuration("SEAWEEDFS_RESUME_MARGIN", 60*time.Second)
	cfg.SeaweedfsClientName = envString("SEAWEEDFS_CLIENT_NAME", "fileagent-controlplane")
	// Stable numeric identity; env override for deployments that need to run
	// several CP instances against one filer.
	cfg.SeaweedfsClientID = int32(envInt("SEAWEEDFS_CLIENT_ID", 1))
	cfg.SeaweedfsApplyMaxRetries = envInt("SEAWEEDFS_APPLY_MAX_RETRIES", 8)
	cfg.SeaweedfsApplyRetryBackoff = envDuration("SEAWEEDFS_APPLY_RETRY_BACKOFF", time.Second)
	cfg.SeaweedfsSourceIDOverride = os.Getenv("SEAWEEDFS_SOURCE_ID")

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
	// S1 (PR #110 review): below-floor WEBHOOK_FAIL_LIMIT is a startup failure,
	// not a silent fallback — limit=1 tolerates ~3s of outage, which would
	// dead-letter ordinary DB blips and defeat the retry mechanism entirely.
	if c.WebhookFailLimit < handler.MinWebhookFailLimit {
		return fmt.Errorf("WEBHOOK_FAIL_LIMIT=%d is below the safety floor %d — a few-second blip would dead-letter events; raise it to exceed (expected outage seconds ÷ 3)",
			c.WebhookFailLimit, handler.MinWebhookFailLimit)
	}
	if c.WebhookMaxParseBytes < handler.MinWebhookParseCap {
		return fmt.Errorf("WEBHOOK_MAX_PARSE_BYTES=%d is below the safety floor %d — a near-zero parse cap turns every normal notification into a permanent 5xx",
			c.WebhookMaxParseBytes, handler.MinWebhookParseCap)
	}
	// ── storage event source (PR3b D6) ──────────────────────────────────────
	switch c.StorageEventSource {
	case StorageEventSourceMinioWebhook:
		// nothing more to check; the SeaweedFS variables are irrelevant here
	case StorageEventSourceSeaweedFSFiler:
		if c.SeaweedfsFilerGRPCAddr == "" {
			return errors.New("STORAGE_EVENT_SOURCE=seaweedfs_filer requires SEAWEEDFS_FILER_GRPC_ADDR (the filer gRPC address to subscribe to)")
		}
		// Fail-closed (PR3b D6): filer gRPC has no token auth — only mTLS
		// (D-037 known constraint) — and mTLS lands in B7. Until then,
		// plaintext is an explicit per-deployment opt-in; a deployment that
		// forgets the variable must fail at startup rather than dial an
		// unauthenticated, all-powerful filer port in the clear.
		if !c.SeaweedfsFilerInsecure {
			return errors.New("STORAGE_EVENT_SOURCE=seaweedfs_filer requires SEAWEEDFS_FILER_INSECURE=true: this PR supports plaintext only (mTLS arrives in B7) and plaintext must be opted into explicitly — refusing to start rather than dial the filer unauthenticated")
		}
		if c.SeaweedfsResumeMargin <= 0 {
			return errors.New("SEAWEEDFS_RESUME_MARGIN must be positive (it rewinds every resubscribe to cover filer clock rollback)")
		}
		if c.SeaweedfsApplyMaxRetries < 0 {
			return errors.New("SEAWEEDFS_APPLY_MAX_RETRIES must not be negative")
		}
		if c.SeaweedfsApplyRetryBackoff < 0 {
			return errors.New("SEAWEEDFS_APPLY_RETRY_BACKOFF must not be negative")
		}
	default:
		return fmt.Errorf("STORAGE_EVENT_SOURCE must be %q or %q, got %q",
			StorageEventSourceMinioWebhook, StorageEventSourceSeaweedFSFiler, c.StorageEventSource)
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
