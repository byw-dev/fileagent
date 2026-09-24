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

// MinWebhookFailLimit is the safety floor for the cap (S-2, PR #110 round-2
// review). Like the default, this is a DECLARED POLICY VALUE, not a derived
// requirement: 60 × ~3s ≈ 3 minutes of tolerated outage — enough that
// ordinary 1-2 retry-round blips are never dead-lettered, with no
// project-authoritative RTO to derive it from. Operators should raise it
// alongside WEBHOOK_FAIL_LIMIT for larger windows. Below the floor,
// config.Validate rejects the configuration at startup (never a silent
// fallback). ⚠️ Deployments that had explicitly configured 1–59 while this
// PR is un-merged will change from bootable to FATAL — intentional (see the
// PR description's configuration-constraints section).
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
// would stay blocked forever. The production store layers TWO persistent
// backends (round-2 rework, B-OLD-1/B-NEW-1): Redis as the fast path and
// PostgreSQL (webhook_fail_counters, migration 000009) as the durable
// fallback; with both down the handler fail-closes on 5xx (B2). The interface
// keeps the handler unit-testable without real backends.
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

// NewRedisWebhookFailStore builds the production failure-counter store
// (round-3 rework, B-1): **PostgreSQL is the authoritative monotonic
// counter**; Redis is only a cache/accelerator in front of it.
//
// Why (round-3 review): the previous max(redis, pg) merge treated counts
// from DISJOINT delivery epochs (Redis = before the outage, PG = during)
// as duplicates of each other and swallowed failures; the SELECT→SET→DELETE
// merge was also non-atomic and could return non-monotonic values.
//
// Model now: every failure atomically upserts the PG row (the single
// authoritative series, monotonic under PostgreSQL row locking — the
// previous round's review already verified this upsert has no lost
// update). Redis caches the value to keep the hot path off PG; a Redis
// cache miss (expiry, eviction, loss) simply re-reads PG — the series
// never regresses. The PG row is deleted ONLY after a durable verdict:
// successful indexing or a durably stored dead letter.
//
// If BOTH Redis and PG are unavailable, IncrFailCount errors and the
// handler fail-closes with 5xx (B2): the event stays in MinIO's queue.
func NewRedisWebhookFailStore(client *cache.Client, pg DBTX) WebhookFailStore {
	return &redisFailStore{client: client, pg: pg}
}

// DBTX is the minimal database interface the authoritative counter needs
// (satisfied by *db.Queries); an interface keeps the store unit-testable
// without a real DB.
type DBTX interface {
	IncrWebhookFailCounter(ctx context.Context, dedupKey string) (int64, error)
	GetWebhookFailCounter(ctx context.Context, dedupKey string) (int64, error)
	ClearWebhookFailCounter(ctx context.Context, dedupKey string) error
	DeleteStaleWebhookFailCounters(ctx context.Context, before time.Time) (int64, error)
}

// redisFailStore: Redis cache in front of the authoritative PG counter.
type redisFailStore struct {
	client *cache.Client
	pg     DBTX
}

// IncrFailCount advances the authoritative monotonic series: the PG row is
// atomically upserted (+1) on every failure; Redis caches the result with a
// sliding TTL. On Redis failure the count still advances via PG alone — the
// series is monotonic regardless of Redis state, and CP restarts / Redis
// data loss can never reset it.
func (s *redisFailStore) IncrFailCount(ctx context.Context, identity string) (int64, error) {
	// identity arrives RAW (bucket/key/sequencer); both backends use the
	// hashed form so keys stay bounded and byte-safe.
	dk := DeadLetterRedisKey(identity)
	pgCount, err := s.pg.IncrWebhookFailCounter(ctx, dk)
	if err != nil {
		// PG unavailable → B2 fail-closed territory. Propagate the error so
		// the handler answers 5xx; the event stays in MinIO's queue.
		return 0, err
	}
	// Best-effort cache write: a failed cache refresh is harmless (the next
	// read falls through to PG), so errors are not propagated.
	_ = s.client.Set(ctx, cache.WebhookFailCountKey(identity), pgCount, webhookFailCounterTTL)
	return pgCount, nil
}

// FailCount reads the cached value when present, otherwise the authoritative
// PG count. A cache miss never regresses the series.
func (s *redisFailStore) FailCount(ctx context.Context, identity string) (int64, error) {
	count, err := s.client.GetInt64(ctx, cache.WebhookFailCountKey(identity))
	if err == nil && count > 0 {
		return count, nil
	}
	pgCount, pgErr := s.pg.GetWebhookFailCounter(ctx, DeadLetterRedisKey(identity))
	if pgErr != nil {
		// No PG row = no failures recorded (not an error condition).
		return 0, nil
	}
	return pgCount, nil
}

// ClearFailCount drops the counter after a durable verdict (dead letter
// stored or delivery succeeded): the authoritative PG row plus the cache.
func (s *redisFailStore) ClearFailCount(ctx context.Context, identity string) error {
	pgErr := s.pg.ClearWebhookFailCounter(ctx, DeadLetterRedisKey(identity))
	if err := s.client.Del(ctx, cache.WebhookFailCountKey(identity)); err != nil {
		return err
	}
	return pgErr
}

// WebhookFailCounterTTL exposes the counter TTL for the cleanup runner
// (main.go) — the authoritative PG series and the Redis cache share one
// lifecycle value so the two backends age identically.
func WebhookFailCounterTTL() time.Duration { return webhookFailCounterTTL }

// CleanupStaleWebhookFailCounters deletes counter rows untouched for longer
// than ttl (S-1, round-3 review): the authoritative series lives in PG, so
// the lifecycle claim in migration 000009 (stale cleanup after 7d) must have
// a REAL caller — without it the table grows unboundedly with historical
// failure identities. Runs at startup and periodically (RunStaleCounterCleanup).
func CleanupStaleWebhookFailCounters(ctx context.Context, pg DBTX, ttl time.Duration) (int64, error) {
	return pg.DeleteStaleWebhookFailCounters(ctx, time.Now().Add(-ttl))
}

// RunStaleCounterCleanup sweeps stale counter rows immediately and then on a
// ticker until ctx is cancelled (S-1). interval should be coarse (the sweep
// is hygiene, not latency-sensitive); each round logs deletions so growth is
// observable in structured logs (no Prometheus — T4-1 deferred).
func RunStaleCounterCleanup(ctx context.Context, pg DBTX, ttl time.Duration, interval time.Duration, logger *zap.Logger) {
	sweep := func() {
		deleted, err := CleanupStaleWebhookFailCounters(ctx, pg, ttl)
		if err != nil {
			logger.Warn("webhook fail-counter stale sweep failed",
				zap.Error(err))
			return
		}
		if deleted > 0 {
			logger.Info("webhook fail-counter stale sweep",
				zap.Int64("deleted", deleted),
				zap.Duration("ttl", ttl))
		}
	}
	sweep()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sweep()
		}
	}
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

// MinWebhookParseCap is the safety floor for WEBHOOK_MAX_PARSE_BYTES
// (B-2-3, round-3 review): a cap near zero turns every normal notification
// into a permanent 5xx (self-inflicted config). 64KiB ≈ the dead-letter
// capture cap — a floor at the capture size keeps ordinary notifications
// parseable while still rejecting obviously self-harming values. Validated
// at startup by config.Validate (no silent fallback).
const MinWebhookParseCap int64 = 64 << 10

// maxDeadLetterPayloadBytes caps how much raw body is captured for the
// unparseable-payload dead letter (S2). Independent from the parse cap
// (B-NEW-2 item 4): large enough for any plausible single-record notification,
// smaller than the parse cap so a hostile 100MB garbage body cannot blow up a
// dead-letter row. Bodies larger than this are captured truncated, with the
// DeadLetter.Truncated flag set — the dedup hash is computed over the FULL
// body elsewhere, so identity is unaffected by the capture cap.
const maxDeadLetterPayloadBytes = 64 << 10

// MaxWebhookParseBytesForTest exposes the parse cap for tests (unexported
// consts can't be referenced from handler_test).
func MaxWebhookParseBytesForTest() int64 { return maxWebhookParseBytes }

// readBodyCapped reads the request body up to cap bytes and reports whether
// the body was LARGER than cap (detected by reading cap+1 bytes —
// io.LimitReader alone silently returns a prefix, which is the bug B-NEW-2
// closed). B-2-1 (round-3): the ENTIRE body is consumed and hashed — the
// returned hash always covers the full body even when oversized, so two
// payloads sharing only a prefix can never collide on one dedup key.
func readBodyCapped(r io.Reader, cap int64) (body []byte, oversized bool, fullHash string, err error) {
	hasher := sha256.New()
	tee := io.TeeReader(r, hasher)
	full, err := io.ReadAll(io.LimitReader(tee, cap+1))
	if err != nil {
		return nil, false, "", err
	}
	// Drain whatever the parser cap excluded so the hash sees the whole body.
	if _, err := io.Copy(io.Discard, tee); err != nil {
		return nil, false, "", err
	}
	hash := hex.EncodeToString(hasher.Sum(nil))
	if int64(len(full)) > cap {
		return full[:cap], true, hash, nil
	}
	return full, false, hash, nil
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
		DedupKey:     dl.DedupKey,
		EventName:    dl.EventName,
		Bucket:       dl.Bucket,
		Key:          dl.Key,
		SizeBytes:    dl.SizeBytes,
		Etag:         sql.NullString{String: dl.ETag, Valid: dl.ETag != ""},
		ObservedAt:   sql.NullTime{Time: dl.ObservedAt, Valid: !dl.ObservedAt.IsZero()},
		EventSeq:     sql.NullString{String: dl.EventSeq, Valid: dl.EventSeq != ""},
		FailCount:    int32(dl.FailCount),
		LastError:    sql.NullString{String: dl.LastError, Valid: dl.LastError != ""},
		Active:       dl.Removed,
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
