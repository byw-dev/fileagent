package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/credential"
	"github.com/byw-dev/fileagent/agent/internal/executor"
	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/byw-dev/fileagent/agent/internal/scheduler"
	uploadpkg "github.com/byw-dev/fileagent/agent/internal/uploader"
	"github.com/byw-dev/fileagent/agent/internal/watcher"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/pkg/trollsift"
	"github.com/minio/minio-go/v7"
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
	submitFile(context.Background(), exec, q, rule, "/tmp/f.txt", 100, time.Now(), 0, "", trollsift.AgentContext{}, zap.NewNop())

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

	submitFile(context.Background(), exec, q, rule, "/tmp/dup.txt", 0, time.Unix(0, 0), 0, "", trollsift.AgentContext{}, zap.NewNop())
	select {
	case <-called:
		t.Fatal("duplicate file triggered upload")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestSubmitFile_SubmitsModifiedFile(t *testing.T) {
	q := openTestQueue(t)
	called := make(chan struct{}, 1)
	exec := executor.New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		called <- struct{}{}
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 200}, nil
	}, zap.NewNop(), 0)
	exec.Start(context.Background())
	defer exec.Stop()

	rule := scheduler.CollectionRule{RuleID: "r1", BasePath: "/tmp", UploadBucket: "bkt", DestPathTemplate: "logs/{filename}"}
	oldMtime := time.Unix(1_700_000_000, 0)
	require.NoError(t, q.UpsertProcessedFile(&queue.ProcessedFile{
		ID:         "pf-old",
		RuleID:     rule.RuleID,
		LocalPath:  "/tmp/changed.txt",
		FileSize:   100,
		FileMtime:  oldMtime.Unix(),
		UploadedAt: oldMtime.Unix(),
	}))

	submitFile(context.Background(), exec, q, rule, "/tmp/changed.txt", 200, oldMtime.Add(time.Second), 0, "", trollsift.AgentContext{}, zap.NewNop())
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("modified file was incorrectly treated as already processed")
	}
}

func TestSubmitFile_PersistsTailFields(t *testing.T) {
	q := openTestQueue(t)
	exec := executor.New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		return nil, nil
	}, zap.NewNop(), 0)
	rule := scheduler.CollectionRule{RuleID: "r-tail", BasePath: "/tmp", UploadBucket: "bkt", DestPathTemplate: "logs/{filename}"}

	submitFile(context.Background(), exec, q, rule, "/tmp/tail.log", 256, time.Unix(1_700_000_000, 0), 128, watcher.AppendModeTail, trollsift.AgentContext{}, zap.NewNop())
	tasks, err := q.DequeuePending(1)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, int64(128), tasks[0].FileOffset)
	assert.Equal(t, watcher.AppendModeTail, tasks[0].AppendMode)
}

func TestUploadWithTimeout_CancelsBlockedUpload(t *testing.T) {
	task := &queue.UploadTask{FileSize: 1}
	started := time.Now()
	_, err := uploadWithTimeout(context.Background(), task, 20*time.Millisecond, defaultAssumedUploadBytesPerSecond, zap.NewNop(), func(ctx context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	})
	require.ErrorIs(t, err, context.DeadlineExceeded)
	elapsed := time.Since(started)
	assert.GreaterOrEqual(t, elapsed, 900*time.Millisecond)
	assert.Less(t, elapsed, 2*time.Second)
}

// Regression for PR #100 review F4: multipart ignores FileOffset and the
// whole file is hashed before transfer, so the uploader actually moves
// task.FileSize bytes regardless of tail mode. Deriving the deadline from the
// tail increment made large files always time out and retry to exhaustion.
// The deadline must be derived from FileSize; it is a safety net, wider is
// better than shorter.
func TestUploadWithTimeout_UsesFullFileSizeForDeadline(t *testing.T) {
	task := &queue.UploadTask{
		FileSize:   120 * 1024 * 1024,
		FileOffset: 119 * 1024 * 1024,
		AppendMode: watcher.AppendModeTail,
	}
	before := time.Now()
	result, err := uploadWithTimeout(context.Background(), task, 30*time.Second, defaultAssumedUploadBytesPerSecond, zap.NewNop(), func(ctx context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		// Full 120 MiB at 1 MiB/s → ~120s, NOT the 1 MiB increment → 30s min.
		assert.WithinDuration(t, before.Add(2*time.Minute), deadline, time.Second)
		return &uploadpkg.UploadResult{SizeBytes: 1024 * 1024}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, result)
}

func TestUploadWithTimeout_ClampsInvalidTailOffset(t *testing.T) {
	task := &queue.UploadTask{FileSize: 10, FileOffset: 20, AppendMode: watcher.AppendModeTail}
	result, err := uploadWithTimeout(context.Background(), task, 30*time.Second, defaultAssumedUploadBytesPerSecond, zap.NewNop(), func(ctx context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		assert.WithinDuration(t, time.Now().Add(30*time.Second), deadline, time.Second)
		return &uploadpkg.UploadResult{}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, result)
}

// Regression for PR #100 review R2: the deadline must be derived from the
// file size at upload time (os.Stat), not the size frozen at detection time.
// A file that keeps growing between enqueue and worker pickup would get a
// deadline from the stale size, time out at the minimum, and retry to
// exhaustion — a regression this PR introduced. Stat failure falls back to
// the stored size.
func TestUploadWithTimeout_UsesCurrentFileSizeFromStat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "growing.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 120*1024*1024)), 0o644))
	// The frozen detection-time size is tiny; the real file is 120 MiB.
	task := &queue.UploadTask{LocalPath: path, FileSize: 1024}

	before := time.Now()
	result, err := uploadWithTimeout(context.Background(), task, 30*time.Second, defaultAssumedUploadBytesPerSecond, zap.NewNop(), func(ctx context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok)
		// Current 120 MiB at 1 MiB/s → ~120s, NOT the frozen 1 KiB → 30s min.
		assert.WithinDuration(t, before.Add(2*time.Minute), deadline, time.Second)
		return &uploadpkg.UploadResult{SizeBytes: 1024}, nil
	})
	require.NoError(t, err)
	require.NotNil(t, result)
}

const defaultAssumedUploadBytesPerSecond int64 = 1024 * 1024

// PR #100 review R3: the assumed upload rate is now configuration, not a const.
func TestUploadTimeoutForSize_ScalesAndHonorsMinimum(t *testing.T) {
	rate := defaultAssumedUploadBytesPerSecond
	assert.Equal(t, 30*time.Second, uploadTimeoutForSize(0, 30*time.Second, rate))
	assert.Equal(t, 30*time.Second, uploadTimeoutForSize(1, 30*time.Second, rate))
	assert.Equal(t, 2*time.Minute, uploadTimeoutForSize(120*1024*1024, 30*time.Second, rate))
	assert.Equal(t, time.Duration(1<<63-1), uploadTimeoutForSize(1<<63-1, time.Second, rate))
}

// A slow link must be able to get a workable deadline by lowering the rate.
func TestUploadTimeoutForSize_SlowLinkConfig(t *testing.T) {
	assert.Equal(t, 200*time.Second,
		uploadTimeoutForSize(100*1024*1024, 30*time.Second, 512*1024),
		"500 KiB/s over 100 MiB must yield ~200s, not the 1 MiB/s default")
}

// Regression for PR #100 review F3: runWatcher must rebuild tail offsets from
// processed_files before the initial scan, otherwise a restart re-sends
// already-stored bytes from offset 0.
func TestRunWatcher_TailOffsetSeededFromPersistedState(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 1500)), 0o644))

	q := openTestQueue(t)
	require.NoError(t, q.UpsertProcessedFile(&queue.ProcessedFile{
		ID: "pf-1", RuleID: "r-tail", LocalPath: path, FileSize: 1000, FileMtime: 111,
	}))

	exec := executor.New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		return nil, nil
	}, zap.NewNop(), 0)

	rule := scheduler.CollectionRule{
		RuleID: "r-tail", BasePath: dir, PathPattern: "*.log",
		UploadBucket: "bkt", DestPathTemplate: "logs/{filename}", AppendMode: watcher.AppendModeTail,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		runWatcher(ctx, rule, exec, q, trollsift.AgentContext{}, zap.NewNop())
	}()

	// The task must appear with the persisted offset, not 0.
	var tasks []*queue.UploadTask
	deadline := time.After(5 * time.Second)
	for {
		var err error
		tasks, err = q.ListByStatus(queue.StatusPending)
		require.NoError(t, err)
		if len(tasks) > 0 {
			break
		}
		select {
		case <-deadline:
			t.Fatal("runWatcher did not submit the grown file within deadline")
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-done

	require.Len(t, tasks, 1)
	assert.Equal(t, path, tasks[0].LocalPath)
	assert.Equal(t, int64(1000), tasks[0].FileOffset,
		"task must resume tail from the persisted offset, not 0")
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

// ── IC-BUG-20: AccessDenied invalidates credentials, refreshes and retries once ─

func accessDeniedError(bucket string) error {
	return fmt.Errorf("uploader: put object: %w", minio.ErrorResponse{
		Code:       "AccessDenied",
		Message:    "Access Denied",
		BucketName: bucket,
	})
}

func otherForbiddenError(bucket string) error {
	return fmt.Errorf("uploader: put object: %w", minio.ErrorResponse{
		Code:       "InvalidAccessKeyId",
		Message:    "bad key",
		BucketName: bucket,
	})
}

func TestIsAccessDenied(t *testing.T) {
	assert.True(t, isAccessDenied(accessDeniedError("bkt")), "wrapped minio AccessDenied must be detected")
	assert.False(t, isAccessDenied(otherForbiddenError("bkt")), "other S3 codes must not match")
	assert.False(t, isAccessDenied(assert.AnError))
	assert.False(t, isAccessDenied(nil))
}

func writeLocalFile(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o600))
	return path
}

func testUploadTask(path string) *queue.UploadTask {
	return &queue.UploadTask{ID: "task-1", RuleID: "r1", LocalPath: path, Bucket: "bkt", StoragePath: "logs/f.bin"}
}

// A first AccessDenied must trigger exactly one invalidate+refresh+retry; the
// refreshed attempt succeeding must surface its result.
func TestUploadWithAccessDeniedRetry_RefreshesAndSucceedsOnce(t *testing.T) {
	path := writeLocalFile(t, "f.bin")
	task := testUploadTask(path)
	attempts := 0
	uploads := make([]string, 0, 2)
	upload := func(_ context.Context, tk *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		attempts++
		uploads = append(uploads, tk.Bucket)
		if attempts == 1 {
			return nil, accessDeniedError("bkt")
		}
		return &uploadpkg.UploadResult{StoragePath: tk.StoragePath, Bucket: tk.Bucket, SizeBytes: 5}, nil
	}
	refreshes := 0
	refresh := func(_ context.Context) error { refreshes++; return nil }

	result, err := uploadWithAccessDeniedRetry(context.Background(), task, 30*time.Second,
		defaultAssumedUploadBytesPerSecond, zap.NewNop(), upload, refresh)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, 2, attempts, "exactly one retry after AccessDenied")
	assert.Equal(t, 1, refreshes, "credentials must be refreshed exactly once")
}

// A second AccessDenied must land terminally with no further refresh/retry
// cycle — policy errors must not spin into a silent refresh loop.
func TestUploadWithAccessDeniedRetry_SecondDenied_Terminal(t *testing.T) {
	path := writeLocalFile(t, "f.bin")
	task := testUploadTask(path)
	attempts := 0
	upload := func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		attempts++
		return nil, accessDeniedError("bkt")
	}
	refreshes := 0
	refresh := func(_ context.Context) error { refreshes++; return nil }

	_, err := uploadWithAccessDeniedRetry(context.Background(), task, 30*time.Second,
		defaultAssumedUploadBytesPerSecond, zap.NewNop(), upload, refresh)
	require.Error(t, err)
	assert.Equal(t, 2, attempts, "no attempt beyond the single refresh retry")
	assert.Equal(t, 1, refreshes)
	assert.ErrorIs(t, err, executor.ErrTerminalUpload, "second AccessDenied must be terminal, not retried")
}

// If the refresh itself fails the original AccessDenied propagates unchanged
// (non-terminal): a transient control-plane failure must not fail the task
// terminally — the executor's backoff will retry it later.
func TestUploadWithAccessDeniedRetry_RefreshFails_Propagates(t *testing.T) {
	path := writeLocalFile(t, "f.bin")
	task := testUploadTask(path)
	attempts := 0
	upload := func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		attempts++
		return nil, accessDeniedError("bkt")
	}
	refresh := func(_ context.Context) error { return assert.AnError }

	_, err := uploadWithAccessDeniedRetry(context.Background(), task, 30*time.Second,
		defaultAssumedUploadBytesPerSecond, zap.NewNop(), upload, refresh)
	require.Error(t, err)
	assert.False(t, errors.Is(err, executor.ErrTerminalUpload),
		"refresh failure is transient, not a policy verdict")
	assert.Equal(t, 1, attempts, "no retry without refreshed credentials")
	assert.Error(t, refresh(context.Background()))
}

// Non-AccessDenied failures must pass through untouched — no refresh, no retry.
func TestUploadWithAccessDeniedRetry_OtherError_PassThrough(t *testing.T) {
	path := writeLocalFile(t, "f.bin")
	task := testUploadTask(path)
	upload := func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		return nil, otherForbiddenError("bkt")
	}
	refreshes := 0
	refresh := func(_ context.Context) error { refreshes++; return nil }

	_, err := uploadWithAccessDeniedRetry(context.Background(), task, 30*time.Second,
		defaultAssumedUploadBytesPerSecond, zap.NewNop(), upload, refresh)
	require.Error(t, err)
	assert.False(t, errors.Is(err, executor.ErrTerminalUpload))
	assert.Zero(t, refreshes, "other error classes must not burn a credential refresh")
}
