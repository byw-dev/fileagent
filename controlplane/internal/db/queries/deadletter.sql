-- IC-4a: dead-letter storage for MinIO webhook events that exhausted the
-- poison-pill retry cap (IC-BUG-6). Redrive is operator-driven; see the
-- migration header and docs/design/consistency-and-ingest.md §3.6.

-- name: UpsertDeadLetter :one
INSERT INTO webhook_dead_letters (
    dedup_key, event_name, bucket, key,
    size_bytes, etag, observed_at, event_seq,
    fail_count, last_error, active, raw_payload
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12
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
    updated_at  = now()
RETURNING *;

-- name: CountDeadLetters :one
SELECT count(*) FROM webhook_dead_letters;

-- name: DeleteDeadLetter :exec
DELETE FROM webhook_dead_letters WHERE dedup_key = $1;
