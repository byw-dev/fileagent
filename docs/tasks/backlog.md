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
| T4-5 | Agent 重命名 | 管理员可在 Web UI 为采集器设置自定义显示名；前置依赖：T3-6-BUG-A 已修复 | ⬜ |
| T4-6 | 采集规则原地编辑 | 在规则列表中增加编辑入口，复用三步创建 Wizard 并回填已有值；后端扩展 PUT 接口支持全字段更新 | ⬜ |

---

## T4-5 — Agent 重命名

**前置依赖**：T3-6-BUG-A 已修复（`agent_name` 路径模板注入正常后，重命名才有实际意义）  
**生效时机**：改名后 agent **重连**时生效（方案 B 的已知约束；重连前路径模板仍使用旧名）

### 子任务

| # | 模块 | 内容 |
|---|------|------|
| 1 | controlplane — DB | `agents` 表 `name` 字段无需改动（已存在）；新增 `UpdateAgentName(ctx, agentID, newName)` DB 方法 |
| 2 | controlplane — REST | `PATCH /api/v1/agents/:id`，body `{"name": "自定义名称"}`；权限：`super_admin`；校验：非空、长度 ≤ 64、无特殊字符；成功返回更新后的 agent 对象 |
| 3 | controlplane — 单元测试 | handler 正常路径 + 空名 422 + 超长 422 + 非管理员 403 |
| 4 | webui — Agent 详情 | 详情页标题区增加铅笔图标，点击弹出 Modal 输入新名称，调用 `PATCH` 接口，成功后刷新页面数据 |
| 5 | webui — 单元测试 | 重命名 Modal 的提交 / 取消 / 校验三条路径 |

### 验收标准

- [ ] 管理员在详情页修改 agent 名称后，列表页和详情页展示新名称
- [ ] Agent 重新连接后，干跑（Dry-Run）和实际上传路径中的 `{agent_name}` 使用新名称
- [ ] 旧名称期间已采集的文件路径不受影响（路径是写入时快照，不会追溯修改）
- [ ] 非管理员账号调用 `PATCH /api/v1/agents/:id` 返回 403

---

## T4-6 — 采集规则原地编辑

**背景**：当前 `PUT /api/v1/agents/:id/rules/:rid` 只支持 enable/disable（`status` 字段），规则内容无法原地修改，用户须删除重建。`updateRule` service 方法已存在，仅缺少后端全字段支持和 Web UI 编辑入口。

### 子任务

| # | 模块 | 内容 |
|---|------|------|
| 1 | controlplane — REST | 扩展 `updateRuleRequest`，支持全量规则字段（`name`、`base_path`、`path_pattern`、`dest_path_template`、`bucket_id`、`mode`、`cron_expr`、`recursive`、`append_mode`、`enabled`）；DB 新增 `UpdateCollectionRule` 方法；若规则为 active 且内容有变化则重新 dispatch |
| 2 | controlplane — 单元测试 | 全字段更新正常路径；仅 status 变更路径；规则不存在 404；非法字段 422 |
| 3 | webui — 规则列表 | 每行操作列增加「编辑」按钮，点击跳转或打开三步 Wizard |
| 4 | webui — RuleForm Wizard | 新增 `mode` prop（`"create"` \| `"edit"`）；edit 模式下回填已有值，标题改为「编辑采集规则」；Step 3 Dry-Run 测试为可选步骤（可跳过直接提交）；提交时调用 `updateRule` 而非 `createRule` |
| 5 | webui — 单元测试 | 回填值正确渲染；edit 模式提交调用 PUT；跳过 Dry-Run 直接提交 |

### 设计约束

| 约束 | 结论 | 理由 |
|------|------|------|
| agent 离线时是否允许编辑 | ✅ 允许 | 与创建规则对称：CP 更新 DB，agent 重连时经 `SyncRulesOnConnect` 自动同步；UI 提示"离线时编辑将在重连后生效" |
| 编辑前是否必须先 disable | ❌ 不需要 | agent 收到 `PushRuleCommand` 时内部执行 `stopRule` + 重启 watcher，热重载已内置；强制 disable 只增加操作步骤而无安全收益。编辑时正在上传的任务已写入 SQLite 队列（含旧路径），会按旧路径完成，不受影响 |

### 验收标准

- [ ] 编辑已有规则后，列表页和详情页展示新配置
- [ ] 编辑处于 active 状态且 agent 在线的规则后，agent 侧收到更新并热重载（无需手动 disable）
- [ ] agent 离线时编辑规则，重连后规则自动同步为新配置
- [ ] Step 3 Dry-Run 在 edit 模式下可跳过
- [ ] 编辑时规则 ID 不变，历史上传日志保持关联

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

## 前端重做实现（Half A）

**来源**：Claude Design 项目「前端页面重做计划」（2026-07-07，登录态私有）。设计规范与分页意图已落文档：
[`docs/design/webui-redesign.md`](../design/webui-redesign.md)。

**性质**：纯前端（webui），**无后端契约改动**——统一既有页面的视觉与交互。

**建议切分**（每页独立 PR + review，实现后对照规范与快照核验）：
1. **主题 + 骨架**：`webui/src/main.tsx` 注入 `theme={{ token, components }}`（§1 色彩/字号/尺寸）、
   208px 固定 Sider、统一「圆点 + 文字」状态徽标（升级 `AgentStatusBadge`）、7 条交互定则的通用件
   （480px 抽屉、危险确认弹窗、顶部单行筛选栏、骨架屏/空态/内联错误）。
2. **按页**：仪表盘 → 采集器（+ 规则三步抽屉 `2a–2d`）→ 文件（+ 详情抽屉 / 类型树）→ 文件类型 →
   事件规则（+ 投递历史）→ Bucket → 上传日志 → 登录 / 用户管理。映射见规范 §4。

## 元数据 / 数据集能力（Half B · epic）

**来源**：同上，round 6–7。提案文档：[`docs/design/webui-metadata-model.md`](../design/webui-metadata-model.md)。

**状态：提案，待产品拍板**（是否采纳混合模型 `6c`）。这是**跨模块 epic**（DB 迁移 + `proto` + CP REST +
SDK 消费），**非**前端小改。落地前置：拍板后先补 `DECISIONS.md` + `system-design.md` §3/§5 + 迁移与 proto
设计，再按 `7a`–`7d`（规则表单元数据步骤 / 文件页 faceted 筛选 / 标签词表 / 待确认取值队列）分期实现。

---

## 已知技术债（低优先级，Phase 4 可处理）

| 描述 | 来源 | 优先级 |
|------|------|--------|
| `auth.go OIDCCallback` 始终返回 501（设计文档标注为"预留扩展"，可接受） | phase3-readiness-audit.md B-7 | ⚪ P3 |
| `append_mode=tail` 断点续传的 SQLite schema migration（当前直接修改 schema const） | P3-P4 | ⚪ P3 |
| 前后端 JSON 契约应引入 OpenAPI spec 自动校验，避免再次出现 T3-2-FIX 类问题 | T3-2-FIX 根本原因复盘 | ⚪ P3 |
| Web UI 页面使用 MSW（Mock Service Worker）补充真实 API 格式的集成测试 | T3-2-FIX 根本原因复盘 | ⚪ P3 |
