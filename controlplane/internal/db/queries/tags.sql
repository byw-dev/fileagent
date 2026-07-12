-- name: ListTagKeys :many
SELECT id, org_id, key, label, value_controlled, required_at_collection,
       allow_path_var, system_reserved, created_at
FROM tag_keys
WHERE org_id = $1
ORDER BY key ASC;

-- name: GetTagKey :one
SELECT id, org_id, key, label, value_controlled, required_at_collection,
       allow_path_var, system_reserved, created_at
FROM tag_keys
WHERE org_id = $1 AND key = $2
LIMIT 1;

-- name: CreateTagKey :one
INSERT INTO tag_keys (org_id, key, label, value_controlled, required_at_collection, allow_path_var)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING id, org_id, key, label, value_controlled, required_at_collection,
          allow_path_var, system_reserved, created_at;

-- name: UpdateTagKey :one
UPDATE tag_keys
SET label = $3,
    value_controlled = $4,
    required_at_collection = $5,
    allow_path_var = $6
WHERE org_id = $1 AND key = $2
RETURNING id, org_id, key, label, value_controlled, required_at_collection,
          allow_path_var, system_reserved, created_at;

-- name: DeleteTagKey :execrows
DELETE FROM tag_keys WHERE org_id = $1 AND key = $2;

-- name: ListTagValues :many
SELECT id, tag_key_id, value, created_at
FROM tag_values
WHERE tag_key_id = $1
ORDER BY value ASC;

-- name: CreateTagValue :one
INSERT INTO tag_values (tag_key_id, value)
VALUES ($1, $2)
RETURNING id, tag_key_id, value, created_at;

-- name: DeleteTagValue :execrows
DELETE FROM tag_values WHERE id = $1 AND tag_key_id = $2;

-- name: ListPendingTagValues :many
-- LEFT JOIN + COALESCE so a corrupted/org-mismatched pending row (tag_key_id not
-- matching the org — not prevented by any composite FK) still surfaces for
-- operational cleanup with an empty key name, rather than being silently hidden.
-- The org-scoped ON clause keeps a cross-tenant key name from leaking.
SELECT p.id, p.tag_key_id, COALESCE(k.key, '') AS key, p.extracted_value, p.source,
       p.source_rule_id, p.hit_count, p.suggested_value, p.first_seen_at
FROM pending_tag_values p
LEFT JOIN tag_keys k ON k.id = p.tag_key_id AND k.org_id = p.org_id
WHERE p.org_id = $1 AND p.status = 'pending'
ORDER BY p.hit_count DESC, p.first_seen_at ASC;

-- name: GetPendingTagValue :one
SELECT id, org_id, tag_key_id, extracted_value, source, source_rule_id,
       hit_count, suggested_value, status, first_seen_at
FROM pending_tag_values
WHERE id = $1 AND org_id = $2
LIMIT 1;

-- name: CreateTagValueIfAbsent :execrows
-- Inserts only when the tag key belongs to the org, so a corrupted pending row
-- (tag_key_id pointing at another org's key) cannot promote a value cross-tenant.
INSERT INTO tag_values (tag_key_id, value)
SELECT k.id, $2 FROM tag_keys k WHERE k.id = $1 AND k.org_id = $3
ON CONFLICT (tag_key_id, value) DO NOTHING;

-- name: DeletePendingTagValue :execrows
DELETE FROM pending_tag_values WHERE id = $1 AND org_id = $2;

-- name: GetFileTagValue :one
SELECT value FROM file_tags WHERE file_entry_id = $1 AND key = $2 LIMIT 1;

-- name: SetFileTag :exec
INSERT INTO file_tags (file_entry_id, key, value, source)
VALUES ($1, $2, $3, $4)
ON CONFLICT (file_entry_id, key) DO UPDATE SET value = EXCLUDED.value, source = EXCLUDED.source;

-- name: DeleteFileTag :execrows
DELETE FROM file_tags WHERE file_entry_id = $1 AND key = $2;

-- name: TagValueExists :one
SELECT EXISTS (SELECT 1 FROM tag_values WHERE tag_key_id = $1 AND value = $2);

-- name: TagValueExistsInOrg :one
-- Like TagValueExists but joins tag_keys to require the key belongs to the org,
-- so a corrupted pending_tag_values row (tag_key_id pointing at another org's
-- key — not prevented by any composite FK) cannot validate a merge target
-- against another tenant's vocabulary.
SELECT EXISTS (
    SELECT 1 FROM tag_values tv
    JOIN tag_keys tk ON tk.id = tv.tag_key_id
    WHERE tv.tag_key_id = $1 AND tv.value = $2 AND tk.org_id = $3
);

-- name: FindSimilarTagValue :one
SELECT value FROM tag_values WHERE tag_key_id = $1 AND lower(value) = lower($2) LIMIT 1;

-- name: UpsertPendingTagValueManual :exec
-- Bump hit_count on repeat sightings, and backfill suggested_value if a
-- case-insensitive match was found this time but the existing queue row had none
-- (e.g. the matching tag_value was created after the row was first queued).
INSERT INTO pending_tag_values (org_id, tag_key_id, extracted_value, source, suggested_value)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (tag_key_id, extracted_value) DO UPDATE SET
    hit_count = pending_tag_values.hit_count + 1,
    suggested_value = COALESCE(pending_tag_values.suggested_value, EXCLUDED.suggested_value);

-- name: CreateTagAudit :exec
INSERT INTO tag_audit (org_id, file_entry_id, key, old_value, new_value, action, actor_user_id, source)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);
