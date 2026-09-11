package executor

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	uploadpkg "github.com/byw-dev/fileagent/agent/internal/uploader"
	agentv1 "github.com/byw-dev/fileagent/api/v1"
	_ "github.com/mattn/go-sqlite3"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"google.golang.org/protobuf/proto"
)

// corruptReport is a payload that proto.Unmarshal always rejects (a truncated
// varint tag). It stands in for a report row damaged on disk or written by an
// incompatible build.
var corruptReport = []byte{0xff, 0xff, 0xff}

// newFileQueue opens a file-backed queue (instead of the shared ":memory:"
// helper) so a second connection can reach the same database — see
// failStatement.
func newFileQueue(t *testing.T) (*queue.Queue, string) {
	t.Helper()
	dsn := filepath.Join(t.TempDir(), "queue.db")
	q, err := queue.Open(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Close() })
	return q, dsn
}

// failStatement makes exactly one kind of statement fail on the queue database
// by installing an aborting trigger over a second connection. Executor holds a
// concrete *queue.Queue, so no mock can be placed between it and SQLite; this
// is how a single queue call is failed while its neighbours keep working
// (closing the database, the blunt alternative, fails the earlier calls first
// and never reaches the branch under test).
func failStatement(t *testing.T, dsn, name, event, table string) {
	t.Helper()
	db, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(fmt.Sprintf(
		`CREATE TRIGGER %s BEFORE %s ON %s BEGIN SELECT RAISE(ABORT, 'injected failure'); END;`,
		name, event, table))
	require.NoError(t, err)
}

// saveCorruptReport persists an undecodable report for task, leaving the task
// in status "reported" exactly as a real result would.
func saveCorruptReport(t *testing.T, q *queue.Queue, task *queue.UploadTask) {
	t.Helper()
	require.NoError(t, q.Enqueue(task))
	running, err := q.DequeuePending(1)
	require.NoError(t, err)
	require.Len(t, running, 1)
	require.Equal(t, task.ID, running[0].ID)
	require.NoError(t, q.SaveReport(context.Background(), running[0].ID, corruptReport))
}

// mustFinish fails the test if fn has not returned within d — used where the
// bug guarded against is an unbounded loop rather than a wrong value.
func mustFinish(t *testing.T, d time.Duration, msg string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { fn(); close(done) }()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatal(msg)
	}
}

// An Enqueue failure must surface to the caller: the watcher relies on the
// error to retry collection instead of treating the file as accepted.
func TestExecutor_SubmitPropagatesEnqueueError(t *testing.T) {
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 0)
	require.NoError(t, q.Close())

	err := e.Submit(context.Background(), newTask("r1", "/f.txt"))
	require.Error(t, err)
	assert.Contains(t, err.Error(), "executor: enqueue task")
}

// A capacity-check DB failure must warn and abandon enforcement. Without the
// return the eviction loop would spin forever on the broken count, so this is
// asserted with a deadline rather than by return value alone.
func TestExecutor_EnforceCapacityCountErrorWarns(t *testing.T) {
	q := newTestQueue(t)
	core, logs := observer.New(zapcore.InfoLevel)
	e := New(1, q, successUploader, zap.New(core), 3)
	require.NoError(t, q.Close())

	mustFinish(t, 3*time.Second, "enforceCapacity must give up when the count fails, not loop", func() {
		e.enforceCapacity("any")
	})
	assert.NotZero(t, logs.FilterMessage("executor: queue capacity check failed").Len())
}

// A dequeue failure must abort the drain loop: without the return the worker
// would spin forever on a broken DB (this test would hang).
func TestExecutor_DrainQueueStopsOnDequeueError(t *testing.T) {
	q := newTestQueue(t)
	core, logs := observer.New(zapcore.InfoLevel)
	e := New(1, q, successUploader, zap.New(core), 0)
	require.NoError(t, q.Close())

	mustFinish(t, 3*time.Second, "drainQueue must return when dequeue fails", func() {
		e.drainQueue(context.Background())
	})
	assert.NotZero(t, logs.FilterMessage("executor: dequeue error").Len())
}

// A dedup-check failure must not drop the task: the upload still runs, because
// the worst outcome of a broken dedup lookup is a duplicate upload, not a lost
// file.
func TestExecutor_DedupCheckErrorStillUploads(t *testing.T) {
	q := newTestQueue(t)
	core, logs := observer.New(zapcore.InfoLevel)
	var uploaded atomic.Bool
	e := New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		uploaded.Store(true)
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "b", SHA256: "sha", SizeBytes: 1}, nil
	}, zap.New(core), 0)

	task := newTask("r1", "/f.txt")
	require.NoError(t, q.Enqueue(task))
	running, err := q.DequeuePending(1)
	require.NoError(t, err)
	require.Len(t, running, 1)
	require.NoError(t, q.Close())

	e.processTask(context.Background(), running[0])
	assert.True(t, uploaded.Load(), "a dedup lookup failure must not skip the upload")
	assert.NotZero(t, logs.FilterMessage("executor: dedup check error").Len())
}

// An uploader returning (nil, nil) is a bug, not a success: it must be reported
// as a failed upload (success=false) rather than crashing the worker or being
// silently dropped.
func TestExecutor_NilUploaderResultReportedAsFailure(t *testing.T) {
	q := newTestQueue(t)
	e := New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		return nil, nil
	}, zap.NewNop(), 0)

	task := newTask("r1", "/nil.txt")
	task.RetryCount = maxRetries - 1 // exhaust retries so the failure is final
	require.NoError(t, q.Enqueue(task))
	running, err := q.DequeuePending(1)
	require.NoError(t, err)
	require.Len(t, running, 1)

	e.processTask(context.Background(), running[0])

	payload, err := q.GetReport(context.Background(), task.ID)
	require.NoError(t, err)
	var result agentv1.UploadResult
	require.NoError(t, proto.Unmarshal(payload, &result))
	assert.False(t, result.Success)
	assert.Equal(t, "uploader returned nil result", result.ErrorMessage)
}

// Stop must cancel retries still sleeping in backoff: none may re-queue after
// shutdown (an agent restart rescans pending work anyway).
func TestExecutor_RetryCancelledByStop(t *testing.T) {
	q := newTestQueue(t)
	e := New(1, q, failUploader, zap.NewNop(), 0)
	e.retryDelays = []time.Duration{time.Hour}

	task := newTask("r1", "/slow-retry.txt")
	require.NoError(t, q.Enqueue(task))
	running, err := q.DequeuePending(1)
	require.NoError(t, err)
	require.Len(t, running, 1)
	e.handleFailure(running[0], fmt.Errorf("boom"))

	stopDone := make(chan struct{})
	go func() { e.Stop(); close(stopDone) }()
	select {
	case <-stopDone:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop blocked on a sleeping retry")
	}

	pending, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	assert.Empty(t, pending, "a cancelled retry must not re-queue")
	failed, err := q.ListByStatus(queue.StatusFailed)
	require.NoError(t, err)
	assert.Len(t, failed, 1)
}

// A re-queue failure other than eviction (e.g. the DB going away under us)
// must surface as a warning so the dropped retry is visible in logs.
func TestExecutor_RequeueFailureAfterCloseWarns(t *testing.T) {
	q := newTestQueue(t)
	core, logs := observer.New(zapcore.InfoLevel)
	e := New(1, q, failUploader, zap.New(core), 0)
	e.retryDelays = []time.Duration{50 * time.Millisecond}

	task := newTask("r1", "/f.txt")
	require.NoError(t, q.Enqueue(task))
	running, err := q.DequeuePending(1)
	require.NoError(t, err)
	require.Len(t, running, 1)
	e.handleFailure(running[0], fmt.Errorf("boom"))

	require.NoError(t, q.Close()) // DB goes away before the retry wakes

	require.Eventually(t, func() bool {
		return logs.FilterMessage("executor: re-queue failed").Len() == 1
	}, 2*time.Second, 10*time.Millisecond)
	assert.Zero(t, logs.FilterMessage("executor: retry skipped, task was evicted").Len(),
		"a closed DB is not an eviction and must not be logged as one")
}

// Both the reporter and the workers must exit on root-context cancellation
// alone — the agent's shutdown path — not only via Stop.
func TestExecutor_GoroutinesExitOnContextCancel(t *testing.T) {
	q := newTestQueue(t)
	e := New(2, q, successUploader, zap.NewNop(), 0)
	require.NoError(t, e.ConfigureReporting(func(_ *agentv1.AgentMessage) error {
		return assert.AnError
	}, 10*time.Millisecond, 1))

	ctx, cancel := context.WithCancel(context.Background())
	e.Start(ctx)
	cancel()

	done := make(chan struct{})
	go func() { e.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("reporter/workers did not exit after context cancel")
	}
	e.Stop()
}

// The 5s periodic drain is the safety net for tasks that enter the queue
// without a notify signal (e.g. a re-queue racing the worker's select): the
// worker must still pick the task up.
func TestExecutor_PeriodicDrainPicksUpUnnotifiedTask(t *testing.T) {
	q := newTestQueue(t)
	var processed atomic.Int32
	e := New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		processed.Add(1)
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "b", SHA256: "sha", SizeBytes: 1}, nil
	}, zap.NewNop(), 0)

	// Enqueue directly (no Submit → no notify signal is ever sent).
	require.NoError(t, q.Enqueue(newTask("r1", "/unnotified.txt")))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.Start(ctx)

	require.Eventually(t, func() bool { return processed.Load() == 1 },
		8*time.Second, 50*time.Millisecond, "periodic drain must process the unnotified task")
	e.Stop()
}

// A failure resetting report timers at startup must be logged without
// preventing the reporter and workers from starting.
func TestExecutor_StartLogsReportTimerResetError(t *testing.T) {
	q := newTestQueue(t)
	core, logs := observer.New(zapcore.InfoLevel)
	e := New(1, q, successUploader, zap.New(core), 0)
	require.NoError(t, e.ConfigureReporting(func(_ *agentv1.AgentMessage) error {
		return assert.AnError
	}, 10*time.Millisecond, 1))
	require.NoError(t, q.Close())

	e.Start(context.Background())
	e.Stop()
	assert.NotZero(t, logs.FilterMessage("executor: reset report timers").Len())
}

// An eviction that fails against the database must warn and give up, keeping
// the just-submitted task: the queue stays slightly over cap, which is the same
// concession made when nothing is evictable. Without the return, enforceCapacity
// would retry the failing DELETE forever and Submit would never come back.
func TestExecutor_EvictionFailureWarnsAndKeepsTask(t *testing.T) {
	q, dsn := newFileQueue(t)
	core, logs := observer.New(zapcore.InfoLevel)
	e := New(1, q, successUploader, zap.New(core), 1) // cap of 1

	require.NoError(t, e.Submit(context.Background(), taskWithTime("old", "/old", 1)))
	// Only the DELETE fails; the capacity count that precedes it still works,
	// which is exactly the state this branch exists for.
	failStatement(t, dsn, "no_evict", "DELETE", "upload_tasks")

	errCh := make(chan error, 1)
	go func() { errCh <- e.Submit(context.Background(), taskWithTime("new", "/new", 2)) }()
	select {
	case err := <-errCh:
		require.NoError(t, err, "a failed eviction must not fail the submit")
	case <-time.After(3 * time.Second):
		t.Fatal("enforceCapacity must give up when eviction fails, not retry forever")
	}

	assert.NotZero(t, logs.FilterMessage("executor: queue eviction failed").Len())
	assert.Zero(t, logs.FilterMessage("executor: queue full, dropped oldest task").Len(),
		"nothing was dropped, so nothing may be logged as dropped")
	pending, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	assert.Len(t, pending, 2, "both tasks must survive a failed eviction")
}

// If the ack deadline cannot be stamped, the report must not be sent. Sending
// first would let a CP ack complete a report whose retry timer still reads
// "never sent", so a lost ack would re-send it in a hot loop.
func TestExecutor_ReportTimerFailureSkipsSend(t *testing.T) {
	ctx := context.Background()
	q, dsn := newFileQueue(t)
	core, logs := observer.New(zapcore.InfoLevel)
	var sends atomic.Int32
	e := New(1, q, successUploader, zap.New(core), 0)
	require.NoError(t, e.ConfigureReporting(func(*agentv1.AgentMessage) error {
		sends.Add(1)
		return nil
	}, 10*time.Millisecond, 1))

	task := newTask("r1", "/f.txt")
	require.NoError(t, q.Enqueue(task))
	running, err := q.DequeuePending(1)
	require.NoError(t, err)
	require.Len(t, running, 1)
	e.persistResult(ctx, running[0], &uploadpkg.UploadResult{
		StoragePath: "bucket/key", Bucket: "b", SHA256: "sha", SizeBytes: 1,
	}, nil)

	// Installed after the report exists, so only the timer UPDATE fails while
	// the listing SELECT that precedes it still returns the report.
	failStatement(t, dsn, "no_touch", "UPDATE", "upload_reports")

	e.sendDueReports(ctx)

	assert.Zero(t, sends.Load(), "a report whose deadline could not be stamped must not be sent")
	assert.NotZero(t, logs.FilterMessage("executor: report timer").Len())
}

// A report that cannot be decoded is skipped, not fatal: the rest of the outbox
// must still drain, otherwise one damaged row would block every later upload
// from ever being reported.
func TestExecutor_UndecodableReportSkippedRestStillSent(t *testing.T) {
	ctx := context.Background()
	q := newTestQueue(t)
	core, logs := observer.New(zapcore.InfoLevel)
	var sent []string
	e := New(1, q, successUploader, zap.New(core), 0)
	require.NoError(t, e.ConfigureReporting(func(m *agentv1.AgentMessage) error {
		sent = append(sent, m.GetMessageId())
		return nil
	}, 10*time.Millisecond, 1))

	// DueReports orders by (last_reported_at, task_id) and both rows are unsent,
	// so the corrupt id sorts first and would block the good one on a `return`.
	corrupt := newTask("r1", "/corrupt.txt")
	corrupt.ID = "aaaa-corrupt"
	corrupt.CreatedAt, corrupt.UpdatedAt = 1, 1
	saveCorruptReport(t, q, corrupt)

	good := newTask("r1", "/good.txt")
	good.ID = "bbbb-good"
	good.CreatedAt, good.UpdatedAt = 2, 2
	require.NoError(t, q.Enqueue(good))
	running, err := q.DequeuePending(1)
	require.NoError(t, err)
	require.Len(t, running, 1)
	e.persistResult(ctx, running[0], &uploadpkg.UploadResult{
		StoragePath: "bucket/key", Bucket: "b", SHA256: "sha", SizeBytes: 1,
	}, nil)

	e.sendDueReports(ctx)

	assert.Equal(t, []string{"bbbb-good"}, sent,
		"the corrupt report is dropped and the following one is still delivered")
	assert.NotZero(t, logs.FilterMessage("executor: decode persisted report").Len())
}

// An ack for a report that cannot be decoded must surface as an error and leave
// the task in "reported": completing it would mark the file done (and, for a
// success, write dedup state) from a payload nobody could read.
func TestExecutor_AcknowledgementOfUndecodableReportErrors(t *testing.T) {
	ctx := context.Background()
	q := newTestQueue(t)
	e := New(1, q, successUploader, zap.NewNop(), 0)

	task := newTask("r1", "/corrupt.txt")
	saveCorruptReport(t, q, task)

	err := e.HandleAcknowledgement(ctx, &agentv1.Acknowledgement{RefMessageId: task.ID, Success: true})
	require.Error(t, err)

	reported, err := q.ListByStatus(queue.StatusReported)
	require.NoError(t, err)
	assert.Len(t, reported, 1, "an undecodable report must stay reported, not be completed")
	processed, err := q.IsProcessed(context.Background(), task.RuleID, task.LocalPath, task.FileMtime, task.FileSize)
	require.NoError(t, err)
	assert.False(t, processed, "dedup state must not be written from an unreadable payload")
}
