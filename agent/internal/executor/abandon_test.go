package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/fileid"
	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// Terminal-state multipart cleanup (IC-3 ②): when a task is abandoned — retry
// budget exhausted, terminal upload failure, or eviction while a multipart
// upload is in flight — the recorded MinIO upload must be aborted, or the
// uploaded parts leak without bound (no ILM rule can see task identity).

// abandonRecorder captures AbandonFunc invocations.
type abandonRecorder struct {
	calls        atomic.Int32
	lastUploadID atomic.Value // string
}

func (a *abandonRecorder) abandon(_ context.Context, task *queue.UploadTask) error {
	a.calls.Add(1)
	a.lastUploadID.Store(task.UploadID)
	return nil
}

// newAbandonTask returns a dequeued (running) task that carries an in-flight
// multipart upload ID, as multipart.go would have persisted it.
func abandonTask(t *testing.T, q *queue.Queue) *queue.UploadTask {
	t.Helper()
	task := newTask("r1", "/path/big.dat")
	task.UploadID = "upload-live-1"
	task.CompletedParts = `{"parts":[{"PartNumber":1,"ETag":"e1"}]}`
	require.NoError(t, q.Enqueue(task))
	tasks, err := q.DequeuePending(1)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	return tasks[0]
}

// abandonTaskWithFile is abandonTask with a REAL file at task.LocalPath whose
// content hashes to "seeded-sha" — needed by dedup-shortcut tests under the
// IC-BUG-48 verdict flow: the hash-confirm stage actually reads the file, so
// the dedup-seeded row must be backed by real bytes (a nonexistent path makes
// the hash fail → Recollect → no shortcut to test).
func abandonTaskWithFile(t *testing.T, q *queue.Queue) *queue.UploadTask {
	t.Helper()
	path := filepath.Join(t.TempDir(), "big.dat")
	require.NoError(t, os.WriteFile(path, []byte("big"), 0o644))
	task := newTask("r1", path)
	task.UploadID = "upload-live-1"
	task.CompletedParts = `{"parts":[{"PartNumber":1,"ETag":"e1"}]}`
	require.NoError(t, q.Enqueue(task))
	tasks, err := q.DequeuePending(1)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	return tasks[0]
}

// seedDedupRow writes the processed-file row a completed "task B" would have
// left: full identity of the REAL file (so the prefilter sees a fully-known,
// fully-equal record) plus its sha256 and a snapshot anchor far enough in the
// past to be outside the racy window (anchor=2, stamps=current — the
// identity's stamps are the real file's; anchor 2 ns after epoch sorts
// strictly before them → not racy).
func seedDedupRow(t *testing.T, q *queue.Queue, task *queue.UploadTask) {
	t.Helper()
	id, err := fileid.Stat(task.LocalPath)
	require.NoError(t, err)
	hr, err := hashFileSHA256(task.LocalPath)
	require.NoError(t, err)
	require.NoError(t, q.UpsertProcessedFile(&queue.ProcessedFile{
		ID: "pf-" + task.ID, RuleID: task.RuleID, LocalPath: task.LocalPath,
		FileSize: task.FileSize, FileMtime: task.FileMtime,
		SHA256: hr.SHA256,
		Ident: queue.FileIdentity{
			MtimeNs: id.MtimeNs, CtimeNs: id.CtimeNs, Dev: id.Dev, Ino: id.Ino,
			Size: id.Size, SnapAtNs: 2,
		},
		UploadedAtNs: 2,
	}))
}

func TestExecutor_AbortsOnGiveUp(t *testing.T) {
	q := newTestQueue(t)
	rec := &abandonRecorder{}

	task := abandonTask(t, q)
	task.RetryCount = maxRetries - 1

	e := New(1, q, failUploader, zap.NewNop(), 0)
	require.NoError(t, e.ConfigureAbandon(rec.abandon))
	e.handleFailure(context.Background(), task, fmt.Errorf("boom"))
	task.RetryCount = maxRetries // handleFailure mutated its copy

	require.Equal(t, int32(1), rec.calls.Load(),
		"retry-exhausted give-up must abort the in-flight multipart upload")
	assert.Equal(t, "upload-live-1", rec.lastUploadID.Load())
}

func TestExecutor_AbortsOnTerminalFailure(t *testing.T) {
	q := newTestQueue(t)
	rec := &abandonRecorder{}

	task := abandonTask(t, q)
	e := New(1, q, failUploader, zap.NewNop(), 0)
	require.NoError(t, e.ConfigureAbandon(rec.abandon))
	e.handleFailure(context.Background(), task, fmt.Errorf("%w: still AccessDenied", ErrTerminalUpload))

	require.Equal(t, int32(1), rec.calls.Load(),
		"terminal upload failure must abort the in-flight multipart upload")
}

func TestExecutor_NoAbortWhileRetryScheduled(t *testing.T) {
	q := newTestQueue(t)
	rec := &abandonRecorder{}

	task := abandonTask(t, q)
	task.RetryCount = 0

	e := New(1, q, failUploader, zap.NewNop(), 0)
	e.retryDelays = []time.Duration{time.Hour} // keep the retry pending
	require.NoError(t, e.ConfigureAbandon(rec.abandon))
	e.handleFailure(context.Background(), task, fmt.Errorf("boom"))

	time.Sleep(100 * time.Millisecond)
	assert.Zero(t, rec.calls.Load(),
		"a failed task awaiting backoff retry must keep its upload for resume")
}

func TestExecutor_NoAbortOnSuccess(t *testing.T) {
	q := newTestQueue(t)
	rec := &abandonRecorder{}

	task := abandonTask(t, q)

	e := New(1, q, successUploader, zap.NewNop(), 0)
	require.NoError(t, e.ConfigureAbandon(rec.abandon))
	e.processTask(context.Background(), task)

	assert.Zero(t, rec.calls.Load(),
		"completed tasks must not abort: CompleteMultipartUpload already consumed the upload ID")
}

func TestExecutor_AbortsOnEviction(t *testing.T) {
	q := newTestQueue(t)
	rec := &abandonRecorder{}

	// A failed task awaiting backoff carries a live upload ID; capacity
	// eviction dropping it orphans that upload unless it is aborted.
	task := abandonTask(t, q)
	require.NoError(t, q.MarkFailed(context.Background(), task.ID, "boom", time.Time{}))

	e := New(1, q, successUploader, zap.NewNop(), 0)
	e.queueMaxSize = 1
	require.NoError(t, e.ConfigureAbandon(rec.abandon))

	fresh := newTask("r1", "/path/other.txt")
	require.NoError(t, e.Submit(context.Background(), fresh))

	require.Eventually(t, func() bool { return rec.calls.Load() >= 1 },
		2*time.Second, 20*time.Millisecond,
		"evicting a failed task with an in-flight upload must abort it")
	assert.Equal(t, "upload-live-1", rec.lastUploadID.Load())
}

func TestExecutor_AbortHookErrorIsNonFatal(t *testing.T) {
	q := newTestQueue(t)

	task := abandonTask(t, q)
	task.RetryCount = maxRetries - 1

	e := New(1, q, failUploader, zap.NewNop(), 0)
	require.NoError(t, e.ConfigureAbandon(func(_ context.Context, _ *queue.UploadTask) error {
		return errors.New("minio unreachable")
	}))
	// Give-up proceeds even when the abort fails: the durable abort record
	// keeps the identity retryable for the abort worker. There is NO ILM
	// fallback — current MinIO builds do not implement
	// AbortIncompleteMultipartUpload (IC-3 ③).
	assert.NotPanics(t, func() { e.handleFailure(context.Background(), task, fmt.Errorf("boom")) })
}

func TestExecutor_ConfigureAbandonRejectsNil(t *testing.T) {
	e := New(1, newTestQueue(t), successUploader, zap.NewNop(), 0)
	require.Error(t, e.ConfigureAbandon(nil))
}

// TestExecutor_DedupCompleteAbortsOrphanedUpload pins the P1-b fix: `completed`
// is NOT only reached via CompleteMultipartUpload. Task A uploads part of a
// multipart and fails; task B (the dedup guard does not block against failed
// rows) completes the same file version; A retries, hits the dedup check and is
// marked completed — while its upload ID still owns an in-flight MinIO upload
// that must be aborted, or it leaks without bound.
func TestExecutor_DedupCompleteAbortsOrphanedUpload(t *testing.T) {
	q := newTestQueue(t)
	rec := &abandonRecorder{}

	// Task A: partial multipart upload, then failed — the upload ID stays on
	// the row exactly as multipart.go persisted it. A REAL file backs the
	// task (IC-48: the dedup hash-confirm reads it).
	task := abandonTaskWithFile(t, q)
	require.NoError(t, q.MarkFailed(context.Background(), task.ID, "boom", time.Time{}))

	// Task B completed the same file version: dedup state exists (full
	// identity + hash of the real file), so A's processTask now takes the
	// dedup shortcut to completed.
	seedDedupRow(t, q, task)

	e := New(1, q, successUploader, zap.NewNop(), 0)
	require.NoError(t, e.ConfigureAbandon(rec.abandon))
	e.processTask(context.Background(), task)

	require.Equal(t, int32(1), rec.calls.Load(),
		"marking a dedup-duplicate completed must abort its still-live multipart upload")
	assert.Equal(t, "upload-live-1", rec.lastUploadID.Load())
}
