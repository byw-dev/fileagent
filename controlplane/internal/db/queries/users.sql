-- name: GetUserByUsername :one
SELECT * FROM users
WHERE username = $1
  AND is_active = TRUE
LIMIT 1;

-- name: GetUserByID :one
SELECT * FROM users
WHERE id = $1
LIMIT 1;

-- name: ListUsers :many
SELECT * FROM users
WHERE org_id = $1
ORDER BY created_at DESC;

-- name: CreateUser :one
INSERT INTO users (
    org_id,
    username,
    email,
    password_hash,
    role
) VALUES (
    $1, $2, $3, $4, $5
)
RETURNING *;

-- name: UpdateUserPassword :exec
UPDATE users
SET password_hash = $2,
    updated_at = NOW()
WHERE id = $1;

-- name: UpdateUserActive :exec
UPDATE users
SET is_active = $2,
    updated_at = NOW()
WHERE id = $1;

-- name: UpdateUserLastLogin :exec
UPDATE users
SET last_login_at = NOW(),
    updated_at = NOW()
WHERE id = $1;

-- name: UpdateUser :one
UPDATE users
SET username   = $2,
    email      = $3,
    role       = $4,
    updated_at = NOW()
WHERE id = $1
RETURNING id, org_id, username, email, password_hash, role, is_active, last_login_at, created_at, updated_at;

-- name: DeleteUser :exec
DELETE FROM users WHERE id = $1;
