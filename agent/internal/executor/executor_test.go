package executor

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"sync/atomic"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	uploadpkg "github.com/byw-dev/fileagent/agent/internal/uploader"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newTestQueue(t *testing.T) *queue.Queue {
	t.Helper()
	q, err := queue.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func newTask(ruleID, path string) *queue.UploadTask {
	return &queue.UploadTask{
		ID:          uuid.New().String(),
		RuleID:      ruleID,
		LocalPath:   path,
		StoragePath: "bucket/key",
		Bucket:      "test-bucket",
		FileSize:    100,
		FileMtime:   time.Now().Unix(),
	}
}

func successUploader(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
	return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
}

func failUploader(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
	return nil, fmt.Errorf("upload failed")
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestExecutor_SubmitAndProcess(t *testing.T) {
	q := newTestQueue(t)
	var processed atomic.Int32
	uploader := func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		processed.Add(1)
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}

	e := New(2, q, uploader, zap.NewNop(), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Start(ctx)

	task := newTask("r1", "/path/file.txt")
	require.NoError(t, e.Submit(context.Background(), task))

	require.Eventually(t, func() bool {
		return processed.Load() == 1
	}, 3*time.Second, 50*time.Millisecond)

	e.Stop()
}

func TestExecutor_Dedup(t *testing.T) {
	q := newTestQueue(t)
	var uploadCount atomic.Int32
	uploader := func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		uploadCount.Add(1)
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}

	e := New(1, q, uploader, zap.NewNop(), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Start(ctx)

	// Submit a task and let it complete.
	t1 := newTask("r1", "/path/file.txt")
	require.NoError(t, e.Submit(context.Background(), t1))

	require.Eventually(t, func() bool {
		return uploadCount.Load() == 1
	}, 3*time.Second, 50*time.Millisecond)

	require.Eventually(t, func() bool { r, _ := q.GetReport(context.Background(), t1.ID); return len(r) > 0 }, time.Second, time.Millisecond)
	require.NoError(t, e.HandleAcknowledgement(context.Background(), &agentv1.Acknowledgement{RefMessageId: t1.ID, Success: true}))

	// Submit a second task with the same path and metadata — should be skipped.
	t2 := newTask("r1", "/path/file.txt")
	t2.FileMtime = t1.FileMtime
	t2.FileSize = t1.FileSize
	require.NoError(t, e.Submit(context.Background(), t2))

	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int32(1), uploadCount.Load(), "duplicate file should not be uploaded again")

	e.Stop()
}

// TestExecutor_FailedBackoffRecoveredAfterRestart pins the P1-d fix: the
// failed-state backoff must not live only in a memory goroutine. A process
// that exits during the backoff window used to leave the task `failed`
// forever — its in-flight multipart upload neither resumed nor aborted, while
// a new task for the same file version could enqueue over it. After a
// "restart" (fresh executor over the same queue), the task must be re-queued
// and RESUMED — not aborted.
func TestExecutor_FailedBackoffRecoveredAfterRestart(t *testing.T) {
	q := newTestQueue(t)
	rec := &abandonRecorder{}

	// Attempt 1: task fails while its multipart upload is in flight; the
	// backoff retry is scheduled in memory.
	task := abandonTask(t, q) // running, upload-live-1
	e1 := New(1, q, failUploader, zap.NewNop(), 0)
	e1.retryDelays = []time.Duration{50 * time.Millisecond}
	require.NoError(t, e1.ConfigureAbandon(rec.abandon))
	e1.handleFailure(context.Background(), task, fmt.Errorf("boom"))
	// "Process exit": the sleeping in-memory retry goroutine dies with it.
	e1.Stop()

	// Fresh process over the same SQLite queue.
	var uploads atomic.Int32
	e2 := New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		uploads.Add(1)
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}, zap.NewNop(), 0)
	require.NoError(t, e2.ConfigureAbandon(rec.abandon))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e2.Start(ctx)

	require.Eventually(t, func() bool { return uploads.Load() >= 1 },
		3*time.Second, 20*time.Millisecond,
		"after a restart the failed task must be recovered into the queue and re-attempted")
	assert.Zero(t, rec.calls.Load(),
		"recovery resumes the task's upload — it must not abort it")
	e2.Stop()

	// The upload ID must have survived for the resume: the stub uploader
	// completes the attempt, the result is persisted (reported, awaiting CP
	// ack), and the row still carries the original upload id.
	rows, err := q.ListByStatus(queue.StatusReported)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "upload-live-1", rows[0].UploadID,
		"recovery must not clear the persisted upload id; the retry resumes it")
}

// A failed task whose persisted retry schedule is still in the future must
// stay failed until due — recovery must not re-queue it early — and then be
// flipped exactly once the schedule passes.
func TestExecutor_RecoveryHonoursPersistedRetrySchedule(t *testing.T) {
	q := newTestQueue(t)
	task := abandonTask(t, q) // running, upload-live-1
	// next_retry_at persists at second granularity; use a clearly-future
	// schedule so the window cannot truncate away.
	require.NoError(t, q.MarkFailed(context.Background(), task.ID, "boom", time.Now().Add(2*time.Second)))

	e := New(1, q, successUploader, zap.NewNop(), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	e.Start(ctx)

	// Still within the backoff window: must remain failed.
	time.Sleep(300 * time.Millisecond)
	failed, err := q.ListByStatus(queue.StatusFailed)
	require.NoError(t, err)
	require.Len(t, failed, 1, "a task inside its persisted backoff window must not be re-queued early")

	// Once the schedule passes, recovery's armed timer flips it — the pending
	// window may be brief (a worker dequeues immediately), so accept any
	// post-failed state.
	require.Eventually(t, func() bool {
		for _, st := range []string{queue.StatusPending, queue.StatusRunning, queue.StatusReported, queue.StatusCompleted} {
			rows, err := q.ListByStatus(st)
			if err == nil && len(rows) > 0 {
				return true
			}
		}
		return false
	}, 5*time.Second, 20*time.Millisecond, "the persisted schedule must be honoured and then fire")
	e.Stop()
}

// A task abandoned before the restart (retry budget exhausted or terminal
// failure — stamped with the never-retry sentinel) must never be resurrected
// by startup recovery: re-running it would loop forever on a verdict that will
// not change, and its upload was already aborted at abandonment time.
func TestExecutor_RecoveryNeverResurrectsAbandonedTasks(t *testing.T) {
	q := newTestQueue(t)
	task := abandonTask(t, q)
	// An abandoned task carries the never-retry sentinel, stamped by
	// handleFailure's give-up/terminal branches (persistResult then flips it
	// to reported; here we keep it in failed to test the sentinel itself).
	require.NoError(t, q.MarkFailed(context.Background(), task.ID, "permanent failure", time.Time{}))
	require.NoError(t, q.UpsertProcessedFile(&queue.ProcessedFile{
		ID: "pf-" + task.ID, RuleID: task.RuleID, LocalPath: task.LocalPath,
		FileSize: task.FileSize, FileMtime: task.FileMtime, UploadedAt: time.Now().Unix(),
	}))

	// "Restart".
	var uploads atomic.Int32
	e2 := New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		uploads.Add(1)
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}, zap.NewNop(), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e2.Start(ctx)

	assert.Never(t, func() bool { return uploads.Load() > 0 },
		600*time.Millisecond, 50*time.Millisecond,
		"an abandoned task must not be resurrected by startup recovery")
	failed, err := q.ListByStatus(queue.StatusFailed)
	require.NoError(t, err)
	assert.Len(t, failed, 1, "the abandoned task stays failed")
	e2.Stop()
}

// R1: a legacy row written before the retry schedule was persisted
// (next_retry_at = 0, the ALTER TABLE default) is UNDECIDABLE — it can be a
// task abandoned on a terminal verdict by an older build (low retry count, so
// the retry-count guard does not catch it) just as well as one awaiting
// backoff. Recovery must treat it conservatively: NOT resurrect it (re-running
// an abandoned upload would be wrong), and durably record an abort intent for
// its in-flight upload so it cannot leak either.
func TestExecutor_RecoveryTreatsLegacyFailedRowsAsAbandoned(t *testing.T) {
	q, dsn := newFileQueue(t)

	task := abandonTask(t, q) // running, upload-live-1
	require.NoError(t, q.MarkFailed(context.Background(), task.ID, "boom", time.Now().Add(time.Minute)))
	// Rewrite the row into the exact shape an older build left behind:
	// failed, no persisted schedule, LOW retry count.
	db, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(t, err)
	_, err = db.Exec(`UPDATE upload_tasks SET next_retry_at=0, retry_count=1 WHERE id=?`, task.ID)
	require.NoError(t, err)
	require.NoError(t, db.Close())

	// "Restart" with a failing abort hook (MinIO unreachable) so the abort
	// intent stays visible in the outbox instead of being drained away.
	e := New(1, q, successUploader, zap.NewNop(), 0)
	e.abortPoll = 20 * time.Millisecond
	e.retryDelays = []time.Duration{time.Hour}
	require.NoError(t, e.ConfigureAbandon(func(_ context.Context, _ *queue.UploadTask) error {
		return errors.New("minio unreachable")
	}))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Start(ctx)

	assert.Never(t, func() bool {
		failed, err := q.ListByStatus(queue.StatusFailed)
		return err == nil && len(failed) == 0
	}, 600*time.Millisecond, 50*time.Millisecond,
		"a legacy failed row must never leave failed: it may be an abandoned task")
	failed, err := q.ListByStatus(queue.StatusFailed)
	require.NoError(t, err)
	require.Len(t, failed, 1, "the legacy row stays failed (re-collection goes through the scan)")
	entries, err := q.DueMultipartAborts(context.Background(), time.Now().Add(time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the legacy row's in-flight upload must get a durable abort intent")
	assert.Equal(t, "upload-live-1", entries[0].UploadID)
	e.Stop()
}

func TestExecutor_RetryDelay(t *testing.T) {
	e := New(1, nil, nil, zap.NewNop(), 0)
	assert.Equal(t, 1*time.Minute, e.retryDelay(1))
	assert.Equal(t, 5*time.Minute, e.retryDelay(2))
	assert.Equal(t, 15*time.Minute, e.retryDelay(3))
	assert.Equal(t, 60*time.Minute, e.retryDelay(4))
	assert.Equal(t, 60*time.Minute, e.retryDelay(10), "should be capped at last delay")
}

func TestExecutor_GivenUpAfterMaxRetries(t *testing.T) {
	q := newTestQueue(t)

	// Inject a task that has already been retried maxRetries-1 times.
	task := newTask("r1", "/path/old.txt")
	task.RetryCount = maxRetries - 1
	require.NoError(t, q.Enqueue(task))

	// Mark it running so we can control state manually.
	tasks, err := q.DequeuePending(1)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	runningTask := tasks[0]
	runningTask.RetryCount = maxRetries - 1

	var requeued atomic.Bool
	e := New(1, q, failUploader, zap.NewNop(), 0)
	// Directly call handleFailure — at maxRetries, it should not re-queue.
	e.handleFailure(context.Background(), runningTask, fmt.Errorf("permanent error"))

	time.Sleep(100 * time.Millisecond)
	// Task should remain failed and not be re-queued.
	failed, err := q.ListByStatus(queue.StatusReported)
	require.NoError(t, err)
	assert.Len(t, failed, 1)
	assert.False(t, requeued.Load())
}

func TestExecutor_ConcurrentWorkers(t *testing.T) {
	q := newTestQueue(t)
	var processed atomic.Int32
	uploader := func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		time.Sleep(20 * time.Millisecond)
		processed.Add(1)
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}

	e := New(4, q, uploader, zap.NewNop(), 0)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.Start(ctx)

	const n = 8
	for i := 0; i < n; i++ {
		task := newTask("r1", fmt.Sprintf("/path/file%d.txt", i))
		require.NoError(t, e.Submit(context.Background(), task))
	}

	require.Eventually(t, func() bool {
		return processed.Load() == int32(n)
	}, 5*time.Second, 50*time.Millisecond, "all tasks should be processed")

	e.Stop()
}

func TestExecutor_GracefulShutdown(t *testing.T) {
	q := newTestQueue(t)
	started := make(chan struct{})
	done := make(chan struct{})

	uploader := func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		close(started)
		time.Sleep(100 * time.Millisecond)
		close(done)
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}

	e := New(1, q, uploader, zap.NewNop(), 0)
	ctx := context.Background()
	e.Start(ctx)

	task := newTask("r1", "/path/slow.txt")
	require.NoError(t, e.Submit(context.Background(), task))
	<-started // wait until worker started processing

	stopDone := make(chan struct{})
	go func() {
		e.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		// Stop may return before or after the task finishes — both are valid.
	case <-time.After(3 * time.Second):
		t.Fatal("Stop did not return in time")
	}
}

func TestExecutor_SubmitAssignsID(t *testing.T) {
	q := newTestQueue(t)
	e := New(0, q, successUploader, zap.NewNop(), 0)

	task := &queue.UploadTask{
		RuleID:      "r1",
		LocalPath:   "/path/file.txt",
		StoragePath: "key",
		Bucket:      "bucket",
	}
	require.NoError(t, e.Submit(context.Background(), task))
	assert.NotEmpty(t, task.ID)
}

func TestExecutor_SubmitSkipsOnlySameActiveVersion(t *testing.T) {
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 0)
	first := newTask("r1", "/path/active.txt")
	require.NoError(t, e.Submit(context.Background(), first))

	duplicate := newTask("r1", first.LocalPath)
	duplicate.FileMtime = first.FileMtime
	duplicate.FileSize = first.FileSize
	require.NoError(t, e.Submit(context.Background(), duplicate))
	pending, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	assert.Len(t, pending, 1)

	modified := newTask("r1", first.LocalPath)
	modified.FileMtime = first.FileMtime + 1
	modified.FileSize = first.FileSize + 1
	require.NoError(t, e.Submit(context.Background(), modified))
	pending, err = q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	assert.Len(t, pending, 2)
}

func TestExecutor_RetryRequeues(t *testing.T) {
	q := newTestQueue(t)

	var callCount atomic.Int32
	uploader := func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		n := callCount.Add(1)
		if n < 2 {
			return nil, fmt.Errorf("transient error")
		}
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "test-bucket", SHA256: "sha", SizeBytes: 100}, nil
	}

	e := New(1, q, uploader, zap.NewNop(), 0)
	// Use short delays so the test does not take minutes.
	e.retryDelays = []time.Duration{50 * time.Millisecond, 50 * time.Millisecond}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Start(ctx)

	task := newTask("r1", "/path/retry.txt")
	require.NoError(t, e.Submit(context.Background(), task))

	require.Eventually(t, func() bool {
		return callCount.Load() >= 2
	}, 3*time.Second, 50*time.Millisecond, "task should be retried")

	e.Stop()
}

// taskWithTime builds a pending task with an explicit created_at for
// deterministic eviction ordering.
func taskWithTime(id, path string, createdAt int64) *queue.UploadTask {
	tk := newTask("r1", path)
	tk.ID = id
	tk.CreatedAt = createdAt
	tk.UpdatedAt = createdAt
	return tk
}

func TestExecutor_Submit_EnforcesQueueMaxSize(t *testing.T) {
	q := newTestQueue(t)
	// cap of 3, no workers started so tasks stay pending.
	e := New(1, q, successUploader, zap.NewNop(), 3)

	require.NoError(t, e.Submit(context.Background(), taskWithTime("t1", "/f1", 1)))
	require.NoError(t, e.Submit(context.Background(), taskWithTime("t2", "/f2", 2)))
	require.NoError(t, e.Submit(context.Background(), taskWithTime("t3", "/f3", 3)))

	// Fourth submit is at cap → oldest pending (t1) must be evicted.
	require.NoError(t, e.Submit(context.Background(), taskWithTime("t4", "/f4", 4)))

	pending, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	assert.Len(t, pending, 3)

	ids := map[string]bool{}
	for _, p := range pending {
		ids[p.ID] = true
	}
	assert.False(t, ids["t1"], "oldest task t1 should have been evicted")
	assert.True(t, ids["t2"] && ids["t3"] && ids["t4"])
}

func TestExecutor_Submit_UnlimitedWhenCapZero(t *testing.T) {
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 0) // cap disabled

	for i := 0; i < 5; i++ {
		require.NoError(t, e.Submit(context.Background(), taskWithTime(
			uuid.New().String(), fmt.Sprintf("/f%d", i+1), int64(i+1))))
	}

	pending, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	assert.Len(t, pending, 5)
}

func TestExecutor_Submit_AtCapButNothingPendingToEvict(t *testing.T) {
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 2)

	// Fill to cap, then move both to running so no pending task can be evicted.
	require.NoError(t, e.Submit(context.Background(), taskWithTime("t1", "/f1", 1)))
	require.NoError(t, e.Submit(context.Background(), taskWithTime("t2", "/f2", 2)))
	running, err := q.DequeuePending(10)
	require.NoError(t, err)
	require.Len(t, running, 2)

	// At cap (2 active, both running) → eviction finds nothing; task is accepted anyway.
	require.NoError(t, e.Submit(context.Background(), taskWithTime("t3", "/f3", 3)))

	n, err := q.CountActive()
	require.NoError(t, err)
	assert.Equal(t, 3, n) // exceeded cap because nothing was evictable
}

// Enforcement runs after the enqueue, so the just-submitted task is never the one
// dropped: at cap it sheds older backlog and keeps the fresh file.
func TestExecutor_Submit_KeepsNewTaskEvictsOld(t *testing.T) {
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 1) // cap of 1

	require.NoError(t, e.Submit(context.Background(), taskWithTime("old", "/old", 1)))
	require.NoError(t, e.Submit(context.Background(), taskWithTime("new", "/new", 2)))

	pending, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "new", pending[0].ID, "the freshly submitted task must survive; the older one is evicted")
}

// When the new task is the only evictable one (all others in-flight), it is kept
// rather than immediately dropped — collection is never starved by its own submit.
func TestExecutor_Submit_NeverEvictsOnlyTheNewTask(t *testing.T) {
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 1) // cap of 1

	// Occupy the cap with a running (in-flight, non-evictable) task.
	require.NoError(t, e.Submit(context.Background(), taskWithTime("running", "/r", 1)))
	running, err := q.DequeuePending(10)
	require.NoError(t, err)
	require.Len(t, running, 1)

	require.NoError(t, e.Submit(context.Background(), taskWithTime("new", "/new", 2)))

	pending, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "new", pending[0].ID, "new task must not evict itself when it is the only evictable candidate")
}

// When a failed task is evicted (queue_max_size) while its retry goroutine is
// still sleeping, the re-queue on wake finds the row gone. That is expected, so
// it must log at debug ("retry skipped") — not warn ("re-queue failed").
func TestExecutor_RetryOfEvictedTaskLogsNoWarning(t *testing.T) {
	q := newTestQueue(t)
	core, logs := observer.New(zapcore.DebugLevel)
	e := New(1, q, failUploader, zap.New(core), 0) // cap disabled; we evict manually
	e.retryDelays = []time.Duration{50 * time.Millisecond}

	// Enqueue a task, then drive it through a failure so a retry is scheduled.
	task := newTask("r1", "/f1")
	require.NoError(t, q.Enqueue(task))
	e.handleFailure(context.Background(), task, fmt.Errorf("boom"))

	// Evict the (now failed) task before the retry goroutine wakes.
	dropped, err := q.DeleteOldestEvictable("")
	require.NoError(t, err)
	require.NotNil(t, dropped)

	// On wake the retry finds nothing to re-queue and logs a benign debug line.
	require.Eventually(t, func() bool {
		return logs.FilterMessage("executor: retry skipped, task was evicted").Len() == 1
	}, 2*time.Second, 10*time.Millisecond)
	assert.Zero(t, logs.FilterMessage("executor: re-queue failed").Len(),
		"eviction is expected; must not surface as a warning")
}
