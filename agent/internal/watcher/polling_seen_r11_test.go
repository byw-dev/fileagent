package watcher

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestReviewR11_PollingLoopSeenIsLifecycleSeen — rework-11: the POLLING
// path's seen map must be THE SAME map the dirty recheck uses (the
// published triple's map). The reviewer's mutation 3 (polling builds a
// local fresh map) fails here at the pointer assertion. Unlike the
// fsnotify-path SplitSeen test, this drives runPolling directly (the same
// entry Start uses after its fallbacks) so the hook fires on the polling
// use site.
func TestReviewR11_PollingLoopSeenIsLifecycleSeen(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "p.log"), []byte("AAA"), 0o600))
	w, err := New(dir, "*.log", false, 5*time.Millisecond, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	var mu sync.Mutex
	var loopSeen map[string]seenEntry
	hookFired := make(chan struct{}, 1)
	w.loopSeenObserved = func(m map[string]seenEntry) {
		mu.Lock()
		loopSeen = m
		mu.Unlock()
		select {
		case hookFired <- struct{}{}:
		default:
		}
	}

	events := make(chan FileEvent)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Publish the lifecycle triple the same way Start does BEFORE calling
	// runPolling (Start's fallbacks call runPolling after its own publish —
	// this replicates that ordering without driving full Start).
	w.recheckMu.Lock()
	w.recheckCtx = ctx
	w.recheckEvents = events
	published := make(map[string]seenEntry)
	w.recheckSeen = published
	w.recheckMu.Unlock()
	defer func() {
		w.recheckMu.Lock()
		w.recheckCtx = nil
		w.recheckEvents = nil
		w.recheckSeen = nil
		w.recheckMu.Unlock()
	}()
	pollDone := make(chan error, 1)
	go func() { pollDone <- w.runPolling(ctx, events) }()

	// The hook must fire with the map the polling loop actually uses...
	select {
	case <-hookFired:
	case <-time.After(3 * time.Second):
		t.Fatal("the polling loop never reported its seen map (loopSeenObserved)")
	}
	mu.Lock()
	captured := loopSeen
	mu.Unlock()
	require.NotNil(t, captured)
	// ...and that map must BE the lifecycle map (the recheck's map). Under
	// the mutation (polling builds a local fresh map) the pointers differ.
	lifecycle := w.lifecycleSeen()
	require.NotNil(t, lifecycle, "the lifecycle map must be published for runPolling")
	require.True(t, reflect.ValueOf(lifecycle).Pointer() == reflect.ValueOf(published).Pointer(),
		"lifecycleSeen must return the published triple map")
	require.True(t, reflect.ValueOf(captured).Pointer() == reflect.ValueOf(lifecycle).Pointer(),
		"the polling loop's seen map must be THE SAME MAP as the lifecycle (recheck) map "+
			"(polling=%p lifecycle=%p — a local-map regression makes the recheck's deliveries "+
			"invisible to the polling path's suppression verdicts)", captured, lifecycle)
	fmt.Printf("r11_polling polling_map=%p lifecycle_map=%p same=%v\\n",
		captured, lifecycle, true)

	cancel()
	select {
	case <-pollDone:
	case <-time.After(2 * time.Second):
	}
}
