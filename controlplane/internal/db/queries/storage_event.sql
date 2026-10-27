-- Storage-event-source adapter persistence (B3-PR3b, DECISIONS.md D-037
-- decisions D2/D4/D5): durable subscription cursor, pending-reconciliation
-- marks, the per-event DB clock read (decision D2) and the unknown-bucket row
-- count used by bucket-removed marks.

-- name: GetStorageEventCursor :one
SELECT declared_ts_ns FROM storage_event_cursors WHERE source_id = $1;

-- name: UpsertStorageEventCursor :exec
-- GREATEST keeps the upsert monotonic even if a caller races itself: a
-- persisted cursor may never regress (it is a completeness claim).
INSERT INTO storage_event_cursors (source_id, declared_ts_ns)
VALUES ($1, $2)
ON CONFLICT (source_id) DO UPDATE
    SET declared_ts_ns = GREATEST(storage_event_cursors.declared_ts_ns, EXCLUDED.declared_ts_ns),
        updated_at = now();

-- name: InsertStorageEventMark :exec
-- Write-only by design (PR3b): the consumer is L2 reconciliation (IC-13).
INSERT INTO storage_event_marks (source_id, reason, bucket, from_ts_ns, to_ts_ns, detail)
VALUES ($1, $2, sqlc.narg('bucket')::text, sqlc.narg('from_ts_ns')::bigint, sqlc.narg('to_ts_ns')::bigint, sqlc.narg('detail')::jsonb);

-- name: CountActiveFileEntriesByBucketName :one
-- Rows of a bucket that are not soft-deleted: the "ledger still knows this
-- bucket" signal a bucket-removed mark must carry (D-037 rule 4: an ops bypass
-- that deletes a bucket directory under a live ledger is an incident).
SELECT count(*) FROM file_entries fe
JOIN buckets b ON b.id = fe.bucket_id
WHERE b.name = $1 AND fe.status <> 'deleted';

-- name: SelectDBClockTimestamp :one
-- clock_timestamp(), NOT now(): inside a transaction now() is the transaction
-- start time, while the read-back adapter needs a fresh wall reading taken
-- BEFORE each S3 HEAD (decision D2 — observed_at must order read-back facts by
-- when the HEAD observed them). The ::timestamptz cast makes the result type
-- explicit for sqlc (a bare function call has no schema type to infer from).
SELECT clock_timestamp()::timestamptz;
