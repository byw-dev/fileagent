package watcher

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/fileid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Review-r5 reproductions, ported as regression tests ─────────────────────

// TestReviewR5RealSymlinkReturn — fix 1 (ported from the reviewer's real-file
// probe): A delivered and its seen entry has passed the racy window; the path
// (symlink) switches to B, B takes a claim and parks mid-delivery; the path
// switches BACK to still-existing A. A's candidate hits the OLD seen entry
// and — pre-fix — returns false WITHOUT marking B's token dirty: B's settle
// then sees clean and never re-examines the path, and this A candidate is
// lost. Fix: claimDelivery checks the IN-FLIGHT token FIRST (any candidate
// arriving during flight marks the token dirty → settle re-examines), and
// only suppresses seen-hits when nothing is in flight.
func TestReviewR5RealSymlinkReturn(t *testing.T) {
	d := t.TempDir()
	a := filepath.Join(d, "a")
	b := filepath.Join(d, "b")
	p := filepath.Join(d, "p")
	require.NoError(t, os.WriteFile(a, []byte("AAAA"), 0o600))
	require.NoError(t, os.WriteFile(b, []byte("BBBB"), 0o600))
	// Both files must be older than the racy window so a seen entry for A
	// provably sorts A's stamps before anchor−G (the suppression the
	// reviewer's scenario needs).
	time.Sleep(2100 * time.Millisecond)
	require.NoError(t, os.Symlink(a, p))
	ia, err := fileid.Stat(p)
	require.NoError(t, err)
	seen := map[string]seenEntry{p: {ident: ia, anchorNs: ia.SnapAtNs}}
	w := &Watcher{logger: zap.NewNop()}

	// Path switches to B: B claims (in flight).
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink(b, p))
	ib, err := fileid.Stat(p)
	require.NoError(t, err)
	tokB, okB := w.claimDelivery(seen, p, ib, ib.SnapAtNs)
	require.True(t, okB, "B's claim must be admitted (nothing in flight, seen=A)")

	// Path switches BACK to A: A's candidate must not be silently discarded —
	// it must mark B's in-flight token dirty so B's settle re-examines.
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink(a, p))
	again, err := fileid.Stat(p)
	require.NoError(t, err)
	_, admitted := w.claimDelivery(seen, p, again, again.SnapAtNs)
	fmt.Printf("real_symlink A_inode=%d B_inode=%d B_inflight=%v A_return_admitted=%v B_dirty=%v\n",
		ia.Ino, ib.Ino, okB, admitted, tokB.dirty)
	require.True(t, admitted || tokB.dirty,
		"the returned-A candidate was lost while B is in flight (B's token stayed clean; settle will not re-examine)")
}
