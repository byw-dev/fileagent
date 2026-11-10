package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/byw-dev/fileagent/agent/internal/fileid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Review-r3 reproductions, ported as regression tests ─────────────────────

// TestReviewR3CoarseStamp_Watcher_Ported — blocker 1 (ported): a coarse-FS
// same-tick rewrite with restored mtime produces an identity IDENTICAL to the
// delivered version's; the watcher must still deliver it (SameVersionProvable
// → racy window → Undecidable → not provably-same). Mutation "watcher drops
// the racy window (identity-only equality)" turns this red (emitted=0).
//
// The coarse identity is a deterministic simulation (not FAT/SMB on this
// host): the second stat's stamps are forced to the first's — which is what
// a 2s-granularity stamp yields for two writes inside one tick.
func TestReviewR3CoarseStamp_Watcher_Ported(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	require.NoError(t, os.WriteFile(p, []byte("BBBB"), 0o600))
	id, err := fileid.Stat(p)
	require.NoError(t, err)
	info, err := os.Stat(p)
	require.NoError(t, err)

	// seen: the FIRST version's identity with its delivery anchor — both
	// stamps identical to the current stat's (coarse-granularity collision).
	seen := map[string]seenEntry{p: {ident: id, anchorNs: id.SnapAtNs}}

	// Force the "current" stat to report the same coarse stamps: temporarily
	// backdate the file to id's stamps (they already are), so cur == seen.
	// (id was just stat'ed, so they match by construction.)

	events := make(chan FileEvent, 1)
	w := &Watcher{logger: zap.NewNop()}
	require.NoError(t, w.scanFile(context.Background(), events, seen, p, info))
	require.Len(t, events, 1,
		"a coarse-stamp same-tick rewrite must be delivered: SameVersionProvable's racy window makes it not provably-same (pre-fix: emitted=0)")
	fmt.Printf("coarse rewrite delivered=%d (content=BBBB)\n", len(events))
}

// TestReviewR3UnknownClaimLeak_Ported — fix 4 (ported): a claim with an
// UNKNOWN identity (zero fields) must release its inflight entry on settle —
// version-equality matching can never match zero fields and leaked one entry
// per unknown-identity path (pre-fix: inflight=1 after settle).
func TestReviewR3UnknownClaimLeak_Ported(t *testing.T) {
	w := &Watcher{logger: zap.NewNop()}
	seen := map[string]seenEntry{}
	unknown := versionOf{} // all-zero: filesystems that cannot provide identity
	tok, ok := w.claimDelivery(seen, "gone", unknown, 0)
	require.True(t, ok, "an unknown identity must be delivered (never suppressed)")
	w.completeDelivery(seen, "gone", tok, false)
	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	require.Len(t, w.inflight, 0,
		"the claim must release by token: pre-fix version matching leaked the entry (inflight=1)")
}
