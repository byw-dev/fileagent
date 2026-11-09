package queue

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestIC48_FutureStamp_MustStayRacy — the single-sided racy rule's reason to
// exist. A writer can set an arbitrary mtime (`touch -t`, os.Chtimes): a
// FUTURE stamp must keep the row racy forever — hash-confirm on every check.
// A symmetric window (|stamp−anchor| ≤ G) would verdict Same on a stamp far
// from the anchor, blessing a time-manipulated file as unchanged.
//
// Construction: both stamps must be far from the anchor for this test to
// kill the symmetric mutation (the unforgeable ctime alone would keep the
// symmetric window honest). So the file is FIRST chmod'ed (moving its ctime
// far from the soon-to-be-recorded one), then the ledger row is seeded with
// the OLD ctime and an anchor far before both stamps, and only then is the
// mtime pushed an hour into the future. Every field compares equal; both
// stamps sit far outside any window around the anchor; the forged mtime is
// the only witness — exactly the case git's one-sided racily-clean covers.
func TestIC48_FutureStamp_MustStayRacy(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "future.bin")
	require.NoError(t, os.WriteFile(path, []byte("FTUR"), 0o644))

	// Move ctime away from the recorded one: the ledger records the
	// PRE-chmod ctime, the live file's ctime is ~now (chmod is not forgeable
	// backwards). The recorded mtime is the forged future stamp. Both live
	// stamps then sit far outside any window around the stale anchor — but
	// the recorded ctime EQUALS the live one in the row (same value copied),
	// so the field comparison passes; the racy judgement sees the live
	// stamps against the stale anchor.
	// Sanity below asserts this construction actually reaches the racy
	// branch with all fields equal — the symmetric mutation would call it
	// Same; the one-sided rule must call it racy because the forged mtime is
	// at/after anchor−G.
	time.Sleep(5 * time.Millisecond)
	require.NoError(t, os.Chmod(path, 0o600))

	// Seed: anchor far BEFORE everything (a stale ledger written long ago),
	// ctime = the LIVE (post-chmod) ctime — copied verbatim so all fields
	// compare equal — and mtime = the forged future stamp.
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(path, future, future))
	fid := ic48Stat(t, path)
	hr := ic48SHA(t, path)
	anchor := fid.CtimeNs - racyGranularityNs*10

	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "pf-future", RuleID: "rule", LocalPath: path,
		FileSize: 4, FileMtime: fid.MtimeNs / 1e9, SHA256: hr,
		Ident: FileIdentity{
			MtimeNs: fid.MtimeNs, CtimeNs: fid.CtimeNs, // live ctime copied → equal
			Dev: fid.Dev, Ino: fid.Ino, Size: 4, SnapAtNs: anchor,
		},
		UploadedAtNs: anchor,
	}))

	// Sanity: under the symmetric mutation, |mtime−anchor| and |ctime−anchor|
	// are both far beyond G → not racy → Same → bare skip. Under the
	// one-sided rule, mtime (future) ≥ anchor−G → undecidable → hash-confirm.
	rec, err := q.GetProcessedIdentity("rule", path)
	require.NoError(t, err)
	v := PrefilterVerdict(rec.Ident,
		FileIdentity{MtimeNs: fid.MtimeNs, CtimeNs: fid.CtimeNs, Dev: fid.Dev, Ino: fid.Ino, Size: 4},
		rec.SnapshotAtNs)
	require.Equal(t, VerdictUndecidable, v,
		"a forged future mtime must keep the row racy against its anchor (git one-sided rule)")

	dec := ic48Decide(t, q, "rule", path)
	require.Equal(t, RefreshAndSkip, dec.Verdict,
		"the future stamp must hash-confirm (identical content → skip-with-refresh), never bare-skip")
}
