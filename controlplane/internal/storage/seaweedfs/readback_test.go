package seaweedfs

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
	"go.uber.org/zap"
)

// ── fakes shared by the adapter tests ────────────────────────────────────────

// readingCount reports how many clock readings were taken.
func (f *fakeClock) readingCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.times)
}

// fakeClock is a DBClock whose readings are scripted; it records the global
// call sequence so tests can pin that the DB clock is read BEFORE the S3 HEAD
// (decision D2).
type fakeClock struct {
	mu    sync.Mutex
	times []time.Time
	err   error
	seq   *int // shared with other fakes to observe interleaving
	calls []string
}

func (f *fakeClock) Now(context.Context) (time.Time, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seq != nil {
		*f.seq++
		f.calls = append(f.calls, "clock@"+strconv.Itoa(*f.seq))
	}
	if f.err != nil {
		return time.Time{}, f.err
	}
	t := time.Date(2026, 10, 20, 12, 0, 0, 0, time.UTC).Add(time.Duration(len(f.times)) * time.Millisecond)
	f.times = append(f.times, t)
	return t, nil
}

// fakeReader is an ObjectReader with scripted per-key outcomes. outcomes maps
// bucket/key → either an ObjectInfo (exists) or an error (any error type).
type fakeReader struct {
	mu      sync.Mutex
	outcome map[string]any // ObjectInfo | error
	seq     *int
	calls   []string
}

func (f *fakeReader) StatObject(_ context.Context, bucket, key string) (ObjectInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.seq != nil {
		*f.seq++
		f.calls = append(f.calls, "head@"+strconv.Itoa(*f.seq))
	}
	switch v := f.outcome[bucket+"/"+key].(type) {
	case ObjectInfo:
		return v, nil
	case error:
		return ObjectInfo{}, v
	default:
		return ObjectInfo{}, errors.New("fakeReader: no scripted outcome for " + bucket + "/" + key)
	}
}

// newReadBackAdapter builds an adapter wired only for read-back tests (the
// read-back path touches reader/clock/logger only; the loop's dependencies
// are exercised by the subscribe tests).
func newReadBackAdapter(t *testing.T, clock *fakeClock, reader *fakeReader) *Adapter {
	t.Helper()
	cfg := AdapterConfig{
		SourceID:          "seaweedfs:test",
		ClientName:        "fileagent-test",
		ClientID:          7,
		ResumeMargin:      time.Minute,
		ApplyMaxRetries:   0, // read-back tests observe a single attempt
		ApplyRetryBackoff: time.Millisecond,
	}
	if err := cfg.validate(); err != nil {
		t.Fatalf("validate config: %v", err)
	}
	return &Adapter{cfg: cfg, reader: reader, clock: clock, logger: zap.NewNop()}
}

// ── D1: the HEAD result decides the event kind ───────────────────────────────

// HEAD 200 on a Created change → Created carrying the HEAD's Size/ETag (NOT
// the filer event's size — the filer value is a hint, D1).
func TestReadBack_CreatedHead200_TakesHEADTruth(t *testing.T) {
	reader := &fakeReader{outcome: map[string]any{
		"data-sensor/a/b.csv": ObjectInfo{Size: 999, ETag: "etag-from-head"},
	}}
	clock := &fakeClock{}
	a := newReadBackAdapter(t, clock, reader)

	ev, err := a.readBack(context.Background(), Change{Kind: ChangeCreated, Bucket: "data-sensor", Key: "a/b.csv", Size: 5 /* filer hint, must be ignored */})
	if err != nil {
		t.Fatalf("readBack = %v, want nil", err)
	}
	if ev.Kind != storageevent.KindObjectCreated {
		t.Errorf("Kind = %v, want KindObjectCreated", ev.Kind)
	}
	if ev.Size != 999 || ev.ETag != "etag-from-head" {
		t.Errorf("Size/ETag = %d/%q, want the HEAD truth 999/etag-from-head (filer hint ignored)", ev.Size, ev.ETag)
	}
}

// HEAD explicit absence on a Created change → Removed (D1: the event kind
// never decides the fact; Created-but-gone IS a removal).
func TestReadBack_CreatedHeadAbsent_YieldsRemoved(t *testing.T) {
	reader := &fakeReader{outcome: map[string]any{
		"data-sensor/gone.csv": ErrObjectAbsent,
	}}
	a := newReadBackAdapter(t, &fakeClock{}, reader)

	ev, err := a.readBack(context.Background(), Change{Kind: ChangeCreated, Bucket: "data-sensor", Key: "gone.csv"})
	if err != nil {
		t.Fatalf("readBack = %v, want nil", err)
	}
	if ev.Kind != storageevent.KindObjectRemoved {
		t.Errorf("Kind = %v, want KindObjectRemoved (Created event but HEAD says absent)", ev.Kind)
	}
}

// HEAD 200 on a Removed change → Created (Removed event but the object is
// there again — e.g. a replayed deletion after a same-name re-create).
func TestReadBack_RemovedHead200_YieldsCreated(t *testing.T) {
	reader := &fakeReader{outcome: map[string]any{
		"data-sensor/back.csv": ObjectInfo{Size: 7, ETag: "e2"},
	}}
	a := newReadBackAdapter(t, &fakeClock{}, reader)

	ev, err := a.readBack(context.Background(), Change{Kind: ChangeRemoved, Bucket: "data-sensor", Key: "back.csv"})
	if err != nil {
		t.Fatalf("readBack = %v, want nil", err)
	}
	if ev.Kind != storageevent.KindObjectCreated {
		t.Errorf("Kind = %v, want KindObjectCreated (Removed event but HEAD says present)", ev.Kind)
	}
	if ev.Size != 7 || ev.ETag != "e2" {
		t.Errorf("Size/ETag = %d/%q, want the HEAD truth 7/e2", ev.Size, ev.ETag)
	}
}

// ANY other HEAD error (timeout, 5xx, 403, connection failure) is a PROCESSING
// failure — it must NOT be mapped to absence, otherwise CP would write a
// wrongful deletion on a storage hiccup (D1, mutation ①).
func TestReadBack_OtherHeadError_IsProcessingFailure(t *testing.T) {
	for name, headErr := range map[string]error{
		"timeout":      context.DeadlineExceeded,
		"forbidden":    errors.New("403: access denied"),
		"internal":     errors.New("500: internal error"),
		"conn failure": errors.New("connection refused"),
	} {
		reader := &fakeReader{outcome: map[string]any{"b/k": headErr}}
		a := newReadBackAdapter(t, &fakeClock{}, reader)
		ev, err := a.readBack(context.Background(), Change{Kind: ChangeCreated, Bucket: "b", Key: "k"})
		if err == nil {
			t.Errorf("%s: readBack returned no error — a non-absence HEAD error must be a processing failure, not an event", name)
		}
		if ev.Kind != 0 {
			t.Errorf("%s: readBack produced event kind %v on failure; want no event at all", name, ev.Kind)
		}
	}
}

// HEAD absence on a Removed change → Removed (the sane default path).
func TestReadBack_RemovedHeadAbsent_YieldsRemoved(t *testing.T) {
	reader := &fakeReader{outcome: map[string]any{"b/gone": ErrObjectAbsent}}
	a := newReadBackAdapter(t, &fakeClock{}, reader)
	ev, err := a.readBack(context.Background(), Change{Kind: ChangeRemoved, Bucket: "b", Key: "gone"})
	if err != nil {
		t.Fatalf("readBack = %v, want nil", err)
	}
	if ev.Kind != storageevent.KindObjectRemoved {
		t.Errorf("Kind = %v, want KindObjectRemoved", ev.Kind)
	}
}

// ── D2: ObservedAt is the DB clock, read BEFORE the HEAD ────────────────────

// The event's ObservedAt comes from the DB clock interface, not from CP's
// wall clock, and the reading is taken BEFORE the HEAD is issued (D2; both
// halves are caught by the shared sequence counter — mutation ② flips the
// order and this test must go red).
func TestReadBack_ObservedAt_DBClockBeforeHead(t *testing.T) {
	seq := 0
	reader := &fakeReader{outcome: map[string]any{"b/k": ObjectInfo{Size: 1, ETag: "e"}}, seq: &seq}
	clock := &fakeClock{seq: &seq}
	a := newReadBackAdapter(t, clock, reader)

	ev, err := a.readBack(context.Background(), Change{Kind: ChangeCreated, Bucket: "b", Key: "k"})
	if err != nil {
		t.Fatalf("readBack = %v, want nil", err)
	}
	if len(clock.calls) != 1 || len(reader.calls) != 1 {
		t.Fatalf("want exactly one clock reading and one HEAD, got %v / %v", clock.calls, reader.calls)
	}
	if clock.calls[0] != "clock@1" || reader.calls[0] != "head@2" {
		t.Errorf("call order = %v then %v, want clock BEFORE head (D2)", clock.calls, reader.calls)
	}
	if !ev.ObservedAt.Equal(clock.times[0]) {
		t.Errorf("ObservedAt = %v, want the DB clock reading %v", ev.ObservedAt, clock.times[0])
	}
}

// A DB-clock failure is a processing failure: without a trustworthy
// observed_at the fact must not be written at all.
func TestReadBack_ClockError_IsProcessingFailure(t *testing.T) {
	reader := &fakeReader{outcome: map[string]any{"b/k": ObjectInfo{Size: 1}}}
	clock := &fakeClock{err: errors.New("pg down")}
	a := newReadBackAdapter(t, clock, reader)

	ev, err := a.readBack(context.Background(), Change{Kind: ChangeCreated, Bucket: "b", Key: "k"})
	if err == nil {
		t.Fatal("readBack returned no error on clock failure; want a processing failure")
	}
	if ev.Kind != 0 {
		t.Errorf("produced event kind %v on clock failure; want no event", ev.Kind)
	}
}

// ── provenance ────────────────────────────────────────────────────────────────

// Every read-back fact is labeled SourceStorageReadback — both the Created and
// the Removed shape (D3).
func TestReadBack_SourceIsStorageReadback(t *testing.T) {
	reader := &fakeReader{outcome: map[string]any{
		"b/live": ObjectInfo{Size: 1, ETag: "e"},
		"b/gone": ErrObjectAbsent,
	}}
	a := newReadBackAdapter(t, &fakeClock{}, reader)
	for _, tc := range []struct {
		key string
	}{
		{key: "live"},
		{key: "gone"},
	} {
		ev, err := a.readBack(context.Background(), Change{Kind: ChangeCreated, Bucket: "b", Key: tc.key})
		if err != nil {
			t.Fatalf("readBack(%s) = %v, want nil", tc.key, err)
		}
		if ev.Source != storageevent.SourceStorageReadback {
			t.Errorf("readBack(%s).Source = %q, want %q", tc.key, ev.Source, storageevent.SourceStorageReadback)
		}
	}
}
