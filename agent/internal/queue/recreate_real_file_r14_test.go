package queue

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/fileid"
	"github.com/stretchr/testify/require"
)

// TestRecreateRealFile_NeverProvableSame — rework-13 item 2 (moved into the
// queue package, where SameVersionProvable lives): drive a REAL
// delete-and-recreate with restored mtime through fileid.Stat and assert the
// verdict-level invariant — the recreated file must NOT be provably the same
// version as the recorded snapshot (anchor = the first snapshot's SnapAtNs).
//
// Per-field assertions are forbidden: PR #144's CI proved the assumptions
// wrong on both platforms —
//   - Ubuntu/ext4: delete + immediate recreate REUSED the inode
//     (TestStat_RecreateChangesInode: `Should not be: 0x580dc5`);
//   - Windows/NTFS: the two stats landed in the same time tick
//     (`Should not be: 1794303049484199400` — ChangeTime equal).
//
// The design only guarantees the CANDIDATE cannot be proven identical: any
// field differing → Changed; all fields equal → the stamps sit inside
// anchor−2s → Undecidable. Either way: not provably same.
//
// Note: with the restored mtime and an unchanged size, the recreated stamps
// depend on when the recreate's ctime lands. The anchor here is the FIRST
// snapshot's SnapAtNs (taken well before the recreate), so both the original
// stamps and the recreated stamps can sit inside anchor−2s — the assertion
// must hold regardless of which branch (Changed / Undecidable) the prefilter
// takes.
func TestRecreateRealFile_NeverProvableSame(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")

	require.NoError(t, os.WriteFile(path, []byte("same"), 0o644))
	// Backdate so the first snapshot's stamps sit well before the recreate.
	past := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, past, past))
	// Capture the mtime the first snapshot will actually record (AFTER the
	// backdate) — this is the value cp -p restores (r12 review: capturing it
	// before the backdate made before/after mtimes differ, so the test never
	// exercised the restored-mtime case).
	fiOld, err := os.Stat(path)
	require.NoError(t, err)

	before, err := fileid.Stat(path)
	require.NoError(t, err)
	anchor := before.SnapAtNs // the first snapshot's snapshot moment

	// Delete → recreate with the SAME content → restore mtime (cp -p shape).
	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte("same"), 0o644))
	require.NoError(t, os.Chtimes(path, fiOld.ModTime(), fiOld.ModTime()))

	after, err := fileid.Stat(path)
	require.NoError(t, err)
	// Precondition: the restored mtime really equals the recorded one, so the
	// verdict cannot be carried by an mtime difference alone.
	require.Equal(t, before.MtimeNs, after.MtimeNs,
		"precondition: cp -p shape must restore the exact recorded mtime")

	// Informational only (platform-dependent — see the CI facts above).
	t.Logf("recreate: ino %d→%d (ext4 may reuse), ctime %d→%d (NTFS may keep the tick)",
		before.Ino, after.Ino, before.CtimeNs, after.CtimeNs)

	// The invariant under test: NOT provably the same version.
	require.False(t, SameVersionProvable(
		IdentityFromFileid(before), IdentityFromFileid(after), anchor),
		"a delete-and-recreate with restored mtime must never be provably the same version "+
			"(before=%+v after=%+v anchor=%d)", before, after, anchor)
	fmt.Printf("r14_recreate provable=%v (ino %d→%d, ctime %d→%d, anchor=%d)\n",
		SameVersionProvable(IdentityFromFileid(before), IdentityFromFileid(after), anchor),
		before.Ino, after.Ino, before.CtimeNs, after.CtimeNs, anchor)
}
