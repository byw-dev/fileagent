package queue

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"
)

// ── Rework-13: platform-recreate identities must never be provably-same ──────
//
// PR #144's CI exposed two platform facts the old TestStat_RecreateChangesInode
// assumed away:
//   - Ubuntu/ext4: a delete-then-immediately-recreate REUSED the inode
//     (TestStat_RecreateChangesInode: `Should not be: 0x580dc5` — Ino equal);
//   - Windows/NTFS: the two stats landed in the same time tick, so ChangeTime
//     was equal (`Should not be: 1794303049484199400`).
//
// The design never promised a specific field changes — only that a recreated
// candidate cannot be PROVEN the same version. This file pins the verdict for
// every combination the platforms can produce, plus the end-to-end
// DecideProcessed behaviour (differing content → Recollect), deterministically:
// no sleeps, no filesystem, pure identity construction.

// recreatedIdentityCase enumerates what delete+recreate-with-restored-mtime
// can look like on each platform:
//   - "ext4-inode-reuse": ino identical, ctime advanced (create stamps it);
//   - "ntfs-same-tick": ino different, ctime identical (same timestamp tick);
//   - "both-changed": the darwin/APFS-friendly case;
//   - "both-same-tick": worst case — everything identical except the stat
//     moment, i.e. the stamps all sit inside the racy window.
type recreatedIdentityCase struct {
	name     string
	mutate   func(after *FileIdentity) // applied to a copy of the recorded A identity
	prependA timeNanos                 // offsets (ns) applied to A's stamps/anchor
}

type timeNanos = int64

func TestRecreatedIdentities_NeverProvableSame(t *testing.T) {
	// Recorded identity A: uploaded long ago, stamps far in the past so the
	// racy window (anchor − 2s) is comfortably clear of the ledger's own
	// stamps. The recreate happens NOW, so the candidate's anchor is now.
	baseA := FileIdentity{
		MtimeNs:  1_000_000_000_000, // stamps T
		CtimeNs:  1_000_000_000_000,
		Dev:      1,
		Ino:      42,
		Size:     4,
		SnapAtNs: 1_000_001_000_000, // anchor = T + 1s: the upload stat happened 1s after the stamps
	}
	// Candidate B is created "now": anchor now, mtime restored to A's, and
	// per-case ino/ctime.
	now := int64(9_000_000_000_000)

	cases := []struct {
		name  string
		ino   uint64
		ctime int64
	}{
		{"ext4_inode_reuse_ctime_advances", 42, now},        // ino reused, ctime new
		{"ntfs_new_ino_same_tick_ctime", 43, baseA.CtimeNs}, // new ino, ctime unchanged
		{"darwin_both_change", 43, now},                     // friendly case
		{"worst_all_fields_equal", 42, baseA.CtimeNs},       // ino reused AND ctime in the same tick: every kernel field identical to the record
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The recorded side's anchor is the snapshot moment of A's upload
			// — 1s AFTER A's stamps (stat happens at upload, stamps predate
			// it). The candidate is a recreate stat'ed NOW.
			rec := baseA
			cur := FileIdentity{
				MtimeNs:  baseA.MtimeNs, // mtime restored (cp -p / recreate)
				CtimeNs:  tc.ctime,
				Dev:      baseA.Dev,
				Ino:      tc.ino,
				Size:     baseA.Size,
				SnapAtNs: now, // candidate stat'ed now
			}
			// Record-side anchor: T+1s (the upload stat happened 1s after
			// A's stamps). The worst case's stamps T sit within anchor−2s
			// (= T−1s) → racy → Undecidable under the fix.
			anchor := baseA.SnapAtNs

			// The invariant: no platform-recreate combination may be provably
			// the same version. The worst case (every field identical: ino
			// reused, ctime unchanged) has stamps T with anchor T+1s — inside
			// anchor−2s (= T−1s) → racy → Undecidable. The other cases differ
			// in some field → Changed. Either way: NOT provably same.
			require.False(t, SameVersionProvable(rec, cur, anchor),
				"a recreated candidate must never be provably the same version (case %s)", tc.name)
		})
	}

	// And the sanity counterpart: a genuinely untouched file (identical
	// identity, stamps far before anchor−2s) IS provably same — proving the
	// predicate is not just always-false.
	cur := baseA
	require.True(t, SameVersionProvable(baseA, cur, baseA.SnapAtNs+10_000_000_000),
		"sanity: an untouched identical version (anchor ≥ stamps+G) must remain provably same")
}

// TestDecideProcessed_RecreatedContentDiffers_Recollects — deterministic
// end-to-end: a real Queue whose ledger holds A's identity; the candidate
// carries the recreated identity and its content hash DIFFERS from the
// ledger's. DecideProcessed must hash-confirm and Recollect because the
// content differs.
//
// ⚠️ Rework-13 honest note: every case below makes the candidate's ctime (or
// ino) DIFFER from the recorded row, so the prefilter verdict is
// **VerdictChanged** — the racy-window branch is NOT exercised here. The
// all-fields-equal + anchor=stamps+1s racy variant lives in
// TestDecideProcessed_Recreated*RacyWindow below.
func TestDecideProcessed_RecreatedContentDiffers_Recollects(t *testing.T) {
	ctx := context.Background()

	cases := []struct {
		name  string
		ino   uint64
		ctime int64
	}{
		{"ext4_inode_reuse", 42, int64(9_000_000_000_000)},
		{"ntfs_new_ino_same_tick", 43, 1_000_000_000_000},
		{"worst_all_stamps_now", 42, int64(9_000_000_000_000)},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q, err := Open(":memory:")
			require.NoError(t, err)
			defer q.Close()

			rec := FileIdentity{
				MtimeNs:  1_000_000_000_000,
				CtimeNs:  1_000_000_000_000,
				Dev:      1,
				Ino:      42,
				Size:     4,
				SnapAtNs: 2_000_000_000_000,
			}
			require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
				ID: "rec-1", RuleID: "r", LocalPath: "p",
				FileSize:     4,
				SHA256:       "sha-of-old-content",
				Ident:        rec,
				UploadedAtNs: rec.SnapAtNs,
			}))

			// Candidate: recreated "now", mtime restored to A's.
			cur := FileIdentity{
				MtimeNs:  rec.MtimeNs,
				CtimeNs:  tc.ctime,
				Dev:      rec.Dev,
				Ino:      tc.ino,
				Size:     rec.Size,
				SnapAtNs: int64(9_000_000_000_000),
			}
			hashCalls := 0
			dec, err := q.DecideProcessed(ctx, "r", "p", cur, func() (HashResult, error) {
				hashCalls++
				return HashResult{
					SHA256:           "sha-of-new-content", // content differs
					Size:             rec.Size,
					Identity:         cur,
					IdentityComplete: true,
				}, nil
			})
			require.NoError(t, err)
			require.Equal(t, Recollect, dec.Verdict,
				"a recreated file whose content differs must Recollect (case %s; verdict %s prefilter %s)",
				tc.name, dec.Verdict, dec.Prefilter)
			require.Equal(t, 1, hashCalls, "the decision must hash-confirm (not free-skip)")

			fmt.Printf("r13_recreate case=%s verdict=%s prefilter=%s hash_calls=%d\n",
				tc.name, dec.Verdict, dec.Prefilter, hashCalls)
		})
	}
}

// TestDecideProcessed_RecreatedSameContent_NeverFreeSkip — the acceptance
// counterpart: identical content hash-confirms equal → Refresh or Skip is
// allowed ONLY when the prefilter proved Same.
//
// ⚠️ Rework-13 honest note: the first candidate here has ctime differing
// from the record → **VerdictChanged** branch (hash-confirm with differing
// content → Recollect). The racy-window variant (all fields equal, anchor =
// stamps+1s → VerdictUndecidable → hash-equal → RefreshAndSkip) lives in
// TestDecideProcessed_RecreatedSameContent_RacyWindow_RefreshAndSkip below.
func TestDecideProcessed_RecreatedSameContent_NeverFreeSkip(t *testing.T) {
	ctx := context.Background()
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	rec := FileIdentity{
		MtimeNs:  1_000_000_000_000,
		CtimeNs:  1_000_000_000_000,
		Dev:      1,
		Ino:      42,
		Size:     4,
		SnapAtNs: 2_000_000_000_000,
	}
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "rec-1", RuleID: "r", LocalPath: "p",
		FileSize: 4, SHA256: "sha-same",
		Ident:        rec,
		UploadedAtNs: rec.SnapAtNs,
	}))

	// Racy-window recreated candidate: the recreate's stamps land 1s BEFORE
	// the ledger anchor (uploaded_at_ns) — inside anchor−2s — even though
	// every kernel field is identical to the recorded row (extreme stamp
	// cache: ino reused AND ctime in the same tick). This is the exact
	// scenario the racy window exists for: the wall clock cannot order the
	// stamps against a fresh anchor → Undecidable → must hash-confirm.
	rec = FileIdentity{
		MtimeNs:  1_000_000_002_000_000_000,
		CtimeNs:  1_000_000_002_000_000_000,
		Dev:      1,
		Ino:      42,
		Size:     4,
		SnapAtNs: 1_000_000_003_000_000_000, // recorded: anchor 1s after A's stamps
	}
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "rec-2", RuleID: "r2", LocalPath: "p2",
		FileSize: 4, SHA256: "sha-same",
		Ident:        rec,
		UploadedAtNs: rec.SnapAtNs,
	}))
	cur := FileIdentity{
		MtimeNs:  1_000_000_002_000_000_000, // mtime restored to A's
		CtimeNs:  1_000_000_002_500_000_000, // recreated 0.5s after A's stamps: inside anchor−2s
		Dev:      rec.Dev,
		Ino:      rec.Ino, // ext4 inode reuse
		Size:     rec.Size,
		SnapAtNs: 1_000_000_003_000_000_000,
	}
	hashCalls := 0
	dec, err := q.DecideProcessed(ctx, "r2", "p2", cur, func() (HashResult, error) {
		hashCalls++
		return HashResult{
			SHA256:           "sha-same", // identical content
			Size:             rec.Size,
			Identity:         cur,
			IdentityComplete: true,
		}, nil
	})
	require.NoError(t, err)
	require.Equal(t, 1, hashCalls,
		"the recreated candidate sits inside the racy window: it must hash-confirm, never free-skip")
	require.NotEqual(t, Skip, dec.Verdict,
		"a bare Skip without hash confirmation is forbidden for a racy-window candidate")
	fmt.Printf("r13_same_content verdict=%s hash_calls=%d (RefreshAndSkip allowed after hash-equal)\n",
		dec.Verdict, hashCalls)
}

// TestDecideProcessed_RecreatedContentDiffers_RacyWindow_Recollects — the
// racy-window variant the rework-13 versions missed: the recorded identity
// and the candidate are FULLY EQUAL on every kernel field (ino reused, ctime
// in the same tick as the record) and the recorded anchor = stamps + 1s, so
// the stamps sit inside anchor−2s → Prefilter == VerdictUndecidable → the
// decision MUST hash-confirm (hash_calls == 1) and, the content differing,
// Recollect. Mutation: removing the racy window from PrefilterVerdict turns
// this into VerdictSame → Skip → hash_calls == 0 → this test goes red.
func TestDecideProcessed_RecreatedContentDiffers_RacyWindow_Recollects(t *testing.T) {
	ctx := context.Background()
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	// Recorded A: stat'ed 1s AFTER its stamps (the upload stat moment).
	rec := FileIdentity{
		MtimeNs:  1_000_000_000_000,
		CtimeNs:  1_000_000_000_000,
		Dev:      1,
		Ino:      42,
		Size:     4,
		SnapAtNs: 1_001_000_000_000, // anchor = stamps + 1s → inside anchor−2s
	}
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "rec-1", RuleID: "r", LocalPath: "p",
		FileSize:     4,
		SHA256:       "sha-of-old-content",
		Ident:        rec,
		UploadedAtNs: rec.SnapAtNs,
	}))

	// Candidate: recreated with EVERY kernel field equal to the record
	// (extreme stamp cache: ino reused, ctime in the same tick).
	cur := FileIdentity{
		MtimeNs:  rec.MtimeNs,
		CtimeNs:  rec.CtimeNs,
		Dev:      rec.Dev,
		Ino:      rec.Ino,
		Size:     rec.Size,
		SnapAtNs: int64(9_000_000_000_000), // candidate stat'ed much later
	}

	// Precondition: the prefilter really is Undecidable (racy branch).
	require.Equal(t, VerdictUndecidable, PrefilterVerdict(rec, cur, rec.SnapAtNs),
		"precondition: all-equal fields + anchor=stamps+1s must be VerdictUndecidable (racy)")

	hashCalls := 0
	dec, err := q.DecideProcessed(ctx, "r", "p", cur, func() (HashResult, error) {
		hashCalls++
		return HashResult{
			SHA256:           "sha-of-new-content", // content differs
			Size:             rec.Size,
			Identity:         cur,
			IdentityComplete: true,
		}, nil
	})
	require.NoError(t, err)
	require.Equal(t, VerdictUndecidable, dec.Prefilter,
		"the prefilter must be Undecidable (racy window), not Changed")
	require.Equal(t, 1, hashCalls, "the racy candidate must hash-confirm (mutation: window removed → 0)")
	require.Equal(t, Recollect, dec.Verdict,
		"racy-window + differing content must Recollect")
	fmt.Printf("r13_racy content_differs prefilter=%s verdict=%s hash_calls=%d\n",
		dec.Prefilter, dec.Verdict, hashCalls)
}

// TestDecideProcessed_RecreatedSameContent_RacyWindow_RefreshAndSkip — the
// acceptance counterpart with the same racy construction: all kernel fields
// equal + anchor = stamps + 1s → VerdictUndecidable → the candidate MUST
// hash-confirm (hash_calls == 1) and, the content being identical,
// RefreshAndSkip (never a bare Skip without a hash read).
func TestDecideProcessed_RecreatedSameContent_RacyWindow_RefreshAndSkip(t *testing.T) {
	ctx := context.Background()
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	rec := FileIdentity{
		MtimeNs:  1_000_000_000_000,
		CtimeNs:  1_000_000_000_000,
		Dev:      1,
		Ino:      42,
		Size:     4,
		SnapAtNs: 1_001_000_000_000, // anchor = stamps + 1s → inside anchor−2s
	}
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "rec-1", RuleID: "r", LocalPath: "p",
		FileSize:     4,
		SHA256:       "sha-same",
		Ident:        rec,
		UploadedAtNs: rec.SnapAtNs,
	}))

	cur := FileIdentity{
		MtimeNs:  rec.MtimeNs,
		CtimeNs:  rec.CtimeNs,
		Dev:      rec.Dev,
		Ino:      rec.Ino,
		Size:     rec.Size,
		SnapAtNs: int64(9_000_000_000_000),
	}

	require.Equal(t, VerdictUndecidable, PrefilterVerdict(rec, cur, rec.SnapAtNs),
		"precondition: all-equal fields + anchor=stamps+1s must be VerdictUndecidable (racy)")

	hashCalls := 0
	dec, err := q.DecideProcessed(ctx, "r", "p", cur, func() (HashResult, error) {
		hashCalls++
		return HashResult{
			SHA256:           "sha-same", // identical content
			Size:             rec.Size,
			Identity:         cur,
			IdentityComplete: true,
		}, nil
	})
	require.NoError(t, err)
	require.Equal(t, VerdictUndecidable, dec.Prefilter,
		"the prefilter must be Undecidable (racy window), not Changed")
	require.Equal(t, 1, hashCalls, "the racy candidate must hash-confirm (mutation: window removed → 0)")
	require.Equal(t, RefreshAndSkip, dec.Verdict,
		"racy-window + identical content must hash-confirm equal and refresh")
	fmt.Printf("r13_racy same_content prefilter=%s verdict=%s hash_calls=%d\n",
		dec.Prefilter, dec.Verdict, hashCalls)
}
