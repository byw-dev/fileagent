//go:build windows

package fileid

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestStatHandle_IdentityFromHandleNotPath — rework-2 blocker 4: after the
// handle is open, the path is re-pointed to a DIFFERENT file; StatHandle must
// still return the volume serial / file index / ChangeTime of the file the
// HANDLE refers to (queried via GetFileInformationByHandle(Ex) on f.Fd()),
// never a fresh by-path query of whatever the path now points at.
//
// Run on CI windows-latest (this machine's checks are GOOS=windows vet +
// test -c compile only).
func TestStatHandle_IdentityFromHandleNotPath(t *testing.T) {
	d := t.TempDir()
	a := filepath.Join(d, "a.txt")
	b := filepath.Join(d, "b.txt")
	p := filepath.Join(d, "link.txt")
	require.NoError(t, os.WriteFile(a, []byte("AAAA"), 0o600))
	require.NoError(t, os.WriteFile(b, []byte("BBBB"), 0o600))
	require.NoError(t, os.Symlink(a, p))

	f, err := os.Open(p) // handle → a
	require.NoError(t, err)
	defer f.Close()

	pre, err := StatHandle(f)
	require.NoError(t, err)
	require.NotZero(t, pre.CtimeNs, "ChangeTime must come from the handle")
	require.NotZero(t, pre.Dev, "volume serial must come from the handle")
	require.NotZero(t, pre.Ino, "file index must come from the handle")

	// Re-point the path to b while the handle stays open on a.
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink(b, p))

	post, err := StatHandle(f)
	require.NoError(t, err)

	// The handle still refers to a: every kernel field must be a's —
	// unchanged, even though the path now resolves to b.
	assert.Equal(t, pre.Dev, post.Dev, "volume serial must come from the handle, not the re-pointed path")
	assert.Equal(t, pre.Ino, post.Ino, "file index must come from the handle, not the re-pointed path")
	assert.Equal(t, pre.CtimeNs, post.CtimeNs, "ChangeTime must come from the handle, not the re-pointed path")
	assert.Equal(t, pre.MtimeNs, post.MtimeNs)

	// Cross-check: a BY-PATH stat now describes b and must differ from the
	// handle's identity (this is what a buggy by-name query would return).
	byPath, err := Stat(p)
	require.NoError(t, err)
	assert.NotEqual(t, pre.Ino, byPath.Ino,
		"sanity: the path now points to b; if StatHandle matched this, it queried by name")
}
