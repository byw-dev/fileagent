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
