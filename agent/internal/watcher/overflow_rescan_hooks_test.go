package watcher

// Platform-independent guard for the safety-net rescan's debounced branches
// (AUD-10 round 2). The linux&&overflow e2e sibling re-asserts the same two
// counters inside a real kernel-overflow run on CI; this test pins the
// branches themselves deterministically — no overflow, no wall-clock
// assumption — so it runs in the normal suite on every platform.

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestSafetyNetRescan_HotSkipAndRearmObserved drives handleWatchError with
// the exact production overflow sentinel and requires the rescan's hot-skip
// and re-arm branches to be OBSERVED, not inferred.
//
// Why inference is unsound and observation is the only way: a rescan that
// finds a file gone quiet emits it DIRECTLY (pollScan), and a recheck-armed
// file is delivered through the same channel as a flush-delivered one — no
// delivery pattern distinguishes "recovered via hot-skip + recheck re-arm"
// from "recovered via direct emit". Under the known regression (safetyNetRescan
// dropping its scheduleDebounceRecheck re-arm), the collection assertions
// below would STILL pass on the quiet file and on any flush delivery — the
// rescanRecheckArmed counter is what turns that mutation red.
func TestSafetyNetRescan_HotSkipAndRearmObserved(t *testing.T) {
	dir := t.TempDir()

	// One QUIET file (mtime an hour old): outside the debounce window, so
	// the rescan's pollScan must emit it directly via scanFile.
	quiet := filepath.Join(dir, "quiet.dat")
	if err := os.WriteFile(quiet, []byte("q"), 0o644); err != nil {
		t.Fatalf("write quiet file: %v", err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(quiet, old, old); err != nil {
		t.Fatalf("backdate quiet file: %v", err)
	}

	// One HOT file (mtime now): inside the debounce window, so pollScan must
	// SKIP it and the rescan must re-arm a recheck — which delivers it once
	// the window has passed.
	hot := filepath.Join(dir, "hot.dat")
	if err := os.WriteFile(hot, []byte("h"), 0o644); err != nil {
		t.Fatalf("write hot file: %v", err)
	}

	w, err := New(dir, "*.dat", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	w.debounce = 250 * time.Millisecond

	var skips, arms atomic.Int32
	w.rescanSkippedHot = func(string) { skips.Add(1) }
	w.rescanRecheckArmed = func(string) { arms.Add(1) }

	events := make(chan FileEvent, 8)
	var mu sync.Mutex
	got := make(map[string]bool)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go func() {
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

	// The exact production trigger: the sentinel fsnotify delivers for
	// Linux IN_Q_OVERFLOW and the Windows buffer overflow alike.
	w.handleWatchError(ctx, events, make(map[string]time.Time), fsnotify.ErrEventOverflow)

	assert.Equal(t, int32(1), skips.Load(),
		"pollScan's hot-skip branch must have been observed for the hot file")
	assert.Equal(t, int32(1), arms.Load(),
		"the rescan must have been observed re-arming a recheck for the hot file")

	// The quiet file comes back directly from the rescan's scan; the hot
	// file comes back through the recheck once the window has passed
	// (~window - age + grace ≈ 350ms).
	require.Eventually(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return got[quiet] && got[hot]
	}, 3*time.Second, 20*time.Millisecond,
		"both the directly-emitted quiet file and the recheck-armed hot file must be collected")
}
