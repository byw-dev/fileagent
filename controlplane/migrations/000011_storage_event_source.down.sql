-- Migration: 000011_storage_event_source (down)
-- Reverts exactly what the up migration added (D3+D4 revert).

DROP TABLE IF EXISTS storage_event_marks;
DROP TABLE IF EXISTS storage_event_cursors;

ALTER TABLE file_entries DROP CONSTRAINT file_entries_source_check;
ALTER TABLE file_entries ADD CONSTRAINT file_entries_source_check
    CHECK (source IN ('agent', 'api', 'minio_event', 'audit'));
