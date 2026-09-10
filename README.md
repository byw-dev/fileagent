# FileAgent — 文件采集同步存储分发系统

一套完全私有化部署的文件采集、同步、存储与分发平台。边缘设备上的 Agent 采集文件后直传 MinIO 对象存储，Control Plane 负责管理调度与指令下发，Web UI 提供管理界面，Client SDK 供内部系统集成。所有组件均可私有化部署，无外部依赖。

---

## 整体架构

```
┌─────────────────────────────────────────────────────────────────┐
│                      私有化部署边界                              │
│                                                                 │
│  ┌──────────────┐   HTTPS    ┌──────────────────────────────┐  │
│  │   Web UI     │──────────►│       Control Plane (Go)      │  │
│  │ React/AntD   │           │  - Agent 注册审批 & 连接管理   │  │
│  └──────────────┘           │  - 任务调度 & 指令下发         │  │
│                             │  - 文件索引 & 查询 API         │  │
│  ┌──────────────┐   HTTPS   │  - 事件规则引擎                │  │
│  │ Client SDK   │──────────►│  - STS 凭据管理 & 轮转         │  │
│  │ Python/Java  │           └───────┬──────────┬─────────────┘  │
│  └──────────────┘                   │          │                │
│                            gRPC/TLS │          │ Admin API      │
│  ┌──────────────┐                   │          │                │
│  │  Edge Agent  │◄──────────────────┘   ┌──────▼──────┐        │
│  │  (Go 单二进制)│                        │    MinIO    │        │
│  │  Win/Linux   │  S3 API/TLS(直传)      │  对象存储   │        │
│  └──────────────┘──────────────────────►│  MNMD集群   │        │
│                                          └──────┬──────┘        │
│  ┌───────────────────────────────────┐          │ 事件通知       │
│  │  PostgreSQL  │ Redis │ NATS       │◄─────────┘               │
│  │  主数据库    │ 缓存  │ 事件总线   │                           │
│  └───────────────────────────────────┘                          │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │        Prometheus + Grafana + Loki + Alertmanager         │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
```

**控制平面与数据平面分离：**

- **控制平面**：Agent ↔ Control Plane 走 gRPC/TLS；Web UI / SDK → Control Plane 走 HTTPS REST。
- **数据平面**：Agent 获得短期 STS 凭据后，直接向 MinIO 上传，文件内容不经过 Control Plane 中转。

---

## 功能模块

### Control Plane（`controlplane/`）

Go 服务，是系统的核心枢纽。

| 子模块 | 说明 |
|--------|------|
| gRPC 服务端 | 接收 Agent 注册、心跳、上传结果上报；下发采集任务和吊销指令 |
| Agent 注册审批 | 维护 Agent 生命周期状态机（PENDING → APPROVED → RUNNING → OFFLINE/REVOKED） |
| 任务调度 | 下发采集规则；在线立即推送，离线暂存至 Redis 并在重连后同步 |
| 文件索引引擎 | 按路径规则归类文件条目，写入 PostgreSQL，幂等 upsert |
| STS 凭据管理 | 为 Agent 生成短期 MinIO STS 凭据（TTL 1h），到期前自动轮转 |
| 事件规则引擎 | 订阅 NATS 事件，按规则过滤并投递 Webhook，支持重试 |
| REST API | `/api/v1/...` 资源接口 + `/api/auth/...` 认证接口，JWT Bearer 认证 |
| 用户认证 | 内置账号 + JWT，预留 OIDC 扩展 |

技术栈：Go 1.22+ / Gin / gRPC / sqlc / PostgreSQL / Redis / NATS / zap

### Edge Agent（`agent/`）

Go 单二进制，运行于 Windows / Linux 边缘设备。

| 子模块 | 说明 |
|--------|------|
| gRPC 客户端 | TLS 连接；指数退避重连；30s 心跳保活 |
| File Watcher | inotify（Linux）/ ReadDirectoryChangesW（Windows）+ 降级定时轮询 |
| Scheduler | cron 表达式驱动的定时采集任务 |
| Upload Engine | ≤64MB 单次上传，>64MB 分片上传（64MB/片），断点续传状态存 SQLite |
| Task Executor | 去重（path+mtime+size）；3 个 Worker 并发；指数退避重试 |
| Credential Manager | AES-256-GCM 持久化 JWT Token；STS 凭据内存存储 + 有效期检测 |
| SQLite 本地队列 | 离线时持续写入，重连后补传，保证不丢文件 |

技术栈：Go 1.22+ / gRPC / fsnotify / robfig-cron / go-sqlite3 / minio-go / zap

### Web UI（`webui/`）

React 管理后台，供管理员操作。

| 页面 | 说明 |
|------|------|
| 登录页 | JWT 认证，Token 自动刷新 |
| 仪表盘 | 统计卡片、采集趋势折线图、最近日志 |
| 采集器管理 | 列表/详情/审批/吊销；采集器状态实时显示 |
| 采集规则 | 3 步表单创建；Watch/Scheduled 模式切换；cron 实时预览 |
| 文件浏览器 | 多维筛选、单文件下载、StreamSaver 批量下载 |

技术栈：Vite / React 19 / TypeScript / Ant Design 5 + ProComponents / Zustand / Axios + SWR / Vitest

### Python SDK（`sdk/python/`）

供内部系统集成，提供文件查询与下载能力。

| 模块 | 说明 |
|------|------|
| `auth.py` | TokenManager：线程安全，<5min 自动刷新，Refresh 失效时重新登录 |
| `http.py` | httpx 封装，自动重试，统一错误码映射 |
| `files.py` | list/get/download/stream/batch；Cursor-based 分页迭代器 |
| 其他 Resource | FileTypes / Agents / UploadLogs |

技术栈：Python 3.10+ / httpx / Poetry / pytest + respx

### Java SDK（`sdk/java/`）

（规划中，Phase 4 实现）

技术栈：Java 11+ / OkHttp3 / Jackson / Gradle / JUnit 5

### 基础设施（`deploy/`）

| 文件 | 用途 |
|------|------|
| `docker-compose.dev.yml` | 本地开发环境（PostgreSQL / Redis / MinIO / NATS） |
| `docker-compose.test.yml` | 集成测试专用环境（固定端口，isolated） |
| `scripts/init-minio.sh` | 初始化 MinIO：创建 Bucket、配置 Lifecycle、创建最小权限 IAM 用户、配置 Webhook 并自检 STS/预签名下载 |

---

## 仓库结构

```
fileagent/
├── CLAUDE.md                     # 项目规范与技术栈约定（必读）
├── TASK_LIST.md                  # 任务清单与进度
├── DECISIONS.md                  # 技术决策记录
├── Makefile                      # 统一构建入口
├── go.work                       # Go workspace
├── go.mod                        # 根模块
│
├── proto/v1/agent.proto          # gRPC 协议定义（共享契约，只增不改）
│
├── controlplane/                 # Control Plane 服务（Go）
│   ├── cmd/server/               # 程序入口
│   ├── internal/                 # 业务逻辑
│   └── migrations/               # 数据库迁移文件（append-only）
│
├── agent/                        # Edge Agent（Go）
│   ├── cmd/agent/                # 程序入口
│   └── internal/                 # 业务逻辑
│
├── webui/                        # Web 管理后台（React）
│   └── src/
│       ├── pages/
│       ├── services/
│       └── store/
│
├── sdk/
│   ├── python/                   # Python SDK
│   └── java/                     # Java SDK（规划中）
│
├── deploy/                       # 部署配置与脚本
│   ├── docker-compose.dev.yml
│   ├── docker-compose.test.yml
│   └── scripts/init-minio.sh
│
└── docs/design/system-design.md # 完整系统设计文档（权威来源）
```

---

## 快速开始

### 前置依赖

- Docker & Docker Compose v2
- Go 1.24+
- Node.js 24 LTS (krypton) + pnpm 11（通过 Corepack 管理）
- Python 3.10+ + Poetry（仅 SDK 开发）
- [golang-migrate](https://github.com/golang-migrate/migrate) CLI（数据库迁移）

### 1. 启动基础服务

```bash
cd deploy
docker compose -f docker-compose.dev.yml up -d

# 确认全部 healthy
docker compose -f docker-compose.dev.yml ps
```

本地服务端口：

| 服务 | 端口 |
|------|------|
| PostgreSQL | 5432 |
| Redis | 6379 |
| MinIO API | 9000 |
| MinIO Console | 9001 |
| NATS | 4222 |
| NATS Monitor | 8222 |

### 2. 初始化数据库

> 说明：这一步是**可选**的。`controlplane` 启动时会自动执行数据库迁移（`db.Migrate`）。
> 迁移文件已**嵌入二进制**（`//go:embed`，见 D-023），无需 `MIGRATIONS_PATH`、也无需随二进制分发
> `migrations/` 目录。仅当你希望手动提前迁移，或单独排查迁移问题时，才需要用下面的 golang-migrate CLI。

```bash
export DATABASE_URL="postgres://fileagent:fileagent@localhost:5432/fileagent?sslmode=disable"

cd controlplane
migrate -database "$DATABASE_URL" -path ./migrations up
```

### 3. 初始化 MinIO

```bash
bash deploy/scripts/init-minio.sh
```

脚本幂等，重复执行不报错。执行内容：创建 `data-sensor` 和 `tmp-uploads` Bucket，配置 7 天 Lifecycle，创建供 Control Plane 使用的真实 IAM 用户及最小权限 policy，配置 Webhook 事件通知，并用该用户自检 STS AssumeRole 与预签名下载。脚本默认 access key 为 `cpAdminIAM000000000`；若覆盖 `CP_ADMIN_ACCESS_KEY`，长度须为 3–20 字符，`CP_ADMIN_SECRET_KEY` 须为 8–40 字符。

> 重跑脚本会把同名 IAM 用户的 secret 收敛为本次传入值。已部署环境必须同步更新 Control Plane 的 `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY` 并重启，否则旧进程会因凭据失效收到 Access Denied。

### 4. 构建二进制

```bash
# 构建全部（输出到 bin/）
make build

# 或单独构建
make build-controlplane
make build-agent
```

### 5. 启动服务

```bash
# Control Plane（推荐从示例文件复制后集中维护环境变量）
cp controlplane/.env.example controlplane/.env
set -a && source controlplane/.env && set +a
./bin/controlplane

# Edge Agent（需先准备 TOML 配置文件）
# 支持 --config 参数，也支持 AGENT_CONFIG 环境变量
cp agent/config.toml.example agent/config.toml
./bin/agent --config /path/to/agent.toml
```

首次启动时，如果数据库中没有用户，controlplane 会自动创建 `admin` 超级管理员，并将一次性凭据写入 `BOOTSTRAP_ADMIN_CREDENTIALS_FILE`。  
如发生管理员密码丢失，可设置 `BOOTSTRAP_ADMIN_PASSWORD` + `BOOTSTRAP_ADMIN_FORCE_RESET=true` 临时重置，恢复后请立即关闭该开关。

---

## 生产部署（单二进制分发）

`make bundle` 产出**自包含**的 Control Plane 二进制——Web UI（D-022）与数据库迁移（D-023）均已内嵌，
分发时**无需**随行 `migrations/` 目录或单独的前端静态站点，迁移在启动时自动应用。

```bash
make bundle          # bin/controlplane（内嵌 Web UI + 迁移）+ bin/agent
```

两条部署路径（完整步骤见运维文档）：

- **容器 all-in-one**：`docker compose -f deploy/docker-compose.prod.yml up -d --build`
  （PG/Redis/NATS/MinIO + CP + Caddy TLS 网关一把梭）。
- **主机 systemd**：`deploy/systemd/controlplane.service` + `fileagent-agent.service`；
  Windows agent 见 `deploy/windows/install-agent.ps1`（NSSM）。

📖 **部署指南**：[`docs/ops/deployment.md`](docs/ops/deployment.md) ·
**运维手册**（配置参考/升级/备份/排障）：[`docs/ops/operations.md`](docs/ops/operations.md)

---

## 开发

### 运行单元测试

```bash
# 全部模块
make test

# 单独模块
cd controlplane && go test ./...
cd agent && go test ./...
cd sdk/python && poetry run pytest
cd webui && pnpm test
```

### 查看测试覆盖率

```bash
# Go 模块（在 controlplane/ 或 agent/ 目录下）
go test ./... -coverprofile=coverage.out
go tool cover -func=coverage.out | tail -1     # 总覆盖率
go tool cover -html=coverage.out -o coverage.html  # HTML 报告

# Python SDK
cd sdk/python
poetry run pytest --cov=fileagent --cov-report=term-missing

# Web UI
cd webui && pnpm test --coverage
```

覆盖率要求：Go / Python ≥ 80%（核心业务逻辑 ≥ 90%）；Web UI store / service 层 ≥ 80%。

### 运行集成测试

```bash
# 启动集成测试专用环境
docker compose -f deploy/docker-compose.test.yml up -d

# 运行集成测试
cd controlplane && go test ./... -tags=integration
cd agent && go test ./... -tags=integration

# 清理
docker compose -f deploy/docker-compose.test.yml down -v
```

### Web UI 开发

```bash
# 首次使用：启用 Corepack（Node.js 内置，一次性操作）
corepack enable

cd webui
pnpm install     # Corepack 自动使用 pnpm@11.0.4
pnpm dev         # 启动开发服务器
pnpm build       # 生产构建
```

### 整理依赖

```bash
make tidy
```

---

## 契约文件（修改需谨慎）

以下文件是多模块共享契约，修改前必须在 `DECISIONS.md` 中记录：

| 文件 | 规则 |
|------|------|
| `proto/v1/agent.proto` | 只增字段，不改字段编号，不删除字段 |
| `controlplane/migrations/` | 只追加新迁移文件，不修改已有文件 |
| `deploy/docker-compose.test.yml` 端口 | 修改前通知所有模块 |

---

## 文档

- [系统设计文档](docs/design/system-design.md) — 完整架构设计，权威来源
- [CLAUDE.md](CLAUDE.md) — 技术栈约定与编码规范
- [TASK_LIST.md](TASK_LIST.md) — 任务清单与当前进度
- [DECISIONS.md](DECISIONS.md) — 技术决策记录
