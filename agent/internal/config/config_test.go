package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// writeTOML writes content to a temporary file and returns its path.
func writeTOML(t *testing.T, content string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "agent.toml")
	require.NoError(t, os.WriteFile(p, []byte(content), 0o600))
	return p
}

func TestLoad_Defaults(t *testing.T) {
	// Only server.endpoint is required; everything else should fall back to defaults.
	p := writeTOML(t, `
[server]
endpoint = "cp.internal:9090"
`)
	cfg, err := Load(p)
	require.NoError(t, err)

	assert.Equal(t, "cp.internal:9090", cfg.Server.Endpoint)
	assert.Equal(t, "", cfg.Server.TLSCACert)
	assert.Equal(t, 3, cfg.Upload.Concurrency)
	assert.Equal(t, 64, cfg.Upload.PartSizeMB)
	assert.Equal(t, 10000, cfg.Upload.QueueMaxSize)
	assert.Equal(t, 10, cfg.Upload.RetryMax)
	assert.Equal(t, 300, cfg.Upload.MinTimeoutSeconds)
	assert.True(t, cfg.Metrics.Enabled)
	assert.Equal(t, 9100, cfg.Metrics.Port)
	assert.Equal(t, "info", cfg.Log.Level)
	assert.Equal(t, "/var/log/fileagent/agent.log", cfg.Log.Output)
	assert.Equal(t, 100, cfg.Log.MaxSizeMB)
	assert.Equal(t, 7, cfg.Log.MaxBackups)
}

func TestLoad_FullConfig(t *testing.T) {
	p := writeTOML(t, `
[server]
endpoint = "cp.example.com:9090"
tls_ca_cert = "/etc/ssl/ca.pem"

[agent]
fingerprint_file = "/var/lib/fileagent/fingerprint"
token_file = "/var/lib/fileagent/token.enc"
data_dir = "/var/lib/fileagent"

[upload]
concurrency = 5
part_size_mb = 128
queue_max_size = 5000
retry_max = 3
min_timeout_seconds = 45

[metrics]
enabled = false
port = 9200

[log]
level = "debug"
output = "/var/log/fileagent/agent.log"
max_size_mb = 50
max_backups = 3
`)
	cfg, err := Load(p)
	require.NoError(t, err)

	assert.Equal(t, "cp.example.com:9090", cfg.Server.Endpoint)
	assert.Equal(t, "/etc/ssl/ca.pem", cfg.Server.TLSCACert)
	assert.Equal(t, "/var/lib/fileagent/fingerprint", cfg.Agent.FingerprintFile)
	assert.Equal(t, "/var/lib/fileagent/token.enc", cfg.Agent.TokenFile)
	assert.Equal(t, "/var/lib/fileagent", cfg.Agent.DataDir)
	assert.Equal(t, 5, cfg.Upload.Concurrency)
	assert.Equal(t, 128, cfg.Upload.PartSizeMB)
	assert.Equal(t, 5000, cfg.Upload.QueueMaxSize)
	assert.Equal(t, 3, cfg.Upload.RetryMax)
	assert.Equal(t, 45, cfg.Upload.MinTimeoutSeconds)
	assert.False(t, cfg.Metrics.Enabled)
	assert.Equal(t, 9200, cfg.Metrics.Port)
	assert.Equal(t, "debug", cfg.Log.Level)
	assert.Equal(t, 50, cfg.Log.MaxSizeMB)
	assert.Equal(t, 3, cfg.Log.MaxBackups)
}

func TestLoad_MissingRequired(t *testing.T) {
	// No server.endpoint → validation error.
	p := writeTOML(t, `
[upload]
concurrency = 3
`)
	_, err := Load(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.endpoint is required")
}

func TestLoad_EnvOverride(t *testing.T) {
	p := writeTOML(t, `
[server]
endpoint = "original:9090"

[upload]
concurrency = 2
`)
	t.Setenv("AGENT_SERVER_ENDPOINT", "override:9090")
	t.Setenv("AGENT_UPLOAD_CONCURRENCY", "8")
	t.Setenv("AGENT_LOG_LEVEL", "warn")
	t.Setenv("AGENT_METRICS_PORT", "9300")

	cfg, err := Load(p)
	require.NoError(t, err)

	assert.Equal(t, "override:9090", cfg.Server.Endpoint)
	assert.Equal(t, 8, cfg.Upload.Concurrency)
	assert.Equal(t, "warn", cfg.Log.Level)
	assert.Equal(t, 9300, cfg.Metrics.Port)
}

func TestLoad_EnvOnlyNoFile(t *testing.T) {
	t.Setenv("AGENT_SERVER_ENDPOINT", "env-only:9090")
	cfg, err := Load("")
	require.NoError(t, err)
	assert.Equal(t, "env-only:9090", cfg.Server.Endpoint)
	// Defaults still apply.
	assert.Equal(t, 3, cfg.Upload.Concurrency)
}

func TestLoad_BadFile(t *testing.T) {
	_, err := Load("/nonexistent/path/agent.toml")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "decode file")
}

func TestLoad_InvalidLogLevel(t *testing.T) {
	p := writeTOML(t, `
[server]
endpoint = "cp:9090"

[log]
level = "verbose"
`)
	_, err := Load(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "log.level")
}

func TestApplyEnvOverrides_AllVars(t *testing.T) {
	t.Setenv("AGENT_SERVER_TLS_CA_CERT", "/certs/ca.pem")
	t.Setenv("AGENT_FINGERPRINT_FILE", "/data/fp.txt")
	t.Setenv("AGENT_TOKEN_FILE", "/data/tok.enc")
	t.Setenv("AGENT_DATA_DIR", "/data/agent")
	t.Setenv("AGENT_UPLOAD_PART_SIZE_MB", "128")
	t.Setenv("AGENT_UPLOAD_QUEUE_MAX_SIZE", "5000")
	t.Setenv("AGENT_UPLOAD_RETRY_MAX", "5")
	t.Setenv("AGENT_UPLOAD_MIN_TIMEOUT_SECONDS", "90")
	t.Setenv("AGENT_METRICS_ENABLED", "true")
	t.Setenv("AGENT_LOG_OUTPUT", "/var/log/agent.log")
	t.Setenv("AGENT_LOG_MAX_SIZE_MB", "200")
	t.Setenv("AGENT_LOG_MAX_BACKUPS", "14")
	t.Setenv("AGENT_SERVER_ENDPOINT", "ep:9090")

	cfg, err := Load("")
	require.NoError(t, err)

	assert.Equal(t, "/certs/ca.pem", cfg.Server.TLSCACert)
	assert.Equal(t, "/data/fp.txt", cfg.Agent.FingerprintFile)
	assert.Equal(t, "/data/tok.enc", cfg.Agent.TokenFile)
	assert.Equal(t, "/data/agent", cfg.Agent.DataDir)
	assert.Equal(t, 128, cfg.Upload.PartSizeMB)
	assert.Equal(t, 5000, cfg.Upload.QueueMaxSize)
	assert.Equal(t, 5, cfg.Upload.RetryMax)
	assert.Equal(t, 90, cfg.Upload.MinTimeoutSeconds)
	assert.True(t, cfg.Metrics.Enabled)
	assert.Equal(t, "/var/log/agent.log", cfg.Log.Output)
	assert.Equal(t, 200, cfg.Log.MaxSizeMB)
	assert.Equal(t, 14, cfg.Log.MaxBackups)
}

func TestApplyEnvOverrides_MetricsDisabled(t *testing.T) {
	t.Setenv("AGENT_SERVER_ENDPOINT", "ep:9090")
	t.Setenv("AGENT_METRICS_ENABLED", "false")

	cfg, err := Load("")
	require.NoError(t, err)
	assert.False(t, cfg.Metrics.Enabled)
}

func TestApplyEnvOverrides_InvalidNumbers(t *testing.T) {
	t.Setenv("AGENT_SERVER_ENDPOINT", "ep:9090")
	// Non-numeric values should be silently ignored (env override skipped).
	t.Setenv("AGENT_UPLOAD_CONCURRENCY", "not-a-number")
	t.Setenv("AGENT_UPLOAD_PART_SIZE_MB", "bad")
	t.Setenv("AGENT_UPLOAD_QUEUE_MAX_SIZE", "bad")
	t.Setenv("AGENT_UPLOAD_RETRY_MAX", "bad")
	t.Setenv("AGENT_UPLOAD_MIN_TIMEOUT_SECONDS", "bad")
	t.Setenv("AGENT_METRICS_PORT", "bad")
	t.Setenv("AGENT_LOG_MAX_SIZE_MB", "bad")
	t.Setenv("AGENT_LOG_MAX_BACKUPS", "bad")

	cfg, err := Load("")
	require.NoError(t, err)
	// Defaults are used because env vars were invalid.
	assert.Equal(t, 3, cfg.Upload.Concurrency)
}

// PR #100 review R3: the assumed upload rate must be configurable, not a
// hardwired const — slow links make any large file permanently fail to upload.
func TestLoad_Defaults_AssumedUploadBytesPerSecond(t *testing.T) {
	p := writeTOML(t, `
[server]
endpoint = "cp.internal:9090"
`)
	cfg, err := Load(p)
	require.NoError(t, err)
	assert.Equal(t, int64(1024*1024), cfg.Upload.AssumedUploadBytesPerSecond,
		"default assumed upload rate must stay at 1 MiB/s")
}

func TestLoad_AssumedUploadBytesPerSecondOverride(t *testing.T) {
	p := writeTOML(t, `
[server]
endpoint = "cp.internal:9090"

[upload]
assumed_upload_bytes_per_second = 262144
`)
	cfg, err := Load(p)
	require.NoError(t, err)
	assert.Equal(t, int64(262144), cfg.Upload.AssumedUploadBytesPerSecond)
}

func TestValidate_AssumedUploadBytesPerSecondMustBePositive(t *testing.T) {
	p := writeTOML(t, `
[server]
endpoint = "cp.internal:9090"

[upload]
assumed_upload_bytes_per_second = 0
`)
	_, err := Load(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upload.assumed_upload_bytes_per_second must be > 0")
}

func TestValidate_MultipleErrors(t *testing.T) {
	p := writeTOML(t, `
[server]
endpoint = ""

[upload]
concurrency = -1
part_size_mb = -1
queue_max_size = -1
retry_max = -1
min_timeout_seconds = -1

[metrics]
port = 99999

[log]
level = "bad"
`)
	_, err := Load(p)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "server.endpoint is required")
	assert.Contains(t, err.Error(), "upload.min_timeout_seconds must be > 0")
}
