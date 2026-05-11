# CLAUDE.md — FileAgent 项目全局说明

> 所有 AI Agent 在开始任何任务之前，必须先完整阅读本文件。

---

## 项目概述

**文件采集同步存储分发系统（FileAgent）**

一套完全私有化部署的文件采集、同步、存储与分发平台。边缘设备上的 Agent
采集文件后直传 MinIO，Control Plane 负责管理调度，Web UI 提供管理界面，
SDK 供内部系统集成。

详细设计见：`docs/design/system-design.md`

---

## 架构总览

- 系统明确分为**控制平面**和**数据平面**（`system-design.md` §§1.4, 2.2）。
- 控制平面：`Agent <-> Control Plane` 走 **gRPC/TLS**；`Web UI / SDK -> Control Plane` 走 **HTTPS REST**。
- 数据平面：`Agent -> MinIO` 走 **S3 API/TLS**，使用短期 **STS 凭据**，文件内容**不经过** Control Plane。
- 固定基础设施：**PostgreSQL**（持久化）、**Redis**（TTL/锁/在线状态）、**NATS JetStream**（事件总线）、**MinIO**（对象存储）。

---

## 仓库结构

```
fileagent/                        # Monorepo 根目录
├── CLAUDE.md                     # 本文件（所有 Agent 必读）
├── TASK_LIST.md                  # 任务总索引（各阶段状态概览 + 导航至 docs/tasks/）
├── DECISIONS.md                  # 技术决策记录（修改前必读）
├── Makefile                      # 统一构建入口（make build → bin/）
│
├── bin/                          # ⚠️ 编译产物（.gitignore 忽略，不提交）
│
├── docs/
│   ├── design/
│   │   ├── system-design.md      # 完整系统设计文档（权威来源）
│   │   └── fileagent_design_complete.docx
│   ├── reports/                  # 审计报告（已归档）
│   └── tasks/                    # 任务管理（详见 TASK_LIST.md 导航）
│       ├── SCHEMA.md             # 字段定义与状态枚举
│       ├── active.md             # ⭐ 当前执行入口（轻量）
│       ├── phases/
│       │   └── phase-3.md        # 当前 Phase 主线（完整依赖与验收）
│       ├── backlog.md            # 待规划任务
│       ├── changelog.md          # 历史记录索引（兼容入口）
│       ├── archive/              # 按 Phase 归档的历史完成记录
│       └── bugs/
│           ├── open.md           # 未解决 Bug（Agent 可直接消费）
│           └── closed.md         # 已关闭 Bug 归档
│
├── proto/                        # ⚠️ 契约文件，修改须知会所有模块
│   └── v1/
│       └── agent.proto
│
├── controlplane/                 # Control Plane 服务（Go）
├── agent/                        # Edge Agent（Go）
├── webui/                        # Web 管理后台（React）
├── sdk/
│   ├── python/                   # Python SDK
│   └── java/                     # Java SDK
│
├── deploy/
│   ├── docker-compose.dev.yml    # 本地开发环境
│   ├── docker-compose.test.yml   # 集成测试环境
│   └── scripts/
│       └── init-minio.sh
│
└── .github/
    └── workflows/                # CI 配置
```

---

## 模块边界（已落地）

- `agent/`：Go 单二进制，包含 watcher / scheduler / executor / uploader / SQLite 队列 / credential manager（`system-design.md` §§4.1, 4.9）。
- `controlplane/`：Go 服务，包含 gRPC 服务端、REST API、agent manager、indexer、STS manager、后台 worker（`system-design.md` §§5.1, 5.12）。
- `webui/`：Vite + React + Ant Design Pro，按 `pages/`、`services/`、`store/` 组织（`system-design.md` §7.5）。
- `sdk/python/`：Python SDK，核心是 `auth.py`、`http.py`、各资源模块（`system-design.md` §8.2）。

---

## 技术栈约定

### 通用
- 所有配置项必须通过环境变量或配置文件注入，**禁止 hardcode**
- 日志统一输出结构化 JSON 格式
- 时间统一使用 UTC，数据库字段使用 TIMESTAMPTZ

### Go（controlplane / agent）
- Go 版本：**1.22+**
- 错误处理：显式返回 error，禁止 panic（除非程序无法继续运行）
- 日志库：**zap**（uber-go/zap）
- HTTP 框架：**Gin**（仅 controlplane）
- 数据库查询：**sqlc** 生成代码，禁止手写裸 SQL 字符串拼接
- 测试框架：**testify**（assert + require + mock）
- 单元测试覆盖率要求：**≥ 80%**（核心业务逻辑 ≥ 90%）
- 每个导出函数必须有 godoc 注释

### React（webui）
- Node 版本：**24**（`>=24.0.0 <25.0.0`）
- 包管理器：**pnpm 11**（`pnpm@11.0.4`，`>=11.0.0 <12.0.0`）
- UI 组件：**Ant Design 5 + ProComponents**
- 状态管理：**Zustand**
- HTTP：**Axios + SWR**
- 构建：**Vite**
- 测试：**Vitest + React Testing Library**

### Python SDK（sdk/python）
- Python 版本：**3.10+**
- 包管理：**Poetry**
- HTTP 客户端：**httpx**
- 测试：**pytest + respx**（mock HTTP）

### Java SDK（sdk/java）
- Java 版本：**11+**
- 构建：**Gradle**
- HTTP 客户端：**OkHttp3**
- 序列化：**Jackson**
- 测试：**JUnit 5 + MockWebServer**

---

## 契约文件（修改前必须在 DECISIONS.md 记录）

以下文件是多模块共享的接口契约，**任何修改都可能影响其他模块**：

| 文件 | 影响范围 | 修改规则 |
|------|----------|----------|
| `proto/v1/agent.proto` | controlplane + agent | 只增字段，不改字段编号；不删除字段 |
| `deploy/docker-compose.test.yml` 中的端口定义 | 所有集成测试 | 端口固定：PostgreSQL `5432`、Redis `6379`、MinIO `9000/9001`、NATS `4222/8222`；修改前通知所有模块 |
| `controlplane/migrations/` 迁移文件 | controlplane + 所有依赖 DB 的测试 | 只追加，不修改已有迁移文件 |
| REST 接口路径 | controlplane + 所有客户端 | 资源类接口统一在 `/api/v1/...`；认证接口统一在 `/api/auth/*`，不得混写 |
| 文件查询分页 | controlplane + SDK | cursor-based pagination，不得改为 offset-based |

---

## 关键实现模式

- Agent 生命周期是显式状态机：`INIT -> PENDING -> APPROVED -> RUNNING -> OFFLINE/REVOKED`；离线时继续写本地 SQLite 队列，重连后补传（`system-design.md` §4.2）。
- 上传策略按大小分流：`<=64MB` 单次上传，`>64MB` 分片上传；分片大小 `64MB`，断点续传状态保存在 SQLite（`system-design.md` §4.5）。
- 心跳周期是 `30s`，Control Plane 通过 Redis TTL `90s` 判定离线（`system-design.md` §5.2）。
- STS 默认有效期 `1h`，到期前刷新；JWT 身份认证与 STS 上传凭据是两套独立机制（`system-design.md` §§4.7, 5.7）。
- Control Plane 在处理 `UploadResult` 后做文件归类与索引，并发布 `events.file.uploaded` 等 NATS 事件（`system-design.md` §§5.8, 5.9）。
- 第一版已知边界：**单组织**、**Bearer JWT**、**不启用 mTLS**、**Control Plane 单实例**（`system-design.md` 附录 D）。

---

## 编码规范

### 必须遵守

- **每个函数/方法必须有注释**（Go: godoc 格式；Python: docstring；Java: Javadoc）
- **每个公共接口必须有对应单元测试**，覆盖正常路径 + 至少一个错误路径
- **禁止在业务代码中直接使用** `fmt.Println` / `print()` 等，统一使用日志库
- **数据库操作必须支持 context 取消**，传入 `ctx context.Context`
- **所有外部依赖（DB、Redis、MinIO、NATS）必须通过接口抽象**，便于单元测试 Mock

### 测试规范

```
每完成一个函数/模块，立即编写对应测试，不得积压。

测试文件命名：
  Go:     xxx_test.go（与源文件同目录）
  Python: test_xxx.py（tests/ 目录）
  Java:   XxxTest.java（src/test/java/）

测试必须覆盖：
  1. Happy path（正常输入，期望输出）
  2. Error path（无效输入、依赖失败等错误场景）
  3. Edge case（空值、边界值、并发场景）

单元测试中禁止：
  - 访问真实数据库
  - 访问真实 Redis
  - 访问真实 MinIO / NATS
  - 发出真实 HTTP 请求
  以上均须 Mock
```

### 测试覆盖率要求

**覆盖率标准：**

- Go（controlplane / agent）：整体 **≥ 80%**，核心业务逻辑 **≥ 90%**
- Python SDK：整体 **≥ 80%**
- Web UI（Vitest）：核心 store / service 层 **≥ 80%**

**运行覆盖率报告：**

```bash
# Go（在 controlplane/ 或 agent/ 目录下）
go test ./... -coverprofile=coverage.out
go tool cover -html=coverage.out -o coverage.html   # 生成 HTML 报告
go tool cover -func=coverage.out | tail -1           # 查看总覆盖率

# Python SDK
cd sdk/python
poetry run pytest --cov=fileagent --cov-report=term-missing --cov-report=html

# Web UI
cd webui
pnpm test --coverage
```

**强制要求：**

- 每完成一个 Phase 2+ 的任务，必须运行覆盖率检查，确保未低于阈值。
- PR 提交前需附上主要模块的覆盖率数据（可在 PR 描述中粘贴 `go tool cover -func` 输出）。
- 禁止为提高覆盖率数字而写无意义的空测试。

### Git 规范

```
提交信息格式：<type>(<scope>): <subject>

type:
  feat     新功能
  fix      Bug 修复
  test     添加或修改测试
  refactor 重构（不影响功能）
  docs     文档变更
  chore    构建/工具链变更

示例：
  feat(agent): implement multipart upload with resume support
  test(controlplane): add unit tests for JWT token revocation
  fix(agent): fix credential refresh timing when TTL < 10min
```

---

## 本地开发环境启动

```bash
# 启动所有基础服务（PostgreSQL、Redis、MinIO、NATS）
cd deploy
docker compose -f docker-compose.dev.yml up -d

# 检查服务状态
docker compose -f docker-compose.dev.yml ps

# 构建所有二进制（输出到 bin/）
make build

# 初始化数据库（首次）
cd controlplane
migrate -database "$DATABASE_URL" -path ./migrations up

# 初始化 MinIO（首次）
bash ../deploy/scripts/init-minio.sh
```

**本地服务端口：**

| 服务 | 端口 |
|------|------|
| PostgreSQL | 5432 |
| Redis | 6379 |
| MinIO API | 9000 |
| MinIO Console | 9001 |
| NATS | 4222 |
| NATS Monitor | 8222 |

---

## 集成测试

```bash
# 启动集成测试专用环境
docker compose -f deploy/docker-compose.test.yml up -d

# 运行 Control Plane 集成测试
cd controlplane && go test ./... -tags=integration

# 运行 Agent 集成测试
cd agent && go test ./... -tags=integration

# 关闭测试环境
docker compose -f deploy/docker-compose.test.yml down -v
```

---

## 重要决策快速参考

> 完整记录见 `DECISIONS.md`

| 决策点 | 结论 |
|--------|------|
| Agent ↔ Control Plane 通信 | gRPC 双向流，不用 WebSocket |
| Agent 文件上传 | 直传 MinIO，不经过 Control Plane |
| Agent 身份认证 | Bearer JWT Token，第一版不启用 mTLS |
| 数据库主键 | 统一 UUID v4 |
| 分页方式 | Cursor-based pagination，不用 offset |
| Agent 本地持久化 | SQLite，不用其他嵌入式 DB |
| 多租户 | 第一版单组织，org_id 字段已预留 |
| Control Plane HA | 第一版单实例，无状态设计为 HA 预留 |
| 二进制构建与输出 | `make build` → `bin/`，禁止提交编译产物（见 D-003） |

---

## 当前阶段

当前为 **Phase 3 — 集成联调**（T3-1 ✅，T3-1-FIX ✅，T3-1-BUGFIX ✅，T3-2-FIX ✅，T3-2-BUG 进行中）。
详细任务与状态以 `docs/tasks/active.md` + `docs/tasks/phases/phase-3.md` 为准；`TASK_LIST.md` 提供总索引。

**开始任务前必须确认（按顺序）：**
1. 读本文件（`CLAUDE.md`）全文。
2. 读 `docs/tasks/active.md` 确认当前 Phase 和前置依赖。
3. 读 `docs/tasks/phases/phase-3.md` 获取当前 Phase 主线与验收标准。
4. 若涉及 Bug 修复，读 `docs/tasks/bugs/open.md`。
5. 阅读 `docs/design/system-design.md` 中与本次任务相关的章节。
6. 若要修改共享契约文件，先在 `DECISIONS.md` 中记录决策。
