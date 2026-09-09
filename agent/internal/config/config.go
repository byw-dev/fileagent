// Package config loads and validates the Edge Agent configuration from a TOML
// file, with environment variable overrides applied on top.
package config

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/BurntSushi/toml"
)

// Config is the top-level configuration for the Edge Agent.
type Config struct {
	Server  ServerConfig  `toml:"server"`
	Agent   AgentConfig   `toml:"agent"`
	Upload  UploadConfig  `toml:"upload"`
	Metrics MetricsConfig `toml:"metrics"`
	Log     LogConfig     `toml:"log"`
}

// ServerConfig holds Control Plane connection settings.
type ServerConfig struct {
	// Endpoint is the gRPC Control Plane address (required), e.g. "cp.internal:9090".
	Endpoint string `toml:"endpoint"`
	// TLSCACert is an optional path to a PEM-encoded CA certificate for TLS verification.
	TLSCACert string `toml:"tls_ca_cert"`
	// TLSInsecure disables TLS on the gRPC connection. Leave it off outside
	// local development: a plaintext connection sends the bearer token in clear.
	//
	// The switch exists because the dev Control Plane serves gRPC in plaintext
	// while the agent had no way to dial without TLS, so an agent could never
	// reach a local Control Plane at all — which is a large part of why the data
	// plane went so long without ever running end to end (IC-BUG-18).
	//
	// It is phrased negatively on purpose: the zero value must be the secure
	// one, so a Config built in code (not through Load) never silently drops TLS.
	TLSInsecure bool `toml:"tls_insecure"`
}

// AgentConfig holds agent identity and local storage settings.
type AgentConfig struct {
	// FingerprintFile is the path where the agent fingerprint (machine ID) is stored.
	FingerprintFile string `toml:"fingerprint_file"`
	// TokenFile is the path where the encrypted JWT token is persisted.
	TokenFile string `toml:"token_file"`
	// DataDir is the local directory used for SQLite queue and other persistent data.
	DataDir string `toml:"data_dir"`
}

// UploadConfig holds file upload behaviour settings.
type UploadConfig struct {
	// Concurrency is the number of concurrent upload workers (default 3).
	Concurrency int `toml:"concurrency"`
	// PartSizeMB is the multipart upload part size in mebibytes (default 64).
	PartSizeMB int `toml:"part_size_mb"`
	// QueueMaxSize is the maximum number of tasks in the SQLite queue (default 10000).
	QueueMaxSize int `toml:"queue_max_size"`
	// RetryMax is the maximum number of upload retry attempts per task (default 10).
	RetryMax int `toml:"retry_max"`
}

// MetricsConfig holds Prometheus metrics exposure settings.
type MetricsConfig struct {
	// Enabled controls whether the metrics HTTP endpoint is started (default true).
	Enabled bool `toml:"enabled"`
	// Port is the TCP port for the metrics endpoint (default 9100).
	Port int `toml:"port"`
}

// LogConfig holds structured logging settings.
type LogConfig struct {
	// Level is the minimum log level: debug, info, warn, error (default "info").
	Level string `toml:"level"`
	// Output is the log file path (default "/var/log/fileagent/agent.log").
	Output string `toml:"output"`
	// MaxSizeMB is the maximum log file size in mebibytes before rotation (default 100).
	MaxSizeMB int `toml:"max_size_mb"`
	// MaxBackups is the number of rotated log files to retain (default 7).
	MaxBackups int `toml:"max_backups"`
}

// defaults returns a Config populated with all documented default values.
func defaults() Config {
	return Config{
		Upload: UploadConfig{
			Concurrency:  3,
			PartSizeMB:   64,
			QueueMaxSize: 10000,
			RetryMax:     10,
		},
		Metrics: MetricsConfig{
			Enabled: true,
			Port:    9100,
		},
		Log: LogConfig{
			Level:      "info",
			Output:     "/var/log/fileagent/agent.log",
			MaxSizeMB:  100,
			MaxBackups: 7,
		},
	}
}

// Load reads the TOML configuration file at path, applies environment variable
// overrides, and validates the result. An empty path skips file loading and
// relies entirely on defaults + environment variables.
func Load(path string) (*Config, error) {
	cfg := defaults()

	if path != "" {
		if _, err := toml.DecodeFile(path, &cfg); err != nil {
			return nil, fmt.Errorf("config: decode file %q: %w", path, err)
		}
	}

	applyEnvOverrides(&cfg)

	if err := validate(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// applyEnvOverrides replaces any field in cfg whose corresponding environment
// variable is non-empty.
func applyEnvOverrides(cfg *Config) {
	// [server]
	if v := os.Getenv("AGENT_SERVER_ENDPOINT"); v != "" {
		cfg.Server.Endpoint = v
	}
	if v := os.Getenv("AGENT_SERVER_TLS_CA_CERT"); v != "" {
		cfg.Server.TLSCACert = v
	}
	if v := os.Getenv("AGENT_SERVER_TLS_INSECURE"); v != "" {
		// Parse leniently but fail safe: anything unparseable leaves TLS on.
		if b, err := strconv.ParseBool(strings.TrimSpace(v)); err == nil {
			cfg.Server.TLSInsecure = b
		}
	}

	// [agent]
	if v := os.Getenv("AGENT_FINGERPRINT_FILE"); v != "" {
		cfg.Agent.FingerprintFile = v
	}
	if v := os.Getenv("AGENT_TOKEN_FILE"); v != "" {
		cfg.Agent.TokenFile = v
	}
	if v := os.Getenv("AGENT_DATA_DIR"); v != "" {
		cfg.Agent.DataDir = v
	}

	// [upload]
	if v := os.Getenv("AGENT_UPLOAD_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Upload.Concurrency = n
		}
	}
	if v := os.Getenv("AGENT_UPLOAD_PART_SIZE_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Upload.PartSizeMB = n
		}
	}
	if v := os.Getenv("AGENT_UPLOAD_QUEUE_MAX_SIZE"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Upload.QueueMaxSize = n
		}
	}
	if v := os.Getenv("AGENT_UPLOAD_RETRY_MAX"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Upload.RetryMax = n
		}
	}

	// [metrics]
	if v := os.Getenv("AGENT_METRICS_ENABLED"); v != "" {
		cfg.Metrics.Enabled = strings.ToLower(v) == "true" || v == "1"
	}
	if v := os.Getenv("AGENT_METRICS_PORT"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Metrics.Port = n
		}
	}

	// [log]
	if v := os.Getenv("AGENT_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	if v := os.Getenv("AGENT_LOG_OUTPUT"); v != "" {
		cfg.Log.Output = v
	}
	if v := os.Getenv("AGENT_LOG_MAX_SIZE_MB"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Log.MaxSizeMB = n
		}
	}
	if v := os.Getenv("AGENT_LOG_MAX_BACKUPS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Log.MaxBackups = n
		}
	}
}

// validate checks that all required fields are set and values are sane.
func validate(cfg *Config) error {
	var errs []string

	if cfg.Server.Endpoint == "" {
		errs = append(errs, "server.endpoint is required")
	}
	if cfg.Upload.Concurrency <= 0 {
		errs = append(errs, "upload.concurrency must be > 0")
	}
	if cfg.Upload.PartSizeMB <= 0 {
		errs = append(errs, "upload.part_size_mb must be > 0")
	}
	if cfg.Upload.QueueMaxSize <= 0 {
		errs = append(errs, "upload.queue_max_size must be > 0")
	}
	if cfg.Upload.RetryMax < 0 {
		errs = append(errs, "upload.retry_max must be >= 0")
	}
	if cfg.Metrics.Port <= 0 || cfg.Metrics.Port > 65535 {
		errs = append(errs, "metrics.port must be between 1 and 65535")
	}

	valid := map[string]bool{"debug": true, "info": true, "warn": true, "error": true}
	if !valid[cfg.Log.Level] {
		errs = append(errs, fmt.Sprintf("log.level must be one of debug/info/warn/error, got %q", cfg.Log.Level))
	}

	if len(errs) > 0 {
		return errors.New("config: " + strings.Join(errs, "; "))
	}
	return nil
}
