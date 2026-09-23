package handler

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"sync"
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

// NewRedisWebhookFailStore builds the Redis-backed failure-counter store
// (production wiring). B1: while Redis is unavailable the store falls back to
// a bounded in-process per-identity counter so the retry cap stays reachable
// even during a long Redis outage; entries expire and are dropped once Redis
// answers again. System-design appendix D pins the CP to a single instance,
// so process-local counters are safe as a degradation path (Redis remains the
// durable source once it recovers: every successful Redis op supersedes the
// fallback count for that identity).
func NewRedisWebhookFailStore(client *cache.Client) WebhookFailStore {
	return &redisFailStore{client: client, fallback: make(map[string]*fallbackCounter)}
}

// fallbackCounterTTL bounds each in-process fallback entry; fallbackCountLimit
// bounds how many identities the fallback tracks. Both are deliberately
// generous relative to the default cap window (~30 min): the fallback exists
// only to survive a Redis outage longer than that, and an unbounded map would
// trade one unbounded resource for another (B1's requirement is a BOUNDED
// escape path, not an unbounded one).
const (
	fallbackEntryTTL   = 2 * time.Hour
	fallbackCountLimit = 10000
)

// redisFailStore adapts *cache.Client to WebhookFailStore.
type redisFailStore struct {
	client *cache.Client

	// fallback is the B1 escape path: per-identity counters kept ONLY while
	// Redis is unreachable. Entries expire via fallbackEntryTTL and the map
	// is capped at fallbackCountLimit entries (oldest-expiry evicted).
	mu       sync.Mutex
	fallback map[string]*fallbackCounter
}

type fallbackCounter struct {
	count   int64
	expires time.Time
}

// incrFallback advances the in-process counter for one identity, evicting
// expired entries and enforcing the map cap.
func (s *redisFailStore) incrFallback(identity string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if len(s.fallback) >= fallbackCountLimit {
		// Evict expired entries first; if still full, drop the earliest
		// expiring entry (single-instance CP: the map is bounded by design).
		for k, v := range s.fallback {
			if v.expires.Before(now) {
				delete(s.fallback, k)
			}
		}
		if len(s.fallback) >= fallbackCountLimit {
			var oldestKey string
			var oldest time.Time
			for k, v := range s.fallback {
				if oldestKey == "" || v.expires.Before(oldest) {
					oldest = v.expires
					oldestKey = k
				}
			}
			if oldestKey != "" {
				delete(s.fallback, oldestKey)
			}
		}
	}
	e, ok := s.fallback[identity]
	if !ok || e.expires.Before(now) {
		s.fallback[identity] = &fallbackCounter{count: 1, expires: now.Add(fallbackEntryTTL)}
		return 1
	}
	e.count++
	e.expires = now.Add(fallbackEntryTTL)
	return e.count
}

// readFallback returns the in-process count for one identity (0 if absent).
func (s *redisFailStore) readFallback(identity string) int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.fallback[identity]; ok && e.expires.After(time.Now()) {
		return e.count
	}
	return 0
}

// dropFallback discards the in-process entry once Redis answers again.
func (s *redisFailStore) dropFallback(identity string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.fallback, identity)
}

// IncrFailCount increments the persistent failure counter for the event.
// Redis ops refresh the sliding TTL (S1); when Redis is down the bounded
// in-process fallback keeps the count advancing (B1) so the cap stays
// reachable and the feed cannot be stalled by a broken counter backend.
func (s *redisFailStore) IncrFailCount(ctx context.Context, identity string) (int64, error) {
	count, err := s.client.IncrRefreshTTL(ctx, cache.WebhookFailCountKey(identity), webhookFailCounterTTL)
	if err == nil {
		s.dropFallback(identity) // Redis recovered: it supersedes any local count
		return count, nil
	}
	return s.incrFallback(identity), nil
}

// FailCount reads the persistent failure counter for the event, falling back
// to the in-process count while Redis is unavailable.
func (s *redisFailStore) FailCount(ctx context.Context, identity string) (int64, error) {
	count, err := s.client.GetInt64(ctx, cache.WebhookFailCountKey(identity))
	if err == nil {
		return count, nil
	}
	return s.readFallback(identity), nil
}

// ClearFailCount drops the counter after a durable verdict (dead letter
// stored or delivery succeeded).
func (s *redisFailStore) ClearFailCount(ctx context.Context, identity string) error {
	s.dropFallback(identity)
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

// maxDeadLetterPayloadBytes caps how much raw body is captured for the
// unparseable-payload dead letter (S2). Large enough for any plausible MinIO
// notification; small enough that a hostile 100MB garbage body cannot blow up
// a dead-letter row. Bodies larger than this are truncated at the cap (the
// captured prefix is still enough to identify and hand-replay the source).
const maxDeadLetterPayloadBytes = 64 << 10

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
	// maxDeadLetterPayloadBytes. Nil for events whose structured fields were
	// decoded fine — those rows carry everything needed for replay in the
	// typed columns. Trade-off note (S2): the raw webhook body may contain
	// object keys (customer data by construction — this system's whole
	// purpose is storing them), no credentials; the DB already holds
	// storage_path values, so the marginal exposure is the payload envelope.
	RawPayload string
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
		RawPayload: sql.NullString{String: dl.RawPayload, Valid: dl.RawPayload != ""},
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
