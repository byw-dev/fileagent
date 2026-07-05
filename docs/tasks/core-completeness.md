# core-completeness.md — 核心模块补完备清单

> **目的**：止血冲刺（P0+P1）收官后，聚焦 **webui + CP + Agent** 三个核心模块的剩余缺口。
> **来源**：`docs/reports/design-gap-analysis/`（01 CP / 02 Agent / 03 webui）。
> **优先级原则**：**空心陷阱 / 数据安全优先**——最侵蚀信任的是"看起来完成实则不触发"的功能。
> **不在本清单**（已按产品决策推后，见文末）：SDK、Prometheus 监控、契约工具化、结构性文档重构。

---

## Tier A — 空心陷阱 / 数据安全（先做）

| ID | 模块 | 缺口 | 现状（已核实） | 来源 |
|----|------|------|---------------|------|
| **CC-1** ✅ | CP + webui | ~~`file_deleted` 事件死配置~~ | **已修复（PR #47 / D-017）**：新增 `IndexDeletion` + `publishFileDeleted`（发布 `events.file.deleted`）；webhook 按 `ObjectCreated:*` / `ObjectRemoved:*` 路由，删除不再当成上传索引 | 01 §4 |
| **CC-2** ✅ | Agent | ~~`queue_max_size` 未强制~~ | **已修复（PR #48）**：`queue.CountActive` + `DeleteOldestEvictable` + `executor.enforceCapacity`——入队后回收、驱逐最旧 pending/failed（running 不驱逐）、排除刚入队的新任务；design §4.6 已更新 | 02 §1/§4 |

## Tier B — API / 存储健壮性（可打包）

| ID | 模块 | 缺口 | 现状 | 来源 |
|----|------|------|------|------|
| **CC-3** ⏸️ | CP | Bucket 创建后未设 Policy + `tmp-uploads` 无 Lifecycle | **已推后（低价值，2026-07-04）**：`tmp-uploads` 全代码库未接入（agent 直传 rule 目标 bucket，无 staging/ETL），其 lifecycle 当前无对象可清；bucket policy 对本系统冗余（MinIO bucket 默认私有，访问全走 STS/presigned IAM，与 bucket policy 无关）。待真有 staging→promote workflow 或多租户共享 bucket 场景再做。原缺口：设计 §6.6 要求 CP 创建 bucket 后自动 `SetBucketPolicy` + `tmp-uploads` 7 天 Lifecycle，CP 运行时路径皆无（init 脚本那步 e2e 亦报错，06 D-1） | 01 §5 |
| **CC-4** ✅ | CP | ~~无 API 限流~~ | **已修复（本 PR）**：`/api/v1/*` 按用户固定窗口限流——`cache.IncrWithWindow`（Lua 原子 INCR+EXPIRE）计数 `ratelimit:api:{user_id}`（TTL 60s），`middleware.RateLimit` 在 JWT 后按 `sub` 计数，超限 `429 RATE_LIMITED`+`Retry-After`+`X-RateLimit-*` 头。上限 `API_RATE_LIMIT_PER_MINUTE`（默认 600，`<=0` 关闭）。失败开放。设计 §5.1/§5.11 + 附录 C 已更新（D-018） | 01 §1 |
| **CC-5** ✅ | CP | ~~错误响应缺顶层 `request_id`；未知 query 参数静默 200~~ | **已修复（本 PR）**：新增 `middleware.RespondError`（非 abort，写完整信封含顶层 `request_id`）+ 将 ~120 处 handler `gin.H{"error":NewErrorBody(...)}` 与 JWT/RequireRole 中间件统一改走 `RespondError`/`AbortWithError`——所有错误响应现携带 `request_id`（`error` 子对象形状不变，纯增量）；新增 `middleware.RejectUnknownQuery`，`GET /api/v1/files` 对白名单外参数返回 `400 INVALID_QUERY_PARAM`。设计 §5.11 已更新 | 06 契约瑕疵 |
| **CC-6** ✅ | CP | ~~无 TTL 驱动的离线兜底判定~~ | **已修复（本 PR）**：新增 `internal/worker` OfflineSweeper——每 30s 扫描"DB=online 但 Redis 在线 key 已过期"的 Agent，兜底置 offline + 补发 `events.agent.offline`（形状与流断开路径一致）；设计 §5.2/§5.12 已更新。修前：仅 gRPC 流断开时置离线，CP 崩溃/半开残留 online | 01 §3 |
| **CC-7** ✅ | CP + webui | ~~`kafka_publish` 死配置 + `nats_publish` 空心 + action_type UI 失配~~ | **已修复（本 PR / D-019）**：核查发现 `nats_publish` 其实**也是空心的**（只写 `delivered` delivery，从不真正发布）。真正实现之：`NATSActionConfig{subject}` + `Engine.WithPublisher` + `dispatchNATS`（发布到 subject、失败进重试）+ retry 打通 nats。API 拒绝非 `webhook`/`nats_publish` 的 action_type（`400 INVALID_ACTION_TYPE`）与缺字段的 config（`400 INVALID_ACTION_CONFIG`）。webui 恢复 `nats_publish`、不提供 `kafka_publish`、移除 `TODO(CC-7)`。live-e2e：发 `events.file.uploaded` → 引擎重发到自定义 subject 收到、delivery=delivered。设计 §5.9 已更新 | 01 §4 |

## Tier C — 功能完备（webui，按实际使用价值可提前到 A/B）

| ID | 模块 | 缺口 | 来源 |
|----|------|------|------|
| **CC-8** ✅ | CP + webui | ~~Agent 重命名（管理员设自定义显示名）~~ | **已完成（本 PR / D-021）**：`PATCH /api/v1/agents/:id`（super_admin）+ sqlc `UpdateAgentName`；名称校验偏严（≤64 rune、白名单 `^[\p{L}\p{N} ._-]+$` 防路径注入，因 `{agent_name}` 入模板）→ `422`。webui 详情页标题加铅笔 → 重命名 Modal（回填/校验/提交 `renameAgent`）。handler 7 测（含非管理员 403）+ webui 3 测；后端 curl + **浏览器 live-e2e** 全过（改名→标题+名称字段刷新→DB 持久化）。backlog T4-5 |
| **CC-9** ✅ | CP + webui | ~~采集规则原地编辑~~ | **已完成**：Part 1 后端（PR #56 / D-020）——sqlc `UpdateCollectionRule` + `PUT .../rules/{rid}` 双形态（status-only 向后兼容 + 含 `name` 全字段更新，缺字段/非法 mode `422`、非法 bucket `400`、`id+agent_id+org_id` 防 IDOR）+ active 结果热重载。Part 2 webui（本 PR）——规则列表/详情加「编辑」入口 + RuleForm edit 模式（三步全回填、mode 大小写归一、标题/按钮切换、提交调 `updateRule`、Dry-Run 可跳过、保留 enabled）+ 路由 `/agents/:id/rules/:rid/edit`。**浏览器 live-e2e 全程通过**（三步回填 → 改模板 → 保存 → DB 持久化、规则 ID 不变）。backlog T4-6 |
| **CC-10** | webui | 状态枚举大小写映射等"隐性契约"无文档 | 03 §小结 / 05 §4 |

> **注**：CC-8/CC-9 是纯功能增量，若按实际使用它们比 Tier B 的健壮性修复更有价值，可提前。
> 取决于真实使用场景（人工决定）。

---

## 已修复（勿重复）

- **止血冲刺**：G-1 refresh 契约（D-012）、G-2 Agent token 生命周期 + **JWT 续期自愈**（D-013，解决了 02 报告"token 续期疑似缺失"）、
  G-3 minio-event 鉴权（D-014）、G-4 心跳遥测（D-015）、G-5 Dashboard 统计（D-016）、G-15 凭据文件 gitignore。
- **本清单**：CC-1 file_deleted（PR #47 / D-017）、CC-2 queue_max_size（PR #48）、CC-6 离线兜底扫描（PR #51）、CC-5 错误响应 `request_id` + 未知参数拒绝（PR #53）、CC-4 API 限流（PR #54 / D-018）、CC-7 nats_publish 实现 + kafka 拒绝（本 PR / D-019）。

## 明确推后（非核心模块 / 按产品决策）

| 项 | 原因 |
|----|------|
| Python SDK 联调（T3-3）/ Java SDK（T4-4）| 暂无消费方；CP 契约维护好则后期单独开发风险低（人工决策 2026-07-04） |
| G-8/G-9 契约单一权威 / OpenAPI 工具化 | 原为保护 T3-3；T3-3 推后则连带推后。webui↔CP 契约用现有轻量纪律（DECISIONS + 契约测试范式）维护 |
| G-7 Prometheus 指标（T4-1）/ Agent `disks`/`upload_bps` 遥测 | 监控完善，体量大，非阻塞 |
| 结构性文档重构（design 去重指针化、CLAUDE.md 减肥）| 等核心代码稳定后做，避免文档追着动的代码跑 |
| 存储物理用量（`madmin.BucketUsageInfo`）| Dashboard 已用 `SUM(size_bytes)` 索引口径够用；物理用量为可选增强 |

---

## 执行顺序建议

`CC-1 ✅` → `CC-2 ✅` → `CC-6 ✅` → `CC-3 ⏸️ 推后` → `CC-5 ✅` → `CC-4 ✅` → `CC-7 ✅` → `CC-9 ✅（规则原地编辑）` → `CC-8 ✅（Agent 重命名）` → `CC-10（隐性契约文档，视价值）/ optional proto→buf`
