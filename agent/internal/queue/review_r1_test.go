package queue

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/fileid"
	"github.com/stretchr/testify/require"
)

// ── Review-r1 reproductions, ported as regression tests ─────────────────────
//
// The reviewer's sandbox tests (bin/review-r1/sandbox) demonstrated three
// independent ledger-identity/content decoupling paths. These ports assert the
// FIXED behaviour; on the pre-fix commit each one demonstrates the flaw (the
// red run is in bin/ic48/evidence/rework2-red.txt).

// hashResultHex is the sha256 hex of s (test helper).
func hashResultHex(s string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(s)))
}

// TestReviewPathSwap_Ported — blocker 2: the dedup hash-confirm must hash and
// stat the SAME open. Construction: path p initially → b (uncollected
// content "BBBB"); ledger holds A's hash ("AAAA"). The buggy hash callback
// swaps p → a, reads, swaps back → b. The verdict hashes A's bytes but (pre
// fix) refreshes the ledger with B's path-statted identity + a NEW anchor →
// the next check skips B forever, though B was never uploaded.
//
// Fixed contract: DecideProcessed's hash callback opens the file ITSELF and
// returns {identity, sha256, complete}; the ledger refresh uses ONLY that
// identity. With the callback reading a (hash matches A), the refresh may
// proceed ONLY if the callback's identity matches what was hashed — here the
// callback's fd sees A, so the second check (path → b, content B) must NOT
// skip: its hash differs from A's → Recollect.
func TestReviewPathSwap_Ported(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	d := t.TempDir()
	a := filepath.Join(d, "a")
	b := filepath.Join(d, "b")
	p := filepath.Join(d, "link")
	require.NoError(t, os.WriteFile(a, []byte("AAAA"), 0o600))
	require.NoError(t, os.WriteFile(b, []byte("BBBB"), 0o600))
	require.NoError(t, os.Symlink(b, p))
	// Age the stamps out of the racy window.
	time.Sleep(2100 * time.Millisecond)

	sumA := hashResultHex("AAAA")
	idA, err := fileid.Stat(p) // p → b right now; identity is b's (the buggy caller's path stat)
	require.NoError(t, err)
	cur := FileIdentity{
		MtimeNs: idA.MtimeNs, CtimeNs: idA.CtimeNs, Dev: idA.Dev, Ino: idA.Ino,
		Size: idA.Size, SnapAtNs: idA.SnapAtNs,
	}
	old := cur
	old.Ino++ // ledger's recorded identity differs from current → undecidable → hash-confirm

	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "x", RuleID: "r", LocalPath: p, FileSize: 4, SHA256: sumA,
		Ident: old, UploadedAtNs: cur.SnapAtNs,
	}))

	// Round 1: the buggy swap — path re-open inside the callback reads A
	// (hash matches the ledger), while the caller's path-statted cur is B's.
	// FIXED behaviour: the callback returns A's fd identity, so the refresh
	// (if it happens) writes A's identity — never B's path identity.
	decision, err := q.DecideProcessed(context.Background(), "r", p, cur,
		func() (HashResult, error) {
			require.NoError(t, os.Remove(p))
			require.NoError(t, os.Symlink(a, p))
			defer func() {
				require.NoError(t, os.Remove(p))
				require.NoError(t, os.Symlink(b, p))
			}()
			bb, err := os.ReadFile(p)
			if err != nil {
				return HashResult{}, err
			}
			// The callback opens the file it reads and returns THAT identity
			// (fixed contract: the caller's path-stat cur is ignored for the
			// ledger write).
			fid, err := fileid.Stat(p)
			if err != nil {
				return HashResult{}, err
			}
			return HashResult{
				SHA256:           hashResultHex(string(bb)),
				Identity:         FileIdentity{MtimeNs: fid.MtimeNs, CtimeNs: fid.CtimeNs, Dev: fid.Dev, Ino: fid.Ino, Size: fid.Size, SnapAtNs: fid.SnapAtNs},
				IdentityComplete: true,
			}, nil
		})
	require.NoError(t, err)
	t.Logf("first=%s", decision.Verdict)

	// The ledger must now describe A (the hashed bytes' fd identity), NOT B:
	rec, err := q.GetProcessedIdentity("r", p)
	require.NoError(t, err)
	require.Equal(t, sumA, rec.SHA256)
	require.NotEqual(t, cur.Ino, rec.Ident.Ino,
		"the ledger must carry the hashed file's identity (the callback's fd), never the caller's path-stat identity")

	// Round 2 (after the racy window): path → b, content B, identity b's.
	// B was never uploaded (hash mismatch with the recorded A) → Recollect.
	time.Sleep(2100 * time.Millisecond)
	fresh, err := fileid.Stat(p)
	require.NoError(t, err)
	cur.SnapAtNs = fresh.SnapAtNs
	decision, err = q.DecideProcessed(context.Background(), "r", p, cur,
		func() (HashResult, error) {
			bb, err := os.ReadFile(p)
			if err != nil {
				return HashResult{}, err
			}
			fid, err := fileid.Stat(p)
			if err != nil {
				return HashResult{}, err
			}
			return HashResult{
				SHA256:           hashResultHex(string(bb)),
				Identity:         FileIdentity{MtimeNs: fid.MtimeNs, CtimeNs: fid.CtimeNs, Dev: fid.Dev, Ino: fid.Ino, Size: fid.Size, SnapAtNs: fid.SnapAtNs},
				IdentityComplete: true,
			}, nil
		})
	require.NoError(t, err)
	t.Logf("second=%s current=BBBB uploaded=AAAA", decision.Verdict)
	require.Equal(t, Recollect, decision.Verdict,
		"b's content was never uploaded (hash differs from the recorded A) → must re-collect, never skip")
}

// TestReviewConcurrentAck_Ported — blocker 3: the refresh must be a CAS on
// the row the decision READ. Construction: ledger = A (ino 1). During the
// hash-confirm, a concurrent ack overwrites the row to C (ino 3, hash-C).
// The buggy refresh then unconditionally UPDATEs by (rule,path), blending
// b's identity onto C's hash; the next check for b skips though C holds the
// bucket. Fixed: the CAS requires the row to still be A; 0 rows affected →
// Recollect (fail-closed: the decision's basis vanished mid-flight).
func TestReviewConcurrentAck_Ported(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	now := time.Now().UnixNano()
	a := FileIdentity{MtimeNs: now - 10e9, CtimeNs: now - 10e9, Dev: 1, Ino: 1, Size: 4, SnapAtNs: now}
	b := a
	b.Ino = 2
	c := a
	c.Ino = 3

	up := func(id FileIdentity, sha string) {
		require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
			ID: "id", RuleID: "r", LocalPath: "p", FileSize: 4, SHA256: sha,
			Ident: id, UploadedAtNs: id.SnapAtNs,
		}))
	}
	up(a, "hash-A")

	// Control subtest: the SAME inputs without any concurrent write must
	// reach the CAS and succeed — RefreshAndSkip with the ledger refreshed
	// to b's identity (proving the path really goes through
	// refreshProcessedIdentityCAS; the racing subtest below relies on this).
	t.Run("control-no-race-refreshes", func(t *testing.T) {
		// Fresh queue: seed A, candidate is b with a hash matching A.
		qc, err := Open(":memory:")
		require.NoError(t, err)
		defer qc.Close()
		upc := func(id FileIdentity, sha string) {
			require.NoError(t, qc.UpsertProcessedFile(&ProcessedFile{
				ID: "id", RuleID: "r", LocalPath: "p", FileSize: 4, SHA256: sha,
				Ident: id, UploadedAtNs: id.SnapAtNs,
			}))
		}
		upc(a, "hash-A")
		dec, err := qc.DecideProcessed(context.Background(), "r", "p", b,
			func() (HashResult, error) {
				return HashResult{
					SHA256:           "hash-A", // b's bytes happen to match A's hash
					Identity:         b,        // the callback's same-fd snapshot of b
					IdentityComplete: true,
				}, nil
			})
		require.NoError(t, err)
		require.Equal(t, RefreshAndSkip, dec.Verdict,
			"control: with the basis row untouched, the CAS refresh must succeed")
		rec, err := qc.GetProcessedIdentity("r", "p")
		require.NoError(t, err)
		require.Equal(t, int64(2), int64(rec.Ident.Ino),
			"control: the ledger must now carry b's identity (the CAS write landed)")
		require.Equal(t, "hash-A", rec.SHA256)
	})

	// Racing subtest: the hash callback simulates the racing ack — it
	// overwrites the row to C (as a concurrent CompleteReportedIdentity
	// would) and returns a COMPLETE hash result for b (hash matches A's
	// recorded hash, identity = b with its snapshot anchor). The complete
	// flag is what lets DecideProcessed reach the CAS at all: without it the
	// !IdentityComplete branch returns Recollect before the CAS runs (the
	// reason this test originally passed for the wrong reason — coordinator
	// mutation C2).
	decision, err := q.DecideProcessed(context.Background(), "r", "p", b,
		func() (HashResult, error) {
			up(c, "hash-C")
			return HashResult{
				SHA256:           "hash-A", // b's bytes match A's recorded hash
				Identity:         b,        // with SnapAtNs — the refresh's new anchor
				IdentityComplete: true,
			}, nil
		})
	require.NoError(t, err)
	record, err := q.GetProcessedIdentity("r", "p")
	require.NoError(t, err)
	t.Logf("first=%s ledger_inode=%d ledger_sha=%s", decision.Verdict, record.Ident.Ino, record.SHA256)

	// The CAS must have detected the lost basis: the row is C's, not the A we
	// read. The refresh must NOT have blended b onto C — the ledger keeps C's
	// hash with C's identity, and the verdict is Recollect (or a re-read; we
	// chose Recollect, see refreshProcessedIdentity).
	require.Equal(t, "hash-C", record.SHA256,
		"the concurrent ack's row must survive the losing refresh (CAS on the read row)")
	require.Equal(t, int64(3), int64(record.Ident.Ino),
		"the concurrent ack's identity must survive the losing refresh")
	require.Equal(t, Recollect, decision.Verdict,
		"a decision whose basis row changed mid-flight must not claim RefreshAndSkip")

	// Second check for b (hash b ≠ C's recorded hash) → Recollect, never
	// skip. The verdict comes from the HASH MISMATCH branch (checked before
	// the complete-flag branch in DecideProcessed), so an incomplete result
	// is fine here — the verdict path under test is the mismatch, and it is
	// reached regardless.
	decision2, err := q.DecideProcessed(context.Background(), "r", "p", b,
		func() (HashResult, error) {
			return HashResult{SHA256: "hash-B"}, nil // deliberately incomplete: mismatch branch does not read it
		})
	require.NoError(t, err)
	t.Logf("second=%s current=B", decision2.Verdict)
	require.Equal(t, Recollect, decision2.Verdict,
		"b's content differs from the recorded hash → re-collect, never skip")
}

// TestReviewConcurrentAck_ShaOnlyWrite_CASDetects — rework 3 requirement 2:
// the CAS's sha256 condition specifically. A concurrent write changes ONLY
// the recorded sha256 while keeping the identity fields and the anchor —
// e.g. a duplicate ack for the same identity carrying a different hash
// (defensive guard: in the current architecture this is believed
// unreachable — acks for one task carry one immutable report payload, and
// CompleteReportedIdentity upserts the same (identity, sha) pair for the
// same task; but the CAS must not silently blend a refresh across ANY row
// mutation, so the guard stays).
//
// Construction: ledger = (ident X, sha "sha-old"); the hash callback races a
// write of (ident X, sha "sha-new") — same identity, different sha. The CAS
// WHERE requires sha256 == "sha-old" → 0 rows → Recollect, and the racing
// write's "sha-new" must survive.
func TestReviewConcurrentAck_ShaOnlyWrite_CASDetects(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	now := time.Now().UnixNano()
	x := FileIdentity{MtimeNs: now - 10e9, CtimeNs: now - 10e9, Dev: 1, Ino: 1, Size: 4, SnapAtNs: now - 30e9}
	cur := x // the candidate's identity equals the recorded one (aged → not racy)
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "id", RuleID: "r", LocalPath: "p", FileSize: 4, SHA256: "sha-old",
		Ident: x, UploadedAtNs: x.SnapAtNs,
	}))

	decision, err := q.DecideProcessed(context.Background(), "r", "p", cur,
		func() (HashResult, error) {
			// Racing write: SAME identity, DIFFERENT sha256 (a duplicate ack
			// carrying a different hash for the same file version).
			require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
				ID: "id", RuleID: "r", LocalPath: "p", FileSize: 4, SHA256: "sha-new",
				Ident: x, UploadedAtNs: x.SnapAtNs,
			}))
			return HashResult{
				SHA256:           "sha-old", // the hash the decision read
				Identity:         x,         // with SnapAtNs
				IdentityComplete: true,      // must reach the CAS
			}, nil
		})
	require.NoError(t, err)

	rec, err := q.GetProcessedIdentity("r", "p")
	require.NoError(t, err)
	t.Logf("verdict=%s ledger_sha=%s", decision.Verdict, rec.SHA256)
	require.Equal(t, "sha-new", rec.SHA256,
		"the racing sha-only write must survive the CAS (its sha differs from the read row)")
	require.Equal(t, Recollect, decision.Verdict,
		"the CAS must detect the sha-only mutation of the basis row (0 rows matched) → Recollect, never RefreshAndSkip")
}

// TestDecideProcessed_IncompleteHashIdentity_Recollects — rework-2 blocker 2
// (the mutation-detector half): a hash callback that returns a MATCHING sha
// but IdentityComplete=false (bytes and identity may diverge) must yield
// Recollect — never RefreshAndSkip — and the ledger row must stay untouched
// (identity/anchor unchanged). Killing the "ignore complete" mutation: with
// the check removed the verdict becomes RefreshAndSkip and the row is
// refreshed → red.
func TestDecideProcessed_IncompleteHashIdentity_Recollects(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	d := t.TempDir()
	f := filepath.Join(d, "torn.txt")
	require.NoError(t, os.WriteFile(f, []byte("AAAA"), 0o600))
	fid, err := fileid.Stat(f)
	require.NoError(t, err)
	// Ledger identity differs from current (undecidable) → hash-confirm.
	old := FileIdentity{MtimeNs: fid.MtimeNs, CtimeNs: fid.CtimeNs, Dev: fid.Dev, Ino: fid.Ino, Size: fid.Size, SnapAtNs: fid.SnapAtNs}
	oldLedger := old
	oldLedger.Ino++
	sha := hashResultHex("AAAA")
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "pf-torn", RuleID: "r", LocalPath: f, FileSize: 4, SHA256: sha,
		Ident: oldLedger, UploadedAtNs: old.SnapAtNs,
	}))

	cur := old // the caller path-stat identity (same underlying file here)
	dec, err := q.DecideProcessed(context.Background(), "r", f, cur,
		func() (HashResult, error) {
			return HashResult{
				SHA256:   sha, // matching hash…
				Identity: cur, // …and an identity — but…
				// …IdentityComplete=false: bytes and identity may diverge.
				// Deliberately incomplete: this test pins the !complete guard
				// (Recollect + ledger untouched); NOT testing the CAS here.
				IdentityComplete: false,
			}, nil
		})
	require.NoError(t, err)
	require.Equal(t, Recollect, dec.Verdict,
		"an incomplete hash identity must not RefreshAndSkip even when the hash matches")
	rec, err := q.GetProcessedIdentity("r", f)
	require.NoError(t, err)
	require.Equal(t, int64(oldLedger.Ino), int64(rec.Ident.Ino),
		"the losing refresh must not touch the ledger row")
}
