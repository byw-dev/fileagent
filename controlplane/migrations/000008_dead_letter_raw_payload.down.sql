ALTER TABLE webhook_dead_letters DROP COLUMN IF EXISTS raw_payload;

DROP INDEX IF EXISTS idx_webhook_dead_letters_active_created;
CREATE INDEX idx_webhook_dead_letters_active
    ON webhook_dead_letters (active)
    WHERE active;