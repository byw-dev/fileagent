# 文件元数据 / 标签 / 数据集 · 系统设计（6c 混合模型）

> **状态：已拍板 —— 采用混合模型 `6c`，分期实施（`DECISIONS.md` D-025）。**
> 本文件是该能力的**权威设计文档**（跨 DB / CP / webui / SDK）。**Phase 1（受控标签）为当前设计目标**；
> **Phase 2（数据集注册表）设计在此完整留存、暂缓实施**，以免信息丢失、下次从头再来。
>
> 权威层级（同 `CLAUDE.md`）：代码 > `DECISIONS.md` > `system-design.md` / 本文件。下文的 DDL / API 均为
> **设计草案**，实现时以真实迁移与 handler 为准（本轮不落代码）。
>
> **来源**：Claude Design「前端页面重做计划」round 6–7 · **UI 面（Half A）**：[`webui-redesign.md`](./webui-redesign.md)
> · **隐性契约**：[`contracts.md`](./contracts.md)

## 1. 背景与约束

采集数据存在多维度变种，仅靠 `file_types`（glob 归类）会「类型爆炸」。约束（Claude Design 讨论提炼）：

- **维度** = 厂家 / 型号 / 站点 / 传感器类型 / 数据版本 + **加工级别**（`raw → calibrated → derived`，即血缘）。
- **元数据必须在采集规则处声明**（源头即元数据）。
- **SDK 是主要消费方**（按维度拉取 / 订阅）。
- 需要**对账**（应到未到）与**历史回溯打标**。

## 2. 模型选型（round 6，已拍板 6c）

| 能力 | A 文件标签 (`6a`) | B 数据集 (`6b`) | **C 混合（选定 `6c`）** |
|------|:---:|:---:|:---:|
| SDK 按标签/维度拉取 | ✓ 拼查询 | ✓ 订阅 | ✓ 两者都有 |
| 对账（应到未到） | ✗ 无载体 | ✓ 一等能力 | ◐ Phase 2 补齐 |
| 血缘（raw→calibrated→…） | ✗ 弱 | ✓ 数据集级 | ◐ Phase 2 补齐 |
| 历史回溯打标 | ✓ | ◐ 需迁移 | ✓ Phase 1 就有 |
| 长尾 / 临时数据成本 | 低 | 高（先建模） | 低 |
| 后端 + UI 改动量 | 小 | 大 | 小 → 中（分期） |

**选 6c 的理由**：分期不返工。Phase 1（= 6a + 词表治理）即解决「类型爆炸」与元数据源头；对账 / 血缘 / SDK
契约留到 Phase 2 以**薄层**补齐，且长尾数据永远可只打标、不建数据集。详见 D-025。

## 3. 治理决策（`6d`，已确认）

- **取值治理：key 严格受控，value 受控可扩。** 管理员定义 key 词表与每个 key 的合法取值；规则里只能选。
  路径变量提取出**未知值**时进「**待确认**」队列由管理员核准（防 `tokyo / Tokyo / TYO` 漂移）；文件仍正常入库
  并携带原始值，但未核准的取值**不出现在筛选器与规则可选项**中。
- **`file_types` 降级为兜底**：保留做**粗分类**（`pressure / vibration / app_logs`，10~20 个、稳定）；变种维度
  一律走标签。规则**声明的类型优先**，glob `file_type_rules` 只兜没声明类型的旧数据。

---

# Phase 1 · 受控标签（当前设计目标）

关键前提（已核代码）：CP 已在索引阶段做服务端分类（`internal/indexer/classifier.go` 按 storage path 匹配
`file_type_rules`）。**打标同样落在 CP 侧**——agent 仍只按 `dest_path_template` 上传，CP 在处理 `UploadResult`
时打标。**Phase 1 不改 `proto/v1/agent.proto`、不改 agent。**

## P1.1 数据模型（DRAFT DDL，只追加）

```sql
-- 标签 key 词表（key 严格受控）
CREATE TABLE tag_keys (
    id                    UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id                UUID NOT NULL REFERENCES organizations(id),
    key                   VARCHAR(64) NOT NULL,          -- vendor / model / site / ...
    label                 VARCHAR(128) NOT NULL,
    value_controlled      BOOLEAN NOT NULL DEFAULT TRUE, -- 取值是否必须在 tag_values 内
    required_at_collection BOOLEAN NOT NULL DEFAULT FALSE,
    allow_path_var        BOOLEAN NOT NULL DEFAULT TRUE, -- 是否允许路径变量映射
    system_reserved       BOOLEAN NOT NULL DEFAULT FALSE,-- level 等系统保留，不可删
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, key)
);

-- 每个受控 key 的合法取值（value 受控可扩）
CREATE TABLE tag_values (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    tag_key_id  UUID NOT NULL REFERENCES tag_keys(id) ON DELETE CASCADE,
    value       VARCHAR(128) NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tag_key_id, value)
);

-- 文件 ↔ 标签（每 file × key 至多一值）
CREATE TABLE file_tags (
    file_entry_id UUID NOT NULL REFERENCES file_entries(id) ON DELETE CASCADE,
    key           VARCHAR(64) NOT NULL,                  -- 冗余 key 便于查询
    value         VARCHAR(128) NOT NULL,
    source        VARCHAR(16) NOT NULL,                  -- rule_static | path_var | manual | api
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (file_entry_id, key)
);
CREATE INDEX idx_file_tags_kv ON file_tags (key, value);   -- faceted 筛选

-- 待确认取值队列（路径变量提取到、但不在词表内）
CREATE TABLE pending_tag_values (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id         UUID NOT NULL REFERENCES organizations(id),
    tag_key_id     UUID NOT NULL REFERENCES tag_keys(id) ON DELETE CASCADE,
    extracted_value VARCHAR(128) NOT NULL,
    source         VARCHAR(24) NOT NULL,                 -- path_var | path_backfill
    source_rule_id UUID REFERENCES collection_rules(id),
    hit_count      INT NOT NULL DEFAULT 1,
    suggested_value VARCHAR(128),                        -- 疑似 = 既有值（大小写/近似）
    status         VARCHAR(16) NOT NULL DEFAULT 'pending',-- pending（唯一活跃态）
    first_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (tag_key_id, extracted_value)
);

-- 改标审计（人工 / API / 回溯任务）
CREATE TABLE tag_audit (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id        UUID NOT NULL REFERENCES organizations(id),
    file_entry_id UUID REFERENCES file_entries(id) ON DELETE SET NULL,
    key           VARCHAR(64) NOT NULL,
    old_value     VARCHAR(128),
    new_value     VARCHAR(128),
    action        VARCHAR(24) NOT NULL,                  -- set | merge | retag | clear
    actor_user_id UUID REFERENCES users(id),
    source        VARCHAR(16) NOT NULL,                  -- manual | api | retro
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

**规则声明**放进现有 `collection_rules.metadata JSONB`（**不改 rules 表结构**），约定形状：

```json
{ "file_type": "pressure",
  "static_tags": { "vendor": "omron" },
  "path_tag_map": { "site": "{site}" } }
```

> `file_type` = 声明的粗分类（优先于 glob）；`static_tags` = 规则级固定标签；`path_tag_map` = 路径变量→key，
> 变量名复用现有 trollsift 模板（`contracts.md` V-3），不另造机制。

## P1.2 打标引擎（CP `internal/indexer`）

在现有 classify / `UpsertFileEntry` 之后追加打标步骤：

1. 读规则 `metadata`：写 `static_tags`（source=`rule_static`）。
2. 按 `path_tag_map` 从 storage path 用 trollsift 变量抽值→写 `file_tags`（source=`path_var`）。
3. **未登记值**（受控 key 且值不在 `tag_values`）：`file_tags` 仍写原始值，同时 upsert `pending_tag_values`
   （`hit_count+1`，算 `suggested_value` 近似匹配）；该值在核准前不进筛选器 / 规则可选项。
4. **幂等**：`file_tags` 主键 `(file_entry_id,key)` on-conflict update；重复 `UploadResult` 不重复打标。
5. **回溯打标**：改规则 / 词表（新增取值、合并）触发后台任务（复用 `internal/worker` 模式），按 org/规则批量
   重算 `file_tags` 并写 `tag_audit`。

## P1.3 API 契约草案（REST，`/api/v1/...`）

- **词表**：`GET/POST/PATCH/DELETE /api/v1/tag-keys`；`GET/POST/DELETE /api/v1/tag-keys/{key}/values`
  （删除取值不动已打标文件，仅阻止后续采集再用；super_admin 写）。
- **待确认队列**：`GET /api/v1/pending-tag-values`；动作用 **path segment**（对齐现有约定，如
  `POST /api/v1/agents/:id/approve`，见 `controlplane/internal/api/router.go`）——
  `POST /api/v1/pending-tag-values/{id}/approve`、`/{id}/merge`、`/{id}/reject`（merge 触发回溯改写 + 审计）。
- **文件按标签筛选**：`GET /api/v1/files` 增**可重复** `tag` 参数——`?tag=site:tokyo&tag=level:raw`（多条 AND）。
  须把 `tag` 加入 `middleware.RejectUnknownQuery` allowlist（`handler/files.go:103`），**保持 cursor 分页与信封
  V-2**；沿用 `db.CountFileEntriesFilter` + list 查询，标签谓词经 `file_tags` join。
- **手动 / 批量打标**（2026-07-10 补充——初稿遗漏：`file_tags.source` 有 `manual`/`api` 且建了 `tag_audit`，
  却没有对应写入端点；且**历史数据批量入库后需人工补标**，这是最近期的真实场景）：
  - `PUT /api/v1/files/{id}/tags`：整体设置单文件标签（body `{ "tags": { "site": "tokyo", ... } }`；
    value 为 `null` 表示清除该 key）。source=`manual`，写 `tag_audit`（action=`set`/`clear`）。
  - `POST /api/v1/files/batch-tag`：按**与 `GET /files` 相同的筛选谓词**（含 `tag`）圈定文件集，统一
    set/clear 指定标签；异步走回溯打标 worker 同一通道，写 `tag_audit`。
  - 治理一致：受控 key 的未登记值同样进 `pending_tag_values`，不绕过词表。权限 super_admin（同词表）。
- 响应 / 错误信封沿用 V-2 / V-4；分页**不得**改为 offset（契约）。

## P1.4 `file_types` 语义变更

`classifier.go`：规则 `metadata.file_type` 声明的类型**优先**；仅当规则未声明时才回落 glob `file_type_rules`
（旧数据兜底）。附向后兼容说明：既有仅靠 glob 的部署行为不变。

## P1.5 webui（round 7，UI 意图见 `webui-redesign.md` §4）

`7a` 规则表单「元数据」步骤 · `7b` 文件页 faceted 筛选（`?tag=` 谓词）+ **批量选中打标**（服务于历史数据人工补标）
· `7c` 设置·标签词表 · `7d` 待确认队列。

## P1.6 迁移与测试

- **迁移**：新增 `0000NN_metadata_tags.up/down.sql`（**只追加**，序号接现有最大号；不改既有迁移）。
- **测试**：indexer 打标（静态 + 路径变量 + 未登记值入队 + 幂等）；classifier 优先级（声明类型 > glob）；
  词表 / 待确认 handler 正常 + 权限 + 校验路径；files `tag` 筛选与 cursor 分页共存；回溯任务批量正确性；
  手动 / 批量打标（set/clear、未登记值入队、审计落表、非 super_admin 403）。

## P1.7 子任务拆分（供实现 PR 消费）

迁移 → sqlc 查询 → indexer 打标引擎 → 词表/待确认/文件筛选/手动批量打标 API → 回溯打标 worker → webui `7a–7d`。
（每步独立 PR + review；改动契约前在 `DECISIONS.md` 记录。）
**实施追踪**：[`docs/tasks/metadata-phase1.md`](../tasks/metadata-phase1.md)（MT-1…MT-6）。

---

# Phase 2 · 数据集注册表 + 衍生数据/血缘（设计留存，暂缓实施）

> 不在当前实施范围。**完整设计在此留存**，Phase 1 落地后再起；届时补 `DECISIONS.md` 子决策 + 迁移。
> 血缘与衍生数据入口部分为 **2026-07-10 修订**（原设计只有数据集级血缘，见 D-025 补充记录）。

## P2.1 数据集注册表

**数据集 = 命名并固化的标签组合（谓词）**，如
`tokyo-pressure-raw ≔ file_type:pressure ∧ site:tokyo ∧ level:raw`。

- **薄层，不动文件表**：数据集只是查询的「视图 + 元信息」；`file_entries` / `file_tags` 零改动。
- **DRAFT 表**：`datasets`（id, org_id, name, predicate JSONB, owner_user_id, sdk_subscription_name,
  expected_arrivals JSONB[对账], created_at）。
- **能力**：对账（应到未到——按 predicate 期望 vs 实到 `file_tags`）、SDK 订阅名。
- **API（草案）**：`/api/v1/datasets` CRUD + `/api/v1/datasets/{id}/reconcile`；文件页「另存为数据集」由
  `7b` 的标签谓词直接固化。
- **SDK**（当前推后 T3-3/T4-4）：按数据集订阅 / 按标签拉取是主要消费场景。

## P2.2 衍生数据入口（2026-07-10 定向）

ETL（订正质控 / 计算衍生产品）是**未来的 SDK 消费方**：向 CP 查询有哪些数据 → 拉取 → 加工 → 回写。
**衍生文件禁止直连 MinIO 读写**（否则元数据失控）；写入路径**镜像 agent 的数据面模式**：

1. SDK 向 CP 申请**上传会话** → CP 发 STS 短期凭据（复用现有 STS manager 模式）；
2. SDK 直传 MinIO（文件内容照旧**不过 CP**，符合架构原则）；
3. SDK 回报**注册**：携带 tags / `level` / 所属 run（见 P2.3），source=`api`，受同一套词表治理
   （未登记值入 `pending_tag_values`）。

> minio-event 通知（D-014）只做对账兜底，不承担衍生数据打标——**注册即打标**。

## P2.3 血缘 = 加工批次（run）模型（2026-07-10 修订，取代原「数据集级血缘」）

原设计的 `dataset_lineage`（数据集 ← 数据集）表达不了「这个衍生文件从哪些文件来」；而逐条维护
文件↔文件边又过于繁琐（输入输出关系有 1:1 / 1:n / n:1）。**采用 run 模型**（参考 OpenLineage 的
job/run 思路）：血缘载体是**一次加工运行**，文件级精度可推导，ETL 侧零额外申报：

- SDK 拉取数据时开 **run 上下文**，自动记录「本次拉了哪些 `file_entry_id`」；
- ETL 注册输出文件时挂在同一 run 上；
- `输出文件 → run → 输入文件集合` 即文件级血缘，1:1 / 1:n / n:1 统一表达。

**DRAFT 表（纯追加，不动 Phase 1 任何表）**：
`lineage_runs`（id, org_id, name/job 标识, started_at, finished_at, created_by）、
`run_inputs`（run_id, file_entry_id）、输出文件注册时带 `run_id`（`file_entries` 加可空列或旁表，届时定）。

**触发 Phase 2 的信号**：出现明确的对账 / SDK 订阅需求，或 ETL 开始建设（需要衍生数据入口 + 血缘）。
在此之前只打标、不建数据集；历史数据入库用 Phase 1 的规则声明 + 批量打标即可覆盖。
