# changelog.md — 历史记录

> 本文件仅供人工阅读，记录各阶段完成情况。Agent 无需读取。

---

## 2026-05-07 — T3-1 全链路与 Agent 生命周期修复完成

**提交**：`e295c05` — Fix T3-1-FIX and T3-1-BUGFIX lifecycle and gRPC registration issues

### 完成的工作

**T3-1 Control Plane + Agent 端到端联调（最终完成）**

| 子任务 | 状态 |
|--------|------|
| CP 侧：Register / PollApproval / Connect 处理器逻辑 | ✅ |
| CP 侧：规则下发（SyncRulesOnConnect → PushRuleCommand） | ✅ |
| CP 侧：断流重连后规则重新同步 + UploadResult 索引 | ✅ |
| CP 侧：RevokeCommand 下发 | ✅ |
| CP 侧：`POST /api/v1/agents/:id/revoke` 在线下发 RevokeCommand | ✅ |
| Agent 侧：收到 RevokeCommand 后清理 Token/STS 并优雅退出 | ✅ |

**T3-1-FIX Agent 生命周期健壮性修复**

| 修复项 | 代码位置 | 状态 |
|--------|---------|------|
| FIX-1：Register 加指数退避重试（PermissionDenied/AlreadyExists 才停） | `agent/internal/grpcclient/registration.go`：`registerWithRetry` | ✅ |
| FIX-2：Connect runLoop 延后启动（lc.Start 完成并 SetToken 后才 Connect） | `agent/cmd/agent/main.go`：248→256→260 顺序 | ✅ |
| FIX-3：lc.Start 失败不再 logger.Fatal，改为 logger.Error + return | `agent/cmd/agent/main.go` | ✅ |
| FIX-4：补充 Agent 侧生命周期单元测试 | `agent/internal/grpcclient/registration_test.go` | ✅ |

**T3-1-BUGFIX gRPC 注册链路三个关键 Bug 修复**

| Bug | 根因摘要 | 代码位置 | 状态 |
|-----|---------|---------|------|
| Bug A：Register ErrNoRows 判断逻辑颠倒 | sqlc 返回非 nil 指针 + `if existing != nil` 永为 true | `manager.go:80-83`：改为 `if err == nil` 判断 | ✅ |
| Bug B：PollApproval 审批通过后不返回 auth_token | PollApproval 响应体缺 auth_token 字段 | `manager.go:198-214`：approved 时生成并返回 JWT | ✅ |
| Bug C：gRPC 反射服务未豁免 JWT 认证 | 反射端点不在 jwtExemptMethods 白名单 | `interceptor.go:34-35`：添加 v1/v1alpha 反射端点豁免 | ✅ |

---

## 2026-05-06 — Phase 3 前置质量关卡全部通过（P3-P1~P3-P10）

所有 10 项 P3-P 质量门任务完成，Phase 3 正式开始。

详细 Bug 列表见 `docs/tasks/bugs/closed.md`。

---

## 2026-05-03~05 — Phase 2 遗留工作扫除完成（T2-X1~T2-X8）

| 任务 | 内容摘要 |
|------|---------|
| T2-X1 | DB 查询层补全（file_entries/upload_logs/file_types/buckets/event_rules/event_deliveries/users） |
| T2-X2 | Controlplane 组件接线（RefreshCredentials RPC、Indexer、Dispatcher、Event Engine、JWT 黑名单） |
| T2-X3 | 离线规则暂存（已归并至 T2-X2：SyncRulesOnConnect 全量补发） |
| T2-X4 | REST Handler 实现（33 个端点：Agents/Files/FileTypes/Buckets/EventRules/UploadLogs/Users/MinioEvent） |
| T2-X5 | Agent main.go 组装（10 步完整进程组装） |
| T2-X6 | Agent RefreshCredentials 调用（grpcclient + 后台定时刷新 STS 凭据） |
| T2-X7 | Web UI 未实现页面（13 个 placeholder 页面） |
| T2-X8 | 集成测试补建（STS 集成测试 + Upload Engine 集成测试） |

---

## 2026-04-30~05-02 — Phase 2 核心业务逻辑完成

### Control Plane（组 A）
T2-A1 JWT 认证模块 / T2-A2 Agent 注册审批 / T2-A3 Agent 连接注册表 / T2-A4 文件索引引擎 / T2-A5 STS 凭据管理 / T2-A6 任务调度下发 / T2-A7 事件规则引擎 / T2-A8 用户认证接口

### Edge Agent（组 B）
T2-B1 注册审批流程 / T2-B2 File Watcher / T2-B3 Scheduler / T2-B4 Upload Engine / T2-B5 Task Executor

### Web UI（组 C）
T2-C1 登录页与认证 / T2-C2 仪表盘 / T2-C3 采集器列表与详情 / T2-C4 采集规则创建表单 / T2-C5 文件浏览器

### Python SDK（组 D）
T2-D1 FilesResource / T2-D2 其他 Resource（FileTypes/Agents/UploadLogs）

---

## 2026-04-28~30 — Phase 1 基础骨架完成

### Control Plane（组 A）
T1-A1 配置加载 / T1-A2 数据库连接层 / T1-A3 Redis 连接层 / T1-A4 gRPC 服务端骨架 / T1-A5 REST API 骨架

### Edge Agent（组 B）
T1-B1 配置加载 / T1-B2 SQLite 本地队列 / T1-B3 gRPC 客户端骨架 / T1-B4 Credential Manager

### Web UI（组 C）
T1-C1 项目初始化 / T1-C2 基础布局与路由 / T1-C3 Axios + auth store

### Python SDK（组 D）
T1-D1 项目初始化 / T1-D2 异常类 + HTTP 层 / T1-D3 TokenManager

---

## 2026-04-26~28 — Phase 0 契约定义完成

| 任务 | 产出 |
|------|------|
| T0-1 proto 文件 | `proto/v1/agent.proto`（4 个 RPC，含全部消息类型） |
| T0-2 数据库迁移文件 | `controlplane/migrations/000001_init_schema.up/down.sql`（7 枚举，12 表，全部索引） |
| T0-3 Docker Compose 文件 | `deploy/docker-compose.dev.yml` / `deploy/docker-compose.test.yml` |
| T0-4 MinIO 初始化脚本 | `deploy/scripts/init-minio.sh` |
| T0-5 go.mod 初始化 | Go Workspace（go.work）；有效最低版本 1.24（见 DECISIONS.md D-001） |
