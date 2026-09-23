-- IC-4a rework (PR #110 review S2 + A1):
--
-- S2: dead letters for payloads that cannot be parsed must carry the raw
-- request bytes so an operator can identify and replay them. Without this,
-- the unparseable dead letter had only metadata and was not replayable.
-- Size is bounded by the handler (maxDeadLetterPayloadBytes = 64KiB) — a
-- hostile oversized body is captured truncated. Trade-off (S2): the raw body
-- contains object keys (customer data by construction — this system's whole
-- purpose is storing keys); it contains no credentials.
--
-- A1: the old partial index keyed on (active) is useless — inside the partial
-- set the key is constant true, and no query consumes it. The documented
-- redrive procedure processes rows oldest-first (§3.6), so the index matches
-- that future query shape: created_at ordering over active rows only.
DROP INDEX IF EXISTS idx_webhook_dead_letters_active;
CREATE INDEX idx_webhook_dead_letters_active_created
    ON webhook_dead_letters (created_at)
    WHERE active;

ALTER TABLE webhook_dead_letters
    ADD COLUMN raw_payload TEXT;