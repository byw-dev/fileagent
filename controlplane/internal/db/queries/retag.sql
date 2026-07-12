-- name: EnqueueRetagJob :one
INSERT INTO retag_jobs (org_id, kind, spec, actor_user_id)
VALUES ($1, $2, $3, $4)
RETURNING id, org_id, kind, spec, status, attempts, affected_count,
          last_error, actor_user_id, created_at, started_at, finished_at;

-- name: GetRetagJob :one
SELECT id, org_id, kind, spec, status, attempts, affected_count,
       last_error, actor_user_id, created_at, started_at, finished_at
FROM retag_jobs
WHERE id = $1 AND org_id = $2;

-- name: ClaimNextRetagJob :one
-- Atomically claim the oldest pending job, marking it running and bumping
-- attempts. FOR UPDATE SKIP LOCKED keeps the claim safe if the single-instance
-- assumption ever changes; today it just guarantees one drain at a time.
UPDATE retag_jobs
SET status = 'running', started_at = NOW(), attempts = attempts + 1
WHERE id = (
    SELECT id FROM retag_jobs
    WHERE status = 'pending'
    ORDER BY created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING id, org_id, kind, spec, status, attempts, affected_count,
          last_error, actor_user_id, created_at, started_at, finished_at;

-- name: RequeueRunningRetagJobs :execrows
-- Single-instance CP crash recovery: on worker startup any job left in `running`
-- is a leftover from a stopped/crashed process (no worker is in-flight yet), so
-- reset it to pending for re-claim. Merge execution is idempotent, so re-running a
-- partially-completed job is safe. Without this a job claimed when the process
-- stopped would be stranded forever (ClaimNextRetagJob only selects `pending`).
UPDATE retag_jobs SET status = 'pending', started_at = NULL WHERE status = 'running';

-- name: MarkRetagJobDone :exec
UPDATE retag_jobs
SET status = 'done', affected_count = $2, finished_at = NOW()
WHERE id = $1;

-- name: MarkRetagJobFailed :exec
UPDATE retag_jobs
SET status = 'failed', last_error = $2, finished_at = NOW()
WHERE id = $1;

-- name: MergeTagValue :execrows
-- Fold every file_tags occurrence of @from_value into the canonical @to_value
-- for (org, tag key), writing one tag_audit row (action=merge, source=retro)
-- per rewritten file. The key is resolved from tag_keys so org ownership is
-- enforced (a corrupted spec cannot rewrite another org's tags). Idempotent: a
-- re-run finds no rows still holding from_value and rewrites nothing.
WITH tk AS (
    SELECT key FROM tag_keys
    WHERE id = sqlc.arg(tag_key_id)::uuid AND org_id = sqlc.arg(org_id)::uuid
),
updated AS (
    UPDATE file_tags ft
    SET value = sqlc.arg(to_value)::varchar
    FROM file_entries fe, tk
    WHERE ft.file_entry_id = fe.id
      AND fe.org_id = sqlc.arg(org_id)::uuid
      AND ft.key = tk.key
      AND ft.value = sqlc.arg(from_value)::varchar
    RETURNING ft.file_entry_id, ft.key
)
INSERT INTO tag_audit (org_id, file_entry_id, key, old_value, new_value,
                       action, actor_user_id, source)
SELECT sqlc.arg(org_id)::uuid, u.file_entry_id, u.key,
       sqlc.arg(from_value)::varchar, sqlc.arg(to_value)::varchar,
       'merge', sqlc.narg(actor_user_id)::uuid, 'retro'
FROM updated u;
