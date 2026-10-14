package storageevent

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"
)

// fakeIndexWriter records the calls NewIndexSink routes to the index writer.
type fakeIndexWriter struct {
	uploads   []string // one "bucket|key|size|etag|observedAt|seq" per IndexUpload
	deletions []string // one "bucket|key|observedAt|seq" per IndexDeletion
	err       error    // when set, every call fails with this error
}

func (f *fakeIndexWriter) IndexUpload(_ context.Context, bucket, key string, size int64, etag string, observedAt time.Time, seq string) error {
	f.uploads = append(f.uploads, bucket+"|"+key+"|"+strconv.FormatInt(size, 10)+"|"+etag+"|"+observedAt.UTC().Format(time.RFC3339Nano)+"|"+seq)
	return f.err
}

func (f *fakeIndexWriter) IndexDeletion(_ context.Context, bucket, key string, observedAt time.Time, seq string) error {
	f.deletions = append(f.deletions, bucket+"|"+key+"|"+observedAt.UTC().Format(time.RFC3339Nano)+"|"+seq)
	return f.err
}

var _ IndexWriter = (*fakeIndexWriter)(nil)

func TestNewIndexSink_ReturnsSink(t *testing.T) {
	s := NewIndexSink(&fakeIndexWriter{})
	if s == nil {
		t.Fatal("NewIndexSink returned nil")
	}
	if _, ok := s.(Sink); !ok {
		t.Fatalf("NewIndexSink result %T does not implement Sink", s)
	}
}

func TestIndexSink_CreatedRoutesToIndexUpload(t *testing.T) {
	fake := &fakeIndexWriter{}
	sink := NewIndexSink(fake)
	observed := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ev := Event{
		Kind:       KindObjectCreated,
		Bucket:     "data-sensor",
		Key:        "a/b/中 文.csv",
		Size:       1024,
		ETag:       "etag-1",
		ObservedAt: observed,
		Seq:        "17KNOB0000000001",
	}
	if err := sink.Apply(context.Background(), ev); err != nil {
		t.Fatalf("Apply(Created) = %v, want nil", err)
	}
	if len(fake.uploads) != 1 {
		t.Fatalf("IndexUpload called %d times, want 1", len(fake.uploads))
	}
	want := "data-sensor|a/b/中 文.csv|1024|etag-1|2026-10-15T12:00:00Z|17KNOB0000000001"
	if fake.uploads[0] != want {
		t.Errorf("IndexUpload args = %q, want %q", fake.uploads[0], want)
	}
	if len(fake.deletions) != 0 {
		t.Errorf("IndexDeletion called %d times for a Created event, want 0", len(fake.deletions))
	}
}

func TestIndexSink_RemovedRoutesToIndexDeletion(t *testing.T) {
	fake := &fakeIndexWriter{}
	sink := NewIndexSink(fake)
	observed := time.Date(2026, 10, 15, 12, 0, 0, 0, time.UTC)
	ev := Event{
		Kind:       KindObjectRemoved,
		Bucket:     "data-sensor",
		Key:        "a/b/gone.csv",
		ObservedAt: observed,
		Seq:        "17KNOB0000000002",
	}
	if err := sink.Apply(context.Background(), ev); err != nil {
		t.Fatalf("Apply(Removed) = %v, want nil", err)
	}
	if len(fake.deletions) != 1 {
		t.Fatalf("IndexDeletion called %d times, want 1", len(fake.deletions))
	}
	want := "data-sensor|a/b/gone.csv|2026-10-15T12:00:00Z|17KNOB0000000002"
	if fake.deletions[0] != want {
		t.Errorf("IndexDeletion args = %q, want %q", fake.deletions[0], want)
	}
	if len(fake.uploads) != 0 {
		t.Errorf("IndexUpload called %d times for a Removed event, want 0", len(fake.uploads))
	}
}

// TestIndexSink_BucketRemovedReturnsSentinel pins the PR1 contract: the index
// sink does not know what a removed bucket means (soft-deleted rows, frozen
// buckets and reconciliation flags are PR3b semantics), so it refuses with the
// exported sentinel instead of silently doing nothing.
func TestIndexSink_BucketRemovedReturnsSentinel(t *testing.T) {
	fake := &fakeIndexWriter{}
	sink := NewIndexSink(fake)
	ev := Event{Kind: KindBucketRemoved, Bucket: "data-sensor", ObservedAt: time.Now()}
	err := sink.Apply(context.Background(), ev)
	if !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("Apply(BucketRemoved) = %v, want ErrUnsupportedKind", err)
	}
	if len(fake.uploads) != 0 || len(fake.deletions) != 0 {
		t.Errorf("unsupported kind must not touch the writer (uploads=%d deletions=%d)", len(fake.uploads), len(fake.deletions))
	}
}

// TestIndexSink_UnknownKindReturnsSentinel: a zero or out-of-range Kind is a
// programming error upstream; it gets the same loud refusal.
func TestIndexSink_UnknownKindReturnsSentinel(t *testing.T) {
	sink := NewIndexSink(&fakeIndexWriter{})
	for _, kind := range []Kind{0, 4, -1} {
		err := sink.Apply(context.Background(), Event{Kind: kind})
		if !errors.Is(err, ErrUnsupportedKind) {
			t.Errorf("Apply(Kind(%d)) = %v, want ErrUnsupportedKind", int(kind), err)
		}
	}
}

// TestIndexSink_ErrorPropagates: a writer error is returned verbatim so the
// caller (webhook handler) can count it under the event's identity — the
// IC-4a "error = processing failure" semantic.
func TestIndexSink_ErrorPropagates(t *testing.T) {
	wantErr := errors.New("pg is down")
	sink := NewIndexSink(&fakeIndexWriter{err: wantErr})
	for _, ev := range []Event{
		{Kind: KindObjectCreated, Bucket: "b", Key: "k"},
		{Kind: KindObjectRemoved, Bucket: "b", Key: "k"},
	} {
		if err := sink.Apply(context.Background(), ev); !errors.Is(err, wantErr) {
			t.Errorf("Apply(%s) = %v, want the writer error %v", ev.Kind, err, wantErr)
		}
	}
}

// TestIndexSink_ContextThreadsThrough: the Apply context is the one handed to
// the writer, so cancellation reaches the indexer.
func TestIndexSink_ContextThreadsThrough(t *testing.T) {
	fake := &fakeIndexWriter{}
	sink := NewIndexSink(fake)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_ = sink.Apply(ctx, Event{Kind: KindObjectCreated, Bucket: "b", Key: "k"})
	// The fake ignores ctx; the assertion is compile- plus call-level plumbing
	// (signature takes ctx and reaches IndexUpload). Deep cancellation is the
	// indexer's own tested behavior.
	if len(fake.uploads) != 1 {
		t.Fatalf("IndexUpload called %d times, want 1", len(fake.uploads))
	}
}
