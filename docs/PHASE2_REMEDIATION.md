# Phase 2 遗留工作清单（集成联调前必须扫除）

> 文件路径：`docs/PHASE2_REMEDIATION.md`
> 对应 TASK_LIST.md 入口：Phase 2 末尾 **T2-X 集成测试前遗留工作扫除**
>
> 本文档记录在代码审计（2026-05-03）中发现的所有 Phase 0/1/2 验收点偏差，
> 分析根本原因，并给出逐条遗留工作。
> 所有条目解决后方可进入 Phase 3。

---

## 根本原因分析

| 类型 | 描述 |
|------|------|
| **验收点过粗**  | Task 的验收点只写"包要实现"，没有写"组件要接入 server/binary"，执行时把包逻辑写完就打 ✅ |
| **任务拆分遗漏** | REST handler 实现、controlplane 组装、agent main.go 组装、多个 WebUI 页面均无对应任务编号 |
| **测试声称但未建** | T2-A5 / T2-B4 写了"集成测试"验收点，但实际无 `-tags=integration` 文件 |

---

## Control Plane — 组件接线（Wiring Layer）

### CP-W1 gRPC `RefreshCredentials` RPC 未实现
- **现状**：`handler.go:105` 直接返回 `codes.Unimplemented`；`stsMgr` 在 `main.go` 中被 `_ = stsMgr` 丢弃
- **对应任务**：T2-A5（STS 凭据管理）验收点遗漏
- **修复**：
  1. 在 `grpcserver.Server` 中注入 `STSManager` 接口
  2. 实现 `RefreshCredentials`：验证 Agent JWT → 调用 `stsMgr.IssueCredentials` → 返回凭据
  3. `main.go` 传入 `stsMgr` 而不是 `_ = stsMgr`

### CP-W2 `handleUploadResult` 未调用 Indexer
- **现状**：只打日志；`ix` 在 `main.go` 中被 `_ = ix` 丢弃
- **对应任务**：T2-A4（文件索引引擎）验收点遗漏
- **修复**：
  1. 在 `grpcserver.Server` 中注入 `Indexer` 接口
  2. `handleUploadResult` 调用 `ix.IndexUpload(ctx, agentID, result)` 完成文件归类、写库、发布 NATS 事件
  3. `main.go` 传入 `ix`

### CP-W3 Dispatcher 未接入 gRPC Server
- **现状**：`dispatcher` 在 `main.go` 中被 `_ = dispatcher`；`Connect` 时从不调用 `SyncRulesOnConnect`
- **对应任务**：T2-A6（任务调度下发）验收点遗漏
- **修复**：
  1. 在 `grpcserver.Server` 中注入 `Dispatcher` 接口
  2. Agent `Connect` 成功后调用 `dispatcher.SyncRulesOnConnect(ctx, agentID)`
  3. `main.go` 传入 `dispatcher`

### CP-W4 离线规则暂存未实现
- **现状**：`DispatchRule` 离线时 `return nil`，没有持久化到任何队列
- **对应任务**：T2-A6 验收点"离线暂存+重连同步"未落地
- **修复**：
  - 离线时将规则写入 DB `pending_dispatch` 记录（或复用 collection_rules 加字段），重连后由 `SyncRulesOnConnect` 补发

### CP-W5 Event Engine 未启动 / 无 NATS 订阅
- **现状**：`eventEngine` 在 `main.go` 中被 `_ = eventEngine`；Engine 没有 `Start()` / NATS subscriber 方法
- **对应任务**：T2-A7（事件规则引擎）验收点"订阅 NATS；Background Worker"未落地
- **修复**：
  1. 为 `Engine` 添加 `Start(ctx, natsConn)` 方法，订阅 `events.file.uploaded` / `events.agent.*` 等主题
  2. 添加后台 Retry Worker，定期重试 `pending`/`failed` 的 `event_deliveries`
  3. `main.go` 调用 `eventEngine.Start(ctx, natsConn)` 并在 goroutine 中运行

### CP-W6 REST JWT 中间件未检查 Redis 黑名单
- **现状**：`middleware/jwt.go:72` 有 `// TODO (Phase 2 T2-A1): check jti against Redis blacklist.`
- **对应任务**：T2-A1（JWT 认证模块）验收点"黑名单测试"遗漏
- **修复**：
  - 中间件注入 `auth.Service`，调用 `IsRevoked(ctx, claims.ID)` 并在 true 时返回 401

---

## Control Plane — REST Handler 实现（完全缺失的任务）

以下 Handler 全部为 `middleware.NotImplemented(c)`。
需要新增 DB 查询、注入 Manager/DB 依赖并完整实现每个端点。

### CP-H1 AgentsHandler（10 个端点）
| 端点 | 方法 | 关键逻辑 |
|------|------|---------|
| GET `/api/v1/agents` | List | 分页列出本组织 agents |
| GET `/api/v1/agents/:id` | Get | 按 ID 查询单个 agent |
| POST `/api/v1/agents/:id/approve` | Approve | 调用 agentMgr.Approve；仅 super_admin |
| POST `/api/v1/agents/:id/revoke` | Revoke | 调用 agentMgr.Revoke；仅 super_admin |
| POST `/api/v1/agents/:id/list-dir` | ListDir | 向在线 agent 发送 ListDir 命令并等待响应 |
| GET `/api/v1/agents/:id/rules` | ListRules | 查询 collection_rules 表 |
| POST `/api/v1/agents/:id/rules` | CreateRule | 写库后调用 dispatcher.DispatchRule |
| PUT `/api/v1/agents/:id/rules/:rid` | UpdateRule | 更新规则后重新下发 |
| DELETE `/api/v1/agents/:id/rules/:rid` | DeleteRule | 软删除 + dispatcher.DispatchRuleCancel |
| GET `/api/v1/agents/:id/upload-logs` | ListUploadLogs | 按 agent 分页查询 upload_logs |

### CP-H2 FilesHandler（4 个端点）
| 端点 | 方法 | 关键逻辑 |
|------|------|---------|
| GET `/api/v1/files` | List | cursor-based 分页，支持 agent_id/bucket/file_type/status/日期 筛选 |
| GET `/api/v1/files/:id` | Get | 按 ID 返回 file_entry |
| GET `/api/v1/files/:id/download-url` | DownloadURL | MinIO presigned URL（有效期 5 min） |
| POST `/api/v1/files/batch-download-urls` | BatchDownloadURLs | 批量 presigned URL |

### CP-H3 FileTypesHandler（4 个端点）
CRUD：List / Create / Update / Delete

### CP-H4 BucketsHandler（2 个端点）
List / Create（仅 super_admin 创建）

### CP-H5 EventRulesHandler（5 个端点）
CRUD + ListDeliveries

### CP-H6 UploadLogsHandler（2 个端点）
List（cursor-based）/ Get

### CP-H7 UsersHandler（5 个端点）
List / Create / Update / Delete / UpdatePassword

### CP-H8 `/internal/minio-event` Webhook 处理器
- 接收 MinIO 事件通知 → 调用 Indexer.IndexUpload（备选链路，非主路径）

---

## Control Plane — DB 查询层缺失

以下表的 SQL 查询尚未通过 sqlc 或手写实现，REST Handler 实现前需补齐。

| 表 | 缺失查询 |
|----|---------|
| `file_entries` | ListFileEntries（cursor + 多筛选）、GetFileEntryByID |
| `upload_logs` | ListUploadLogs（cursor + agent_id 筛选）、GetUploadLogByID |
| `file_types` | ListFileTypes、CreateFileType、UpdateFileType、DeleteFileType |
| `buckets` | ListBuckets、CreateBucket |
| `event_rules` | ListEventRules、CreateEventRule、UpdateEventRule、DeleteEventRule |
| `event_deliveries` | ListDeliveriesByRule（cursor）|
| `users` | UpdateUser、DeleteUser（UpdateActive 已有）|

> 注意：indexer/queries.go 中已有写路径（UpsertFileEntry、CreateUploadLog 等），
> 读路径查询（List/Get）需要新增到 `db/queries/` 下并重新生成 sqlc 代码，
> 或继续沿用 indexer/queries.go 的手写 SQL 风格（保持一致）。

---

## Control Plane — 集成测试缺失

### CP-IT1 STS 集成测试（T2-A5 验收遗漏）
- **修复**：在 `controlplane/internal/storage/` 增加 `sts_integration_test.go`
  - `//go:build integration`
  - 连接真实 MinIO（`deploy/docker-compose.test.yml`），验证 `IssueCredentials` 返回可用凭据

---

## Edge Agent — 组装层

### AG-W1 agent main.go 是空壳（完全缺失的任务）
- **现状**：`agent/cmd/agent/main.go` 仅 `fmt.Println("fileagent agent")`
- **修复**：将以下组件组装为完整 Agent 进程：
  1. 加载配置（`config.Load()`）
  2. 初始化日志（zap）
  3. 创建 SQLite 队列（`queue.New`）
  4. 创建 Credential Manager（`credential.NewTokenManager` + `STSManager`）
  5. 创建 gRPC Client（`grpcclient.New`）
  6. 执行注册审批生命周期（`Lifecycle.Run`）
  7. 启动 Executor（`executor.New` + `Start`）
  8. 根据规则启动 Watcher / Scheduler（接收 gRPC `PushRule` 命令）
  9. 启动 Connect 流（心跳 + 接收 ServerMessage）
  10. 处理 SIGINT/SIGTERM 优雅退出

### AG-W2 Agent 未调用 `RefreshCredentials` RPC
- **现状**：`grpcclient.Client` 没有 `RefreshCredentials` 方法，STS 凭据永不刷新
- **修复**：
  1. 在 `grpcclient.Client` 中添加 `RefreshCredentials(ctx) (*CredentialsPayload, error)`
  2. 在 main 组装层启动后台 goroutine，距 STS 到期前 10 min 调用刷新

---

## Edge Agent — 集成测试缺失

### AG-IT1 Upload Engine 集成测试（T2-B4 验收遗漏）
- **修复**：在 `agent/internal/uploader/` 增加 `uploader_integration_test.go`
  - `//go:build integration`
  - 连接真实 MinIO，验证单次上传和分片上传均成功写入，SHA-256 校验正确

---

## Web UI — 未实现页面（完全缺失的任务）

以下页面在路由中存在但内容是"页面开发中"占位符，对应功能在 Phase 2 设计范围内，
但 TASK_LIST.md 的 T2-C1~C5 并未覆盖这些页面。

| 页面 | 文件 | 所需功能 |
|------|------|---------|
| 文件类型列表 | `pages/FileTypes/index.tsx` | ProTable 列表 + 创建入口 |
| 文件类型创建 | `pages/FileTypes/Create.tsx` | 表单 + 提交 |
| 文件类型详情/编辑 | `pages/FileTypes/Detail.tsx` | 展示 + 编辑 + 删除 |
| 事件规则列表 | `pages/Events/index.tsx` | ProTable 列表 |
| 事件规则创建 | `pages/Events/Create.tsx` | 表单（事件类型/Webhook URL 等） |
| 事件投递历史 | `pages/Events/Deliveries.tsx` | 按规则查询投递记录 |
| Bucket 管理 | `pages/Buckets/index.tsx` | 列表 + 创建 |
| 采集器规则子页 | `pages/Agents/Rules.tsx` | ProTable 规则列表 + 创建/编辑/删除 |
| 采集器上传日志子页 | `pages/Agents/Logs.tsx` | 按 agent 查询 upload_logs |
| 上传日志页 | `pages/Logs/index.tsx` | 全局 upload_logs 列表 |
| 文件详情 | `pages/Files/Detail.tsx` | 文件元数据 + 下载链接 |
| 用户管理 | `pages/Settings/Users.tsx` | 用户列表 + 创建/编辑/删除/改密 |
| 个人信息 | `pages/Settings/Profile.tsx` | 展示当前用户信息 + 改密 |

---

## 遗留工作汇总（按优先级排序）

实施时建议按以下顺序执行，低编号任务是高编号任务的依赖。

### 第一优先级：DB 查询层（其他全依赖）
- [ ] **REM-DB1** 补充 file_entries 读查询（List + Get）
- [ ] **REM-DB2** 补充 upload_logs 读查询（List + Get）
- [ ] **REM-DB3** 补充 file_types CRUD 查询
- [ ] **REM-DB4** 补充 buckets CRUD 查询
- [ ] **REM-DB5** 补充 event_rules CRUD 查询
- [ ] **REM-DB6** 补充 event_deliveries 读查询（ListByRule）
- [ ] **REM-DB7** 补充 users Update/Delete 查询

### 第二优先级：Controlplane 组件接线
- [ ] **REM-CP1** JWT 中间件注入 auth.Service，完成黑名单检查（CP-W6）
- [ ] **REM-CP2** gRPC `RefreshCredentials` 实现（CP-W1）
- [ ] **REM-CP3** `handleUploadResult` 调用 Indexer（CP-W2）
- [ ] **REM-CP4** `Connect` 调用 `SyncRulesOnConnect`（CP-W3）
- [ ] **REM-CP5** Event Engine 添加 NATS 订阅 + Start()（CP-W5）
- [ ] **REM-CP6** Event Engine 添加后台 Retry Worker（CP-W5）
- [ ] **REM-CP7** 离线规则暂存（CP-W4）

### 第三优先级：REST Handler 实现（依赖 DB 查询层）
- [ ] **REM-H1** AgentsHandler 全部 10 端点
- [ ] **REM-H2** FilesHandler 全部 4 端点（含 MinIO presigned URL）
- [ ] **REM-H3** FileTypesHandler 全部 4 端点
- [ ] **REM-H4** BucketsHandler 全部 2 端点
- [ ] **REM-H5** EventRulesHandler 全部 5 端点
- [ ] **REM-H6** UploadLogsHandler 全部 2 端点
- [ ] **REM-H7** UsersHandler 全部 5 端点
- [ ] **REM-H8** `/internal/minio-event` webhook 处理器

### 第四优先级：Agent 组装
- [ ] **REM-AG1** agent/cmd/agent/main.go 完整组装（AG-W1）
- [ ] **REM-AG2** grpcclient 添加 `RefreshCredentials` 调用（AG-W2）

### 第五优先级：Web UI 页面实现
- [ ] **REM-UI1** FileTypes list/create/detail 页面
- [ ] **REM-UI2** Events list/create/deliveries 页面
- [ ] **REM-UI3** Buckets 管理页面
- [ ] **REM-UI4** Agent Rules 子页面（规则 CRUD）
- [ ] **REM-UI5** Agent Logs 子页面
- [ ] **REM-UI6** Upload Logs 全局页面
- [ ] **REM-UI7** Files Detail 页面
- [ ] **REM-UI8** Settings/Users 页面
- [ ] **REM-UI9** Settings/Profile 页面

### 第六优先级：集成测试补建
- [ ] **REM-IT1** STS 集成测试（controlplane/internal/storage）
- [ ] **REM-IT2** Upload Engine 集成测试（agent/internal/uploader）

---

## Phase 0 / Phase 1 合规性说明

经审计，Phase 0 和 Phase 1 所有任务的核心验收点均已满足：

| 任务 | 结论 |
|------|------|
| T0-1 proto | ✅ 编译无错，4 个 RPC 完整 |
| T0-2 迁移文件 | ✅ 12 张表、全部索引、down.sql 完整 |
| T0-3 Docker Compose | ✅ 端口固定、healthcheck 完整 |
| T0-4 MinIO 初始化脚本 | ✅ 幂等、Bucket/Lifecycle/账号/Webhook 均有 |
| T0-5 go.mod | ✅ go.work 配置正确，build 无错 |
| T1-A1~A5 | ✅ 配置/DB/Redis/gRPC 骨架/路由骨架均已实现（501 是 Phase 1 预期行为） |
| T1-B1~B4 | ✅ 配置/SQLite/gRPC 客户端/Credential Manager 均已实现 |
| T1-C1~C3 | ✅ Vite+React 骨架/路由/Zustand store/401 自动刷新均已实现 |
| T1-D1~D3 | ✅ Poetry 包结构/HTTP 层/TokenManager 均已实现 |

Phase 1 唯一遗漏点：**T1-A4** gRPC JWT 拦截器已在 Phase 2 前实现（超额完成），但 Phase 1 验收点只要求骨架，无问题。

