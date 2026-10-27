package seaweedfs

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
	"go.uber.org/zap"
)

// ── AdapterConfig.validate (D6 guard rails live here for the loop) ───────────

func TestAdapterConfigValidate(t *testing.T) {
	valid := AdapterConfig{SourceID: "s", ClientName: "c"}
	if err := valid.validate(); err != nil {
		t.Fatalf("valid config rejected: %v", err)
	}
	if valid.ResumeMargin != DefaultResumeMargin || valid.ApplyMaxRetries != DefaultApplyMaxRetries ||
		valid.ApplyRetryBackoff != DefaultApplyRetryBackoff || valid.CursorFlushInterval != DefaultCursorFlushInterval {
		t.Errorf("defaults not applied: %+v", valid)
	}
	for name, mutate := range map[string]func(*AdapterConfig){
		"missing source id": func(c *AdapterConfig) { c.SourceID = "" },
		"missing client":    func(c *AdapterConfig) { c.ClientName = "" },
		"negative margin":   func(c *AdapterConfig) { c.ResumeMargin = -1 },
		"negative retries":  func(c *AdapterConfig) { c.ApplyMaxRetries = -1 },
		"negative backoff":  func(c *AdapterConfig) { c.ApplyRetryBackoff = -time.Second },
		"negative flush":    func(c *AdapterConfig) { c.CursorFlushInterval = -time.Second },
	} {
		cfg := valid
		mutate(&cfg)
		if err := cfg.validate(); err == nil {
			t.Errorf("%s: validate = nil, want an error", name)
		}
	}
}

// ── NewAdapter wiring validation ─────────────────────────────────────────────

func TestNewAdapter_RejectsMissingDependencies(t *testing.T) {
	cfg := AdapterConfig{SourceID: "s", ClientName: "c"}
	okDial := func(context.Context) (FilerClient, func(), error) { return nil, nil, errors.New("unused") }
	okReader := &fakeReader{outcome: map[string]any{}}
	okStore := &fakeStore{}
	okClock := &fakeClock{}
	sink := &fakeSink{}

	if _, err := NewAdapter(cfg, nil, okReader, okStore, okClock, sink, zap.NewNop()); err == nil {
		t.Error("nil dial accepted")
	}
	if _, err := NewAdapter(cfg, okDial, nil, okStore, okClock, sink, zap.NewNop()); err == nil {
		t.Error("nil reader accepted")
	}
	if _, err := NewAdapter(cfg, okDial, okReader, nil, okClock, sink, zap.NewNop()); err == nil {
		t.Error("nil store accepted")
	}
	if _, err := NewAdapter(cfg, okDial, okReader, okStore, nil, sink, zap.NewNop()); err == nil {
		t.Error("nil clock accepted")
	}
	if _, err := NewAdapter(cfg, okDial, okReader, okStore, okClock, nil, zap.NewNop()); err == nil {
		t.Error("nil sink accepted")
	}
	if _, err := NewAdapter(AdapterConfig{ClientName: "c"}, okDial, okReader, okStore, okClock, sink, nil); err == nil {
		t.Error("invalid config accepted")
	}
}

// ── Run: dial failure resubscribes until ctx is done ─────────────────────────

// A dial failure is a stream failure like any other: Run keeps trying (never
// gives up, never skips) until ctx cancellation ends it cleanly.
func TestRun_DialFailure_KeepsRetryingUntilCancel(t *testing.T) {
	cfg := AdapterConfig{SourceID: "s", ClientName: "c", ResumeMargin: 100, ApplyMaxRetries: 1, ApplyRetryBackoff: time.Millisecond, CursorFlushInterval: 0}
	dials := 0
	a := &Adapter{
		cfg: cfg,
		dial: func(context.Context) (FilerClient, func(), error) {
			dials++
			return nil, nil, errors.New("proxy down")
		},
		reader:         &fakeReader{outcome: map[string]any{}},
		store:          &fakeStore{cursorFound: true, cursorDeclared: 1_000},
		clock:          &fakeClock{},
		sink:           &fakeSink{},
		logger:         zap.NewNop(),
		reconnectDelay: time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := a.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if dials < 2 {
		t.Errorf("dials = %d, want repeated attempts (never give up on a broken path)", dials)
	}
}

// ── error paths of the mark writers ──────────────────────────────────────────

// A gap-mark write failure is a stream failure (the mark feeds L2; silently
// losing it would hide a possible loss window).
func TestRunStream_GapMarkFailure_EndsStream(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(2_000)}}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000, markErr: errMarkInjected}
	cfg := loopConfig()
	a := newLoopAdapter(t, cfg, filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())
	last := store.cursorDeclared
	err := a.runStream(context.Background(), &last)
	if !errors.Is(err, errMarkInjected) {
		t.Fatalf("runStream = %v, want the mark failure propagated", err)
	}
	if len(store.saves) != 0 {
		t.Errorf("no cursor save expected after a failed gap mark (saves=%v)", store.saves)
	}
}

// errMarkInjected is the shared injected mark-write failure.
var errMarkInjected = errors.New("pg down")

// A bucket-removed mark write failure is a stream failure: the incident must
// not be forgotten, the following heartbeat must not persist a cursor, and the
// later event must not be applied (review F0 — the previous version asserted
// only a non-nil error and stayed green when the failure was mutated away).
func TestRunStream_BucketRemovedMarkFailure_EndsStream(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{
		respBucketRemoved("doomed", 1_200),
		respHeartbeat(2_000),
		respCreated("/buckets/data-sensor", "after.csv", 3, 2_100),
	}}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000, markErrs: []error{errMarkInjected}}
	sink := &fakeSink{}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, sink, zap.NewNop())
	last := store.cursorDeclared
	err := a.runStream(context.Background(), &last)
	if !errors.Is(err, errMarkInjected) {
		t.Fatalf("runStream = %v, want the INJECTED mark error (EOF/exhaustion would mean the loop ran past the failure)", err)
	}
	if len(store.saves) != 0 {
		t.Errorf("cursor saves = %v, want none — the heartbeat after the failed mark must not be persisted", store.saves)
	}
	if sink.appliedCount() != 0 {
		t.Errorf("applied events = %d, want 0 — the event after the failed mark must never be applied", sink.appliedCount())
	}
	if last != 1_000 {
		t.Errorf("lastDeclared = %d, want unchanged 1000", last)
	}
}

// CountActiveEntries failure: the incident mark cannot be filled → stream
// failure (the adapter never writes a half-truth mark when it can avoid it).
func TestRunStream_BucketRemovedCountFailure_EndsStream(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{
		respBucketRemoved("doomed", 1_200),
		respHeartbeat(2_000),
		respCreated("/buckets/data-sensor", "after.csv", 3, 2_100),
	}}}}
	store := &failingCountStore{fakeStore: fakeStore{cursorFound: true, cursorDeclared: 1_000}}
	sink := &fakeSink{}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, &store.fakeStore, sink, zap.NewNop())
	// Swap the store behind the adapter: CountActiveEntries fails, marks fine.
	a.store = store
	cursor := int64(1_000)
	err := a.runStream(context.Background(), &cursor)
	if !errors.Is(err, errCountInjected) {
		t.Fatalf("runStream = %v, want the INJECTED count error", err)
	}
	if len(store.fakeStore.saves) != 0 {
		t.Errorf("cursor saves = %v, want none — the heartbeat after the failed count must not be persisted", store.fakeStore.saves)
	}
	if sink.appliedCount() != 0 {
		t.Errorf("applied events = %d, want 0 — the event after the failed count must never be applied", sink.appliedCount())
	}
	if cursor != 1_000 {
		t.Errorf("cursor = %d, want unchanged 1000", cursor)
	}
}

// errCountInjected is the shared injected count failure.
var errCountInjected = errors.New("count pg down")

// failingCountStore fails only CountActiveEntries.
type failingCountStore struct{ fakeStore }

func (s *failingCountStore) CountActiveEntries(context.Context, string) (int64, error) {
	return 0, errCountInjected
}

// ── selfCheck branch coverage ────────────────────────────────────────────────

// An unparsable earliest log name is a Warn-and-continue (no mark).
func TestRun_SelfCheck_UnparsableLogName_NoMark(t *testing.T) {
	filer := &scriptedFiler{listName: "not-a-date", streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(5_000)}}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixNano()}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := a.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	for _, m := range store.marks {
		if m.Reason == MarkReasonCursorBeforeEarliestLog {
			t.Errorf("unexpected out-of-range mark for an unparsable name: %+v", m)
		}
	}
}

// An empty log directory (fresh filer) skips the check without a mark.
func TestRun_SelfCheck_EmptyLogDir_NoMark(t *testing.T) {
	filer := &scriptedFiler{listName: "", streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(5_000)}}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := a.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	for _, m := range store.marks {
		if m.Reason == MarkReasonCursorBeforeEarliestLog {
			t.Errorf("unexpected out-of-range mark for an empty log dir: %+v", m)
		}
	}
}

// A failed out-of-range mark write still lets startup continue (the Error log
// is the remaining signal).
func TestRun_SelfCheck_MarkWriteFailure_StillContinues(t *testing.T) {
	cursor := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	filer := &scriptedFiler{listName: "2026-10-01", streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(cursor + 5_000)}}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: cursor, markErr: errMarkInjected}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := a.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled (self-check must not block startup)", err)
	}
	if len(filer.requests) == 0 {
		t.Error("the subscription must start even when the self-check mark cannot be written")
	}
}

// ── misc ─────────────────────────────────────────────────────────────────────

// Retry backoff honors ctx cancellation (a wedged DB must not pin shutdown).
func TestProcessChange_BackoffHonorsCtx(t *testing.T) {
	cfg := loopConfig()
	cfg.ApplyMaxRetries = 3
	cfg.ApplyRetryBackoff = time.Hour
	filer := &scriptedFiler{streams: []*scriptedStream{{}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	a := newLoopAdapter(t, cfg, filer, &fakeReader{outcome: map[string]any{
		"b/k": errors.New("head fails forever"),
	}}, store, &fakeSink{}, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	err := a.processChange(ctx, Change{Kind: ChangeCreated, Bucket: "b", Key: "k"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("processChange = %v, want context.Canceled from the backoff sleep", err)
	}
}

// The flushed-report response and an empty batch envelope produce no changes
// and no cursor movement (already pinned elsewhere; here for Translate
// integration through the loop).
func TestRunStream_BatchEnvelope_ProcessedInOrder(t *testing.T) {
	// A batch envelope: first response is the carrier, members ride in Events.
	// Two DIFFERENT keys so the two facts are distinguishable.
	envelope := &filerpb.SubscribeMetadataResponse{
		Directory: "/buckets/data-sensor",
		TsNs:      1_100,
		EventNotification: &filerpb.EventNotification{
			NewEntry: &filerpb.Entry{Name: "batch.csv", Attributes: &filerpb.FuseAttributes{FileSize: 5}},
		},
		Events: []*filerpb.SubscribeMetadataResponse{
			{
				Directory: "/buckets/data-sensor",
				TsNs:      1_150,
				EventNotification: &filerpb.EventNotification{
					OldEntry: &filerpb.Entry{Name: "sibling.csv"},
				},
			},
		},
	}
	filer := &scriptedFiler{streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{
		envelope,
		respHeartbeat(2_000),
	}}}}
	reader := &fakeReader{outcome: map[string]any{
		"data-sensor/batch.csv":   ObjectInfo{Size: 5, ETag: "e"},
		"data-sensor/sibling.csv": ErrObjectAbsent,
	}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	sink := &fakeSink{}
	a := newLoopAdapter(t, loopConfig(), filer, reader, store, sink, zap.NewNop())
	last := store.cursorDeclared
	if err := a.runStream(context.Background(), &last); err == nil {
		t.Fatal("want scripted exhaustion")
	}
	if len(sink.events) != 2 {
		t.Fatalf("applied events = %d, want 2 (envelope + member, in order)", len(sink.events))
	}
	if sink.events[0].Kind.String() != "object_created" || sink.events[1].Kind.String() != "object_removed" {
		t.Errorf("kinds = %s, %s; want created then removed (strict envelope→member order)",
			sink.events[0].Kind, sink.events[1].Kind)
	}
}
