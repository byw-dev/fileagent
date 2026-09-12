package queue

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The durable multipart-abort outbox (IC-3 ②/P2): the retryable identity of a
// MinIO multipart upload that must be aborted. The queue-level CRUD is tested
// here; the executor's abort worker behaviour lives in the executor package.

func TestMultipartAbortOutbox_CRUD(t *testing.T) {
	ctx := context.Background()
	q := openMemQueue(t)

	entries, err := q.DueMultipartAborts(ctx, time.Now(), 10)
	require.NoError(t, err)
	assert.Empty(t, entries)

	require.NoError(t, q.EnqueueMultipartAbort(ctx, &AbortOutboxEntry{
		UploadID:    "upload-1",
		TaskID:      "task-1",
		Bucket:      "bkt",
		StoragePath: "obj/key",
	}))

	entries, err = q.DueMultipartAborts(ctx, time.Now(), 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "upload-1", entries[0].UploadID)
	assert.Equal(t, "task-1", entries[0].TaskID)
	assert.Equal(t, "bkt", entries[0].Bucket)
	assert.Equal(t, "obj/key", entries[0].StoragePath)
	assert.Equal(t, 0, entries[0].Attempts)
	assert.Zero(t, entries[0].NextAttemptAt, "new records are due immediately")
	assert.Empty(t, entries[0].LastError)

	// A future-scheduled record is not due yet.
	require.NoError(t, q.EnqueueMultipartAbort(ctx, &AbortOutboxEntry{
		UploadID:      "upload-2",
		TaskID:        "task-2",
		Bucket:        "bkt",
		StoragePath:   "obj/key2",
		NextAttemptAt: time.Now().Add(time.Hour).Unix(),
	}))
	entries, err = q.DueMultipartAborts(ctx, time.Now(), 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "upload-1", entries[0].UploadID)

	// Re-enqueueing the same upload keeps the original record (INSERT OR
	// IGNORE keyed by upload id).
	require.NoError(t, q.EnqueueMultipartAbort(ctx, &AbortOutboxEntry{
		UploadID: "upload-1", TaskID: "task-1", Bucket: "bkt", StoragePath: "obj/key",
	}))
	entries, err = q.DueMultipartAborts(ctx, time.Now(), 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	// A failed attempt bumps the counter and reschedules.
	next := time.Now().Add(5 * time.Minute)
	require.NoError(t, q.RecordMultipartAbortAttempt(ctx, "upload-1", next, "minio unreachable"))
	// Not due until the rescheduled time.
	entries, err = q.DueMultipartAborts(ctx, time.Now(), 10)
	require.NoError(t, err)
	assert.Empty(t, entries)
	entries, err = q.DueMultipartAborts(ctx, next, 10)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, 1, entries[0].Attempts)
	assert.Equal(t, "minio unreachable", entries[0].LastError)
	assert.Equal(t, next.Unix(), entries[0].NextAttemptAt)

	// Recording an attempt for a deleted record is benign (ErrTaskNotFound).
	require.NoError(t, q.DeleteMultipartAbort(ctx, "upload-1"))
	err = q.RecordMultipartAbortAttempt(ctx, "upload-1", time.Now(), "x")
	require.ErrorIs(t, err, ErrTaskNotFound)

	n, err := q.CountMultipartAborts(ctx)
	require.NoError(t, err)
	assert.Equal(t, 1, n, "upload-2's future record remains")

	// The record disappears once deleted.
	require.NoError(t, q.DeleteMultipartAbort(ctx, "upload-2"))
	n, err = q.CountMultipartAborts(ctx)
	require.NoError(t, err)
	assert.Zero(t, n)
}
// Error paths: a broken database must surface as an error, not a silent
// success — the abort record is the last retryable identity, and a silently
// dropped write would manufacture exactly the orphan this mechanism exists
// to prevent.
func TestMultipartAbortOutbox_DatabaseErrors(t *testing.T) {
	ctx := context.Background()
	q, err := Open(":memory:")
	require.NoError(t, err)
	require.NoError(t, q.Close())

	require.Error(t, q.EnqueueMultipartAbort(ctx, &AbortOutboxEntry{UploadID: "u", TaskID: "t", Bucket: "b", StoragePath: "k"}))
	_, err = q.DueMultipartAborts(ctx, time.Now(), 10)
	require.Error(t, err)
	require.Error(t, q.RecordMultipartAbortAttempt(ctx, "u", time.Now(), "e"))
	require.Error(t, q.DeleteMultipartAbort(ctx, "u"))
	_, err = q.CountMultipartAborts(ctx)
	require.Error(t, err)
}
