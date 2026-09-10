-- name: ListFileEntriesBase :many
SELECT id, org_id, file_type_id, agent_id, rule_id, bucket_id,
       storage_path, original_path, file_name, size_bytes,
       sha256, etag, content_type, file_mtime, status, uploaded_at,
       created_at, updated_at, observed_at, source, event_seq, meta_incomplete
FROM file_entries
WHERE org_id = $1
  AND ($2::UUID IS NULL OR agent_id = $2)
  AND ($3::UUID IS NULL OR bucket_id = $3)
  AND ($4::UUID IS NULL OR file_type_id = $4)
  AND ($5::file_status IS NULL OR status = $5)
  AND ($6::TIMESTAMPTZ IS NULL OR (created_at, id) < ($6, $7::UUID));

-- name: GetFileEntryByID :one
SELECT * FROM file_entries WHERE id=$1 LIMIT 1;
