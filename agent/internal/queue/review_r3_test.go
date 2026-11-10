package queue

import (
	"context"
	"fmt"
	"testing"
)

// ── Review-r3 reproductions, ported as regression tests ─────────────────────

// TestReviewR3UnknownActive_Ported — blocker 2 (ported): a candidate with an
// UNKNOWN identity (zero ctime/dev/ino — failed stat, filesystem without the
// fields) must NOT be suppressed by the active-task guard, even when the old
// second-granularity tuple matches a pending task. Pre-fix the guard fell
// back to the tuple and rejected the candidate (rewritten_candidate=false);
// the fix delivers/enqueues anything not provably the same version.
func TestReviewR3UnknownActive_Ported(t *testing.T) {
	q, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	ctx := context.Background()
	a := &UploadTask{ID: "a", RuleID: "r", LocalPath: "p", FileSize: 4, FileMtime: 42}
	b := &UploadTask{ID: "b", RuleID: "r", LocalPath: "p", FileSize: 4, FileMtime: 42}
	x, err := q.EnqueueIfNoActive(ctx, a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := q.EnqueueIfNoActive(ctx, b)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("unknown_identity first=%v rewritten_candidate=%v\n", x, y)
	if !x {
		t.Fatal("the first task must enqueue")
	}
	if !y {
		t.Fatal("a candidate with an unknown identity must enqueue (never suppressed by the legacy tuple)")
	}
}

// TestReviewR3CoarseActive_Ported — blocker 1's queue half (ported), with its
// ORIGINAL intent restored (rework-6; the rework-5 round had flipped it to
// pin the suppression, which was exactly the bug the reviewer reported): two
// candidates with the SAME full kernel identity (coarse-stamp collision)
// where the active task's snapshot anchor sits INSIDE the racy window of the
// stamps — the identity-equal candidate is NOT provably the same version and
// MUST enqueue. The provably-same variant (anchor far after the stamps)
// stays suppressed in TestEnqueueIfNoActive_ProvablySame_Rejected; both run
// through the ONE SameVersionProvable predicate.
func TestReviewR3CoarseActive_Ported(t *testing.T) {
	q, err := Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()

	// anchor−G falls one ns BEFORE the stamps → both stamps ≥ anchor−G →
	// racy → Undecidable → not provably same (exact G ordering, no slack).
	G := int64(2 * 1e9)
	anchor := nowNs()
	stamps := anchor - G + 1
	a := &UploadTask{ID: "a", RuleID: "r", LocalPath: "p", FileSize: 4,
		FileMtime:   stamps / 1e9,
		FileMtimeNs: stamps, FileCtimeNs: stamps, FileDev: 1, FileIno: 2,
		FileSnapAtNs: anchor}
	b := *a
	b.ID = "b"
	x, err := q.EnqueueIfNoActive(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	y, err := q.EnqueueIfNoActive(context.Background(), &b)
	if err != nil {
		t.Fatal(err)
	}
	fmt.Printf("equal_coarse_identity first=%v rewritten_candidate=%v\n", x, y)
	if !x {
		t.Fatal("the first task must enqueue")
	}
	if !y {
		t.Fatal("a coarse same-tick rewrite with an anchor inside the racy window is NOT " +
			"provably the same version: it MUST enqueue (review-r3 blocker 1, enqueue half)")
	}
}
