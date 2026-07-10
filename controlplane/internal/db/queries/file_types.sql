-- name: ListFileTypes :many
SELECT id, org_id, name, description, created_by, created_at
FROM file_types
WHERE org_id = $1
ORDER BY name ASC;

-- name: GetFileTypeByID :one
SELECT id, org_id, name, description, created_by, created_at
FROM file_types
WHERE id = $1
LIMIT 1;

-- name: GetFileTypeByName :one
SELECT id, org_id, name, description, created_by, created_at
FROM file_types
WHERE org_id = $1 AND name = $2
LIMIT 1;

-- name: CreateFileType :one
INSERT INTO file_types (org_id, name, description, created_by)
VALUES ($1, $2, $3, $4)
RETURNING id, org_id, name, description, created_by, created_at;

-- name: UpdateFileType :one
UPDATE file_types
SET name        = $2,
    description = $3
WHERE id = $1
RETURNING id, org_id, name, description, created_by, created_at;

-- name: DeleteFileType :exec
DELETE FROM file_types WHERE id = $1;
