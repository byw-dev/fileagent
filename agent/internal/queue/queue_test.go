package queue

import (
	"database/sql"
	"errors"
	"fmt"
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
		tasks, _ := q.ListByStatus(StatusFailed)
		assert.Equal(t, i, tasks[0].RetryCount)
		// Reset back to pending to allow re-failing.
		require.NoError(t, q.UpdateStatus("retry-task", StatusPending))
		// Re-dequeue so it's running again before marking failed.
		dequeued, _ := q.DequeuePending(1)
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
