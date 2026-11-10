package queue

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestIC48_InodeOnly_RebuildDetectedWhenCtimeAmbiguous — the inode field is
// not redundant: on filesystems where the restored mtime lands bit-identical
// AND ctime resolution is coarse (or the record ctime equals the new ctime by
// tick collision), the inode is the ONLY field that catches a rebuild. This
// test constructs that case deterministically: the recorded identity carries
// the SAME mtime-ns and ctime-ns the recreated file reports, and differs ONLY
// in inode. Removing the inode comparison (mutation) makes every field equal →
// stamps ancient → Same → bare Skip → red; the real verdict must be Changed →
// hash-confirm → different content → Recollect.
func TestIC48_InodeOnly_RebuildDetectedWhenCtimeAmbiguous(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	dir := t.TempDir()
	path := filepath.Join(dir, "ino.bin")
	require.NoError(t, os.WriteFile(path, []byte("OLD1"), 0o644))
	id := ic48Stat(t, path)
	oldSha := ic48SHA(t, path)

	// Record with the identity the REBUILT file will report (same
	// mtime-ns/ctime-ns — tick collision on a coarse-ctime filesystem) and an
	// anchor far before the stamps (not racy). Only Ino differs.
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte("NEW2"), 0o644))
	newID := ic48Stat(t, path)
	// Anchor AFTER the stamps by 3G: stamps are strictly below anchor−G, so
	// the racy window is out of the picture and only field equality decides.
	anchor := newID.MtimeNs + racyGranularityNs*3
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "pf-ino", RuleID: "rule", LocalPath: path,
		FileSize: 4, FileMtime: id.MtimeNs / 1e9, SHA256: oldSha,
		Ident: FileIdentity{
			MtimeNs: newID.MtimeNs, CtimeNs: newID.CtimeNs, // ambiguous stamps
			Dev: newID.Dev, Ino: id.Ino, // ← the ONLY difference: the OLD inode
			Size: 4, SnapAtNs: anchor,
		},
		UploadedAtNs: anchor,
	}))

	dec := ic48Decide(t, q, "rule", path)
	require.Equal(t, Recollect, dec.Verdict,
		"an inode-only mismatch (rebuild under timestamp collision) must re-collect, never skip")
}
