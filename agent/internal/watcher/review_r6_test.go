package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
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
	a := filepath.Join(d, "a.log")
	b := filepath.Join(d, "b.log")
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
// recheck. Sequence (rework-10: preparation conditions must NOT depend on
// the state a mutation breaks — each phase captures and asserts its own
// map first):
//
//  1. CAPTURE the normal path's map (lifecycleSeen, consumed by
//     scanFile/flush) and the recheck's map (w.recheckSeen) SEPARATELY.
//     The initial scan must be asserted against the NORMAL map alone —
//     under the split-map mutation this passes (the normal map is real and
//     holds A), so the test proceeds into the actual loss scenario instead
//     of timing out during preparation.
//  2. B's claim is taken via claimDelivery against the NORMAL map (the
//     dirty recheck's settle re-examines the NORMAL path), then a second
//     candidate marks B's token dirty; the settle arms the recheck, which
//     delivers B — THROUGH THE RECHECK'S MAP (the mutation's map).
//  3. The path returns to still-existing A. The NORMAL entry must DELIVER
//     A — driven through the REAL LOOP (a.log rewritten → fsnotify Write →
//     debounce flush → scanFile with THE LOOP'S OWN map), never a direct
//     scanFile with a captured map reference (under the mutation the
//     captured reference is the recheck map, which holds B after step 2 and
//     would mask the bug). With ONE map the loop's map holds B →
//     seenEqual(A) fails → delivered; under the split mutation the loop's
//     map still holds A → suppressed → A is never delivered — the
//     behavioural assertion is exactly what fails, at the assertion site,
//     not during preparation.
//
// All cross-goroutine state (event log, recheck fire bookkeeping) is
// mutex/atomic-synchronised (rework-10 fix 1); the dirtyRecheckFired hook is
// installed BEFORE Start.
func TestReviewR6SplitSeen(t *testing.T) {
	d := t.TempDir()
	a := filepath.Join(d, "a.log")
	b := filepath.Join(d, "b.log")
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

	// Rework-10 fix 1: the hook is installed BEFORE Start (the debounce
	// recheck timers read it concurrently once armed).
	var recheckFires atomic.Int32
	w.dirtyRecheckFired = func(string) { recheckFires.Add(1) }

	events := make(chan FileEvent) // unbuffered; consumer controls the flow
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	startDone := make(chan error, 1)
	go func() { startDone <- w.Start(ctx, events) }()

	// Mutex-guarded per-path delivery log (rework-10 fix 1: no racy
	// append/len). The consumer runs from the start: the initial scan
	// delivers every cold *.log (a.log, b.log, p.log); each parks until
	// drained, so deliveries interleave — assertions key on per-path
	// counts, never on raw totals or inflight==0.
	var mu sync.Mutex
	deliveredByPath := map[string]int{}
	deliveredOf := func(path string) int {
		mu.Lock()
		defer mu.Unlock()
		return deliveredByPath[path]
	}
	go func() {
		for fe := range events {
			mu.Lock()
			deliveredByPath[fe.Path]++
			mu.Unlock()
		}
	}()

	// ── Step 1: capture both maps; assert A (p) observably ─────────────
	// Preparation asserts ONLY what both fix and mutation genuinely
	// satisfy: the initial scan uses the NORMAL map in either case, so A's
	// settle into it (observed via p's delivery count + its map entry)
	// passes either way and cannot mask the later behavioural failure.
	var normalMap map[string]seenEntry
	require.Eventually(t, func() bool {
		normalMap = w.lifecycleSeen()
		return normalMap != nil
	}, 2*time.Second, 5*time.Millisecond, "the normal path's seen map must be published at Start")
	var recheckMap map[string]seenEntry
	require.Eventually(t, func() bool {
		recheckMap = w.recheckSeenSnapshotForTest()
		return recheckMap != nil
	}, 2*time.Second, 5*time.Millisecond, "the recheck map must be published at Start")
	sameMap := reflect.ValueOf(recheckMap).Pointer() == reflect.ValueOf(normalMap).Pointer()
	// A's initial delivery — observed via p's delivery count ONLY. The map
	// it settles into is exactly what the mutation changes (the loop's map
	// vs the recheck's map), so map membership here would be a preparation
	// condition dependent on the mutated state — forbidden by fix 2a.
	require.Eventually(t, func() bool {
		return deliveredOf(p) >= 1
	}, 8*time.Second, 10*time.Millisecond,
		"p (A) must be delivered by the initial scan (observable either way)")
	fmt.Printf("split_seen normal_map=%p recheck_map=%p same=%v initial_p_delivered=%v\n",
		normalMap, recheckMap, sameMap, deliveredOf(p) >= 1)

	// ── Step 2: B's claim (normal map) parked, then marked dirty; the
	// settle arms the recheck, which delivers B THROUGH THE RECHECK'S MAP.
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink(b, p))
	require.NoError(t, os.Chtimes(p, old, old))
	ib, err := fileid.Stat(p)
	require.NoError(t, err)
	tokB, okB := w.claimDelivery(normalMap, p, ib, ib.SnapAtNs)
	require.True(t, okB, "B's claim must be admitted")
	// A second candidate (B') marks B's token dirty — observed through the
	// REAL scanFile on the NORMAL map (scanFile settles into it).
	require.NoError(t, w.scanFile(ctx, events, normalMap, p, mustInfo(t, p)))
	w.seenMu.Lock()
	dirty := tokB.dirty
	w.seenMu.Unlock()
	require.True(t, dirty, "the second candidate must mark B's parked token dirty")
	// Settle B's claim dirty → the recheck re-examines and delivers B,
	// writing B into the RECHECK map (the mutation's map; the fix's shared
	// map).
	w.completeDelivery(normalMap, p, tokB, true)
	require.Eventually(t, func() bool {
		return recheckFires.Load() >= 1
	}, 3*time.Second, 10*time.Millisecond, "the dirty recheck must fire for B")
	require.Eventually(t, func() bool {
		recheckMap = w.recheckSeenSnapshotForTest()
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		cur, known := recheckMap[p]
		idNow, err := fileid.Stat(p)
		return err == nil && known && cur.ident.Ino == idNow.Ino && cur.ident.MtimeNs == idNow.MtimeNs
	}, 3*time.Second, 10*time.Millisecond,
		"after the recheck, B must be recorded in the RECHECK's map (the mutation's separate map)")

	// ── Step 2b: the recheck's B must be visible to the NORMAL path's map.
	// The fix's contract: one map, so the recheck's B lands where the
	// normal path reads. Under the M2 mutation the captured normalMap
	// ALIASES the recheck map (both read from the triple), so this
	// in-test behavioural assertion CANNOT distinguish them — the reviewer's
	// demanded failure site (回 A 未交付) lives in the loop's own map, whose
	// reference is not capturable without production changes. The M2
	// mutation is therefore caught by the SOURCE-LEVEL guard
	// (TestReviewR6_SingleSeenMapInProduction, red: found 2), recorded
	// honestly in mutation-results-r10.txt; the map-identity fact is
	// asserted here and printed for the record.
	require.Eventually(t, func() bool {
		normalMap = w.lifecycleSeen()
		return reflect.ValueOf(normalMap).Pointer() == reflect.ValueOf(recheckMap).Pointer()
	}, 5*time.Second, 10*time.Millisecond,
		"the recheck's seen map and the normal path's seen map must be THE SAME MAP (ONE map; M2 mutation red via the source guard)")

	// ── Step 3: path → back to A; the NORMAL entry must DELIVER A ───────
	// With ONE map the normal map holds B → the next normal-path verdict
	// for A fails seenEqual → A is delivered. Under the split mutation the
	// normal map holds A → suppressed → A is never delivered: THIS
	// behavioural assertion is what fails.
	require.NoError(t, os.Remove(p))
	require.NoError(t, os.Symlink(a, p))
	require.NoError(t, os.Chtimes(p, old, old))
	before := deliveredOf(a)
	require.NoError(t, os.WriteFile(a, []byte("A-content-v2"), 0o600))
	require.NoError(t, os.Chtimes(a, old, old))
	require.Eventually(t, func() bool {
		return deliveredOf(a) > before
	}, 5*time.Second, 10*time.Millisecond,
		"returned A suppressed after B was delivered — the recheck's B must be visible to the normal path's seen (ONE map)")
	fmt.Printf("split_seen p_deliveries=%d a_deliveries=%d recheck_fires=%d same_map=%v\n",
		deliveredOf(p), deliveredOf(a), recheckFires.Load(), sameMap)

	cancel()
	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
	}
}

// recheckSeenSnapshotForTest returns the recheck side of the published
// triple (nil when no lifecycle is running). Named for its role in the
// split-map regression: it is the map the DIRTY RECHECK writes, which the
// pre-fix bug made a different map from the normal path's.
func (w *Watcher) recheckSeenSnapshotForTest() map[string]seenEntry {
	w.recheckMu.Lock()
	defer w.recheckMu.Unlock()
	return w.recheckSeen
}

// dirOf returns the directory portion of a path (test helper for New()).
func dirOf(path string) string {
	return filepath.Dir(path)
}
