package queue

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

// ── Rework-6 blocker 2: the enqueue guard must use the ONE predicate ─────────
//
// EnqueueIfNoActive decides "is this candidate the same version as an active
// task" — that judgement has exactly one authority in the codebase:
// SameVersionProvable(rec, cur, recAnchorNs) (= PrefilterVerdict == VerdictSame:
// every identity field known AND equal AND both stamps strictly before
// anchor−G AND the anchor known and not in the future). Not provably-same →
// the candidate MUST enqueue; the downstream DecideProcessed hash-confirms and
// never re-uploads identical content.
//
// The anchor of an active task is its SNAPSHOT MOMENT — the moment the task's
// identity was stat'ed, stored in the new file_snap_at_ns column. Without the
// column the guard could not run the racy-window judgement at all (that was
// the review-r3 blocker 1 "enqueue half" hole: plain SQL identity equality,
// no racy window, swallowed a same-tick rewritten candidate).

// identicalTo returns a duplicate task of t with a new id.
func identicalTo(id string, t *UploadTask) *UploadTask {
	c := *t
	c.ID = id
	return &c
}

// TestEnqueueIfNoActive_RacyWindow_MustEnqueue — the review-r3 coarse-active
// reproduction, ported to its ORIGINAL intent (rework-6): the active task's
// snapshot anchor falls inside the racy window of the stamps (stamp ≥ anchor−2s),
// so the identity-equal candidate is NOT provably the same version → it must
// ENQUEUE. The rework-5 version of this test had been changed to pin the
// opposite (require.False) — pinning the bug.
func TestEnqueueIfNoActive_RacyWindow_MustEnqueue(t *testing.T) {
	ctx := context.Background()
	q := openMemQueue(t)

	// anchor−G exactly AT the mtime stamp (one ns later): the ctime stamp
	// (equal to mtime) is ≥ anchor−G → racy → Undecidable → must enqueue.
	// Both timestamps are read from the real clock (anchored at nowNs, the
	// same function the verdict uses for the future-anchor guard), keeping
	// the ordering exact rather than approximation-dependent.
	G := int64(2 * 1e9)
	anchor := nowNs()
	stamps := anchor - G + 1 // stamps == anchor−G+1 → ≥ anchor−G → racy
	a := &UploadTask{ID: "a", RuleID: "r", LocalPath: "p", FileSize: 4,
		FileMtime:   stamps / 1e9,
		FileMtimeNs: stamps, FileCtimeNs: stamps, FileDev: 1, FileIno: 2,
		FileSnapAtNs: anchor, // anchor = the snapshot moment of a's identity
	}
	x, err := q.EnqueueIfNoActive(ctx, a)
	require.NoError(t, err)
	require.True(t, x, "the first task must enqueue")

	// A same-tick rewrite with coarse stamps identical to a's: same identity,
	// but the anchor is inside the racy window → not provably same.
	b := identicalTo("b", a)
	y, err := q.EnqueueIfNoActive(ctx, b)
	require.NoError(t, err)
	require.True(t, y,
		"racy-window candidate (stamp >= anchor-2s) is not provably the same version: it MUST enqueue")
}

// TestEnqueueIfNoActive_ProvablySame_Rejected — the suppression half: the
// anchor is far AFTER the stamps (well beyond the 2s granularity), so identity
// equality plus a clear racy window DOES prove the same version → the
// duplicate is rejected.
func TestEnqueueIfNoActive_ProvablySame_Rejected(t *testing.T) {
	ctx := context.Background()
	q := openMemQueue(t)

	anchor := nowNs()
	stamps := anchor - 30e9 // 30s before the snapshot: far outside the 2s window
	a := &UploadTask{ID: "a", RuleID: "r", LocalPath: "p", FileSize: 4,
		FileMtime:   stamps / 1e9,
		FileMtimeNs: stamps, FileCtimeNs: stamps, FileDev: 1, FileIno: 2,
		FileSnapAtNs: anchor,
	}
	x, err := q.EnqueueIfNoActive(ctx, a)
	require.NoError(t, err)
	require.True(t, x)

	b := identicalTo("b", a)
	y, err := q.EnqueueIfNoActive(ctx, b)
	require.NoError(t, err)
	require.False(t, y,
		"a provably-same-version duplicate (identity equal + racy window clear) must be rejected")
}

// TestEnqueueIfNoActive_FutureAnchor_MustEnqueue — the ledger's clock-rollback
// guard mirrored in the enqueue guard: an active task whose snapshot anchor
// claims a moment in the future (clock stepped back after the stat) makes the
// racy judgement meaningless → Undecidable → the candidate must enqueue.
func TestEnqueueIfNoActive_FutureAnchor_MustEnqueue(t *testing.T) {
	ctx := context.Background()
	q := openMemQueue(t)

	anchor := nowNs() + 3600e9 // an hour in the future (clock rolled back)
	stamps := anchor - 30e9
	a := &UploadTask{ID: "a", RuleID: "r", LocalPath: "p", FileSize: 4,
		FileMtime:   stamps / 1e9,
		FileMtimeNs: stamps, FileCtimeNs: stamps, FileDev: 1, FileIno: 2,
		FileSnapAtNs: anchor,
	}
	x, err := q.EnqueueIfNoActive(ctx, a)
	require.NoError(t, err)
	require.True(t, x)

	b := identicalTo("b", a)
	y, err := q.EnqueueIfNoActive(ctx, b)
	require.NoError(t, err)
	require.True(t, y,
		"a future anchor cannot order the stamps: not provably same → must enqueue")
}

// TestEnqueueIfNoActive_ZeroMtimeNs_MustEnqueue — review-r3 blocker 2 tail
// note: FileMtimeNs == 0 while the other identity fields are non-zero is an
// INCOMPLETE identity ("unknown" field) → the guard cannot prove the same
// version → must enqueue.
func TestEnqueueIfNoActive_ZeroMtimeNs_MustEnqueue(t *testing.T) {
	ctx := context.Background()
	q := openMemQueue(t)

	anchor := nowNs()
	stamps := anchor - 30e9
	a := &UploadTask{ID: "a", RuleID: "r", LocalPath: "p", FileSize: 4,
		FileMtime:   stamps / 1e9,
		FileMtimeNs: stamps, FileCtimeNs: stamps, FileDev: 1, FileIno: 2,
		FileSnapAtNs: anchor,
	}
	x, err := q.EnqueueIfNoActive(ctx, a)
	require.NoError(t, err)
	require.True(t, x)

	// b has the same identity EXCEPT MtimeNs == 0 (unknown).
	b := identicalTo("b", a)
	b.FileMtimeNs = 0
	y, err := q.EnqueueIfNoActive(ctx, b)
	require.NoError(t, err)
	require.True(t, y,
		"an identity with an unknown (zero) mtime_ns can never be proven the same version: must enqueue")
}

// TestEnqueueIfNoActive_ActiveWithoutAnchor_MustEnqueue — an active task row
// written BEFORE the file_snap_at_ns column existed (or by an unknown-path)
// carries anchor 0: the racy judgement cannot run → undecidable → enqueue.
// (Mirrors the ledger's anchor-0 → Undecidable guard.)
func TestEnqueueIfNoActive_ActiveWithoutAnchor_MustEnqueue(t *testing.T) {
	ctx := context.Background()
	q := openMemQueue(t)

	anchor := nowNs()
	stamps := anchor - 30e9
	a := &UploadTask{ID: "a", RuleID: "r", LocalPath: "p", FileSize: 4,
		FileMtime:   stamps / 1e9,
		FileMtimeNs: stamps, FileCtimeNs: stamps, FileDev: 1, FileIno: 2,
		// FileSnapAtNs deliberately 0 (legacy row).
	}
	x, err := q.EnqueueIfNoActive(ctx, a)
	require.NoError(t, err)
	require.True(t, x)

	b := identicalTo("b", a)
	y, err := q.EnqueueIfNoActive(ctx, b)
	require.NoError(t, err)
	require.True(t, y,
		"an active task without a recorded snapshot anchor cannot prove same-version: the candidate must enqueue")
}

// TestEnqueueIfNoActive_ConcurrentSameVersion_SingleInsert — the concurrency
// requirement: two goroutines enqueueing the same version must not both
// insert. The verdict+insert runs inside one BEGIN IMMEDIATE transaction, so
// the second goroutine's verdict sees the first goroutine's committed row.
func TestEnqueueIfNoActive_ConcurrentSameVersion_SingleInsert(t *testing.T) {
	q, err := Open(filepath.Join(t.TempDir(), "q.db"))
	require.NoError(t, err)
	defer q.Close()
	ctx := context.Background()

	anchor := nowNs()
	stamps := anchor - 30e9
	mk := func(id string) *UploadTask {
		return &UploadTask{ID: id, RuleID: "r", LocalPath: "p", FileSize: 4,
			FileMtime:   stamps / 1e9,
			FileMtimeNs: stamps, FileCtimeNs: stamps, FileDev: 1, FileIno: 2,
			FileSnapAtNs: anchor,
		}
	}

	const n = 8
	var wg sync.WaitGroup
	accepted := make([]bool, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, err := q.EnqueueIfNoActive(ctx, mk(fmt.Sprintf("t%d", i)))
			if err == nil {
				accepted[i] = ok
			}
		}(i)
	}
	wg.Wait()

	rows := countPathRows(t, q, "r", "p")
	require.Equal(t, 1, rows,
		"concurrent enqueue of the same version must admit exactly one task (got %d rows)", rows)
	admitted := 0
	for _, ok := range accepted {
		if ok {
			admitted++
		}
	}
	require.Equal(t, 1, admitted, "exactly one goroutine may observe ok=true")
}
