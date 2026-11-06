package queue

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestProcessedFiles_ChangedFileIsReCollected pins the IC-BUG-51 conclusion
// about processed_files: the row written on ack carries the (size, mtime)
// tuple the task was ENQUEUED with (CompleteReported projects it from
// upload_tasks). If the file changes during the upload, the file's CURRENT
// tuple no longer matches the stored one, so the next scan's IsProcessed
// returns false and the new version is re-collected — the ledger can never
// claim "already processed" for bytes it never saw.
func TestProcessedFiles_ChangedFileIsReCollected(t *testing.T) {
	ctx := context.Background()
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	task := &UploadTask{ID: "ic51", RuleID: "rule", LocalPath: "file", FileSize: 100, FileMtime: 1000}
	require.NoError(t, q.Enqueue(task))
	_, err = q.DequeuePending(1)
	require.NoError(t, err)
	require.NoError(t, q.SaveReport(ctx, task.ID, []byte("result")))
	require.NoError(t, q.CompleteReported(ctx, task.ID, true, "sha-of-delivered-bytes"))

	// Same version → dedup still hits (no re-upload of unchanged content).
	processed, err := q.IsProcessed(ctx, "rule", "file", 1000, 100)
	require.NoError(t, err)
	require.True(t, processed, "an identical version stays deduplicated")

	// The file was rewritten while/before the upload finished: new mtime.
	processed, err = q.IsProcessed(ctx, "rule", "file", 1001, 100)
	require.NoError(t, err)
	require.False(t, processed, "a new mtime must be re-collected")

	// The file grew while the upload was in flight: new size.
	processed, err = q.IsProcessed(ctx, "rule", "file", 1000, 101)
	require.NoError(t, err)
	require.False(t, processed, "a new size must be re-collected")
}

// TestProcessedFiles_FailedUploadWritesNothing pins the other half: a task
// whose upload FAILED never reaches processed_files, so a truncation-mid-
// upload failure (IC-BUG-51: loud, not silent) leaves the file re-collectable
// with its new (correct) tuple on the next attempt.
func TestProcessedFiles_FailedUploadWritesNothing(t *testing.T) {
	ctx := context.Background()
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := filepath.Join(t.TempDir(), "f")
	task := &UploadTask{ID: "ic51f", RuleID: "rule", LocalPath: path, FileSize: 100, FileMtime: 1000}
	require.NoError(t, q.Enqueue(task))
	require.NoError(t, q.MarkFailed(ctx, task.ID, "uploader: delivered 50 bytes but declared 100", time.Now()))
	require.NoError(t, q.SaveReport(ctx, task.ID, []byte("failure")))
	require.NoError(t, q.CompleteReported(ctx, task.ID, false, ""))

	processed, err := q.IsProcessed(ctx, "rule", path, 1000, 100)
	require.NoError(t, err)
	require.False(t, processed, "a failed upload must never be recorded as processed")
}
