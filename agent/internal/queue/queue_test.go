package queue

import (
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
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
	assert.Contains(t, err.Error(), "not found")
}

func TestMarkFailed(t *testing.T) {
	q := openMemQueue(t)
	task := newTask("task-fail", "")
	require.NoError(t, q.Enqueue(task))

	require.NoError(t, q.MarkFailed("task-fail", "connection refused"))

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
		require.NoError(t, q.MarkFailed("retry-task", "err"))
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
	err := q.MarkFailed("nonexistent", "err")
	require.Error(t, err)
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

	ok, err := q.IsProcessed("rule-1", "/data/file.txt")
	require.NoError(t, err)
	assert.True(t, ok)

	ok2, err := q.IsProcessed("rule-1", "/data/other.txt")
	require.NoError(t, err)
	assert.False(t, ok2)
}

func TestProcessedFiles_Upsert(t *testing.T) {
	q := openMemQueue(t)
	f := &ProcessedFile{ID: "pf-2", RuleID: "r1", LocalPath: "/x", FileSize: 10, FileMtime: 1}
	require.NoError(t, q.UpsertProcessedFile(f))

	// Update same file with new size.
	f2 := &ProcessedFile{ID: "pf-3", RuleID: "r1", LocalPath: "/x", FileSize: 999, FileMtime: 2}
	require.NoError(t, q.UpsertProcessedFile(f2))

	// Still only one record (the upsert replaced it).
	ok, err := q.IsProcessed("r1", "/x")
	require.NoError(t, err)
	assert.True(t, ok)
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
	require.NoError(t, q.MarkFailed("b", "boom"))
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

	dropped, err := q.DeleteOldestEvictable()
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
	require.NoError(t, q.MarkFailed("failed", "boom"))
	require.NoError(t, q.Enqueue(taskAt("pending", 300)))

	dropped, err := q.DeleteOldestEvictable()
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

	dropped, err := q.DeleteOldestEvictable()
	require.NoError(t, err)
	assert.Nil(t, dropped)
}

func TestDeleteOldestEvictable_Empty(t *testing.T) {
	q := openMemQueue(t)

	dropped, err := q.DeleteOldestEvictable()
	require.NoError(t, err)
	assert.Nil(t, dropped)
}
