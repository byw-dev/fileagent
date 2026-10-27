package config

// storage_event_config_test.go — B3-PR3b D6: the storage-event-source
// selection knob and the SeaweedFS filer subscription settings. Fail-closed
// rules under test:
//
//   - STORAGE_EVENT_SOURCE accepts only minio_webhook (default) and
//     seaweedfs_filer;
//   - seaweedfs_filer REQUIRES SEAWEEDFS_FILER_GRPC_ADDR;
//   - seaweedfs_filer REQUIRES an explicit SEAWEEDFS_FILER_INSECURE=true:
//     filer gRPC has no token auth, only mTLS (D-037 known constraint), and
//     mTLS lands in B7 — so plaintext must be an explicit opt-in, never a
//     silent default (a production deployment that forgets the variable must
//     fail at startup, not dial the filer in the clear).

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func seaweedfsEnv(extra map[string]string) map[string]string {
	env := validEnv()
	for k, v := range extra {
		env[k] = v
	}
	return env
}

func TestLoad_StorageEventSource_DefaultIsMinioWebhook(t *testing.T) {
	setEnv(t, validEnv())
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, StorageEventSourceMinioWebhook, cfg.StorageEventSource)
	assert.NoError(t, cfg.Validate(), "the default must validate without any SEAWEEDFS_* variables")
}

func TestLoad_StorageEventSource_FullSeaweedfsSetup(t *testing.T) {
	setEnv(t, seaweedfsEnv(map[string]string{
		"STORAGE_EVENT_SOURCE":     "seaweedfs_filer",
		"SEAWEEDFS_FILER_GRPC_ADDR": "localhost:25889",
		"SEAWEEDFS_FILER_INSECURE":  "true",
		"SEAWEEDFS_RESUME_MARGIN":   "90s",
		"SEAWEEDFS_CLIENT_NAME":     "fileagent-cp-custom",
		"SEAWEEDFS_CLIENT_ID":       "42",
		"SEAWEEDFS_APPLY_MAX_RETRIES": "3",
		"SEAWEEDFS_APPLY_RETRY_BACKOFF": "2s",
		"SEAWEEDFS_SOURCE_ID":       "seaweedfs:custom-id",
	}))
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, StorageEventSourceSeaweedFSFiler, cfg.StorageEventSource)
	assert.Equal(t, "localhost:25889", cfg.SeaweedfsFilerGRPCAddr)
	assert.True(t, cfg.SeaweedfsFilerInsecure)
	assert.Equal(t, 90*time.Second, cfg.SeaweedfsResumeMargin)
	assert.Equal(t, "fileagent-cp-custom", cfg.SeaweedfsClientName)
	assert.Equal(t, int32(42), cfg.SeaweedfsClientID)
	assert.Equal(t, 3, cfg.SeaweedfsApplyMaxRetries)
	assert.Equal(t, 2*time.Second, cfg.SeaweedfsApplyRetryBackoff)
	assert.Equal(t, "seaweedfs:custom-id", cfg.SeaweedfsSourceID())
	require.NoError(t, cfg.Validate())
}

func TestLoad_SeaweedfsDefaults(t *testing.T) {
	setEnv(t, seaweedfsEnv(map[string]string{
		"STORAGE_EVENT_SOURCE":      "seaweedfs_filer",
		"SEAWEEDFS_FILER_GRPC_ADDR": "filer:18888",
		"SEAWEEDFS_FILER_INSECURE":  "true",
	}))
	cfg, err := Load()
	require.NoError(t, err)
	assert.Equal(t, 60*time.Second, cfg.SeaweedfsResumeMargin, "D6: resume margin defaults to 60s")
	assert.Equal(t, "fileagent-controlplane", cfg.SeaweedfsClientName, "client name must have a STABLE default")
	assert.Equal(t, int32(1), cfg.SeaweedfsClientID, "client id must have a STABLE default")
	assert.Equal(t, "seaweedfs:filer:18888", cfg.SeaweedfsSourceID(), "source id derives from the filer address unless overridden")
}

func TestValidate_StorageEventSource_UnknownValueRejected(t *testing.T) {
	setEnv(t, seaweedfsEnv(map[string]string{"STORAGE_EVENT_SOURCE": "kafka"}))
	cfg, err := Load()
	require.NoError(t, err, "Load accepts the raw string; Validate judges it")
	require.Error(t, cfg.Validate())
	assert.Contains(t, cfg.Validate().Error(), "STORAGE_EVENT_SOURCE")
}

func TestValidate_SeaweedfsFiler_MissingAddrFailsStartup(t *testing.T) {
	setEnv(t, seaweedfsEnv(map[string]string{
		"STORAGE_EVENT_SOURCE":     "seaweedfs_filer",
		"SEAWEEDFS_FILER_INSECURE": "true",
	}))
	cfg, err := Load()
	require.NoError(t, err)
	err = cfg.Validate()
	require.Error(t, err, "seaweedfs_filer without a filer address must fail at startup")
	assert.True(t, strings.Contains(err.Error(), "SEAWEEDFS_FILER_GRPC_ADDR"), "error must name the missing variable: %v", err)
}

// Fail-closed: forgetting SEAWEEDFS_FILER_INSECURE must REFUSE to start — the
// alternative is silently dialing an unauthenticated, all-powerful filer port
// in production. mTLS arrives in B7; until then the error says so.
func TestValidate_SeaweedfsFiler_PlaintextRequiresExplicitInsecure(t *testing.T) {
	setEnv(t, seaweedfsEnv(map[string]string{
		"STORAGE_EVENT_SOURCE":      "seaweedfs_filer",
		"SEAWEEDFS_FILER_GRPC_ADDR": "filer:18888",
	}))
	cfg, err := Load()
	require.NoError(t, err)
	err = cfg.Validate()
	require.Error(t, err, "plaintext filer gRPC without the explicit opt-in must fail at startup")
	assert.Contains(t, err.Error(), "SEAWEEDFS_FILER_INSECURE")
	assert.Contains(t, err.Error(), "mTLS", "the error must explain WHY (mTLS is the B7 milestone)")

	// INSECURE explicitly false is the same refusal — only true is an opt-in.
	setEnv(t, seaweedfsEnv(map[string]string{
		"STORAGE_EVENT_SOURCE":      "seaweedfs_filer",
		"SEAWEEDFS_FILER_GRPC_ADDR": "filer:18888",
		"SEAWEEDFS_FILER_INSECURE":  "false",
	}))
	cfg, err = Load()
	require.NoError(t, err)
	require.Error(t, cfg.Validate())
}
