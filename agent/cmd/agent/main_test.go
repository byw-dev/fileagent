package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/credential"
	"github.com/byw-dev/fileagent/agent/internal/executor"
	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/byw-dev/fileagent/agent/internal/scheduler"
	uploadpkg "github.com/byw-dev/fileagent/agent/internal/uploader"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/pkg/trollsift"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

type mockTokenSetter struct {
	token string
}

func (m *mockTokenSetter) SetToken(token string) { m.token = token }

func TestHandleRevokeCommand_ClearsCredentialsAndStops(t *testing.T) {
	tokenPath := filepath.Join(t.TempDir(), "token.enc")
	tokenMgr := credential.NewTokenManager(tokenPath, "machine-id")
	require.NoError(t, tokenMgr.Save("jwt-token"))

	stsMgr := credential.NewSTSManager()
	stsMgr.SetSTS(&credential.STSCredentials{Expiry: time.Now().Add(time.Hour)})

	client := &mockTokenSetter{token: "jwt-token"}
	stopped := false
	stop := func() { stopped = true }

	handleRevokeCommand(tokenMgr, stsMgr, client, stop, zap.NewNop(), "manual revoke")

	assert.True(t, stopped)
	assert.Equal(t, "", tokenMgr.Token())
	assert.Nil(t, stsMgr.GetSTS())
	assert.Equal(t, "", client.token)
}

// ── buildLogger ───────────────────────────────────────────────────────────────

func TestBuildLogger_ProductionLevel(t *testing.T) {
	l, err := buildLogger("info")
	require.NoError(t, err)
	assert.NotNil(t, l)
}

func TestBuildLogger_DebugLevel(t *testing.T) {
	l, err := buildLogger("debug")
	require.NoError(t, err)
	assert.NotNil(t, l)
}

func TestBuildLogger_InvalidLevelReturnsError(t *testing.T) {
	_, err := buildLogger("bogus")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid log level")
}

// ── protoToSchedulerRule ──────────────────────────────────────────────────────

func TestProtoToSchedulerRule_NilReturnsEmpty(t *testing.T) {
	r := protoToSchedulerRule(nil)
	assert.Equal(t, scheduler.CollectionRule{}, r)
}

func TestProtoToSchedulerRule_MapsAllFields(t *testing.T) {
	proto := &agentv1.CollectionRule{
		RuleId:           "rule-1",
		Name:             "watch-log",
		Mode:             "watch",
		BasePath:         "/var/log",
		PathPattern:      "*.log",
		UploadBucket:     "bucket-a",
		DestPathTemplate: "logs/",
		Recursive:        true,
		CronExpr:         "* * * * *",
		RunOnceOnStart:   true,
		AppendMode:       "overwrite",
		Enabled:          true,
	}
	got := protoToSchedulerRule(proto)
	assert.Equal(t, "rule-1", got.RuleID)
	assert.Equal(t, "watch-log", got.Name)
	assert.Equal(t, "watch", got.Mode)
	assert.Equal(t, "/var/log", got.BasePath)
	assert.Equal(t, "*.log", got.PathPattern)
	assert.Equal(t, "bucket-a", got.UploadBucket)
	assert.Equal(t, "logs/", got.DestPathTemplate)
	assert.True(t, got.Recursive)
	assert.Equal(t, "* * * * *", got.CronExpr)
	assert.True(t, got.RunOnceOnStart)
	assert.Equal(t, "overwrite", got.AppendMode)
	assert.True(t, got.Enabled)
}

// ── buildStoragePath ──────────────────────────────────────────────────────────

func testLogger() *zap.Logger { return zap.NewNop() }

func TestBuildStoragePath_WithPrefix(t *testing.T) {
	rule := scheduler.CollectionRule{BasePath: "/tmp", DestPathTemplate: "data/logs/{filename}"}
	got := buildStoragePath(rule, "/tmp/file.txt", trollsift.AgentContext{}, time.Now().UTC(), testLogger())
	assert.Equal(t, "data/logs/file.txt", got)
}

func TestBuildStoragePath_EmptyPrefix(t *testing.T) {
	rule := scheduler.CollectionRule{BasePath: "/tmp", DestPathTemplate: ""}
	got := buildStoragePath(rule, "/tmp/report.csv", trollsift.AgentContext{}, time.Now().UTC(), testLogger())
	assert.Equal(t, "report.csv", got)
}

func TestBuildStoragePath_TrailingSlash(t *testing.T) {
	rule := scheduler.CollectionRule{BasePath: "/data", DestPathTemplate: "uploads/{filename}"}
	got := buildStoragePath(rule, "/data/out.bin", trollsift.AgentContext{}, time.Now().UTC(), testLogger())
	assert.Equal(t, "uploads/out.bin", got)
}

// A leading "/" must not survive into the object key — the Control Plane
// reverse-parses the normalised template against exactly this string.
func TestBuildStoragePath_LeadingSlashTemplate(t *testing.T) {
	rule := scheduler.CollectionRule{BasePath: "/data", DestPathTemplate: "/{agent_name}/{filename}"}
	got := buildStoragePath(rule, "/data/out.bin",
		trollsift.AgentContext{AgentName: "tokyo-site"}, time.Now().UTC(), testLogger())
	assert.Equal(t, "tokyo-site/out.bin", got)
}

// Regression for IC-BUG-17: an agent restarted from a cached token used to have
// an empty AgentContext, so {agent_name} could not resolve and every upload
// silently collapsed to the bare base name at the bucket root.
func TestBuildStoragePath_MissingAgentIdentity_FallsBackVisibly(t *testing.T) {
	rule := scheduler.CollectionRule{BasePath: "/data", DestPathTemplate: "/{agent_name}/{filename}"}
	got := buildStoragePath(rule, "/data/out.bin", trollsift.AgentContext{}, time.Now().UTC(), testLogger())
	assert.Equal(t, "out.bin", got, "unresolvable template still falls back")
}

// ── submitFile ────────────────────────────────────────────────────────────────

func openTestQueue(t *testing.T) *queue.Queue {
	t.Helper()
	q, err := queue.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func TestSubmitFile_SubmitsNewFile(t *testing.T) {
	q := openTestQueue(t)
	submitted := make(chan struct{}, 1)
	exec := executor.New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		submitted <- struct{}{}
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}, zap.NewNop(), 0)
	exec.Start(context.Background())
	defer exec.Stop()

	rule := scheduler.CollectionRule{RuleID: "r1", BasePath: "/tmp", UploadBucket: "bkt", DestPathTemplate: "logs/{filename}"}
	submitFile(exec, q, rule, "/tmp/f.txt", 100, time.Now(), 0, "", trollsift.AgentContext{}, zap.NewNop())

	select {
	case <-submitted:
	case <-time.After(time.Second):
		t.Fatal("async worker did not process submitted file")
	}
}

func TestSubmitFile_SkipsDuplicate(t *testing.T) {
	q := openTestQueue(t)
	called := make(chan struct{}, 1)
	exec := executor.New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		called <- struct{}{}
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}, zap.NewNop(), 0)
	exec.Start(context.Background())
	defer exec.Stop()

	rule := scheduler.CollectionRule{RuleID: "r1", BasePath: "/tmp", UploadBucket: "bkt", DestPathTemplate: "logs/{filename}"}

	// Mark the file as already processed.
	err := q.UpsertProcessedFile(&queue.ProcessedFile{
		ID:        "pf-1",
		RuleID:    rule.RuleID,
		LocalPath: "/tmp/dup.txt",
	})
	require.NoError(t, err)

	submitFile(exec, q, rule, "/tmp/dup.txt", 100, time.Now(), 0, "", trollsift.AgentContext{}, zap.NewNop())
	select {
	case <-called:
		t.Fatal("duplicate file triggered upload")
	case <-time.After(100 * time.Millisecond):
	}
}

// ── walkAndSubmit ─────────────────────────────────────────────────────────────

func TestWalkAndSubmit_SubmitsMatchingFiles(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "a.log"), []byte("x"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "b.txt"), []byte("y"), 0o644))

	q := openTestQueue(t)
	submitted := make(chan string, 1)
	exec := executor.New(1, q, func(_ context.Context, task *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		submitted <- task.LocalPath
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}, zap.NewNop(), 0)
	exec.Start(context.Background())
	defer exec.Stop()

	rule := scheduler.CollectionRule{RuleID: "r2", BasePath: dir, PathPattern: "*.log", UploadBucket: "bkt", DestPathTemplate: "logs/{filename}"}
	walkAndSubmit(context.Background(), exec, q, rule, dir, trollsift.AgentContext{}, zap.NewNop())

	select {
	case path := <-submitted:
		assert.Contains(t, path, "a.log")
	case <-time.After(time.Second):
		t.Fatal("matching file was not submitted")
	}
}

func TestWalkAndSubmit_NonExistentPathLogsWarning(t *testing.T) {
	q := openTestQueue(t)
	exec := executor.New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}, zap.NewNop(), 0)
	exec.Start(context.Background())
	defer exec.Stop()

	rule := scheduler.CollectionRule{RuleID: "r3", BasePath: "/nonexistent/path", PathPattern: "*.log"}
	// Should not panic; just logs a warning.
	walkAndSubmit(context.Background(), exec, q, rule, "/nonexistent/path", trollsift.AgentContext{}, zap.NewNop())
}

// ── runWatcher ────────────────────────────────────────────────────────────────

func TestRunWatcher_CancelExits(t *testing.T) {
	dir := t.TempDir()
	q := openTestQueue(t)
	exec := executor.New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}, zap.NewNop(), 0)
	exec.Start(context.Background())
	defer exec.Stop()

	rule := scheduler.CollectionRule{
		RuleID:           "r4",
		BasePath:         dir,
		PathPattern:      "*.log",
		UploadBucket:     "bkt",
		DestPathTemplate: "logs/{filename}",
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		runWatcher(ctx, rule, exec, q, trollsift.AgentContext{}, zap.NewNop())
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("runWatcher did not exit after context cancellation")
	}
}
