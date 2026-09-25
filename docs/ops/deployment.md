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

### 0.1 MinIO 镜像从哪来（D-036，必读）

上游已经**没有任何公共通路**能拉到 MinIO 镜像：Docker Hub 的 `minio/minio` / `minio/mc`
与 `dl.min.io` 的 `mc` 下载在 2026 年被移除，`quay.io/minio/minio` 此后也不再公开可读
（匿名 token 能签发，但取 manifest 返回 401），`ghcr.io/minio/minio` 返回 403。
所以三个 compose 现在钉的是**我们自持的私有镜像**，按 **digest** 而非 tag：

```
ghcr.io/byw-dev/minio@sha256:a66e1fd7e5cc10cbbc4d5a24bb4b81ae3a17b4000db6535e450c0efbdc447fee
```

它是一个多架构 manifest list（`linux/amd64` + `linux/arm64`），内容是上游
`RELEASE.2025-04-22T22-12-26Z`（AGPL-3.0），只加了 provenance 标签，**没有新增层、
没有执行任何命令**（「文件系统与上游逐字节相同」的作用域是我们的构建相对于
它 `FROM` 的那个基础镜像；整个镜像是否为官方发布那份的对照缺口见
`DECISIONS.md` D-036「已知缺口」）。镜像的 `org.opencontainers.image.source`
指向 **`github.com/byw-dev/minio`**（源码，见下方 AGPL 说明），而不是本仓库——
本仓库只是它的**消费方**（记在 `io.byw.mirror.consumer`）。

**两条取得路径，按环境选一条：**

**① 能访问 GitHub 的环境** —— 私有 package，先登录：

```bash
docker login ghcr.io -u <github-user>          # 需要 read:packages 的 PAT
docker compose -f deploy/docker-compose.prod.yml pull minio
```

**② 离线 / 客户现场** —— 用交付的 tarball 导入，全程不联网。
⚠️ 以下每条命令都在 Apple Silicon + Docker Desktop 上实测过（2026-09-25）；
实测结论决定了流程形状：**`docker save` 按 digest 引用只保存当前平台**（一个 tarball
一个架构，amd64 与 arm64 要各导一份）；**`docker load` 只恢复镜像内容（Image ID），
不恢复 RepoDigest**——load 后 `name@sha256:…` 引用在本地**解析不到**，compose 会转而
访问 GHCR。所以离线侧必须显式打 tag，并用环境变量 `FA_MINIO_IMAGE` 指给 compose
（这是离线交付的逃生舱，不是给人随便换镜像用的；不设该变量时三个 compose 仍按
digest 钉住，行为与本节开头逐字节一致）：

```bash
# 在有网络的机器上导出（每个架构一份；--platform 必须显式给）：
docker pull --platform linux/amd64 ghcr.io/byw-dev/minio@sha256:a66e1fd7e5cc10cbbc4d5a24bb4b81ae3a17b4000db6535e450c0efbdc447fee
docker save --platform linux/amd64 ghcr.io/byw-dev/minio@sha256:a66e1fd7e5cc10cbbc4d5a24bb4b81ae3a17b4000db6535e450c0efbdc447fee -o fileagent-minio-amd64.tar
docker pull --platform linux/arm64 ghcr.io/byw-dev/minio@sha256:a66e1fd7e5cc10cbbc4d5a24bb4b81ae3a17b4000db6535e450c0efbdc447fee
docker save --platform linux/arm64 ghcr.io/byw-dev/minio@sha256:a66e1fd7e5cc10cbbc4d5a24bb4b81ae3a17b4000db6535e450c0efbdc447fee -o fileagent-minio-arm64.tar

# tarball 自身的完整性校验值随交付一起提供（交付单里记录，目标机上比对）：
shasum -a 256 fileagent-minio-amd64.tar fileagent-minio-arm64.tar
```

```bash
# 在目标机器上导入（按机器架构选对应 tarball）：
shasum -a 256 fileagent-minio-amd64.tar        # 与交付单记录的 SHA-256 比对，不一致即停止
docker load -i fileagent-minio-amd64.tar
# load 输出的是 Image ID（悬空镜像，无 tag、无 RepoDigest）——显式打 tag：
docker tag <load 打出的 Image ID> fileagent-minio:RELEASE.2025-04-22T22-12-26Z
export FA_MINIO_IMAGE=fileagent-minio:RELEASE.2025-04-22T22-12-26Z

# 导入校验：Image ID 与交付单记录的期望值比较（不相等就退出，不要硬起）：
#   linux/amd64 期望 sha256:a2c4bb0ac69eca344c26b8ceb07f1cf9eb2afdef55157ea5d045b56e150e0f9e
#   linux/arm64 期望 sha256:adffe052fa1ad81a757cbb753d2208c9459989808c19539891be95eaaac1e5e4
ACTUAL="$(docker image inspect fileagent-minio:RELEASE.2025-04-22T22-12-26Z --format '{{.Id}}')"
EXPECTED="<见上，按架构取>"
[ "$ACTUAL" = "$EXPECTED" ] || { echo "镜像与交付单不符：$ACTUAL" >&2; exit 1; }

docker compose -f deploy/docker-compose.prod.yml up -d minio
```

> ⚠️ **在线环境不要设 `FA_MINIO_IMAGE`**：不设时 compose 用 §0.1 开头的 digest
> （三个 compose 逐字节等价已用 `docker compose config` 前后对比验证过）；
> 设了它会整体覆盖 digest 钉定，绕过「按 digest 钉」的意图。

> ⚠️ **AGPL-3.0 的分发义务**：本系统是私有化交付，交付物里包含 MinIO，因此
> 随交付**应当提供 AGPL-3.0 许可副本与对应源码的获取途径**。如何满足 AGPLv3 §6
> 取决于 conveyance 方式（该条列了 6(a)–(e) 多种路径，且「Corresponding Source」
> 的定义还包含控制生成、安装、运行所需的脚本）——**这不是技术证据能单方下结论的事，
> 本节描述的是本项目选择的合规方案，是否充分待法务确认**。
>
> **本项目选择的方案**：`github.com/byw-dev/minio`（MinIO 官方源码的 fork，
> 已同步全部 tag）的 tag **`RELEASE.2025-04-22T22-12-26Z`**——与上面这个镜像一一对应。
> 该仓库是**公开**的（fork 只能与上游保持一致的可见性）。**二进制私有、源码公开**，
> 是本项目当前选择的拆分。
>
> **实际随交付提供的物项（待法务逐项确认的清单，不是已达标的结论）**：
> ① AGPL-3.0 许可副本；② 源码获取途径（上述公开 tag，及归档快照）；③
> 交付 tarball 的构建方式说明；④ 源码的可获得期限。任一项若法务认定不充分，
> 需另行补足（例如随交付附源码归档包）。
>
> 上游 MinIO 仓库已归档，所以**长期保有那份源码的责任在我们这边**——不要指望上游还在。
> 详见 `DECISIONS.md` D-036。

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
> 用 compose 已钉 digest 的 **MinIO 镜像**执行——它同时自带 mc 和 curl。
> **不要用 `minio/mc` 镜像：它没有 curl**，脚本会在建任何资源之前就报错退出——
> 何况 `minio/mc` 也已经拉不到了（D-036）。`mc` 现在**只能**从这个服务端镜像里取。
>
> ```bash
> docker run --rm --network <compose 网络> \
>   -v "$PWD/deploy/scripts:/s:ro" \
>   -e MINIO_ENDPOINT=http://minio:9000 \
>   -e MINIO_ROOT_USER=<同上> -e MINIO_ROOT_PASSWORD=<同上> \
>   -e CP_ADMIN_ACCESS_KEY=<同 A.1> -e CP_ADMIN_SECRET_KEY=<同 A.1> \
>   -e WEBHOOK_AUTH_TOKEN=<同 INTERNAL_WEBHOOK_SECRET> \
>   --entrypoint bash ghcr.io/byw-dev/minio@sha256:a66e1fd7e5cc10cbbc4d5a24bb4b81ae3a17b4000db6535e450c0efbdc447fee /s/init-minio.sh
> ```
>
> 说明：镜像 digest 与 `docker-compose.prod.yml` 里钉的一致（见 §0.1；**没有「换新版」这个选项了**，
> 上游已无供给）；`--entrypoint bash` 是必须的，镜像默认 entrypoint 是 MinIO 自己的启动脚本。容器内用
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
