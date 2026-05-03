-- name: ListFileTypeRulesByFileTypeID :many
SELECT id, file_type_id, path_pattern, priority, created_at
FROM file_type_rules
WHERE file_type_id = $1
ORDER BY priority DESC;

-- name: CreateFileTypeRule :one
INSERT INTO file_type_rules (file_type_id, path_pattern, priority)
VALUES ($1, $2, $3)
RETURNING id, file_type_id, path_pattern, priority, created_at;

-- name: DeleteFileTypeRulesByFileTypeID :exec
DELETE FROM file_type_rules WHERE file_type_id = $1;
