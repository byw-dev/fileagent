-- IC-4a round-3 (S-1): the stale-cleanup runner (startup + periodic) filters
-- on updated_at < now() - TTL; without this index every sweep is a full scan
-- and the table's unbounded-growth fix would itself degrade the hot path.
CREATE INDEX IF NOT EXISTS idx_webhook_fail_counters_updated_at
    ON webhook_fail_counters (updated_at);
