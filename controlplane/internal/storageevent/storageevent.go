// Package storageevent defines the Control Plane's「storage event source」
// contract (DECISIONS.md D-037 rule 2): the single interface the index side
// knows about object-storage change events, independent of which storage
// backend produced them.
//
// Adapters translate their transport-specific payloads (MinIO S3 event
// records today; the SeaweedFS filer subscription in PR3a/PR3b) into Event
// values and hand them to a Sink. Business and index code never see a storage
// client — that boundary is enforced by an import-boundary test in this
// package (boundary_test.go).
//
// Events are a low-latency hint, not the source of truth: D-037 keeps
// reconciliation (L2) authoritative, and consumers re-read object state via
// the S3 API before writing the index.
package storageevent

import (
	"strconv"
	"time"
)

// Kind classifies a storage event. The numeric values are pinned by the
// contract (KindObjectCreated = 1, ascending); do not renumber.
type Kind int

const (
	// KindObjectCreated reports that an object was written (S3
	// ObjectCreated:*, a filer CreateEntry notification).
	KindObjectCreated Kind = iota + 1
	// KindObjectRemoved reports that one object was deleted (S3
	// ObjectRemoved:*, a filer DeleteEntry notification).
	KindObjectRemoved
	// KindBucketRemoved reports that a bucket directory was removed (only
	// some backends emit it — SeaweedFS leveldb3/postgres2 delete a bucket as
	// one directory event without per-object events). The index sink does not
	// implement its semantics yet; see ErrUnsupportedKind.
	KindBucketRemoved
)

// String returns a stable, log-friendly name for the kind.
func (k Kind) String() string {
	switch k {
	case KindObjectCreated:
		return "object_created"
	case KindObjectRemoved:
		return "object_removed"
	case KindBucketRemoved:
		return "bucket_removed"
	default:
		return "Kind(" + strconv.Itoa(int(k)) + ")"
	}
}

// Event is one object-storage change, normalized across backends.
type Event struct {
	// Kind selects the routing semantics (see the Kind constants).
	Kind Kind
	// Bucket is the bucket name the event happened in.
	Bucket string
	// Key is the DECODED object key; empty for KindBucketRemoved. Producers
	// that URL-encode keys in their payload (MinIO) must decode before
	// filling this field — storage_path holds the real object key.
	Key string
	// Size is the object size in bytes; only meaningful for KindObjectCreated.
	Size int64
	// ETag is the object ETag; only meaningful for KindObjectCreated.
	ETag string
	// ObservedAt is the event time as observed at the source (payload
	// eventTime / notification timestamp), not the local processing time.
	// Ordering caveat (db/queries/ingest.sql): when Seq is empty the index
	// still evicts stale writes by ObservedAt alone — a delayed event
	// carrying an old source timestamp can be applied (nil error) yet
	// suppressed by the upsert. Sources that re-read current object state
	// before applying (the PR3b read-back adapter) must therefore define
	// what ObservedAt means for that read-back fact; they cannot reuse a
	// source event timestamp blindly.
	ObservedAt time.Time
	// Seq is the source-side ordering key (MinIO sequencer). Sources without
	// a trustworthy ordering key leave it empty — D-037: a source timestamp
	// that is not strictly ordered (SeaweedFS ts_ns) must NOT be used as one.
	// An empty Seq does NOT disable time-based eviction: with Seq empty the
	// index orders solely by ObservedAt (see the caveat above), so the
	// empty-string case is "no source-side tie-break", not "no ordering".
	Seq string
	// Source records which storage-event adapter produced the fact — the
	// file_entries.source value the index will persist. It MUST be filled
	// explicitly by the adapter (SourceMinioEvent for the MinIO webhook
	// translateRecord, SourceStorageReadback for the SeaweedFS filer
	// read-back adapter); the index sink refuses Events with an empty Source
	// rather than defaulting one, because provenance is part of the fact.
	Source string
}

// Provenance values adapters must set on Event.Source. They are part of the
// file_entries.source CHECK constraint (migration 000011) — extend that
// migration chain first if a new source ever appears.
const (
	// SourceMinioEvent marks facts that arrived via the MinIO S3-event webhook
	// (the transitional backend's event path, unchanged behavior).
	SourceMinioEvent = "minio_event"
	// SourceStorageReadback marks facts the SeaweedFS filer-subscription
	// adapter wrote after re-reading the object's current state over S3
	// (the "event is only a hint, read back the truth" semantic, D-037).
	SourceStorageReadback = "storage_readback"
)
