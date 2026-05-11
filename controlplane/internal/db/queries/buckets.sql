-- name: ListBuckets :many
SELECT id, org_id, name, description, policy_json, sts_role_arn, created_at, updated_at
FROM buckets
WHERE org_id = $1
ORDER BY name ASC;

-- name: GetBucketByID :one
SELECT id, org_id, name, description, policy_json, sts_role_arn, created_at, updated_at
FROM buckets
WHERE id = $1
LIMIT 1;

-- name: CreateBucket :one
INSERT INTO buckets (org_id, name, description, policy_json, sts_role_arn)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, org_id, name, description, policy_json, sts_role_arn, created_at, updated_at;

-- name: DeleteBucket :exec
DELETE FROM buckets WHERE id = $1;
