-- IC-4a: dead-letter storage for MinIO webhook events that exhausted the
-- poison-pill retry cap (IC-BUG-6). Redrive is operator-driven; see the
-- migration header and docs/design/consistency-and-ingest.md §3.6.

-- name: UpsertDeadLetter :one
INSERT INTO webhook_dead_letters (
    dedup_key, event_name, bucket, key,
    size_bytes, etag, observed_at, event_seq,
    fail_count, last_error, active, raw_payload, raw_truncated
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12, $13
)
ON CONFLICT (dedup_key) DO UPDATE SET
    event_name  = EXCLUDED.event_name,
    bucket      = EXCLUDED.bucket,
    key         = EXCLUDED.key,
    size_bytes  = EXCLUDED.size_bytes,
    etag        = EXCLUDED.etag,
    observed_at = EXCLUDED.observed_at,
    event_seq   = EXCLUDED.event_seq,
    fail_count  = EXCLUDED.fail_count,
    last_error  = EXCLUDED.last_error,
    active      = EXCLUDED.active,
    raw_payload = EXCLUDED.raw_payload,
    raw_truncated = EXCLUDED.raw_truncated,
    updated_at  = now()
RETURNING *;

-- name: CountDeadLetters :one
SELECT count(*) FROM webhook_dead_letters;

-- name: DeleteDeadLetter :exec
DELETE FROM webhook_dead_letters WHERE dedup_key = $1;

-- IC-4a round-2 rework (B-OLD-1/B-NEW-1): PostgreSQL-backed failure counters —
-- the durable fallback when Redis is unavailable (CP restarts must not reset
-- the count; the previous in-process map violated the IC-4 (b) constraint).

-- name: IncrWebhookFailCounter :one
INSERT INTO webhook_fail_counters (dedup_key, count, updated_at)
VALUES ($1, 1, now())
ON CONFLICT (dedup_key) DO UPDATE SET
    count      = webhook_fail_counters.count + 1,
    updated_at = now()
RETURNING count;

-- name: GetWebhookFailCounter :one
SELECT count FROM webhook_fail_counters WHERE dedup_key = $1;

-- name: ClearWebhookFailCounter :exec
DELETE FROM webhook_fail_counters WHERE dedup_key = $1;

-- name: DeleteStaleWebhookFailCounters :execrows
DELETE FROM webhook_fail_counters WHERE updated_at < sqlc.arg('before')::timestamptz;
