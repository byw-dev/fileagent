BEGIN;

ALTER TABLE collection_rules
    RENAME COLUMN source_path_template TO base_path;

ALTER TABLE collection_rules
    RENAME COLUMN file_glob TO path_pattern;

ALTER TABLE collection_rules
    RENAME COLUMN upload_path_template TO dest_path_template;

ALTER TABLE collection_rules
    RENAME COLUMN watch_recursive TO recursive;

ALTER TABLE collection_rules
    DROP COLUMN IF EXISTS watch_subdir_pattern;

COMMIT;
