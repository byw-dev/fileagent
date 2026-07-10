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
