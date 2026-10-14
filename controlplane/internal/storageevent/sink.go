package storageevent

import (
	"context"
	"errors"
	"time"
)

// ErrUnsupportedKind is returned by the index sink for event kinds whose index
// semantics do not exist yet (KindBucketRemoved) or that are out of range.
// It is a sentinel so callers can distinguish "this sink refuses this kind"
// from an indexing failure that retry/dead-letter machinery should count.
// Bucket-removal semantics (freeze/alert/reconciliation marking) arrive with
// B3-PR3b.
var ErrUnsupportedKind = errors.New("storageevent: event kind not supported by the index sink")

// Sink is the only entry point the index side knows. Apply returns a non-nil
// error when processing FAILED — the caller decides retry / dead-letter
// policy (the MinIO webhook handler counts failures under the event identity,
// IC-4a). A nil error means the event is durably reflected in the index.
type Sink interface {
	Apply(ctx context.Context, ev Event) error
}

// IndexWriter is the indexing surface the index sink adapts: exactly the two
// operations the MinIO webhook path already drove (signatures unchanged from
// the former handler.IndexerClient). It is satisfied by *indexer.Indexer.
type IndexWriter interface {
	// IndexUpload records an ObjectCreated event in the file index.
	// A returned error is a PROCESSING failure: the caller counts it under
	// the event's identity and answers 5xx (under the cap) so the source
	// redelivers, or dead-letters the event past the cap. This is the core
	// IC-4a semantic; errors are NOT "logged only".
	IndexUpload(ctx context.Context, bucketName, objectKey string, sizeBytes int64, etag string, observedAt time.Time, eventSeq string) error
	// IndexDeletion records an ObjectRemoved event: it soft-deletes the
	// matching file entry and publishes events.file.deleted. Errors follow
	// the same counted failure path as IndexUpload.
	IndexDeletion(ctx context.Context, bucketName, objectKey string, observedAt time.Time, eventSeq string) error
}

// indexSink routes storage events into the file index. It is the MinIO-era
// behavior wrapped behind Sink: Created → IndexUpload, Removed →
// IndexDeletion, anything else refused.
type indexSink struct {
	ix IndexWriter
}

// NewIndexSink adapts an IndexWriter (the concrete indexer) into a Sink.
// ix must be non-nil; wiring that wants "no indexing" passes a nil Sink
// instead of an IndexWriter here.
func NewIndexSink(ix IndexWriter) Sink {
	return &indexSink{ix: ix}
}

// Apply routes one event to the index writer. See Sink for the error
// contract; ErrUnsupportedKind covers KindBucketRemoved and unknown kinds.
func (s *indexSink) Apply(ctx context.Context, ev Event) error {
	switch ev.Kind {
	case KindObjectCreated:
		return s.ix.IndexUpload(ctx, ev.Bucket, ev.Key, ev.Size, ev.ETag, ev.ObservedAt, ev.Seq)
	case KindObjectRemoved:
		return s.ix.IndexDeletion(ctx, ev.Bucket, ev.Key, ev.ObservedAt, ev.Seq)
	default:
		// Includes KindBucketRemoved: the index has no bucket-removal
		// semantics yet (PR3b), and silently dropping the event would hide
		// exactly the "bucket gone but rows remain" divergence D-037 says
		// must alert.
		return ErrUnsupportedKind
	}
}
