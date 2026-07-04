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

-- name: MarkAgentOfflineIfOnline :execrows
-- Transition an agent to offline only when it is currently online. Returns the
-- number of rows affected (1 = actually transitioned, 0 = already offline or
-- gone), so a caller such as the offline sweeper can publish
-- events.agent.offline exactly once and avoid double-firing when the gRPC
-- disconnect path already marked the agent offline.
UPDATE agents
SET status = 'offline',
    updated_at = NOW()
WHERE id = $1 AND status = 'online';

-- name: MarkAgentOnlineIfOffline :execrows
-- Restore an agent to online when a heartbeat arrives but the persisted status is
-- offline (e.g. the offline sweeper's reconnect-race false positive). Constrained
-- to offline→online only — it must never resurrect terminal states such as
-- 'revoked' or 'pending'. Returns rows affected (1 = actually restored), so the
-- heartbeat handler publishes a corrective events.agent.online only when a
-- transition really happened (0 rows in steady state = no event spam).
UPDATE agents
SET status = 'online',
    updated_at = NOW()
WHERE id = $1 AND status = 'offline';

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
