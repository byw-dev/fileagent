# IC-2a PoC 决策闸门

真实 PostgreSQL 15，2026-09-10；生产 sqlc 查询 + 生产回查方法，测试 DBTX 层单分支变异。
测试在唯一 schema 的事务内执行真实迁移（去除迁移自己的 BEGIN/COMMIT，由测试统一回滚），
在 000006 前插入旧行，验证已有数据的 NOT NULL 回填。

## 新迁移 up

```sql
-- IC-2a: ordering metadata; defaults also backfill existing rows safely.
ALTER TABLE file_entries
    ADD COLUMN observed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    ADD COLUMN source TEXT NOT NULL DEFAULT 'minio_event'
        CHECK (source IN ('agent', 'api', 'minio_event', 'audit')),
    ADD COLUMN event_seq TEXT,
    ADD COLUMN meta_incomplete BOOLEAN NOT NULL DEFAULT false;
```

## 新迁移 down

```sql
ALTER TABLE file_entries
    DROP COLUMN meta_incomplete,
    DROP COLUMN event_seq,
    DROP COLUMN source,
    DROP COLUMN observed_at;
```

## 完整 upsert / 回查 / 软删除 SQL

```sql
-- name: UpsertIndexedFile :one
INSERT INTO file_entries AS fe (
    org_id, file_type_id, agent_id, rule_id, bucket_id,
    storage_path, original_path, file_name, size_bytes,
    sha256, etag, content_type, file_mtime, status, uploaded_at,
    observed_at, source, event_seq, meta_incomplete
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    $10, $11, $12, $13, $14, $15,
    CASE WHEN sqlc.arg(source)::text IN ('agent', 'api') THEN now()
         ELSE sqlc.arg(observed_at)::timestamptz END,
    sqlc.arg(source), sqlc.narg(event_seq)::text, sqlc.arg(meta_incomplete)::boolean
)
ON CONFLICT (bucket_id, storage_path) DO UPDATE SET
    file_type_id = COALESCE(EXCLUDED.file_type_id, fe.file_type_id),
    agent_id = COALESCE(EXCLUDED.agent_id, fe.agent_id),
    rule_id = COALESCE(EXCLUDED.rule_id, fe.rule_id),
    original_path = COALESCE(EXCLUDED.original_path, fe.original_path),
    file_name = EXCLUDED.file_name,
    size_bytes = EXCLUDED.size_bytes,
    sha256 = COALESCE(EXCLUDED.sha256, fe.sha256),
    etag = COALESCE(EXCLUDED.etag, fe.etag),
    content_type = COALESCE(EXCLUDED.content_type, fe.content_type),
    file_mtime = COALESCE(EXCLUDED.file_mtime, fe.file_mtime),
    status = EXCLUDED.status,
    uploaded_at = COALESCE(EXCLUDED.uploaded_at, fe.uploaded_at),
    observed_at = EXCLUDED.observed_at,
    source = EXCLUDED.source,
    event_seq = EXCLUDED.event_seq,
    meta_incomplete = fe.meta_incomplete OR EXCLUDED.meta_incomplete,
    updated_at = now()
WHERE EXCLUDED.observed_at > fe.observed_at
 OR (EXCLUDED.observed_at = fe.observed_at
     AND (EXCLUDED.event_seq IS NULL OR fe.event_seq IS NULL
          OR lpad(EXCLUDED.event_seq,32,'0') >= lpad(fe.event_seq,32,'0')))
RETURNING fe.*;

-- name: GetIndexedFileByKey :one
SELECT * FROM file_entries WHERE bucket_id = $1 AND storage_path = $2;

-- name: DeleteIndexedFile :one
UPDATE file_entries AS fe
SET status = 'deleted', updated_at = now(),
    observed_at = $3, event_seq = $4, source = 'minio_event'
WHERE bucket_id = $1 AND storage_path = $2
 AND ($3::timestamptz > fe.observed_at
      OR ($3::timestamptz = fe.observed_at
          AND ($4::text IS NULL OR fe.event_seq IS NULL
               OR lpad($4::text,32,'0') >= lpad(fe.event_seq,32,'0'))))
RETURNING fe.*;
```

## 真实输出

| 变异 | A1 | A2 | A3′ | P2 | A4b | LPAD |
|---|---|---|---|---|---|---|
| baseline | PASS | PASS | PASS | PASS | PASS | PASS |
| no_guard | FAIL | PASS | FAIL | PASS | PASS | FAIL |
| no_greater | PASS | FAIL | PASS | PASS | PASS | PASS |
| no_equal | PASS | PASS | FAIL | FAIL | PASS | FAIL |
| strict_null | PASS | PASS | PASS | FAIL | PASS | PASS |
| no_delete_advance | PASS | PASS | FAIL | PASS | PASS | FAIL |
| no_fallback | FAIL | PASS | FAIL | PASS | FAIL | FAIL |
| no_lpad | PASS | PASS | PASS | PASS | PASS | FAIL |

`go test -tags=integration ./controlplane/internal/db -run TestObservationMutationMatrix -v`：PASS，0.723s。
TEST_DATABASE_URL 指向 dev PostgreSQL，凭据不写入报告。
FAIL 表示用例成功杀死变异；整个测试断言此矩阵，因此最终测试通过。

## ErrNoRows 方案

选「调用方回查 + suppressed=true」：`db.Queries.UpsertObservedFile` 调生成的 upsert，
遇 sql.ErrNoRows 后按 bucket/path 回查并返回现有行及 suppressed=true。
后续 indexer 接线必须在 suppressed 时跳过标签与 NATS 副作用，仅记抑制计数。
软删除仍允许不存在行 no-op，不创建墓碑；推进字段不以 status!=deleted 限制，保证连续删除也推进围栏。

## 状态

仅完成 ⓪；等待协调者闸门批准，尚未开始 ①–⑧ 接线与 live 验收。
