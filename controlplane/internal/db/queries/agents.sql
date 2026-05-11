-- name: CreateAgent :one
INSERT INTO agents (
    org_id,
    name,
    fingerprint,
    os_info,
    ip_address,
    status
) VALUES (
    $1, $2, $3, $4, $5, 'pending'
)
RETURNING *;

-- name: GetAgentByID :one
SELECT * FROM agents
WHERE id = $1
LIMIT 1;

-- name: GetAgentByFingerprint :one
SELECT * FROM agents
WHERE fingerprint = $1
LIMIT 1;

-- name: ListAgents :many
SELECT * FROM agents
WHERE org_id = $1
ORDER BY created_at DESC;

-- name: ListAgentsByStatus :many
SELECT * FROM agents
WHERE org_id = $1
  AND status = $2
ORDER BY created_at DESC;

-- name: UpdateAgentStatus :one
UPDATE agents
SET status = $2,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: UpdateAgentLastSeen :exec
UPDATE agents
SET last_seen_at = NOW(),
    updated_at = NOW()
WHERE id = $1;

-- name: UpdateAgentHeartbeat :exec
UPDATE agents
SET last_seen_at = NOW(),
    ip_address = $2,
    updated_at = NOW()
WHERE id = $1;

-- name: UpdateAgentAuthToken :one
UPDATE agents
SET auth_token_hash = $2,
    token_expires_at = $3,
    updated_at = NOW()
WHERE id = $1
RETURNING *;

-- name: DeleteAgent :exec
DELETE FROM agents WHERE id = $1;
