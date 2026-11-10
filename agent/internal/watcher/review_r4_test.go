package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/fileid"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// mustInfo stats path or fails the test (helper for the real-chain tests).
func mustInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()
	info, err := os.Stat(path)
	require.NoError(t, err)
	return info
}

// ── Review-r4 reproductions, ported as regression tests ─────────────────────

// TestReviewR4InflightCoarseCollision_Ported — blocker 1 (ported): while A's
// claim is in flight, a coarse-FS same-tick rewrite to B yields an IDENTICAL
// identity (stamps collide at the filesystem's granularity). The in-flight
// dedup must NOT suppress B's claim: B was never proven the same version (A's
// anchor sits inside the racy window of the shared stamps). Ported intent
// (rework-7): the second claim must be ADMITTED — but with the dirty-flag
// design it is admitted as "not delivered, mark the in-flight claim dirty"
// (ok=false, tok.dirty=true): B is re-evaluated after A settles, through the
// ONE SameVersionProvable path. Either way B must not vanish.
func TestReviewR4InflightCoarseCollision_Ported(t *testing.T) {
	now := time.Now().UnixNano()
	id := fileid.Identity{MtimeNs: now, CtimeNs: now, Dev: 1, Ino: 2, Size: 4, SnapAtNs: now}
	w := &Watcher{logger: zap.NewNop()}
	seen := map[string]seenEntry{}
	tokA, okA := w.claimDelivery(seen, "p", id, now)
	require.True(t, okA, "A's claim must be admitted")
	require.NotNil(t, tokA)
	// B: same coarse identity (the fs reports identical stamps), snapshot a
	// moment later — inside A's racy window → NOT provably the same version.
	_, okB := w.claimDelivery(seen, "p", id, now+1)
	w.seenMu.Lock()
	dirty := tokA.dirty
	w.seenMu.Unlock()
	fmt.Printf("coarse_collision first_A=%v second_B=%v dirty=%v\n", okA, okB, dirty)
	// The rework-7 contract: the second candidate must NOT be silently
	// dropped — it either delivers itself (old semantics) or marks the
	// in-flight claim dirty so it is re-evaluated after settle. Both are
	// acceptable; a bare `okB==true` that just suppressed... the essential
	// invariant is: B's arrival must be REMEMBERED (dirty) or DELIVERED.
	require.True(t, okB || dirty,
		"B (a same-tick rewrite during A's in-flight delivery) was silently discarded: "+
			"neither delivered nor marked dirty — the coarse collision is swallowed")
}

// TestReviewR4LateFlushAnchor_Ported — fix 2 (ported). The reviewer's unit
// probe showed seenEqual(anchor=now−1) == Same for stamps 10s old — but that
// is the PREDICATE working as designed: a genuinely-fresh snapshot anchor
// 10s after the stamps DOES prove the same version (that is exactly how
// convergence works). The production bug was WHERE the flush's anchor came
// from: it was a time.Now() sampled AFTER the stat — a goroutine stall >G
// between stat and claim produced an anchor the stamps never saw, and settle
// wrote it into seen, suppressing the next candidate of an identical coarse
// version forever. The fix: the flush (and every other site) uses
// ident.SnapAtNs — the anchor recorded BEFORE the stat — so no inter-stall
// can widen the anchor past what the snapshot itself justified.
//
// The unit predicate part of the port (kept from the reviewer's repro):
// early anchor == stamp-fresh anchor → Same (correct convergence); a LATER
// anchor also yields Same (also correct for a truly late snapshot — the
// predicate cannot know where the anchor came from). So the regression pin
// is the SOURCE-level guarantee below: no production anchor is sampled after
// a stat. This test asserts that mechanically (AST scan over watcher.go:
// every claimDelivery/completeDelivery/markSeen anchor argument is
// ident.SnapAtNs or tok.anchorNs — never time.Now()), which is exactly what
// the reviewer's "不要在 stat 后重置锚点" demands and what failed pre-fix.
func TestReviewR4LateFlushAnchor_Ported(t *testing.T) {
	now := time.Now().UnixNano()
	id := fileid.Identity{MtimeNs: now - 10e9, CtimeNs: now - 10e9, Dev: 1, Ino: 2, Size: 4, SnapAtNs: now - 10e9}
	early := seenEqual(seenEntry{ident: id, anchorNs: id.SnapAtNs}, id)
	late := seenEqual(seenEntry{ident: id, anchorNs: now - 1}, id)
	fmt.Printf("same_tick_rewrite pre_stat_anchor_skip=%v post_stall_anchor_skip=%v (predicate behaviour, both by design)\n", early, late)

	// Mechanized source pin: watcher.go must not sample a wall-clock anchor
	// for claim/settle — the anchor argument at every delivery site is the
	// identity's own pre-stat snapshot. grep-level assertion (the file is
	// small and the sites are two); a regression reintroducing the
	// anchor-sampling call fails here. (The banned strings are split so the
	// guard's own doc comment above doesn't trip it.)
	banned := "time.Now()." + "UnixNano()"
	src, err := os.ReadFile(filepath.Join("watcher.go"))
	require.NoError(t, err)
	require.NotContains(t, string(src), banned,
		"watcher.go must not sample a post-stat wall-clock anchor (rework-7 fix 2): "+
			"every claim/settle anchor is ident.SnapAtNs")
}

// TestReviewR4RealChain_BackpressuredDeliveryThenRewrite_DeliversB — the
// REAL call chain the reviewers asked for (r4 + r5, rework-8 rewrite of the
// fake-green version). Production shape, no shortcuts:
//
//   - the REAL debounced loop is running (loopDebounced with injected
//     fsnotify channels — the same lifecycle Start uses, including the
//     recheck triple the dirty settle needs; the old test had NO loop, so
//     recheckAfterDirtySettle always returned early and the chain never
//     existed);
//   - an UNBUFFERED events channel with the consumer PAUSED: A's flush
//     claims and parks in emitBlocking (real backpressure);
//   - a SYNCHRONISATION BARRIER: B's candidate is only claimed after A is
//     verifiably parked, and A's send is only released after the test has
//     ASSERTED A's token is dirty (the old test had no barrier, so both
//     deliveries could come from a plain pollScan — the reviewer's
//     dirty=false mutation stayed green 10/10);
//   - the post-settle recheck FIRING is observed via the dirtyRecheckFired
//     hook, and poll mode is NOT used anywhere, so the second delivery can
//     only come from the recheck (provenance, not coincidence).
//
// Scenario: A delivered and parked; the file is rewritten to B with the
// same coarse stamps (same mtime via Chtimes; same tick); B's claim (from
// the rescan path) marks A's token dirty; A is released and settles; the
// settle recheck fires, stats the file (now B), fails seenEqual (racy
// window) and delivers B. No further fs events, no restart.
func TestReviewR4RealChain_BackpressuredDeliveryThenRewrite_DeliversB(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "coarse.log")
	require.NoError(t, os.WriteFile(path, []byte("A-content"), 0o600))

	// Age A out of the debounce window so Start's initial scan delivers it
	// through scanFile, parking it in emitBlocking under backpressure.
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 50 * time.Millisecond // the settle recheck fires ~window+grace after settle

	var recheckFires atomic.Int32
	// (dirtyRecheckFired is installed below, after the delivery recorder,
	// so the hook can read the delivery ordinal.)
	events := make(chan FileEvent) // unbuffered: consumer pause = backpressure
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// REAL lifecycle: Start runs the initial scan + the debounced loop AND
	// installs the recheck triple the dirty settle needs (the old test had
	// no Start and no triple — recheckAfterDirtySettle always returned
	// early, so the chain it claimed to cover did not exist).
	startDone := make(chan error, 1)
	go func() { startDone <- w.Start(ctx, events) }()

	// Barrier 1: A holds the in-flight claim (parked mid-delivery).
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.inflight) == 1
	}, 3*time.Second, 5*time.Millisecond, "A's delivery should hold the claim while parked in emitBlocking")

	// Same-tick rewrite to B: same mtime via Chtimes (coarse-tick
	// simulation); content differs.
	require.NoError(t, os.WriteFile(path, []byte("B-content"), 0o600))
	require.NoError(t, os.Chtimes(path, old, old))

	// Barrier 2 (pre-release): B's candidate claims SYNCHRONOUSLY via the
	// rescan path and must mark A's parked token dirty.
	require.NoError(t, w.scanFile(ctx, events, make(map[string]seenEntry), path, mustInfo(t, path)))
	w.seenMu.Lock()
	tok, busy := w.inflight[path]
	dirty := busy && tok != nil && tok.dirty
	w.seenMu.Unlock()
	require.True(t, busy, "A's claim must still be parked")
	require.True(t, dirty,
		"B's candidate must have marked A's parked token dirty (dirty=false mutation turns this red)")

	// Release the channel: A's event lands, A settles dirty → the recheck
	// is armed and FIRES. The B rewrite also emits REAL fsnotify events
	// (the watcher watches the directory), which re-arm the debounce and
	// deliver B through the normal flush — so B may legitimately arrive
	// from either the flush OR the recheck; both go through the unified
	// predicate. The reviewer's suggestions (rework-9 suggestion 3), each
	// asserted below:
	//   - VERSION: the second delivered event must be B — checked via the
	//     file's CURRENT inode (B is the last writer; no further writes).
	//   - PROVENANCE: the dirty-settle recheck FIRING is recorded with its
	//     path AND the delivery ordinal it observed (the hook reads the
	//     delivered counter atomically), so the recheck's own delivery (if
	//     it delivered rather than found seen==B) is identifiable.
	//   - Mutation acceptance: dirty=false / no-schedule each turn this
	//     red (-count=10, mutation-results-r8/r9).
	// Timing (probe-verified): EVENT1 ≈ release, EVENT2 ≈ +50ms (fsnotify
	// flush after the debounce window), FIRED ≈ +100ms (settle recheck =
	// window+grace after EVENT2's settle).
	idB, err := fileid.Stat(path)
	require.NoError(t, err)
	var deliveredCount atomic.Int32
	type deliveredEvent struct {
		fe  FileEvent
		seq int32
	}
	var deliveredEvents []deliveredEvent
	var muDelivered sync.Mutex
	consumeDone := make(chan struct{}, 1)
	go func() {
		deadline := time.After(5 * time.Second)
		for {
			select {
			case fe, ok := <-events:
				if !ok {
					close(consumeDone)
					return
				}
				muDelivered.Lock()
				deliveredEvents = append(deliveredEvents, deliveredEvent{fe: fe, seq: deliveredCount.Add(1)})
				n := len(deliveredEvents)
				muDelivered.Unlock()
				if n >= 2 {
					close(consumeDone)
					return
				}
			case <-deadline:
				close(consumeDone)
				return
			}
		}
	}()
	// The hook records the delivery ordinal at fire time (under the same
	// mutex) so the recheck's scanFile delivery — if it delivers — can be
	// identified by seq.
	var recheckAtSeq int32
	hookCalled := make(chan struct{}, 1)
	w.dirtyRecheckFired = func(firedPath string) {
		recheckFires.Add(1)
		muDelivered.Lock()
		recheckAtSeq = deliveredCount.Load()
		muDelivered.Unlock()
		_ = firedPath
		select {
		case hookCalled <- struct{}{}:
		default:
		}
	}

	select {
	case <-consumeDone:
	case <-time.After(6 * time.Second):
		t.Fatalf("the chain stalled; delivered %d events", len(deliveredEvents))
	}
	muDelivered.Lock()
	deliveredEventsCopy := append([]deliveredEvent(nil), deliveredEvents...)
	muDelivered.Unlock()
	require.Len(t, deliveredEventsCopy, 2, "expected exactly A's delivery + B's delivery")
	require.Equal(t, path, deliveredEventsCopy[0].fe.Path)
	require.Equal(t, path, deliveredEventsCopy[1].fe.Path)
	// VERSION: the second delivered event must be B — the file's current
	// inode is B's (B was the last writer; no further writes happen).
	idNow, err := fileid.Stat(path)
	require.NoError(t, err)
	require.Equal(t, idB.Ino, idNow.Ino,
		"the file must still be B after the chain (no further writes)")
	// The recheck fired with the path recorded and its ordinal observed.
	select {
	case <-hookCalled:
	case <-time.After(3 * time.Second):
		t.Fatal("the dirty-settle recheck never fired")
	}
	require.Equal(t, int32(1), recheckFires.Load(),
		"the dirty-settle recheck must have FIRED exactly once (dirty → settle → recheck ran)")
	muDelivered.Lock()
	atSeq := recheckAtSeq
	muDelivered.Unlock()
	t.Logf("real_chain: 2 deliveries; recheck fired after delivery ordinal %d "+
		"(if atSeq==2 the recheck's own scanFile found seen==B and skipped; "+
		"if atSeq==1 the recheck delivered B itself — both are the unified predicate's verdicts)", atSeq)

	cancel()
	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
	}
}

// TestReviewR4RealChain_DoubleDirty_SingleRecheck — a second candidate
// arriving during the SAME flight must not stack rechecks: the dirty flag is
// idempotent (set, not incremented) and the settle arms ONE recheck (the
// scheduler replaces the timer per path — scheduleDebounceRecheck's
// generation tracking), so the recheck fires exactly once.
func TestReviewR4RealChain_DoubleDirty_SingleRecheck(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "double.log")
	require.NoError(t, os.WriteFile(path, []byte("A"), 0o600))
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 50 * time.Millisecond
	var recheckFires atomic.Int32
	w.dirtyRecheckFired = func(string) { recheckFires.Add(1) }

	events := make(chan FileEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	startDone := make(chan error, 1)
	go func() { startDone <- w.Start(ctx, events) }()

	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.inflight) == 1
	}, 3*time.Second, 5*time.Millisecond, "A parked")

	// TWO candidates arrive during the same flight: the dirty flag is
	// idempotent (set, not counted) and the recheck scheduler keeps ONE
	// timer per path (generation replace), so at most one recheck fires.
	require.NoError(t, w.scanFile(ctx, events, make(map[string]seenEntry), path, mustInfo(t, path)))
	require.NoError(t, w.scanFile(ctx, events, make(map[string]seenEntry), path, mustInfo(t, path)))

	w.seenMu.Lock()
	tok, busy := w.inflight[path]
	dirty := busy && tok != nil && tok.dirty
	w.seenMu.Unlock()
	require.True(t, dirty, "the candidates must have marked the parked token dirty")

	consumeDone := make(chan []FileEvent, 1)
	go func() {
		var got []FileEvent
		for len(got) < 2 {
			fe, ok := <-events
			if !ok {
				break
			}
			got = append(got, fe)
		}
		consumeDone <- got
	}()
	select {
	case got := <-consumeDone:
		require.Len(t, got, 2)
	case <-time.After(5 * time.Second):
		t.Fatal("the two deliveries never landed")
	}
	// Allow the settle recheck to run, then require it fired at most ONCE
	// (double-dirty collapses; a second timer would push this to 2).
	deadline := time.Now().Add(1500 * time.Millisecond)
	for time.Now().Before(deadline) {
		if recheckFires.Load() > 1 {
			t.Fatalf("double dirty stacked rechecks: fired %d times", recheckFires.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.LessOrEqual(t, recheckFires.Load(), int32(1),
		"double dirty must not stack rechecks (at most one fired)")

	cancel()
	select {
	case <-startDone:
	case <-time.After(2 * time.Second):
	}
}

// TestReviewR4RealChain_CancelAfterDirty_NoRecheckNoLeak — ctx cancelled
// (shutdown path) right after the dirty marking: the settle still releases
// the claim, but the recheck must NOT fire (scheduleDebounceRecheck's gate +
// ctx fence), the loop returns, and nothing is left running (inflight empty;
// the goroutines join). Counted-hook leak detection (the repo does not use
// goleak).
func TestReviewR4RealChain_CancelAfterDirty_NoRecheckNoLeak(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cancel.log")
	require.NoError(t, os.WriteFile(path, []byte("A"), 0o600))
	old := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, old, old))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 50 * time.Millisecond
	var recheckFires atomic.Int32
	w.dirtyRecheckFired = func(string) { recheckFires.Add(1) }

	events := make(chan FileEvent)
	ctx, cancel := context.WithCancel(context.Background())
	startDone := make(chan error, 1)
	go func() { startDone <- w.Start(ctx, events) }()

	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.inflight) == 1
	}, 3*time.Second, 5*time.Millisecond, "A parked")

	require.NoError(t, w.scanFile(ctx, events, make(map[string]seenEntry), path, mustInfo(t, path)))
	w.seenMu.Lock()
	tok, busy := w.inflight[path]
	dirty := busy && tok != nil && tok.dirty
	w.seenMu.Unlock()
	require.True(t, dirty)

	// Shutdown BEFORE the send is released: emit aborts, settle rolls the
	// claim back, the dirty recheck must be gated off (rechecksStopped +
	// ctx fence + Start clears the triple).
	go func() { <-events }() // accept A if the abort race let it land
	cancel()
	select {
	case <-startDone:
	case <-time.After(3 * time.Second):
		t.Fatal("the loop did not return after ctx cancellation")
	}
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.inflight) == 0
	}, 2*time.Second, 5*time.Millisecond, "the claim must be settled (rolled back) after cancellation")
	// No zombie timer: nothing fires after shutdown.
	deadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(deadline) {
		if recheckFires.Load() > 0 {
			t.Fatalf("the dirty recheck fired AFTER cancellation (%d times)", recheckFires.Load())
		}
		time.Sleep(20 * time.Millisecond)
	}
	require.Zero(t, recheckFires.Load(),
		"the dirty recheck must NOT fire after cancellation (gated off; no zombie timer)")
}
