# Phase 3 准入审计任务清单

> 审计日期：2026-05-04
> 审计结论：**不满足进入 Phase 3 的条件**
> T2-X 系列（8项）全部处于 ⬜ 未开始状态，详见下文。
>
> 本文是对 `docs/PHASE2_REMEDIATION.md` 的细化落地版本，
> 按实现依赖顺序排列，每条任务标注目标文件与关键改动点。

---

## 审计总览

| 类别 | 总项 | 已完成 | 占位/未实现 |
|------|------|--------|------------|
| Control Plane 组件接线 | 6 | 0 | 6 |
| Control Plane DB 查询层 | 13 条查询 | 部分写路径 | 缺所有读路径及 CRUD |
| Control Plane REST Handler | 35 端点 | 4（Auth） | 31 全为 NotImplemented |
| Agent 组装层 | 2 | 0 | 2 |
| Web UI 页面 | 13 | 0 | 13 全为占位 |
| 集成测试 | 2 | 0 | 2 |

---

## 第一优先级：DB 查询层（全部其他任务的依赖）

> 目标目录：`controlplane/internal/db/queries/`（sqlc）或沿用 `indexer/queries.go` 手写风格
> 注意：写路径（UpsertFileEntry / CreateUploadLog 等）已在 `indexer/queries.go` 实现，勿重复。

| 编号 | 任务 | 目标函数 | 现有状态 |
|------|------|---------|---------|
| DB-1 | file_entries 读路径 | `ListFileEntries(ctx, orgID, cursor, filters)` / `GetFileEntryByID(ctx, id)` | 缺失 |
| DB-2 | upload_logs 读路径 | `ListUploadLogs(ctx, agentID, cursor)` / `GetUploadLogByID(ctx, id)` | 缺失 |
| DB-3 | file_types CRUD | `ListFileTypes` / `CreateFileType` / `UpdateFileType` / `DeleteFileType` | 缺失 |
| DB-4 | buckets CRUD | `ListBuckets` / `CreateBucket` | 缺失 |
| DB-5 | event_rules 管理 CRUD | `ListEventRules`（全量，非仅 enabled）/ `CreateEventRule` / `UpdateEventRule` / `DeleteEventRule` | `ListEnabledEventRules` 已有（内部用），管理接口查询缺失 |
| DB-6 | event_deliveries 读 | `ListDeliveriesByRule(ctx, ruleID, cursor)` | `ListPendingEventDeliveries` 已有（重试用），按 rule 分页查询缺失 |
| DB-7 | users 管理操作 | `UpdateUser(ctx, id, params)`（改 email/profile 等）/ `DeleteUser(ctx, id)` | `UpdateUserPassword` / `UpdateUserActive` 已有，通用 Update/Delete 缺失 |

---

## 第二优先级：Control Plane 组件接线

> 目标文件：
> - `controlplane/internal/grpcserver/handler.go`（gRPC 实现）
> - `controlplane/cmd/server/main.go`（组装层，当前所有组件均被 `_ = x` 丢弃）
> - `controlplane/internal/middleware/jwt.go`（JWT 中间件）
> - `controlplane/internal/event/engine.go`（事件引擎）

| 编号 | 任务 | 目标文件:行 | 改动摘要 |
|------|------|-----------|---------|
| CP-1 | JWT 中间件黑名单检查 | `middleware/jwt.go:72`（TODO 行） | 注入 `auth.Service`，调用 `IsRevoked(ctx, claims.ID)`，true 时返回 401 |
| CP-2 | gRPC `RefreshCredentials` 实现 | `grpcserver/handler.go:104` | 替换 Unimplemented：验证 JWT → 调用 `stsMgr.IssueCredentials` → 返回凭据；`cmd/server/main.go` 将 `_ = stsMgr` 改为注入 Server |
| CP-3 | `handleUploadResult` 调用 Indexer | `grpcserver/handler.go:136` | 注入 Indexer，调用 `ix.IndexUpload(ctx, agentID, result)`；`cmd/server/main.go` 将 `_ = ix` 改为注入 Server |
| CP-4 | `Connect` 调用 `SyncRulesOnConnect`（含 CP-W4） | `grpcserver/handler.go`（Connect 方法） | Agent 连接成功后调用 `dispatcher.SyncRulesOnConnect(ctx, agentID)`；`cmd/server/main.go` 将 `_ = dispatcher` 改为注入 Server |
| CP-5 | Event Engine 添加 `Start()` + NATS 订阅 | `event/engine.go` | 新增 `Start(ctx, natsConn)` 方法，订阅 `events.file.uploaded` / `events.agent.*` 主题，调用 `HandleEvent` 分发 |
| CP-6 | Event Engine 后台 Retry Worker | `event/engine.go` | 在 `Start` 内启动 goroutine，定期调用 `ListPendingEventDeliveries` 重试失败投递；`cmd/server/main.go` 将 `_ = eventEngine` 改为 `eventEngine.Start(ctx, natsConn)` |

---

## 第三优先级：REST Handler 实现（依赖 DB 查询层）

> 目标目录：`controlplane/internal/api/handler/`
> 所有 Handler 当前均为 `middleware.NotImplemented(c)` 占位。

### AgentsHandler（`handler/agents.go`，10 端点）

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/agents` | DB: `ListAgents`（已有） |
| GET `/api/v1/agents/:id` | DB: `GetAgentByID`（已有） |
| POST `/api/v1/agents/:id/approve` | `agentMgr.Approve`（已有） |
| POST `/api/v1/agents/:id/revoke` | `agentMgr.Revoke`（已有） |
| POST `/api/v1/agents/:id/list-dir` | gRPC 向在线 agent 发 ListDir 命令（需实现 ServerMessage 下发） |
| GET `/api/v1/agents/:id/rules` | DB: `ListCollectionRulesByAgent`（已有） |
| POST `/api/v1/agents/:id/rules` | DB: `CreateCollectionRule`（已有）→ `dispatcher.DispatchRule` |
| PUT `/api/v1/agents/:id/rules/:rid` | DB: `UpdateCollectionRuleStatus` 扩展 + `dispatcher.DispatchRule` |
| DELETE `/api/v1/agents/:id/rules/:rid` | DB: `DeleteCollectionRule`（已有）→ `dispatcher.DispatchRuleCancel` |
| GET `/api/v1/agents/:id/upload-logs` | DB: `ListUploadLogs`（DB-2，需先完成） |

### FilesHandler（`handler/files.go`，4 端点）

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/files` | DB: `ListFileEntries`（DB-1，需先完成）|
| GET `/api/v1/files/:id` | DB: `GetFileEntryByID`（DB-1，需先完成）|
| GET `/api/v1/files/:id/download-url` | MinIO presigned URL（5min 有效期） |
| POST `/api/v1/files/batch-download-urls` | MinIO 批量 presigned |

### FileTypesHandler（`handler/files.go`，4 端点）

| 端点 | 关键依赖 |
|------|---------|
| GET/POST/PUT/DELETE `/api/v1/file-types` | DB: `ListFileTypes` / `CreateFileType` / `UpdateFileType` / `DeleteFileType`（DB-3，需先完成）|

### BucketsHandler（`handler/events.go`，2 端点）

| 端点 | 关键依赖 |
|------|---------|
| GET/POST `/api/v1/buckets` | DB: `ListBuckets` / `CreateBucket`（DB-4，需先完成）；POST 调用 MinIO Admin API 创建 bucket |

### EventRulesHandler（`handler/events.go`，5 端点）

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/event-rules` | DB: `ListEventRules`（DB-5，需先完成）|
| POST `/api/v1/event-rules` | DB: `CreateEventRule`（DB-5）|
| PUT `/api/v1/event-rules/:id` | DB: `UpdateEventRule`（DB-5）|
| DELETE `/api/v1/event-rules/:id` | DB: `DeleteEventRule`（DB-5）|
| GET `/api/v1/event-rules/:id/deliveries` | DB: `ListDeliveriesByRule`（DB-6）|

### UploadLogsHandler（`handler/events.go`，2 端点）

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/upload-logs` | DB: `ListUploadLogs`（DB-2，需先完成）|
| GET `/api/v1/upload-logs/:id` | DB: `GetUploadLogByID`（DB-2）|

### UsersHandler（`handler/users.go`，5 端点）

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/users` | DB: `ListUsers`（已有）|
| POST `/api/v1/users` | DB: `CreateUser`（已有）；密码 bcrypt hash |
| PUT `/api/v1/users/:id` | DB: `UpdateUser`（DB-7，需先完成）|
| DELETE `/api/v1/users/:id` | DB: `DeleteUser`（DB-7，需先完成）|
| POST `/api/v1/users/:id/password` | DB: `UpdateUserPassword`（已有）|

### MinIO Event Webhook（`handler/events.go`，1 端点）

| 端点 | 关键依赖 |
|------|---------|
| POST `/internal/minio-event` | 解析 MinIO Webhook 事件 → 调用 `ix.IndexUpload`（备选链路） |

---

## 第四优先级：Agent 组装层

### AG-1：`agent/cmd/agent/main.go` 完整组装

当前文件内容仅 `fmt.Println("fileagent agent")`，需实现完整进程组装：

1. `config.Load()` — 加载配置
2. 初始化 zap logger
3. `queue.New()` — SQLite 队列
4. `credential.NewTokenManager()` + `STSManager` — 凭据管理
5. `grpcclient.New()` — gRPC 客户端（TLS + 指数退避）
6. 注册审批生命周期 `Lifecycle.Run()` — 阻塞直到 APPROVED
7. `executor.New()` + `Start()` — Worker Pool（3个）
8. 启动 gRPC `Connect` 流，处理 `ServerMessage`（PushRule → Watcher/Scheduler，RevokeME → 清除 Token，ListDir → 响应）
9. 后台 STS 刷新 goroutine（距到期 10min 调用 AG-2）
10. `signal.NotifyContext(SIGINT/SIGTERM)` 优雅退出

### AG-2：`agent/internal/grpcclient/client.go` 添加 `RefreshCredentials`

当前仅有 `Connect()` 和 `SendHeartbeat()`，缺 `RefreshCredentials` 方法：

```go
// RefreshCredentials calls the Control Plane to obtain fresh STS credentials.
func (c *Client) RefreshCredentials(ctx context.Context) (*agentv1.CredentialsPayload, error)
```

---

## 第五优先级：Web UI 页面实现

> 目标目录：`webui/src/pages/`
> 所有页面当前内容均为 `<Typography>页面开发中（Phase 2）</Typography>` 占位。

| 编号 | 文件 | 所需功能摘要 |
|------|------|------------|
| UI-1 | `FileTypes/index.tsx` | ProTable 列表 + 创建入口（调用 GET /api/v1/file-types） |
| UI-2 | `FileTypes/Create.tsx` | 表单（name / glob_patterns / description）+ 提交 |
| UI-3 | `FileTypes/Detail.tsx` | 详情展示 + 编辑 + 删除 |
| UI-4 | `Events/index.tsx` | ProTable 事件规则列表 |
| UI-5 | `Events/Create.tsx` | 表单（event_type / webhook_url / 过滤条件） |
| UI-6 | `Events/Deliveries.tsx` | 按规则查询投递记录（调用 GET /api/v1/event-rules/:id/deliveries） |
| UI-7 | `Buckets/index.tsx` | Bucket 列表 + super_admin 创建 |
| UI-8 | `Agents/Rules.tsx` | 采集器规则子页，ProTable + 创建/编辑/删除 |
| UI-9 | `Agents/Logs.tsx` | 采集器上传日志子页（调用 GET /api/v1/agents/:id/upload-logs） |
| UI-10 | `Logs/index.tsx` | 全局上传日志（调用 GET /api/v1/upload-logs） |
| UI-11 | `Files/Detail.tsx` | 文件元数据详情 + 预签名下载链接 |
| UI-12 | `Settings/Users.tsx` | 用户列表 + 创建/编辑/删除/改密 |
| UI-13 | `Settings/Profile.tsx` | 当前用户信息展示 + 改密 |

---

## 第六优先级：集成测试补建

| 编号 | 文件（需新建） | Build Tag | 测试内容 |
|------|-------------|-----------|---------|
| IT-1 | `controlplane/internal/storage/sts_integration_test.go` | `//go:build integration` | 连接 `deploy/docker-compose.test.yml` MinIO，验证 `IssueCredentials` 返回可用凭据，STS AssumeRole 能实际读写 bucket |
| IT-2 | `agent/internal/uploader/uploader_integration_test.go` | `//go:build integration` | 连接真实 MinIO，验证单次上传（<5MB）和分片上传（>5MB）均成功，SHA-256 校验一致，断点续传能续传 |

---

## 对应 TASK_LIST.md T2-X 任务映射

| T2-X 任务 | 本文档对应编号 | 预估工作量 |
|----------|-------------|---------|
| T2-X1 DB 查询层补全 | DB-1 ~ DB-7 | 中（sqlc .sql 文件 + go generate） |
| T2-X2 Controlplane 组件接线 | CP-1 ~ CP-6 | 中（主要是接线，逻辑已实现） |
| T2-X3 离线规则暂存 | **已归并至 CP-4**（不需单独实现） | — |
| T2-X4 REST Handler 实现 | AgentsH / FilesH / FileTypesH / BucketsH / EventRulesH / UploadLogsH / UsersH / MinioEvent | 大（33 端点） |
| T2-X5 Agent main.go 组装 | AG-1 | 大（完整进程组装） |
| T2-X6 Agent RefreshCredentials | AG-2 | 小 |
| T2-X7 Web UI 未实现页面 | UI-1 ~ UI-13 | 大（13 个页面） |
| T2-X8 集成测试补建 | IT-1 / IT-2 | 小 |

---

## 建议实施顺序

```
DB-1~DB-7（查询层）
    ↓
CP-1~CP-6（接线，可与 DB 并行推进）
    ↓
AgentsH / FilesH / FileTypesH / BucketsH / EventRulesH / UploadLogsH / UsersH / MinioEvent（REST Handler）
    ↓
AG-1 / AG-2（Agent 组装）
    ↓
IT-1 / IT-2（集成测试）
    ↓
UI-1~UI-13（Web UI，可与以上并行，等后端接口就绪后联调）
    ↓
进入 Phase 3 集成联调
```
