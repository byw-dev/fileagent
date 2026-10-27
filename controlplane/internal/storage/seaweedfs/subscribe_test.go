package seaweedfs

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/indexer"
	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// ── fakes for the subscription loop ──────────────────────────────────────────

// scriptedStream replays a fixed response list, then fails with a terminal
// error (a broken stream — the adapter must resubscribe, never skip).
type scriptedStream struct {
	resps []*filerpb.SubscribeMetadataResponse
	i     int
}

func (s *scriptedStream) Recv() (*filerpb.SubscribeMetadataResponse, error) {
	if s.i < len(s.resps) {
		r := s.resps[s.i]
		s.i++
		return r, nil
	}
	return nil, errors.New("scripted stream exhausted")
}

// listExpect is one scripted ListEntries answer: the directory asked and the
// entry name to return (F2 two-level self-check serves two of these).
type listExpect struct {
	dir  string
	name string
}

// scriptedFiler serves scripted subscribe streams and optional ListEntries
// answers for the startup self-check (one per directory, consumed in order;
// the legacy single-answer fields remain for older tests).
type scriptedFiler struct {
	mu        sync.Mutex
	streams   []*scriptedStream // consumed in subscribe order
	streamIdx int
	requests  []*filerpb.SubscribeMetadataRequest
	lists     []listExpect // consumed in ListEntries order
	blockList bool         // ListEntries never answers (deadline test)
	listName  string       // earliest log entry name for the self-check
	listErr   error
}

func (f *scriptedFiler) SubscribeLocalMetadata(_ context.Context, in *filerpb.SubscribeMetadataRequest) (FilerStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, in)
	if f.streamIdx >= len(f.streams) {
		return nil, errors.New("scriptedFiler: no scripted stream left")
	}
	s := f.streams[f.streamIdx]
	f.streamIdx++
	return s, nil
}

func (f *scriptedFiler) ListEntries(ctx context.Context, in *filerpb.ListEntriesRequest) (ListEntriesStream, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.blockList {
		// Never answers: the caller's deadline (not the stream) must end it.
		<-ctx.Done() //nolint:staticcheck // intentionally blocking
		return nil, ctx.Err()
	}
	name := f.listName
	if len(f.lists) > 0 {
		exp := f.lists[0]
		f.lists = f.lists[1:]
		if in.GetDirectory() != exp.dir {
			return nil, errors.New("scriptedFiler: unexpected ListEntries dir " + in.GetDirectory() + " (want " + exp.dir + ")")
		}
		name = exp.name
	}
	var entry *filerpb.Entry
	if name != "" {
		entry = &filerpb.Entry{Name: name}
	}
	resps := []*filerpb.ListEntriesResponse{}
	if entry != nil {
		resps = append(resps, &filerpb.ListEntriesResponse{Entry: entry})
	}
	return &scriptedListStream{resps: resps}, nil
}

type scriptedListStream struct {
	resps []*filerpb.ListEntriesResponse
	i     int
}

func (s *scriptedListStream) Recv() (*filerpb.ListEntriesResponse, error) {
	if s.i < len(s.resps) {
		r := s.resps[s.i]
		s.i++
		return r, nil
	}
	return nil, errors.New("scripted list stream exhausted")
}

// fakeStore is a CursorMarkStore over plain fields.
type fakeStore struct {
	cursorDeclared int64
	cursorFound    bool
	cursorErrs     []error // consumed per DeclaredCursor call; exhausted = ok
	saves          []int64
	marks          []Mark
	activeCount    int64
	saveErr        error
	markErr        error
}

func (s *fakeStore) DeclaredCursor(_ context.Context, _ string) (int64, bool, error) {
	if len(s.cursorErrs) > 0 {
		err := s.cursorErrs[0]
		s.cursorErrs = s.cursorErrs[1:]
		return 0, false, err
	}
	return s.cursorDeclared, s.cursorFound, nil
}

func (s *fakeStore) SaveCursor(_ context.Context, _ string, declared int64) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	s.saves = append(s.saves, declared)
	s.cursorDeclared = declared
	s.cursorFound = true
	return nil
}

func (s *fakeStore) RecordMark(_ context.Context, mark Mark) error {
	if s.markErr != nil {
		return s.markErr
	}
	s.marks = append(s.marks, mark)
	return nil
}

func (s *fakeStore) CountActiveEntries(_ context.Context, _ string) (int64, error) {
	return s.activeCount, nil
}

// fakeSink applies scripted errors in order and records the events.
type fakeSink struct {
	errs   []error // one entry per Apply; nil = success; exhausted = success
	events []storageevent.Event
}

func (f *fakeSink) Apply(_ context.Context, ev storageevent.Event) error {
	f.events = append(f.events, ev)
	if len(f.errs) > 0 {
		err := f.errs[0]
		f.errs = f.errs[1:]
		return err
	}
	return nil
}

func (f *fakeSink) applyCount() int { return len(f.events) }

// newLoopAdapter wires a fully-fake adapter for loop tests. reader is the
// scripted S3 read-back the loop's HEAD calls hit.
func newLoopAdapter(t *testing.T, cfg AdapterConfig, filer *scriptedFiler, reader *fakeReader, store *fakeStore, sink *fakeSink, logger *zap.Logger) *Adapter {
	t.Helper()
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate: %v", err)
	}
	dial := func(ctx context.Context) (FilerClient, func(), error) {
		return filer, func() {}, nil
	}
	a, err := NewAdapter(cfg, dial, reader, store, &fakeClock{}, sink, logger)
	if err != nil {
		t.Fatalf("NewAdapter: %v", err)
	}
	a.reconnectDelay = time.Millisecond
	return a
}

// loopConfig is the default loop-test config (tiny values for fast tests).
func loopConfig() AdapterConfig {
	return AdapterConfig{
		SourceID:            "seaweedfs:test",
		ClientName:          "fileagent-test",
		ClientID:            7,
		ResumeMargin:        100, // 100ns, easy arithmetic in assertions
		ApplyMaxRetries:     2,
		ApplyRetryBackoff:   time.Millisecond,
		CursorFlushInterval: 0, // disabled: every heartbeat persists
	}
}

// resp helpers build filerpb responses that Translate maps deterministically.
func respCreated(dir, name string, size uint64, ts int64) *filerpb.SubscribeMetadataResponse {
	return &filerpb.SubscribeMetadataResponse{
		Directory: dir,
		TsNs:      ts,
		EventNotification: &filerpb.EventNotification{
			NewEntry: &filerpb.Entry{
				Name:       name,
				Attributes: &filerpb.FuseAttributes{FileSize: size},
			},
		},
	}
}

func respHeartbeat(ts int64) *filerpb.SubscribeMetadataResponse {
	return &filerpb.SubscribeMetadataResponse{TsNs: ts}
}

func respFlushReport(ts int64) *filerpb.SubscribeMetadataResponse {
	return &filerpb.SubscribeMetadataResponse{FlushedTsNs: ts}
}

func respBucketRemoved(bucket string, ts int64) *filerpb.SubscribeMetadataResponse {
	return &filerpb.SubscribeMetadataResponse{
		Directory: "/buckets",
		TsNs:      ts,
		EventNotification: &filerpb.EventNotification{
			OldEntry: &filerpb.Entry{Name: bucket, IsDirectory: true},
		},
	}
}

// ── subscription request shape (D5) ──────────────────────────────────────────

// The SubscribeLocalMetadata request must carry the exact D5 contract:
// /buckets/ prefix, idle heartbeat + batching on, metadata chunks OFF
// (true would make the CLIENT read log chunks from volume servers), stable
// client identity, and since_ns = ResumeFrom(cursor, M).
func TestRunStream_RequestShape(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(5_000)}}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())
	last := store.cursorDeclared

	if err := a.runStream(context.Background(), &last); err == nil {
		t.Fatal("runStream = nil, want the scripted exhaustion error")
	}
	req := filer.requests[0]
	if req.GetPathPrefix() != "/buckets/" {
		t.Errorf("PathPrefix = %q, want /buckets/", req.GetPathPrefix())
	}
	if !req.GetClientSupportsIdleHeartbeat() {
		t.Error("ClientSupportsIdleHeartbeat must be true (the declared-value cursor depends on it)")
	}
	if !req.GetClientSupportsBatching() {
		t.Error("ClientSupportsBatching must be true")
	}
	if req.GetClientSupportsMetadataChunks() {
		t.Error("ClientSupportsMetadataChunks must be FALSE (true would push log-chunk reading onto the client)")
	}
	if req.GetClientName() != "fileagent-test" || req.GetClientId() != 7 {
		t.Errorf("client identity = %q/%d, want fileagent-test/7", req.GetClientName(), req.GetClientId())
	}
	// since = ResumeFrom(1000, 100ns) = 900
	if want := int64(900); req.GetSinceNs() != want {
		t.Errorf("SinceNs = %d, want %d (declared %d − margin %d)", req.GetSinceNs(), want, 1_000, 100)
	}
}

// A fresh deployment has no cursor row: subscribe from 0 (full replay — the
// logs are kept forever and read-back makes replay idempotent) and say so in
// an Info log.
func TestRun_NoCursor_SubscribesFromZeroWithInfo(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(5_000)}}}}
	store := &fakeStore{cursorFound: false}
	core, logs := observer.New(zap.InfoLevel)
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.New(core))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	err := a.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(filer.requests) == 0 || filer.requests[0].GetSinceNs() != 0 {
		t.Fatalf("first SinceNs = %v, want 0 (no cursor → full replay)", filer.requests[0].GetSinceNs())
	}
	if logs.FilterLevelExact(zap.InfoLevel).Len() == 0 {
		t.Error("a cursor-less start must log Info (full replay from 0)")
	}
}

// A persisted cursor triggers the startup out-of-range self-check: the
// earliest log is listed via the filer and a cursor older than it produces an
// Error log AND a cursor_before_earliest_log mark, while startup continues
// (the subscription is not blocked).
func TestRun_SelfCheck_CursorBeforeEarliestLog_MarksAndContinues(t *testing.T) {
	// cursor older than the earliest log name (2026-10-01 UTC midnight) →
	// CursorBeforeEarliestLog = true: the range the cursor points into was
	// purged.
	cursor := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC).UnixNano()
	filer := &scriptedFiler{
		lists: []listExpect{
			{dir: "/topics/.system/log", name: "2026-10-01"},
			{dir: "/topics/.system/log/2026-10-01", name: "00-00.0badc0de"},
		},
		streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(cursor + 5_000)}}},
	}
	store := &fakeStore{cursorFound: true, cursorDeclared: cursor}
	core, logs := observer.New(zap.ErrorLevel)
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.New(core))

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := a.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(filer.requests) == 0 {
		t.Fatal("the self-check must NOT block the subscription (no subscribe happened)")
	}
	found := false
	for _, m := range store.marks {
		if m.Reason == MarkReasonCursorBeforeEarliestLog {
			found = true
		}
	}
	if !found {
		t.Errorf("want a cursor_before_earliest_log mark, got %+v", store.marks)
	}
	if logs.FilterLevelExact(zap.ErrorLevel).Len() == 0 {
		t.Error("an out-of-range cursor must log at Error level")
	}
}

// A cursor still inside the earliest log must NOT produce a mark.
func TestRun_SelfCheck_CursorInRange_NoMark(t *testing.T) {
	filer := &scriptedFiler{listName: "2026-10-20/00-00.0badc0de", streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(5_000)}}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: time.Date(2026, 10, 20, 0, 0, 30, 0, time.UTC).UnixNano()}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := a.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	for _, m := range store.marks {
		if m.Reason == MarkReasonCursorBeforeEarliestLog {
			t.Errorf("unexpected out-of-range mark: %+v", m)
		}
	}
}

// A filer ListEntries failure during the self-check is a Warn, not a startup
// blocker: the subscription must still start.
func TestRun_SelfCheck_ListFailure_WarnsAndContinues(t *testing.T) {
	filer := &scriptedFiler{listErr: errors.New("filer busy"), streams: []*scriptedStream{{resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(5_000)}}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(20 * time.Millisecond); cancel() }()
	if err := a.Run(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(filer.requests) == 0 {
		t.Fatal("a failed self-check must not block the subscription")
	}
	for _, m := range store.marks {
		if m.Reason == MarkReasonCursorBeforeEarliestLog {
			t.Errorf("no out-of-range mark expected when the self-check could not list, got %+v", m)
		}
	}
}

// ── cursor semantics (D5) ────────────────────────────────────────────────────

// An idle heartbeat persists its declared value — but ONLY because every
// event before it was applied successfully (strict stream order guarantees
// it). The flush report (flushed_ts_ns only) must NOT advance the cursor.
func TestRunStream_HeartbeatPersistsCursor_AfterEventsApplied(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{
		resps: []*filerpb.SubscribeMetadataResponse{
			respCreated("/buckets/data-sensor", "a.csv", 12, 1_100),
			respFlushReport(555),
			respHeartbeat(2_000),
		},
	}}}
	reader := &fakeReader{outcome: map[string]any{"data-sensor/a.csv": ObjectInfo{Size: 12, ETag: "e"}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	a := newLoopAdapter(t, loopConfig(), filer, reader, store, &fakeSink{}, zap.NewNop())
	last := store.cursorDeclared

	if err := a.runStream(context.Background(), &last); err == nil {
		t.Fatal("want scripted exhaustion error")
	}
	if len(store.saves) != 1 || store.saves[0] != 2_000 {
		t.Errorf("cursor saves = %v, want exactly [2000] (heartbeat persists; flush report does not)", store.saves)
	}
	if last != 2_000 {
		t.Errorf("lastDeclared = %d, want 2000", last)
	}
}

// A change that keeps failing must not advance the cursor: in-place retries
// (1+ApplyMaxRetries attempts), then the stream is abandoned — and the NEXT
// subscription resumes from ResumeFrom(persisted cursor, M), NOT from the
// declared value (mutation ⑤) and NOT past the failing change (mutation ③).
func TestRunStream_ApplyFailure_RetriesThenResubscribeFromRewoundCursor(t *testing.T) {
	event := respCreated("/buckets/data-sensor", "stuck.csv", 3, 1_100)
	filer := &scriptedFiler{streams: []*scriptedStream{
		{resps: []*filerpb.SubscribeMetadataResponse{event}},
		{resps: []*filerpb.SubscribeMetadataResponse{event}},
	}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	sink := &fakeSink{errs: []error{errors.New("db down"), errors.New("db down"), errors.New("db down")}}
	reader := &fakeReader{outcome: map[string]any{"data-sensor/stuck.csv": ObjectInfo{Size: 3, ETag: "e"}}}
	cfg := loopConfig()
	cfg.ApplyMaxRetries = 2 // 1 attempt + 2 retries per stream
	a := newLoopAdapter(t, cfg, filer, reader, store, sink, zap.NewNop())
	last := store.cursorDeclared

	if err := a.runStream(context.Background(), &last); err == nil {
		t.Fatal("runStream = nil, want the apply-cap failure")
	}
	if got := sink.applyCount(); got != 3 {
		t.Errorf("Apply attempts = %d, want 3 (1 + ApplyMaxRetries 2)", got)
	}
	if len(store.saves) != 0 {
		t.Errorf("cursor saves = %v, want none — a failing change must not advance the cursor (mutation ③)", store.saves)
	}
	if last != 1_000 {
		t.Errorf("lastDeclared = %d, want unchanged 1000", last)
	}

	// The next subscription rewinds by M from the PERSISTED cursor.
	if err := a.runStream(context.Background(), &last); err == nil {
		t.Fatal("second runStream = nil, want scripted exhaustion")
	}
	if len(filer.requests) != 2 {
		t.Fatalf("subscribe calls = %d, want 2", len(filer.requests))
	}
	if want := ResumeFrom(1_000, cfg.ResumeMargin); filer.requests[1].GetSinceNs() != want {
		t.Errorf("resubscribe SinceNs = %d, want %d = ResumeFrom(persisted %d, M) — mutation ⑤ rewinds less and must go red here",
			filer.requests[1].GetSinceNs(), want, 1_000)
	}
}

// ── unknown buckets (D5) ─────────────────────────────────────────────────────

// A bucket the ledger does not know is NOT a failure: Warn + count, treated as
// processed (no retry, stream keeps moving) — otherwise one strange bucket
// would wedge the whole stream forever (mutation ④).
func TestRunStream_UnknownBucket_TreatedAsProcessed(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{
		resps: []*filerpb.SubscribeMetadataResponse{
			respCreated("/buckets/ghost-bucket", "x.csv", 1, 1_100),
			respHeartbeat(2_000),
		},
	}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	sink := &fakeSink{errs: []error{fmtWrap{indexer.ErrUnknownBucket}}}
	reader := &fakeReader{outcome: map[string]any{"ghost-bucket/x.csv": ObjectInfo{Size: 1, ETag: "e"}}}
	cfg := loopConfig()
	cfg.ApplyMaxRetries = 5 // would retry 5× if misrouted to the failure path
	a := newLoopAdapter(t, cfg, filer, reader, store, sink, zap.NewNop())
	last := store.cursorDeclared

	if err := a.runStream(context.Background(), &last); err == nil {
		t.Fatal("want scripted exhaustion error")
	}
	if got := sink.applyCount(); got != 1 {
		t.Errorf("Apply attempts = %d, want 1 — ErrUnknownBucket must be treated as processed, not retried (mutation ④)", got)
	}
	if a.UnknownBucketEvents() != 1 {
		t.Errorf("UnknownBucketEvents = %d, want 1", a.UnknownBucketEvents())
	}
	if len(store.saves) != 1 || store.saves[0] != 2_000 {
		t.Errorf("cursor saves = %v, want [2000] — the stream must keep moving after an unknown bucket", store.saves)
	}
}

// fmtWrap builds an error chain carrying the indexer sentinel, the way
// storageevent.NewIndexSink(indexer) surfaces it.
type fmtWrap struct{ err error }

func (w fmtWrap) Error() string { return "indexer: get bucket: " + w.err.Error() }
func (w fmtWrap) Unwrap() error { return w.err }

// ── gap marks (D5) ───────────────────────────────────────────────────────────

// The FIRST heartbeat after every (re)subscribe records one gap mark with the
// window GapWindow(C, H, M); later heartbeats in the same stream do not.
func TestRunStream_FirstHeartbeatRecordsGapWindow(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{
		resps: []*filerpb.SubscribeMetadataResponse{
			respHeartbeat(2_000),
			respHeartbeat(2_500),
		},
	}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())
	last := store.cursorDeclared

	if err := a.runStream(context.Background(), &last); err == nil {
		t.Fatal("want scripted exhaustion")
	}
	var gaps []Mark
	for _, m := range store.marks {
		if m.Reason == MarkReasonGap {
			gaps = append(gaps, m)
		}
	}
	if len(gaps) != 1 {
		t.Fatalf("gap marks = %d, want exactly 1 (first heartbeat only)", len(gaps))
	}
	if gaps[0].FromTsNs == nil || gaps[0].ToTsNs == nil {
		t.Fatalf("gap mark lacks bounds: %+v", gaps[0])
	}
	wantFrom, wantTo := GapWindow(1_000, 2_000, loopConfig().ResumeMargin)
	if *gaps[0].FromTsNs != wantFrom || *gaps[0].ToTsNs != wantTo {
		t.Errorf("gap window = [%d, %d], want GapWindow(1000, 2000, M) = [%d, %d]",
			*gaps[0].FromTsNs, *gaps[0].ToTsNs, wantFrom, wantTo)
	}
}

// ── bucket removal (D5 / D-037 rule 4) ───────────────────────────────────────

// A bucket-directory deletion never reaches the sink: Error log + a
// bucket_removed mark carrying the bucket and its live-row count, then the
// stream moves on. The ledger is left untouched.
func TestRunStream_BucketRemoved_NoSinkMarkWritten(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{
		resps: []*filerpb.SubscribeMetadataResponse{
			respBucketRemoved("doomed-bucket", 1_200),
			respHeartbeat(2_000),
		},
	}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000, activeCount: 42}
	core, logs := observer.New(zap.ErrorLevel)
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.New(core))
	last := store.cursorDeclared

	if err := a.runStream(context.Background(), &last); err == nil {
		t.Fatal("want scripted exhaustion")
	}
	if a.sink != nil && len(store.saves) != 1 {
		t.Fatalf("stream must continue after a bucket removal (saves=%v)", store.saves)
	}
	var removed *Mark
	for i := range store.marks {
		if store.marks[i].Reason == MarkReasonBucketRemoved {
			removed = &store.marks[i]
		}
	}
	if removed == nil {
		t.Fatalf("want a bucket_removed mark, got %+v", store.marks)
	}
	if removed.Bucket == nil || *removed.Bucket != "doomed-bucket" {
		t.Errorf("mark bucket = %v, want doomed-bucket", removed.Bucket)
	}
	if !strings.Contains(string(removed.Detail), "42") {
		t.Errorf("mark detail = %s, want it to carry the live-row count 42", removed.Detail)
	}
	var detail map[string]any
	if err := json.Unmarshal(removed.Detail, &detail); err != nil {
		t.Fatalf("detail is not JSON: %v", err)
	}
	if detail["active_entries"].(float64) != 42 {
		t.Errorf("detail.active_entries = %v, want 42", detail["active_entries"])
	}
	if logs.FilterLevelExact(zap.ErrorLevel).Len() == 0 {
		t.Error("a bucket-directory deletion must log at Error level (potential D-037 rule-4 ops bypass)")
	}
}

// ── clean shutdown (D5) ──────────────────────────────────────────────────────

// Cancelling ctx must end Run cleanly: no resubscribe attempt, no cursor
// written on the way out.
func TestRun_CtxCancel_CleanExitNoWrites(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{
		resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(2_000)},
	}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancelled before Run even starts
	err := a.Run(ctx)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Run = %v, want context.Canceled", err)
	}
	if len(filer.requests) != 0 {
		t.Errorf("subscribe calls = %d, want 0 (cancelled before subscribing)", len(filer.requests))
	}
	if len(store.saves) != 0 {
		t.Errorf("cursor saves = %v, want none on the shutdown path", store.saves)
	}
}

// ── cursor persistence throttle (D5) ─────────────────────────────────────────

// Throttling must never move the cursor backwards: with a 1h flush interval
// only the first heartbeat persists, but the persisted value stays monotonic;
// a heartbeat older than the persisted cursor is never saved at all.
func TestRunStream_CursorThrottle_NeverRegresses(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{
		resps: []*filerpb.SubscribeMetadataResponse{
			respHeartbeat(2_000),
			respHeartbeat(2_100), // inside the throttle window → skipped
		},
	}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000}
	cfg := loopConfig()
	cfg.CursorFlushInterval = time.Hour
	a := newLoopAdapter(t, cfg, filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())
	last := store.cursorDeclared

	if err := a.runStream(context.Background(), &last); err == nil {
		t.Fatal("want scripted exhaustion")
	}
	if len(store.saves) != 1 || store.saves[0] != 2_000 {
		t.Errorf("saves = %v, want [2000] (second heartbeat throttled)", store.saves)
	}

	// A later stream whose heartbeats carry an OLDER value must not regress.
	filer2 := &scriptedFiler{streams: []*scriptedStream{{
		resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(1_500)},
	}}}
	a2 := newLoopAdapter(t, cfg, filer2, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())
	if err := a2.runStream(context.Background(), &last); err == nil {
		t.Fatal("want scripted exhaustion")
	}
	if len(store.saves) != 1 {
		t.Errorf("older heartbeat must not be saved (saves=%v) — the cursor is a completeness claim", store.saves)
	}
}

// A cursor-persist failure is a stream failure: without a durable cursor the
// completeness claim would be a lie, so the adapter gives up the stream
// (resubscribing later from the last good value) instead of continuing.
func TestRunStream_CursorSaveFailure_EndsStream(t *testing.T) {
	filer := &scriptedFiler{streams: []*scriptedStream{{
		resps: []*filerpb.SubscribeMetadataResponse{respHeartbeat(2_000)},
	}}}
	store := &fakeStore{cursorFound: true, cursorDeclared: 1_000, saveErr: errors.New("pg down")}
	a := newLoopAdapter(t, loopConfig(), filer, &fakeReader{outcome: map[string]any{}}, store, &fakeSink{}, zap.NewNop())
	last := store.cursorDeclared

	err := a.runStream(context.Background(), &last)
	if err == nil || !strings.Contains(err.Error(), "pg down") {
		t.Fatalf("runStream = %v, want the save error propagated", err)
	}
}
