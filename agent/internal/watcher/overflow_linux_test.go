//go:build linux && overflow

// Real kernel-queue overflow test for the IC-BUG-44 safety-net rescan.
//
// This file is deliberately double-gated:
//   - build tag `overflow` (plus `linux`): plain `go test ./...` never even
//     compiles it, so it cannot accidentally run in the normal suite;
//   - env FILEAGENT_TEST_INOTIFY_OVERFLOW=1: the CI step (ci-agent.yml, Linux
//     only) sets it explicitly. If the gate is closed the test SKIPS — and
//     the CI step additionally greps the -v output for the "gate open" log
//     line, so a misconfigured gate fails the step instead of silently
//     skipping.
//
// macOS (kqueue) is NOT an authoritative environment for overflow behaviour
// (it may drop silently); Linux inotify is the production backend, so this
// test only builds on linux.
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

// TestInotifyOverflow_SafetyNetRescan_RecoversLostFiles reproduces the real
// IC-BUG-44 scenario end to end:
//
//  1. the watcher is started on an empty directory (fsnotify path);
//  2. a burst of files is created while the consumer channel is parked —
//     emitBlocking wedges the event loop on its first delivery, fsnotify's
//     backend stops draining the kernel watch queue, and the queue
//     (fs.inotify.max_queued_events, default 16384) overflows;
//  3. the overflow surfaces on fw.Errors as fsnotify.ErrEventOverflow;
//  4. handleWatchError must trigger the safety-net rescan, and every file
//     created during the burst — including the ones whose create events were
//     lost to the overflow — must eventually be collected.
func TestInotifyOverflow_SafetyNetRescan_RecoversLostFiles(t *testing.T) {
	if os.Getenv("FILEAGENT_TEST_INOTIFY_OVERFLOW") != "1" {
		t.Skip("gate closed: set FILEAGENT_TEST_INOTIFY_OVERFLOW=1 to run the real kernel-queue overflow test")
	}
	t.Log("gate open: FILEAGENT_TEST_INOTIFY_OVERFLOW=1, running real inotify overflow test")

	const numFiles = 20000 // ≫ a sane fs.inotify.max_queued_events (default 16384)

	// The burst can only overflow the kernel queue if it is larger than
	// fs.inotify.max_queued_events. On machines where the limit has been
	// raised (some container VMs set it to 1M+), the test MUST fail loudly
	// instead of "passing" without ever exercising the overflow path.
	// The CI step lowers the limit explicitly (sudo sysctl -w) before running.
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
	// Observe the watcher's Warn logs so the test can PROVE the overflow
	// error really surfaced on fw.Errors (and was not merely simulated by a
	// big kernel queue with no overflow at all).
	logCore, observed := observer.New(zap.WarnLevel)
	w, err := New(dir, "*.dat", false, time.Hour, AppendModeOverwrite, zap.New(logCore))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	// Unbuffered channel: the parked consumer wedges emitBlocking, which is
	// what stops the fsnotify backend from draining the kernel queue.
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

	// Let Start register the watch and finish the (empty) initial scan.
	time.Sleep(500 * time.Millisecond)

	t.Logf("burst: creating %d files while the consumer is parked", numFiles)
	for i := 0; i < numFiles; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("f-%05d.dat", i)), []byte("x"), 0o644); err != nil {
			t.Fatalf("creating burst file %d: %v", i, err)
		}
	}

	// Give the wedged loop and the kernel queue time to overflow for real.
	time.Sleep(2 * time.Second)
	close(consumeGate)

	t.Log("consumer released; waiting for overflow error + safety-net rescan to collect everything")
	deadline := time.After(150 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n >= numFiles {
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
			t.Logf("all %d files collected and the overflow error was observed on fw.Errors; safety-net rescan closed the gap", numFiles)
			return
		}
		select {
		case <-deadline:
			t.Fatalf("collected only %d of %d files", n, numFiles)
		case <-time.After(200 * time.Millisecond):
		}
	}
}
