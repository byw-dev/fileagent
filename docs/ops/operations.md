# 运维手册（Operations）

> 配置参考、健康检查、升级、备份与故障排查。部署步骤见 [`deployment.md`](deployment.md)。

---

## 1. Control Plane 配置参考（环境变量）

CP **仅从环境变量**读取配置，启动时校验并一次性报出所有缺失项。

### 必填

| 变量 | 说明 / 示例 |
|------|-------------|
| `DATABASE_URL` | `postgres://user:pass@host:5432/fileagent?sslmode=disable` |
| `REDIS_URL` | `redis://[:pass@]host:6379/0`（`rediss://` 走 TLS） |
| `JWT_SECRET` | JWT 签名密钥（HMAC-SHA256），强随机 ≥32 字节 |
| `MINIO_ENDPOINT` | 内网端点，CP 自身调用（STS AssumeRole + 建桶）用；`host:port`（无 scheme，由 `MINIO_USE_SSL` 决定） |
| `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY` | CP 访问 MinIO 的凭据（用于签发 STS） |
| `NATS_URL` | `nats://host:4222`（JetStream 需开启） |

### 可选（含默认值）

| 变量 | 默认 | 说明 |
|------|------|------|
| `HTTP_PORT` | `8080` | REST + Web UI 端口 |
| `GRPC_PORT` | `9090` | Agent gRPC 端口 |
| `LOG_LEVEL` | `info` | `debug`/`info`/`warn`/`error` |
| `MINIO_USE_SSL` | `false` | 内网 MinIO 客户端 TLS |
| `MINIO_PUBLIC_ENDPOINT` | =`MINIO_ENDPOINT` | 面向客户端的端点：写入 STS payload 交给 agent、并作为 presign URL 的 host；须对浏览器/agent 可达（D-024） |
| `MINIO_PUBLIC_USE_SSL` | =`MINIO_USE_SSL` | public 端点 TLS |
| `MINIO_ROLE_ARN` | `arn:aws:iam:::role/agent-role` | STS AssumeRole |
| `JWT_ACCESS_TOKEN_TTL` | `2h` | 用户 access token |
| `JWT_REFRESH_TOKEN_TTL` | `720h` | 用户 refresh token |
| `AGENT_TOKEN_TTL` | `720h` | Agent 长效 token |
| `API_RATE_LIMIT_PER_MINUTE` | `600` | 每用户限流；≤0 关闭 |
| `INTERNAL_WEBHOOK_SECRET` | `""` | MinIO→CP webhook 共享密钥；**空则拒绝所有 webhook**，生产必设 |
| `BOOTSTRAP_ADMIN_USERNAME` | `admin` | 首个管理员用户名 |
| `BOOTSTRAP_ADMIN_PASSWORD` | `""` | 空则随机生成并写入凭据文件 |
| `BOOTSTRAP_ADMIN_FORCE_RESET` | `false` | 每次启动重置管理员口令（救援用；需同时设 PASSWORD） |
| `BOOTSTRAP_ADMIN_CREDENTIALS_FILE` | `bootstrap_admin_credentials.txt` | 生成凭据的落盘路径（0600） |

> **`MIGRATIONS_PATH` 已移除**：迁移内嵌二进制，启动自动应用，不再需要外部迁移目录（D-023）。

---

## 2. Agent 配置参考（TOML + 环境变量覆盖）

配置源优先级：`--config` flag → `AGENT_CONFIG` 环境变量 → `./config.toml`。所有字段可被
`AGENT_*` 环境变量覆盖。模板：`agent/config.toml.example`（Linux）、
`agent/config.windows.toml.example`（Windows）。

| 段.字段 | 环境变量 | 默认 | 说明 |
|---------|----------|------|------|
| `server.endpoint` | `AGENT_SERVER_ENDPOINT` | （必填） | CP gRPC 地址 |
| `server.tls_ca_cert` | `AGENT_SERVER_TLS_CA_CERT` | 系统根 CA | 自定义 CA 证书路径 |
| `agent.fingerprint_file` | `AGENT_FINGERPRINT_FILE` | — | 机器指纹缓存路径 |
| `agent.token_file` | `AGENT_TOKEN_FILE` | — | 加密 token 落盘路径（0600） |
| `agent.data_dir` | `AGENT_DATA_DIR` | — | SQLite 队列 / 本地状态目录 |
| `upload.concurrency` | `AGENT_UPLOAD_CONCURRENCY` | `3` | 并发上传 worker |
| `upload.part_size_mb` | `AGENT_UPLOAD_PART_SIZE_MB` | `64` | 分片大小 |
| `upload.queue_max_size` | `AGENT_UPLOAD_QUEUE_MAX_SIZE` | `10000` | SQLite 队列上限 |
| `upload.retry_max` | `AGENT_UPLOAD_RETRY_MAX` | `10` | 每任务最大重试 |
| `metrics.enabled` / `metrics.port` | `AGENT_METRICS_ENABLED` / `_PORT` | `true` / `9100` | Prometheus 端点 |
| `log.level` | `AGENT_LOG_LEVEL` | `info` | 日志级别 |

> `log.output` / `log.max_size_mb` / `log.max_backups` 目前**未接线**：agent 只写 stdout，
> 轮转依赖 journald（Linux）/ NSSM（Windows）捕获。

---

## 3. 健康检查

`GET /healthz` 只做 **liveness**（永远返回 `{"status":"ok"} 200`，**不探依赖**）——适合
K8s livenessProbe / LB 心跳。**Readiness**（依赖是否 OK）通过实际业务探测判断，例如
`POST /api/auth/login` 能否返回 200，或独立监控 PG/Redis/NATS/MinIO。

---

## 4. 升级

- **迁移随启动自动执行且幂等**：新版二进制 + schema 变更一起发布是安全的（先滚 CP，迁移在启动时应用；
  重复启动对已最新的库是 no-op）。迁移文件**只追加不改**（契约约束）。
- **Control Plane**：无状态，可滚动重启（第一版单实例；停机窗口内替换二进制并 `systemctl restart`）。
- **Agent**：走 gRPC 下发新二进制 URL，agent 自下载→校验 SHA256→自替换→重启（system-design §10.7）。
  离线期间 agent 继续写本地 SQLite 队列，重连后补传。

---

## 5. 备份

| 对象 | 内容 | 方式 |
|------|------|------|
| PostgreSQL | 全部业务数据（用户/agent/规则/文件索引/事件投递） | `pg_dump` / 卷快照 |
| MinIO | 对象数据本体 | MinIO 侧复制 / 卷快照 |
| Bootstrap 凭据文件 | 首个管理员口令 | 首次记录后妥善保管；丢失可用 `BOOTSTRAP_ADMIN_FORCE_RESET` 救援 |

> Redis / NATS 为易失状态（TTL / 事件流），无需备份。

---

## 6. 故障排查

| 症状 | 可能原因 / 处理 |
|------|-----------------|
| CP 启动即退出 | 某依赖不可达（fail-fast，无重试）；看日志定位 PG/Redis/NATS/MinIO；编排层会重拉 |
| webhook 全被拒 / 无 file 事件 | `INTERNAL_WEBHOOK_SECRET` 未设或与 `init-minio.sh` 的 `WEBHOOK_AUTH_TOKEN` 不一致 |
| Web UI 打开 404 | 用了纯 API 构建（`make build`）而非 `make bundle`；或反代未指向 CP:8080 |
| 深链硬刷新 404 | 反代未回退 SPA；CP 内嵌 SPA 已处理，确认请求确实到达 CP |
| Agent 一直不采集 | 处于 PENDING，未在 Web UI 审批 |
| 下载文件名变成哈希/UUID | 浏览器对跨域 `a.download` 忽略；presign 需带 `response-content-disposition`（已知项） |
| Agent 显示离线但进程在跑 | 心跳/Redis TTL；CP 有 TTL 驱动的离线兜底扫描（CC-6） |
| prod compose 里 MinIO 永不 healthy | healthcheck 用镜像内 `mc ready local`；**新版 `minio/minio` 已不再随镜像带 `mc`**（移到 `minio/mc`）。compose 因此 pin 了内置 mc 的版本；若升级镜像，改用 `minio/mc` sidecar 或 mc-free 健康检查（如探 `/minio/health/live`） |
| 浏览器/agent 无法下载或上传 | presign/STS 里的 MinIO host 不可达：`MINIO_PUBLIC_ENDPOINT` 须为对客户端可达的地址（非内网 `minio:9000`）。CP 自身走 `MINIO_ENDPOINT`（内网），两者已拆分（D-024）；生产应经 TLS 网关暴露 MinIO 并把 `MINIO_PUBLIC_ENDPOINT` 指向网关地址 |

---

## 相关

- 部署步骤：[`deployment.md`](deployment.md)
- 架构背景：`docs/design/system-design.md §5`（CP）/`§4`（Agent）/`§10`（部署）
- 隐性契约（枚举/信封/错误码）：`docs/design/contracts.md`
