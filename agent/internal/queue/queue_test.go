package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// openMemQueue returns a fresh in-memory queue for testing.
func openMemQueue(t *testing.T) *Queue {
	t.Helper()
	q, err := Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func newTask(id, status string) *UploadTask {
	now := time.Now().Unix()
	return &UploadTask{
		ID:          id,
		RuleID:      "rule-1",
		LocalPath:   fmt.Sprintf("/data/%s.txt", id),
		StoragePath: fmt.Sprintf("org/2024/%s.txt", id),
		Bucket:      "uploads",
		FileSize:    1024,
		FileMtime:   now,
		Status:      status,
		CreatedAt:   now,
		UpdatedAt:   now,
	}
}

// ── Enqueue / Dequeue ─────────────────────────────────────────────────────────

func TestEnqueueDequeue(t *testing.T) {
	q := openMemQueue(t)

	t1 := newTask("task-1", "")
	t2 := newTask("task-2", "")
	require.NoError(t, q.Enqueue(t1))
	require.NoError(t, q.Enqueue(t2))

	tasks, err := q.DequeuePending(10)
	require.NoError(t, err)
	assert.Len(t, tasks, 2)
	for _, task := range tasks {
		assert.Equal(t, StatusRunning, task.Status)
	}

	// Second dequeue should return nothing (already running).
	tasks2, err := q.DequeuePending(10)
	require.NoError(t, err)
	assert.Empty(t, tasks2)
}

func TestCountPending(t *testing.T) {
	q := openMemQueue(t)

	n, err := q.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 0, n)

	require.NoError(t, q.Enqueue(newTask("a", "")))
	require.NoError(t, q.Enqueue(newTask("b", "")))
	require.NoError(t, q.Enqueue(newTask("c", "")))

	n, err = q.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 3, n)

	// Dequeuing transitions tasks to running, so they no longer count as pending.
	_, err = q.DequeuePending(2)
	require.NoError(t, err)
	n, err = q.CountPending()
	require.NoError(t, err)
	assert.Equal(t, 1, n)
}

func TestDequeuePending_Limit(t *testing.T) {
	q := openMemQueue(t)
	for i := 0; i < 5; i++ {
		require.NoError(t, q.Enqueue(newTask(fmt.Sprintf("t%d", i), "")))
	}
	tasks, err := q.DequeuePending(3)
	require.NoError(t, err)
	assert.Len(t, tasks, 3)
}

func TestEnqueue_DuplicateID(t *testing.T) {
	q := openMemQueue(t)
	task := newTask("dup-id", "")
	require.NoError(t, q.Enqueue(task))
	err := q.Enqueue(task)
	require.Error(t, err) // PRIMARY KEY violation.
}

func TestEnqueueIfNoActive_UsesMetadataAndActiveStatuses(t *testing.T) {
	ctx := context.Background()
	for _, status := range []string{StatusPending, StatusRunning, StatusReported} {
		t.Run(status+" blocks same version", func(t *testing.T) {
			q := openMemQueue(t)
			existing := newTask("existing", "")
			require.NoError(t, q.Enqueue(existing))
			require.NoError(t, q.UpdateStatus(existing.ID, status))
			candidate := newTask("candidate", "")
			candidate.LocalPath = existing.LocalPath
			candidate.FileMtime = existing.FileMtime
			candidate.FileSize = existing.FileSize

			enqueued, err := q.EnqueueIfNoActive(ctx, candidate)
			require.NoError(t, err)
			assert.False(t, enqueued)
		})
	}

	for _, status := range []string{StatusFailed, StatusCompleted} {
		t.Run(status+" does not block retry", func(t *testing.T) {
			q := openMemQueue(t)
			existing := newTask("existing", "")
			require.NoError(t, q.Enqueue(existing))
			require.NoError(t, q.UpdateStatus(existing.ID, status))
			candidate := newTask("candidate", "")
			candidate.LocalPath = existing.LocalPath
			candidate.FileMtime = existing.FileMtime
			candidate.FileSize = existing.FileSize

			enqueued, err := q.EnqueueIfNoActive(ctx, candidate)
			require.NoError(t, err)
			assert.True(t, enqueued)
		})
	}

	t.Run("db error surfaces instead of looking like a duplicate", func(t *testing.T) {
		q := openMemQueue(t)
		require.NoError(t, q.db.Close())

		enqueued, err := q.EnqueueIfNoActive(ctx, newTask("candidate", ""))
		require.Error(t, err)
		assert.False(t, enqueued)
		assert.Contains(t, err.Error(), "enqueue task")
	})

	t.Run("changed metadata is a new version", func(t *testing.T) {
		q := openMemQueue(t)
		existing := newTask("existing", "")
		require.NoError(t, q.Enqueue(existing))
		candidate := newTask("candidate", "")
		candidate.LocalPath = existing.LocalPath
		candidate.FileMtime = existing.FileMtime + 1
		candidate.FileSize = existing.FileSize + 1

		enqueued, err := q.EnqueueIfNoActive(ctx, candidate)
		require.NoError(t, err)
		assert.True(t, enqueued)
	})
}

// countTupleRows counts upload_tasks rows sharing a four-tuple. Used to prove
// dedup behavior directly at the row level.
func countTupleRows(t *testing.T, q *Queue, ruleID, path string, mtime, size int64) int {
	t.Helper()
	var n int
	err := q.db.QueryRow(
		`SELECT COUNT(*) FROM upload_tasks
         WHERE rule_id=? AND local_path=? AND file_mtime=? AND file_size=?`,
		ruleID, path, mtime, size).Scan(&n)
	require.NoError(t, err)
	return n
}

// countPathRows counts upload_tasks rows sharing (rule_id, local_path) regardless
// of version. Used to prove a modified file adds a row instead of being absorbed.
func countPathRows(t *testing.T, q *Queue, ruleID, path string) int {
	t.Helper()
	var n int
	err := q.db.QueryRow(
		`SELECT COUNT(*) FROM upload_tasks WHERE rule_id=? AND local_path=?`,
		ruleID, path).Scan(&n)
	require.NoError(t, err)
	return n
}

// Regression: IC-5 subitem ④ (startup reset running→pending) combined with
// ⑤ (fsnotify initial scan) made a crash-restart re-PUT the same file: after
// the reset, the scheduler could dequeue the task back to running, and the
// initial scan then re-enqueued the same file version because running was not
// treated as active. EnqueueIfNoActive's four-tuple guard is what stops the
// double PUT — this test pins it.
func TestEnqueueIfNoActive_NoDuplicateAfterCrashRestartReset(t *testing.T) {
	// Guards the ④+⑤ double-PUT fix: after a crash restart, if the scheduler
	// has already picked the reset task back up (running), the initial scan's
	// re-enqueue of the same file version must be a no-op, not a second upload.
	q := openMemQueue(t)
	ctx := context.Background()

	killed := newTask("killed-mid-upload", "")
	require.NoError(t, q.Enqueue(killed))
	require.NoError(t, q.UpdateStatus(killed.ID, StatusRunning))

	reset, err := q.ResetRunningToPending(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), reset)

	// The double-PUT window: the scheduler dequeues the reset task again
	// while the initial scan is still discovering files.
	dequeued, err := q.DequeuePending(10)
	require.NoError(t, err)
	require.Len(t, dequeued, 1)
	require.Equal(t, killed.ID, dequeued[0].ID)

	// Initial scan rediscovers the same file: identical rule/path/mtime/size.
	rescanned := newTask("rescanned", "")
	rescanned.LocalPath = killed.LocalPath
	rescanned.FileMtime = killed.FileMtime
	rescanned.FileSize = killed.FileSize

	enqueued, err := q.EnqueueIfNoActive(ctx, rescanned)
	require.NoError(t, err)
	assert.False(t, enqueued, "same file version with an active task must not be re-enqueued")

	assert.Equal(t, 1, countTupleRows(t, q, killed.RuleID, killed.LocalPath, killed.FileMtime, killed.FileSize),
		"exactly one row for the four-tuple — no double upload")
}

func TestEnqueueIfNoActive_ModifiedFileAfterCrashRestartReset(t *testing.T) {
	// Guards PR #100 review R1: the running task's frozen (mtime, size) tuple
	// diverges from the file the initial scan sees after a crash, so the
	// four-tuple guard would let a second task enqueue and both would upload
	// the same current bytes — the double PUT this PR claims to close.
	// ResetRunningToPending therefore refreshes each reset row's size/mtime
	// from the filesystem, which FLIPS this test's old semantics ("a changed
	// file must re-enqueue") to "a changed file must have exactly one task in
	// flight": after the reset the task's tuple matches what the scan sees, so
	// the guard must reject the scan's duplicate. Re-collection after a file
	// changes is covered by the terminal-state test below.
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", 1000)), 0o644))
	oldInfo, err := os.Stat(path)
	require.NoError(t, err)

	q := openMemQueue(t)
	ctx := context.Background()

	stale := newTask("stale-version", "")
	stale.LocalPath = path
	stale.FileSize = oldInfo.Size()
	stale.FileMtime = oldInfo.ModTime().Unix()
	require.NoError(t, q.Enqueue(stale))
	require.NoError(t, q.UpdateStatus(stale.ID, StatusRunning))

	// Crash → the writer appends 500 more bytes → restart.
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", 1500)), 0o644))
	newInfo, err := os.Stat(path)
	require.NoError(t, err)
	require.NotEqual(t, oldInfo.Size(), newInfo.Size())

	reset, err := q.ResetRunningToPending(ctx)
	require.NoError(t, err)
	require.Equal(t, int64(1), reset)

	// The initial scan rediscovers the file at its current size.
	rescanned := newTask("rescanned", "")
	rescanned.LocalPath = path
	rescanned.FileSize = newInfo.Size()
	rescanned.FileMtime = newInfo.ModTime().Unix()

	enqueued, err := q.EnqueueIfNoActive(ctx, rescanned)
	require.NoError(t, err)
	assert.False(t, enqueued,
		"the reset task was refreshed to the current tuple, so the scan's duplicate must be rejected")

	pending, err := q.ListByStatus(StatusPending)
	require.NoError(t, err)
	require.Len(t, pending, 1, "exactly one in-flight task for the file — no double upload")
	assert.Equal(t, stale.ID, pending[0].ID)
	assert.Equal(t, newInfo.Size(), pending[0].FileSize,
		"the reset task must carry the refreshed size so the upload matches the file")
}

// Guards ① (IsProcessed mtime/size fix): once a task reached a terminal state,
// a later change to the file must still be collected. Refresh-on-reset must
// not break this — terminal rows are never reset, so the guard only sees an
// active-row conflict when there genuinely is one.
func TestEnqueueIfNoActive_ChangedFileAfterTerminalStateRecovers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "done.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", 1000)), 0o644))
	oldInfo, err := os.Stat(path)
	require.NoError(t, err)

	q := openMemQueue(t)
	ctx := context.Background()

	done := newTask("done-version", "")
	done.LocalPath = path
	done.FileSize = oldInfo.Size()
	done.FileMtime = oldInfo.ModTime().Unix()
	require.NoError(t, q.Enqueue(done))
	require.NoError(t, q.UpdateStatus(done.ID, StatusCompleted))

	// File changes after completion.
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", 1500)), 0o644))
	newInfo, err := os.Stat(path)
	require.NoError(t, err)

	_, err = q.ResetRunningToPending(ctx)
	require.NoError(t, err)

	recollected := newTask("recollected", "")
	recollected.LocalPath = path
	recollected.FileSize = newInfo.Size()
	recollected.FileMtime = newInfo.ModTime().Unix()

	enqueued, err := q.EnqueueIfNoActive(ctx, recollected)
	require.NoError(t, err)
	assert.True(t, enqueued, "a file changed after its task reached a terminal state must be re-collected")
}

// Regression for PR #100 review F3: tail offsets live in the watcher's
// in-memory map and were never rebuilt from persisted state after a restart,
// so the initial scan re-sent already-stored bytes. processed_files.file_size
// is the size at last successful upload — the correct next tail offset.
func TestTailOffsets_ReturnsLastUploadedSizes(t *testing.T) {
	q := openMemQueue(t)
	ctx := context.Background()

	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "pf-1", RuleID: "r-tail", LocalPath: "/logs/a.log", FileSize: 1000, FileMtime: 111,
	}))
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "pf-2", RuleID: "r-tail", LocalPath: "/logs/b.log", FileSize: 2000, FileMtime: 222,
	}))
	// Another rule must not leak into the result.
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "pf-3", RuleID: "r-other", LocalPath: "/logs/a.log", FileSize: 9999, FileMtime: 333,
	}))

	offsets, err := q.TailOffsets(ctx, "r-tail")
	require.NoError(t, err)
	assert.Equal(t, map[string]int64{"/logs/a.log": 1000, "/logs/b.log": 2000}, offsets)
}

func TestTailOffsets_EmptyRule(t *testing.T) {
	q := openMemQueue(t)
	offsets, err := q.TailOffsets(context.Background(), "r-none")
	require.NoError(t, err)
	assert.Empty(t, offsets)
}

func TestResetRunningToPending_PreservesReported(t *testing.T) {
	q := openMemQueue(t)
	require.NoError(t, q.Enqueue(newTask("running-task", "")))
	require.NoError(t, q.Enqueue(newTask("reported-task", "")))
	require.NoError(t, q.UpdateStatus("running-task", StatusRunning))
	require.NoError(t, q.UpdateStatus("reported-task", StatusReported))

	reset, err := q.ResetRunningToPending(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int64(1), reset)

	pending, err := q.ListByStatus(StatusPending)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, "running-task", pending[0].ID)
	reported, err := q.ListByStatus(StatusReported)
	require.NoError(t, err)
	require.Len(t, reported, 1)
	assert.Equal(t, "reported-task", reported[0].ID)
}

// Guards the atomicity of R1's refresh-on-reset (round-2 fix): the metadata
// refresh and the running→pending flip must be one transaction, refreshed
// metadata FIRST. Falsifies the old order (status flipped outside/first, then
// per-row metadata): there a failing metadata update left the status flipped
// but the tuple stale — and since only status=running rows ever get reset
// again, that stale tuple could never be corrected. The new implementation
// must roll back everything when any metadata write fails.
func TestResetRunningToPending_MetadataFailureRollsBackWholeReset(t *testing.T) {
	dir := t.TempDir()
	dsn := filepath.Join(dir, "test.db")
	q, err := Open(dsn)
	require.NoError(t, err)
	defer q.Close()

	// A real file so the reset attempts a metadata refresh.
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", 1000)), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	tk := newTask("running-1", "")
	tk.LocalPath = path
	tk.FileSize = info.Size()
	tk.FileMtime = info.ModTime().Unix()
	require.NoError(t, q.Enqueue(tk))
	require.NoError(t, q.UpdateStatus(tk.ID, StatusRunning))

	// Force the per-row metadata UPDATE inside the reset to fail while letting
	// the status UPDATE through: the trigger fires only on writes touching
	// file_size, which is exactly the write the old order performed second,
	// after the status had already been flipped.
	_, err = q.db.Exec(`CREATE TRIGGER block_task_meta BEFORE UPDATE OF file_size ON upload_tasks
        BEGIN SELECT RAISE(ABORT, 'injected failure'); END`)
	require.NoError(t, err)

	_, err = q.ResetRunningToPending(context.Background())
	require.Error(t, err, "the injected metadata-write failure must abort the reset")

	// The whole reset must have rolled back: the task stays running with its
	// original tuple — never flipped-but-unrefreshed.
	tasks, err := q.ListByStatus(StatusRunning)
	require.NoError(t, err)
	require.Len(t, tasks, 1, "status must NOT have been flipped when the reset failed")
	assert.Equal(t, tk.ID, tasks[0].ID)
	assert.Equal(t, info.Size(), tasks[0].FileSize, "tuple must be untouched")
	assert.Equal(t, info.ModTime().Unix(), tasks[0].FileMtime)
}

// Atomic visibility companion: after a successful reset, the row must expose
// the refreshed size AND pending status together — the combination the old
// two-step (flip status first, write metadata after) order could split across
// an executor dequeue.
func TestResetRunningToPending_RefreshedTupleVisibleWithPendingStatus(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", 1000)), 0o644))
	oldInfo, err := os.Stat(path)
	require.NoError(t, err)

	q := openMemQueue(t)
	tk := newTask("reset-me", "")
	tk.LocalPath = path
	tk.FileSize = oldInfo.Size()
	tk.FileMtime = oldInfo.ModTime().Unix()
	require.NoError(t, q.Enqueue(tk))
	require.NoError(t, q.UpdateStatus(tk.ID, StatusRunning))

	// Crash → file grows → restart.
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("a", 1500)), 0o644))
	newInfo, err := os.Stat(path)
	require.NoError(t, err)

	reset, err := q.ResetRunningToPending(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), reset)

	pending, err := q.ListByStatus(StatusPending)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	assert.Equal(t, newInfo.Size(), pending[0].FileSize,
		"pending status and refreshed size must be visible atomically")
	assert.Equal(t, newInfo.ModTime().Unix(), pending[0].FileMtime)
}

func TestResetRunningToPending_ClosedQueue(t *testing.T) {
	q := openMemQueue(t)
	require.NoError(t, q.Close())
	_, err := q.ResetRunningToPending(context.Background())
	require.Error(t, err)
}

// ── Status Updates ────────────────────────────────────────────────────────────

func TestUpdateStatus(t *testing.T) {
	q := openMemQueue(t)
	task := newTask("task-status", "")
	require.NoError(t, q.Enqueue(task))

	require.NoError(t, q.UpdateStatus("task-status", StatusCompleted))

	tasks, err := q.ListByStatus(StatusCompleted)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, StatusCompleted, tasks[0].Status)
}

func TestUpdateStatus_NotFound(t *testing.T) {
	q := openMemQueue(t)
	err := q.UpdateStatus("nonexistent", StatusCompleted)
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTaskNotFound)
}

func TestMarkFailed(t *testing.T) {
	q := openMemQueue(t)
	task := newTask("task-fail", "")
	require.NoError(t, q.Enqueue(task))

	require.NoError(t, q.MarkFailed(context.Background(), "task-fail", "connection refused", time.Now()))

	tasks, err := q.ListByStatus(StatusFailed)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, StatusFailed, tasks[0].Status)
	assert.Equal(t, 1, tasks[0].RetryCount)
	assert.Equal(t, "connection refused", tasks[0].LastError)
}

func TestMarkFailed_IncrementRetryCount(t *testing.T) {
	q := openMemQueue(t)
	require.NoError(t, q.Enqueue(newTask("retry-task", "")))

	for i := 1; i <= 3; i++ {
		require.NoError(t, q.MarkFailed(context.Background(), "retry-task", "err", time.Now()))
		tasks, err := q.ListByStatus(StatusFailed)
		require.NoError(t, err)
		assert.Equal(t, i, tasks[0].RetryCount)
		// Reset back to pending to allow re-failing.
		require.NoError(t, q.UpdateStatus("retry-task", StatusPending))
		// Re-dequeue so it's running again before marking failed.
		dequeued, err := q.DequeuePending(1)
		require.NoError(t, err)
		require.Len(t, dequeued, 1)
	}
}

func TestMarkFailed_NotFound(t *testing.T) {
	q := openMemQueue(t)
	err := q.MarkFailed(context.Background(), "nonexistent", "err", time.Now())
	require.Error(t, err)
	assert.ErrorIs(t, err, ErrTaskNotFound)
}

func TestListByStatus(t *testing.T) {
	q := openMemQueue(t)
	require.NoError(t, q.Enqueue(newTask("p1", "")))
	require.NoError(t, q.Enqueue(newTask("p2", "")))
	_, err := q.DequeuePending(1) // mark p1 running
	require.NoError(t, err)

	pending, err := q.ListByStatus(StatusPending)
	require.NoError(t, err)
	assert.Len(t, pending, 1)

	running, err := q.ListByStatus(StatusRunning)
	require.NoError(t, err)
	assert.Len(t, running, 1)
}

// ── Processed Files ───────────────────────────────────────────────────────────

func TestProcessedFiles_Dedup(t *testing.T) {
	q := openMemQueue(t)

	f := &ProcessedFile{
		ID:         "pf-1",
		RuleID:     "rule-1",
		LocalPath:  "/data/file.txt",
		FileSize:   512,
		FileMtime:  time.Now().Unix(),
		SHA256:     "abc123",
		UploadedAt: time.Now().Unix(),
	}
	require.NoError(t, q.UpsertProcessedFile(f))

	ok, err := q.IsProcessed(context.Background(), "rule-1", "/data/file.txt", f.FileMtime, f.FileSize)
	require.NoError(t, err)
	assert.True(t, ok)

	ok2, err := q.IsProcessed(context.Background(), "rule-1", "/data/other.txt", f.FileMtime, f.FileSize)
	require.NoError(t, err)
	assert.False(t, ok2)

	changedMtime, err := q.IsProcessed(context.Background(), f.RuleID, f.LocalPath, f.FileMtime+1, f.FileSize)
	require.NoError(t, err)
	assert.False(t, changedMtime)

	changedSize, err := q.IsProcessed(context.Background(), f.RuleID, f.LocalPath, f.FileMtime, f.FileSize+1)
	require.NoError(t, err)
	assert.False(t, changedSize)
}

func TestProcessedFiles_Upsert(t *testing.T) {
	q := openMemQueue(t)
	f := &ProcessedFile{ID: "pf-2", RuleID: "r1", LocalPath: "/x", FileSize: 10, FileMtime: 1}
	require.NoError(t, q.UpsertProcessedFile(f))

	// Update same file with new size.
	f2 := &ProcessedFile{ID: "pf-3", RuleID: "r1", LocalPath: "/x", FileSize: 999, FileMtime: 2}
	require.NoError(t, q.UpsertProcessedFile(f2))

	// Still only one record (the upsert replaced it).
	ok, err := q.IsProcessed(context.Background(), "r1", "/x", f2.FileMtime, f2.FileSize)
	require.NoError(t, err)
	assert.True(t, ok)
}

func TestIsProcessed_ClosedQueue(t *testing.T) {
	q := openMemQueue(t)
	require.NoError(t, q.Close())
	_, err := q.IsProcessed(context.Background(), "rule", "/file", 1, 1)
	require.Error(t, err)
}

// ── Rules ─────────────────────────────────────────────────────────────────────

func TestRules_CRUD(t *testing.T) {
	q := openMemQueue(t)

	r := &Rule{ID: "rule-abc", Payload: `{"path":"/data"}`, UpdatedAt: time.Now().Unix()}
	require.NoError(t, q.UpsertRule(r))

	got, err := q.GetRule("rule-abc")
	require.NoError(t, err)
	assert.Equal(t, r.ID, got.ID)
	assert.Equal(t, r.Payload, got.Payload)
}

func TestRules_Upsert(t *testing.T) {
	q := openMemQueue(t)

	r := &Rule{ID: "rule-1", Payload: "v1"}
	require.NoError(t, q.UpsertRule(r))

	r2 := &Rule{ID: "rule-1", Payload: "v2"}
	require.NoError(t, q.UpsertRule(r2))

	got, err := q.GetRule("rule-1")
	require.NoError(t, err)
	assert.Equal(t, "v2", got.Payload)
}

func TestRules_NotFound(t *testing.T) {
	q := openMemQueue(t)
	_, err := q.GetRule("missing")
	require.Error(t, err)
	assert.True(t, errors.Is(err, sql.ErrNoRows))
}

func TestQueue_AppendModeFields_PersistAndLoad(t *testing.T) {
q, err := Open(":memory:")
require.NoError(t, err)
defer q.Close()

task := &UploadTask{
ID:          "task-tail",
RuleID:      "rule-1",
LocalPath:   "/data/file.log",
StoragePath: "logs/file.log",
Bucket:      "my-bucket",
FileSize:    1024,
FileMtime:   time.Now().Unix(),
FileOffset:  512,
AppendMode:  "tail",
}
require.NoError(t, q.Enqueue(task))

tasks, err := q.DequeuePending(10)
require.NoError(t, err)
require.Len(t, tasks, 1)
assert.Equal(t, int64(512), tasks[0].FileOffset)
assert.Equal(t, "tail", tasks[0].AppendMode)
}

func TestQueue_Open_ExistingDB_MigratesNewColumns(t *testing.T) {
// Simulate a pre-existing DB that doesn't have file_offset/append_mode columns.
// Opening it again should run the ALTER TABLE migrations without error.
dir := t.TempDir()
dsn := filepath.Join(dir, "test.db")

// Create a DB without file_offset/append_mode.
db, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL")
require.NoError(t, err)
_, err = db.Exec(`
CREATE TABLE IF NOT EXISTS upload_tasks (
id TEXT PRIMARY KEY, rule_id TEXT NOT NULL, local_path TEXT NOT NULL,
storage_path TEXT NOT NULL, bucket TEXT NOT NULL, upload_id TEXT,
completed_parts TEXT, file_size INTEGER NOT NULL DEFAULT 0,
file_mtime INTEGER NOT NULL DEFAULT 0, sha256 TEXT,
status TEXT NOT NULL DEFAULT 'pending', retry_count INTEGER NOT NULL DEFAULT 0,
last_error TEXT, created_at INTEGER NOT NULL, updated_at INTEGER NOT NULL
);
CREATE TABLE IF NOT EXISTS processed_files (
id TEXT PRIMARY KEY, rule_id TEXT NOT NULL, local_path TEXT NOT NULL,
file_size INTEGER NOT NULL, file_mtime INTEGER NOT NULL, sha256 TEXT,
uploaded_at INTEGER NOT NULL, UNIQUE (rule_id, local_path)
);
CREATE TABLE IF NOT EXISTS rules (
id TEXT PRIMARY KEY, payload TEXT NOT NULL, updated_at INTEGER NOT NULL
);
`)
require.NoError(t, err)
db.Close()

// Re-open with Queue.Open — migration should succeed.
q, err := Open(dsn)
require.NoError(t, err)
defer q.Close()

// Should be able to enqueue with the new fields.
task := &UploadTask{
ID: "m1", RuleID: "r1", LocalPath: "/f", StoragePath: "s",
Bucket: "b", FileSize: 1, FileMtime: 1, FileOffset: 100, AppendMode: "close_wait",
}
require.NoError(t, q.Enqueue(task))
}

// ── Capacity enforcement (queue_max_size) ─────────────────────────────────────

// taskAt builds a pending task with an explicit created_at so ordering is
// deterministic in fast-running tests.
func taskAt(id string, createdAt int64) *UploadTask {
	t := newTask(id, "")
	t.CreatedAt = createdAt
	t.UpdatedAt = createdAt
	return t
}

func TestCountActive_ExcludesCompleted(t *testing.T) {
	q := openMemQueue(t)

	require.NoError(t, q.Enqueue(taskAt("a", 1)))
	require.NoError(t, q.Enqueue(taskAt("b", 2)))
	require.NoError(t, q.Enqueue(taskAt("c", 3)))

	// One running, one failed, one completed: only pending+running+failed count.
	require.NoError(t, q.UpdateStatus("a", StatusRunning))
	require.NoError(t, q.MarkFailed(context.Background(), "b", "boom", time.Now()))
	require.NoError(t, q.UpdateStatus("c", StatusCompleted))

	n, err := q.CountActive()
	require.NoError(t, err)
	assert.Equal(t, 2, n) // a (running) + b (failed); c (completed) excluded
}

func TestDeleteOldestEvictable(t *testing.T) {
	q := openMemQueue(t)

	require.NoError(t, q.Enqueue(taskAt("old", 100)))
	require.NoError(t, q.Enqueue(taskAt("mid", 200)))
	require.NoError(t, q.Enqueue(taskAt("new", 300)))

	dropped, err := q.DeleteOldestEvictable("")
	require.NoError(t, err)
	require.NotNil(t, dropped)
	assert.Equal(t, "old", dropped.ID)

	// "old" is gone; the remaining two are intact.
	remaining, err := q.ListByStatus(StatusPending)
	require.NoError(t, err)
	assert.Len(t, remaining, 2)
}

func TestDeleteOldestEvictable_IncludesFailedSkipsRunning(t *testing.T) {
	q := openMemQueue(t)

	// Oldest is running (in-flight, must NOT be evicted); next-oldest is failed
	// (awaiting retry, IS evictable); newest is pending.
	require.NoError(t, q.Enqueue(taskAt("running", 100)))
	require.NoError(t, q.UpdateStatus("running", StatusRunning))
	require.NoError(t, q.Enqueue(taskAt("failed", 200)))
	require.NoError(t, q.MarkFailed(context.Background(), "failed", "boom", time.Now()))
	require.NoError(t, q.Enqueue(taskAt("pending", 300)))

	dropped, err := q.DeleteOldestEvictable("")
	require.NoError(t, err)
	require.NotNil(t, dropped)
	assert.Equal(t, "failed", dropped.ID,
		"failed task is evictable and older than pending; running is skipped")
}

func TestDeleteOldestEvictable_OnlyRunning(t *testing.T) {
	q := openMemQueue(t)

	// Nothing evictable when the sole active task is running (in-flight).
	require.NoError(t, q.Enqueue(taskAt("running", 100)))
	require.NoError(t, q.UpdateStatus("running", StatusRunning))

	dropped, err := q.DeleteOldestEvictable("")
	require.NoError(t, err)
	assert.Nil(t, dropped)
}

func TestDeleteOldestEvictable_Empty(t *testing.T) {
	q := openMemQueue(t)

	dropped, err := q.DeleteOldestEvictable("")
	require.NoError(t, err)
	assert.Nil(t, dropped)
}

func TestDeleteOldestEvictable_ExcludesGivenID(t *testing.T) {
	q := openMemQueue(t)

	require.NoError(t, q.Enqueue(taskAt("old", 100)))
	require.NoError(t, q.Enqueue(taskAt("new", 200)))

	// Excluding the oldest forces eviction to skip it and pick the next candidate.
	dropped, err := q.DeleteOldestEvictable("old")
	require.NoError(t, err)
	require.NotNil(t, dropped)
	assert.Equal(t, "new", dropped.ID)
}

func TestDeleteOldestEvictable_ExcludedIsOnlyCandidate(t *testing.T) {
	q := openMemQueue(t)

	// The only evictable task is the excluded one → nothing to drop.
	require.NoError(t, q.Enqueue(taskAt("solo", 100)))

	dropped, err := q.DeleteOldestEvictable("solo")
	require.NoError(t, err)
	assert.Nil(t, dropped)
}

// TestDeleteOldestEvictable_RecordsDurableAbort pins the IC-3 ②/P2 ordering:
// evicting a task whose multipart upload is in flight must write the durable
// abort record in the SAME transaction as the delete. The executor's own
// best-effort record happens only AFTER the row is gone — a crash (or a failed
// enqueue) in between would lose the upload identity for good, and no ILM
// rule backstop exists on current MinIO builds.
func TestDeleteOldestEvictable_RecordsDurableAbort(t *testing.T) {
	q := openMemQueue(t)

	task := taskAt("old", 100)
	task.UploadID = "upload-evict-1"
	task.Bucket = "bkt"
	task.StoragePath = "obj/key"
	require.NoError(t, q.Enqueue(task))
	require.NoError(t, q.MarkFailed(context.Background(), "old", "boom", time.Now()))

	// No abort record beforehand.
	n, err := q.CountMultipartAborts(context.Background())
	require.NoError(t, err)
	require.Zero(t, n)

	dropped, err := q.DeleteOldestEvictable("")
	require.NoError(t, err)
	require.NotNil(t, dropped)

	entries, err := q.DueMultipartAborts(context.Background(), time.Now(), 10)
	require.NoError(t, err)
	require.Len(t, entries, 1, "evicting a task with a live upload must durably record the abort")
	assert.Equal(t, "upload-evict-1", entries[0].UploadID)
	assert.Equal(t, "old", entries[0].TaskID)
	assert.Equal(t, "bkt", entries[0].Bucket)
	assert.Equal(t, "obj/key", entries[0].StoragePath)
	assert.Equal(t, 0, entries[0].Attempts)

	// Evicting a task WITHOUT an upload id must not leave a record behind.
	require.NoError(t, q.Enqueue(taskAt("plain", 200)))
	dropped, err = q.DeleteOldestEvictable("")
	require.NoError(t, err)
	require.NotNil(t, dropped)
	entries, err = q.DueMultipartAborts(context.Background(), time.Now(), 10)
	require.NoError(t, err)
	assert.Len(t, entries, 1, "no abort record for a task without an upload id")
}

func TestSaveMultipartProgress_PersistsBothColumns(t *testing.T) {
	q := openMemQueue(t)

	require.NoError(t, q.Enqueue(taskAt("mp-1", 1)))

	// Simulate per-part progress: initiate then two completed parts, as the
	// uploader calls it (IC-BUG-5).
	require.NoError(t, q.SaveMultipartProgress(context.Background(), "mp-1", "upload-abc", ""))
	require.NoError(t, q.SaveMultipartProgress(context.Background(), "mp-1", "upload-abc", `{"parts":[{"PartNumber":1,"ETag":"e1"}]}`))

	tasks, err := q.ListByStatus(StatusPending)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "upload-abc", tasks[0].UploadID)
	assert.Contains(t, tasks[0].CompletedParts, "PartNumber")

	// The reset path must keep the resume state so the task can continue
	// rather than restart (IC-3 / IC-BUG-34 synergy).
	require.NoError(t, q.UpdateStatus("mp-1", StatusRunning))
	reset, err := q.ResetRunningToPending(context.Background())
	require.NoError(t, err)
	require.Equal(t, int64(1), reset)
	tasks, err = q.ListByStatus(StatusPending)
	require.NoError(t, err)
	require.Len(t, tasks, 1)
	assert.Equal(t, "upload-abc", tasks[0].UploadID, "reset must preserve upload_id for resume")
	assert.NotEmpty(t, tasks[0].CompletedParts, "reset must preserve completed_parts for resume")
}

func TestSaveMultipartProgress_TaskNotFound(t *testing.T) {
	q := openMemQueue(t)

	err := q.SaveMultipartProgress(context.Background(), "missing", "upload-abc", "")
	require.Error(t, err)
	assert.True(t, errors.Is(err, ErrTaskNotFound))
}

func TestSaveMultipartProgress_DatabaseError(t *testing.T) {
	q := openMemQueue(t)

	require.NoError(t, q.Close())
	err := q.SaveMultipartProgress(context.Background(), "mp-err", "upload-abc", "")
	require.Error(t, err)
	assert.False(t, errors.Is(err, ErrTaskNotFound), "a closed DB is a real failure, not eviction")
}

// A failed DELETE inside the eviction transaction must roll the abort record
// back together with the delete: a committed record for a task that still
// exists would make the abort worker race the live task.
func TestDeleteOldestEvictable_FailedDeleteRollsBackAbortRecord(t *testing.T) {
	dsn := filepath.Join(t.TempDir(), "queue.db")
	q, err := Open(dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Close() })

	task := taskAt("old", 100)
	task.UploadID = "upload-rollback-1"
	require.NoError(t, q.Enqueue(task))
	require.NoError(t, q.MarkFailed(context.Background(), "old", "boom", time.Now()))
	require.NoError(t, q.Enqueue(taskAt("new", 200)))

	db, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL&_busy_timeout=5000")
	require.NoError(t, err)
	defer func() { require.NoError(t, db.Close()) }()
	_, err = db.Exec(`CREATE TRIGGER no_evict BEFORE DELETE ON upload_tasks BEGIN SELECT RAISE(ABORT, 'injected failure'); END;`)
	require.NoError(t, err)

	_, err = q.DeleteOldestEvictable("")
	require.Error(t, err, "the injected DELETE failure must surface")

	n, err := q.CountMultipartAborts(context.Background())
	require.NoError(t, err)
	assert.Zero(t, n, "the rolled-back transaction must not leave an abort record behind")
	remaining, err := q.ListByStatus(StatusFailed)
	require.NoError(t, err)
	assert.Len(t, remaining, 1, "the evicted task must survive the failed eviction")
}

// MarkFailed on a broken database must surface the error.
func TestMarkFailed_DatabaseError(t *testing.T) {
	q := openMemQueue(t)
	require.NoError(t, q.Enqueue(taskAt("t", 1)))
	require.NoError(t, q.Close())
	require.Error(t, q.MarkFailed(context.Background(), "t", "err", time.Now().Add(time.Minute)))
}
