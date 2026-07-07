package config

import (
	"os"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// setEnv sets a batch of environment variables and returns a cleanup function
// that restores them to their original values.
func setEnv(t *testing.T, pairs map[string]string) {
	t.Helper()
	for k, v := range pairs {
		prev, existed := os.LookupEnv(k)
		if existed {
			t.Cleanup(func() { os.Setenv(k, prev) })
		} else {
			t.Cleanup(func() { os.Unsetenv(k) })
		}
		os.Setenv(k, v)
	}
}

// validEnv returns the minimum set of environment variables required for a
// successful Load().
func validEnv() map[string]string {
	return map[string]string{
		"DATABASE_URL":     "postgres://user:pass@localhost:5432/fileagent?sslmode=disable",
		"REDIS_URL":        "redis://localhost:6379/0",
		"JWT_SECRET":       "supersecret",
		"MINIO_ENDPOINT":   "localhost:9000",
		"MINIO_ACCESS_KEY": "minioadmin",
		"MINIO_SECRET_KEY": "minioadmin",
		"NATS_URL":         "nats://localhost:4222",
	}
}

func TestLoad_AllRequired_HappyPath(t *testing.T) {
	setEnv(t, validEnv())
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "postgres://user:pass@localhost:5432/fileagent?sslmode=disable", cfg.DatabaseURL)
	assert.Equal(t, "redis://localhost:6379/0", cfg.RedisURL)
	assert.Equal(t, "supersecret", cfg.JWTSecret)
	assert.Equal(t, "localhost:9000", cfg.MinIOEndpoint)
	assert.Equal(t, "nats://localhost:4222", cfg.NATSURL)
}

func TestLoad_Defaults(t *testing.T) {
	setEnv(t, validEnv())
	cfg, err := Load()
	require.NoError(t, err)

	// port defaults
	assert.Equal(t, 8080, cfg.HTTPPort)
	assert.Equal(t, 9090, cfg.GRPCPort)

	// JWT TTL defaults
	assert.Equal(t, 2*time.Hour, cfg.JWTAccessTokenTTL)
	assert.Equal(t, 720*time.Hour, cfg.JWTRefreshTokenTTL)
	// Agent token must default to a long lifetime so reconnects past the 2h
	// access TTL do not permanently lock the Agent out.
	assert.Equal(t, 720*time.Hour, cfg.AgentTokenTTL)

	// misc defaults
	assert.Equal(t, "info", cfg.LogLevel)
	assert.False(t, cfg.MinIOUseSSL)
	assert.Equal(t, "arn:aws:iam:::role/agent-role", cfg.MinIORoleARN)
	assert.Equal(t, "admin", cfg.BootstrapAdminUsername)
	assert.Equal(t, "", cfg.BootstrapAdminPassword)
	assert.False(t, cfg.BootstrapAdminForceReset)
	assert.Equal(t, "bootstrap_admin_credentials.txt", cfg.BootstrapAdminCredentialsFile)
}

func TestLoad_CustomPorts(t *testing.T) {
	env := validEnv()
	env["HTTP_PORT"] = "9999"
	env["GRPC_PORT"] = "50051"
	setEnv(t, env)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 9999, cfg.HTTPPort)
	assert.Equal(t, 50051, cfg.GRPCPort)
}

func TestLoad_CustomDurations(t *testing.T) {
	env := validEnv()
	env["JWT_ACCESS_TOKEN_TTL"] = "30m"
	env["JWT_REFRESH_TOKEN_TTL"] = "168h"
	setEnv(t, env)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 30*time.Minute, cfg.JWTAccessTokenTTL)
	assert.Equal(t, 168*time.Hour, cfg.JWTRefreshTokenTTL)
}

func TestLoad_CustomAgentTokenTTL(t *testing.T) {
	env := validEnv()
	env["AGENT_TOKEN_TTL"] = "168h"
	setEnv(t, env)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 168*time.Hour, cfg.AgentTokenTTL)
}

func TestValidate_RejectsNonPositiveAgentTokenTTL(t *testing.T) {
	setEnv(t, validEnv())
	cfg, err := Load()
	require.NoError(t, err)
	cfg.AgentTokenTTL = 0
	assert.Error(t, cfg.Validate())
}

func TestLoad_MissingRequired_SingleVariable(t *testing.T) {
	requiredVars := []string{
		"DATABASE_URL",
		"REDIS_URL",
		"JWT_SECRET",
		"MINIO_ENDPOINT",
		"MINIO_ACCESS_KEY",
		"MINIO_SECRET_KEY",
		"NATS_URL",
	}

	for _, key := range requiredVars {
		t.Run("missing_"+key, func(t *testing.T) {
			env := validEnv()
			delete(env, key)
			// Also unset the actual variable in case it's set in the real environment.
			setEnv(t, env)
			os.Unsetenv(key)

			_, err := Load()
			require.Error(t, err)
			assert.Contains(t, err.Error(), key)
		})
	}
}

func TestLoad_MissingRequired_Multiple(t *testing.T) {
	// Clear all required variables.
	required := []string{
		"DATABASE_URL", "REDIS_URL", "JWT_SECRET",
		"MINIO_ENDPOINT", "MINIO_ACCESS_KEY", "MINIO_SECRET_KEY", "NATS_URL",
	}
	for _, k := range required {
		setEnv(t, map[string]string{k: ""})
		os.Unsetenv(k)
	}

	_, err := Load()
	require.Error(t, err)
	// All missing vars should appear in the error.
	for _, k := range required {
		assert.Contains(t, err.Error(), k, "expected %s in error message", k)
	}
}

func TestLoad_InvalidPortFallsToDefault(t *testing.T) {
	env := validEnv()
	env["HTTP_PORT"] = "not-a-number"
	setEnv(t, env)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 8080, cfg.HTTPPort)
}

func TestLoad_MinIOSSL(t *testing.T) {
	env := validEnv()
	env["MINIO_USE_SSL"] = "true"
	setEnv(t, env)

	cfg, err := Load()
	require.NoError(t, err)
	assert.True(t, cfg.MinIOUseSSL)
}

func TestLoad_MinIOPublicEndpoint_DefaultsToInternal(t *testing.T) {
	env := validEnv()
	env["MINIO_USE_SSL"] = "true"
	// Explicitly clear the public vars so a host-set value cannot leak in;
	// empty is treated as unset and must fall back to the internal values (D-024).
	env["MINIO_PUBLIC_ENDPOINT"] = ""
	env["MINIO_PUBLIC_USE_SSL"] = ""
	setEnv(t, env)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, cfg.MinIOEndpoint, cfg.MinIOPublicEndpoint)
	assert.Equal(t, cfg.MinIOUseSSL, cfg.MinIOPublicUseSSL)
}

func TestLoad_MinIOPublicEndpoint_OverrideWins(t *testing.T) {
	env := validEnv()
	env["MINIO_USE_SSL"] = "false"
	env["MINIO_PUBLIC_ENDPOINT"] = "minio.public.example:443"
	env["MINIO_PUBLIC_USE_SSL"] = "true"
	setEnv(t, env)

	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, "localhost:9000", cfg.MinIOEndpoint, "internal endpoint unchanged")
	assert.False(t, cfg.MinIOUseSSL, "internal TLS flag unchanged")
	assert.Equal(t, "minio.public.example:443", cfg.MinIOPublicEndpoint)
	assert.True(t, cfg.MinIOPublicUseSSL)
}

func TestValidate_InvalidLogLevel(t *testing.T) {
	setEnv(t, validEnv())
	setEnv(t, map[string]string{"LOG_LEVEL": "verbose"})

	cfg, err := Load()
	require.NoError(t, err)
	assert.Error(t, cfg.Validate())
}

func TestValidate_ValidConfig(t *testing.T) {
	setEnv(t, validEnv())
	cfg, err := Load()
	require.NoError(t, err)
	assert.NoError(t, cfg.Validate())
}

func TestValidate_BootstrapForceResetRequiresPassword(t *testing.T) {
	setEnv(t, validEnv())
	t.Setenv("BOOTSTRAP_ADMIN_FORCE_RESET", "true")
	t.Setenv("BOOTSTRAP_ADMIN_PASSWORD", "")

	cfg, err := Load()
	require.NoError(t, err)
	assert.Error(t, cfg.Validate())
}

func TestValidate_InvalidPort(t *testing.T) {
	setEnv(t, validEnv())
	setEnv(t, map[string]string{"HTTP_PORT": "0"})

	cfg, err := Load()
	require.NoError(t, err)
	assert.Error(t, cfg.Validate())
}
