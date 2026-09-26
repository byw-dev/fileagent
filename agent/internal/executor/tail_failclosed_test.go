package executor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	uploadpkg "github.com/byw-dev/fileagent/agent/internal/uploader"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// IC-BUG-46 fail-closed: an existing rule with append_mode=tail must fail on
// the upload path WITHOUT producing any object — the current tail upload path
// would replace the whole object with only the appended bytes (silent data
// loss). The failure must be terminal (retrying cannot help while the mode is
// unsupported) and must leave a success=false report so the failure is
// visible, not silent.
func TestExecutor_TailModeTask_FailsTerminal_NoUpload(t *testing.T) {
	q := newTestQueue(t)
	attempts := atomic.Int32{}
	e := New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		attempts.Add(1)
		return successUploader(context.Background(), nil)
	}, zap.NewNop(), 0)

	task := newTask("r1", "/data/app.log")
	task.AppendMode = "tail"
	require.NoError(t, e.Submit(context.Background(), task))

	e.Start(context.Background())
	defer e.Stop()

	// The upload function must never run: no object may be produced.
	require.Eventually(t, func() bool {
		reports, err := q.ListByStatus(queue.StatusReported)
		return err == nil && len(reports) == 1
	}, 2*time.Second, 20*time.Millisecond, "the tail task must fail with a persisted report")
	assert.Never(t, func() bool { return attempts.Load() > 0 },
		500*time.Millisecond, 50*time.Millisecond,
		"no upload attempt may happen for a tail task")
	// Terminal: no backoff resurrection.
	reported, err := q.ListByStatus(queue.StatusReported)
	require.NoError(t, err)
	require.Len(t, reported, 1)
	assert.Equal(t, int64(queue.NextRetryNever), reported[0].NextRetryAt,
		"an unsupported-mode failure must not be retried")
}

// IC-BUG-46 fail-closed + abort order law: a tail task that carries an
// in-flight multipart upload ID (a pre-block tail rule may have initiated
// one) must first durably record the abort intent (MarkFailedAbandoned's
// transactional outbox write) and only then abort — and after a successful
// abort the durable record must be gone again.
//
// The test is deliberately DETERMINISTIC: e.Start is never called, so the
// durable-abort worker (runAbortWorker) never runs. That matters — the abort
// worker performs an immediate initial drainAbortOutbox at startup (before
// its first ticker tick), so with Start a deleted direct fast path could
// still be masked whenever the initial drain happens to land after the
// outbox commit: the poller would run the hook, delete the record and
// satisfy every end-state assertion within the test window, wrongly
// approving a regression that turns the usually-instant cleanup into a
// worst-case 30s delay. Instead the test drives the exact production
// sequence synchronously on the test goroutine — processTask → tail gate →
// handleFailure (terminal) → MarkFailedAbandoned → abandonTaskUpload — so
// the ONLY possible abort caller is the direct fast path.
//
// What the assertions pin (and what they deliberately exclude):
//
//   - PINNED: the direct fast path aborted exactly once (hookCalls == 1).
//     Deterministic because no abort worker exists in this test to add a
//     legal second attempt.
//
//   - PINNED: the durable abort intent existed while that abort ran
//     (hookSawRecords == 1) — the order law. Enforced from inside the
//     abandon hook, synchronously on the test goroutine. The pre-rewrite
//     test was blind to the most damaging regression here: with only
//     end-state assertions, dropping the outbox write leaves everything
//     green, because "record drained at the end" is trivially true for a
//     record that never existed.
//
//   - PINNED: after the successful direct abort the durable record is gone
//     (CountMultipartAborts == 0 — stronger than a due-only check: a second
//     persistent record for ANY upload would keep the count above 0).
//
//   - EXCLUDED: the abort worker entirely. The production system as a whole
//     only guarantees AT-LEAST-ONCE abort (direct fast path racing the
//     outbox drain), so a second Abort call is legal and benign for data
//     correctness — AbandonUpload treats MinIO's NoSuchUpload (an
//     already-aborted/completed upload) as success (uploader.go). Note the
//     duplicate count is NOT structurally bounded: the outbox's unique key
//     caps record ROWS, not abort CALLS — if DeleteMultipartAbort fails
//     after a successful abort, the record stays and EVERY subsequent drain
//     round aborts again (TestExecutor_AbortRecordDeleteFailureWarns pins
//     that path's warning). Benign either way; just not "at most twice".
//     Those worker-side behaviours are covered by the abort-worker tests
//     (aborts_test.go), not by this one.
func TestExecutor_TailModeTask_WithUploadID_RecordsIntentThenAborts(t *testing.T) {
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 0)

	var hookCalls int
	hookSawRecords := -1
	var gotUploadID string
	require.NoError(t, e.ConfigureAbandon(func(_ context.Context, task *queue.UploadTask) error {
		hookCalls++
		n, err := q.CountMultipartAborts(context.Background())
		if err != nil {
			return err
		}
		if hookCalls == 1 {
			hookSawRecords = n
		}
		gotUploadID = task.UploadID
		return nil
	}))

	task := newTask("r1", "/data/app.log")
	task.AppendMode = "tail"
	task.UploadID = "in-flight-upload-id"
	task.Bucket = "test-bucket"
	task.StoragePath = "bucket/key"
	require.NoError(t, e.Submit(context.Background(), task))

	// The exact production sequence, synchronously: the tail gate refuses
	// the upload and handleFailure's terminal branch records the durable
	// abort intent (MarkFailedAbandoned, one transaction) BEFORE the direct
	// abort runs (abandonTaskUpload), which removes the record again on
	// success.
	e.processTask(context.Background(), task)

	assert.Equal(t, 1, hookCalls,
		"the direct fast path must have aborted exactly once (no abort worker is running to add a legal second attempt)")
	assert.Equal(t, "in-flight-upload-id", gotUploadID,
		"the abort must target the task's in-flight upload")
	assert.Equal(t, 1, hookSawRecords,
		"the abort must run under exactly one durable abort record (order law: intent committed before abort)")
	n, err := q.CountMultipartAborts(context.Background())
	require.NoError(t, err)
	assert.Equal(t, 0, n,
		"the durable record must be drained by the successful direct abort")
}
