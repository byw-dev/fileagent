# 01 — Control Plane 与设计文档对照

> 对照基准：`docs/design/system-design.md` 第三、五、六章 + 附录 B/C
> 方法：静态代码对照（动态验证见 `06-e2e-verification.md`）
> 标记：✅ 一致 ⚠️ 有出入 ❌ 缺失/错误 📝 设计未覆盖

---

## 1. REST API（设计 §5.11 / 附录 B.2）

| 项 | 状态 | 说明 |
|----|------|------|
| 认证接口 `/api/auth/*` | ✅ | login/refresh/logout/me/oidc-callback 全部注册（`router.go:62-68`）。**注意**：设计文档自身矛盾——§5.3.2 写 `/api/auth/*`，附录 B.2 却写 `/api/v1/auth/*`；实现遵循 §5.3.2 和 CLAUDE.md 契约表 |
| 用户/agents/files/file-types/buckets/event-rules/upload-logs | ✅ | 与附录 B.2 完全对齐，另新增 `POST /agents/:id/test-rule`（T3-6 dry-run，📝设计文档未回填） |
| `/internal/minio-event` | ✅ | 路由存在且已接 indexer（B-4 已修）。~~未校验共享密钥~~ **已修复（止血冲刺 G-3 / D-014）**：用 `INTERNAL_WEBHOOK_SECRET`（哈希后常量时间比较）校验 auth_token，未配置密钥时 fail-closed（注：报告原文写的 env 名 `MINIO_WEBHOOK_TOKEN` 有误，实际为 `INTERNAL_WEBHOOK_SECRET`） |
| API 限流 | ❌ | 设计 §5.1 与 Redis Key 表要求 `ratelimit:api:{user_id}`；`internal/api/middleware/` 只有 error.go 和 jwt.go，**无限流中间件**。`cache/keys.go` 注释里列了 key 模式但无实现 |
| 统一错误响应格式 | 待验证 | 设计 §5.11 定义 `{error:{code,message,detail},request_id}`，待 e2e 验证实际格式 |

## 2. 数据模型（设计 §3.3）

| 项 | 状态 | 说明 |
|----|------|------|
| 全部 11 张表 | ✅ | `000001_init_schema` 覆盖设计 DDL |
| collection_rules 字段 | 📝⚠️ | `000003_rename_rule_fields` 按 D-009 重命名（source_path_template→base_path 等）并**删除了 watch_subdir_pattern**。代码为准绳，但设计文档 §3.3.6 / §4.3 proto 均未回填，已是过时契约 |
| 默认 bucket 种子 | 📝 | `000002_seed_default_buckets` 设计未提及（T3-1 补丁产物） |

## 3. Agent 连接管理（设计 §5.2）

| 项 | 状态 | 说明 |
|----|------|------|
| AgentRegistry + 双 loop | ✅ | `grpcserver/handler.go` 与设计伪码一致 |
| Redis 在线状态 TTL | ✅ | `cache/keys.go` 定义、handler 使用 |
| **离线判定方式** | ⚠️ | 设计要求"Redis Key 过期 → 判定离线 → 触发 agent_offline 事件"（TTL 驱动）；实现是 **gRPC 流断开时**改状态并发事件（`handler.go:65-70`）。CP 自身崩溃重启、TCP 半开连接等场景下无兜底离线判定（无 TTL 过期监听/扫描 worker） |

## 4. 事件系统（设计 §5.9）

| 项 | 状态 | 说明 |
|----|------|------|
| NATS 主题 | ✅ | uploaded/online/offline/approved/revoked 5 个已发布；~~`events.file.deleted` 只有订阅、无发布方~~ **已修复（CC-1 / PR #47 / D-017）**：新增 `IndexDeletion` + `publishFileDeleted` 发布 `events.file.deleted`，webhook 按 `ObjectCreated:*` / `ObjectRemoved:*` 路由（不再一律 IndexUpload），MinIO ObjectRemoved 已接入 |
| Webhook 重试退避 | ✅ | 30s→2min→10min→30min→2h（`event/engine.go:175`，B-6 已修） |
| kafka_publish action | ⚠️ → CC-7 | 已核实：engine 实现了 `webhook` + `nats_publish`（`engine.go:290,292`），**仅 `kafka_publish` 无实现**（配了不生效）。归入 CC-7 收尾 |

## 5. 存储层集成（设计 §6.6）

| 项 | 状态 | 说明 |
|----|------|------|
| 创建 Bucket 调 MinIO | ✅ | `events.go:158 MakeBucket`（B-3 已修） |
| **Bucket 创建后 SetBucketPolicy** | ❌ | 设计 §6.6 要求创建后设置 Policy；代码无 SetBucketPolicy 调用 |
| **tmp-uploads Lifecycle 7 天清理** | ❌ | 无 Lifecycle 配置代码 |
| Bucket 存储用量（Dashboard） | 待查 | 设计要求 madmin.BucketUsageInfo 每 5 分钟缓存 |
| STS AssumeRole | ✅ | `storage/sts.go:49` 按设计实现 |
| 预签名 URL 15 分钟 | ✅ | `files.go:257`（B-1/B-2 已修） |

## 6. 后台 Worker（设计 §5.12）

| 项 | 状态 | 说明 |
|----|------|------|
| `worker/credential_rotator.go` | ❌ | 目录不存在。STS 续期完全依赖 Agent 主动 RefreshCredentials（可接受的简化，但与设计不符且无决策记录） |
| `worker/event_retry.go` | ⚠️ | 功能存在但内嵌在 `event/engine.go`（30s ticker），结构性偏差，可接受 |

## 7. 监控（设计 §9.2）

| 项 | 状态 | 说明 |
|----|------|------|
| **Prometheus 指标** | ❌ | 全代码库无任何 prometheus 引用。设计列出的 9 个 CP 自定义指标、`METRICS_LISTEN=:9200` 配置项全部未实现。第九章整章落空（backlog T4-1 只写了"监控配置"，未明确包含代码侧插桩） |

## 8. 小结

- 主链路（注册审批、规则下发、上传索引、文件查询下载、事件 webhook）结构完整，历史审计缺口（B-1~B-6）确已修复。
- **实质缺口**：~~minio-event 无鉴权~~（✅ G-3/D-014）、无 API 限流（→ CC-4）、无 Prometheus 指标（推后）、bucket policy/lifecycle 未设置（→ CC-3）、TTL 驱动离线判定缺失（→ CC-6）、~~file_deleted 事件死配置~~（✅ CC-1/D-017）；`kafka_publish` 死 action（→ CC-7）。
- **文档欠账**：test-rule 端点、字段重命名、seed migration 均未回填设计文档。
