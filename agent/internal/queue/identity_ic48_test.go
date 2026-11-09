package queue

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/fileid"
	"github.com/stretchr/testify/require"
)

// ── IC-BUG-48 red tests ──────────────────────────────────────────────────────
//
// These tests use the REAL Queue (production functions) plus real files on
// disk and real sha256 hashing. They encode the three acceptance scenarios
// from the bug card plus the racy window and the legacy-row migration
// behaviour. They must run against master's IsProcessed-only behaviour and
// fail there (red), then pass once DecideProcessed exists (green).

// ic48RecordProcessed simulates a successful upload+ack: writes the
// processed-file ledger row with the file's full kernel identity, anchored at
// the identity's SNAPSHOT moment (v2: the anchor is when the stat was taken,
// not when the row is written).
func ic48RecordProcessed(t *testing.T, q *Queue, ruleID, path string, id fileid.Identity, sha string) {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	snapAt := id.SnapAtNs
	if snapAt == 0 {
		snapAt = time.Now().UnixNano()
	}
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID:           "pf-" + path,
		RuleID:       ruleID,
		LocalPath:    path,
		FileSize:     fi.Size(),
		FileMtime:    fi.ModTime().Unix(),
		SHA256:       sha,
		Ident:        FileIdentity{MtimeNs: id.MtimeNs, CtimeNs: id.CtimeNs, Dev: id.Dev, Ino: id.Ino, Size: id.Size, SnapAtNs: snapAt},
		UploadedAtNs: snapAt,
	}))
}

// ic48HashFile is the production hash-confirm function: full-content sha256.
func ic48HashFile(path string) func() (HashResult, error) {
	return func() (HashResult, error) {
		b, err := os.ReadFile(path)
		if err != nil {
			return HashResult{}, err
		}
		sum := sha256.Sum256(b)
		return HashResult{SHA256: hex.EncodeToString(sum[:]), Size: int64(len(b))}, nil
	}
}

// ic48Decide calls the candidate decision entry with the file's current
// identity (snapshot moment anchored at the stat).
func ic48Decide(t *testing.T, q *Queue, ruleID, path string) CandidateDecision {
	t.Helper()
	dec, err := ic48DecideErr(q, ruleID, path)
	require.NoError(t, err)
	return dec
}

// ic48DecideErr is ic48Decide returning the raw error (for negative tests).
func ic48DecideErr(q *Queue, ruleID, path string) (CandidateDecision, error) {
	id, err := fileid.Stat(path)
	if err != nil {
		return CandidateDecision{}, err
	}
	return q.DecideProcessed(context.Background(), ruleID, path,
		FileIdentity{MtimeNs: id.MtimeNs, CtimeNs: id.CtimeNs, Dev: id.Dev, Ino: id.Ino, Size: id.Size, SnapAtNs: id.SnapAtNs},
		ic48HashFile(path))
}

// TestIC48_SameSecondSameSizeRewrite_MustRecollect — acceptance scenario ①:
// a file rewritten within the same second at the same size must be
// re-collected. The nanosecond mtime and the ctime both change on a rewrite;
// mtime+size alone cannot see it (master's blind spot).
func TestIC48_SameSecondSameSizeRewrite_MustRecollect(t *testing.T) {
	ctx := context.Background()
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "same-second.bin", "AAAA")
	ic48RecordProcessed(t, q, "rule", path, ic48Stat(t, path), ic48SHA(t, path))

	// Same-second, same-size rewrite: identical length, different bytes. On
	// coarse (second-granularity) stamps the legacy tuple is unchanged; the
	// nanosecond mtime/ctime are not forgeable and will differ.
	ic48RewriteSameSize(t, path, "BBBB")

	dec := ic48Decide(t, q, "rule", path)
	require.Equal(t, Recollect, dec.Verdict,
		"a same-second same-size rewrite must be re-collected, not silently skipped")
	_ = ctx
}

// TestIC48_RebuildWithRestoredMtime_MustRecollect — acceptance scenario ②:
// the file is deleted and recreated in place with cp -p semantics (mtime
// restored via os.Chtimes, same size), but with DIFFERENT content. The new
// inode (and the fresh ctime) must force a re-collection.
func TestIC48_RebuildWithRestoredMtime_MustRecollect(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "rebuild.bin", "OLD1")
	oldStat := ic48Stat(t, path)
	ic48RecordProcessed(t, q, "rule", path, oldStat, ic48SHA(t, path))

	// Delete and recreate with different content, same size, mtime restored.
	fiOld, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	ic48WriteDir(t, filepath.Dir(path), "rebuild.bin", "NEW2")
	require.NoError(t, os.Chtimes(path, fiOld.ModTime(), fiOld.ModTime()))

	dec := ic48Decide(t, q, "rule", path)
	require.Equal(t, Recollect, dec.Verdict,
		"a delete-and-recreate with restored mtime must be re-collected (new inode)")
}

// TestIC48_IdenticalRebuild_SkipViaHashConfirm — acceptance scenario ③:
// content-identical rebuild may skip (ledger semantics: no new content to
// upload), but the row's identity must be refreshed so the NEXT check can
// trust the prefilter again.
func TestIC48_IdenticalRebuild_SkipViaHashConfirm(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "identical.bin", "SAME")
	oldStat := ic48Stat(t, path)
	ic48RecordProcessed(t, q, "rule", path, oldStat, ic48SHA(t, path))

	// Delete and recreate with IDENTICAL content. cp -p restores mtime (and
	// on APFS the restored nanosecond mtime can be bit-identical), but ctime
	// cannot be restored: the fresh inode carries a NEW ctime.
	fiOld, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	ic48WriteDir(t, filepath.Dir(path), "identical.bin", "SAME")
	require.NoError(t, os.Chtimes(path, fiOld.ModTime(), fiOld.ModTime()))
	newStat := ic48Stat(t, path)
	require.NotEqual(t, oldStat.CtimeNs, newStat.CtimeNs,
		"ctime must change on delete-and-recreate even with restored mtime (kernel guarantee)")

	dec := ic48Decide(t, q, "rule", path)
	// The prefilter sees the new inode/ctime against the recorded identity —
	// "changed" at the metadata level. The hash confirmation is what proves
	// the content is identical and turns that into refresh-and-skip; only a
	// candidate flow that reaches the hash may report Skip/RefreshAndSkip.
	require.Equal(t, RefreshAndSkip, dec.Verdict,
		"identical content must be confirmed by hash and skip with a refreshed row, never re-uploaded")

	// The ledger caught up (identity + anchor refreshed during hash confirm).
	// The refreshed anchor is the second check's snapshot moment, and the
	// file's stamps fall within G before it — so the second decision is
	// hash-confirmed AGAIN (RefreshAndSkip, not a bare Skip): correct
	// racily-clean behaviour. After G has passed (simulated by backdating the
	// anchor) the prefilter decides alone — convergence, git-style.
	dec2 := ic48Decide(t, q, "rule", path)
	require.Equal(t, RefreshAndSkip, dec2.Verdict,
		"inside the racy window after the refresh, the second check must hash-confirm again (content identical → skip)")
	rec, err := q.GetProcessedIdentity("rule", path)
	require.NoError(t, err)
	require.NotZero(t, rec.SnapshotAtNs, "the refresh must anchor the ledger at the snapshot moment")
	cur := ic48Stat(t, path)
	// Simulate that G has passed since the refresh: move the anchor FORWARD,
	// past the file's stamps + G. The anchor is "when the ledger identity was
	// stat'ed"; time passing means the anchor (a wall-clock moment) rises
	// above stamp + G, and then stamp < anchor − G → not racy → Same.
	// (Real convergence just waits; moving the anchor forward simulates the
	// identical ordering without a sleep.)
	require.NoError(t, q.SetProcessedIdentityAnchor("rule", path, cur.MtimeNs+racyGranularityNs*3))
	dec3, err := q.DecideProcessed(context.Background(), "rule", path,
		FileIdentity{MtimeNs: cur.MtimeNs, CtimeNs: cur.CtimeNs,
			Dev: cur.Dev, Ino: cur.Ino, Size: cur.Size, SnapAtNs: cur.SnapAtNs},
		func() (HashResult, error) {
			t.Fatal("hash must not be read outside the racy window")
			return HashResult{}, nil
		})
	require.NoError(t, err)
	require.Equal(t, Skip, dec3.Verdict,
		"outside the racy window the refreshed prefilter must decide alone (no hash read)")
}

// TestIC48_DelayedAck_RefreshNotAck_MustNotSkip — coordinator counter-example
// (a): coarse-FS sequence stat(S=0s) → full read (1s) → same-size rewrite
// (1.5s, same coarse tick) → CP ack 60s later. If the ledger anchor were the
// ACK time, |60−0| > 2s would verdict Same → silent miss of rewritten
// content. The anchor must be the SNAPSHOT (stat) moment: |1.5−0| < 2s window
// relative to the stamps → undecidable → hash → content differs → Recollect.
func TestIC48_DelayedAck_RefreshNotAck_MustNotSkip(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "late-ack.bin", "Z001")
	id := ic48Stat(t, path)
	// Anchor at the snapshot moment (id.SnapAtNs), NOT at a later ack time.
	ic48RecordProcessed(t, q, "rule", path, id, ic48SHA(t, path))

	// Same-size rewrite 1.5s "after" the snapshot (simulated by keeping the
	// rewrite inside the granularity window — real clock, real rewrite).
	ic48RewriteSameSize(t, path, "Z002")

	// Even if the ack (and the row write) happened 60s later, the ledger
	// anchor stays the snapshot moment, so the prefilter cannot call this
	// Same — it must hash-confirm; the hash catches the rewrite.
	dec := ic48Decide(t, q, "rule", path)
	require.NotEqual(t, Skip, dec.Verdict,
		"a same-tick rewrite must never bare-skip when the anchor is the snapshot moment")
	require.Equal(t, Recollect, dec.Verdict,
		"hash confirmation must catch the rewritten content")
}

// TestIC48_RebuildConvergence_TwoScanRounds — coordinator requirement (b):
// after a cp -p rebuild, scan round 1 hash-confirms + refreshes the ledger;
// once G has passed, scan round 2 must Same-skip WITHOUT reading the file
// (git racily-clean convergence, anchored on the refreshed snapshot moment).
func TestIC48_RebuildConvergence_TwoScanRounds(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "conv.bin", "CON1")
	oldStat := ic48Stat(t, path)
	ic48RecordProcessed(t, q, "rule", path, oldStat, ic48SHA(t, path))

	// cp -p rebuild with identical content.
	fiOld, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	ic48WriteDir(t, filepath.Dir(path), "conv.bin", "CON1")
	require.NoError(t, os.Chtimes(path, fiOld.ModTime(), fiOld.ModTime()))

	// Round 1: metadata mismatch → hash-confirm → identical → RefreshAndSkip.
	round1 := ic48Decide(t, q, "rule", path)
	require.Equal(t, RefreshAndSkip, round1.Verdict)

	// G passes (simulated by moving the anchor forward past stamps+G — the
	// identical ordering time would produce).
	rec, err := q.GetProcessedIdentity("rule", path)
	require.NoError(t, err)
	cur := ic48Stat(t, path)
	require.NoError(t, q.SetProcessedIdentityAnchor("rule", path, cur.MtimeNs+racyGranularityNs*3))

	// Round 2: prefilter decides alone — the hash function must NOT run.
	hashReads := 0
	round2, err := q.DecideProcessed(context.Background(), "rule", path,
		FileIdentity{MtimeNs: cur.MtimeNs, CtimeNs: cur.CtimeNs, Dev: cur.Dev, Ino: cur.Ino, Size: cur.Size, SnapAtNs: cur.SnapAtNs},
		func() (HashResult, error) {
			hashReads++
			return ic48HashFile(path)()
		})
	require.NoError(t, err)
	require.Equal(t, Skip, round2.Verdict, "after G the refreshed prefilter must Same-skip")
	require.Zero(t, hashReads, "round 2 must not read the file (converged)")
	_ = rec
}
func TestIC48_RacyWindow_MustHashConfirm(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "racy.bin", "RACY")
	ic48RecordProcessed(t, q, "rule", path, ic48Stat(t, path), ic48SHA(t, path))

	// No file change at all: the stamps are within the record time ±2s by
	// construction, so the verdict must NOT be a bare Skip.
	dec := ic48Decide(t, q, "rule", path)
	require.NotEqual(t, Skip, dec.Verdict,
		"a just-recorded file is racily-clean: it must be hash-confirmed, never bare-skipped")
	require.Contains(t, []CandidateVerdict{RefreshAndSkip, Skip}, dec.Verdict)
}

// TestIC48_LegacyRow_MissingColumns_HashConfirm — migration compatibility:
// a row written before the new columns (zero identity, empty/unknown hash
// anchor semantics) must be treated as undecidable → hash confirmation, and
// with identical content must skip-and-refresh rather than re-upload.
func TestIC48_LegacyRow_MissingColumns_HashConfirm(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "legacy.bin", "LEGC")
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "pf-legacy", RuleID: "rule", LocalPath: path,
		FileSize: 4, FileMtime: time.Now().Unix(),
		SHA256: ic48SHA(t, path),
		// Ident zero, UploadedAtNs zero → legacy row.
	}))

	dec := ic48Decide(t, q, "rule", path)
	require.Equal(t, RefreshAndSkip, dec.Verdict,
		"a legacy row with identical content must hash-confirm and refresh, not re-upload")
}

// TestIC48_UndecidableIdentity_HashConfirm — a current-file identity with
// unknown fields (platform could not provide ctime/inode) must never be
// treated as equal: it must go through hash confirmation, and with changed
// content must re-collect.
func TestIC48_UndecidableIdentity_HashConfirm(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "undec.bin", "UUU1")
	oldStat := ic48Stat(t, path)
	ic48RecordProcessed(t, q, "rule", path, oldStat, ic48SHA(t, path))

	// Rewrite with different content, same size.
	ic48RewriteSameSize(t, path, "VVV2")

	// The current identity is partially unknown (zero ctime/inode) — the
	// comparison input is missing, so the decision cannot trust the prefilter
	// and must hash-confirm. The hash catches the changed content.
	dec, err := q.DecideProcessed(context.Background(), "rule", path,
		FileIdentity{MtimeNs: ic48Stat(t, path).MtimeNs, CtimeNs: 0, Dev: 0, Ino: 0, Size: 4},
		ic48HashFile(path))
	require.NoError(t, err)
	require.Equal(t, Recollect, dec.Verdict,
		"an undecidable identity with changed content must re-collect via hash confirmation")
}

// TestIC48_ChangedContent_DifferentHash_Recollect — the hash-confirm path
// must re-collect when the recorded sha256 disagrees with the file (legacy
// row + same-size rewrite).
func TestIC48_ChangedContent_DifferentHash_Recollect(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "hashdiff.bin", "W1W1")
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "pf-hashdiff", RuleID: "rule", LocalPath: path,
		FileSize: 4, FileMtime: time.Now().Unix(),
		SHA256: ic48SHA(t, path),
	}))
	ic48RewriteSameSize(t, path, "X2X2")

	dec, err := q.DecideProcessed(context.Background(), "rule", path,
		FileIdentity{}, // fully unknown current identity → undecidable
		ic48HashFile(path))
	require.NoError(t, err)
	require.Equal(t, Recollect, dec.Verdict,
		"hash confirmation must re-collect when the recorded sha256 differs")
}

// TestIC48_NoRecord_Recollect — a file with no ledger row is always collected
// (the old IsProcessed also returned false; pinned here for the new entry).
func TestIC48_NoRecord_Recollect(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "fresh.bin", "FRSH")
	dec := ic48Decide(t, q, "rule", path)
	require.Equal(t, Recollect, dec.Verdict)
}

// ── file helpers ─────────────────────────────────────────────────────────────

// ic48Dir returns one shared temp dir per test: every ic48WriteFile inside a
// test writes into the same directory so a recreate lands on the same path.
func ic48Dir(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func ic48WriteFile(t *testing.T, name, content string) string {
	t.Helper()
	return ic48WriteDir(t, ic48Dir(t), name, content)
}

func ic48WriteDir(t *testing.T, dir, name, content string) string {
	t.Helper()
	path := dir + "/" + name
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
	return path
}

func ic48Stat(t *testing.T, path string) fileid.Identity {
	t.Helper()
	id, err := fileid.Stat(path)
	require.NoError(t, err)
	return id
}

func ic48SHA(t *testing.T, path string) string {
	t.Helper()
	hr, err := ic48HashFile(path)()
	require.NoError(t, err)
	return hr.SHA256
}

// ic48RewriteSameSize replaces the file's content with same-length different
// bytes, in place, as fast as possible (same-second on coarse stamps).
func ic48RewriteSameSize(t *testing.T, path, content string) {
	t.Helper()
	require.Len(t, content, 4)
	require.NoError(t, os.WriteFile(path, []byte(content), 0o644))
}
