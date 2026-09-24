# 部署指南（Deployment）

> FileAgent 生产部署 runbook。两条路径任选其一：
> **A) 容器 all-in-one**（单机，最省事）；**B) 主机 systemd**（更贴近生产分层）。
> 二者都基于 D-022/D-023 的**单二进制**：一个 CP 二进制内嵌 Web UI 与数据库迁移，
> 无需随行 `migrations/` 目录，也无需单独部署前端静态站点。

---

## 0. 前置依赖

Control Plane 启动时会**快速失败**（fail-fast）——任一依赖不可达即退出（无重试循环），
交由编排层（systemd / Docker restart policy）拉起。部署前确保以下服务就绪：

| 依赖 | 版本 | 用途 |
|------|------|------|
| PostgreSQL | 15+ | 持久化（迁移在 CP 启动时自动应用） |
| Redis | 7+ | 在线状态 TTL / 锁 / 限流 |
| NATS | 2.x（**JetStream 开启**，`-js`） | 事件总线 |
| MinIO | 近期版本 | 对象存储（浏览器/agent 直传直下，需可达） |

> **网络可达性**：文件上传/下载走**浏览器/agent ↔ MinIO 直连**（CP 只签发 presigned URL），
> 因此 MinIO 的对外地址必须对客户端可达，而不仅仅对 CP 可达。

---

## 路径 A — 容器 all-in-one（`docker-compose.prod.yml`）

一条命令拉起 PG / Redis / NATS / MinIO / Control Plane（含 Web UI）/ Caddy 网关。

> ⚠️ **这是 dev / PoC 形态**，用于快速跑通与本地演示，**不是加固的生产拓扑**：默认口令、且 MinIO 为便利
> **直接发布到宿主机**。CP↔MinIO（内网 `MINIO_ENDPOINT`）与客户端（公网 `MINIO_PUBLIC_ENDPOINT`）
> 两个 endpoint **已拆分**（D-024），CP 流量留在内网、不再 hairpin。**生产**还应经 TLS 网关暴露 MinIO
> 并把 `MINIO_PUBLIC_ENDPOINT` 指向网关地址（网关无关，Caddy/nginx 皆可）而非直接发布到宿主机。

### A.1 配置

在 `deploy/` 下建一个 `.env`（与 compose 同级）覆盖默认口令/密钥（默认值仅供 PoC，**生产必须改**）：

```dotenv
POSTGRES_PASSWORD=<强随机>
MINIO_ROOT_USER=<改我>
MINIO_ROOT_PASSWORD=<强随机>
# Control Plane 的真实 IAM 用户凭据；须与 A.3 初始化命令一致。
# 脚本按 3–20 / 8–40 字符校验：MinIO 对 IAM 用户只强制下界（access key ≥3、secret ≥8），
# 上界 20 / 40 是 service account 的限制，脚本主动收敛到该窗口以便两种账号形态互换。
CP_ADMIN_ACCESS_KEY=<3–20 字符>
CP_ADMIN_SECRET_KEY=<8–40 字符>
JWT_SECRET=<强随机，≥32 字节>
INTERNAL_WEBHOOK_SECRET=<强随机>
# 必填：MinIO 对外（客户端）可达地址（host:port）。CP 用它构造交给浏览器/agent 的
# presigned/STS URL，故须对浏览器、agent 可达——单机填宿主 LAN IP（MinIO 已发布在 :9000），
# 不能用内网名 minio:9000。不设则 `docker compose up` 直接报错。CP 自身走内网
# MINIO_ENDPOINT=minio:9000（已在 compose 固定），两端点已拆分（D-024），不再 hairpin。
MINIO_PUBLIC_ENDPOINT=192.168.1.10:9000
# 可选：固定首个管理员口令（留空则 CP 随机生成并写入凭据文件）
BOOTSTRAP_ADMIN_PASSWORD=<留空或指定>
```

### A.2 启动

```bash
docker compose -f deploy/docker-compose.prod.yml up -d --build
```

CP 依赖各服务 healthcheck，会等其就绪后再启动；启动时自动应用内嵌迁移
（日志出现 `db migrate: migrations applied successfully`）。

### A.3 初始化 MinIO

建桶（`data-sensor`、`tmp-uploads`）+ 生命周期 + webhook + Control Plane 最小权限 IAM 用户，并用该
用户自检 CP 运行时真正会用到的三件事：STS AssumeRole、建桶（`POST /api/v1/buckets`）、预签名下载。
脚本可幂等重跑：

```bash
MINIO_ENDPOINT=http://localhost:9000 \
MINIO_ROOT_USER=<同上> MINIO_ROOT_PASSWORD=<同上> \
CP_ADMIN_ACCESS_KEY=<同 A.1> CP_ADMIN_SECRET_KEY=<同 A.1> \
WEBHOOK_AUTH_TOKEN=<同 INTERNAL_WEBHOOK_SECRET> \
bash deploy/scripts/init-minio.sh
```

> ⚠️ 重跑时脚本**不会**擅自改写已存在 IAM 用户的 secret：它先用传入的凭据试一次 AssumeRole，
> 通过就原样保留（policy 仍会覆盖更新）。若传入的 secret 与线上不符，脚本**报错退出、不做任何改动**，
> 以免把正在运行的 CP 凭据换掉——那种故障要等 agent 的 STS 会话过期（≤1h）才在 agent 侧爆出来。
> 确实要轮换时显式加 `CP_ADMIN_ROTATE=1`，脚本会在结论里明确告知已轮换；随后同步更新 CP 的
> `MINIO_ACCESS_KEY` / `MINIO_SECRET_KEY` 并重启。
>
> 不得用 root 的 service account 代替：MinIO 不允许 service account 调 `AssumeRole`，
> 脚本的内置自检会直接失败。

> ⚠️ 注意变量同名但格式不同：`init-minio.sh` 的 `MINIO_ENDPOINT` 是 **`mc` 用的完整 URL**（含
> `http://`/`https://` scheme），而 Control Plane 的同名配置 `MINIO_ENDPOINT` 是 **`host:port`**（无 scheme，
> 由 `MINIO_USE_SSL` 决定协议）。别把两者的取值互相照搬。

> 执行环境需同时具备 **`mc`** 与 **`curl`**（≥7.75，自检要用 `--aws-sigv4`）。宿主机没装 `mc` 时，
> 用 compose 已 pin 的 **`minio/minio`** 镜像执行——它同时自带 mc 和 curl。
> **不要用 `minio/mc` 镜像：它没有 curl**，脚本会在建任何资源之前就报错退出。
>
> ```bash
> docker run --rm --network <compose 网络> \
>   -v "$PWD/deploy/scripts:/s:ro" \
>   -e MINIO_ENDPOINT=http://minio:9000 \
>   -e MINIO_ROOT_USER=<同上> -e MINIO_ROOT_PASSWORD=<同上> \
>   -e CP_ADMIN_ACCESS_KEY=<同 A.1> -e CP_ADMIN_SECRET_KEY=<同 A.1> \
>   -e WEBHOOK_AUTH_TOKEN=<同 INTERNAL_WEBHOOK_SECRET> \
>   --entrypoint bash quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z /s/init-minio.sh
> ```
>
> 说明：镜像 tag 与 `docker-compose.prod.yml` 里 pin 的一致（换 tag 前先确认镜像里仍有 `mc` 与
> `curl`）；`--entrypoint bash` 是必须的，镜像默认 entrypoint 是 MinIO 自己的启动脚本。容器内用
> compose 网络里的服务名 `minio:9000`，不是宿主的 `localhost:9000`。脚本刻意不依赖 grep/sed/awk，
> 正是为了能在这个只带 mc + curl 的镜像里跑完。

### A.4 TLS

`deploy/caddy/Caddyfile` 默认 `tls internal`（Caddy 自签 CA，便于 PoC）。生产改为：
- `tls you@example.com` 走 Let's Encrypt（公网域名）；或
- `tls /path/cert.pem /path/key.pem` 提供证书；或
- 对接内网 ACME（step-ca，见 system-design §10.5）。

Agent 侧信任：导出 Caddy 内部 CA（`docker compose exec caddy caddy trust` 或从 `caddy_data` 卷取），
设 agent 的 `server.tls_ca_cert`。

---

## 路径 B — 主机 systemd

### B.1 构建产物

```bash
make bundle          # 产出 bin/controlplane（内嵌 Web UI + 迁移）与 bin/agent
```

### B.2 安装 Control Plane

```bash
sudo useradd --system --no-create-home --shell /usr/sbin/nologin fileagent
sudo install -D -m 0755 bin/controlplane /opt/fileagent/controlplane
sudo install -D -m 0640 -o fileagent controlplane/.env.example /etc/fileagent/controlplane.env
sudoedit /etc/fileagent/controlplane.env      # 填入真实 DSN / 密钥；无需 MIGRATIONS_PATH
sudo cp deploy/systemd/controlplane.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now controlplane
```

> `controlplane.env` 里连接串指向真实 PG/Redis/NATS/MinIO 主机；`BOOTSTRAP_ADMIN_CREDENTIALS_FILE`
> 用相对路径（默认 `bootstrap_admin_credentials.txt`）即写入 `StateDirectory`（`/var/lib/fileagent`）。

TLS：CP 本身是明文 HTTP(:8080)/gRPC(:9090)，前置 Caddy/nginx 终结 TLS（见 §10.4 / 路径 A Caddyfile）。

### B.3 安装 Edge Agent（每台边缘设备）

```bash
sudo install -D -m 0755 bin/agent /opt/fileagent/agent
sudo install -D -m 0640 agent/config.toml.example /etc/fileagent/agent.toml
sudoedit /etc/fileagent/agent.toml            # 填 server.endpoint、watch 路径等
sudo cp deploy/systemd/fileagent-agent.service /etc/systemd/system/
sudo systemctl daemon-reload
sudo systemctl enable --now fileagent-agent
```

Windows 设备见 [`../../deploy/windows/install-agent.ps1`](../../deploy/windows/install-agent.ps1)
（NSSM 封装）与 `agent/config.windows.toml.example`；`agent.exe` 取自 CI
`build-agent.yml` 产物（`agent-windows-amd64`）或 `make build-agent-windows`（需 mingw）。

---

## Agent enrollment（首次注册审批）

Agent 首次启动是显式状态机（`INIT → PENDING → APPROVED → RUNNING`）：

1. Agent 生成机器指纹（缓存于 `fingerprint_file`），gRPC `Register()` 上报，进入 **PENDING**。
2. 管理员在 Web UI 的 Agent 列表**审批**该 agent。
3. Agent 轮询到审批后取得 token（加密存 `token_file`，0600），进入 **RUNNING** 开始采集。

未审批前 agent 会持续等待（有日志），不采集。

---

## 部署后验证清单

| 检查 | 命令 / 动作 | 期望 |
|------|-------------|------|
| 迁移已应用 | CP 启动日志 | `db migrate: migrations applied successfully`（或 already up to date） |
| Liveness | `curl -k https://<host>/healthz`（经 Caddy）或 `curl http://<cp>:8080/healthz` | `{"status":"ok"}` 200 |
| 首个管理员 | 读取 `BOOTSTRAP_ADMIN_CREDENTIALS_FILE`（容器：`/data/...`；主机：`/var/lib/fileagent/...`） | 用户名 + 口令 |
| 登录（Readiness） | Web UI 或 `POST /api/auth/login` | 200 + JWT |
| Web UI | 浏览器打开 `https://<host>/` | 仪表盘渲染；深链硬刷新经 SPA fallback 正常 |
| Agent gRPC | agent 启动后 Web UI 出现 PENDING agent | 可审批 |

---

## 相关文件

- 运维手册（配置参考 / 升级 / 备份 / 排障）：[`operations.md`](operations.md)
- 部署产物：`controlplane/Dockerfile`、`deploy/docker-compose.prod.yml`、`deploy/caddy/Caddyfile`、
  `deploy/systemd/*.service`、`deploy/windows/install-agent.ps1`
- 架构背景：`docs/design/system-design.md §10`
- 决策：`DECISIONS.md` D-022（Web UI 嵌入）、D-023（迁移嵌入 + 部署形态）
