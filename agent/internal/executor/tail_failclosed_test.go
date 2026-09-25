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
// What the assertions pin (and what they deliberately give up):
//
//   - PINNED: the durable abort intent exists while the FIRST abort attempt
//     runs (hookSawRecordsAtFirstCall == 1). The old test was blind to the
//     most damaging regression here — dropping the outbox write would have
//     left every later assertion green, because "record drained at the end"
//     is trivially true for a record that never existed. The order law is
//     now enforced from inside the abandon hook: a synchronous outbox count
//     at first-call time is deterministic on both paths (direct: commit
//     strictly precedes abandonTaskUpload; poller: it only drains records
//     that exist).
//
//   - PINNED: the outbox is fully drained after the abort succeeds
//     (CountMultipartAborts == 0 — stronger than the old due-only check: a
//     second persistent record for ANY upload would keep the count above 0).
//     This also pins that the direct abort actually runs: without it the
//     record would only drain on the 30s abort poll, far outside the
//     Eventually window.
//
//   - PINNED: abort happens at least once (abandoned >= 1).
//
//   - GIVEN UP: "exactly one abort attempt" (the old ==1, observed red with
//     actual == 2 roughly once per 100 -race runs). The implementation only
//     guarantees AT-LEAST-ONCE abort: the abort poller (drainAbortOutbox) can
//     legally drain the due record inside the window between the durable
//     intent's commit and the direct abort's completion, producing a second
//     Abort call. That duplicate is benign by design, not a product bug:
//     the outbox is unique per upload_id (no second record can exist), the
//     first successful attempt deletes it (so the duplicate count is
//     structurally bounded at 2), and MinIO Abort is idempotent —
//     AbandonUpload tolerates NoSuchUpload as success (uploader.go), i.e. an
//     already-aborted upload confirms as aborted. A test pinning ==1 would
//     keep failing on a correct implementation; one pinning only >=1 alone
//     would pin nothing — the drained-outbox and first-call-record checks
//     above are what carry the regression net.
func TestExecutor_TailModeTask_WithUploadID_RecordsIntentThenAborts(t *testing.T) {
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 0)

	var abandoned atomic.Int32
	abandonedUploadID := make(chan string, 1)
	// Outbox size observed by the FIRST abort attempt (-1 = no call yet,
	// -2 = the count query failed). Exactly one writer wins the CAS, so the
	// value is the observation of whichever abort ran first.
	var hookSawRecordsAtFirstCall atomic.Int64
	hookSawRecordsAtFirstCall.Store(-1)
	require.NoError(t, e.ConfigureAbandon(func(_ context.Context, task *queue.UploadTask) error {
		abandoned.Add(1)
		n, err := q.CountMultipartAborts(context.Background())
		observed := int64(n)
		if err != nil {
			observed = -2
		}
		hookSawRecordsAtFirstCall.CompareAndSwap(-1, observed)
		select {
		case abandonedUploadID <- task.UploadID:
		default:
		}
		return nil
	}))

	task := newTask("r1", "/data/app.log")
	task.AppendMode = "tail"
	task.UploadID = "in-flight-upload-id"
	task.Bucket = "test-bucket"
	task.StoragePath = "bucket/key"
	require.NoError(t, e.Submit(context.Background(), task))

	e.Start(context.Background())
	defer e.Stop()

	select {
	case got := <-abandonedUploadID:
		assert.Equal(t, "in-flight-upload-id", got)
	case <-time.After(2 * time.Second):
		t.Fatal("the in-flight multipart upload was never abandoned")
	}
	// The durable intent must have been recorded BEFORE the abort (the abandon
	// hook only runs after MarkFailedAbandoned committed its outbox write) and
	// removed again after the successful direct abort.
	require.Eventually(t, func() bool {
		n, err := q.CountMultipartAborts(context.Background())
		return err == nil && n == 0
	}, 2*time.Second, 20*time.Millisecond,
		"the abort record must be drained after the successful abort")
	assert.GreaterOrEqual(t, abandoned.Load(), int32(1),
		"at least one abort attempt is expected (at-least-once: the abort poller may legally add one)")
	assert.Equal(t, int64(1), hookSawRecordsAtFirstCall.Load(),
		"the first abort attempt must run under exactly one durable abort record (order law: intent before abort)")
}
