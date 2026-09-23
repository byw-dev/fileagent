package handler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"go.uber.org/zap"
)

// ── Webhook failure semantics (IC-4a ① / IC-BUG-6) ──────────────────────────
//
// One path, no 4xx/5xx split:
//
//	indexing fails → count the failure under the event's identity
//	  → under the cap: respond 5xx (MinIO retries)
//	  → over the cap:  dead-letter the event, respond 200 (free the queue)
//
// Why not 400 for malformed payloads: dev-measured (2026-09-10) MinIO treats
// 400 and 500 identically — both go through the sendSync failure branch and
// are retried. With a persistent queue_dir a 400 would be retried forever and
// head-of-line block the whole feed. Every failure therefore flows through
// this one decision; the only escape is the retry cap.
//
// Why the cap exists at all: MinIO's queue_dir is a single head-of-line
// blocking queue (~3s per retry round, dev-measured). A permanently failing
// event would stall every later event indefinitely. Past the cap the event is
// dead-lettered (never silently dropped) and the queue is freed.
//
// Cap sizing: WEBHOOK_FAIL_LIMIT must exceed (expected outage duration ÷ 3s).
// The default of 600 covers a 30-minute transient outage (600 × 3s = 30min)
// with margin — long enough to ride out a PG failover or a deploy, short
// enough that a true poison pill is dead-lettered within ~30 minutes instead
// of blocking the feed forever. Configurable via WEBHOOK_FAIL_LIMIT.

// DefaultWebhookFailLimit is the poison-pill retry cap when
// WEBHOOK_FAIL_LIMIT is unset. See the sizing rationale above.
const DefaultWebhookFailLimit int64 = 600

// MinWebhookFailLimit floors the configured cap. A cap of 0 would dead-letter
// on the first transient hiccup, defeating the whole retry mechanism.
const MinWebhookFailLimit int64 = 1

// WebhookFailStore is the persistent failure-counter backend (IC-4a (b)).
//
// The counter MUST be persistent — process memory would reset on a CP crash
// (the most likely companion of an indexing outage) or an ops restart, and a
// poison pill whose counter resets can never reach the cap: the feed would
// stay blocked forever. Redis backs this store in production; the interface
// keeps the handler unit-testable without a real Redis.
type WebhookFailStore interface {
	// IncrFailCount increments the persistent failure counter for one event
	// identity and returns the new value.
	IncrFailCount(ctx context.Context, identity string) (int64, error)
	// FailCount returns the current failure count for one event identity
	// (0 when the event has never failed).
	FailCount(ctx context.Context, identity string) (int64, error)
	// ClearFailCount removes the counter after a successful delivery.
	ClearFailCount(ctx context.Context, identity string) error
}

// NewRedisWebhookFailStore builds the Redis-backed failure-counter store
// (production wiring).
func NewRedisWebhookFailStore(client *cache.Client) WebhookFailStore {
	return &redisFailStore{client: client}
}

// redisFailStore adapts *cache.Client to WebhookFailStore.
type redisFailStore struct {
	client *cache.Client
}

// IncrFailCount increments the persistent failure counter for the event.
func (s *redisFailStore) IncrFailCount(ctx context.Context, identity string) (int64, error) {
	return s.client.Incr(ctx, cache.WebhookFailCountKey(identity))
}

// FailCount reads the persistent failure counter for the event.
func (s *redisFailStore) FailCount(ctx context.Context, identity string) (int64, error) {
	return s.client.GetInt64(ctx, cache.WebhookFailCountKey(identity))
}

// ClearFailCount drops the counter after a successful indexing attempt.
func (s *redisFailStore) ClearFailCount(ctx context.Context, identity string) error {
	return s.client.Del(ctx, cache.WebhookFailCountKey(identity))
}

// deadLetterSink is where exhausted events land (IC-4a (c)).
//
// Dead letters MUST land in a table, not only in logs: a log line is a silent
// loss (nothing to query, nothing to alert on, nothing to replay). Redrive is
// operator-driven; who redrives and how is documented on the table and in
// consistency-and-ingest.md §3.7.
// DeadLetterSink is where exhausted events land (IC-4a (c)).
//
// Dead letters MUST land in a table, not only in logs: a log line is a silent
// loss (nothing to query, nothing to alert on, nothing to replay). Redrive is
// operator-driven; who redrives and how is documented on the table and in
// consistency-and-ingest.md §3.7.
type DeadLetterSink interface {
	// DeadLetter stores (or refreshes) a dead letter for the event.
	DeadLetter(ctx context.Context, dl DeadLetter) error
}

// deadLetterIdentity builds the event's dedup key from the only stable parts
// of a MinIO webhook delivery (IC-4a (a)).
//
// Measured live (2026-09-10): the request carries only
// Host / User-Agent / Content-Length / Authorization / Content-Type — no event
// ID, no MinIO-side retry count. bucket+key+sequencer is the identity S3
// itself defines for ordering events on one object, and it is stable across
// redeliveries of the same event, so retries of one failure count together
// while distinct events never share a counter.
func DeadLetterIdentity(bucket, key, sequencer string) string {
	return bucket + "/" + key + "/" + sequencer
}

// DeadLetterRedisKey hashes the identity into a bounded, byte-safe Redis key.
// Object keys can be long and contain any UTF-8; the raw identity would make
// keys unwieldy and force escaping rules into the key namespace. A SHA-256 is
// deterministic (retries of one event collide on the same counter, which is
// the whole point) and collision-free for our purposes.
func DeadLetterRedisKey(identity string) string {
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:])
}

// DeadLetter is one exhausted webhook event.
type DeadLetter struct {
	// DedupKey is the event identity (bucket+key+sequencer).
	DedupKey string
	// EventName is the S3 event name (routing: created vs removed).
	EventName string
	// Bucket and Key are the DECODED bucket name and object key, ready for
	// an operator to replay indexing without re-doing the decode step.
	Bucket string
	Key    string
	// SizeBytes and ETag are the ObjectCreated payload fields (0/"" for
	// deletions).
	SizeBytes int64
	ETag      string
	// ObservedAt is the payload's eventTime (zero value when absent); kept so
	// a replay preserves the original ordering semantics instead of adopting
	// the replay moment.
	ObservedAt time.Time
	// EventSeq is the payload's sequencer.
	EventSeq string
	// FailCount is how many indexing attempts failed before dead-lettering.
	FailCount int64
	// LastError is the final failed attempt's error, for triage.
	LastError string
	// Removed marks ObjectRemoved dead letters (d): a lost delete is a
	// "PG has / MinIO hasn't" divergence that reconciliation cannot self-heal
	// (deletes never advance the shard-activity signal), so the redrive flow
	// for these rows force-marks the affected shard active.
	Removed bool
}

// NewDBDeadLetterSink builds the webhook_dead_letters-backed dead-letter
// sink (production wiring).
func NewDBDeadLetterSink(store deadLetterStore) DeadLetterSink {
	return &dbDeadLetterSink{store: store}
}

// deadLetterStore is the DB side of DeadLetterSink (sqlc-generated surface).
// db.Queries.UpsertDeadLetterWrap satisfies it (sqlc's raw method returns the
// row too; the wrap drops it).
type deadLetterStore interface {
	UpsertDeadLetterWrap(ctx context.Context, arg db.UpsertDeadLetterParams) error
}

// dbDeadLetterSink adapts the sqlc store to DeadLetterSink.
type dbDeadLetterSink struct {
	store deadLetterStore
}

// DeadLetter persists one dead letter row (upsert by dedup key: a re-drowned
// event refreshes the row instead of duplicating it).
func (s *dbDeadLetterSink) DeadLetter(ctx context.Context, dl DeadLetter) error {
	return s.store.UpsertDeadLetterWrap(ctx, db.UpsertDeadLetterParams{
		DedupKey:   dl.DedupKey,
		EventName:  dl.EventName,
		Bucket:     dl.Bucket,
		Key:        dl.Key,
		SizeBytes:  dl.SizeBytes,
		Etag:       sql.NullString{String: dl.ETag, Valid: dl.ETag != ""},
		ObservedAt: sql.NullTime{Time: dl.ObservedAt, Valid: !dl.ObservedAt.IsZero()},
		EventSeq:   sql.NullString{String: dl.EventSeq, Valid: dl.EventSeq != ""},
		FailCount:  int32(dl.FailCount),
		LastError:  sql.NullString{String: dl.LastError, Valid: dl.LastError != ""},
		Active:     dl.Removed,
	})
}

// ShouldDeadLetter reports whether an event with the given failure count has
// exhausted the cap. Exported (and pure) so the boundary decision is a single
// testable line: strictly greater — exactly AT the cap the event is still
// retried, the dead letter happens on the first attempt PAST the cap.
func ShouldDeadLetter(count, limit int64) bool {
	return count > limit
}

// logDeadLetter emits the structured log line every dead letter must produce.
// The dead-letter row is the durable record; this is the operator's immediate
// signal, carrying the identity so it can be grepped against the table.
func logDeadLetter(logger *zap.Logger, dl DeadLetter) {
	logger.Error("minio event: dead-lettered after exhausting retry cap; the event will NOT be retried — operator redrive required (see webhook_dead_letters)",
		zap.String("dedup_key", dl.DedupKey),
		zap.String("event", dl.EventName),
		zap.String("bucket", dl.Bucket),
		zap.String("key", dl.Key),
		zap.Int64("fail_count", dl.FailCount),
		zap.String("last_error", dl.LastError),
		zap.Bool("removed", dl.Removed),
	)
}
