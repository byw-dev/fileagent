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
| **CC-3** | CP | Bucket 创建后未设 Policy + `tmp-uploads` 无 Lifecycle | 设计 §6.6 要求 **CP 创建 bucket 后**自动 `SetBucketPolicy` + `tmp-uploads` 7 天 Lifecycle。**CP 运行时创建 bucket 的代码路径两者皆无**；`deploy/scripts/init-minio.sh` 虽*尝试*给种子 bucket 配 Lifecycle，但 e2e 实测该步失败（06 D-1，`Unable to read ILM configuration`）——即当前 tmp-uploads 根本未自动清理 | 01 §5 |
| **CC-4** | CP | 无 API 限流 | 设计 §5.1 + Redis `ratelimit:api:{user_id}` 要求限流；`middleware/` 无实现 | 01 §1 |
| **CC-5** | CP | 错误响应缺顶层 `request_id`；未知 query 参数静默 200 | 设计 §5.11 错误格式含 `request_id`，实测无；`files` 未知过滤参数返回 200 不报错 | 06 契约瑕疵 |
| **CC-6** | CP | 无 TTL 驱动的离线兜底判定 | 现仅在 gRPC 流断开时置离线；CP 崩溃重启 / TCP 半开会残留 online 状态，无扫描/过期兜底 | 01 §3 |
| **CC-7** | CP + webui | `kafka_publish` action 死配置 + action_type UI 失配 | 已核实：DB enum = `webhook / nats_publish / kafka_publish`（**无 `email`**）；engine 已实现 `webhook` + `nats_publish`（`engine.go:290,292`），**仅 `kafka_publish` 无实现**（配了不生效）。CC-1 期间 webui `Events/Create.tsx` 已临时收窄为 **webhook-only** 并留 `TODO(CC-7)`。CC-7 收尾：实现或从枚举/UI 移除 `kafka_publish`，并把已可用的 `nats_publish` 放回 UI | 01 §4 |

## Tier C — 功能完备（webui，按实际使用价值可提前到 A/B）

| ID | 模块 | 缺口 | 来源 |
|----|------|------|------|
| **CC-8** | CP + webui | Agent 重命名（管理员设自定义显示名） | backlog T4-5 |
| **CC-9** | CP + webui | 采集规则原地编辑（复用三步 Wizard 回填 + PUT 全字段） | backlog T4-6 |
| **CC-10** | webui | 状态枚举大小写映射等"隐性契约"无文档 | 03 §小结 / 05 §4 |

> **注**：CC-8/CC-9 是纯功能增量，若按实际使用它们比 Tier B 的健壮性修复更有价值，可提前。
> 取决于真实使用场景（人工决定）。

---

## 已修复（勿重复）

- **止血冲刺**：G-1 refresh 契约（D-012）、G-2 Agent token 生命周期 + **JWT 续期自愈**（D-013，解决了 02 报告"token 续期疑似缺失"）、
  G-3 minio-event 鉴权（D-014）、G-4 心跳遥测（D-015）、G-5 Dashboard 统计（D-016）、G-15 凭据文件 gitignore。
- **本清单**：CC-1 file_deleted（PR #47 / D-017）、CC-2 queue_max_size（PR #48）。

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

`CC-1 ✅` → `CC-2 ✅` → `CC-3（下一步：bucket policy/lifecycle）` → `CC-4~7（CP 健壮性，可分批）` → `CC-8/9（视使用价值）`
