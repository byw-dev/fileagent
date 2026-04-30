# AGENTS.md

## 仓库现状
- 当前仓库仍是**文档先行**：实际已存在的是 `CLAUDE.md`、`TASK_LIST.md`、`docs/design/system-design.md`；`controlplane/`、`agent/`、`webui/`、`sdk/` 仍是设计目标目录。
- 以 `docs/design/system-design.md` 作为**架构事实来源**，以 `TASK_LIST.md` 作为**任务顺序与前置依赖来源**，以 `CLAUDE.md` 作为**技术栈与编码规范来源**。
- 当前阶段是 **Phase 0 — 契约定义**；在 `TASK_LIST.md` 的 Phase 0 任务未完成前，不要提前展开 Phase 1+ 的脚手架或业务实现。

## 开始任务前先做什么
1. 先读 `CLAUDE.md`。
2. 再看 `TASK_LIST.md`，确认当前 Phase、前置依赖和验收标准。
3. 然后阅读 `docs/design/system-design.md` 对应章节。
4. 若要改共享契约，先确认 `DECISIONS.md` 是否存在；`CLAUDE.md` 引用了它，但当前仓库根目录并未落地。

## 架构总览
- 系统明确分为**控制平面**和**数据平面**（`system-design.md` §§1.4, 2.2）。
- 控制平面：`Agent <-> Control Plane` 走 **gRPC/TLS**；`Web UI / SDK -> Control Plane` 走 **HTTPS REST**。
- 数据平面：`Agent -> MinIO` 走 **S3 API/TLS**，使用短期 **STS 凭据**，文件内容**不经过** Control Plane。
- 固定基础设施：**PostgreSQL**（持久化）、**Redis**（TTL/锁/在线状态）、**NATS JetStream**（事件总线）、**MinIO**（对象存储）。

## 设计目标中的模块边界
- `agent/`：Go 单二进制，包含 watcher / scheduler / executor / uploader / SQLite 队列 / credential manager（`system-design.md` §§4.1, 4.9）。
- `controlplane/`：Go 服务，包含 gRPC 服务端、REST API、agent manager、indexer、STS manager、后台 worker（`system-design.md` §§5.1, 5.12）。
- `webui/`：Vite + React + Ant Design Pro，按 `pages/`、`services/`、`store/` 组织（`system-design.md` §7.5）。
- `sdk/python/`：Python SDK，核心是 `auth.py`、`http.py`、各资源模块（`system-design.md` §8.2）。

## 必须保护的契约
- `proto/v1/agent.proto` 是 `agent` 与 `controlplane` 的共享契约：**只能增字段，不能改字段编号**（`TASK_LIST.md` T0-1）。
- `controlplane/migrations/` 迁移文件是 **append-only**，不要改历史迁移（`CLAUDE.md`）。
- `deploy/docker-compose.test.yml` 的端口是共享契约：PostgreSQL `5432`、Redis `6379`、MinIO `9000/9001`、NATS `4222/8222`（`TASK_LIST.md` T0-3）。
- 资源类 REST 接口主要在 `/api/v1/...`；认证接口设计为 `/api/auth/*`，不要混写成单一路径规则（`system-design.md` §§5.3, 5.11）。
- 文件查询分页是 **cursor-based**，不是 offset-based（`system-design.md` §8.5）。

## 这个项目的关键实现模式
- Agent 生命周期是显式状态机：`INIT -> PENDING -> APPROVED -> RUNNING -> OFFLINE/REVOKED`；离线时继续写本地 SQLite 队列，重连后补传（`system-design.md` §4.2）。
- 上传策略按大小分流：`<=64MB` 单次上传，`>64MB` 分片上传；分片大小 `64MB`，断点续传状态保存在 SQLite（`system-design.md` §4.5）。
- 心跳周期是 `30s`，Control Plane 通过 Redis TTL `90s` 判定离线（`system-design.md` §5.2）。
- STS 默认有效期 `1h`，到期前刷新；JWT 身份认证与 STS 上传凭据是两套独立机制（`system-design.md` §§4.7, 5.7）。
- Control Plane 在处理 `UploadResult` 后做文件归类与索引，并发布 `events.file.uploaded` 等 NATS 事件（`system-design.md` §§5.8, 5.9）。
- 第一版已知边界：**单组织**、**Bearer JWT**、**不启用 mTLS**、**Control Plane 单实例**（`CLAUDE.md`；`system-design.md` 附录 D）。

## 文档里已经定义的工作流
- 本地开发环境：`deploy/docker-compose.dev.yml` -> `controlplane/migrations/` -> `deploy/scripts/init-minio.sh`。
- 集成测试环境：`deploy/docker-compose.test.yml`，然后分别运行 `controlplane` 和 `agent` 的 `go test ./... -tags=integration`。
- 注意：以上目录和命令来自文档设计；当前仓库尚未落地这些目录时，不要假设它们已经可执行。

## 实施时的仓库约定
- 配置只能来自环境变量或配置文件，禁止 hardcode 端点、密钥、端口、Bucket 名称（`CLAUDE.md`）。
- Go 侧固定使用 `zap`；`Gin` 只用于 `controlplane`；数据库访问使用 `sqlc`，并显式传递 `context.Context`（`CLAUDE.md`）。
- 当前 Phase 0 的首要交付是：`proto/v1/agent.proto`、数据库迁移、两套 `docker-compose`、`init-minio.sh`、以及 `go.mod` 初始化；优先做这些，不要偏离 `TASK_LIST.md`。

