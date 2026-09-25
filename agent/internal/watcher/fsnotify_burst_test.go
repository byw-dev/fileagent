package watcher

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestWatcher_FsnotifyBurst_NoEventDropped covers IC-BUG-47: the real-time
// fsnotify event loop must not drop events when the consumer channel (buffer
// 64, the production size from agent/cmd/agent/main.go) is full. Files whose
// events are dropped are never collected — their mtime/size never change
// again and no further event fires. Events are delivered with backpressure
// (emitBlocking), matching the F1 semantics already used by pollScan.
//
// Tail mode (D-035): the loop under guard here is loopFsnotify, the real-time
// path — since the debounce became universal only tail walks it, so the
// burst must drive tail to keep guarding the right loop.
func TestWatcher_FsnotifyBurst_NoEventDropped(t *testing.T) {
	const numFiles = 500
	dir := t.TempDir()
	w, err := New(dir, "*.txt", false, 0, AppendModeTail, zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 64) // production consumer buffer size
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	go func() { _ = w.Start(ctx, events) }()

	// A deliberately slow consumer: it drains the channel far more slowly than
	// the burst below fills it, so with a dropping send the 64-slot buffer
	// overflows and events are lost. The delay is short (200µs) so the
	// blocking pipeline never stalls the fsnotify backend long enough to
	// stress OS-level event buffering (kqueue / ReadDirectoryChangesW) — the
	// OS layer dropping events is IC-BUG-44's territory, not this test's.
	var mu sync.Mutex
	got := make(map[string]bool, numFiles)
	go func() {
		for ev := range events {
			time.Sleep(200 * time.Microsecond)
			mu.Lock()
			got[ev.Path] = true
			mu.Unlock()
		}
	}()

	// Give the watcher time to set up (fsnotify registration + initial scan).
	time.Sleep(200 * time.Millisecond)

	// Concurrent writers far outpacing the consumer.
	var wg sync.WaitGroup
	paths := make([]string, numFiles)
	for i := 0; i < numFiles; i++ {
		paths[i] = filepath.Join(dir, "burst-"+strconv.Itoa(i)+".txt")
	}
	for g := 0; g < 50; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := g; i < numFiles; i += 50 {
				if err := os.WriteFile(paths[i], []byte("x"), 0o644); err != nil {
					t.Errorf("write %s: %v", paths[i], err)
				}
			}
		}(g)
	}
	wg.Wait()

	// Every file must be delivered — blocking backpressure delays, never drops.
	deadline := time.After(30 * time.Second)
	for {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n == numFiles {
			break
		}
		select {
		case <-deadline:
			mu.Lock()
			t.Fatalf("only %d/%d files delivered; missing events were dropped", n, numFiles)
		case <-time.After(50 * time.Millisecond):
		}
	}
}
