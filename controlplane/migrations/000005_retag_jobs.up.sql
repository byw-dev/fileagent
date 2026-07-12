BEGIN;

-- ---------------------------------------------------------------------------
-- Retro-tagging job outbox (metadata 6c, Phase 1 — MT-5)
-- Design: docs/design/metadata-model.md §P1.2(5)
-- Append-only migration; no existing table is altered.
--
-- A single Control Plane instance drains this queue (worker.RetagWorker). The
-- merge / batch_tag / rule_retag triggers all enqueue here so a large file_tags
-- rewrite runs out of band of the request that triggered it. MT-5a implements
-- the "merge" kind; batch_tag / rule_retag reuse this same channel in MT-5b.
-- ---------------------------------------------------------------------------

CREATE TABLE retag_jobs (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id         UUID NOT NULL REFERENCES organizations(id),
    kind           VARCHAR(24) NOT NULL,                    -- merge | batch_tag | rule_retag
    spec           JSONB NOT NULL,                          -- kind-specific parameters
    status         VARCHAR(16) NOT NULL DEFAULT 'pending',  -- pending | running | done | failed
    attempts       INT NOT NULL DEFAULT 0,
    affected_count INT NOT NULL DEFAULT 0,                  -- file_tags rows rewritten
    last_error     TEXT,
    actor_user_id  UUID REFERENCES users(id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    started_at     TIMESTAMPTZ,
    finished_at    TIMESTAMPTZ
);

-- Claim scan: oldest pending first.
CREATE INDEX idx_retag_jobs_pending ON retag_jobs (created_at) WHERE status = 'pending';

COMMIT;
