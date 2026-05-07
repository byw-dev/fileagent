# TASK_LIST.md — FileAgent 任务清单

> 当前阶段：**Phase 2 遗留扫除**（T2-X1~X8 需在进入 Phase 3 前完成，详见下方 **T2-X 遗留工作详细规格**）
> 状态说明：⬜ 未开始 / 🔄 进行中 / ✅ 已完成 / ❌ 阻塞

---

## 总体阶段概览

```
Phase 0  契约定义（串行，所有模块阻塞于此，需人工审查）
    ↓
Phase 1  基础骨架（4个模块可并行开工）
    ↓
Phase 2  核心业务逻辑（各模块内部串行，模块间并行）
    ↓
Phase 3  集成联调（串行）
    ↓
Phase 4  完善与收尾（可并行）
```

---

## Phase 0 — 契约定义（串行，必须人工审查通过后进入 Phase 1）

### T0-1 proto 文件 ✅
- **产出**：`proto/v1/agent.proto`
- **来源**：`docs/design/system-design.md` 第 4.3 节（直接提取）
- **验收**：
  - [x] `protoc --go_out=. --go-grpc_out=. proto/v1/agent.proto` 编译无错误
  - [x] 包含全部 4 个 RPC：Register / PollApproval / Connect / RefreshCredentials
  - [x] 包含全部消息类型及 oneof 分支
- **锁定规则**：生成后只能增字段，不能改字段编号

### T0-2 数据库迁移文件 ✅
- **产出**：`controlplane/migrations/000001_init_schema.up.sql` + `down.sql`
- **来源**：`docs/design/system-design.md` 第 3.3 节 + 第 3.4 节
- **验收**：
  - [x] 包含全部 7 个枚举类型
  - [x] 包含全部 12 张表（§3.3 DDL 实际定义 12 张，含 file_type_rules 与 event_deliveries）
  - [x] 包含全部索引（第 3.4 节）
  - [x] down.sql 能完整回滚
  - [x] 在本地 PostgreSQL 15 执行无错误

### T0-3 Docker Compose 文件 ✅
- **产出**：`deploy/docker-compose.dev.yml` / `deploy/docker-compose.test.yml`
- **验收**：
  - [x] 包含：PostgreSQL 15 / Redis 7 / MinIO / NATS 2.x（JetStream 启用）
  - [x] 所有服务配置 healthcheck
  - [x] `docker compose up -d` 后全部 healthy
  - [x] 端口固定：PG=5432 / Redis=6379 / MinIO=9000,9001 / NATS=4222,8222

### T0-4 MinIO 初始化脚本 ✅
- **产出**：`deploy/scripts/init-minio.sh`
- **来源**：`docs/design/system-design.md` 第 6.6 节
- **验收**：
  - [x] 创建 data-sensor 和 tmp-uploads Bucket
  - [x] 配置 tmp-uploads 7 天 Lifecycle
  - [x] 创建 controlplane-admin 服务账号
  - [x] 配置 Webhook 事件通知
  - [x] 脚本幂等（重复执行不报错）

### T0-5 go.mod 初始化 ✅
- **产出**：`controlplane/go.mod` / `agent/go.mod` / `go.mod`（根） / `go.work`
- **验收**：
  - [x] Go 版本：1.22（有效最低版本因传递依赖自动升为 1.24，见 DECISIONS.md D-001）
  - [x] controlplane 依赖：gin / grpc@v1.79.3 / zap / golang-migrate / testify / redis / nats / minio-go
  - [x] agent 依赖：grpc@v1.79.3 / zap / fsnotify / robfig-cron / go-sqlite3 / testify / minio-go
  - [x] `go mod tidy` + `go build ./...` 无错误（在各模块目录及根 workspace 均通过）

---

## Phase 1 — 基础骨架（Phase 0 完成后，4 组可并行）

### 组 A：Control Plane 基础层
> 依赖：T0-1 / T0-2 / T0-3 / T0-5

| 任务 | 产出 | 关键验收点 | 状态 |
|------|------|-----------|------|
| T1-A1 配置加载 | `controlplane/internal/config/` | 环境变量加载；缺必填项报错退出；单元测试 | ✅ |
| T1-A2 数据库连接层 | `controlplane/internal/db/` | 连接池；自动迁移；sqlc CRUD；集成测试 | ✅ |
| T1-A3 Redis 连接层 | `controlplane/internal/cache/` | Get/Set/Del/SetNX/Expire 封装；Key 常量；miniredis mock | ✅ |
| T1-A4 gRPC 服务端骨架 | `controlplane/internal/grpcserver/` | 实现 AgentService（Unimplemented 占位）；JWT 拦截器骨架 | ✅ |
| T1-A5 REST API 骨架 | `controlplane/internal/api/` | 所有路由注册（501 占位）；JWT 中间件；统一错误格式 | ✅ |

### 组 B：Edge Agent 基础层
> 依赖：T0-1 / T0-5

| 任务 | 产出 | 关键验收点 | 状态 |
|------|------|-----------|------|
| T1-B1 配置加载 | `agent/internal/config/` | TOML + 环境变量；`--config` 参数；单元测试 | ✅ |
| T1-B2 SQLite 本地队列 | `agent/internal/queue/` | 建表（3张）；入队/出队/更新状态；`:memory:` 单元测试 | ✅ |
| T1-B3 gRPC 客户端骨架 | `agent/internal/grpcclient/` | TLS 连接；指数退避重连；心跳 30s；grpc mock 单元测试 | ✅ |
| T1-B4 Credential Manager | `agent/internal/credential/` | AES-256-GCM 存储 Token；STS 内存存储；有效期检测；单元测试 | ✅ |

### 组 C：Web UI 基础骨架
> 依赖：无

| 任务 | 产出 | 关键验收点 | 状态 |
|------|------|-----------|------|
| T1-C1 项目初始化 | `webui/` | Vite+React18+TS；所有依赖安装；dev/build 无错误 | ✅ |
| T1-C2 基础布局与路由 | `webui/src/` | ProLayout；所有路由注册（占位）；登录页；权限守卫骨架 | ✅ |
| T1-C3 Axios + auth store | `webui/src/services/` `webui/src/store/` | Token 注入；401 自动刷新排队；Zustand store；Vitest 测试 | ✅ |

### 组 D：Python SDK 基础骨架
> 依赖：无

| 任务 | 产出 | 关键验收点 | 状态 |
|------|------|-----------|------|
| T1-D1 项目初始化 | `sdk/python/` | Poetry；包结构完整；install/pytest 可执行 | ✅ |
| T1-D2 异常类 + HTTP 层 | `sdk/python/fileagent/` | 所有异常类；httpx 封装+重试；错误码映射；respx mock 测试 | ✅ |
| T1-D3 TokenManager | `sdk/python/fileagent/auth.py` | 线程安全；<5min 自动刷新；Refresh 失效重登录；并发测试 | ✅ |

---

## Phase 2 — 核心业务逻辑（各组内部串行，组间并行）

### 组 A：Control Plane 核心功能
> 依赖：Phase 1 组 A 全部完成

| 任务 | 产出 | 关键验收点 | 状态 |
|------|------|-----------|------|
| T2-A1 JWT 认证模块 | `internal/auth/jwt.go` | 生成/验证/吊销 Token；覆盖过期/篡改/黑名单测试 | ✅ |
| T2-A2 Agent 注册审批 | `internal/agent/` | Register/PollApproval RPC；approve/revoke REST；错误场景测试 | ✅ |
| T2-A3 Agent 连接注册表 | `internal/grpcserver/registry.go` | AgentRegistry；Connect RPC；心跳刷新 Redis；断连触发 NATS | ✅ |
| T2-A4 文件索引引擎 | `internal/indexer/` | 幂等 upsert；glob 匹配 file_type；发布 NATS 事件 | ✅ |
| T2-A5 STS 凭据管理 | `internal/storage/sts.go` | 动态 Session Policy；MinIO STS；集成测试验证凭据可用 | ✅ |
| T2-A6 任务调度下发 | `internal/agent/dispatch.go` | 在线立即下发；离线暂存+重连同步；Redis 分布式锁 | ✅ |
| T2-A7 事件规则引擎 | `internal/event/` | 订阅 NATS；规则过滤；Webhook 投递+重试；Background Worker | ✅ |
| T2-A8 用户认证接口 | `internal/api/handler/auth.go` | login/refresh/logout/me 完整实现；所有错误场景测试 | ✅ |

### 组 B：Edge Agent 核心功能
> 依赖：Phase 1 组 B 全部完成

| 任务 | 产出 | 关键验收点 | 状态 |
|------|------|-----------|------|
| T2-B1 注册审批流程 | `internal/grpcclient/` | fingerprint 生成持久化；状态机完整实现；单元测试 | ✅ |
| T2-B2 File Watcher | `internal/watcher/` | inotify+ReadDirectoryChangesW；降级轮询；glob 过滤；临时目录测试 | ✅ |
| T2-B3 Scheduler | `internal/scheduler/` | cron 解析；时间变量解析；run_once_on_start；单元测试 | ✅ |
| T2-B4 Upload Engine | `internal/uploader/` | 单次/分片上传；断点续传；SHA-256；集成测试连真实 MinIO | ✅ |
| T2-B5 Task Executor | `internal/executor/` | 去重（path+mtime+size）；Worker Pool（3个）；指数退避重试 | ✅ |

### 组 C：Web UI 核心页面
> 依赖：Phase 1 组 C 完成 / T2-A8 完成

| 任务 | 关键验收点 | 状态 |
|------|-----------|------|
| T2-C1 登录页与认证 | 登录表单；Token 存 store；401 自动刷新；测试 | ✅ |
| T2-C2 仪表盘 | 4个统计卡片；折线图；SWR 30s 刷新；最近20条日志 | ✅ |
| T2-C3 采集器列表与详情 | ProTable + Tab 过滤；审批/吊销；详情页4个Tab；列目录弹窗 | ✅ |
| T2-C4 采集规则创建表单 | 3步 ProForm；Watch/Scheduled 切换；cron 实时解析；路径预览 | ✅ |
| T2-C5 文件浏览器 | 多维筛选；单文件下载；StreamSaver 批量下载+进度条 | ✅ |

### 组 D：Python SDK 核心功能
> 依赖：Phase 1 组 D 完成 / Phase 2 组 A REST API 可用

| 任务 | 关键验收点 | 状态 |
|------|-----------|------|
| T2-D1 FilesResource | list/iter/get/download/stream/batch；分页迭代器；respx mock 测试 | ✅ |
| T2-D2 其他 Resource | FileTypes / Agents / UploadLogs；单元测试 | ✅ |

---

### 集成测试前遗留工作扫除（必须在进入 Phase 3 之前完成）

> 代码审计（2026-05-03）发现 Phase 2 存在大量"包逻辑写完即打 ✅、但组装层/REST Handler/WebUI 页面从未实现"的遗留问题。
> 根本原因和各任务实现规格见下方 **T2-X 遗留工作详细规格**。

| 任务 | 内容摘要 | 状态 |
|------|---------|------|
| T2-X1 DB 查询层补全 | file_entries/upload_logs/file_types/buckets/event_rules/event_deliveries/users 读写查询 | ✅ |
| T2-X2 Controlplane 组件接线 | RefreshCredentials RPC、Indexer 接入 handleUploadResult、Dispatcher 接入 Connect、Event Engine Start+NATS 订阅+Retry Worker、JWT 中间件黑名单检查 | ✅ |
| T2-X3 离线规则暂存 | DispatchRule 离线时持久化暂存，重连后补发（T2-A6 遗漏） | ✅（已归并至 T2-X2：SyncRulesOnConnect 从 DB 全量补发，无需独立暂存队列） |
| T2-X4 REST Handler 实现 | AgentsHandler(10) / FilesHandler(4) / FileTypesHandler(4) / BucketsHandler(2) / EventRulesHandler(5) / UploadLogsHandler(2) / UsersHandler(5) / minio-event(1) 共 33 个端点 | ✅ |
| T2-X5 Agent main.go 组装 | 将 watcher/scheduler/executor/uploader/grpcclient 组装为可运行 Agent 进程 | ✅ |
| T2-X6 Agent RefreshCredentials 调用 | grpcclient 添加 RefreshCredentials；后台定时刷新 STS 凭据 | ✅ |
| T2-X7 Web UI 未实现页面 | FileTypes/Events/Buckets/AgentRules/AgentLogs/Logs/Files·Detail/Settings·Users/Settings·Profile 共 13 个 placeholder 页面 | ✅ |
| T2-X8 集成测试补建 | STS 集成测试（controlplane）+ Upload Engine 集成测试（agent）| ✅ |

---

### Phase 2 完成验证：进入 Phase 3 前的质量关卡（审计 2026-05-06，修订 2026-05-06）

> T2-X 全部完成后，代码审计发现以下问题。分为两类：
> **功能缺口**（会直接导致集成测试失败）和**测试质量**（覆盖率不达标）。
> **所有 P3-P 任务须完成后方可进入 Phase 3**，详见 `docs/reports/phase3-readiness-audit.md`。

#### 汇总表

| 任务 | 内容摘要 | 类型 | 优先级 | 状态 |
|------|---------|------|--------|------|
| P3-P1 DB 集成测试环境修复 | `fileagent_test` DB 不存在，3 个集成测试常态失败 | 测试环境 | 🔴 阻塞 | ✅ |
| P3-P2 DownloadURL bucket 名 Bug 修复 | `DownloadURL`/`BatchDownloadURLs` 用 UUID 而非 bucket 名调用 MinIO；TTL 5min → 15min | 🔴 功能 Bug | 🔴 阻塞 | ✅ |
| P3-P3 BucketsCreate 调用 MinIO Admin API | `POST /api/v1/buckets` 缺少 `madmin.MakeBucket()` 调用，MinIO 中无物理 Bucket | 🔴 功能缺口 | 🔴 阻塞 | ✅ |
| P3-P4 append_mode tail/close_wait 实现 | watcher/executor 均未处理 `tail`（offset 增量上传）和 `close_wait`（CLOSE_WRITE 触发）逻辑 | 🔴 功能缺口 | 🔴 阻塞 | ✅ |
| P3-P5 grpcclient 覆盖率补全 | 当前 61.5%（要求≥80%）；T2-X6 全部新增方法均在 0% | 测试覆盖 | 🔴 阻塞 | ✅ |
| P3-P6 event 包覆盖率补全 | 当前 62.7%（要求≥80%）；`processRetries/retryDelivery/DBAdapter/NewEngine` 均在 0% | 测试覆盖 | 🔴 阻塞 | ✅ |
| P3-P7 事件重试逻辑对齐设计 §5.9 | 退避表错误（30s×2^n 而非 30s→2min→10min→30min→2h）；无最多5次限制；上限1h非2h | 🟡 设计偏差 | 🟡 高 | ✅ |
| P3-P8 MinioEventHandler 调用 Indexer | `Handle` 只打日志，未调用 Indexer，MinIO Webhook 备用采集链路失效 | 🟡 功能缺口 | 🟡 高 | ✅ |
| P3-P9 REST handler 覆盖率补全 | `DownloadURL` 47.6%、`events.Delete` 58.3%、`MinioEventHandler.Handle` 0%、`authdb` 全部 0% | 测试覆盖 | 🟡 高 | ✅ |
| P3-P10 queue 包覆盖率补全 | 当前 79.5%（要求≥80%）；`Open` 错误路径 62.5% | 测试覆盖 | 🟢 中 | ✅ |

---

#### P3-P1 详细规格：DB 集成测试环境修复

**问题根因**：`db_integration_test.go` 默认连接 `fileagent_test` 数据库：
```go
dsn = "postgres://fileagent:fileagent@localhost:5432/fileagent_test?sslmode=disable"
```
但 `deploy/docker-compose.test.yml` 的 postgres service 设置 `POSTGRES_DB: fileagent`（非 `fileagent_test`），导致 3 个集成测试 `TestNew_ConnectsSuccessfully`、`TestMigrate_IdempotentOnCleanDB`、`TestQueriesRoundtrip_Agent` 常态失败。

**修复方案（单行变更）**：修改 `deploy/docker-compose.test.yml`：
```yaml
# 将 POSTGRES_DB: fileagent 改为：
POSTGRES_DB: fileagent_test
```
修改后重建容器（`docker compose -f deploy/docker-compose.test.yml up -d --force-recreate`），
然后对测试 DB 执行迁移：`migrate -database "postgres://fileagent:fileagent@localhost:5432/fileagent_test?sslmode=disable" -path ./migrations up`

---

#### P3-P2 详细规格：DownloadURL bucket 名 Bug + TTL 修复

**Bug 1（致命）**：`handler/files.go:216` 和 `:265`：
```go
// 当前（错误）
h.minio.PresignedGetObject(ctx, entry.BucketID.String(), entry.StoragePath, 5*time.Minute)
// 修复后
h.minio.PresignedGetObject(ctx, bucket.Name, entry.StoragePath, 15*time.Minute)
```

**Bug 2**：TTL 5min → 15min（设计 §5.11.3），`expires_in` 响应字段 300 → 900。

**修复步骤**：
1. `FilesDB` 接口（`handler/files.go`）增加方法：
   ```go
   GetBucketByID(ctx context.Context, id uuid.UUID) (*db.Bucket, error)
   ```
   （`db.Queries` 已有 `GetBucketByID` 实现，直接满足接口）
2. `DownloadURL` 和 `BatchDownloadURLs` 在 `GetFileEntryByID` 之后调用 `GetBucketByID(entry.BucketID)` 取 `bucket.Name`
3. 将两处 `5*time.Minute` 改为 `15*time.Minute`，`expires_in: 300` 改为 `expires_in: 900`
4. 更新 `stubs_test.go` 的 `mockFilesDB` 增加 `GetBucketByID` stub
5. 补充 `files_test.go` 中 presign 路径的测试用例

---

#### P3-P3 详细规格：BucketsCreate 调用 MinIO Admin API

**问题**：`BucketsHandler.Create`（`events.go:87`）只调用 `db.CreateBucket()`，不创建 MinIO 物理 Bucket，导致 Agent 上传文件时 `s3: The specified bucket does not exist`。

**修复步骤**：
1. 新增接口到 `handler/events.go`：
   ```go
   // MinioAdmin is the minimal MinIO admin interface needed to create buckets.
   type MinioAdmin interface {
       MakeBucket(ctx context.Context, bucketName, location string) error
   }
   ```
2. `BucketsHandler` 增加 `admin MinioAdmin` 字段；`NewBucketsHandler` 增加 `admin MinioAdmin` 参数
3. `Create` 方法在 DB 写入成功后调用 `h.admin.MakeBucket(ctx, req.Name, "")`；若 MinIO 失败，回滚 DB（或返回 500，由业务决定是否幂等重试）
4. `RouterConfig` 增加 `MinioAdmin MinioAdmin` 字段；`controlplane/cmd/server/main.go` 中用 `madmin.New(...)` 创建 `madmin.Client` 并注入
5. 补充 `events_test.go` 的 `BucketsHandler.Create` 测试：正常路径（MinIO OK）、MinIO 失败路径

**注意**：`github.com/minio/madmin-go/v3` 已在 controlplane 依赖中（`go.mod` 需确认），`madmin.Client.MakeBucket` 签名为 `MakeBucket(ctx, bucketName string) error`（v3 API 不需要 location 参数）。

---

#### P3-P4 详细规格：append_mode tail/close_wait 实现

**问题**：`proto/v1/agent.proto:140` 定义了 `append_mode` 字段，`agent/cmd/agent/main.go:315` 透传给 `scheduler.Rule.AppendMode`，但 watcher 和 executor 中完全无此字段的处理分支。

设计 §4.4.3 定义三种模式：
| 模式 | 实现位置 | 行为 |
|------|---------|------|
| `overwrite`（默认）| 已实现 | 每次触发完整上传 |
| `close_wait` | **待实现**：watcher | 等待 `inotify.CLOSE_WRITE` 事件而非 `WRITE` 事件触发上传 |
| `tail` | **待实现**：queue + executor | 记录上次上传的文件 offset，仅上传新增字节，以追加分片存储 |

**close_wait 修复**（`agent/internal/watcher/watcher.go`）：
- Watch 模式下，区分 `rule.AppendMode == "close_wait"` 和其他模式
- `close_wait` 时：只在 `fsnotify.CloseWrite`（等同 `inotify IN_CLOSE_WRITE`）事件触发时提交任务；忽略 `Write` 事件
- 需确认 `fsnotify.CloseWrite` 是否已在当前 fsnotify 版本（v1.8.0）中可用（Go 1.20+ 支持 `IN_CLOSE_WRITE`）

**tail 修复**（`agent/internal/queue` + `agent/internal/executor` + `agent/internal/uploader`）：
1. `queue.UploadTask` 增加 `FileOffset int64` 字段（对应 `upload_tasks` 表新增列 `file_offset INTEGER NOT NULL DEFAULT 0`）
2. `executor.processTask` 在 `tail` 模式时：读取 `task.FileOffset`，计算当前文件大小与 offset 的差值，调用 `uploader.AppendFile(ctx, task, offset)` 而非 `UploadFile`
3. `uploader` 增加 `AppendFile` 方法：以追加分片（multipart append）方式上传 `[offset, size)` 区间字节；完成后更新 `task.FileOffset = newSize`
4. 迁移文件：`controlplane/migrations/000002_add_file_offset_to_upload_tasks.up.sql`（仅 agent 的 SQLite 无需 migration，直接修改 schema const）

---

#### P3-P5 详细规格：grpcclient 单元测试补建

文件：`agent/internal/grpcclient/client_test.go`（当前 191 行，5 个测试）
已有基础设施：`fakeAgentServer`（in-process gRPC server）、`setupTest()` 帮助函数

需新增以下测试（目标：覆盖率 ≥ 80%）：

| 测试函数 | 覆盖目标 | 实现方式 |
|---------|---------|---------|
| `TestClient_SetToken_AttachedToMetadata` | `SetToken` | `SetToken("tok")` 后 `SendHeartbeat`；`fakeAgentServer` 从 `metadata.FromIncomingContext` 验证 `authorization: Bearer tok` |
| `TestClient_SetAgentID` | `SetAgentID` | `SetAgentID("ag-1")` 后调用 `RefreshCredentials`；fake server 验证 `RefreshCredentialsRequest.AgentId == "ag-1"` |
| `TestClient_SetMessageHandler_Dispatches` | `SetMessageHandler` | 注册 handler；`fakeAgentServer.Connect` 主动 `Send` 一条 `ServerMessage`；等待 handler 被调用（`chan struct{}` + timeout） |
| `TestClient_SendMessage_WithStream` | `SendMessage` | connect 后调用 `SendMessage(&agentv1.AgentMessage{...})`；验证无 error |
| `TestClient_SendMessage_NoStream` | `SendMessage` error path | 不 connect，直接 `SendMessage`；验证返回 `"no active stream"` 错误 |
| `TestClient_ServiceClient_BeforeConnect` | `ServiceClient` returns nil | `New()` 后直接 `ServiceClient()`；验证返回 nil |
| `TestClient_RefreshCredentials_Success` | `RefreshCredentials` | `fake server` `RefreshCredentials` RPC 返回 `CredentialsPayload`；验证返回值字段正确 |
| `TestClient_RefreshCredentials_NoSvc` | `RefreshCredentials` no-svc error | 不 connect，调用 `RefreshCredentials`；验证返回 "not initialised" 错误 |
| `TestBuildDialOpts_Insecure` | `buildDialOpts` insecure | `cfg.UseSSL=false`；验证 dial options 包含 `insecure.NewCredentials()` |
| `TestBuildDialOpts_TLS` | `buildDialOpts` TLS | `cfg.UseSSL=true, cfg.CACertFile=""`；验证使用系统 CA |

**fakeAgentServer 需新增**：
- `RefreshCredentials` RPC 实现（接收请求，返回固定 `CredentialsPayload`）
- `serverMessages chan *agentv1.ServerMessage` 字段，`Connect` 中 range 发送

---

#### P3-P6 详细规格：event 包覆盖率补建

**根因分析**：
- `NewEngine` 0%：`engine_extra_test.go:75` 的 `TestNewEngine` 调用了 `NewEngine`，但测试传入的 `db` 参数类型导致 coverage 工具将其统计到接口调用方，而非 `engine.go` 本身的构造函数行（待验证）
- `DBAdapter` 方法 0%：`TestNewDBAdapter`（`engine_extra_test.go:255`）只断言 `adapter != nil`，**从未调用接口方法**，5 个 wrapper 方法全部未覆盖
- `processRetries`/`retryDelivery` 0%：`retryWorker` 内用 `time.NewTicker(30*time.Second)` 异步触发，测试 goroutine 在 ticker 首次触发前即退出

**修复方案**：

1. **DBAdapter 直通测试**（新建 `engine_db_test.go`）：
   ```go
   // 对每个接口方法用 mock DB 做正常路径 + 错误路径各一个测试
   func TestDBAdapter_ListEnabledEventRules(t *testing.T) { ... }
   func TestDBAdapter_CreateEventDelivery(t *testing.T) { ... }
   // ... 5个方法共 10 个用例
   ```

2. **processRetries/retryDelivery 直接调用**：
   - `Engine` 结构体的 `processRetries` 和 `retryDelivery` 均为包级私有方法，测试文件与源文件同包（`package event`），**可直接调用**
   - 在 `engine_start_test.go` 中补充：
   ```go
   func TestProcessRetries_DirectCall(t *testing.T) {
       // 构造 Engine，mock store 返回 1 个 pending delivery
       // 直接调用 e.processRetries(ctx)，验证 retryDelivery 被调用
   }
   ```

3. **NewEngine 覆盖确认**：运行 `go tool cover -html` 确认是否真的 0%；如确实 0%，在 `TestNewEngine` 中断言 `engine.db != nil`（触发函数体执行）

---

#### P3-P7 详细规格：事件重试逻辑对齐设计 §5.9

**问题**：`event/engine.go:218-221` 当前退避计算：
```go
backoff := time.Duration(30<<uint(newAttemptCount-1)) * time.Second  // 30s,60s,120s,240s,480s
if backoff > time.Hour { backoff = time.Hour }
```

设计 §5.9 要求：**30s → 2min → 10min → 30min → 2h，最多 5 次，上限 2h**。

**修复方案**：
```go
var retrySchedule = []time.Duration{
    30 * time.Second,
    2 * time.Minute,
    10 * time.Minute,
    30 * time.Minute,
    2 * time.Hour,
}

func (e *Engine) retryDelivery(ctx context.Context, d *db.EventDelivery) error {
    // ...
    newAttemptCount := d.AttemptCount + 1

    // 达到最大重试次数：标记为 exhausted，不再重试
    if int(d.AttemptCount) >= len(retrySchedule) {
        return e.store.UpdateDelivery(ctx, indexer.UpdateEventDeliveryParams{
            ID: d.ID, Status: "exhausted", AttemptCount: newAttemptCount,
        })
    }

    sendErr := e.sender.Send(ctx, rec)
    if sendErr != nil {
        idx := int(d.AttemptCount)  // 0-indexed，对应下一次重试的等待时间
        if idx >= len(retrySchedule) { idx = len(retrySchedule) - 1 }
        nextRetryAt = sql.NullTime{Time: time.Now().Add(retrySchedule[idx]), Valid: true}
    }
    // ...
}
```

**注意**：DB schema 的 `event_deliveries.status` 枚举需增加 `exhausted` 值，或复用 `failed` 并用 `next_retry_at IS NULL` 区分"永久失败"。建议新增 migration（`000003_add_exhausted_delivery_status.up.sql`）或在 `ListPendingDeliveries` 查询中增加 `attempt_count < 5` 过滤条件（无需改 schema）。

**推荐最小改动**：`processRetries` 时 `ListPendingDeliveries` 已过滤 `next_retry_at <= now()`，只需在 `retryDelivery` 开头加：
```go
if d.AttemptCount >= 5 {
    return nil  // 已超过最大重试次数，跳过（next_retry_at 不再更新）
}
```
并修正退避表。补充 `TestRetryDelivery_ExhaustedAfter5Attempts` 测试。

---

#### P3-P8 详细规格：MinioEventHandler 调用 Indexer

**问题**：`handler/events.go:578` 的 `Handle` 方法只打日志，`MinioEventHandler` 无 Indexer 字段。

**修复步骤**：
1. `MinioEventHandler` 增加字段：
   ```go
   type MinioEventHandler struct {
       logger  *zap.Logger
       indexer IndexerClient  // 新增
   }
   // IndexerClient is the minimal Indexer interface needed by MinioEventHandler.
   type IndexerClient interface {
       IndexUpload(ctx context.Context, bucketName, objectKey string, sizeBytes int64, etag string) error
   }
   ```
2. `indexer.Indexer` 需实现 `IndexUpload(ctx, bucketName, objectKey string, sizeBytes int64, etag string) error`（当前 `IndexUpload` 参数为 `IndexUploadParams`，可增加该签名或修改接口）
3. `NewMinioEventHandler` 增加 `indexer IndexerClient` 参数；`RouterConfig` 增加 `MinioIndexer IndexerClient`；`main.go` 中注入 `ix`
4. `Handle` 在解析 Records 后调用 `h.indexer.IndexUpload(...)`；失败只记录 warn（非阻塞）
5. 补充 `events_test.go` 中 `TestMinioEventHandler_Handle_*` 测试

---

#### P3-P9 详细规格：REST handler 覆盖率补全

目标：`internal/api/handler` 整体 ≥ 80%。

| 文件 | 函数 | 当前 | 补充测试场景 |
|------|------|------|------------|
| `files_test.go` | `DownloadURL` | 47.6% | 1. file not found；2. DB error；3. presign 失败；4. 正常（用 P3-P2 修复后的 bucket 查询 mock） |
| `events_test.go` | `EventRulesHandler.Delete` | 58.3% | 1. ID 解析失败；2. DB error；3. 成功 |
| `agents_test.go` | `AgentsHandler.DeleteRule` | 62.5% | 1. dispatcher.DispatchRuleCancel 失败（仅 warn）；2. DeleteRule DB error |
| `events_test.go` | `MinioEventHandler.Handle` | 0% | 1. 正常多 Record；2. JSON 解析失败（status 200）；3. 空 Records |
| `handler/authdb_test.go`（新建）| `GetUserByUsername`/`UpdateUserLastLogin` | 0% | 用 `sqlmock` 或内存 DB 做直通测试 |
| `helpers_test.go` | `encodeCursor` / `parseLimitParam` | 0%/45% | 补充 cursor encode/decode roundtrip；limit 边界值 |

---

#### P3-P10 详细规格：queue 包覆盖率补全

目标：`agent/internal/queue` ≥ 80%（当前 79.5%）。

缺失：`queue.Open` 62.5%——当前测试只测试成功路径，未测试以下错误路径：
1. 无效 DSN（触发 `sql.Open` 报错）——需传入非 `sqlite3://` 协议的字符串
2. Schema 执行失败（传入只读目录下的 DB 路径）——可用 chmod 模拟

最小补充（`queue_test.go` 新增）：
```go
func TestOpen_InvalidDSN(t *testing.T) {
    // 传入无效驱动名触发 sql.Open 错误
    _, err := queue.Open("invalid_driver://foo")
    require.Error(t, err)
    assert.Contains(t, err.Error(), "queue: open")
}
```

---

### T2-X 遗留工作详细规格

#### 根本原因分析

| 类型 | 描述 |
|------|------|
| **验收点过粗** | Task 的验收点只写"包要实现"，没有写"组件要接入 server/binary"，执行时把包逻辑写完就打 ✅ |
| **任务拆分遗漏** | REST handler 实现、controlplane 组装、agent main.go 组装、多个 WebUI 页面均无对应任务编号 |
| **测试声称但未建** | T2-A5 / T2-B4 写了"集成测试"验收点，但实际无 `-tags=integration` 文件 |

#### T2-X4 REST Handler 实现规格（33 端点）

> 目标目录：`controlplane/internal/api/handler/`，所有 Handler 当前均为 `middleware.NotImplemented(c)` 占位

**AgentsHandler（`handler/agents.go`，10 端点）**

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/agents` | DB: `ListAgents`（已有） |
| GET `/api/v1/agents/:id` | DB: `GetAgentByID`（已有） |
| POST `/api/v1/agents/:id/approve` | `agentMgr.Approve`（已有） |
| POST `/api/v1/agents/:id/revoke` | `agentMgr.Revoke`（已有） |
| POST `/api/v1/agents/:id/list-dir` | gRPC 向在线 agent 发 ListDir 命令 |
| GET `/api/v1/agents/:id/rules` | DB: `ListCollectionRulesByAgent`（已有） |
| POST `/api/v1/agents/:id/rules` | DB: `CreateCollectionRule`（已有）→ `dispatcher.DispatchRule` |
| PUT `/api/v1/agents/:id/rules/:rid` | DB: `UpdateCollectionRuleStatus` + `dispatcher.DispatchRule` |
| DELETE `/api/v1/agents/:id/rules/:rid` | DB: `DeleteCollectionRule`（已有）→ `dispatcher.DispatchRuleCancel` |
| GET `/api/v1/agents/:id/upload-logs` | DB: `ListUploadLogs`（T2-X1 DB-2） |

**FilesHandler（`handler/files.go`，4 端点）**

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/files` | DB: `ListFileEntries`（T2-X1 DB-1） |
| GET `/api/v1/files/:id` | DB: `GetFileEntryByID`（T2-X1 DB-1） |
| GET `/api/v1/files/:id/download-url` | MinIO presigned URL（5 min 有效期） |
| POST `/api/v1/files/batch-download-urls` | MinIO 批量 presigned |

**FileTypesHandler（`handler/file_types.go`，4 端点）**

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/file-types` | DB: `ListFileTypes`（T2-X1 DB-3） |
| POST `/api/v1/file-types` | DB: `CreateFileType`（T2-X1 DB-3） |
| PUT `/api/v1/file-types/:id` | DB: `UpdateFileType`（T2-X1 DB-3） |
| DELETE `/api/v1/file-types/:id` | DB: `DeleteFileType`（T2-X1 DB-3） |

**BucketsHandler（`handler/events.go`，2 端点）**

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/buckets` | DB: `ListBuckets`（T2-X1 DB-4） |
| POST `/api/v1/buckets` | DB: `CreateBucket`（T2-X1 DB-4）+ MinIO Admin API 创建 bucket |

**EventRulesHandler（`handler/events.go`，5 端点）**

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/event-rules` | DB: `ListEventRules`（T2-X1 DB-5） |
| POST `/api/v1/event-rules` | DB: `CreateEventRule`（T2-X1 DB-5） |
| PUT `/api/v1/event-rules/:id` | DB: `UpdateEventRule`（T2-X1 DB-5） |
| DELETE `/api/v1/event-rules/:id` | DB: `DeleteEventRule`（T2-X1 DB-5） |
| GET `/api/v1/event-rules/:id/deliveries` | DB: `ListDeliveriesByRule`（T2-X1 DB-6） |

**UploadLogsHandler（`handler/events.go`，2 端点）**

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/upload-logs` | DB: `ListUploadLogs`（T2-X1 DB-2） |
| GET `/api/v1/upload-logs/:id` | DB: `GetUploadLogByID`（T2-X1 DB-2） |

**UsersHandler（`handler/users.go`，5 端点）**

| 端点 | 关键依赖 |
|------|---------|
| GET `/api/v1/users` | DB: `ListUsers`（已有） |
| POST `/api/v1/users` | DB: `CreateUser`（已有）；密码 bcrypt hash |
| PUT `/api/v1/users/:id` | DB: `UpdateUser`（T2-X1 DB-7） |
| DELETE `/api/v1/users/:id` | DB: `DeleteUser`（T2-X1 DB-7） |
| POST `/api/v1/users/:id/password` | DB: `UpdateUserPassword`（已有） |

**MinIO Event Webhook（`handler/events.go`，1 端点）**

| 端点 | 关键依赖 |
|------|---------|
| POST `/internal/minio-event` | 解析 MinIO Webhook 事件 → 调用 `ix.IndexUpload`（备选链路） |

#### T2-X5 Agent main.go 组装规格（`agent/cmd/agent/main.go`）

当前文件仅 `fmt.Println("fileagent agent")`，需实现 10 步完整进程组装：

1. `config.Load()` — 加载配置
2. 初始化 zap logger
3. `queue.New()` — SQLite 队列
4. `credential.NewTokenManager()` + `STSManager` — 凭据管理
5. `grpcclient.New()` — gRPC 客户端（TLS + 指数退避）
6. `Lifecycle.Run()` — 注册审批生命周期，阻塞直到 APPROVED
7. `executor.New()` + `Start()` — Worker Pool（3 个）
8. 启动 gRPC `Connect` 流，处理 `ServerMessage`（PushRule → Watcher/Scheduler；RevokeME → 清除 Token；ListDir → 响应）
9. 后台 STS 刷新 goroutine（距到期 10 min 调用 T2-X6）
10. `signal.NotifyContext(SIGINT/SIGTERM)` 优雅退出

#### T2-X6 Agent RefreshCredentials 规格（`agent/internal/grpcclient/client.go`）

在已有 `Connect()` / `SendHeartbeat()` 的 `Client` 中添加：

```go
// RefreshCredentials calls the Control Plane to obtain fresh STS credentials.
func (c *Client) RefreshCredentials(ctx context.Context) (*agentv1.CredentialsPayload, error)
```

#### T2-X7 Web UI 未实现页面规格（`webui/src/pages/`）

| 编号 | 文件 | 所需功能摘要 |
|------|------|------------|
| UI-1 | `FileTypes/index.tsx` | ProTable 列表 + 创建入口（GET /api/v1/file-types） |
| UI-2 | `FileTypes/Create.tsx` | 表单（name / glob_patterns / description）+ 提交 |
| UI-3 | `FileTypes/Detail.tsx` | 详情展示 + 编辑 + 删除 |
| UI-4 | `Events/index.tsx` | ProTable 事件规则列表 |
| UI-5 | `Events/Create.tsx` | 表单（event_type / webhook_url / 过滤条件） |
| UI-6 | `Events/Deliveries.tsx` | 按规则查询投递记录（GET /api/v1/event-rules/:id/deliveries） |
| UI-7 | `Buckets/index.tsx` | Bucket 列表 + super_admin 创建 |
| UI-8 | `Agents/Rules.tsx` | 采集器规则子页，ProTable + 创建/编辑/删除 |
| UI-9 | `Agents/Logs.tsx` | 采集器上传日志子页（GET /api/v1/agents/:id/upload-logs） |
| UI-10 | `Logs/index.tsx` | 全局上传日志（GET /api/v1/upload-logs） |
| UI-11 | `Files/Detail.tsx` | 文件元数据详情 + 预签名下载链接 |
| UI-12 | `Settings/Users.tsx` | 用户列表 + 创建/编辑/删除/改密 |
| UI-13 | `Settings/Profile.tsx` | 当前用户信息展示 + 改密 |

#### T2-X8 集成测试补建规格

| 编号 | 文件 | Build Tag | 测试内容 |
|------|------|-----------|---------|
| IT-1 | `controlplane/internal/storage/sts_integration_test.go` | `//go:build integration` | 连接 `deploy/docker-compose.test.yml` MinIO，验证 `IssueCredentials` 返回可用凭据，STS AssumeRole 能实际读写 bucket |
| IT-2 | `agent/internal/uploader/uploader_integration_test.go` | `//go:build integration` | 连接真实 MinIO，验证单次上传（<5 MB）和分片上传（>5 MB）均成功，SHA-256 校验一致，断点续传能续传 |

---

## Phase 3 — 集成联调（串行，依赖 Phase 2 全部完成）

### T3-1 Control Plane + Agent 端到端联调 ⬜
- [ ] Agent 注册 → 审批 → 建立 gRPC 连接
- [ ] 下发采集规则 → Watch 模式 → 文件上传 MinIO → file_entries 写入
- [ ] 模拟网络中断 → OFFLINE → 本地队列工作 → 重连后补传
- [ ] 吊销 Agent → 收到 RevokeCommand → 清除 Token

### T3-2 Web UI + Control Plane 联调 ⬜
- [ ] 登录 → 仪表盘数据正确
- [ ] 审批采集器 → 采集器状态更新
- [ ] 创建采集规则 → 规则下发到 Agent
- [ ] 文件浏览器搜索并下载文件

### T3-3 Python SDK + Control Plane 联调 ⬜
- [ ] 登录 → 查询 → 分页迭代 → 下载文件
- [ ] Token 自动刷新验证

---

## Phase 4 — 完善与收尾（可并行）

| 任务 | 内容 | 状态 |
|------|------|------|
| T4-1 监控配置 | Prometheus 抓取 + Grafana Dashboard + 告警规则 + Loki/Promtail | ⬜ |
| T4-2 CI 配置 | GitHub Actions：controlplane / agent / webui / sdk-python | ⬜ |
| T4-3 部署脚本与文档 | systemd service 文件；Windows 安装脚本；README | ⬜ |
| T4-4 Java SDK | OkHttp3 + Jackson + Lombok；JUnit 5 + MockWebServer | ⬜ |

---

## 进度追踪

| Phase | 任务数 | 完成数 | 进度 |
|-------|--------|--------|------|
| Phase 0 | 5 | 5 | 100% |
| Phase 1 | 15 | 15 | 100% |
| Phase 2 核心 | 20 | 20 | 100%（含组件包逻辑）|
| Phase 2 遗留（T2-X） | 8 | 8（全部完成）| 100% |
| Phase 3 前质量关卡（P3-P） | 10 | 10 | 100% |
| Phase 3 | 3 | 0 | 0% |
| Phase 4 | 4 | 0 | 0% |
| **合计** | **65** | **54** | **83%** |

---

## 给 AI Agent 的操作说明

开始每个任务前，按顺序执行：

1. 阅读 `CLAUDE.md`（项目规范与技术栈约定）
2. 阅读 `DECISIONS.md`（已有技术决策，避免重复决策）
3. 阅读 `docs/design/system-design.md` 对应章节
4. 确认前置依赖任务已完成（状态为 ✅）
5. 实现代码
6. 编写测试，运行测试，确认全部通过
7. 运行构建命令确认无编译错误
8. 将本文件中对应任务状态更新为 ✅
9. 如有新的技术决策，写入 `DECISIONS.md`
