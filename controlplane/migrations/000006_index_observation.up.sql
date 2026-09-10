-- IC-2a: ordering metadata; defaults also backfill existing rows safely.
ALTER TABLE file_entries
    ADD COLUMN observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN source TEXT NOT NULL DEFAULT 'minio_event'
        CHECK (source IN ('agent', 'api', 'minio_event', 'audit')),
    ADD COLUMN event_seq TEXT,
    ADD COLUMN meta_incomplete BOOLEAN NOT NULL DEFAULT false;

-- Defaults above exist only to backfill legacy rows. Every writer must choose
-- its clock and provenance explicitly after this migration.
ALTER TABLE file_entries
    ALTER COLUMN observed_at DROP DEFAULT,
    ALTER COLUMN source DROP DEFAULT;
