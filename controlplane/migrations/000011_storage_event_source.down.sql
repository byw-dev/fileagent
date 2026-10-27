-- Migration: 000011_storage_event_source (down)
-- Reverts exactly what the up migration added (D3+D4 revert).
--
-- Guard (PR #135 rework 1, F4): once this migration is in use,
-- storage_readback rows exist and a plain down cannot honour them — the
-- restored CHECK would reject the rows (obscure failure) and silently
-- rewriting provenance is not acceptable. Refuse loudly with the remedy and
-- leave the schema untouched.

DO $$
DECLARE readback_rows bigint;
BEGIN
    SELECT count(*) INTO readback_rows FROM file_entries WHERE source = 'storage_readback';
    IF readback_rows > 0 THEN
        RAISE EXCEPTION USING MESSAGE = format(
            '迁移 000011 已投入使用：file_entries 存在 %s 条 storage_readback 行，不能直接回滚（静默改写 provenance 或 CHECK 失败都不可接受）。请先人工处理这些行（归档 / 改写为允许的 source / 确认丢弃）后再执行 down。',
            readback_rows
        );
    END IF;
END
$$;

DROP TABLE IF EXISTS storage_event_marks;
DROP TABLE IF EXISTS storage_event_cursors;

ALTER TABLE file_entries DROP CONSTRAINT file_entries_source_check;
ALTER TABLE file_entries ADD CONSTRAINT file_entries_source_check
    CHECK (source IN ('agent', 'api', 'minio_event', 'audit'));
