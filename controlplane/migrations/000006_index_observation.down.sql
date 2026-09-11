ALTER TABLE file_entries
    DROP COLUMN meta_incomplete,
    DROP COLUMN event_seq,
    DROP COLUMN source,
    DROP COLUMN observed_at;
