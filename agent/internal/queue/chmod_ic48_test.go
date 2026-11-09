package queue

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIC48_ChmodOnly_CtimeChange_MustHashConfirm: a chmod keeps mtime and
// size but bumps ctime. With the anchor far enough in the past that the racy
// window cannot shadow the signal, the ONLY changed field is ctime.
// The ctime comparison is the mutation-detector: WITHOUT it every field
// compares equal and the stamps are ancient → bare Skip (no file read).
// WITH it the verdict is Changed → hash-confirm → identical → RefreshAndSkip.
// The assertion targets the PREFILTER verdict (changed), which only the ctime
// comparison can produce here — killing the "remove ctime comparison"
// mutation with the hash-confirm outcome unchanged.
func TestIC48_ChmodOnly_CtimeChange_MustHashConfirm(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "chmod.bin", "CHM1")
	id := ic48Stat(t, path)
	hr := ic48SHA(t, path)
	// Anchor 3G BEFORE the stamps: the recorded version is fully outside the
	// racy window, so any skip/confirm difference comes from field equality.
	anchor := id.MtimeNs + racyGranularityNs*3
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "pf-chmod", RuleID: "rule", LocalPath: path,
		FileSize: int64(len("CHM1")), FileMtime: id.MtimeNs / 1e9, SHA256: hr,
		Ident:        FileIdentity{MtimeNs: id.MtimeNs, CtimeNs: id.CtimeNs, Dev: id.Dev, Ino: id.Ino, Size: id.Size, SnapAtNs: anchor},
		UploadedAtNs: anchor,
	}))

	// chmod: mtime/size/inode unchanged, ctime bumps.
	require.NoError(t, os.Chmod(path, 0o600))
	fi, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, os.Chtimes(path, fi.ModTime(), fi.ModTime()))
	newID := ic48Stat(t, path)
	require.NotEqual(t, id.CtimeNs, newID.CtimeNs, "chmod must bump ctime")
	require.Equal(t, id.MtimeNs, newID.MtimeNs, "chmod must not change mtime")

	// The prefilter alone must see the state change: the only differing
	// field is ctime. (Mutation "ctime comparison removed" → all fields
	// equal + stamps outside the window → Same → this assertion FAILS.)
	rec, err := q.GetProcessedIdentity("rule", path)
	require.NoError(t, err)
	pv := PrefilterVerdict(rec.Ident,
		FileIdentity{MtimeNs: newID.MtimeNs, CtimeNs: newID.CtimeNs, Dev: newID.Dev, Ino: newID.Ino, Size: newID.Size},
		rec.SnapshotAtNs)
	require.Equal(t, VerdictChanged, pv,
		"the prefilter must detect a ctime-only state change (chmod)")

	dec := ic48Decide(t, q, "rule", path)
	require.Equal(t, RefreshAndSkip, dec.Verdict,
		"a ctime-only change must hash-confirm (identical → refresh), never bare-skip")
}
