-- name: UpsertIndexedFile :one
INSERT INTO file_entries AS fe (
    org_id, file_type_id, agent_id, rule_id, bucket_id,
    storage_path, original_path, file_name, size_bytes,
    sha256, etag, content_type, file_mtime, status, uploaded_at,
    observed_at, source, event_seq, meta_incomplete
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    $10, $11, $12, $13, $14, $15,
    CASE WHEN sqlc.arg(source)::text IN ('agent', 'api') THEN now()
         ELSE sqlc.arg(observed_at)::timestamptz END,
    sqlc.arg(source), sqlc.narg(event_seq)::text, sqlc.arg(meta_incomplete)::boolean
)
ON CONFLICT (bucket_id, storage_path) DO UPDATE SET
    file_type_id = COALESCE(EXCLUDED.file_type_id, fe.file_type_id),
    agent_id = COALESCE(EXCLUDED.agent_id, fe.agent_id),
    rule_id = COALESCE(EXCLUDED.rule_id, fe.rule_id),
    original_path = COALESCE(EXCLUDED.original_path, fe.original_path),
    file_name = EXCLUDED.file_name,
    size_bytes = EXCLUDED.size_bytes,
    sha256 = COALESCE(EXCLUDED.sha256, fe.sha256),
    etag = COALESCE(EXCLUDED.etag, fe.etag),
    content_type = COALESCE(EXCLUDED.content_type, fe.content_type),
    file_mtime = COALESCE(EXCLUDED.file_mtime, fe.file_mtime),
    status = EXCLUDED.status,
    uploaded_at = COALESCE(EXCLUDED.uploaded_at, fe.uploaded_at),
    observed_at = EXCLUDED.observed_at,
    source = EXCLUDED.source,
    event_seq = EXCLUDED.event_seq,
    meta_incomplete = CASE WHEN EXCLUDED.source IN ('agent','api')
                           THEN EXCLUDED.meta_incomplete ELSE fe.meta_incomplete END,
    updated_at = now()
WHERE EXCLUDED.observed_at > fe.observed_at
 OR (EXCLUDED.observed_at = fe.observed_at
     AND (EXCLUDED.event_seq IS NULL OR fe.event_seq IS NULL
          OR lpad(EXCLUDED.event_seq,32,'0') >= lpad(fe.event_seq,32,'0')))
RETURNING fe.*;

-- name: GetIndexedFileByKey :one
SELECT * FROM file_entries WHERE bucket_id = $1 AND storage_path = $2;

-- name: DeleteIndexedFile :one
UPDATE file_entries AS fe
SET status = 'deleted', updated_at = now(),
    observed_at = $3, event_seq = $4, source = $5
WHERE bucket_id = $1 AND storage_path = $2
 AND ($3::timestamptz > fe.observed_at
      OR ($3::timestamptz = fe.observed_at
          AND ($4::text IS NULL OR fe.event_seq IS NULL
               OR lpad($4::text,32,'0') >= lpad(fe.event_seq,32,'0'))))
RETURNING fe.*;

-- name: GetIngestRuleInfo :one
SELECT agent_id, metadata, dest_path_template FROM collection_rules
WHERE id = $1 AND org_id = $2 LIMIT 1;
