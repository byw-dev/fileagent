package queue

import (
	"context"
	"database/sql"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"testing"
	"time"
)

// TestReportsPersistAndAcknowledge covers startup replay and atomic dedup state.
func TestReportsPersistAndAcknowledge(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "queue.db")
	q, err := Open(path)
	require.NoError(t, err)
	task := &UploadTask{ID: "report", RuleID: "rule", LocalPath: "file", FileSize: 100, FileMtime: 1}
	require.NoError(t, q.Enqueue(task))
	_, err = q.DequeuePending(1)
	require.NoError(t, err)
	require.NoError(t, q.SaveReport(ctx, task.ID, []byte("result")))
	active, err := q.CountActive()
	require.NoError(t, err)
	require.Equal(t, 1, active)
	dropped, err := q.DeleteOldestEvictable("")
	require.NoError(t, err)
	require.Nil(t, dropped)
	require.NoError(t, q.TouchReport(ctx, task.ID, time.Now().Add(time.Hour)))
	due, err := q.DueReports(ctx, time.Now())
	require.NoError(t, err)
	require.Empty(t, due)
	require.NoError(t, q.Close())
	q, err = Open(path)
	require.NoError(t, err)
	defer q.Close()
	require.NoError(t, q.ResetReportTimers(ctx))
	due, err = q.DueReports(ctx, time.Now())
	require.NoError(t, err)
	require.Len(t, due, 1)
	require.Equal(t, []byte("result"), due[0].Payload)
	require.NoError(t, q.CompleteReported(ctx, task.ID, true, "sha"))
	// IC-BUG-48: CompleteReported (no identity) writes an UNDECIDABLE row —
	// identity fields zero, sha recorded — not the enqueue-time tuple.
	rec, err := q.GetProcessedIdentity("rule", "file")
	require.NoError(t, err)
	require.Equal(t, "sha", rec.SHA256)
	require.False(t, IdentityFullyKnown(rec.Ident),
		"an identity-less completion must leave the row undecidable (hash-confirm next time)")
	_, err = q.GetReport(ctx, task.ID)
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, q.CompleteReported(ctx, task.ID, true, "sha"))
	require.NoError(t, q.Close())
	require.Error(t, q.SaveReport(ctx, "missing", nil))
	_, err = q.DueReports(ctx, time.Now())
	require.Error(t, err)
	require.Error(t, q.TouchReport(ctx, "missing", time.Now()))
	require.Error(t, q.ResetReportTimers(ctx))
	require.Error(t, q.CompleteReported(ctx, "missing", false, ""))
}

// TestReportFailureDoesNotMarkProcessed keeps failed uploads out of dedup state.
func TestReportFailureDoesNotMarkProcessed(t *testing.T) {
	ctx := context.Background()
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()
	require.ErrorIs(t, q.SaveReport(ctx, "missing", []byte("x")), ErrTaskNotFound)
	task := &UploadTask{ID: "failed", RuleID: "rule", LocalPath: "file"}
	require.NoError(t, q.Enqueue(task))
	require.NoError(t, q.MarkFailed(context.Background(), task.ID, "failure", time.Now()))
	require.NoError(t, q.SaveReport(ctx, task.ID, []byte("failure")))
	require.NoError(t, q.CompleteReported(ctx, task.ID, false, ""))
	processed, err := q.IsProcessed(ctx, "rule", "file", task.FileMtime, task.FileSize)
	require.NoError(t, err)
	require.False(t, processed)
}

// TestCompleteReportedIdentity_OkFalse_ZeroesIdentity — rework-2 fix 6: the
// ledger API itself must enforce the ok flag. A non-zero identity with
// ok=false must land as a ZERO identity row (undecidable → hash-confirm next
// time), regardless of what the caller passed.
func TestCompleteReportedIdentity_OkFalse_ZeroesIdentity(t *testing.T) {
	ctx := context.Background()
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	task := &UploadTask{ID: "okf", RuleID: "rule", LocalPath: "file", FileSize: 100, FileMtime: 1}
	require.NoError(t, q.Enqueue(task))
	_, err = q.DequeuePending(1)
	require.NoError(t, err)
	require.NoError(t, q.SaveReport(ctx, task.ID, []byte("r")))

	nonZero := FileIdentity{MtimeNs: 123, CtimeNs: 456, Dev: 7, Ino: 8, Size: 9, SnapAtNs: 10}
	require.NoError(t, q.CompleteReportedIdentity(ctx, task.ID, true, "sha", nonZero, false))

	rec, err := q.GetProcessedIdentity("rule", "file")
	require.NoError(t, err)
	require.False(t, IdentityFullyKnown(rec.Ident),
		"ok=false must zero the identity at the ledger API entry (fail-closed)")
	require.Equal(t, "sha", rec.SHA256)
}
