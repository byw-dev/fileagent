//go:build linux && overflow

// Debounced-mode sibling of TestInotifyOverflow_SafetyNetRescan_
// RecoversLostFiles (AUD-10 / D-035). The tail-mode test pins the overflow →
// handleWatchError → safetyNetRescan chain on loopFsnotify — a mode that is
// fail-closed BLOCKED in production (IC-BUG-46), so the only end-to-end
// coverage of the recovery chain ran on a mode no production watcher walks.
// This file pins the same chain on the DEFAULT mode (append_mode=overwrite,
// debounced), including the rescan leg the injected-erc unit tests cannot
// cover end to end: pollScan skipping files that are still hot (inside the
// debounce window) and scheduleDebounceRechecks re-arming their delivery.
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
	// EMIT them (not skip them as hot), so the scan parks on the first
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
	// Widen the debounce window so every burst file is still HOT when the
	// safety-net rescan runs (the burst finishes seconds before the rescan
	// starts; with the 500ms production default the early files would have
	// gone quiet and taken the direct-emit branch instead). With this window
	// the rescan MUST take the branch under audit here — pollScan skipping
	// hot files and scheduleDebounceRechecks re-arming them — and delivery
	// happens when the rechecks fire past the window.
	w.debounce = 10 * time.Second

	// Unbuffered channel: the parked consumer is what wedges the initial
	// scan's first emission.
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

	// Let Start register the watch and wedge the initial scan on seed #1.
	time.Sleep(500 * time.Millisecond)

	t.Logf("burst: creating %d files while the initial scan is parked on the seed emission", numFiles)
	for i := 0; i < numFiles; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f-%05d.dat", i)), []byte("x"), 0o644); err != nil {
			t.Fatalf("creating burst file %d: %v", i, err)
		}
	}

	// The kernel queue has long since overflowed (nothing drains it while
	// the scan is parked). Release the consumer: the seeds get delivered,
	// the scan finishes, the loop drains what survived of the burst, reads
	// IN_Q_OVERFLOW from fw.Errors, and the safety-net rescan recovers the
	// lost files — skipping the (hot) burst files and re-arming them via
	// debounce rechecks, which deliver once the window has passed.
	time.Sleep(2 * time.Second)
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
			t.Logf("all %d burst files (+2 seeds) collected and the overflow error was observed on fw.Errors; the debounced safety-net rescan closed the gap (hot files skipped and re-armed via debounce rechecks)", numFiles)
			return
		}
		select {
		case <-deadline:
			t.Fatalf("collected only %d of %d files", n, numFiles+2)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
