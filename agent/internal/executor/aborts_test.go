package executor

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// Durable multipart-abort cleanup (IC-3 ②/P2): a failed abort must keep a
// retryable local identity. There is NO reliable ILM backstop — current MinIO
// builds do not implement AbortIncompleteMultipartUpload (IC-3 ③) — so the
// durable outbox plus the background abort worker is the last line of defence
// against unbounded part leaks.

// switchableAbandon fails until switched, then succeeds.
type switchableAbandon struct {
	fail atomic.Bool
	err  error
}

func (s *switchableAbandon) abandon(ctx context.Context, task *queue.UploadTask) error {
	if s.fail.Load() {
		return s.err
	}
	return nil
}

// TestExecutor_EvictionLeavesDurableAbortRecord pins the outbox write: evicting
// a task whose multipart upload is in flight must leave a retryable abort
// identity in the durable outbox — transactionally with the delete — because
// the synchronous best-effort abort can fail (MinIO unreachable) and the
// deleted row can never provide the identity again.
func TestExecutor_EvictionLeavesDurableAbortRecord(t *testing.T) {
	q := newTestQueue(t)
	rec := &abandonRecorder{}

	// A failed task awaiting backoff carries a live upload ID; capacity
	// eviction drops it and the abort hook fails (MinIO unreachable).
	task := abandonTask(t, q)
	require.NoError(t, q.MarkFailed(context.Background(), task.ID, "boom", time.Time{}))

	e := New(1, q, successUploader, zap.NewNop(), 1) // cap of 1
	e.abortPoll = 20 * time.Millisecond
	require.NoError(t, e.ConfigureAbandon(func(_ context.Context, task *queue.UploadTask) error {
		rec.calls.Add(1)
		rec.lastUploadID.Store(task.UploadID)
		return errors.New("minio unreachable")
	}))

	require.NoError(t, e.Submit(context.Background(), newTask("r1", "/path/other.txt")))

require.Eventually(t, func() bool { return rec.calls.Load() >= 1 },
		2*time.Second, 20*time.Millisecond, "the direct best-effort abort must still be attempted")
	entries, err := q.DueMultipartAborts(context.Background(), time.Now().Add(time.Hour), 10)
	require.NoError(t, err)
	require.Len(t, entries, 1, "the durable abort record must survive the failed abort")
	assert.Equal(t, "upload-live-1", entries[0].UploadID)
	assert.Equal(t, task.Bucket, entries[0].Bucket)
	assert.Equal(t, task.StoragePath, entries[0].StoragePath)
}

// TestExecutor_DurableAbortRetriedUntilSuccess walks the retry loop end to
// end: the first attempts fail (MinIO unreachable), the record is rescheduled
// with backoff, and once MinIO recovers the record is drained and removed.
func TestExecutor_DurableAbortRetriedUntilSuccess(t *testing.T) {
	q := newTestQueue(t)
	hook := &switchableAbandon{err: errors.New("minio unreachable")}
	hook.fail.Store(true)
	var calls atomic.Int32
	ctx := context.Background()

	require.NoError(t, q.EnqueueMultipartAbort(ctx, &queue.AbortOutboxEntry{
		UploadID:    "upload-outbox-1",
		TaskID:      "task-1",
		Bucket:      "b",
		StoragePath: "k",
	}))

	e := New(1, q, successUploader, zap.NewNop(), 0)
	e.retryDelays = []time.Duration{30 * time.Millisecond}
	e.abortPoll = 20 * time.Millisecond
	e.abortTimeout = time.Second
	require.NoError(t, e.ConfigureAbandon(func(_ context.Context, task *queue.UploadTask) error {
		calls.Add(1)
		return hook.abandon(ctx, task)
	}))

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.Start(sctx)

	// Attempts fail while the hook fails; the record must survive and be
	// rescheduled.
	require.Eventually(t, func() bool {
		entries, err := q.DueMultipartAborts(ctx, time.Now(), 10)
		return err == nil && len(entries) == 1 && entries[0].Attempts >= 1
	}, 3*time.Second, 20*time.Millisecond, "failed aborts must bump the durable attempt counter")
	n, err := q.CountMultipartAborts(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n)

	// MinIO recovers: the record must drain to success and be removed.
	hook.fail.Store(false)
	require.Eventually(t, func() bool {
		n, err := q.CountMultipartAborts(ctx)
		return err == nil && n == 0
	}, 3*time.Second, 20*time.Millisecond, "a recovered abort must remove the durable record")
	e.Stop()
}

// TestExecutor_DurableAbortRespectsTimeout pins the timeout half of the P2
// fix: an abort hook that hangs must not occupy the worker forever — the
// attempt is bounded by the abort timeout and recorded as failed.
func TestExecutor_DurableAbortRespectsTimeout(t *testing.T) {
	q := newTestQueue(t)
	var calls atomic.Int32
	ctx := context.Background()

	require.NoError(t, q.EnqueueMultipartAbort(ctx, &queue.AbortOutboxEntry{
		UploadID:    "upload-slow-1",
		TaskID:      "task-1",
		Bucket:      "b",
		StoragePath: "k",
	}))

	core, logs := observer.New(zapcore.WarnLevel)
	e := New(1, q, successUploader, zap.New(core), 0)
	e.abortPoll = 20 * time.Millisecond
	e.abortTimeout = 60 * time.Millisecond
	e.retryDelays = []time.Duration{time.Hour}
	require.NoError(t, e.ConfigureAbandon(func(ctx context.Context, _ *queue.UploadTask) error {
		calls.Add(1)
		<-ctx.Done()
		return ctx.Err()
	}))

	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	e.Start(sctx)

	require.Eventually(t, func() bool { return calls.Load() >= 1 }, 3*time.Second, 20*time.Millisecond)
	// The timed-out attempt must be durably recorded (retryDelays=1h, so the
	// next attempt sits beyond now+1h; the query window is widened to see it).
	// The WARN log ("...scheduled for retry") is checked in the SAME
	// Eventually: in drainAbortOutbox the DB write (RecordMultipartAbortAttempt)
	// lands a few instructions BEFORE the log line on the abort-worker
	// goroutine, so the replaced immediate assert.NotZero raced that window
	// (the original flake at this site, ~5-10% red).
	require.Eventually(t, func() bool {
		entries, err := q.DueMultipartAborts(ctx, time.Now().Add(2*time.Hour), 10)
		return err == nil && len(entries) == 1 && entries[0].Attempts >= 1 &&
			strings.Contains(entries[0].LastError, "context deadline exceeded") &&
			logs.FilterMessage("executor: durable multipart abort failed, scheduled for retry").Len() > 0
	}, 3*time.Second, 20*time.Millisecond, "the timed-out attempt must be recorded and logged")
	e.Stop()
}

// TestExecutor_AbortLogNeverClaimsILMBackstop pins the P2 logging fix: the old
// message claimed a failed abort was handed to the bucket's ILM rule — a rule
// current MinIO builds do not implement (IC-3 ③). The message must instead
// point at the durable abort record that actually retries.
func TestExecutor_AbortLogNeverClaimsILMBackstop(t *testing.T) {
	q := newTestQueue(t)
	core, logs := observer.New(zapcore.WarnLevel)
	e := New(1, q, failUploader, zap.New(core), 0)
	e.abortTimeout = 50 * time.Millisecond

	task := abandonTask(t, q)
	task.RetryCount = maxRetries - 1
	require.NoError(t, e.ConfigureAbandon(func(_ context.Context, _ *queue.UploadTask) error {
		return errors.New("minio unreachable")
	}))

	e.handleFailure(context.Background(), task, errors.New("boom"))

	assert.Zero(t, logs.FilterMessage("leaving it to the bucket ILM rule").Len(),
		"the ILM claim is false on current MinIO builds and must not be logged")
	assert.NotZero(t, logs.FilterMessage("executor: abort of abandoned multipart upload failed; the durable abort record will be retried").Len())
}