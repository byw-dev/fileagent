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
