# 06 — 动态端到端验证结果

> 环境：本地 `docker-compose.dev.yml`（PG/Redis/MinIO/NATS 全 healthy）
> 二进制：`make build` 成功；CP 以 `deploy/config/controlplane.env` 启动，健康检查 200。
> 日期：2026-07-03

---

## 测试状态汇总

| 检查项 | 结果 |
|--------|------|
| 全部 Go 单测（CP + agent） | ✅ 通过 |
| webui vitest（68 项） | ✅ 通过 |
| make build | ✅ CP + agent 二进制产出 |
| 基础设施启动 | ✅ 4 容器 healthy |
| MinIO 初始化 | ⚠️ bucket/SA 创建成功，**tmp-uploads lifecycle 设置失败**（见 D-1） |
| CP 启动 + 迁移 | ✅ |
| 登录 / me / 列表端点 | ✅ 契约正确 |
| **SDK refresh 契约** | ❌ **实测断裂**（见 E-2，本次分析最重要的动态发现） |

---

## 关键动态发现

### E-2 🔴 SDK 与 CP 的 refresh 契约不兼容（联调必炸）

实测：
```
SDK  auth.py:130  →  POST /api/auth/refresh   body: {"refresh_token": "..."}
CP   auth.go Refresh →  只读 Authorization: Bearer <refresh_token> 头
实测响应：{"error":{"code":"MISSING_TOKEN","message":"Bearer token required"}}
```
- SDK 把 refresh token 放在 **JSON body**；CP 只认 **Authorization 头**。
- 后果：SDK 的 access token 到期后，自动刷新 100% 失败 → 回退重新登录（用户名密码）。
  若 SDK 以只读 API-user + 短 access TTL（2h）运行，表现为"每 2 小时莫名重登一次"，
  或在无密码、仅 token 的用法下直接 401。
- 这就是 T3-3 尚未开跑、但**已注定失败**的契约点——与 T3-2-FIX 同一类病。
- 设计 §5.3.2 只写"用 Refresh Token 换新 Access Token"，未规定放 body 还是 header
  → 两边各自实现 → 漂移。**设计粒度不足是根因**。

### E-1 🟡 Agent JWT Token 无续期，30 天后集体掉线风险

代码链确认：
- CP 给 Agent 签发的是 **AccessToken**，TTL = `JWT_ACCESS_TOKEN_TTL`（默认 **2h**，
  `manager.go` 用 `accessTTL`；`main.go:182` 传入 `cfg.JWTAccessTokenTTL`）。
- 设计 §4.7 说 Agent token 默认 30 天、剩余 <20% 时"通过已有 gRPC 连接续期"。
- 但 proto 里**没有 token 续期 RPC**（只有 STS 的 RefreshCredentials）。
- Agent 侧 `IsTokenValid(0.1)` 只在**启动/重注册**时检查（`registration.go:123`），
  长连接运行期间不会主动换 token。
- **已确认**：gRPC 拦截器 `interceptor.go:109-112` 调用 `ValidateToken`，
  经 `jwt.ParseWithClaims` 强制校验 `exp`。因此危险分支成立：
  - 长连接保持期间不校验 token（stream 打开时只验一次），token 过期不影响在途连接；
  - 但**任何重连**（网络抖动、CP 重启、TCP 半开）发生在签发 2h 之后，
    `openStream` 会带着已过期的 Bearer token 重连 → 被 Unauthenticated 拒绝
    → `runLoop` 用同一过期 token 无限重试 → **Agent 永久离线，无法自愈，只能重启**。
  - 这是一枚定时炸弹：部署超过 2 小时的 Agent，一次网络抖动就可能永久掉线。
- 附录 C.1 的 `AGENT_TOKEN_TTL=720h` 配置项在 CP 中根本不存在。

### D-1 🟢 tmp-uploads Lifecycle 未设置成功
`init-minio.sh` 设置 7 天生命周期报错 `Unable to read ILM configuration`。
配合 01 报告"CP 侧无 Lifecycle 代码"，tmp-uploads 永不自动清理。

---

## 契约正确性实测（这些是好的）

| 端点 | 实测响应 | 判定 |
|------|---------|------|
| POST /api/auth/login | `{access_token, refresh_token, expires_in, token_type, user}` | ✅ 与 SDK `_update_tokens` 期望键一致 |
| GET /api/auth/me | 扁平 `{id, org_id, role, username}` | ✅ |
| GET /api/v1/agents | `{items:[...], total}`，agent 含 `status:"OFFLINE"`, `is_online` | ✅ 信封 + 大写枚举符合 T3-2-FIX |
| GET /api/v1/files | `{items, total, next_cursor, has_more}` | ✅ cursor 分页信封正确 |
| GET /api/v1/buckets | `{items:[data-sensor, tmp-uploads], total}` | ✅ |

## 契约瑕疵实测

| 项 | 实测 | 设计要求（§5.11） | 判定 |
|----|------|------------------|------|
| 错误响应体 | `{"error":{"code","message"}}` | `{error:{code,message,detail}, request_id}` | ⚠️ 缺顶层 `request_id`；SDK/前端若依赖它做链路追踪会拿不到 |
| files 未知过滤参数 | `?file_type_name=&path_prefix=&start_time=` 返回 200 | — | ⚠️ 静默忽略未知/无效参数，无 400。SDK FileQuery 参数名若拼错，表现为"过滤不生效"而非报错，难排查 |

---

## 观察到的遗留数据
DB 中存在一台历史测试 agent（name=Rita, OFFLINE, last_seen 2026-06-16）。
说明此环境曾完成过注册审批联调，agent 主链路历史上跑通过。本次未重跑完整
注册→审批→采集→上传（agent config 指向远程 endpoint，需改配置；E-1/E-2 已足够支撑结论）。

## 建议补跑（下一步，可选）
将 `agent/config.toml` 的 endpoint 改为 `localhost:9090`、tls 关闭，走一遍
注册→审批→推规则→落文件→MinIO 事件→file_entries→预签名下载，验证 §5.8 索引闭环与
心跳载荷为空（02 报告 §2）的实际影响。
