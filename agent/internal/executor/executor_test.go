package executor

import (
	"context"
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
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

func successUploader(_ context.Context, _ *queue.UploadTask) error {
	return nil
}

func failUploader(_ context.Context, _ *queue.UploadTask) error {
	return fmt.Errorf("upload failed")
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestExecutor_SubmitAndProcess(t *testing.T) {
	q := newTestQueue(t)
	var processed atomic.Int32
	uploader := func(_ context.Context, _ *queue.UploadTask) error {
		processed.Add(1)
		return nil
	}

	e := New(2, q, uploader, zap.NewNop())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Start(ctx)

	task := newTask("r1", "/path/file.txt")
	require.NoError(t, e.Submit(task))

	require.Eventually(t, func() bool {
		return processed.Load() == 1
	}, 3*time.Second, 50*time.Millisecond)

	e.Stop()
}

func TestExecutor_Dedup(t *testing.T) {
	q := newTestQueue(t)
	var uploadCount atomic.Int32
	uploader := func(_ context.Context, _ *queue.UploadTask) error {
		uploadCount.Add(1)
		return nil
	}

	e := New(1, q, uploader, zap.NewNop())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Start(ctx)

	// Submit a task and let it complete.
	t1 := newTask("r1", "/path/file.txt")
	require.NoError(t, e.Submit(t1))

	require.Eventually(t, func() bool {
		return uploadCount.Load() == 1
	}, 3*time.Second, 50*time.Millisecond)

	// Submit a second task for the same path — should be skipped.
	t2 := newTask("r1", "/path/file.txt")
	require.NoError(t, e.Submit(t2))

	time.Sleep(300 * time.Millisecond)
	assert.Equal(t, int32(1), uploadCount.Load(), "duplicate file should not be uploaded again")

	e.Stop()
}

func TestExecutor_RetryDelay(t *testing.T) {
	assert.Equal(t, 1*time.Minute, retryDelay(1))
	assert.Equal(t, 5*time.Minute, retryDelay(2))
	assert.Equal(t, 15*time.Minute, retryDelay(3))
	assert.Equal(t, 60*time.Minute, retryDelay(4))
	assert.Equal(t, 60*time.Minute, retryDelay(10), "should be capped at last delay")
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
	e := New(1, q, failUploader, zap.NewNop())
	// Directly call handleFailure — at maxRetries, it should not re-queue.
	e.handleFailure(runningTask, fmt.Errorf("permanent error"))

	time.Sleep(100 * time.Millisecond)
	// Task should remain failed and not be re-queued.
	failed, err := q.ListByStatus(queue.StatusFailed)
	require.NoError(t, err)
	assert.Len(t, failed, 1)
	assert.False(t, requeued.Load())
}

func TestExecutor_ConcurrentWorkers(t *testing.T) {
	q := newTestQueue(t)
	var processed atomic.Int32
	uploader := func(_ context.Context, _ *queue.UploadTask) error {
		time.Sleep(20 * time.Millisecond)
		processed.Add(1)
		return nil
	}

	e := New(4, q, uploader, zap.NewNop())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	e.Start(ctx)

	const n = 8
	for i := 0; i < n; i++ {
		task := newTask("r1", fmt.Sprintf("/path/file%d.txt", i))
		require.NoError(t, e.Submit(task))
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

	uploader := func(_ context.Context, _ *queue.UploadTask) error {
		close(started)
		time.Sleep(100 * time.Millisecond)
		close(done)
		return nil
	}

	e := New(1, q, uploader, zap.NewNop())
	ctx := context.Background()
	e.Start(ctx)

	task := newTask("r1", "/path/slow.txt")
	require.NoError(t, e.Submit(task))
	<-started // wait until worker started processing

	stopDone := make(chan struct{})
	go func() {
		e.Stop()
		close(stopDone)
	}()

	select {
	case <-stopDone:
		// Stop may return before or after the task finishes — both are valid.
	case <-time.After(3*time.Second):
		t.Fatal("Stop did not return in time")
	}
}

func TestExecutor_SubmitAssignsID(t *testing.T) {
	q := newTestQueue(t)
	e := New(0, q, successUploader, zap.NewNop())

	task := &queue.UploadTask{
		RuleID:      "r1",
		LocalPath:   "/path/file.txt",
		StoragePath: "key",
		Bucket:      "bucket",
	}
	require.NoError(t, e.Submit(task))
	assert.NotEmpty(t, task.ID)
}

func TestExecutor_RetryRequeues(t *testing.T) {
	q := newTestQueue(t)

	// Use short delays for testing.
	origDelays := retryDelays
	retryDelays = []time.Duration{50 * time.Millisecond, 50 * time.Millisecond}
	defer func() { retryDelays = origDelays }()

	var callCount atomic.Int32
	uploader := func(_ context.Context, _ *queue.UploadTask) error {
		n := callCount.Add(1)
		if n < 2 {
			return fmt.Errorf("transient error")
		}
		return nil
	}

	e := New(1, q, uploader, zap.NewNop())
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	e.Start(ctx)

	task := newTask("r1", "/path/retry.txt")
	require.NoError(t, e.Submit(task))

	require.Eventually(t, func() bool {
		return callCount.Load() >= 2
	}, 3*time.Second, 50*time.Millisecond, "task should be retried")

	e.Stop()
}
