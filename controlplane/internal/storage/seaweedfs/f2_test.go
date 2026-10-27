package seaweedfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── F2: startup cursor load is retried; self-check is bounded and two-level ──

// A transient failure of the FIRST DeclaredCursor read must not permanently
// stop the event source (review F2): Run retries with cancellable backoff and
// eventually subscribes. Before the fix, Run returned the error immediately
// and the CP lived on with no storage event processing.
func TestRun_TransientCursorFailure_ThenSuccess_Subscribes(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{
		resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(5_000)},
	}}}
	store := &fakeStore{cursorFound: false, cursorErrs: []error{errors.New("transient DB disconnect")}}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(2 * time.Second); cancel() }()
	err := a.Run(ctx)
	require.ErrorIs(t, err, context.Canceled, "Run must keep retrying, not return on a transient cursor failure")
	require.GreaterOrEqual(t, len(filer.requests), 1, "after the store recovers, Run must reach SubscribeLocalMetadata")
	require.Len(t, store.saves, 1, "the stream's first heartbeat must persist a cursor once subscribed")
}

// The startup self-check must read the TWO-LEVEL layout (earliest day
// directory, then that day's earliest minute file) per the D-037 known
// constraint, so a cursor sitting between midnight and the earliest minute
// file is detected as out of range. The one-level version compared against
// midnight and missed it.
func TestRun_SelfCheck_TwoLevelListing_DetectsCursorBeforeEarliestMinuteFile(t *testing.T) {
	// Log layout: day dir 2026-10-01, whose earliest minute file is 00-05
	// (00:05 UTC). The cursor sits at 00:02 — after midnight, before the
	// earliest available log entry.
	dayStart := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	cursor := dayStart.Add(2 * time.Minute).UnixNano()
	filer := &scriptedFiler{
		lists: []listExpect{
			{dir: "/topics/.system/log", name: "2026-10-01"},
			{dir: "/topics/.system/log/2026-10-01", name: "00-05.0badc0de"},
		},
		streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(cursor + 5_000)}}},
	}
	store := &fakeStore{cursorFound: true, cursorDeclared: cursor}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(2 * time.Second); cancel() }()
	require.ErrorIs(t, a.Run(ctx), context.Canceled)

	found := false
	for _, m := range store.marks {
		if m.Reason == MarkReasonCursorBeforeEarliestLog {
			found = true
			require.NotNil(t, m.Detail)
			require.Contains(t, string(m.Detail), "00-05.0badc0de", "the mark must name the two-level earliest log entry")
		}
	}
	require.True(t, found, "cursor at 00:02 predates the earliest minute file 00-05 — the self-check must catch it")
}

// The self-check runs on its own deadline: a filer that accepts the dial but
// never answers ListEntries must not delay the subscription beyond the
// deadline (Warn + continue), and must not keep Run from starting.
func TestRun_SelfCheck_DeadlineBounded_ThenSubscribe(t *testing.T) {
	filer := &scriptedFiler{
		blockList: true,
		streams:   []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(5_000)}}},
	}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())
	a.selfCheckTimeout = 300 * time.Millisecond

	// The subscription must start DESPITE the hung self-check: poll for the
	// first SubscribeLocalMetadata and require it well within the (larger)
	// outer cancel horizon — and far below the 2s cancel point, i.e. the
	// self-check deadline (300ms) is what unblocked it, not the cancel.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	start := time.Now()
	runDone := make(chan error, 1)
	go func() { runDone <- a.Run(ctx) }()
	subscribed := make(chan struct{})
	go func() {
		for {
			filer.mu.Lock()
			n := len(filer.requests)
			filer.mu.Unlock()
			if n > 0 {
				close(subscribed)
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	select {
	case <-subscribed:
		require.Less(t, time.Since(start), 1500*time.Millisecond,
			"the subscription must start right after the self-check deadline (~300ms), not after the outer cancel")
	case <-time.After(1500 * time.Millisecond):
		close(subscribed) // unblock the watcher
		t.Fatal("the subscription never started while the self-check hung — the deadline is not bounding it")
	}
	cancel()
	require.ErrorIs(t, <-runDone, context.Canceled)
}
