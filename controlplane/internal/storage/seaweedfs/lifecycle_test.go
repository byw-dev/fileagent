package seaweedfs

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
	"go.uber.org/zap"
)

// ── lifecycle: cancellation while Recv / Apply are in flight ─────────────────
//
// The pre-existing TestRun_CtxCancel_CleanExitNoWrites cancels BEFORE Run does
// any work. These tests cancel while the loop is genuinely parked in Recv or
// in an Apply retry backoff and require Run to return promptly (the shutdown
// path in cmd/server joins this goroutine).

// blockingStream parks in Recv until its context is done.
type blockingStream struct {
	ctx   context.Context
	found bool
	mu    sync.Mutex
}

func (s *blockingStream) Recv() (*filerpb.SubscribeMetadataResponse, error) {
	<-s.ctx.Done()
	s.mu.Lock()
	defer s.mu.Unlock()
	s.found = true
	return nil, s.ctx.Err()
}

// blockingFiler hands out one blocking stream per subscribe.
type blockingFiler struct {
	ctx      context.Context
	mu       sync.Mutex
	requests int
}

func (f *blockingFiler) SubscribeLocalMetadata(ctx context.Context, in *filerpb.SubscribeMetadataRequest) (FilerStream, error) {
	f.mu.Lock()
	f.requests++
	f.mu.Unlock()
	return &blockingStream{ctx: ctx}, nil
}

func (f *blockingFiler) ListEntries(context.Context, *filerpb.ListEntriesRequest) (ListEntriesStream, error) {
	return &scriptedListStream{}, nil
}

// cancelInFlightAdapter builds an adapter over the blocking filer.
func cancelInFlightAdapter(t *testing.T, store *fakeStore) (*Adapter, *blockingFiler) {
	t.Helper()
	cfg := AdapterConfig{SourceID: "seaweedfs:cancel", ClientName: "fileagent-test", ClientID: 9, ResumeMargin: 100}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	filer := &blockingFiler{}
	a := &Adapter{
		cfg:              cfg,
		dial: func(context.Context) (FilerClient, func(), error) {
			return filer, func() {}, nil
		},
		reader:           &fakeReader{outcome: map[string]any{}},
		store:            store,
		clock:            &fakeClock{},
		sink:             &fakeSink{},
		logger:           zap.NewNop(),
		reconnectDelay:   time.Millisecond,
		selfCheckTimeout: 50 * time.Millisecond,
	}
	return a, filer
}

// Cancellation while Run is parked in stream.Recv must end Run promptly with
// context.Canceled and without any cursor write (review "lifecycle").
func TestRun_CancelWhileRecvInFlight_ReturnsPromptly(t *testing.T) {
	store := &fakeStore{cursorFound: false}
	a, filer := cancelInFlightAdapter(t, store)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.Run(ctx) }()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		filer.mu.Lock()
		n := filer.requests
		filer.mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Run = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Run did not return within 2s of cancellation while parked in Recv")
	}
	if len(store.saves) != 0 {
		t.Errorf("cursor saves = %v, want none when cancelled mid-Recv", store.saves)
	}
}

// Cancellation during an Apply retry backoff must abort the retry loop and
// return promptly (the in-flight event is replayed on the next subscribe —
// never skipped).
func TestRun_CancelDuringApplyBackoff_AbortsPromptly(t *testing.T) {
	event := respCreated("/buckets/data-sensor", "backoff.csv", 3, 1_100)
	filer := &scriptedFiler{streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{event}}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	sink := &fakeSink{errs: []error{errInjected, errInjected, errInjected, errInjected, errInjected, errInjected, errInjected, errInjected, errInjected}}
	cfg := loopConfig()
	cfg.ApplyMaxRetries = 50
	cfg.ApplyRetryBackoff = 10 * time.Second // the backoff itself would outlive the test
	reader := &fakeReader{outcome: map[string]any{"data-sensor/backoff.csv": ObjectInfo{Size: 3, ETag: "e"}}}
	a := newLoopAdapter(t, cfg, filer, reader, store, sink, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- a.runStream(ctx, new(int64)) }()
	time.Sleep(50 * time.Millisecond) // let it fail Apply #1 and enter the backoff
	cancel()

	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("runStream = %v, want context.Canceled from the backoff sleep", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runStream did not return within 2s of cancellation during the Apply backoff")
	}
}
