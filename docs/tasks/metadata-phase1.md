# metadata-phase1.md — 元数据 6c Phase 1 实现（受控标签）

> **目的**：落地元数据/标签核心（D-025 混合模型 6c 的 Phase 1）。这是系统的**核心价值层**——
> 文件传 MinIO 是地基，给文件维护有语义的元数据/标签才是采集系统存在的意义。
> **权威设计**：[`docs/design/metadata-model.md`](../design/metadata-model.md)（P1.1–P1.7）。决策：`DECISIONS.md` D-025（含 2026-07-10 补充）。
> **范围**：纯 CP + webui + 迁移，**不改 agent / 不改 `proto/v1/agent.proto`**（打标在 CP 索引侧）。
> **来由**（2026-07-10 产品决策）：暂停 WR-2…WR-10 页面换皮（见 [`webui-redesign-impl.md`](webui-redesign-impl.md)），
> 价值优先转向本 track。UI 用 WR-1 已合并的 token 做到「够用一致」即可，不追像素。

---

## 任务清单

| ID | 模块 | 内容 | 验收要点 | 状态 |
|----|------|------|----------|------|
| **MT-1** | CP 迁移 + sqlc | 新增 5 表：`tag_keys` / `tag_values` / `file_tags` / `pending_tag_values` / `tag_audit`（DDL 见设计 P1.1；**只追加**，序号接现有最大号）+ `queries/*.sql` + `make generate` | 迁移 up/down 干净；sqlc 无漂移（CI `git status --porcelain`） | 🔄 PR（`000004`，+`GetFileTypeByName`；词表 CRUD sqlc 留 MT-4） |
| **MT-2** | CP indexer + API | 打标引擎第一斩：规则 `metadata.file_type` + `static_tags`（source=`rule_static`）在 `UploadResult` 索引时落 `file_tags`（幂等 on-conflict）；classifier 优先级=声明类型>glob；文件响应带 `tags`；`GET /api/v1/files` 增可重复 `tag` 参数（扩 `RejectUnknownQuery`，**保持 cursor 分页 V-2**） | **核心闭环**：建规则声明 static_tags → 上传 → 文件带标签 → `?tag=k:v` 能筛出；既有 glob 部署行为不变 | 🔄 PR（indexer 打标 + files `tags`/`tag` 筛选；单测覆盖；契约细节记 D-025 落地记录） |
| **MT-3** | CP indexer | 路径变量抽取（`path_tag_map`，复用 trollsift 变量 V-3，source=`path_var`）；受控 key 未登记值：`file_tags` 写原始值 + upsert `pending_tag_values`（hit_count / suggested_value）；幂等 | 未核准值不进筛选器/规则可选项；重复 UploadResult 不重复打标 | 🔄 PR（trollsift 反解 dest_path_template；path_var 不覆盖 static；pending hit_count 仅在新插入时+1；suggested=大小写近似；单测 + live SQL 校验） |
| **MT-4** | CP API | 词表 CRUD（`/api/v1/tag-keys` + `/api/v1/tag-keys/{key}/values`）；待确认队列 `GET /api/v1/pending-tag-values` + 动作端点 `POST /api/v1/pending-tag-values/{id}/approve`、`/{id}/merge`、`/{id}/reject`；**手动/批量打标**：`PUT /api/v1/files/{id}/tags` + `POST /api/v1/files/batch-tag`（source=`manual`，写 `tag_audit`，未登记值同样入队）。全部 super_admin 写 | merge 触发回溯改写 + 审计；批量打标按 `GET /files` 同款谓词圈选（服务历史数据人工补标） | 🔄 **拆分**（2026-07-11 决策：先做同步部分，merge/batch-tag 依赖 MT-5 worker）：**MT-4a** 词表 CRUD（tag-keys + values，super_admin 写）✅ PR #71；**MT-4b** 待确认队列 `GET /pending-tag-values` + approve/reject 🔄 PR；**MT-4c**（待）单文件 `PUT /files/{id}/tags`（manual + audit）；merge（回溯）+ batch-tag（异步）随 MT-5 worker 落 |
| **MT-5** | CP worker | 回溯打标：改规则声明 / 词表（新增取值、merge）触发后台任务（复用 `internal/worker` 模式），按 org/规则批量重算 `file_tags` + 写 `tag_audit` | 批量正确性 + 幂等；MT-4 的 merge 与 batch-tag 复用同一通道 | ⬜ |
| **MT-6** | webui | `7a` 规则表单「元数据」步骤 · `7b` 文件页 faceted 筛选 + 批量选中打标 · `7c` 设置·标签词表 · `7d` 待确认队列。**用 WR-1 token「够用一致」，不追 mockup 像素** | 四屏可用即收；live-e2e 走通 7a→上传→7b 筛选 | ⬜ |

> **薄纵切起手**：MT-1 + MT-2 合成第一刀（迁移 + static_tags 打标 + `?tag=` 筛选），先打通核心闭环再铺开。
> 真·e2e 打标需真实 agent（dev 环境无）：用单测保证引擎正确性 + 种子 `file_tags` 数据撑 UI 演示。

---

## 执行纪律

- 每个 MT-x（或合刀）**独立分支 off master + PR + Copilot review + 人工合并**（同 CC 冲刺惯例）。
- 涉及契约的改动（文件响应形状、新端点）先在 `DECISIONS.md` 记录（D-025 已覆盖大盘，增量补充即可）。
- 覆盖率不低于 CLAUDE.md 阈值；**改完真跑 live-e2e 再算完成**（空心陷阱教训）。
- 禁止手改 `*.sql.go`，一律 `make generate`。

## 不在本 track（Phase 2，设计已留存）

数据集注册表（对账 / SDK 订阅）、**衍生数据入口（SDK 经 CP 注册 + STS 直传）**、**血缘 run 模型**
（`lineage_runs` / `run_inputs`，文件级精度、1:1/1:n/n:1 统一表达）——设计见 `metadata-model.md` P2.1–P2.3。
触发信号：对账 / SDK 订阅需求出现，或 ETL 开始建设。ETL 在第一阶段建设完成前不存在（2026-07-10 确认）。
历史数据批量入库的补标需求由 **MT-4 批量打标 + MT-6 `7b`** 覆盖，不必等 Phase 2。
