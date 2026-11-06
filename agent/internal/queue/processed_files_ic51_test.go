package queue

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestProcessedFiles_ChangedFileIsReCollected pins the NARROWED IC-BUG-51
// conclusion about processed_files (review-r1 fix 3): the row written on
// ack carries the (size, mtime) tuple the task was ENQUEUED with
// (CompleteReported projects it from upload_tasks). WHEN a next candidate
// check runs and the file's tuple has CHANGED since enqueue (new second-
// granularity mtime or new size), IsProcessed returns false and the new
// version is re-collectable — the ledger cannot claim "already processed"
// for a version it never saw.
//
// ⚠️ Scope (deliberately narrow — do not widen): this proves the QUEUE-
// LEVEL predicate only. It does NOT prove that a next candidate check will
// actually happen: the initial-scan and cron-walk paths re-walk the tree,
// but the fsnotify/overwrite path debounces via watcher `seen` state and
// does not rescan on its own — delivery of the new version there depends on
// a new filesystem event. And it does NOT cover the same-second, same-size
// in-place rewrite: that tuple is indistinguishable from the recorded one
// and IsProcessed stays true — that blind spot belongs to IC-BUG-48
// (content-identity verification) and is untouched here.
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
