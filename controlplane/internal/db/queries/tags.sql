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
SELECT p.id, p.tag_key_id, k.key, p.extracted_value, p.source, p.source_rule_id,
       p.hit_count, p.suggested_value, p.first_seen_at
FROM pending_tag_values p
JOIN tag_keys k ON k.id = p.tag_key_id AND k.org_id = p.org_id
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
