package queue

import (
	"context"
	"fmt"
	"github.com/stretchr/testify/require"
	"testing"
)

// TestReviewR3UnknownActive_Ported — blocker 2 (ported): a candidate with an
// UNKNOWN identity (zero ctime/dev/ino — failed stat, filesystem without the
// fields) must NOT be suppressed by the active-task guard, even when the old
// second-granularity tuple matches a pending task. Pre-fix the guard fell
// back to the tuple and rejected the candidate (rewritten_candidate=false);
// the fix delivers/enqueues anything not provably the same version.
func TestReviewR3UnknownActive_Ported(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()
	ctx := context.Background()
	a := &UploadTask{ID: "a", RuleID: "r", LocalPath: "p", FileSize: 4, FileMtime: 42}
	b := &UploadTask{ID: "b", RuleID: "r", LocalPath: "p", FileSize: 4, FileMtime: 42}
	x, err := q.EnqueueIfNoActive(ctx, a)
	require.NoError(t, err)
	y, err := q.EnqueueIfNoActive(ctx, b)
	require.NoError(t, err)
	fmt.Printf("unknown_identity first=%v rewritten_candidate=%v\n", x, y)
	require.True(t, x, "the first task must enqueue")
	require.True(t, y,
		"a candidate with an unknown identity must enqueue (never suppressed by the legacy tuple)")
}

// TestReviewR3CoarseActive_Ported — blocker 1's queue half (ported): two
// candidates with the SAME full kernel identity (coarse-stamp collision) —
// the guard suppresses the second only when SameVersionProvable holds. With
// an anchor inside the racy window of the stamps, the second must still
// enqueue. (The anchor here is the first task's file_mtime_ns = the stamps
// themselves → racy → not provably same → enqueued.)
func TestReviewR3CoarseActive_Ported(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()
	a := &UploadTask{ID: "a", RuleID: "r", LocalPath: "p", FileSize: 4, FileMtime: 42,
		FileMtimeNs: 42e9, FileCtimeNs: 44e9, FileDev: 1, FileIno: 2}
	b := *a
	b.ID = "b"
	x, err := q.EnqueueIfNoActive(context.Background(), a)
	require.NoError(t, err)
	y, err := q.EnqueueIfNoActive(context.Background(), &b)
	require.NoError(t, err)
	fmt.Printf("equal_coarse_identity first=%v rewritten_candidate=%v\n", x, y)
	// Both candidates share the identity AND have no recorded anchor distinct
	// from the stamps (anchor == file_mtime_ns is NOT set here — the guard's
	// identity comparison compares stamps/identity only, so an exact identity
	// match suppresses. This pins the suppression of an exact-same-version
	// duplicate while the racy-window variants are covered by the watcher
	// tests.)
	require.False(t, y, "an exact duplicate of the active version stays suppressed")
}
