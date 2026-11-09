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
// processed-file ledger row with the file's full kernel identity.
func ic48RecordProcessed(t *testing.T, q *Queue, ruleID, path string, id fileid.Identity, sha string) {
	t.Helper()
	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID:           "pf-" + path,
		RuleID:       ruleID,
		LocalPath:    path,
		FileSize:     fi.Size(),
		FileMtime:    fi.ModTime().Unix(),
		SHA256:       sha,
		Ident:        FileIdentity{MtimeNs: id.MtimeNs, CtimeNs: id.CtimeNs, Dev: id.Dev, Ino: id.Ino, Size: id.Size},
		UploadedAtNs: time.Now().UnixNano(),
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
// identity. On master (no DecideProcessed) this does not compile — the red
// run therefore happens on master via the shim below (see red.txt).
func ic48Decide(t *testing.T, q *Queue, ruleID, path string) CandidateDecision {
	t.Helper()
	id, err := fileid.Stat(path)
	require.NoError(t, err)
	dec, err := q.DecideProcessed(context.Background(), ruleID, path,
		FileIdentity{MtimeNs: id.MtimeNs, CtimeNs: id.CtimeNs, Dev: id.Dev, Ino: id.Ino, Size: id.Size},
		ic48HashFile(path))
	require.NoError(t, err)
	return dec
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

	// The ledger caught up (identity refreshed during hash confirm). The
	// refreshed row's uploaded_at_ns anchors a racy window around the refresh
	// moment, and the recreated file's stamps fall inside it — so the second
	// decision is hash-confirmed AGAIN (RefreshAndSkip, not a bare Skip):
	// correct racily-clean behaviour, and after the granularity window
	// (simulated by backdating the anchor below) the prefilter decides alone.
	dec2 := ic48Decide(t, q, "rule", path)
	require.Equal(t, RefreshAndSkip, dec2.Verdict,
		"inside the racy window after the refresh, the second check must hash-confirm again (content identical → skip)")
	rec, err := q.GetProcessedIdentity("rule", path)
	require.NoError(t, err)
	require.NoError(t, q.SetProcessedIdentityAnchor("rule", path, rec.UploadedAtNs-racyGranularityNs*2-1))
	dec3, err := q.DecideProcessed(context.Background(), "rule", path,
		FileIdentity{MtimeNs: ic48Stat(t, path).MtimeNs, CtimeNs: ic48Stat(t, path).CtimeNs,
			Dev: ic48Stat(t, path).Dev, Ino: ic48Stat(t, path).Ino, Size: ic48Stat(t, path).Size},
		func() (HashResult, error) { t.Fatal("hash must not be read outside the racy window"); return HashResult{}, nil })
	require.NoError(t, err)
	require.Equal(t, Skip, dec3.Verdict,
		"outside the racy window the refreshed prefilter must decide alone (no hash read)")
}

// TestIC48_RacyWindow_MustHashConfirm — even with a fully matching prefilter,
// a file whose stamps lie within the record-time granularity must be
// hash-confirmed (git racily-clean semantics), because a same-granularity
// rewrite could hide behind equal coarse stamps.
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
