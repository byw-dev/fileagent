# Phase 3 集成测试准入分析报告

> **初版审计日期**：2026-05-06（T2-X8 完成前）
> **更新日期**：2026-05-06（T2-X8 合入后，结合实测覆盖率修订）
> **审计范围**：`controlplane/`、`agent/`、`webui/`、`sdk/python/`
> **审计目的**：确认当前代码是否满足进入 Phase 3 集成测试阶段的前置条件

---

## 一、桩代码 / 501 占位代码现状

### 结论：nil-guard 不是问题，存在 5 处真实功能缺口

代码中大量的 `if h.db == nil { middleware.NotImplemented(c) }` 是 **nil-guard 防御模式，并非未实现**。
`controlplane/cmd/server/main.go` 已将所有依赖完整注入，运行时这些 guard 永远不会触发。

以下是**真正未实现或存在逻辑缺陷的位置**：

| # | 文件 / 函数 | 问题描述 | 严重度 |
|---|------------|---------|--------|
| B-1 | `handler/files.go` `DownloadURL` 和 `BatchDownloadURLs` | 以 `entry.BucketID.String()`（UUID 字符串）作为 MinIO bucket 名传入 `PresignedGetObject`，但 MinIO 中 bucket 名是业务字符串（如 `data-sensor`）。**UUID 不是有效 bucket 名**，预签名 URL 将指向不存在的 bucket | 🔴 High |
| B-2 | `handler/files.go` `DownloadURL` / `BatchDownloadURLs` | 预签名 URL 有效期硬编码为 **5 分钟**（`5*time.Minute`，`expires_in: 300`），设计文档 §5.11.3 要求 **15 分钟** | 🟡 Medium |
| B-3 | `handler/events.go` `BucketsHandler.Create` | `POST /api/v1/buckets` 只写 DB 记录，**未调用 `madmin.MakeBucket()` 在 MinIO 实际创建 Bucket**，设计文档 §6.6 明确要求调用 MinIO Admin API | 🔴 High |
| B-4 | `handler/events.go` `MinioEventHandler.Handle` | 只打日志，**未调用 `Indexer.IndexUpload()` 将文件信息写入 file_entries**，MinIO 事件驱动的备用采集链路完全无效 | 🟡 Medium |
| B-5 | `agent/internal/watcher` + `executor` + `queue` | `append_mode=tail`（仅上传新增字节）和 `close_wait`（等待 CLOSE_WRITE 后上传）**完全未实现**；`AppendMode` 字段只被 `scheduler.Rule` 存储、由 `main.go` 透传，但 watcher 和 executor 中无任何分支处理。设计文档 §4.4.3 有完整规范 | 🔴 High |
| B-6 | `event/engine.go` `retryDelivery` | 退避表不符合设计：代码用 `30s×2^n`（实际序列：30s→60s→120s→240s→480s）而设计 §5.9 要求 **30s→2min→10min→30min→2h**；无最多重试 5 次的终止条件；上限为 1h 而非设计要求的 2h | 🟡 Medium |
| B-7 | `auth.go` `OIDCCallback` | 始终返回 501，无 nil-guard，是真正的空实现（设计文档 §5.3.3 已标注为"预留扩展"，**可接受**，非阻塞） | 🟢 Low |

---

## 二、测试覆盖率汇总

> 要求：整体 **≥ 80%**，核心业务逻辑 **≥ 90%**
> 本次使用 `go test -tags=integration -coverprofile=...` 运行（含集成测试）

### 2.1 Control Plane（整体 77.6%，**未达标**）

| 包 | 覆盖率（含 integration tag） | 达标 |
|----|---------------------------|------|
| `cmd/server` | 0.0% | — (main.go 可豁免) |
| `internal/agent` | ~82% | ✅ |
| `internal/api` | 100.0% | ✅ |
| `internal/api/handler` | **74.1%** | ❌ |
| `internal/api/middleware` | 93.2% | ✅ |
| `internal/auth` | 83.3% | ✅ |
| `internal/cache` | 80.0% | ✅ |
| `internal/config` | 90.1% | ✅ |
| `internal/db` | 84.3% | ✅ ⚠️ (集成测试 3 个失败：DB 不存在) |
| `internal/event` | **62.7%** | ❌ |
| `internal/grpcserver` | 87.9% | ✅ |
| `internal/indexer` | 91.4% | ✅ |
| `internal/storage` | **86.7%** | ✅ (T2-X8 IT-1 通过后提升) |

**handler 未覆盖的关键函数：**

| 函数 | 覆盖率 | 说明 |
|------|--------|------|
| `MinioEventHandler.NewMinioEventHandler` / `Handle` | 0% | 无任何测试 |
| `authdb.NewQueriesAuthDB` / `GetUserByUsername` / `UpdateUserLastLogin` | 0% | 直通 wrapper，无测试 |
| `FilesHandler.DownloadURL` | 47.6% | 错误路径未覆盖 |
| `EventRulesHandler.Delete` | 58.3% | 错误路径未覆盖 |
| `AgentsHandler.DeleteRule` | 62.5% | dispatcher 失败路径未覆盖 |
| `helpers.encodeCursor` | 0% | 纯工具函数 |

**event 未覆盖的关键函数（根因：retryWorker 依赖 30s ticker，测试无法同步触发）：**

| 函数 | 覆盖率 | 根因 |
|------|--------|------|
| `NewEngine`（生产构造函数） | 0% | 测试直接构造内部 Engine struct，绕过公开函数 |
| `DBAdapter` 接口方法（5 个） | 0% | 测试使用 mock，从未调用 `NewDBAdapter` |
| `processRetries` | 0% | 在 30s ticker goroutine 中，测试在 goroutine 启动前就断言完毕 |
| `retryDelivery` | 0% | 同上 |

### 2.2 Agent（含 main 整体 63.9%；排除 cmd/agent 后约 83%，**核心包基本达标**）

| 包 | 覆盖率 | 达标 |
|----|--------|------|
| `cmd/agent` | 0.0% | — (main.go 可豁免) |
| `internal/config` | 100.0% | ✅ |
| `internal/credential` | 86.6% | ✅ |
| `internal/executor` | 87.3% | ✅ |
| `internal/grpcclient` | **61.5%** | ❌ |
| `internal/queue` | **79.5%** | ❌ |
| `internal/scheduler` | 100.0% | ✅ |
| `internal/uploader` | **87.8%** | ✅ (T2-X8 IT-2 通过后提升) |
| `internal/watcher` | 83.0% | ✅ |

**grpcclient 未覆盖的关键函数（根因：T2-X6 新增方法未补测试）：**

| 函数 | 覆盖率 | 说明 |
|------|--------|------|
| `SetToken` / `SetAgentID` / `SetMessageHandler` | 0% | T2-X6 新增 setter，无任何测试 |
| `ServiceClient` / `SendMessage` | 0% | T2-X6 新增方法，无任何测试 |
| `RefreshCredentials` | 0% | T2-X6 核心方法，无任何测试 |
| `Connect` | 0% | `buildDialOpts` 未测，TLS 路径从未覆盖 |
| `buildDialOpts` | 0% | TLS/非 TLS 两条路径均未覆盖 |

### 2.3 WebUI

仅有 5 个测试文件，**无覆盖率收集配置（vitest.config.ts 中未启用 coverage provider）**。
CLAUDE.md 要求核心 store/service 层 ≥ 80%，当前状态：
- `src/store/auth.ts`：有 `auth.store.test.ts`，基本覆盖
- `src/services/`：有 `services.test.ts`，但 13 个新 service 文件（T2-X7 新增）缺少对应测试
- 13 个新页面（T2-X7）：**全部无测试**

### 2.4 Python SDK

有 6 个测试文件（`test_auth.py`、`test_client.py`、`test_files.py`、`test_file_types.py`、
`test_agents.py`、`test_upload_logs.py`），但未运行覆盖率收集。建议执行 `pytest --cov` 确认。

---

## 三、TASK_LIST.md 验收点与设计文档的完整差距清单

| # | 功能 / 验收点 | 设计文档依据 | 当前状态 | 差距描述 |
|---|-------------|------------|---------|---------|
| G-1 | 预签名 URL 使用真实 Bucket 名（非 UUID） | §5.11.3 | ❌ Bug | `DownloadURL` + `BatchDownloadURLs` 均用 `BucketID.String()` |
| G-2 | 预签名 URL 有效期 15 分钟 | §5.11.3 "TTL 15分钟" | ❌ 偏差 | 代码硬编码 5 分钟 |
| G-3 | Bucket 创建同时在 MinIO 建立物理 Bucket | §6.6 `madmin.MakeBucket()` | ❌ 缺失 | 只写 DB，MinIO 中无实际 Bucket |
| G-4 | MinIO 事件 webhook → Indexer 写入 file_entries | §6.5 → §5.4 Indexer | ❌ 缺失 | `Handle` 只打日志，Indexer 未注入 |
| G-5 | `append_mode=tail`：仅上传新增字节 | §4.4.3 | ❌ 未实现 | watcher/executor 无 offset 追踪逻辑 |
| G-6 | `append_mode=close_wait`：等待 CLOSE_WRITE | §4.4.3 | ❌ 未实现 | watcher 不区分 CLOSE_WRITE 事件 |
| G-7 | 事件重试退避表：30s→2min→10min→30min→2h | §5.9 | ❌ 偏差 | 代码用 30s×2^n（30s→60s→120s→…），序列完全不符 |
| G-8 | 事件最多重试 5 次 | §5.9 | ❌ 缺失 | 无 `AttemptCount >= 5` 终止判断 |
| G-9 | 退避上限 2h | §5.9 | ❌ 偏差 | 代码上限为 1h |
| G-10 | Bucket 存储用量 API（Dashboard 5min 缓存） | §6.6 `madmin.BucketUsageInfo()` | ❌ 缺失 | 无此端点 |
| G-11 | `grpcclient` T2-X6 方法覆盖率 ≥ 80% | CLAUDE.md | ❌ 0% | `RefreshCredentials`/`Connect`/`SendMessage` 等全无测试 |
| G-12 | `event` 包重试路径覆盖率 ≥ 80% | CLAUDE.md | ❌ 62.7% | `processRetries`/`retryDelivery` 为 0% |
| G-13 | `queue` 包覆盖率 ≥ 80% | CLAUDE.md | ❌ 79.5% | `Open` 函数错误路径未覆盖 |
| G-14 | DB 集成测试全部通过 | CLAUDE.md | ❌ 3 个失败 | 测试期望 `fileagent_test` DB，docker-compose 创建的是 `fileagent` |
| G-15 | WebUI service/store 层覆盖率 ≥ 80% | CLAUDE.md | ⚠️ 未知 | vitest coverage 未配置；T2-X7 新增 13 个 service 文件无测试 |

---

## 四、总体结论

**当前不满足进入 Phase 3 集成测试阶段的前置要求。**

### 🔴 阻塞项（直接导致集成测试失败）

1. **G-1 bucket 名 Bug**：预签名 URL 永远指向不存在的 bucket，文件下载功能完全失效。
2. **G-3 MinIO bucket 创建缺失**：前端创建 Bucket 后 Agent 无法上传（MinIO 中无物理 bucket）。
3. **G-5/G-6 append_mode 未实现**：日志采集场景（持续追加写文件）无法正常工作；规则下发后行为与预期不符。

### 🔴 覆盖率严重不达标

4. **G-11/G-12**：`grpcclient` 61.5%、`event` 62.7%，核心业务路径（T2-X6 全部新增方法 + 重试逻辑）均为 0%。
5. **G-14**：DB 集成测试失败（docker-compose DB 名不匹配），3 个测试常态 FAIL。

### 🟡 设计偏差（集成测试会发现，最终须修复）

6. **G-2**：presign TTL 5min vs 设计 15min。
7. **G-7/G-8/G-9**：事件重试退避表和终止条件不符合设计 §5.9。
8. **G-4**：MinIO webhook 到 Indexer 管道缺失，备用采集链路失效。

### 🟢 可延迟（Phase 4）

- G-10 Bucket 存储用量 API（Dashboard 功能，非阻塞）
- G-15 WebUI 覆盖率（可在 Phase 3 联调期间同步完善）
- B-7 OIDCCallback 501（明确预留扩展）

---

## 五、修复优先级与技术方案概要

| 优先级 | 编号 | 任务 | 技术方案 |
|--------|------|------|---------|
| P0 | G-14 | 修复 DB 集成测试环境 | `docker-compose.test.yml` 的 postgres service 改 `POSTGRES_DB: fileagent_test` |
| P0 | G-1/G-2 | 修复预签名 URL bug + TTL | `FilesDB` 接口增加 `GetBucketByID`；`DownloadURL` 先查 bucket 取 `Name`；TTL 改 `15*time.Minute` |
| P0 | G-3 | Bucket Create 调用 MinIO Admin API | `BucketsHandler` 增加 `MinioAdmin` 接口（`MakeBucket(ctx, name) error`）；`RouterConfig` 中注入 `madmin.Client`；DB 写入成功后调用 `MakeBucket` |
| P1 | G-5/G-6 | 实现 append_mode tail/close_wait | watcher：`close_wait` 模式监听 `CLOSE_WRITE`（inotify `IN_CLOSE_WRITE`）而非 `WRITE`；`tail`：`queue.UploadTask` 增加 `FileOffset int64` 字段，executor 读取 offset 用 `io.NewSectionReader` 提交追加上传 |
| P1 | G-11 | grpcclient 覆盖率 ≥ 80% | 在 `client_test.go` 中为 T2-X6 全部新增函数（`SetToken/SetAgentID/SetMessageHandler/SendMessage/ServiceClient/RefreshCredentials/buildDialOpts`）补充单元测试，使用已有 `fakeAgentServer` |
| P1 | G-12 | event 包覆盖率 ≥ 80% | 补 `TestNewEngine` 直接调用公开函数；补 `TestDBAdapter_*` 覆盖接口 wrapper；将 `processRetries` 抽为可直接调用的函数测试（不依赖 ticker） |
| P1 | G-7/G-8/G-9 | 修复事件重试逻辑 | 使用固定退避表 `[]time.Duration{30s, 2min, 10min, 30min, 2h}`；增加 `if d.AttemptCount >= 5 { status = "exhausted" }`；补对应测试 |
| P2 | G-4 | MinioEventHandler 调用 Indexer | `MinioEventHandler` 增加 `Indexer` 字段（接口 `IndexUpload(ctx, orgID, bucketName, objectKey, size, etag)`）；`Handle` 解析 Records 后调用 |
| P2 | G-13 | queue 包覆盖率 ≥ 80% | 补 `Open` 错误路径测试（无效 DSN）；补 `scanTasks` rows.Err 路径 |
| P3 | G-10 | Bucket 存储用量 API | 新端点 `GET /api/v1/buckets/:id/usage`；注入 `madmin.Client`；缓存 5min in Redis |
| P3 | G-15 | WebUI 覆盖率 | `vitest.config.ts` 增加 `coverage: { provider: 'v8', reporter: ['text','lcov'] }`；为 T2-X7 新增 service 文件补测试 |
