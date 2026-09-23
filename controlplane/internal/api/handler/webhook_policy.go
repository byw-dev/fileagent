package handler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
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
// Cap sizing (S1, PR #110 review — explicit assumptions, not hidden ones):
// there is no project-authoritative PostgreSQL RTO or maintenance-window
// number to derive the cap from. The default is therefore a DECLARED
// assumption: WEBHOOK_FAIL_LIMIT=600 with MinIO retrying ~3s/round tolerates
// roughly 30 minutes of continuous PG outage before an event is dead-lettered
// (601st failure — ShouldDeadLetter is strictly >). It must be raised for
// deployments whose RTO exceeds that; the hard requirement is
// limit > expected_outage_seconds ÷ 3. It is deliberately NOT tuned to
// "first transient hiccup": a cap of 1 would dead-letter on ordinary blips
// and defeat the retry mechanism.
//
// Two durable-verdict rules (B1/B2, PR #110 review) complete the state
// machine — see handleIndexFailure in events.go:
//   - a broken counter backend must not stall the feed forever: the store
//     falls back to a bounded in-process counter so the cap stays reachable;
//   - a failed dead-letter PERSIST must answer 5xx and keep the counter (a
//     200 after a failed persist silently loses the event; the dead-letter
//     sink shares PostgreSQL with the indexer, so long PG outages hit exactly
//     this path).
// The 200 is only ever emitted after the dead letter is durably stored.

// DefaultWebhookFailLimit is the poison-pill retry cap when
// WEBHOOK_FAIL_LIMIT is unset. With the strict `>` in ShouldDeadLetter the
// default dead-letters on the 601st failed delivery, i.e. ~30 minutes of
// tolerated outage at MinIO's ~3s retry cadence. This is an assumption (see
// the sizing note above), not a measured requirement — operators whose PG
// RTO or maintenance window differs MUST size WEBHOOK_FAIL_LIMIT accordingly.
const DefaultWebhookFailLimit int64 = 600

// MinWebhookFailLimit is the safety floor for the cap. Below it, ordinary
// transient blips (~1-2 retry rounds) would be dead-lettered on almost the
// first failure, which defeats the retry mechanism entirely. Configurations
// below the floor are rejected at startup by config.Validate — silently
// lowering the floor is not an option (S1).
const MinWebhookFailLimit int64 = 60

// webhookFailCounterTTL is the sliding TTL refreshed on every counter
// increment (S1). It must comfortably outlast (a) the longest tolerated
// outage (default cap ≈ 30 min) and (b) the operator redrive window — a dead
// letter may sit for hours or days before replay, and a re-drowned event
// refreshes its row on the same dedup key, so a fresh episode starting from
// a stale counter is acceptable; what must NOT happen is an ORPHAN key living
// forever when the success-path clear fails. 7 days bounds that leak.
const webhookFailCounterTTL = 7 * 24 * time.Hour

// WebhookFailStore is the persistent failure-counter backend (IC-4a (b)).
//
// The counter MUST be persistent — process memory alone would reset on a CP
// crash (the most likely companion of an indexing outage) or an ops restart,
// and a poison pill whose counter resets can never reach the cap: the feed
// would stay blocked forever. Redis backs this store in production, with a
// bounded in-process fallback for Redis outages (B1); the interface keeps the
// handler unit-testable without a real Redis.
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

// NewRedisWebhookFailStore builds the production failure-counter store:
// Redis fast path with a PostgreSQL-backed durable fallback (round-2 rework,
// B-OLD-1 / B-NEW-1). The previous in-process map is gone — the IC-4 card
// forbids in-process counting ("重启后计数归零，永远到不了上限"), and its
// capacity eviction could reset an existing identity's count. PostgreSQL is
// already a required dependency (the dead-letter sink lives there), so the
// fallback adds no new infrastructure: if PG is ALSO down, the handler's B2
// fail-closed already answers 5xx and nothing is lost.
//
// Merge semantics on Redis recovery (A-1): every successful Redis INCR takes
// max(redisCount, pgFallbackCount) and writes it back to Redis, so repeated
// Redis outages can never repeatedly reset the count and postpone the cap.
func NewRedisWebhookFailStore(client *cache.Client, pg DBTX) WebhookFailStore {
	return &redisFailStore{client: client, pg: pg}
}

// DBTX is the minimal database interface the PG fallback needs (satisfied by
// *db.Queries); an interface keeps the store unit-testable without a real DB.
type DBTX interface {
	IncrWebhookFailCounter(ctx context.Context, dedupKey string) (int64, error)
	GetWebhookFailCounter(ctx context.Context, dedupKey string) (int64, error)
	ClearWebhookFailCounter(ctx context.Context, dedupKey string) error
}

// redisFailStore: Redis fast path + PostgreSQL durable fallback.
type redisFailStore struct {
	client *cache.Client
	pg     DBTX
}

// IncrFailCount increments the persistent failure counter for the event:
// Redis (sliding 7d TTL) first; on Redis failure the count advances in
// PostgreSQL — durable across CP restarts (B-OLD-1) with no capacity
// eviction to reset a live identity (B-NEW-1).
func (s *redisFailStore) IncrFailCount(ctx context.Context, identity string) (int64, error) {
	count, err := s.client.IncrRefreshTTL(ctx, cache.WebhookFailCountKey(identity), webhookFailCounterTTL)
	if err == nil {
		return s.mergeRecovered(ctx, identity, count)
	}
	// Redis unavailable → PostgreSQL counter (persistent).
	pgCount, pgErr := s.pg.IncrWebhookFailCounter(ctx, DeadLetterRedisKey(identity))
	if pgErr != nil {
		// Redis AND PG down → B2 fail-closed territory. Propagate the error so
		// the handler answers 5xx; the event stays in MinIO's queue.
		return 0, pgErr
	}
	return pgCount, nil
}

// mergeRecovered implements A-1: on Redis recovery, never let the durable PG
// progress be silently reset — take the max of both and pin it in Redis.
// The PG row is cleared afterwards; it will be recreated only if Redis fails
// again, starting from the merged value.
func (s *redisFailStore) mergeRecovered(ctx context.Context, identity string, redisCount int64) (int64, error) {
	pgCount, err := s.pg.GetWebhookFailCounter(ctx, DeadLetterRedisKey(identity))
	if err != nil || pgCount <= redisCount {
		return redisCount, nil
	}
	// PG has more progress (Redis must have lost key/TTL): pin the larger
	// value so the cap is not postponed.
	merged := pgCount
	if setErr := s.client.Set(ctx, cache.WebhookFailCountKey(identity), merged, webhookFailCounterTTL); setErr != nil {
		// Couldn't pin in Redis — keep PG as the authority by leaving the row.
		return merged, nil
	}
	_ = s.pg.ClearWebhookFailCounter(ctx, DeadLetterRedisKey(identity))
	return merged, nil
}

// FailCount reads the persistent failure counter for the event, falling back
// to the PostgreSQL count while Redis is unavailable.
func (s *redisFailStore) FailCount(ctx context.Context, identity string) (int64, error) {
	count, err := s.client.GetInt64(ctx, cache.WebhookFailCountKey(identity))
	if err == nil {
		return count, nil
	}
	pgCount, pgErr := s.pg.GetWebhookFailCounter(ctx, DeadLetterRedisKey(identity))
	if pgErr != nil {
		return 0, pgErr
	}
	return pgCount, nil
}

// ClearFailCount drops the counter after a durable verdict (dead letter
// stored or delivery succeeded): both Redis and the PG fallback row.
func (s *redisFailStore) ClearFailCount(ctx context.Context, identity string) error {
	_ = s.pg.ClearWebhookFailCounter(ctx, DeadLetterRedisKey(identity))
	return s.client.Del(ctx, cache.WebhookFailCountKey(identity))
}

// DeadLetterSink is where exhausted events land (IC-4a (c)).
//
// Dead letters MUST land in a table, not only in logs: a log line is a silent
// loss (nothing to query, nothing to alert on, nothing to replay). Redrive is
// operator-driven; who redrives and how is documented on the table and in
// consistency-and-ingest.md §3.6 (dead-letter & redrive procedure).
type DeadLetterSink interface {
	// DeadLetter stores (or refreshes) a dead letter for the event.
	// The sink shares PostgreSQL with the indexer, so its failures are the
	// B2 case: callers must treat an error as "event not yet durably
	// recorded" and answer 5xx so MinIO redelivers.
	DeadLetter(ctx context.Context, dl DeadLetter) error
}

// DeadLetterIdentity builds the event's dedup key from the only stable parts
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

// maxWebhookParseBytes is the parse-time body cap (B-NEW-2, PR #110 round-2
// review). It must be far above any legitimate MinIO notification: an envelope
// with a few thousand records and metadata stays in the tens of KiB, so 8MiB
// leaves a >100x margin while still bounding a hostile body. Reading cap+1
// distinguishes oversized from malformed — a silently truncated prefix is
// never treated as a complete payload. Configurable via WEBHOOK_MAX_PARSE_BYTES
// for deployments with genuinely larger envelopes.
const maxWebhookParseBytes = 8 << 20

// maxDeadLetterPayloadBytes caps how much raw body is captured for the
// unparseable-payload dead letter (S2). Independent from the parse cap
// (B-NEW-2 item 4): large enough for any plausible single-record notification,
// smaller than the parse cap so a hostile 100MB garbage body cannot blow up a
// dead-letter row. Bodies larger than this are captured truncated, with the
// DeadLetter.Truncated flag set — the dedup hash is computed over the FULL
// body elsewhere, so identity is unaffected by the capture cap.
const maxDeadLetterPayloadBytes = 64 << 10

// readBodyCapped reads at most cap bytes and reports whether the body was
// LARGER than cap (detected by reading cap+1 bytes — io.LimitReader alone
// silently returns a prefix, which is exactly the bug B-NEW-2 closed).
func readBodyCapped(r io.Reader, cap int64) (body []byte, oversized bool, err error) {
	full, err := io.ReadAll(io.LimitReader(r, cap+1))
	if err != nil {
		return nil, false, err
	}
	if int64(len(full)) > cap {
		return full[:cap], true, nil
	}
	return full, false, nil
}

// captureRawPayload bounds the raw bytes stored in a dead letter: full body
// when it fits, otherwise a prefix (the Truncated flag marks it; the dedup
// hash is computed over the full body, so identity is unaffected).
func captureRawPayload(raw []byte) string {
	if int64(len(raw)) <= maxDeadLetterPayloadBytes {
		return string(raw)
	}
	return string(raw[:maxDeadLetterPayloadBytes])
}

// hashPayload derives the dedup key component for an unparseable payload:
// deterministic, bounded, and distinct per distinct bad payload (S2 — a fixed
// string would make different bad payloads overwrite each other's row).
func HashPayload(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

// DeadLetter is one exhausted webhook event.
type DeadLetter struct {
	// DedupKey is the event identity (bucket+key+sequencer; payload hash for
	// unparseable payloads — S2).
	DedupKey string
	// EventName is the S3 event name (routing: created vs removed).
	EventName string
	// Bucket and Key are the DECODED bucket name and object key, ready for
	// an operator to replay indexing without re-doing the decode step.
	// (For decode failures / unparseable payloads the raw form is kept — see
	// RawPayload.)
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
	// RawPayload carries the raw request bytes for payloads that could not be
	// parsed (S2) so an operator can replay them; bounded by
	// maxDeadLetterPayloadBytes (Truncated marks a prefix-only capture).
	// Sensitive-content note (S3, PR #110 round-2 review): a standard MinIO/S3
	// notification envelope can carry MORE than object keys — userIdentity
	// (principal ids), requestParameters (source IP, principal), request/host
	// IDs and other operational metadata. None are credentials, but they ARE
	// operational metadata: table access must follow the same DB access
	// controls as the rest of the index schema; rows are cleared on redrive
	// (DeleteDeadLetter) or by the operator; and the raw value must NEVER be
	// emitted into logs — log only the dedup key and its length.
	RawPayload string
	// Truncated marks dead letters whose stored RawPayload is only a prefix of
	// the real body (B-NEW-2 item 4: raw capture is decoupled from the parse
	// cap; the dedup hash is over the FULL body, so identity is unaffected).
	Truncated bool
	// Removed marks ObjectRemoved dead letters (d): a lost delete is a
	// "PG has / MinIO hasn't" divergence that reconciliation cannot self-heal
	// (deletes never advance the shard-activity signal), so the future redrive
	// flow must force-mark the affected shard active. ⚠️ IC-4a only WRITES
	// this flag; no consumer exists yet — the unseal action itself is tracked
	// as IC-12 acceptance (PR #110 review B4), NOT delivered by IC-4a.
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
		RawPayload:   sql.NullString{String: dl.RawPayload, Valid: dl.RawPayload != ""},
		RawTruncated: dl.Truncated,
	})
}

// ShouldDeadLetter reports whether an event with the given failure count has
// exhausted the cap. Exported (and pure) so the boundary decision is a single
// testable line: strictly greater — with limit=600 the dead letter happens on
// the 601st failed delivery (S1: the exact boundary is pinned by tests).
func ShouldDeadLetter(count, limit int64) bool {
	return count > limit
}

// logDeadLetter emits the structured log line for a durably stored dead
// letter. The dead-letter row is the durable record; this is the operator's
// immediate signal, carrying the identity so it can be grepped against the
// table. Called only after the sink reports success (B2).
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
