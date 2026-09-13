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
| `overwrite`  | 每次变更上传整文件（**默认**：空字符串会被补齐为 `overwrite`） |
| `tail`       | 追踪字节偏移，仅上传新增部分（断点续传） |
| `close_wait` | 防抖：Write/Create 事件静默一段时间后再整文件上传（写完再传） |

- **值域权威在 Agent watcher**（非 CP：`append_mode` 是自由 TEXT，CP 不做枚举校验，仅默认补齐）：
  `agent/internal/watcher/watcher.go:44`（`AppendModeOverwrite` / `AppendModeTail` / `AppendModeCloseWait` 常量）
- webui 选项清单镜像：`webui/src/pages/Agents/RuleForm.tsx:390`（须与 watcher 常量一致）
- CP 默认补齐逻辑（空→`overwrite`）：`controlplane/internal/api/handler/agents.go:753` 与 `:937`

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
| `{submit_time}` | **该文件被提交上传的时刻**（`submitFile` 时刻，UTC）。⚠️ 不是「当前时刻」的通用时间变量——同一文件重复采集时它是稳定的（IC-BUG-50 / D-034） |
| `{time}`       | ⚠️ **已废弃（deprecated）**：`{submit_time}` 的旧名，**仍可渲染**（语义完全等价），无移除时间表；新建规则一律用 `{submit_time}`。经 UI 旧默认模板创建的存量规则仍带此名 |

- 权威注入点共**两处**（此前只写了第一处）：
  1. `pkg/trollsift/context.go`（`InjectContext`：`agent_name` / `agent_id`）
  2. `pkg/trollsift/uploadfields.go`（`InjectSubmitTime`：`submit_time` + 废弃别名 `time`；
     `filename` / `ext` 由 agent 上传路径构建时注入——`buildStoragePath` 与
     `handleDryRun` 共用 `injectUploadFields`）
- webui 镜像清单：`webui/src/utils/pathTemplate.ts:5`（`SYSTEM_TEMPLATE_VARIABLES`）

### 优先级：解析结果 vs 注入值（IC-BUG-50 / D-034）

**解析结果优先，注入不覆盖**——对全部系统变量与保留字统一成立：
`path_pattern` 从文件相对路径解析出**同名字段**时，一律用解析值；字段缺失才注入。
权威实现在两处注入点本身（`InjectContext` 的 exists 检查、`InjectSubmitTime` 的
inject-if-absent；`TestInjectContext_NoOverwrite`、
`TestBuildStoragePath_SubmitTimeParsedFieldWins` 钉住）。因此：

- 管理员把解析字段命名为 `submit_time`（或旧名 `time`）即可**按数据日期归档**，
  不会被上传时刻静默覆盖（IC-BUG-50 修复前 `{time}` 是无条件覆盖，恰与此相反）。
- 「想要上传时刻」的用法不受影响：解析字段不同名时（大多数规则）注入值生效。
- 字段名共享前缀（如解析字段 `time_zone`）不会被保留字注入误伤——注入是
  inject-if-absent，**不做模板子串前缀匹配**。

### 时间字段（LDML 语法）

`{fieldname:LDML}` 或 `{fieldname:LDML|tz=...}`，UTC 当前时间格式化。支持符号（最长匹配优先）：

| 符号 | 含义 | 符号 | 含义 |
|------|------|------|------|
| `yyyy` | 四位年 | `HH` | 时（24h） |
| `yy`   | 两位年 | `mm` | **分** |
| `MM`   | **月** | `ss` | 秒 |
| `dd`   | 日     |      |    |

> ⚠️ **大小写敏感**：`mm` = 分钟、`MM` = 月，写错**在建规则时不会被拦下**
> （webui/REST 对模板无形状约束，D-030 第八条），**要到上传时才失败**
> （实测报 `month out of range` 一类解析错误，按 IC-BUG-21 任务失败并告警）。

- 权威解析：`pkg/trollsift/parser.go` / `pkg/trollsift/regex.go`
- webui 预览渲染镜像：`webui/src/utils/pathTemplate.ts`（`formatLDML`）
- 决策背景：`DECISIONS.md` **D-010**（引入 `pkg/trollsift` 统一路径模板）、
  **D-034**（`{time}` → `{submit_time}` 改名与优先级对齐）

### 前导 `/` 的归一化（三端共享约定）

**对象键 = 模板渲染结果去掉全部前导 `/`。** 权威实现是
[`pkg/trollsift/normalize.go`](../../pkg/trollsift/normalize.go) 的
`NormalizeTemplate` / `NormalizeObjectKey`（`strings.TrimLeft(s, "/")`——剥的是**全部**
前导分隔符而非一个，因为 agent 侧模板与合成路径各剥一次，只剥一个会让 `//a/{x}` 这类模板
两端再次失配；REST 建规则对模板无任何形状约束，见 `DECISIONS.md` D-030 第八条）。

这条约定有**四个解释者**，IC-1 之前只有 agent 一个做对：

| 端 | 位置 | 状态 |
|---|---|---|
| agent（拼对象键） | `agent/cmd/agent/main.go` `buildStoragePath` | ✅ 一直正确；IC-1 起改调用共享函数 |
| CP（反解 path_var 打标） | `controlplane/internal/indexer/indexer.go` `applyPathVarTags` | ✅ IC-1 修复（此前用未归一化的原始模板 `Parse`，模板带 `/` 时必然失配） |
| webui（模板预览） | `webui/src/utils/pathTemplate.ts` `normalizeTemplate` | ✅ IC-1 修复（此前不剥，预览显示 `/my-agent/…` 而真实键是 `my-agent/…`） |
| agent（dry-run 试运行） | `agent/cmd/agent/main.go` `handleDryRun` | ✅ IC-1 修复（此前用原始模板，且它与 webui 预览显示在**同一个表单**里，两个字段对同一模板给出不同答案） |

webui 新建规则的默认模板就带前导 `/`（`webui/src/pages/Agents/RuleForm.tsx:104`，
D-034 起为 `/{agent_name}/{submit_time:yyyy/MM/dd}/{filename}`；D-034 前是
`/{agent_name}/{time:yyyy/MM/dd}/{filename}`——**经 UI 创建的存量规则仍带旧名**，
agent 侧继续按 deprecated 别名渲染，见上方系统变量表），**经 UI 创建的规则全部命中**——这就是
`docs/tasks/bugs/open.md` **IC-BUG-16** 长期静默的原因。

修复方向是**让 CP 与 webui 剥模板**，不是让 agent 停止剥路径：后者会改写所有既有对象键、需全量重铺。

> **前后端严格度不同是有意的**：webui 的 `validatePathTemplate` 仍拒绝任何 `//`（包括前导），
> 而 Go 侧的 `TrimLeft` 会容忍前导 `//`。二者不矛盾——UI 是给人的即时提示，Go 侧是给
> REST/SDK 建规则兜底（那条路径无任何模板校验）。中间位置的 `a//b` 两侧都救不了，也不打算救。

> ⚠️ **漂移风险点**：系统变量清单与时间符号表当前在
> `pkg/trollsift`（Go，权威）与 `webui/src/utils/pathTemplate.ts`（TS，镜像）**两处手工维护**。
> 修改任一处务必同步另一处；`pkg/trollsift` 为准。（这正是 G-8 契约单一权威想根治的场景，暂以本注记兜底。）
> 加上前导 `/` 的归一化，这条模板契约实际有**四个**手工维护点（Go 侧三处调用 + TS 侧一份镜像实现）。
> Go 与 TS 的归一化是两份独立实现，语义必须保持一致（`TrimLeft(s,"/")` ↔ `replace(/^\/+/,'')`）。

> **`dest_path_template` 不受任何形状约束**——不强制前缀、不要求首段可解析、不要求含时间字段。
> 见 `DECISIONS.md` **D-030 第八条**。

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

## 规划中（未实现，不作为现行契约）

> ⚠️ **本节已部分过期**：6c Phase 1（MT-1…MT-6）已于 2026-07-14 收官（PR #69–#79），
> 下面这条不再是「规划中」。正式条目待回填——IC-BUG-16 恰恰是一条 V-3 契约违规，
> 若当初收官时回填了带 `file:line` 的正式条目，本索引本可拦下它。

- **元数据 / 标签（6c Phase 1，D-025）**：将新增若干跨模块契约——待确认取值队列（`pending_tag_values.status`
  当前设计仅 `pending` 一个活跃态，核准/合并/拒绝即出队，非多态状态机）、文件筛选可重复
  `tag` 查询参数（保持 cursor 分页 V-2）、规则 `metadata.path_tag_map` 复用 trollsift 变量（V-3）。**设计见**
  [`metadata-model.md`](./metadata-model.md)；**实现后**再在本文补入带 `file:line` 权威的正式条目（现在写入会与
  "本文档只记既有契约"原则相悖）。

## 关联

- 契约文件总规则：`CLAUDE.md`「契约文件」表 + 「文档权威优先级」块
- 技术决策：`DECISIONS.md`（D-007 分页、D-010 trollsift、D-018 限流、D-019 事件动作、D-020 规则编辑）
- 差异分析出处：`docs/reports/design-gap-analysis/05-contracts.md` §4
- 单一权威 / 自动校验（OpenAPI + MSW，本文件的工具化后继）：G-8/G-9，已按产品决策推后
  （见 `docs/tasks/core-completeness.md` 文末「明确推后」）
