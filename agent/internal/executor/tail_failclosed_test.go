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
func TestExecutor_TailModeTask_WithUploadID_RecordsIntentThenAborts(t *testing.T) {
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 0)

	var abandoned atomic.Int32
	abandonedUploadID := make(chan string, 1)
	require.NoError(t, e.ConfigureAbandon(func(_ context.Context, task *queue.UploadTask) error {
		abandoned.Add(1)
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
		due, err := q.DueMultipartAborts(context.Background(), time.Now(), 100)
		return err == nil && len(due) == 0
	}, 2*time.Second, 20*time.Millisecond,
		"the abort record must be drained after the successful abort")
	assert.Equal(t, int32(1), abandoned.Load(), "exactly one abort attempt is expected")
}
