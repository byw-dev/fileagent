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

// ErrEmptySource is returned by the index sink for an Event whose Source is
// empty. Adapters must label every fact with its provenance explicitly
// (Event.Source); the sink refuses to guess, because a mislabeled row corrupts
// the ledger's answer to "who wrote this fact" forever after.
var ErrEmptySource = errors.New("storageevent: event Source is empty — adapters must set it explicitly")

// Sink is the only entry point the index side knows. Apply returns a non-nil
// error when processing FAILED — the caller decides retry / dead-letter
// policy (the MinIO webhook handler counts failures under the event identity,
// IC-4a). A nil error means the event is PROCESSED and needs no retry; it is
// deliberately not a durability promise — the writer's own semantics decide
// what "processed" persists (e.g. an upload that lost a last-writer race and
// was suppressed, or a deletion matching no existing row, both return nil,
// and a future alerting sink's nil would only mean "alert accepted").
type Sink interface {
	Apply(ctx context.Context, ev Event) error
}

// IndexWriter is the indexing surface the index sink adapts: exactly the two
// operations the MinIO webhook path already drove, extended in B3-PR3b with
// the source parameter (the adapter's provenance label persisted in
// file_entries.source; migration 000011 added 'storage_readback'). It is
// satisfied by *indexer.Indexer.
type IndexWriter interface {
	// IndexUpload records an ObjectCreated event in the file index. source is
	// the adapter's provenance label (see Event.Source) and is persisted
	// verbatim in file_entries.source.
	// A returned error is a PROCESSING failure: the caller counts it under
	// the event's identity and answers 5xx (under the cap) so the source
	// redelivers, or dead-letters the event past the cap. This is the core
	// IC-4a semantic; errors are NOT "logged only".
	IndexUpload(ctx context.Context, bucketName, objectKey string, sizeBytes int64, etag string, observedAt time.Time, eventSeq string, source string) error
	// IndexDeletion records an ObjectRemoved event: it soft-deletes the
	// matching file entry and publishes events.file.deleted. source follows
	// the same contract as IndexUpload. Errors follow the same counted
	// failure path as IndexUpload.
	IndexDeletion(ctx context.Context, bucketName, objectKey string, observedAt time.Time, eventSeq string, source string) error
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
// An Event with an empty Source is rejected: provenance is part of the fact,
// and silently defaulting it (to minio_event) would mislabel every read-back
// row the SeaweedFS adapter writes.
func (s *indexSink) Apply(ctx context.Context, ev Event) error {
	if ev.Source == "" {
		return ErrEmptySource
	}
	switch ev.Kind {
	case KindObjectCreated:
		return s.ix.IndexUpload(ctx, ev.Bucket, ev.Key, ev.Size, ev.ETag, ev.ObservedAt, ev.Seq, ev.Source)
	case KindObjectRemoved:
		return s.ix.IndexDeletion(ctx, ev.Bucket, ev.Key, ev.ObservedAt, ev.Seq, ev.Source)
	default:
		// Includes KindBucketRemoved: the index has no bucket-removal
		// semantics yet (PR3b), and silently dropping the event would hide
		// exactly the "bucket gone but rows remain" divergence D-037 says
		// must alert.
		return ErrUnsupportedKind
	}
}
