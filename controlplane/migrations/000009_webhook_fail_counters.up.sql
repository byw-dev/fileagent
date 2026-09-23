-- IC-4a round-2 rework (PR #110 review, B-OLD-1 / B-NEW-1):
-- PostgreSQL-backed failure counters for the webhook poison-pill state
-- machine, replacing the previous in-process fallback map.
--
-- Layering after this change:
--   Redis available            → Redis counter (fast path, unchanged)
--   Redis unavailable          → PostgreSQL counter (durable, survives CP restart)
--   Redis AND PG unavailable   → B2 fail-closed: 5xx, event stays in MinIO's queue
--
-- Why a separate table (vs reusing webhook_dead_letters): the counter is a
-- hot, tiny, constantly-churning row per failing event; the dead-letter table
-- is a cold, operator-facing record. Different lifecycles, different hygiene.
--
-- Expiry: rows are tombstoned by updated_at (sliding, refreshed on every
-- increment by the ON CONFLICT clause); a periodic/launch-time cleanup deletes
-- rows older than webhookFailCounterTTL (7d, mirroring the Redis TTL). No
-- capacity eviction exists — PostgreSQL rows are the durable fallback, and
-- evicting a live count would re-open B-NEW-1.
CREATE TABLE IF NOT EXISTS webhook_fail_counters (
    dedup_key  TEXT PRIMARY KEY,
    count      BIGINT NOT NULL DEFAULT 0,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- B-NEW-2 item 4: raw capture in webhook_dead_letters is decoupled from the
-- parse cap and may store only a prefix; the flag tells the operator the
-- stored bytes are incomplete (the dedup hash is over the full body).
ALTER TABLE webhook_dead_letters
    ADD COLUMN raw_truncated BOOLEAN NOT NULL DEFAULT FALSE;