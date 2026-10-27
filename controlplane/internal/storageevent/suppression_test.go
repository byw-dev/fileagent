package storageevent

import (
	"context"
	"errors"
	"testing"
	"time"
)

// ── F1a: suppression as a distinguishable signal ─────────────────────────────
//
// The index upsert guard evicts stale writes (observed_at); for the MinIO
// webhook path a suppressed event is simply processed (nil), and that must not
// change. The SeaweedFS read-back adapter, however, MUST be able to tell "the
// write landed" from "the write was evicted by a newer observation" — a
// suppressed read-back fact (e.g. a delayed agent UploadResult won the
// observed_at race) requires re-deriving the fact (fresh DB clock + fresh
// HEAD + re-Apply) instead of advancing past the last event.

// reportingWriter implements both IndexWriter and SuppressionReporting.
type reportingWriter struct {
	fakeIndexWriter
	upSuppress   bool
	delSuppress  bool
	reportCalls  int
}

func (w *reportingWriter) IndexUploadReporting(ctx context.Context, bucket, key string, size int64, etag string, observedAt time.Time, seq string, source string) (bool, error) {
	w.reportCalls++
	return w.upSuppress, w.IndexUpload(ctx, bucket, key, size, etag, observedAt, seq, source)
}

func (w *reportingWriter) IndexDeletionReporting(ctx context.Context, bucket, key string, observedAt time.Time, seq string, source string) (bool, error) {
	w.reportCalls++
	return w.delSuppress, w.IndexDeletion(ctx, bucket, key, observedAt, seq, source)
}

func (w *reportingWriter) RowState(context.Context, string, string) (RowState, error) {
	return RowState{Found: false}, nil
}

var _ SuppressionReporting = (*reportingWriter)(nil)

// The suppression-aware sink converts a suppressed result into ErrSuppressed
// so errors.Is-based callers can retry with fresh state.
func TestSuppressionAwareSink_SuppressedUploadReturnsSentinel(t *testing.T) {
	w := &reportingWriter{fakeIndexWriter: fakeIndexWriter{}, upSuppress: true}
	sink := NewSuppressionAwareIndexSink(w)
	err := sink.Apply(context.Background(), Event{Kind: KindObjectCreated, Bucket: "b", Key: "k", Source: SourceMinioEvent})
	if !errors.Is(err, ErrSuppressed) {
		t.Fatalf("Apply(suppressed upload) = %v, want ErrSuppressed", err)
	}
	if w.reportCalls != 1 {
		t.Errorf("reporting method called %d times, want 1", w.reportCalls)
	}
}

func TestSuppressionAwareSink_SuppressedDeletionReturnsSentinel(t *testing.T) {
	w := &reportingWriter{fakeIndexWriter: fakeIndexWriter{}, delSuppress: true}
	sink := NewSuppressionAwareIndexSink(w)
	err := sink.Apply(context.Background(), Event{Kind: KindObjectRemoved, Bucket: "b", Key: "k", Source: SourceMinioEvent})
	if !errors.Is(err, ErrSuppressed) {
		t.Fatalf("Apply(suppressed deletion) = %v, want ErrSuppressed", err)
	}
}

func TestSuppressionAwareSink_NonSuppressedStaysNil(t *testing.T) {
	w := &reportingWriter{fakeIndexWriter: fakeIndexWriter{}}
	sink := NewSuppressionAwareIndexSink(w)
	if err := sink.Apply(context.Background(), Event{Kind: KindObjectCreated, Bucket: "b", Key: "k", Source: SourceMinioEvent}); err != nil {
		t.Fatalf("Apply(normal upload) = %v, want nil", err)
	}
	if err := sink.Apply(context.Background(), Event{Kind: KindObjectRemoved, Bucket: "b", Key: "k", Source: SourceMinioEvent}); err != nil {
		t.Fatalf("Apply(normal deletion) = %v, want nil", err)
	}
	if w.reportCalls != 2 {
		t.Errorf("reporting method called %d times, want 2", w.reportCalls)
	}
}

func TestSuppressionAwareSink_WriterErrorStillPropagates(t *testing.T) {
	wantErr := errors.New("pg down")
	w := &reportingWriter{fakeIndexWriter: fakeIndexWriter{err: wantErr}, upSuppress: true}
	sink := NewSuppressionAwareIndexSink(w)
	if err := sink.Apply(context.Background(), Event{Kind: KindObjectCreated, Bucket: "b", Key: "k", Source: SourceMinioEvent}); !errors.Is(err, wantErr) {
		t.Fatalf("Apply(writer error) = %v, want the writer error", err)
	}
}

func TestSuppressionAwareSink_BucketRemovedStillUnsupported(t *testing.T) {
	sink := NewSuppressionAwareIndexSink(&reportingWriter{})
	if err := sink.Apply(context.Background(), Event{Kind: KindBucketRemoved, Bucket: "b", Source: SourceMinioEvent}); !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("Apply(BucketRemoved) = %v, want ErrUnsupportedKind", err)
	}
}

// A writer that does NOT implement SuppressionReporting still works through
// the aware sink (falls back to plain routing, suppression unknowable — the
// cmd wiring always passes the real indexer, which reports).
func TestSuppressionAwareSink_NonReportingWriterFallsBackToPlain(t *testing.T) {
	w := &fakeIndexWriter{}
	sink := NewSuppressionAwareIndexSink(w)
	if err := sink.Apply(context.Background(), Event{Kind: KindObjectCreated, Bucket: "b", Key: "k", Source: SourceMinioEvent}); err != nil {
		t.Fatalf("Apply(non-reporting upload) = %v, want nil", err)
	}
	if err := sink.Apply(context.Background(), Event{Kind: KindObjectRemoved, Bucket: "b", Key: "k", Source: SourceMinioEvent}); err != nil {
		t.Fatalf("Apply(non-reporting deletion) = %v, want nil", err)
	}
	if len(w.uploads) != 1 || len(w.deletions) != 1 {
		t.Errorf("uploads=%d deletions=%d, want 1/1 (plain methods used)", len(w.uploads), len(w.deletions))
	}
}

// The aware sink refuses empty Source and unsupported kinds exactly like the
// plain one (identical refusal contract).
func TestSuppressionAwareSink_RefusalsMatchPlainSink(t *testing.T) {
	sink := NewSuppressionAwareIndexSink(&reportingWriter{})
	if err := sink.Apply(context.Background(), Event{Kind: KindObjectCreated, Bucket: "b", Key: "k"}); !errors.Is(err, ErrEmptySource) {
		t.Fatalf("Apply(empty Source) = %v, want ErrEmptySource", err)
	}
	if err := sink.Apply(context.Background(), Event{Kind: 0, Bucket: "b", Key: "k", Source: SourceMinioEvent}); !errors.Is(err, ErrUnsupportedKind) {
		t.Fatalf("Apply(unknown kind) = %v, want ErrUnsupportedKind", err)
	}
}

// A reporting writer whose underlying methods FAIL propagates the error even
// when it also reports suppression (error precedence).
func TestSuppressionAwareSink_ReportedErrorPrecedesSuppression(t *testing.T) {
	wantErr := errors.New("write failed")
	w := &reportingWriter{fakeIndexWriter: fakeIndexWriter{err: wantErr}, upSuppress: true, delSuppress: true}
	sink := NewSuppressionAwareIndexSink(w)
	if err := sink.Apply(context.Background(), Event{Kind: KindObjectCreated, Bucket: "b", Key: "k", Source: SourceMinioEvent}); !errors.Is(err, wantErr) {
		t.Errorf("Apply(created, writer err + suppressed) = %v, want the writer error", err)
	}
	if err := sink.Apply(context.Background(), Event{Kind: KindObjectRemoved, Bucket: "b", Key: "k", Source: SourceMinioEvent}); !errors.Is(err, wantErr) {
		t.Errorf("Apply(removed, writer err + suppressed) = %v, want the writer error", err)
	}
}

// The MinIO webhook path keeps using the plain constructor: suppression stays
// a silent success there (behavior unchanged by F1a).
func TestPlainIndexSink_IgnoresSuppressionReporting(t *testing.T) {
	w := &reportingWriter{fakeIndexWriter: fakeIndexWriter{}, upSuppress: true, delSuppress: true}
	sink := NewIndexSink(w)
	if err := sink.Apply(context.Background(), Event{Kind: KindObjectCreated, Bucket: "b", Key: "k", Source: SourceMinioEvent}); err != nil {
		t.Fatalf("plain sink Apply(suppressed upload) = %v, want nil (MinIO behavior unchanged)", err)
	}
	if err := sink.Apply(context.Background(), Event{Kind: KindObjectRemoved, Bucket: "b", Key: "k", Source: SourceMinioEvent}); err != nil {
		t.Fatalf("plain sink Apply(suppressed deletion) = %v, want nil (MinIO behavior unchanged)", err)
	}
	if w.reportCalls != 0 {
		t.Errorf("plain sink must not use the reporting methods, called %d times", w.reportCalls)
	}
}
