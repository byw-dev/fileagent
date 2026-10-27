-- Migration: 000011_storage_event_source (down)
-- Reverts exactly what the up migration added (D3+D4 revert).
--
-- Guard (PR #135 rework 1, F4): once this migration is in use,
-- storage_readback rows exist and a plain down cannot honour them — the
-- restored CHECK would reject the rows (obscure failure) and silently
-- rewriting provenance is not acceptable. Refuse loudly with the remedy and
-- leave the schema untouched.
--
-- 精确回退流程（PR #135 返工 3 实测校准；执行顺序如下）：
--   0. 核实现场：schema 仍是 000011 的形态（storage_event_cursors /
--      storage_event_marks 表存在、file_entries 的 CHECK 仍含
--      'storage_readback'）；go-migrate 元数据为 version=10, dirty=true
--      （guard 拒绝 down 时 dirty 位已先于 down SQL 置位，失败后不清除——
--      实测见 TestMigration000011_DirtyRecoveryAfterRefusedDown）。
--   1. 处理 file_entries 里的 storage_readback 行（归档 / 改写为允许的
--      source / 确认丢弃）——这一步在 force 前后均可，但**必须在最终 down
--      之前完成**，否则本文件的 guard 会再次拒绝。
--   2. `migrate force 11`（清除 dirty 位；force 到 11 = 当前版本号本身，
--      不改变 schema）。
--   3. `migrate down 1`（执行本文件：guard 此时计数为 0，放行）。
-- 注意 CLAUDE.md「迁移只追加」约束：本文件随 000011 同属 PR #135 新增、
-- 尚未合并，故直接修订注释；合并后只追加不改。

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
