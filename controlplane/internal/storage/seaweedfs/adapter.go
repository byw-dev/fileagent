// adapter.go — the SeaweedFS storage-event-source adapter core (B3-PR3b,
// DECISIONS.md D-037 rule 2 / design decisions D1–D5, D7).
//
// The adapter subscribes to the filer's local metadata log, translates each
// response into Changes (translate.go), realizes every change as an index fact
// by re-reading the object's current state over S3 (readback.go — "the event
// is only a hint", D1), applies it to a storageevent.Sink in strict stream
// order (D2), and persists the idle-heartbeat declared value as the durable
// cursor (subscribe.go, D5).
//
// Every external dependency sits behind a narrow interface so the loop is
// unit-testable without a filer, S3, or PostgreSQL:
//
//	DialFunc           → dials the filer gRPC (per attempt; fault tolerance)
//	FilerClient        → the two filer RPCs the adapter uses
//	ObjectReader       → the one S3 call read-back needs (HEAD)
//	CursorMarkStore    → durable cursor + reconciliation marks
//	DBClock            → the DB clock ObservedAt comes from (D2)
//
// The production implementations of CursorMarkStore / DBClock / ObjectReader
// live in cmd/server wiring (sqlc + minio-go); this package never imports
// internal/db or minio-go (D7).
package seaweedfs

import (
	"context"
	"encoding/json"
	"errors"
	"sync/atomic"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
	"github.com/byw-dev/fileagent/controlplane/internal/storageevent"
	"go.uber.org/zap"
)

// Reconciliation-mark reasons persisted in storage_event_marks (migration
// 000011; the table is WRITE-ONLY in PR3b — its consumer is L2 reconciliation,
// IC-13).
const (
	// MarkReasonGap records the [min(C,H)−M, max(C,H)] window after a
	// (re)subscribe whose mtime range may contain events lost to a filer-side
	// disruption (D5; storage-event-source report §0.4).
	MarkReasonGap = "gap"
	// MarkReasonCursorBeforeEarliestLog records a startup self-check that found
	// the cursor predating the earliest metadata log file (the range was
	// purged; the subscription would silently skip it).
	MarkReasonCursorBeforeEarliestLog = "cursor_before_earliest_log"
	// MarkReasonBucketRemoved records a bucket-directory deletion observed on
	// the stream — per D-037 rule 4 an ops bypass when the ledger still has
	// rows for that bucket.
	MarkReasonBucketRemoved = "bucket_removed"
)

// Defaults for AdapterConfig fields the caller may leave unset. They are
// deliberately NOT configuration surface (D6 names the env vars); override
// via AdapterConfig when wiring.
const (
	// DefaultResumeMargin is the rewind applied on every (re)subscribe
	// (SEAWEEDFS_RESUME_MARGIN default, D6).
	DefaultResumeMargin = 60 * time.Second
	// DefaultApplyMaxRetries: in-place retries of one change's read-back+Apply
	// before the adapter gives up the stream and resubscribes from the durable
	// cursor (SEAWEEDFS_APPLY_MAX_RETRIES default).
	DefaultApplyMaxRetries = 8
	// DefaultApplyRetryBackoff is the base wait between those retries
	// (SEAWEEDFS_APPLY_RETRY_BACKOFF default).
	DefaultApplyRetryBackoff = time.Second
	// DefaultCursorFlushInterval throttles cursor writes on rapid heartbeats
	// (≥1 write per interval; throttling can never move the cursor backwards).
	DefaultCursorFlushInterval = time.Second
	// reconnectDelay is the pause between resubscribe attempts after a stream
	// ends. Fixed: reconnect pacing is failure handling, not a deployment knob.
	reconnectDelay = time.Second
	// DefaultSelfCheckTimeout bounds one startup self-check (F2). The check is
	// best-effort: on timeout it Warns and the subscription proceeds.
	DefaultSelfCheckTimeout = 5 * time.Second
	// subscribePathPrefix is the filer path prefix every S3 object lives under.
	subscribePathPrefix = "/buckets/"
)

// ErrObjectAbsent is returned by ObjectReader when the object DEFINITIVELY
// does not exist (S3 NoSuchKey / HTTP 404). It is the ONLY outcome read-back
// may translate into a Removed fact — every other error is a processing
// failure (D1).
var ErrObjectAbsent = errors.New("seaweedfs: object does not exist")

// ObjectInfo carries the HEAD result fields the index needs.
type ObjectInfo struct {
	// Size is the object size in bytes.
	Size int64
	// ETag is the object's ETag as reported by S3.
	ETag string
}

// ObjectReader is the narrow S3 surface read-back needs (D1). Implementations
// MUST map a definitive absence (NoSuchKey / HTTP 404) to ErrObjectAbsent and
// return every other failure as-is: mistaking a 5xx/timeout/403 for absence
// would write a wrongful deletion into the index.
type ObjectReader interface {
	StatObject(ctx context.Context, bucket, key string) (ObjectInfo, error)
}

// DBClock provides the timestamp every read-back fact carries (D2): a DB
// reading taken BEFORE the HEAD, so later read-backs strictly order after
// earlier ones on the same stream and the index's observed_at eviction can
// compare agent facts (DB now()) with read-back facts fairly.
type DBClock interface {
	Now(ctx context.Context) (time.Time, error)
}

// FilerStream is the receive side of a metadata subscription.
type FilerStream interface {
	Recv() (*filerpb.SubscribeMetadataResponse, error)
}

// SubStream is an exported alias for test support packages.
type SubStream = FilerStream

// ListEntriesStream is the receive side of a filer directory listing.
type ListEntriesStream interface {
	Recv() (*filerpb.ListEntriesResponse, error)
}

// ListStreamAlias is an exported alias for test support packages.
type ListStreamAlias = ListEntriesStream

// MarkAlias is an exported alias for test support packages.
type MarkAlias = Mark

// FilerClient is the narrow filer gRPC surface the adapter needs (D7). The
// real implementation wraps filerpb.SeaweedFilerClient and lives beside the
// dialer in this package; cmd/server only ever sees this interface.
type FilerClient interface {
	SubscribeLocalMetadata(ctx context.Context, in *filerpb.SubscribeMetadataRequest) (FilerStream, error)
	ListEntries(ctx context.Context, in *filerpb.ListEntriesRequest) (ListEntriesStream, error)
}

// DialFunc dials one filer connection. It is called once per subscription
// attempt (a broken proxy/TCP path must not pin a dead connection), and the
// returned close func is called when the attempt ends. close may be nil.
type DialFunc func(ctx context.Context) (FilerClient, func(), error)

// Mark is one pending-reconciliation row for storage_event_marks. SourceID is
// the event-source instance the mark belongs to; optional columns are pointers
// (nil = NULL). Detail carries reason-specific JSON.
type Mark struct {
	SourceID string
	Reason   string
	Bucket   *string
	FromTsNs *int64
	ToTsNs   *int64
	Detail   json.RawMessage
}

// CursorMarkStore persists the durable cursor and the reconciliation marks
// (D4). Production implementation: sqlc queries over storage_event_cursors /
// storage_event_marks (cmd/server wiring).
type CursorMarkStore interface {
	// DeclaredCursor returns the persisted declared value for sourceID;
	// found is false when no row exists yet (fresh deployment → subscribe
	// from 0, D5).
	DeclaredCursor(ctx context.Context, sourceID string) (declaredTsNs int64, found bool, err error)
	// SaveCursor persists a new declared value. Implementations must keep the
	// persisted value monotonic (never regress).
	SaveCursor(ctx context.Context, sourceID string, declaredTsNs int64) error
	// RecordMark appends one pending-reconciliation mark.
	RecordMark(ctx context.Context, mark Mark) error
	// CountActiveEntries counts the bucket's file_entries rows whose status is
	// not 'deleted' — the "ledger still knows this bucket" number a
	// bucket-removed mark carries (D5 / D-037 rule 4).
	CountActiveEntries(ctx context.Context, bucket string) (int64, error)
}

// AdapterConfig is the adapter's wiring-time configuration (D6 values, loaded
// from env in cmd/server). Validate fills defaults and rejects nonsense.
type AdapterConfig struct {
	// SourceID identifies this event-source instance in
	// storage_event_cursors/marks (e.g. "seaweedfs:<filer addr>"). Required.
	SourceID string
	// ClientName/ClientID identify the subscriber to the filer. Both must be
	// STABLE across restarts: the filer uses them to re-attach a returning
	// subscriber to its backlog (D5).
	ClientName string
	ClientID   int32
	// ResumeMargin is M — the rewind applied on every (re)subscribe to ride
	// out filer clock rollback across restarts (D5). Defaults to
	// DefaultResumeMargin; negative values are rejected (they would skip
	// events — ResumeFrom clamps, but a configured negative margin is a
	// misconfiguration, not a request for zero margin).
	ResumeMargin time.Duration
	// ApplyMaxRetries: in-place retries of one change before the stream is
	// abandoned and resubscribed from the durable cursor (D5; the change is
	// NEVER skipped). Defaults to DefaultApplyMaxRetries.
	ApplyMaxRetries int
	// ApplyRetryBackoff is the wait between in-place retries. Defaults to
	// DefaultApplyRetryBackoff.
	ApplyRetryBackoff time.Duration
	// CursorFlushInterval throttles cursor persistence on rapid heartbeats.
	// Defaults to DefaultCursorFlushInterval; ≤0 disables throttling (write
	// every heartbeat).
	CursorFlushInterval time.Duration
	// SelfCheckTimeout bounds the startup out-of-range self-check (F2).
	// Zero/omitted → DefaultSelfCheckTimeout. Not env surface (D6); wired
	// tests shrink it.
	SelfCheckTimeout time.Duration
}

// validate applies defaults and rejects misconfiguration.
func (c *AdapterConfig) validate() error {
	if c.SourceID == "" {
		return errors.New("seaweedfs: adapter SourceID is required")
	}
	if c.ClientName == "" {
		return errors.New("seaweedfs: adapter ClientName is required (stable across restarts)")
	}
	if c.ResumeMargin == 0 {
		c.ResumeMargin = DefaultResumeMargin
	}
	if c.ResumeMargin < 0 {
		return errors.New("seaweedfs: ResumeMargin must not be negative (a negative margin would subscribe past the declared value and skip events)")
	}
	if c.ApplyMaxRetries == 0 {
		c.ApplyMaxRetries = DefaultApplyMaxRetries
	}
	if c.ApplyMaxRetries < 0 {
		return errors.New("seaweedfs: ApplyMaxRetries must not be negative")
	}
	if c.ApplyRetryBackoff == 0 {
		c.ApplyRetryBackoff = DefaultApplyRetryBackoff
	}
	if c.ApplyRetryBackoff < 0 {
		return errors.New("seaweedfs: ApplyRetryBackoff must not be negative")
	}
	if c.CursorFlushInterval == 0 {
		c.CursorFlushInterval = DefaultCursorFlushInterval
	}
	if c.CursorFlushInterval < 0 {
		return errors.New("seaweedfs: CursorFlushInterval must not be negative")
	}
	return nil
}

// selfCheckTimeoutOrDefault returns the configured self-check bound, or the
// default when unset (validate does not inject it — it is not env surface).
func (c *AdapterConfig) selfCheckTimeoutOrDefault() time.Duration {
	if c.SelfCheckTimeout > 0 {
		return c.SelfCheckTimeout
	}
	return DefaultSelfCheckTimeout
}

// Adapter is the storage-event-source adapter: one subscription loop over one
// filer local stream (D5: a single goroutine, strictly ordered — later
// read-backs must carry later ObservedAt, which parallelism would break).
type Adapter struct {
	cfg    AdapterConfig
	dial   DialFunc
	reader ObjectReader
	store  CursorMarkStore
	clock  DBClock
	sink   storageevent.Sink
	logger *zap.Logger

	// epoch is the filer client_epoch: incremented per (re)subscribe so the
	// filer recognizes a returning subscriber (research prototype precedent).
	epoch atomic.Int32
	// unknownBuckets counts events for buckets without a ledger row (D5:
	// not a failure; Warn + count + treat as processed).
	unknownBuckets atomic.Uint64
	// lastFlush paces cursor persistence (CursorFlushInterval).
	lastFlush time.Time
	// reconnectDelay is a var (not const) only so tests can shrink it.
	reconnectDelay time.Duration
	// selfCheckTimeout bounds the startup out-of-range self-check (F2).
	selfCheckTimeout time.Duration
	// reporter (optional, N1) lets the adapter distinguish a suppressed write
	// that a newer observation already confirmed (same state → processed)
	// from a genuine conflict (re-derive). Wired with the indexer.
	reporter storageevent.SuppressionReporting
}

// NewAdapter wires an Adapter. dial, reader, store, clock and sink must all be
// non-nil — an adapter missing one of them has no way to do its job.
func NewAdapter(cfg AdapterConfig, dial DialFunc, reader ObjectReader, store CursorMarkStore, clock DBClock, sink storageevent.Sink, logger *zap.Logger) (*Adapter, error) {
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if dial == nil {
		return nil, errors.New("seaweedfs: adapter needs a filer DialFunc")
	}
	if reader == nil {
		return nil, errors.New("seaweedfs: adapter needs an ObjectReader")
	}
	if store == nil {
		return nil, errors.New("seaweedfs: adapter needs a CursorMarkStore")
	}
	if clock == nil {
		return nil, errors.New("seaweedfs: adapter needs a DBClock")
	}
	if sink == nil {
		return nil, errors.New("seaweedfs: adapter needs a storageevent.Sink")
	}
	if logger == nil {
		logger = zap.NewNop()
	}
	a := &Adapter{
		cfg:              cfg,
		dial:             dial,
		reader:           reader,
		store:            store,
		clock:            clock,
		sink:             sink,
		logger:           logger,
		reconnectDelay:   reconnectDelay,
		selfCheckTimeout: cfg.selfCheckTimeoutOrDefault(),
	}
	return a, nil
}

// WithSuppressionReporter wires the optional suppression reporter (N1). The
// reporter must be the SAME index writer family the sink applies to (the
// indexer), so its row view reflects what the sink wrote.
func (a *Adapter) WithSuppressionReporter(r storageevent.SuppressionReporting) *Adapter {
	a.reporter = r
	return a
}

// UnknownBucketEvents reports how many events named a bucket the ledger does
// not know (exposed for observability/tests).
func (a *Adapter) UnknownBucketEvents() uint64 { return a.unknownBuckets.Load() }

// int64Ptr is a small helper for building Marks.
func int64Ptr(v int64) *int64 { return &v }
