package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/fileid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Review-r6 reproductions, ported as regression tests ─────────────────────

// TestReviewR6SeenBypass — blocker 1 (ported): the scenario runs through the
// REAL scanFile (pollScan/debounce-recheck's entry), NOT a direct
// claimDelivery call. A delivered + seen past the racy window; path → B, B's
// claim in flight; path → back to A. scanFile must NOT suppress A's candidate
// on the old seen entry before reaching claimDelivery: B's token must end up
// dirty (settle will re-examine the path).
func TestReviewR6SeenBypass(t *testing.T) {
	d := t.TempDir()
	a := filepath.Join(d, "a")
	b := filepath.Join(d, "b")
	p := filepath.Join(d, "p.log")
	for _, v := range []string{a, b} {
		require.NoError(t, os.WriteFile(v, []byte(v), 0o600))
	}
	// Both files past the racy window so a seen entry for A provably
	// suppresses A on a later observation (the pre-fix behaviour).
	time.Sleep(2100 * time.Millisecond)
	require.NoError(t, os.Symlink(a, p))
	ia, err := fileid.Stat(p)
	require.NoError(t, err)
	seen := map[string]seenEntry{p: {ident: ia, anchorNs: ia.SnapAtNs}}
	w := &Watcher{logger: zap.NewNop()}

	// Path → B: B claims (in flight).
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink(b, p))
	ib, err := fileid.Stat(p)
	require.NoError(t, err)
	tokB, okB := w.claimDelivery(seen, p, ib, ib.SnapAtNs)
	require.True(t, okB, "B claim rejected")

	// Path → back to A: observe A through the REAL scanFile — the entry
	// pollScan and the debounce recheck actually use.
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink(a, p))
	info, err := os.Stat(p)
	require.NoError(t, err)
	require.NoError(t, w.scanFile(context.Background(), make(chan FileEvent, 1), seen, p, info))

	// The seen entry still identifies version A (same inode + mtime); note
	// seenEqual compares ctime as well, and os.Chtimes bumps ctime, so the
	// precondition asserted here is identity provenance, not Same-ness.
	require.Equal(t, ia.Ino, seen[p].ident.Ino, "seen still holds A's identity (inode)")
	require.Equal(t, ia.MtimeNs, seen[p].ident.MtimeNs, "seen still holds A's identity (mtime)")
	require.True(t, tokB.dirty,
		"returned A discarded before claimDelivery (seen bypass); no recheck will be armed")
	fmt.Printf("seen_bypass B_inflight=%v A_in_seen=%v B_dirty=%v\n", okB, seen[p].ident.Ino == ia.Ino, tokB.dirty)
}

// TestReviewR6SplitSeen — blocker 2 (ported to a REAL Start lifecycle): ONE
// seen map per Start lifecycle must connect the normal path and the dirty
// recheck. Sequence:
//
//  1. Start's initial scan delivers A and writes it into the (single) seen.
//  2. The path switches to B; B is delivered through the dirty-recheck path
//     (claim parked → candidate marks dirty → settle re-examines). The
//     recheck writes B into THE SAME map (pre-fix: a separate recheckSeen).
//  3. The path returns to still-existing A. The NORMAL path's next
//     observation must DELIVER A — with the pre-fix split, the normal map
//     still held A and suppressed it (A lost); with ONE map, seen holds B,
//     seenEqual(A) fails and A is delivered.
//
// The fsnotify layer is driven with injected channels (same shape as the
// flush tests); the dirty candidate arrives synchronously via scanFile, and
// the recheck map identity is asserted directly (see
// TestReviewR6_SingleSeenMapInProduction for the source-level guard).
func TestReviewR6SplitSeen(t *testing.T) {
	d := t.TempDir()
	a := filepath.Join(d, "a")
	b := filepath.Join(d, "b")
	p := filepath.Join(d, "p.log")
	for _, v := range []string{a, b} {
		require.NoError(t, os.WriteFile(v, []byte(v), 0o600))
	}
	// Past the racy window: seen entries provably suppress their own version.
	time.Sleep(2100 * time.Millisecond)
	require.NoError(t, os.Symlink(a, p))
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(p, old, old))

	w, err := New(dirOf(p), "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 50 * time.Millisecond
	var recheckFires atomic.Int32
	w.dirtyRecheckFired = func(string) { recheckFires.Add(1) }

	events := make(chan FileEvent) // unbuffered; consumer controls the flow
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// REAL lifecycle: the initial scan delivers A and settles it into the
	// ONE lifecycle seen.
	startDone := make(chan error, 1)
	go func() { startDone <- w.Start(ctx, events) }()

	var delivered []FileEvent
	consumeDone := make(chan struct{}, 1)
	go func() {
		for fe := range events {
			delivered = append(delivered, fe)
			if len(delivered) >= 4 {
				consumeDone <- struct{}{}
				return
			}
		}
	}()

	// Step 1: A's initial delivery lands (parked flush → released by the
	// consumer goroutine above).
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.inflight) == 0 && len(w.recheckSeen) == 1
	}, 5*time.Second, 10*time.Millisecond, "A must be delivered and settled into the lifecycle seen")
	seenSnapshot := w.lifecycleSeen()
	require.Len(t, seenSnapshot, 1, "exactly ONE lifecycle seen map exists")
	ia, err := fileid.Stat(p)
	require.NoError(t, err)
	// os.Chtimes bumps ctime (APFS), so A's CURRENT identity differs from
	// the settled one only by ctime — seenEqual compares ctime too. The
	// property that matters for the scenario: the settled entry identifies
	// version A (same inode + same mtime as the file on disk).
	require.Equal(t, ia.Ino, seenSnapshot[p].ident.Ino, "seen holds A's inode after step 1")
	require.Equal(t, ia.MtimeNs, seenSnapshot[p].ident.MtimeNs, "seen holds A's mtime after step 1")

	// Step 2: path → B; B claims and parks (unbuffered channel full? no —
	// the consumer drains, so instead park via a second unbuffered claim:
	// simplest deterministic park is a direct scanFile racing the channel —
	// use the same trick as the r4 chain: a Write event arms the flush,
	// which parks. B's flush parks because the consumer is busy; then a
	// second candidate marks it dirty.)
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink(b, p))
	require.NoError(t, os.Chtimes(p, old, old))
	// A real Write event arms a flush for B; it parks only if the consumer
	// is not draining fast enough — not deterministic. Use the deterministic
	// park: hold the channel with a non-draining reader momentarily.
	stopDrain := make(chan struct{})
	parkHold := make(chan FileEvent)
	go func() {
		for {
			select {
			case <-stopDrain:
				return
			case <-parkHold:
			}
		}
	}()
	_ = parkHold
	// B's flush parks in emitBlocking (consumer paused via delivered>=4 not
	// yet reached... consumer IS draining). Deterministic park alternative:
	// claim B directly (what the flush would do), settle dirty later.
	ib, err := fileid.Stat(p)
	require.NoError(t, err)
	tokB, okB := w.claimDelivery(nil, p, ib, ib.SnapAtNs)
	require.True(t, okB, "B's claim must be admitted")
	// A second candidate (B') marks B's token dirty (same lifecycle map —
	// scanFile settles into it, so it must not be nil).
	require.NoError(t, w.scanFile(ctx, events, seenSnapshot, p, mustInfo(t, p)))
	w.seenMu.Lock()
	dirty := tokB.dirty
	w.seenMu.Unlock()
	require.True(t, dirty, "the second candidate must mark B's parked token dirty")
	close(stopDrain)
	// Settle B's claim dirty → the recheck re-examines and delivers B.
	w.completeDelivery(seenSnapshot, p, tokB, true)
	require.Eventually(t, func() bool {
		return recheckFires.Load() >= 1
	}, 3*time.Second, 10*time.Millisecond, "the dirty recheck must fire for B")
	require.Eventually(t, func() bool {
		s := w.lifecycleSeen()
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		cur, known := s[p]
		idNow, err := fileid.Stat(p)
		return err == nil && known && cur.ident.Ino == idNow.Ino && cur.ident.MtimeNs == idNow.MtimeNs
	}, 3*time.Second, 10*time.Millisecond, "after the recheck, seen must hold B — in the ONE shared map")

	// Step 3: path → back to A. The NORMAL path (scanFile) must DELIVER A:
	// with ONE map, seen holds B → seenEqual(A) fails → delivered. (Pre-fix
	// split: the normal map still held A → suppressed → A lost.)
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink(a, p))
	infoA, err := os.Stat(p)
	require.NoError(t, err)
	before := len(delivered)
	require.NoError(t, w.scanFile(ctx, events, w.lifecycleSeen(), p, infoA))
	require.Eventually(t, func() bool {
		return len(delivered) > before
	}, 3*time.Second, 10*time.Millisecond,
		"returned A suppressed after B was delivered — the recheck's B must be visible to the normal path's seen (ONE map)")
	require.True(t, recheckFires.Load() >= 1)
	fmt.Printf("split_seen deliveries=%d recheck_fires=%d\n", len(delivered), recheckFires.Load())

	cancel()
	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
	}
}

// dirOf returns the directory portion of a path (test helper for New()).
func dirOf(path string) string {
	return filepath.Dir(path)
}
