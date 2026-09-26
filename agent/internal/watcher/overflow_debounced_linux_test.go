//go:build linux && overflow

// Debounced-mode sibling of TestInotifyOverflow_SafetyNetRescan_
// RecoversLostFiles (AUD-10 / D-035). The tail-mode test pins the overflow →
// handleWatchError → safetyNetRescan chain on loopFsnotify. Tail is not a
// default-available production mode (IC-BUG-46): the control plane rejects
// new and updated tail rules (fail-closed), and the executor rejects tail
// uploads. The watcher layer itself has no tail interception — runWatcher
// (agent/cmd/agent/main.go) builds the watcher directly from
// rule.AppendMode — so an active tail rule created before that fail-closed
// landed is still dispatched by the snapshot and starts a tail watcher
// (loopFsnotify); only its uploads are rejected. This file pins the same
// chain on the DEFAULT mode (append_mode=overwrite, debounced), including
// the rescan leg the injected-erc unit tests cannot cover end to end:
// pollScan skipping files that are still hot (inside the debounce window)
// and scheduleDebounceRecheck re-arming their delivery.
//
// The hot-skip / re-arm branches are OBSERVED, not inferred: the
// rescanSkippedHot / rescanRecheckArmed hooks (per-watcher fields, nil in
// production) fire only inside safetyNetRescan at exactly those two
// branches, and the test asserts both counters > 0. No debounce-window
// arrangement can substitute for this — a slow runner can let the rescan's
// pollScan stat a file past any window and emit it directly, and a
// surviving event's flush timer delivers files regardless; the widened
// window below only biases the run toward the audited branch, the counters
// are what pin it. (TestSafetyNetRescan_HotSkipAndRearmObserved pins the
// same two branches platform-independently, without any overflow at all.)
//
// The file is deliberately double-gated, exactly like the tail sibling:
//   - build tag `overflow` (plus `linux`): plain `go test ./...` never even
//     compiles it, so it cannot accidentally run in the normal suite;
//   - env FILEAGENT_TEST_INOTIFY_OVERFLOW=1: the CI step (ci-agent.yml,
//     Linux only) sets it explicitly, and greps the -v output for the
//     "gate open" / "overflow is guaranteed" log lines, so a silently
//     skipped or not-really-overflowing run fails the step.
package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestInotifyOverflow_Debounced_SafetyNetRescan_RecoversLostFiles(t *testing.T) {
	if os.Getenv("FILEAGENT_TEST_INOTIFY_OVERFLOW") != "1" {
		t.Skip("gate closed: set FILEAGENT_TEST_INOTIFY_OVERFLOW=1 to run the real kernel-queue overflow test")
	}
	t.Log("gate open: FILEAGENT_TEST_INOTIFY_OVERFLOW=1, running the debounced-mode inotify overflow test")

	const numFiles = 20000 // ≫ a sane fs.inotify.max_queued_events (default 16384)

	// Same loud precondition as the tail sibling: the burst can only overflow
	// the kernel queue if it is larger than fs.inotify.max_queued_events. The
	// CI step lowers the limit explicitly before running.
	maxQueued := 16384
	if b, err := os.ReadFile("/proc/sys/fs/inotify/max_queued_events"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && n > 0 {
			maxQueued = n
		}
	}
	if maxQueued >= numFiles {
		t.Fatalf("fs.inotify.max_queued_events=%d >= burst size %d: the burst cannot overflow the queue, so the safety-net path would not be exercised. Lower it first (e.g. sudo sysctl -w fs.inotify.max_queued_events=4096)",
			maxQueued, numFiles)
	}
	t.Logf("fs.inotify.max_queued_events=%d, burst=%d files: overflow is guaranteed", maxQueued, numFiles)

	dir := t.TempDir()

	// Seed files with a BACKDATED mtime: the debounced initial scan must
	// EMIT them (not skip them as hot), so the scan parks on a seed
	// emission below. This is the debounced analogue of the tail sibling's
	// parked-consumer wedge: in debounced mode a parked consumer does NOT
	// stop the event loop from draining fw.Events (flushes run on timer
	// goroutines), but a parked INITIAL SCAN does — Start runs pollScan
	// synchronously on its own goroutine before the event loop exists, and
	// scanFile sends through emitBlocking. While the scan is parked, nothing
	// drains fw.Events, the fsnotify backend stops reading the kernel watch
	// queue, and the burst below overflows it — deterministically.
	seed1 := filepath.Join(dir, "seed-0000.dat")
	seed2 := filepath.Join(dir, "seed-0001.dat")
	old := time.Now().Add(-time.Hour)
	for _, p := range []string{seed1, seed2} {
		if err := os.WriteFile(p, []byte("seed"), 0o644); err != nil {
			t.Fatalf("creating seed %s: %v", p, err)
		}
		if err := os.Chtimes(p, old, old); err != nil {
			t.Fatalf("backdating seed %s: %v", p, err)
		}
	}

	// Observe the watcher's Warn logs so the test can PROVE the overflow
	// error really surfaced on fw.Errors (and was not merely simulated).
	logCore, observed := observer.New(zap.WarnLevel)
	// Default production mode: append_mode=overwrite → the debounced loop
	// (runDebounced → loopDebounced), NOT loopFsnotify (that is tail-only
	// since D-035 and fail-closed blocked in production, IC-BUG-46).
	w, err := New(dir, "*.dat", false, time.Hour, AppendModeOverwrite, zap.New(logCore))
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Widen the debounce window so every burst file is EXPECTED to be hot
	// when the safety-net rescan runs — with margin for slow runners, so the
	// hot-skip + re-arm branch is the normal path, not a lucky race. The
	// window is only a bias, never the proof: what PINS the branch is the
	// rescanSkippedHot / rescanRecheckArmed observation below, which fires
	// at exactly those two production sites and is asserted unconditionally.
	// (A wall-clock window alone cannot do this: a slow runner can let the
	// rescan's pollScan stat a file past any window and emit it directly,
	// and the test would pass without ever walking the branch under audit.)
	w.debounce = 10 * time.Second

	// Observe the rescan's hot-skip and re-arm branches directly. Without
	// these counters the test could only infer the branch from deliveries —
	// and a recheck-armed file is delivered through the same channel as a
	// flush-delivered one, so the inference would be unsound.
	var rescanSkips, rescanArms atomic.Int32
	w.rescanSkippedHot = func(string) { rescanSkips.Add(1) }
	w.rescanRecheckArmed = func(string) { rescanArms.Add(1) }

	// Unbuffered channel: the rendezvous on seed #1 is the burst's
	// structural barrier (see below), and with the consumer still gated the
	// scan then parks on seed #2 — which is what wedges fw.Events.
	events := make(chan FileEvent)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	var (
		mu  sync.Mutex
		got = make(map[string]bool)
	)
	consumeGate := make(chan struct{})
	go func() {
		<-consumeGate
		for {
			select {
			case fe := <-events:
				mu.Lock()
				got[fe.Path] = true
				mu.Unlock()
			case <-ctx.Done():
				return
			}
		}
	}()

	go func() { _ = w.Start(ctx, events) }()

	// STRUCTURAL barrier, not a timing guess: receive seed #1 ourselves. On
	// an UNBUFFERED channel a receive is a rendezvous, so completing it
	// establishes happens-before with the scan's send. What the rendezvous
	// ALONE proves is exactly this:
	//
	//   - some initial pollScan has reached a send point (seed #1 was
	//     emitted through the events channel), and that scan CANNOT be
	//     finished: seed #2 is emitted next and parks there, because the
	//     consumer is still gated and we never receive again. pollScan runs
	//     synchronously on Start's goroutine before the event loop exists,
	//     so while it is parked NOTHING drains fw.Events and the burst
	//     below is guaranteed to overflow the kernel queue.
	//
	// What the rendezvous alone does NOT prove is that a kernel watch is
	// live: if Start degraded to its polling fallback, that fallback's
	// initial pollScan emits the same seed on the same channel and parks
	// identically. The watch-is-live half of the proof is machine-enforced
	// by the no-degradation WARN assertion immediately below; only the two
	// assertions together are complete.
	//
	// A fixed sleep could only guess at this, and a loaded runner breaks the
	// guess: the burst lands before the watch is registered, the queue never
	// overflows, and this CI-gating step goes red for reasons unrelated to
	// any product regression.
	select {
	case fe := <-events:
		mu.Lock()
		got[fe.Path] = true
		mu.Unlock()
	case <-time.After(60 * time.Second):
		t.Fatal("initial scan never emitted seed #1 within 60s: the scan is not parked on a seed emission, so the burst below could not deterministically overflow the kernel queue")
	}

	// The watch-is-live half of the barrier's proof (see above): the
	// rendezvous cannot distinguish the fsnotify path from the polling
	// fallback — on degradation Start logs one of the WARNs below and then
	// runs the same pollScan through the same channel, so the barrier would
	// "pass" with NO kernel watch queue to overflow, the burst would be
	// pointless, and the test would only die much later as a misleading
	// "collected only N of M files" timeout. Both WARNs are logged before
	// the fallback's scan sends, so the rendezvous guarantees they are
	// already recorded here. Fail at the barrier point with the real
	// reason instead.
	for _, e := range observed.All() {
		if strings.Contains(e.Message, "fsnotify unavailable") || strings.Contains(e.Message, "cannot add watch paths") {
			t.Fatalf("watcher degraded to polling (WARN %q): fsnotify is not usable on this runner, so there is no kernel watch queue to overflow and this test cannot exercise the overflow recovery chain at all. Check the runner's inotify instance/watch limits (fs.inotify.max_user_instances / fs.inotify.max_user_watches)", e.Message)
		}
	}

	t.Logf("burst: creating %d files while the initial scan is parked on the seed emission", numFiles)
	for i := 0; i < numFiles; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f-%05d.dat", i)), []byte("x"), 0o644); err != nil {
			t.Fatalf("creating burst file %d: %v", i, err)
		}
	}

	// The kernel queue has long since overflowed — deterministically, while
	// the scan was parked: the burst created far more inotify events than
	// fs.inotify.max_queued_events holds, and nothing drained fw.Events
	// during the burst. No settle delay is needed (and none is wanted: it
	// would only age the burst files toward the debounce window). Release
	// the consumer: the seeds get delivered, the scan finishes, the loop
	// drains what survived of the burst, reads IN_Q_OVERFLOW from
	// fw.Errors, and the safety-net rescan recovers the lost files —
	// skipping the (hot) burst files and re-arming them via debounce
	// rechecks, which deliver once the window has passed.
	close(consumeGate)

	t.Log("consumer released; waiting for overflow error + safety-net rescan (hot-skip + rechecks) to collect everything")
	deadline := time.After(150 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= numFiles+len([]string{seed1, seed2}) {
			// Fail loudly if the queue never actually overflowed: the
			// collection would then be explained by ordinary live events and
			// the safety-net path was not exercised at all.
			overflowSeen := false
			for _, e := range observed.All() {
				if strings.Contains(e.Message, "watch queue overflow") {
					overflowSeen = true
				}
			}
			if !overflowSeen {
				t.Fatal("all files collected but no overflow error ever surfaced on fw.Errors — the kernel queue did not overflow; the safety-net path was NOT exercised (check fs.inotify.max_queued_events)")
			}
			// The branch under audit must be OBSERVED, not inferred: both
			// rescan hooks fire only inside safetyNetRescan — one on
			// pollScan's hot-skip, one per actually-armed recheck. Without
			// this, a rescan whose recheck scheduling degraded (or whose
			// pollScan emitted directly on a slow runner) would still pass
			// on the collection + overflow assertions above.
			if rescanSkips.Load() == 0 || rescanArms.Load() == 0 {
				t.Fatalf("safety-net rescan branches not exercised: hot-skip=%d re-arms=%d — the recovery cannot be attributed to the rescan's hot-skip + recheck re-arm path",
					rescanSkips.Load(), rescanArms.Load())
			}
			t.Logf("all %d burst files (+2 seeds) collected; overflow observed on fw.Errors; the rescan was observed skipping %d hot files and re-arming %d rechecks",
				numFiles, rescanSkips.Load(), rescanArms.Load())
			return
		}
		select {
		case <-deadline:
			t.Fatalf("collected only %d of %d files (rescan hot-skips observed: %d, re-arms observed: %d)",
				n, numFiles+2, rescanSkips.Load(), rescanArms.Load())
		case <-time.After(200 * time.Millisecond):
		}
	}
}
