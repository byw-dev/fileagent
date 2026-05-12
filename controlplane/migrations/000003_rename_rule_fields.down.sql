BEGIN;

ALTER TABLE collection_rules
    ADD COLUMN IF NOT EXISTS watch_subdir_pattern VARCHAR(256);

ALTER TABLE collection_rules
    RENAME COLUMN recursive TO watch_recursive;

ALTER TABLE collection_rules
    RENAME COLUMN dest_path_template TO upload_path_template;

ALTER TABLE collection_rules
    RENAME COLUMN path_pattern TO file_glob;

ALTER TABLE collection_rules
    RENAME COLUMN base_path TO source_path_template;

COMMIT;
