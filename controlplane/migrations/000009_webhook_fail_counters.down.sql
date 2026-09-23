ALTER TABLE webhook_dead_letters DROP COLUMN raw_truncated;

DROP TABLE IF EXISTS webhook_fail_counters;