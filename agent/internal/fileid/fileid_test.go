package fileid

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStat_FullIdentityOnRealFile pins the platform contract on a real file:
// every field the platform provides is non-zero, and the snapshot moment is
// anchored (Unix ns).
func TestStat_FullIdentityOnRealFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	id, err := Stat(path)
	require.NoError(t, err)
	assert.NotZero(t, id.MtimeNs, "mtime-ns must be set")
	assert.NotZero(t, id.Size, "size must be set")
	assert.NotZero(t, id.SnapAtNs, "the snapshot moment must be anchored")
	if !assert.NotZero(t, id.CtimeNs, "ctime must be provided by this platform") {
		t.FailNow()
	}
	if !assert.NotZero(t, id.Ino, "inode/file-index must be provided by this platform") {
		t.FailNow()
	}
	// Dev may legitimately be non-zero on every supported platform.
	assert.NotZero(t, id.Dev, "device/volume-serial must be provided")
}

// TestStat_RewriteChangesCtime — a same-size in-place rewrite changes the
// ctime (and, at nanosecond resolution, typically the mtime): the fields the
// IC-BUG-48 prefilter relies on must actually move when the content moves.
func TestStat_RewriteChangesCtime(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	require.NoError(t, os.WriteFile(path, []byte("AAA"), 0o644))
	before, err := Stat(path)
	require.NoError(t, err)

	// Give the clock room so even coarse stamps move.
	time.Sleep(10 * time.Millisecond)
	require.NoError(t, os.WriteFile(path, []byte("BBB"), 0o644))
	after, err := Stat(path)
	require.NoError(t, err)

	assert.NotEqual(t, before.CtimeNs, after.CtimeNs,
		"a rewrite must change ctime (kernel status-change guarantee)")
}

// TestStat_RecreateNotProvableSame — the REAL invariant the IC-BUG-48
// prefilter needs after a delete-and-recreate with restored mtime: the
// recreated file must NOT be provably the same version as the recorded one
// (queue.SameVersionProvable == false). The design never promised that a
// specific identity field changes — only that the candidate cannot be
// PROVEN identical:
//   - any identity field differs → Changed → hash-confirm;
//   - all fields equal → the stamps then sort within anchor−2s (they were
//     read around the recreate) → Undecidable → hash-confirm.
//
// Either way the downstream DecideProcessed hash-confirms; content that
// differs re-collects. Platform facts observed in CI for this exact test
// (PR #144, run 42791201711):
//   - Ubuntu/ext4: delete-then-immediately-recreate REUSED the inode
//     (Ino equal), so an `Ino must change` assertion failed;
//   - Windows/NTFS: the two stats landed in the same time tick, so
//     ChangeTime was equal, and a `Ctime must change` assertion failed.
//
// Hence this test asserts the verdict, never individual fields.
//
// Import note: fileid must not import queue (queue imports fileid), so the
// table-driven verdict part lives in the queue package
// (identity_platform_r13_test.go). This file keeps only the real-fs
// observation: both snapshots must be FULLY KNOWN (every field non-zero),
// which is what makes the verdict decidable at all.
func TestStat_RecreateNotProvableSame(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	require.NoError(t, os.WriteFile(path, []byte("same"), 0o644))
	fiOld, err := os.Stat(path)
	require.NoError(t, err)
	before, err := Stat(path)
	require.NoError(t, err)

	require.NoError(t, os.Remove(path))
	require.NoError(t, os.WriteFile(path, []byte("same"), 0o644))
	require.NoError(t, os.Chtimes(path, fiOld.ModTime(), fiOld.ModTime()))
	after, err := Stat(path)
	require.NoError(t, err)

	// Both snapshots fully known — the precondition for any verdict.
	assert.True(t, identityKnownForTest(before), "before-snapshot must be fully known")
	assert.True(t, identityKnownForTest(after), "after-snapshot must be fully known")

	// Informational (platform-dependent, asserted in the queue package):
	t.Logf("recreate with restored mtime: ino %d→%d (ext4 may reuse), ctime %d→%d (NTFS may keep the tick)",
		before.Ino, after.Ino, before.CtimeNs, after.CtimeNs)
}

// identityKnownForTest mirrors queue.IdentityFullyKnown without importing
// queue (fileid must stay a leaf package).
func identityKnownForTest(id Identity) bool {
	return id.MtimeNs != 0 && id.CtimeNs != 0 && id.Dev != 0 && id.Ino != 0
}

// TestStat_MissingFile errors cleanly (error path of the platform wrapper).
func TestStat_MissingFile(t *testing.T) {
	_, err := Stat(filepath.Join(t.TempDir(), "nope.bin"))
	require.Error(t, err)
}

// TestStatHandle_MatchesPathStat — the same-fd snapshot equals a path stat of
// the same, untouched file (plus the anchor). This is the property the
// uploader relies on: stat the handle you are about to read.
func TestStatHandle_MatchesPathStat(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "f.bin")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	h, err := StatHandle(f)
	require.NoError(t, err)

	p, err := Stat(path)
	require.NoError(t, err)
	assert.Equal(t, p.MtimeNs, h.MtimeNs)
	assert.Equal(t, p.CtimeNs, h.CtimeNs)
	assert.Equal(t, p.Dev, h.Dev)
	assert.Equal(t, p.Ino, h.Ino)
	assert.Equal(t, p.Size, h.Size)
	assert.NotZero(t, h.SnapAtNs)
}

// TestStatHandle_MissingFile errors on a nil/invalid handle path is covered
// by StatHandle(f) with a closed file: it must return an error, not zeros
// that would compare equal to an unknown ledger row.
func TestStatHandle_ClosedHandleErrors(t *testing.T) {
	path := filepath.Join(t.TempDir(), "f.bin")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))
	f, err := os.Open(path)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	_, err = StatHandle(f)
	require.Error(t, err)
}

// TestTimeFromNs round-trips (ledger bookkeeping helper).
func TestTimeFromNs(t *testing.T) {
	want := time.Unix(1_700_000_000, 5).UTC()
	assert.Equal(t, want, TimeFromNs(want.UnixNano()))
}
