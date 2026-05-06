-- name: ListEventRules :many
SELECT id, org_id, name, event_type, filter, action_type, action_config, enabled, created_by, created_at, updated_at
FROM event_rules
WHERE org_id = $1
ORDER BY created_at DESC;

-- name: GetEventRuleByID :one
SELECT id, org_id, name, event_type, filter, action_type, action_config, enabled, created_by, created_at, updated_at
FROM event_rules
WHERE id = $1
LIMIT 1;

-- name: CreateEventRule :one
INSERT INTO event_rules (org_id, name, event_type, filter, action_type, action_config, enabled, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, org_id, name, event_type, filter, action_type, action_config, enabled, created_by, created_at, updated_at;

-- name: UpdateEventRule :one
UPDATE event_rules
SET name          = $2,
    event_type    = $3,
    filter        = $4,
    action_type   = $5,
    action_config = $6,
    enabled       = $7,
    updated_at    = NOW()
WHERE id = $1
RETURNING id, org_id, name, event_type, filter, action_type, action_config, enabled, created_by, created_at, updated_at;

-- name: DeleteEventRule :exec
DELETE FROM event_rules WHERE id = $1;
