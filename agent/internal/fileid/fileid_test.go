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

// TestStat_RecreateChangesInode — delete + recreate yields a different file
// identity even when mtime is restored (cp -p), which is exactly the
// rebuild-detection the IC-BUG-48 prefilter needs.
func TestStat_RecreateChangesInode(t *testing.T) {
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

	assert.NotEqual(t, before.Ino, after.Ino,
		"a delete-and-recreate must change the file identity even with restored mtime")
	assert.NotEqual(t, before.CtimeNs, after.CtimeNs,
		"a delete-and-recreate must change ctime even with restored mtime")
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
