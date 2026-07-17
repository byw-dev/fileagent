# backlog.md — 待规划任务

> 本文件记录尚未排期的低优先级任务。Agent 无需读取，仅供人工规划参考。

---

## Phase 4 — 完善与收尾（可并行）

**前置依赖**：Phase 3 全部完成

| ID | 任务 | 内容摘要 | 状态 |
|----|------|---------|------|
| T4-1 | 监控配置 | Prometheus 抓取端点 + Grafana Dashboard 模板 + 告警规则 + Loki/Promtail 日志采集 | ⬜ |
| T4-2 | CI 配置 | GitHub Actions Workflows：controlplane / agent / webui / sdk-python 四条流水线（lint + test + build） | ⬜ |
| T4-3 | 部署脚本与文档 | 迁移嵌入二进制（D-023）；Dockerfile + prod compose + Caddyfile；systemd service 文件；Windows NSSM 脚本；README 生产部署段；`docs/ops/` 部署+运维手册 | ✅ |
| T4-4 | Java SDK | OkHttp3 + Jackson + Lombok；JUnit 5 + MockWebServer；功能对齐 Python SDK | ⬜ |
| T4-5 | Agent 重命名 | 管理员可在 Web UI 为采集器设置自定义显示名 | ✅ 已作为 CC-8 交付（PR #58 / D-021） |
| T4-6 | 采集规则原地编辑 | 规则列表编辑入口 + 复用三步 Wizard 回填 + 后端 PUT 全字段更新 | ✅ 已作为 CC-9 交付（PR #56–#57 / D-020） |

---

## T4-5 — Agent 重命名 ✅ 已交付（CC-8）

已作为 **CC-8** 实现并合并：`PATCH /api/v1/agents/:id`（super_admin）+ sqlc `UpdateAgentName` + webui 详情页
重命名 Modal。见 PR **#58** / **D-021** 与 `docs/tasks/core-completeness.md` CC-8 行。原详细规格已随实现落地，不再在此维护。

---

## T4-6 — 采集规则原地编辑 ✅ 已交付（CC-9）

已作为 **CC-9** 实现并合并：`PUT .../rules/{rid}` 全字段双形态更新（含 active 热重载）+ webui RuleForm edit 模式。
见 PR **#56–#57** / **D-020** 与 `docs/tasks/core-completeness.md` CC-9 行。原详细规格已随实现落地，不再在此维护。

---

## Follow-up：生产 MinIO 经网关反代暴露（TLS）

**来源**：PR #61（T4-3）review。

**✅ 已完成（D-024）**：internal/public endpoint 拆分——`MINIO_PUBLIC_ENDPOINT` /
`MINIO_PUBLIC_USE_SSL`（缺省回落内网值），`STSManager.WithPublicEndpoint`，`main.go` 独立 presign
client；prod compose 固定 `MINIO_ENDPOINT=minio:9000`（内网）+ 必填 `MINIO_PUBLIC_ENDPOINT`。
CP↔MinIO 不再 hairpin。

**残留（未做，gateway 特定）**：生产把 MinIO 置于 TLS 网关之后（不直发宿主机），`MINIO_PUBLIC_ENDPOINT`
指向网关地址。这是 gateway 特定改动（网关未必是 Caddy），端点拆分已就绪、gateway 无关地支持任意终结代理；
待生产网关选型确定后落地（`deploy/caddy/Caddyfile` 加 `minio.<domain>` 反代站点或等价物）。

---

## 前端重做实现（Half A）→ ⏸️ 已暂停（WR-1 已合并，WR-2…10 让位元数据 track）

**来源**：Claude Design 项目「前端页面重做计划」（2026-07-07，登录态私有）。设计规范：
[`docs/design/webui-redesign.md`](../design/webui-redesign.md)。**性质**：纯前端（webui），无后端契约改动。

**WR-1 地基已合并**（PR #66/#67：主题 token / 暗色闪烁修复 / 固定侧栏 / `StatusBadge` / 时间 util / 共享外壳），
被元数据 track 的 MT-6 复用。**WR-2…WR-10 于 2026-07-10 暂停**（价值优先：先建元数据核心，见
[`metadata-phase1.md`](metadata-phase1.md)）。分期、验收清单、恢复方法均在权威追踪文件
[`webui-redesign-impl.md`](webui-redesign-impl.md)。实施采用**在现有基线上改造（非重写）**。

## 元数据 / 标签 / 数据集能力（epic · 已拍板 6c，D-025）→ Phase 1 已排期，见专项追踪文件

**来源**：Claude Design round 6–7；已拍板混合模型 `6c`（D-025 + 2026-07-10 补充）。**权威设计**：
[`docs/design/metadata-model.md`](../design/metadata-model.md)（Phase 1 工程设计 + Phase 2 留存）。

**Phase 1 · 受控标签**已从"待规划"升级为**当前选定 track**（2026-07-10 价值优先决策，WR-2…10 暂停让位）：
任务拆分 **MT-1…MT-6**、验收要点、执行纪律均落于权威追踪文件
[`metadata-phase1.md`](metadata-phase1.md)。起手 MT-1+MT-2 薄纵切。

**Phase 2 · 数据集注册表 + 衍生数据入口 + 血缘 run 模型（deferred）** —— 薄层不动文件表。
**设计已完整留存**于 `metadata-model.md` P2.1–P2.3（2026-07-10 修订：血缘从数据集级改为 run 模型；
衍生数据 = 未来 ETL 经 SDK/CP 注册，禁止直连 MinIO）；出现对账/SDK 订阅需求或 ETL 起建时再起，届时补
D- 子决策与迁移。

---

## 已知技术债（低优先级，Phase 4 可处理）

| 描述 | 来源 | 优先级 |
|------|------|--------|
| `auth.go OIDCCallback` 始终返回 501（设计文档标注为"预留扩展"，可接受） | phase3-readiness-audit.md B-7 | ⚪ P3 |
| `append_mode=tail` 断点续传的 SQLite schema migration（当前直接修改 schema const） | P3-P4 | ⚪ P3 |
| 前后端 JSON 契约应引入 OpenAPI spec 自动校验，避免再次出现 T3-2-FIX 类问题 | T3-2-FIX 根本原因复盘 | ⚪ P3 |
| Web UI 页面使用 MSW（Mock Service Worker）补充真实 API 格式的集成测试 | T3-2-FIX 根本原因复盘 | ⚪ P3 |

---

## file_types glob 规则管理（后端 + 前端）

**来源**：WR-2 落地（2026-07-14）发现——mockup 的 file_types「glob 规则编辑器（可排序 + 优先级 + 试匹配，3c）」
**无后端支撑**：`file_type_rules` 表仅被 indexer classifier 只读（`ListFileTypeRules`），靠迁移/种子存在，无任何 REST 端点；
WR 禁改后端，故从 WR-2 拆出。

**内容**（真有「配兜底分类规则」需求时再做）：
- CP：`file_type_rules` CRUD 端点（按 org，绑定 file_type，pattern/priority）+「试匹配」端点（给定 storage path 返回命中类型）+ sqlc 查询/迁移。
- webui：file_types 详情/编辑抽屉内嵌 glob 规则编辑器（可排序拖拽 + 优先级 + 试匹配预览）。

**优先级**：⚪ 低。D-025 已把 `file_types` 降级为**兜底粗分类**，变种维度走标签；兜底 glob 规则很少变、可迁移/种子管理，
UI 现价值低。触发信号：出现「需在 UI 配/调兜底 glob 规则」的真实需求。

---

## 文件列表 filename / 日期范围 服务端过滤（后端 + 前端）

**来源**：WR-4 落地（2026-07-17）发现——文件页一直提供「文件名搜索」+「日期范围」筛选控件，但 `GET /api/v1/files`
的 `RejectUnknownQuery` 允许清单只有 `cursor/limit/agent_id/bucket_id/file_type_id/status/tag`（`files.go:132`），
发 `filename`/`since`/`until` 会 **400**（预存 bug，非 WR-4 引入）。WR-4 已改为**不发这些参数、在已加载页（limit 100，
status/tag 服务端预筛后）内客户端过滤**，止血且不 400。

**内容**（需在大数据量下跨 100 条精确搜索时再做）：
- CP：`files.go` List 允许清单加 `filename`/`since`/`until`；`ListFileEntriesParams`/`CountFileEntriesFilter` 加字段；
  list+count SQL 加 `filename ILIKE` + `created_at`（或 `uploaded_at`）范围（参考 D-028 status 过滤同款做法）。
- webui：`Files/index.tsx` request 改回把 filename/date 交服务端，去掉客户端缓存过滤。

**优先级**：⚪ 低（当前列表 limit 100 + 客户端分页，客户端过滤与 UI 展示范围一致；无真实大数据集）。触发信号：单类目文件 > 100
且需按名/日期精确检索。

---

## 抽取 + 加固共享 formatBytes（跨页去重）

**来源**：WR-4 评审（2026-07-17）第七轮——`formatBytes` 在 ~6 个页面（Dashboard / Logs / Files/index /
FileDetailDrawer / Agents/Detail / Agents/Logs）各自复制；单位表 `['B','KB','MB','GB','TB']` 只到 TB，
理论上 ≥1PB 会输出 `X undefined`，且未防非有限/负输入。

**现状判断**：`size`/`bytes_transferred` 均为后端非负有限 int64，边缘采集单文件 ≥1PB 不现实，故当前无实际影响。

**内容**（若做 UI 一致性清理时）：抽 `src/utils/formatBytes.ts`（clamp 单位下标到最后一档 + 非有限/负值回退 `-`），
6 处改为引用，去掉重复定义。

**优先级**：⚪ 低（纯健壮性/去重，无真实触发场景）。
