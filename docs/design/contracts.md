# contracts.md — 跨模块隐性契约参考

> **本文件是参考索引，不是权威来源。** 契约的最终真相始终是所引用的**代码**
> （见 `CLAUDE.md`「文档权威优先级」）。本文件把散落在代码里、只靠口头/记忆维护的隐性约定
> 集中成表并**指向权威代码位置**，供跨模块开发（CP ↔ webui ↔ SDK）快速对齐。
> **发现本文件与代码不符，一律以代码为准，并回来订正本文件。**
>
> 来源：`docs/reports/design-gap-analysis/05-contracts.md` §4「无文档的口头契约」（V-1…V-4）。
> 这些约定在 CC-8 / CC-9 开发期间被反复踩到（`mode` 大小写、status 枚举映射），故成文防复发。

---

## V-1 状态与枚举的大小写 / 命名映射

前端（webui）用**大写**枚举，DB / gRPC 落库用**小写**枚举。CP 在 REST 边界做双向翻译。
**唯一非直译的特例**：DB `online` ⇄ 前端 `RUNNING`；其余仅大小写差异。

### Agent 状态（AgentStatus）

| 前端（大写，API 出入参） | DB（小写） | 说明 |
|--------------------------|-----------|------|
| `PENDING`  | `pending`  | 已注册待审批 |
| `APPROVED` | `approved` | 已审批未连接 |
| `RUNNING`  | `online`   | **特例映射**：在线 |
| `OFFLINE`  | `offline`  | 离线 |
| `REVOKED`  | `revoked`  | 已吊销 |

- 翻译函数：`mapFrontendStatusToDB` / `mapDBStatusToFrontend`
  — `controlplane/internal/api/handler/agents.go:146`
- DB 枚举定义（权威）：`controlplane/internal/db/models.go:61`（`AgentStatus`）
- 前端枚举定义：`webui/src/components/AgentStatusBadge.tsx:5`
- 注：`CLAUDE.md` 生命周期里的 `INIT` 是 **Agent 本地注册前**的状态，不是 DB/API 枚举值——
  API 层可见的状态仅上表五个。

### 采集规则状态（RuleStatus）

| 值 | 说明 |
|----|------|
| `active`   | 生效（agent 在线则热重载） |
| `inactive` | 停用 |

- 定义：`controlplane/internal/db/models.go:196`
- 注：REST `PUT .../rules/:rid` 的 status-only 形态传的就是该小写值（CC-9 / D-020）。

### 上传模式（UploadMode）

| 值 | 说明 |
|----|------|
| `watch`     | 实时监听 |
| `scheduled` | 定时（配 `cron_expr`） |

- 定义：`controlplane/internal/db/models.go:238`
- **入参大小写宽容**：CP 在 create/update 规则时对 `mode` 做 `strings.ToLower` 后再校验，
  **落库统一小写**——前端传 `WATCH`/`Watch`/`watch` 皆可，非法值返回 `422`
  （`controlplane/internal/api/handler/agents.go:757` 与 `:924`）。

### 追加模式（append_mode）

| 值 | 说明 |
|----|------|
| `overwrite` | 覆盖（**默认**：空字符串会被补齐为 `overwrite`） |
| `tail`      | 断点续传追加 |

- 默认补齐逻辑：`controlplane/internal/api/handler/agents.go:753` 与 `:937`

### 上传日志状态（upload-log status，前端）

| 值 | 说明 |
|----|------|
| `SUCCESS` / `FAILED` / `PENDING` | 单条上传结果 |

- 前端类型：`webui/src/services/upload-logs.ts:10`

---

## V-2 列表与响应信封

CP 的 REST 响应有三种固定信封形状，按端点类型选用：

### 1. Cursor 分页（快增长表，如 files）

```json
{ "items": [ ... ], "total": 1234, "has_more": true, "next_cursor": "opaque-token" }
```

- 权威：`controlplane/internal/api/handler/files.go:181`
- `has_more` 检测：查询多取一条（`limit+1`）判断是否还有下一页（`files.go:121`）
- **契约：cursor-based，禁止改成 offset-based**（`CLAUDE.md` 契约表 + `DECISIONS.md` D-007）。
  `next_cursor` 是不透明游标，客户端原样回传，非法游标返回 `400 INVALID_CURSOR`。

### 2. 简单列表（慢增长表，如规则列表 / dry-run 结果）

```json
{ "items": [ ... ], "total": 12 }
```

- 例：`controlplane/internal/api/handler/agents.go:266`（规则列表）、`files.go:367`
- 此处 `total` = 本次返回条数（非全表计数），不分页。

### 3. 单对象包裹

```json
{ "data": { ... } }
```

- 例：`controlplane/internal/api/handler/files.go:303`
- 注：并非所有单对象端点都用 `data` 包裹（部分直接返回对象或 `{token,message}` 等专用形状）；
  以各 handler 实际响应体为准。

> **分页策略背景**：为何快/慢增长表用不同信封，见 `DECISIONS.md` D-007。

---

## V-3 路径模板变量（trollsift）

采集规则的 `dest_path_template` 用 `pkg/trollsift` 渲染。变量分两类：

### 系统变量（Agent 注入，固定可用）

| 变量 | 含义 |
|------|------|
| `{agent_name}` | Agent 显示名 |
| `{agent_id}`   | Agent UUID |
| `{filename}`   | 原始文件名（含扩展名） |
| `{ext}`        | 扩展名（不含点） |

- 权威注入点：`pkg/trollsift/context.go`（`InjectContext`：`agent_name` / `agent_id`；
  `filename` / `ext` 由 agent 上传路径构建时注入）
- webui 镜像清单：`webui/src/utils/pathTemplate.ts:5`（`SYSTEM_TEMPLATE_VARIABLES`）

### 时间字段（LDML 语法）

`{fieldname:LDML}` 或 `{fieldname:LDML|tz=...}`，UTC 当前时间格式化。支持符号（最长匹配优先）：

| 符号 | 含义 | 符号 | 含义 |
|------|------|------|------|
| `yyyy` | 四位年 | `HH` | 时（24h） |
| `yy`   | 两位年 | `mm` | 分 |
| `MM`   | 月     | `ss` | 秒 |
| `dd`   | 日     |      |    |

- 权威解析：`pkg/trollsift/parser.go` / `pkg/trollsift/regex.go`
- webui 预览渲染镜像：`webui/src/utils/pathTemplate.ts`（`formatLDML`）
- 决策背景：`DECISIONS.md` **D-010**（引入 `pkg/trollsift` 统一路径模板）

> ⚠️ **漂移风险点**：系统变量清单与时间符号表当前在
> `pkg/trollsift`（Go，权威）与 `webui/src/utils/pathTemplate.ts`（TS，镜像）**两处手工维护**。
> 修改任一处务必同步另一处；`pkg/trollsift` 为准。（这正是 G-8 契约单一权威想根治的场景，暂以本注记兜底。）

---

## V-4 REST 错误响应格式

所有 API 错误返回统一信封，**含顶层 `request_id`**（CC-5 起全量对齐）：

```json
{
  "error":   { "code": "AGENT_OFFLINE", "message": "agent is not online", "detail": null },
  "request_id": "uuid-v4-or-client-provided"
}
```

- 结构定义（权威）：`controlplane/internal/api/middleware/error.go:31`（`ErrorResponse` / `ErrorDetail`）
- 写入口：
  - `RespondError(...)` — 在 handler 内使用（写完即 `return`）
  - `AbortWithError(...)` — 在中间件内使用（需中止后续 handler）
  - 二者共用同一信封，`error.go:45`–`72`
- `request_id` 来源：`RequestID()` 中间件——优先复用客户端 `X-Request-ID` 头，否则生成 UUID v4，
  并回写响应头 `X-Request-ID`（`error.go:15`）
- 未知 query 参数：`RejectUnknownQuery` 把"静默忽略的错拼过滤器"变为显式
  `400 INVALID_QUERY_PARAM`（`error.go:74`，CC-5）

### 常见错误码（代表性，非穷举）

| code | 典型 HTTP | 场景 |
|------|-----------|------|
| `VALIDATION_ERROR` / `INVALID_REQUEST` / `BAD_REQUEST` | 400 / 422 | 请求体或参数不合法 |
| `INVALID_QUERY_PARAM`   | 400 | 未知 query 参数（CC-5） |
| `INVALID_CURSOR`        | 400 | 分页游标非法 |
| `INVALID_STATUS` / `INVALID_ROLE` / `INVALID_BUCKET_ID` | 400 | 枚举/外键取值非法 |
| `INVALID_PATTERN` / `INVALID_BUCKET_NAME` | 422 | 规则模式 / 名称校验失败 |
| `INVALID_ACTION_TYPE` / `INVALID_ACTION_CONFIG` | 400 | 事件动作类型/配置非法（CC-7） |
| `MISSING_TOKEN` / `INVALID_TOKEN` / `TOKEN_REVOKED` / `INVALID_TOKEN_SCHEME` | 401 | 认证失败 |
| `INVALID_CREDENTIALS` / `ACCOUNT_DISABLED` / `WRONG_TOKEN_TYPE` | 401 | 登录/令牌类型问题 |
| `FORBIDDEN` | 403 | 权限不足（如非 super_admin） |
| `NOT_FOUND` | 404 | 资源不存在 |
| `AGENT_OFFLINE` | 409 | 目标 agent 不在线 |
| `RATE_LIMITED` | 429 | 触发 API 限流（CC-4，带 `Retry-After`） |
| `AGENT_ERROR` / `MINIO_ERROR` | 502 | 下游（agent / MinIO）返回错误 |
| `TIMEOUT` | 504 | 同步等待 agent 命令超时 |
| `INTERNAL_ERROR` / `TOKEN_ERROR` | 500 | 服务端内部错误 |
| `NOT_IMPLEMENTED` | 501 | 端点未实现（占位） |

> 完整、最新的错误码以各 handler 代码为准；上表为跨模块对接时的速查。

---

## 关联

- 契约文件总规则：`CLAUDE.md`「契约文件」表 + 「文档权威优先级」块
- 技术决策：`DECISIONS.md`（D-007 分页、D-010 trollsift、D-018 限流、D-019 事件动作、D-020 规则编辑）
- 差异分析出处：`docs/reports/design-gap-analysis/05-contracts.md` §4
- 单一权威 / 自动校验（OpenAPI + MSW，本文件的工具化后继）：G-8/G-9，已按产品决策推后
  （见 `docs/tasks/core-completeness.md` 文末「明确推后」）
