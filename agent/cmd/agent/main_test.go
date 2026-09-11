package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/types/known/timestamppb"
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
	stsMgr.SetSTS(&credential.STSCredentials{Expiry: time.Now().Add(time.Hour)}, 1)

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
	got, err := buildStoragePath(rule, "/tmp/file.txt", trollsift.AgentContext{}, time.Now().UTC(), testLogger())
	require.NoError(t, err)
	assert.Equal(t, "data/logs/file.txt", got)
}

// An empty template cannot produce an object key. Since D-030 the key is
// produced entirely by dest_path_template (and REST create-rule already
// requires it, agents.go binding:"required"), so "" fails the task rather
// than flattening the file into the bucket root — IC-BUG-21's "empty result"
// fallback path.
func TestBuildStoragePath_EmptyPrefix_FailsTask(t *testing.T) {
	rule := scheduler.CollectionRule{BasePath: "/tmp", DestPathTemplate: ""}
	got, err := buildStoragePath(rule, "/tmp/report.csv", trollsift.AgentContext{}, time.Now().UTC(), testLogger())
	require.Error(t, err, "empty template must fail, not flatten to the bucket root")
	assert.Empty(t, got)
}

func TestBuildStoragePath_TrailingSlash(t *testing.T) {
	rule := scheduler.CollectionRule{BasePath: "/data", DestPathTemplate: "uploads/{filename}"}
	got, err := buildStoragePath(rule, "/data/out.bin", trollsift.AgentContext{}, time.Now().UTC(), testLogger())
	require.NoError(t, err)
	assert.Equal(t, "uploads/out.bin", got)
}

// A leading "/" must not survive into the object key — the Control Plane
// reverse-parses the normalised template against exactly this string.
func TestBuildStoragePath_LeadingSlashTemplate(t *testing.T) {
	rule := scheduler.CollectionRule{BasePath: "/data", DestPathTemplate: "/{agent_name}/{filename}"}
	got, err := buildStoragePath(rule, "/data/out.bin",
		trollsift.AgentContext{AgentName: "tokyo-site"}, time.Now().UTC(), testLogger())
	require.NoError(t, err)
	assert.Equal(t, "tokyo-site/out.bin", got)
}

// Regression for IC-BUG-17: an agent restarted from a cached token used to have
// an empty AgentContext, so {agent_name} could not resolve and every upload
// silently collapsed to the bare base name at the bucket root. After
// IC-BUG-21 an unresolvable template must fail the task, never guess a key:
// with D-030's bucket-wide policy a wrong key is not caught by any 403 and
// would silently land in the wrong place.
func TestBuildStoragePath_MissingAgentIdentity_FailsTask(t *testing.T) {
	rule := scheduler.CollectionRule{BasePath: "/data", DestPathTemplate: "/{agent_name}/{filename}"}
	got, err := buildStoragePath(rule, "/data/out.bin", trollsift.AgentContext{}, time.Now().UTC(), testLogger())
	require.Error(t, err, "unresolvable template must fail, not fall back")
	assert.Empty(t, got, "no object key may be produced on template failure")
	assert.Contains(t, err.Error(), "agent_name")
}

// A syntactically invalid template must return an error naming the rule, the
// template and the cause — never a guessed key.
func TestBuildStoragePath_InvalidTemplate_FailsTask(t *testing.T) {
	rule := scheduler.CollectionRule{
		RuleID:           "r-broken",
		BasePath:         "/data",
		DestPathTemplate: "/logs/{unclosed/{filename}",
	}
	got, err := buildStoragePath(rule, "/data/out.bin",
		trollsift.AgentContext{AgentName: "tokyo-site"}, time.Now().UTC(), testLogger())
	require.Error(t, err, "invalid template must fail, not fall back")
	assert.Empty(t, got)
	assert.Contains(t, err.Error(), "r-broken")
	assert.Contains(t, err.Error(), "logs/{unclosed/{filename")
}

// A template that resolves to an empty key must fail the task — an empty key
// is not a legal object key and falling back would guess one.
func TestBuildStoragePath_EmptyResolvedKey_FailsTask(t *testing.T) {
	rule := scheduler.CollectionRule{
		RuleID:           "r-empty",
		BasePath:         "/data",
		DestPathTemplate: "{ext}",
	}
	got, err := buildStoragePath(rule, "/data/noext",
		trollsift.AgentContext{}, time.Now().UTC(), testLogger())
	require.Error(t, err, "empty resolved key must fail, not fall back")
	assert.Empty(t, got)
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

// stableGeneration 模拟无并发覆盖的重试窗口：代际恒定，第二次 403 即终态。
var stableGeneration = func() uint64 { return 1 }

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
		defaultAssumedUploadBytesPerSecond, zap.NewNop(), upload, refresh, stableGeneration)
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
		defaultAssumedUploadBytesPerSecond, zap.NewNop(), upload, refresh, stableGeneration)
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
		defaultAssumedUploadBytesPerSecond, zap.NewNop(), upload, refresh, stableGeneration)
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
		defaultAssumedUploadBytesPerSecond, zap.NewNop(), upload, refresh, stableGeneration)
	require.Error(t, err)
	assert.False(t, errors.Is(err, executor.ErrTerminalUpload))
	assert.Zero(t, refreshes, "other error classes must not burn a credential refresh")
}

// IC-BUG-21: an unresolved dest_path_template refuses the task — nothing is
// enqueued, so nothing can ever land under a guessed key. One misconfigured
// rule must produce one Warn for the rule, not one per file.
func TestSubmitFile_UnresolvedTemplate_RefusesTask(t *testing.T) {
	q := openTestQueue(t)
	core, logs := observer.New(zapcore.WarnLevel)
	logger := zap.New(core)
	exec := executor.New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		t.Error("no task may reach the uploader with an unresolved object key")
		return nil, fmt.Errorf("must not upload")
	}, zap.NewNop(), 0)
	exec.Start(context.Background())
	defer exec.Stop()

	rule := scheduler.CollectionRule{
		RuleID:           "submit-refusal-1",
		BasePath:         "/tmp",
		UploadBucket:     "bkt",
		DestPathTemplate: "/{agent_name}/{filename}",
	}
	// The rule watches a busy directory: many files, same broken template.
	for i := 0; i < 3; i++ {
		submitFile(context.Background(), exec, q, rule,
			fmt.Sprintf("/tmp/f%d.txt", i), 100, time.Now(), 0, "", trollsift.AgentContext{}, logger)
	}

	depth, err := q.CountPending()
	require.NoError(t, err)
	assert.Zero(t, depth, "a task with an unresolved object key must never be enqueued")

	entries := logs.FilterMessageSnippet("refusing to guess an object key").All()
	require.Len(t, entries, 1, "one broken rule must Warn exactly once, not per file")
}

// ── IC-BUG-30: agent stops rules outside the synced full set ─────────────────

// Rules deleted while disconnected are absent from RulesSyncCommand.rule_ids;
// the agent must stop every held rule outside the set. Inactive rules are NOT
// here — they arrive as ordinary pushes whose Enabled=false stops them.
func TestStopRulesOutsideSync(t *testing.T) {
	stopped := make([]string, 0, 2)
	stop := func(ruleID string) { stopped = append(stopped, ruleID) }

	stopRulesOutsideSync(
		[]string{"keep-a", "keep-b"},
		func() []string { return []string{"keep-a", "stale-deleted", "stale-also-deleted", "keep-b"} },
		stop,
		zap.NewNop(),
	)
	assert.ElementsMatch(t, []string{"stale-deleted", "stale-also-deleted"}, stopped)

	// A rule that is in the set must never be stopped, even if it was pushed
	// just before the sync (the normal reconnect order).
	stopped = nil
	stopRulesOutsideSync([]string{"r1"}, func() []string { return []string{"r1"} }, stop, zap.NewNop())
	assert.Empty(t, stopped)
}

func TestStopRulesOutsideSync_EmptySetStopsEverything(t *testing.T) {
	stopped := make([]string, 0, 2)
	stop := func(ruleID string) { stopped = append(stopped, ruleID) }
	stopRulesOutsideSync(nil, func() []string { return []string{"a", "b"} }, stop, zap.NewNop())
	assert.ElementsMatch(t, []string{"a", "b"}, stopped,
		"an empty full-set means every rule was deleted while disconnected")
}

// ── IC-BUG-30 (D-033 快照形态): agent 以快照原子替换规则集 ─────────────────────

// The snapshot is the agent's complete rule set: rules outside it were deleted
// while disconnected (stop them); rules inside it are applied as-is —
// Enabled=false runs applyRule's existing stop branch, so pausing is covered
// without new code.
func TestApplyRulesSnapshot_ReplacesRuleSet(t *testing.T) {
	type applied struct {
		id      string
		enabled bool
	}
	var stopped []string
	var got []applied

	active := scheduler.CollectionRule{RuleID: "r-active", Enabled: true}
	inactive := scheduler.CollectionRule{RuleID: "r-inactive", Enabled: false}
	deleted := "r-deleted-while-offline"

	applyRulesSnapshot(
		[]scheduler.CollectionRule{active, inactive},
		func() []string { return []string{"r-active", deleted} },
		func(ruleID string) { stopped = append(stopped, ruleID) },
		func(r scheduler.CollectionRule) { got = append(got, applied{r.RuleID, r.Enabled}) },
		zap.NewNop(),
	)

	assert.Equal(t, []string{deleted}, stopped,
		"a held rule absent from the snapshot was deleted while disconnected")
	assert.Len(t, got, 2, "every snapshot rule is applied")
	assert.Equal(t, "r-active", got[0].id)
	assert.True(t, got[0].enabled)
	assert.Equal(t, "r-inactive", got[1].id)
	assert.False(t, got[1].enabled, "inactive snapshot rules go through applyRule's stop branch")
}

// An empty snapshot means every rule was deleted while disconnected — the
// agent must stop everything and start nothing.
func TestApplyRulesSnapshot_EmptySnapshot_StopsAll(t *testing.T) {
	var stopped []string
	var applied []string
	applyRulesSnapshot(nil,
		func() []string { return []string{"a", "b"} },
		func(ruleID string) { stopped = append(stopped, ruleID) },
		func(r scheduler.CollectionRule) { applied = append(applied, r.RuleID) },
		zap.NewNop(),
	)
	assert.ElementsMatch(t, []string{"a", "b"}, stopped)
	assert.Empty(t, applied)
}

// buildStoragePath 分支补齐： trollsift path_pattern 提供变量、{time} 注入、
// 相对路径推导失败回退 Base。
func TestBuildStoragePath_PathPatternFieldsAndTime(t *testing.T) {
	rule := scheduler.CollectionRule{
		RuleID:           "r-fields",
		BasePath:         "/data",
		PathPattern:      "{site}/{filename}",
		DestPathTemplate: "{time:yyyy}/{site}/{filename}",
	}
	got, err := buildStoragePath(rule, "/data/tokyo/out.bin",
		trollsift.AgentContext{}, time.Date(2026, 9, 11, 0, 0, 0, 0, time.UTC), testLogger())
	require.NoError(t, err)
	assert.Equal(t, "2026/tokyo/out.bin", got)
}

func TestBuildStoragePath_RelError_FallsBackToBase(t *testing.T) {
	rule := scheduler.CollectionRule{BasePath: "relative", DestPathTemplate: "p/{filename}"}
	got, err := buildStoragePath(rule, "/abs/path/f.bin", trollsift.AgentContext{}, time.Now().UTC(), testLogger())
	require.NoError(t, err)
	assert.Equal(t, "p/f.bin", got)
}

// submitFile 对 IsProcessed / 入队失败的分支：队列已关闭时只告警，不 panic。
func TestSubmitFile_QueueClosed_LogsAndSurvives(t *testing.T) {
	q := openTestQueue(t)
	exec := executor.New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		return &uploadpkg.UploadResult{}, nil
	}, zap.NewNop(), 0)
	rule := scheduler.CollectionRule{RuleID: "r-closed", BasePath: "/tmp", UploadBucket: "bkt", DestPathTemplate: "p/{filename}"}
	require.NoError(t, q.Close())
	submitFile(context.Background(), exec, q, rule, "/tmp/f.txt", 1, time.Now(), 0, "", trollsift.AgentContext{}, zap.NewNop())
}

// ── F2（IC-2b review）：并发覆盖下旧刷新响应不得复活旧凭据 ─────────────────────

func testCredPayload(accessKey string) *agentv1.CredentialsPayload {
	return &agentv1.CredentialsPayload{
		AccessKey: accessKey,
		ExpiresAt: timestamppb.New(time.Now().Add(time.Hour)),
		Endpoint:  "localhost:9000",
	}
}

// 评审验收场景：重试路径刚拿到指向新 bucket 的新凭据（gen 2），一个更早发出、
// 更晚返回的后台刷新响应（gen 1）到达——它不得覆盖新凭据；第二次尝试必须用
// 新凭据成功，任务不得落终态失败。
// 无代际保护的实现里这个响应会覆盖新凭据 → 第二次尝试 403 → 落终态（红）。
func TestUploadWithAccessDeniedRetry_StaleResponseDoesNotOverrideFresh(t *testing.T) {
	path := writeLocalFile(t, "f.bin")
	task := testUploadTask(path)
	holder := newCredentialHolder(credential.NewSTSManager(), 64, 1, zap.NewNop())
	require.True(t, holder.Apply(testCredPayload("AK-old"), 1))

	attempts := 0
	upload := func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		attempts++
		if attempts == 2 {
			// 模拟后台刷新的迟到旧响应恰在第二次尝试读取凭据前落地。
			holder.Apply(testCredPayload("AK-old"), 1)
		}
		if holder.Current().AccessKey != "AK-new" {
			return nil, accessDeniedError("new-bucket")
		}
		return &uploadpkg.UploadResult{StoragePath: task.StoragePath, Bucket: task.Bucket, SizeBytes: 5}, nil
	}
	refreshes := 0
	refresh := func(_ context.Context) error {
		refreshes++
		require.True(t, holder.Apply(testCredPayload("AK-new"), 2))
		return nil
	}

	result, err := uploadWithAccessDeniedRetry(context.Background(), task, 30*time.Second,
		defaultAssumedUploadBytesPerSecond, zap.NewNop(), upload, refresh, holder.Generation)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.Equal(t, "AK-new", holder.Current().AccessKey, "the fresh credentials must remain in force")
	assert.Equal(t, 2, attempts)
	assert.Equal(t, 1, refreshes)
}

// 代际在重试期间被别人改动过：第二次尝试可能跑在别人的凭据上——再多试一次，
// 而不是判死刑；终态要求代际稳定。
func TestUploadWithAccessDeniedRetry_GenerationChangedDuringRetry_OneMoreAttempt(t *testing.T) {
	path := writeLocalFile(t, "f.bin")
	task := testUploadTask(path)
	genSeq := uint64(1)
	gen := func() uint64 { return genSeq }
	attempts := 0
	upload := func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		attempts++
		if attempts == 2 {
			// 第二次尝试期间另一个写入方应用了新代际。
			genSeq = 2
		}
		return nil, accessDeniedError("bkt")
	}
	refreshes := 0
	refresh := func(_ context.Context) error { refreshes++; return nil }

	_, err := uploadWithAccessDeniedRetry(context.Background(), task, 30*time.Second,
		defaultAssumedUploadBytesPerSecond, zap.NewNop(), upload, refresh, gen)
	require.Error(t, err)
	assert.Equal(t, 3, attempts, "a generation change buys exactly one more attempt")
	assert.ErrorIs(t, err, executor.ErrTerminalUpload, "still terminal on stable generation")
	assert.Equal(t, 1, refreshes)
}

// credentialHolder：迟到旧响应整体丢弃，STS 会话与 uploader 配置不得半更新。
func TestCredentialHolder_RejectsStaleGeneration_Whole(t *testing.T) {
	holder := newCredentialHolder(credential.NewSTSManager(), 64, 2, zap.NewNop())
	require.True(t, holder.Apply(testCredPayload("AK-b"), 7))
	require.NotNil(t, holder.Current())
	require.False(t, holder.Apply(testCredPayload("AK-a"), 6), "stale generation must be dropped")
	assert.Equal(t, "AK-b", holder.Current().AccessKey, "uploader config must not be half-updated")
	assert.Equal(t, uint64(7), holder.Generation())
	assert.False(t, holder.Apply(nil, 8), "nil payload is a no-op")
}

// ── R1（IC-2b review 三轮）：Apply 的原子性 ────────────────────────────────────

// ── R1/R4（IC-2b review）：Apply 的原子性 —— 确定性交错 ───────────────────────

// hookableSession wraps the real STS manager with a test hook that can park a
// goroutine right after SetSTS returns — before Apply continues. In the fixed
// implementation that point sits INSIDE h.mu (the parker holds the whole
// critical section); in the split mutation it sits outside, which is what
// makes the tear constructible deterministically instead of by scheduling
// luck (review R4: the probabilistic probe survived the mutation 5/5).
type hookableSession struct {
	inner *credential.STSManager
	// afterSet, when set, is invoked with the generation after every SetSTS.
	afterSet func(generation uint64)
}

func (h *hookableSession) SetSTS(cred *credential.STSCredentials, generation uint64) bool {
	ok := h.inner.SetSTS(cred, generation)
	if h.afterSet != nil {
		h.afterSet(generation)
	}
	return ok
}

func (h *hookableSession) Generation() uint64 { return h.inner.Generation() }

// The interleaving under test: an older generation completes SetSTS, is then
// suspended; a newer generation completes the WHOLE apply; the suspended
// goroutine resumes and overwrites the uploader credentials with the older
// payload — generation=N, uploader=N-1, and every upload fails on stale
// credentials until the next acquisition.
//
// Protocol (no scheduling luck involved):
//  1. A (gen 1) parks at its post-SetSTS hook — signalled, in both
//     implementations (under h.mu in the fixed one, outside it in the split).
//  2. B (gen 2) runs. In the SPLIT mutation it completes without needing A's
//     lock → doneB fires. In the fixed implementation it is structurally
//     blocked on h.mu (A parks holding it) → doneB cannot fire.
//  3. A is released either strictly AFTER doneB (split: A's cfg write then
//     lands after B's — deterministic tear) or after a grace period (fixed:
//     B is queued on the mutex, so it writes last regardless of when the
//     grace expires — deterministic green). The grace only picks between two
//     deterministic outcomes; no assertion depends on timing.
func TestCredentialHolder_ApplyIsAtomic_DeterministicInterleave(t *testing.T) {
	aParked := make(chan struct{})
	proceedA := make(chan struct{})
	var proceedAOnce sync.Once
	inner := &hookableSession{
		inner: credential.NewSTSManager(),
		afterSet: func(generation uint64) {
			if generation == 1 {
				close(aParked)
				<-proceedA
			}
		},
	}
	holder := newCredentialHolder(inner, 64, 1, zap.NewNop())

	// A: older generation, parks right after SetSTS succeeds.
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		require.True(t, holder.Apply(testCredPayload("AK-1"), 1))
	}()
	<-aParked

	// B: newer generation, full apply.
	doneB := make(chan struct{})
	go func() {
		defer close(doneB)
		holder.Apply(testCredPayload("AK-2"), 2)
	}()

	// Release A strictly after B's completion when B can complete (split
	// mutation), otherwise after a grace period (fixed implementation). Either
	// way the final state is deterministic.
	go func() {
		select {
		case <-doneB:
		case <-time.After(2 * time.Second):
		}
		proceedAOnce.Do(func() { close(proceedA) })
	}()
	<-doneA
	select {
	case <-doneB:
	case <-time.After(2 * time.Second):
		t.Fatal("B's apply never completed")
	}

	cfg := holder.Current()
	require.NotNil(t, cfg)
	require.Equal(t, "AK-2", cfg.AccessKey,
		"generation 2 is in force; the uploader credentials must not lag behind it — "+
			"the apply is not atomic (R1/R4)")
	require.Equal(t, uint64(2), holder.Generation())
}

// ── R5-A（IC-2b review 四轮）：读侧撕裂——Generation() 必须与发布对同源 ──────────

// 中间态构造：Apply(gen 2) 在 SetSTS 成功后停在钩子上——STS 会话已知道 gen 2，
// 但 (AK-2, 2) 这一对尚未发布（cfg 仍是 AK-1/1）。此时读 Generation()：
//
//	绕过 h.mu 的实现（缺陷形态）立即返回 2——一个其凭据尚未发布的代际，
//	  uploadWithAccessDeniedRetry 拿它做终态判断，等于撕裂读重新进来（R1 白修）；
//	正确实现阻塞在 h.mu 上，直到 (AK-2, 2) 整体发布后才返回 2——恒一致。
func TestCredentialHolder_GenerationReadConsistentWithPublishedPair(t *testing.T) {
	aParked := make(chan struct{})
	proceedA := make(chan struct{})
	var proceedOnce sync.Once
	inner := &hookableSession{
		inner: credential.NewSTSManager(),
		afterSet: func(generation uint64) {
			if generation == 2 {
				close(aParked)
				<-proceedA
			}
		},
	}
	holder := newCredentialHolder(inner, 64, 1, zap.NewNop())
	require.True(t, holder.Apply(testCredPayload("AK-1"), 1))

	// A：Apply(gen 2) 停在发布中途，占住 h.mu。
	doneA := make(chan struct{})
	go func() {
		defer close(doneA)
		holder.Apply(testCredPayload("AK-2"), 2)
	}()
	<-aParked

	// 读侧：Generation() 必须等发布对更新后才能返回。
	doneR := make(chan uint64, 1)
	go func() { doneR <- holder.Generation() }()

	// 与 R4 相同的双分支放行：读立即返回（变异）→ 撕裂实锤；2s 内未返回
	// （修复：读被 A 持有的 h.mu 挡住）→ 放行 A，读随后拿到已发布的 2。
	// 两个分支的终态都确定，断言不依赖时序。
	tornCh := make(chan uint64, 1)
	releaseDone := make(chan struct{})
	go func() {
		select {
		case g := <-doneR:
			tornCh <- g
		case <-time.After(2 * time.Second):
		}
		proceedOnce.Do(func() { close(proceedA) })
		close(releaseDone)
	}()
	<-releaseDone
	<-doneA

	cfg, gen := holder.Snapshot()
	require.NotNil(t, cfg)
	require.Equal(t, "AK-2", cfg.AccessKey)
	require.Equal(t, uint64(2), gen)

	select {
	case g := <-tornCh:
		require.FailNow(t, "Generation() returned while its credentials were unpublished",
			"a torn read slipped through: generation %d was visible while cfg was still AK-1 "+
				"— the read bypassed the holder's critical section (R5-A)", g)
	default:
	}
}
