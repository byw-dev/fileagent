-- name: CreateCollectionRule :one
INSERT INTO collection_rules (
    org_id,
    agent_id,
    bucket_id,
    name,
    mode,
    base_path,
    path_pattern,
    dest_path_template,
    recursive,
    status,
    cron_expr,
    run_once_on_start,
    append_mode,
    metadata
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
)
RETURNING *;

-- name: GetCollectionRuleByID :one
SELECT * FROM collection_rules
WHERE id = $1
LIMIT 1;

-- name: ListCollectionRulesByAgent :many
SELECT * FROM collection_rules
WHERE agent_id = $1
ORDER BY created_at DESC;

-- name: UpdateCollectionRuleStatus :one
UPDATE collection_rules
SET status = $2,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteCollectionRule :exec
DELETE FROM collection_rules WHERE id = $1;
