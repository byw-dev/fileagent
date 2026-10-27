package seaweedfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
	"go.uber.org/zap"
)

// ── F1a: a suppressed read-back fact must be re-derived, never skipped ──────
//
// Race (review F1): the adapter takes the DB clock T1, a delayed agent
// UploadResult lands with now()=T2>T1 (its upsert wins the observed_at
// eviction), then the HEAD says 404 and the delete carrying T1 is suppressed.
// If the adapter treated that like a plain success it would advance past the
// key's LAST event and the deletion would be lost forever. The suppression
// must instead trigger a full re-derive: fresh DB clock (T3>T2), fresh HEAD,
// re-Apply — bounded by the existing retry budget; past the cap the stream is
// abandoned so the change is replayed (never skipped).

// scriptedSink returns scripted errors per Apply; nil entries succeed.
type scriptedSink struct {
	outcomes []error // one per Apply; exhausted = success
	events   []storageevent.Event
}

func (s *scriptedSink) Apply(_ context.Context, ev storageevent.Event) error {
	s.events = append(s.events, ev)
	if len(s.outcomes) > 0 {
		err := s.outcomes[0]
		s.outcomes = s.outcomes[1:]
		return err
	}
	return nil
}

func (s *scriptedSink) appliedCount() int { return len(s.events) }

func (a *Adapter) sinkAppliedCount(s *scriptedSink) int { return s.appliedCount() }

// newSuppressionAdapter wires an adapter whose reader/clock/sink are all
// scripted, for suppression-retry tests.
func newSuppressionAdapter(t *testing.T, reader *fakeReader, clock *fakeClock, sink storageevent.Sink, retries int) *Adapter {
	t.Helper()
	cfg := AdapterConfig{
		SourceID: "seaweedfs:supp", ClientName: "fileagent-test", ClientID: 3,
		ResumeMargin: 100, ApplyMaxRetries: retries, ApplyRetryBackoff: time.Millisecond,
		CursorFlushInterval: 0,
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	return &Adapter{cfg: cfg, reader: reader, clock: clock, sink: sink, logger: zap.NewNop(), reconnectDelay: time.Millisecond}
}

// First Apply suppressed → the adapter retries with a FRESH DB clock reading
// taken BEFORE a FRESH HEAD, and the second Apply carries the later timestamp.
// The stream then continues (the heartbeat persists the cursor).
func TestProcessChange_SuppressedApply_RetriesWithFreshClockAndHead(t *testing.T) {
	seq := 0
	reader := &fakeReader{
		outcome: map[string]any{"b/k": ObjectInfo{Size: 223, ETag: "new-etag"}},
		seq:     &seq,
	}
	clock := &fakeClock{seq: &seq}
	// Attempt 1: HEAD 200 fact is evicted (delayed agent write won). Attempt 2: accepted.
	sink := &scriptedSink{outcomes: []error{storageevent.ErrSuppressed}}
	a := newSuppressionAdapter(t, reader, clock, sink, 3)

	err := a.processChange(context.Background(), Change{Kind: ChangeCreated, Bucket: "b", Key: "k"})
	if err != nil {
		t.Fatalf("processChange = %v, want nil (the retry must succeed)", err)
	}
	if a.sinkAppliedCount(sink) != 2 {
		t.Fatalf("Apply attempts = %d, want 2 (first suppressed, then re-derived)", a.sinkAppliedCount(sink))
	}
	if clock.calls[0] != "clock@1" || reader.calls[0] != "head@2" || clock.calls[1] != "clock@3" || reader.calls[1] != "head@4" {
		t.Errorf("interleaving = %v / %v, want clock→head per attempt (fresh clock BEFORE each HEAD)", clock.calls, reader.calls)
	}
	if !sink.events[1].ObservedAt.After(sink.events[0].ObservedAt) {
		t.Errorf("second apply ObservedAt %v must be later than the suppressed attempt %v", sink.events[1].ObservedAt, sink.events[0].ObservedAt)
	}
	_ = filerpb.SubscribeMetadataRequest{} // keep the filerpb import for stream helpers in this file's siblings
}

// Suppressed on every attempt → the retry cap turns it into a processing
// failure (the stream is abandoned and the change replayed — never skipped).
func TestProcessChange_SuppressedAlways_FailsAfterCap(t *testing.T) {
	reader := &fakeReader{outcome: map[string]any{"b/k": ObjectInfo{Size: 1, ETag: "e"}}}
	clock := &fakeClock{}
	sink := &scriptedSink{outcomes: []error{
		storageevent.ErrSuppressed, storageevent.ErrSuppressed, storageevent.ErrSuppressed,
	}}
	a := newSuppressionAdapter(t, reader, clock, sink, 2)

	err := a.processChange(context.Background(), Change{Kind: ChangeCreated, Bucket: "b", Key: "k"})
	if !errors.Is(err, storageevent.ErrSuppressed) {
		t.Fatalf("processChange = %v, want the suppression sentinel after the cap", err)
	}
	if got := a.sinkAppliedCount(sink); got != 3 {
		t.Fatalf("Apply attempts = %d, want 3 (1 + ApplyMaxRetries 2)", got)
	}
	if got := clock.readingCount(); got != 3 {
		t.Errorf("clock readings = %d, want 3 (fresh clock before every HEAD)", got)
	}
}

// A suppression on a deletion fact behaves the same way.
func TestProcessChange_SuppressedDeletion_RetriesAndWins(t *testing.T) {
	seq := 0
	reader := &fakeReader{outcome: map[string]any{"b/gone": ErrObjectAbsent}, seq: &seq}
	clock := &fakeClock{seq: &seq}
	sink := &scriptedSink{outcomes: []error{storageevent.ErrSuppressed}}
	a := newSuppressionAdapter(t, reader, clock, sink, 3)

	if err := a.processChange(context.Background(), Change{Kind: ChangeRemoved, Bucket: "b", Key: "gone"}); err != nil {
		t.Fatalf("processChange = %v, want nil", err)
	}
	if a.sinkAppliedCount(sink) != 2 {
		t.Fatalf("Apply attempts = %d, want 2", a.sinkAppliedCount(sink))
	}
	if clock.calls[1] != "clock@3" {
		t.Errorf("retry must take a fresh clock reading before the fresh HEAD, got %v", clock.calls)
	}
}
