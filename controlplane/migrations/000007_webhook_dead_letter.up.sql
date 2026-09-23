-- IC-4a (IC-BUG-6): dead-letter table for MinIO webhook events whose indexing
-- kept failing past the poison-pill retry cap.
--
-- Who redrives: the operator (human). This table is the intake for a manual /
-- scripted redrive flow — NOT an automatic retry loop. Every row is an event
-- MinIO will never resend (the handler answered 200 to free the queue head),
-- so until someone replays it the index stays missing that create/delete.
-- The documented redrive procedure lives in docs/design/consistency-and-ingest.md
-- §3.7 and system-design.md §6.5.
--
-- Columns mirror what Handle needs to replay an event by hand:
--   dedup_key       bucket+key+sequencer — the event's identity (a); replaying
--                   the same event overwrites nothing here, it upserts.
--   event_name      s3:ObjectCreated:* / s3:ObjectRemoved:* routing.
--   bucket / key    DECODED object key, ready for re-indexing.
--   size / etag     ObjectCreated payload for replay.
--   observed_at     eventTime from the payload; replays keep the original
--                   ordering semantics (minio_event source reads it).
--   event_seq       the payload's sequencer.
--   fail_count      how many indexing attempts failed before dead-lettering.
--   last_error      the error of the final failed attempt.
--   active          (d) force-unseal flag for ObjectRemoved dead letters:
--                   the lost delete is a "PG has / MinIO hasn't" divergence in
--                   the L3 direction, but deletes never advance
--                   object_keys.last_modified, so archived shards would stay
--                   sealed forever and the ghost rows would be invisible to
--                   all three reconciliation levels. An operator redriving a
--                   delete replays with the force-active step; the flag makes
--                   that step explicit and queryable instead of a code branch
--                   nobody remembers.
CREATE TABLE webhook_dead_letters (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    dedup_key    TEXT NOT NULL,
    event_name   TEXT NOT NULL,
    bucket       TEXT NOT NULL,
    key          TEXT NOT NULL,
    size_bytes   BIGINT NOT NULL DEFAULT 0,
    etag         TEXT,
    observed_at  TIMESTAMPTZ,
    event_seq    TEXT,
    fail_count   INT NOT NULL DEFAULT 0,
    last_error   TEXT,
    active       BOOLEAN NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (dedup_key)
);

CREATE INDEX idx_webhook_dead_letters_active
    ON webhook_dead_letters (active)
    WHERE active;
