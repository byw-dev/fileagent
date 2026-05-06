# Phase 3 集成测试准入分析报告

> **审计日期**：2026-05-06  
> **审计范围**：`controlplane/`、`agent/`、`webui/`、`sdk/python/`  
> **审计目的**：确认当前代码是否满足进入 Phase 3 集成测试阶段的前置条件

---

## 一、桩代码 / 501 占位代码现状

### 结论：nil-guard 不是问题，但存在若干真实实现缺失

代码中大量的 `if h.db == nil { middleware.NotImplemented(c) }` 是 **nil-guard 模式，并非未实现**。`main.go` 已将所有依赖注入（`queries`、`agentMgr`、`dispatcher` 等），运行时这些 guard 永远不会触发。

以下是**真正未实现或存在缺陷的位置**：

| 位置 | 问题描述 | 严重度 |
|------|----------|--------|
| `controlplane/internal/api/handler/events.go` `BucketsHandler.Create` | `POST /api/v1/buckets` 只写 DB 记录，**未调用 `madmin.MakeBucket()` 在 MinIO 实际创建 Bucket**。设计文档 §6.6 明确要求调用 MinIO Admin API | 🔴 High |
| `controlplane/internal/api/handler/files.go` `DownloadURL` | 以 `entry.BucketID.String()`（UUID 字符串）作为 MinIO bucket 名调用 `PresignedGetObject`，但 MinIO bucket 名是业务字符串（如 `data-sensor`），**UUID 不是有效 bucket 名** | 🔴 High |
| `controlplane/internal/api/handler/files.go` | 预签名 URL 有效期为 **5 分钟（300s）**，设计文档 §5.11.3 要求 **15 分钟** | 🟡 Medium |
| `controlplane/internal/api/handler/auth.go` `OIDCCallback` | **始终返回 501**，无 nil-guard，是真正的空实现（设计文档 §5.3.3 已标注为"预留扩展"，可接受） | 🟢 Low（设计明确） |
| `controlplane/internal/api/handler/events.go` `MinioEventHandler.Handle` | 只打日志，**未触发 Indexer 对文件条目进行索引**，MinIO 事件备用采集路径失效 | 🟡 Medium |
| `agent/internal/executor` / `watcher` | `append_mode` 字段已传递，但 `tail`（offset 增量上传）和 `close_wait` 逻辑**未在 executor/watcher 中实现**，只有 `none`/`overwrite` 实际生效。设计文档 §4.4.3 有完整规范 | 🔴 High |

---

## 二、测试覆盖率汇总

> 要求：整体 **≥ 80%**，核心业务逻辑 **≥ 90%**

### 2.1 Control Plane（整体 77.4%，**未达标**）

| 包 | 覆盖率 | 达标 |
|----|--------|------|
| `cmd/server` | 0.0% | — (main.go 可豁免) |
| `internal/agent` | 82.1% | ✅ |
| `internal/api` | 100.0% | ✅ |
| `internal/api/handler` | **74.1%** | ❌ |
| `internal/api/middleware` | 93.2% | ✅ |
| `internal/auth` | 83.3% | ✅ |
| `internal/cache` | 80.0% | ✅ |
| `internal/config` | 90.1% | ✅ |
| `internal/db` | 84.3% | ✅ |
| `internal/event` | **62.7%** | ❌ |
| `internal/grpcserver` | 87.9% | ✅ |
| `internal/indexer` | 91.4% | ✅ |
| `internal/storage` | **76.7%** | ❌ |

**handler 未覆盖的关键函数：**
- `MinioEventHandler.NewMinioEventHandler` / `Handle`：0%
- `authdb.go` 中 `NewQueriesAuthDB`、`GetUserByUsername`、`UpdateUserLastLogin`：0%
- `DownloadURL`：47.6%

**event 未覆盖的关键函数：**
- `processRetries`：0%
- `retryDelivery`：0%
- `NewEngine`（生产构造函数）：0%
- DB adapter 方法（`ListEnabledEventRules`、`CreateEventDelivery` 等）：0%

### 2.2 Agent（整体 62.9%，**严重低于 80% 要求**）

| 包 | 覆盖率 | 达标 |
|----|--------|------|
| `cmd/agent` | 0.0% | — (main.go 可豁免) |
| `internal/config` | 100.0% | ✅ |
| `internal/credential` | 86.6% | ✅ |
| `internal/executor` | 87.3% | ✅ |
| `internal/grpcclient` | **61.5%** | ❌ |
| `internal/queue` | **79.5%** | ❌ |
| `internal/scheduler` | 100.0% | ✅ |
| `internal/uploader` | **79.1%** | ❌ |
| `internal/watcher` | 83.0% | ✅ |

**grpcclient 未覆盖的关键函数：**
- `SetToken`、`SetAgentID`、`SetMessageHandler`：0%
- `ServiceClient`、`SendMessage`：0%
- `RefreshCredentials`：0%
- `Connect`：0%
- `buildDialOpts`：0%

### 2.3 WebUI

仅有 5 个测试文件（`api.test.ts`、`auth.store.test.ts`、`services.test.ts`、`pathTemplate.test.ts`、`login.test.tsx`），**没有运行覆盖率检查的配置**。CLAUDE.md 要求核心 store/service 层 ≥ 80%，当前无法验证。

### 2.4 Python SDK

有 6 个测试文件，同样未运行覆盖率收集，无法确认是否达标。

---

## 三、TASK_LIST.md 验收点与设计文档的差距清单

| # | 验收点/功能 | 设计文档位置 | 当前状态 | 差距描述 |
|---|------------|------------|---------|---------|
| 1 | Bucket 创建同时在 MinIO 建立物理 Bucket | §6.6 `madmin.MakeBucket()` | ❌ 缺失 | 只写 DB，MinIO 无实际 Bucket |
| 2 | 预签名 URL 使用真实 Bucket 名（非 UUID） | §5.11.3 | ❌ Bug | 用 BucketID UUID 作为 bucket 名 |
| 3 | 预签名 URL 有效期 15 分钟 | §5.11.3 "TTL 15分钟" | ❌ 误差 | 代码硬编码 5 分钟 |
| 4 | `append_mode=tail`：记录 offset 仅上传新增字节 | §4.4.3 | ❌ 未实现 | executor/watcher 中无此逻辑 |
| 5 | `append_mode=close_wait`：等待 CLOSE_WRITE 后上传 | §4.4.3 | ❌ 未实现 | watcher 中无 CLOSE_WRITE 特殊处理 |
| 6 | MinIO 事件 webhook 触发文件索引 | §6.5 → Indexer | ❌ 未实现 | `Handle` 只打日志，未调用 Indexer |
| 7 | 事件重试退避：最多 5 次，最大间隔 2h | §5.9 | 🟡 部分 | 退避公式正确，但上限为 1h（设计最大 2h），且无"最多 5 次"限制 |
| 8 | Bucket 使用量 API（Dashboard 每 5 分钟缓存） | §6.6 `madmin.BucketUsageInfo()` | ❌ 缺失 | 无此端点 |
| 9 | agent `grpcclient` 核心路径无测试 | CLAUDE.md ≥80% | ❌ 61.5% | `Connect`、`SendMessage`、`RefreshCredentials` 均为 0% |
| 10 | `controlplane/event` 重试路径无测试 | CLAUDE.md ≥80% | ❌ 62.7% | `processRetries`、`retryDelivery` 均为 0% |
| 11 | WebUI 覆盖率未经验证 | CLAUDE.md ≥80% | ⚠️ 未知 | 无 coverage 配置，5 个测试文件对应 13 个页面模块 |

---

## 四、总体结论

**当前不满足进入 Phase 3 集成测试阶段的前置要求**，主要原因：

### 🔴 阻塞项（直接导致集成测试失败）

1. **功能缺口**：`append_mode` 的 tail/close_wait 未实现，Bucket 创建未调用 MinIO Admin API，预签名 URL bucket 名是 Bug——这三项会直接导致集成测试失败。

### 🔴 覆盖率不达标

2. Agent 整体 **62.9%**（要求 80%），Control Plane 整体 **77.4%**（要求 80%），`event` 和 `grpcclient` 包核心路径覆盖率极低（~61-62%），核心业务逻辑要求 90%，差距显著。

### 🟡 设计偏差

3. 预签名 URL TTL：5min（代码）vs 15min（设计）
4. 事件重试最大次数无上限约束，最大退避时间也偏小（1h vs 2h）

---

## 五、建议修复顺序

| 优先级 | 任务 | 类型 |
|--------|------|------|
| P0 | 修复预签名 URL 使用 BucketID UUID 作为 bucket 名的 Bug | Bug Fix |
| P0 | 在 Bucket Create 中调用 `madmin.MakeBucket()` | 功能补全 |
| P0 | 实现 agent `append_mode=tail` 和 `close_wait` | 功能补全 |
| P1 | 实现 MinIO webhook → Indexer 管道 | 功能补全 |
| P1 | 将预签名 URL TTL 从 5min 改为 15min | 设计对齐 |
| P1 | 为 `event` 重试路径补全测试（`processRetries`、`retryDelivery`） | 测试覆盖 |
| P1 | 为 `grpcclient` `Connect`/`SendMessage`/`RefreshCredentials` 补全测试 | 测试覆盖 |
| P2 | 事件重试最多 5 次限制 + 最大退避 2h 对齐设计 | 设计对齐 |
| P2 | WebUI 覆盖率验证（添加 coverage 配置并补齐测试） | 测试覆盖 |
| P2 | Bucket 使用量 API（`madmin.BucketUsageInfo()`） | 功能补全 |
