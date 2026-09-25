# DECISIONS.md — 技术决策记录

> 此文件记录项目中所有重要的技术决策，包括选择原因和替代方案。
> 修改任何契约文件（proto / 迁移 / Docker Compose 端口）前，必须在此记录。

---

## D-001 Go 模块结构与工作空间（T0-5）

**决策日期**：2026-04-30  
**影响范围**：controlplane / agent / api/v1

### 决策

采用 Go Workspace（go.work）将三个独立 Go 模块整合到同一个 Monorepo：

| 模块路径 | 模块名称 |
|---------|---------|
| `/`（根目录） | `github.com/byw-dev/fileagent` |
| `controlplane/` | `github.com/byw-dev/fileagent/controlplane` |
| `agent/` | `github.com/byw-dev/fileagent/agent` |

根模块（`github.com/byw-dev/fileagent`）负责管理 `api/v1/` 下的 gRPC 生成代码。  
`controlplane` 和 `agent` 在 Phase 1 实现 gRPC 调用时，通过 go.work 的本地替换机制引用根模块，无需发布。

### 有效 Go 最低版本

计划目标版本为 Go 1.22，但由于 `google.golang.org/grpc@v1.79.3`（安全补丁版本）
的传递依赖 `golang.org/x/crypto@v0.46.0` 要求 Go 1.24，三个模块的 `go` 指令均
自动设置为 **go 1.24.x**。

**已知安全约束**：
- `google.golang.org/grpc@v1.79.3`：CVE 修复版本（路径权限绕过漏洞）；<1.79.3 受影响。
- `github.com/jackc/pgx/v5@v5.9.0`：CVE 修复版本（内存安全）；需 Go 1.25，**暂未引入**。
  Phase 1 T1-A2 运行 sqlc 代码生成后，待确认 pgx 可用的兼容版本后再添加。

### 路径规则：go.work 与 go.mod 中只允许使用相对路径

`go.work` 的 `use` 指令和 `go.mod` 的 `replace` 指令均支持绝对路径，但**本项目禁止使用绝对路径**：

- 绝对路径与机器环境绑定，提交到仓库后任何其他开发者 clone 均会立即构建失败。
- 相对路径相对于 `go.work` / `go.mod` 所在目录解析，在任意机器上都可重现。

**强制规则**：
- `go.work` 中的 `use` 指令只能写 `./子目录` 形式（如 `./agent`、`./controlplane`）。
- 任何 `go.mod` 中若需要 `replace`，目标路径也必须是相对路径（如 `replace foo => ../foo`）。

### 备选方案（被否决）——D-001

- **单一根 go.mod**：会将 controlplane 和 agent 的依赖混在一起，不利于二进制隔离。
- **offset 参数 -go=1.22 强制保留**：go mod tidy 会因传递依赖版本冲突而报错，无法满足验收标准。

---

## D-002 依赖锚定机制（T0-5）

**决策日期**：2026-04-30  
**影响范围**：controlplane / agent

### 决策

在 `controlplane/tools.go` 和 `agent/tools.go` 中使用 `//go:build tools` 构建标签
对所有 Phase 1 将要用到的依赖进行空白导入（`_ "pkg"`）。

此方式确保：
- `go mod tidy` 不会删除这些依赖（tidy 会读取所有构建约束文件）。
- 正常 `go build ./...` 不会编译这些占位文件（构建标签 `tools` 不在默认标签集内）。
- Phase 1 开发者可以直接 `import` 相应包，无需再 `go get`。

---

## D-003 统一构建约定：Makefile + bin/ 输出目录

**决策日期**：2026-04-30  
**影响范围**：controlplane / agent / CI / 开发者工作流

### 背景

Go 编译产物在 Linux/macOS 上没有固定扩展名，仅靠 `.gitignore` 中的 `*.exe`
等后缀规则无法拦截；历史上曾发生过将二进制直接提交进 git 的情况。

### 决策

1. **所有二进制输出统一写入项目根目录的 `bin/` 目录**。  
   `.gitignore` 追加 `/bin/`，确保编译产物不进入版本控制。

2. **根目录创建 `Makefile` 作为统一构建入口**，提供以下 target：

   | target | 说明 |
   |--------|------|
   | `make build` | 构建全部二进制（controlplane + agent） |
   | `make build-controlplane` | 仅构建 controlplane，输出 `bin/controlplane` |
   | `make build-agent` | 仅构建 agent，输出 `bin/agent` |
   | `make test` | 运行全部单元测试 |
   | `make tidy` | 整理所有模块 `go.mod` / `go.sum` |
   | `make clean` | 删除 `bin/` 目录 |

3. **`cmd/` 目录结构保持不变**（`controlplane/cmd/server/`、`agent/cmd/agent/`）。  
   这符合 Go 社区 [golang-standards/project-layout](https://github.com/golang-standards/project-layout) 的事实标准，
   被 Kubernetes、Prometheus、etcd 等主流项目采用：每个子目录名即二进制名。

### 备选方案（被否决）

- **各模块目录内各自 `go build`，输出到模块自身目录**：产物位置分散，CI 脚本难以统一收集。
- **根目录 `go build ./...`**：多模块 workspace 下行为不直观，无法控制各二进制输出名称。

---

## D-004：Agent Token 哈希算法改用 SHA-256

- **日期**：2026-04-30
- **状态**：已接受

### 背景

JWT 访问令牌长度通常超过 72 字节。bcrypt 在处理超过 72 字节的输入时会静默截断，
导致不同 token 可能哈希到相同值，产生碰撞风险。

### 决策

在 `internal/agent/manager.go` 中，存储 Agent Token 时改用 `SHA-256` hex 哈希
（而非 bcrypt）。SHA-256 输出固定 64 字节 hex 字符串，不存在截断问题，且计算速度更快。

验证时同样计算 SHA-256 hex 与存储值对比，不再使用 bcrypt.CompareHashAndPassword。

### 备选方案

- **继续用 bcrypt，截断到 72 字节**：不可接受，不同 token 可能碰撞。
- **用 bcrypt，先 base64 encode**：引入复杂度，且 base64 输出仍可能超 72 字节。

---

## D-005：controlplane 覆盖率统计口径

- **日期**：2026-04-30
- **状态**：已接受（修订于 2026-05-01）

### 背景

`internal/db` 包是 **sqlc 自动生成代码**，所有函数都需要真实 PostgreSQL 连接，
单元测试环境无法覆盖（原始状态 0%）。若将其计入总覆盖率，整体数字会被拉低至 ~54%。

### 决策

1. 采用 `go-sqlmock v1.5.2` 为 sqlc 生成的 query 函数编写单元测试（使用 `sqlmock.New()` 返回
   实现了 `database/sql` 标准接口的 mock DB，与 sqlc 生成代码的 `DBTX` 接口完全兼容）。
2. 同步对 `internal/event`、`internal/indexer` 等包抽象 `EngineStore` / `IndexerStore` 接口，
   通过 mock 实现完成业务逻辑单元测试覆盖。
3. 当前状态（Phase 2 Group A 完成后，含 go-sqlmock 测试）：
   - 总覆盖率（含 `internal/db`）：**81.2%**（≥ 80% 阈值 ✓）
   - `internal/db`：80.8%，`internal/indexer`：91.4%，`internal/event`：87.5%
   - `internal/grpcserver`：87.0%，核心业务包均 ≥ 80%
4. 集成测试（`go test -tags=integration`）覆盖真实 DB 路径，满足端到端验证要求。

### 备选方案

- **排除 `internal/db` 统计**：考虑过但未采纳；通过 go-sqlmock 实现了包含 DB 层的完整覆盖。
- **使用 `go-sqlmock/v2`**：v2 不存在于代理缓存中，改用 v1.5.2（API 稳定，无已知 CVE）。
- **接受 <80% 总覆盖率**：不符合 AGENTS.md 要求，已通过 sqlmock 测试解决。

---

## D-006：Web UI 运行时版本选型（Node.js + pnpm）

- **日期**：2026-05-03
- **状态**：已接受

### 背景

项目初始时使用 Node.js 20 LTS（Iron）+ pnpm 9，但 Node.js 20 LTS 的 EOL 已于
2026 年 4 月到来，继续使用会失去安全维护。同时评估了是否直接跳至最新版本。

### 决策

| 工具 | 版本 | 说明 |
|------|------|------|
| Node.js | **24.15.0 (LTS krypton)** | 当前活跃 LTS，EOL ≈ 2030 年 4 月，提供最长支持窗口 |
| pnpm | **11.0.4** | 与 Node.js 24 一同发布的最新稳定版，lockfile 格式（v9.0）与 pnpm 9.x 兼容，无破坏性迁移 |
| Corepack | 内置于 Node.js 16.9+ | 通过 `package.json` 的 `packageManager` 字段强制版本一致性 |

版本通过 `webui/package.json` 的以下字段锁定，Corepack 会在执行 `pnpm install` 时
自动检测并强制使用 `pnpm@11.0.4`：

```json
{
  "packageManager": "pnpm@11.0.4",
  "engines": {
    "node": ">=24.0.0 <25.0.0",
    "pnpm": ">=11.0.0 <12.0.0"
  }
}
```

### 升级可行性评估

**优势：**
- Node.js 24 LTS 支持周期到 2030 年，比 Node.js 20（已 EOL）或 22（2027 年）更长
- V8 12.4 引擎提升：原生 `fetch`、更好的 ESM 模块支持、内存性能改善
- Node.js 24 开始默认启用 Corepack，`corepack enable` 后无需额外安装 pnpm
- pnpm 11 改进了 workspace 协议处理和 peer deps 解析
- 当前所有依赖（Vite 8、React 19、TypeScript 6）均明确支持 Node.js 24

**无副作用确认：**
- pnpm 11 的 lockfile 格式（`lockfileVersion: '9.0'`）与 pnpm 9.x 相同，无迁移成本
- `pnpm install --frozen-lockfile`、`pnpm build`、`pnpm test`（48/48）全部通过
- 无任何依赖兼容性错误或警告

### 开发者一致性机制

1. 运行 `corepack enable`（一次性操作，或通过 CI 脚本自动完成）
2. `pnpm install` — Corepack 自动匹配并使用 `pnpm@11.0.4`，无需手动安装 pnpm
3. CI 中使用 `pnpm install --frozen-lockfile` 防止 lockfile 意外变更

### 备选方案（被否决）

- **继续用 Node.js 20 + pnpm 9**：Node 20 已 EOL，无安全更新，不可接受。
- **升级到 Node.js 22 LTS（Jod）**：支持到 2027 年 4 月，比 Node 24 短 3 年；既然升级，直接到更长生命周期版本更合理。
- **使用 node-version-file（.nvmrc / .node-version）**：与 `packageManager` + Corepack 方案相比，需要额外工具（nvm/fnm），且对 pnpm 版本无约束力。

---

## D-011 upload-log 响应不返回 agent_name 字段（T3-2-FIX-G）

**决策日期**：2026-05-08
**影响范围**：controlplane API、Web UI

### 决策

`GET /api/v1/upload-logs` 和 `GET /api/v1/agents/:id/upload-logs` 响应的每条上传日志记录**不返回 `agent_name` 字段**。

### 原因

1. 上传日志 DB 表（`upload_logs`）仅存储 `agent_id`，不冗余存储 `agent_name`；
2. 在 API 层 JOIN agents 表会带来额外的查询开销，且与最小改动原则不符；
3. 前端可通过已有的 `agent_id` 字段（缩短 UUID 显示）满足展示需求，无需完整名称。

### 替代方案（被否决）

- **方案 B（JOIN agents 表）**：额外 JOIN，增加查询复杂度，也不符合 sqlc 生成代码的范式。
- **方案 C（前端额外请求 `/api/v1/agents/:id`）**：列表页需多次请求，性能差。

---

## D-007：分页策略——快增长表 vs 慢增长表（T3-2-FIX-I）

**决策日期**：2026-05-09  
**影响范围**：controlplane API、Web UI

### 背景

系统中存在两类端点：
- **快增长表**：`file_entries`（上传文件）、`upload_logs`（上传日志）、`event_deliveries`（事件投递记录）——数据量随运行时间快速增长，全量拉取会导致内存溢出和响应超时。
- **慢增长表**：`agents`（采集器）、`collection_rules`（采集规则）、`buckets`（存储桶）、`file_types`（文件类型）、`event_rules`（事件规则）、`users`（用户）——通常条目较少且增加缓慢，全量拉取可接受，前端可自行分页。

### 决策

**快增长表必须服务端分页**，响应格式严格遵循 §8.5：
```json
{
  "items": [...],
  "total": 1234,
  "next_cursor": "eyJpZCI6InV1aWQxMjMifQ==",
  "has_more": true
}
```
实现方式：
1. 数据库层请求 `limit + 1` 条记录；若返回 `limit + 1` 条则 `has_more = true`，截断到 `limit` 条；
2. 并发发出 `COUNT(*)` 查询，返回 `total`（符合过滤条件的记录总数，不受 cursor 影响）；
3. 当 `has_more = true` 时，对最后一条记录编码生成 `next_cursor`；否则 `next_cursor` 为空字符串。

**慢增长表不强制服务端分页**，响应格式为：
```json
{
  "items": [...],
  "total": N
}
```
全量返回列表，前端可通过 ProTable 本地分页展示。当条目量增长到影响性能时，再按需升级为服务端分页。

### 涉及端点

| 端点 | 表 | 分页方式 |
|------|---|---------|
| `GET /api/v1/files` | `file_entries` | 服务端 cursor 分页 ✅ |
| `GET /api/v1/upload-logs` | `upload_logs` | 服务端 cursor 分页 ✅ |
| `GET /api/v1/agents/:id/upload-logs` | `upload_logs` | 服务端 cursor 分页 ✅ |
| `GET /api/v1/event-rules/:id/deliveries` | `event_deliveries` | 服务端 cursor 分页 ✅ |
| `GET /api/v1/agents` | `agents` | 全量（前端分页） |
| `GET /api/v1/agents/:id/rules` | `collection_rules` | 全量（前端分页） |
| `GET /api/v1/buckets` | `buckets` | 全量（前端分页） |
| `GET /api/v1/file-types` | `file_types` | 全量（前端分页） |
| `GET /api/v1/event-rules` | `event_rules` | 全量（前端分页） |
| `GET /api/v1/users` | `users` | 全量（前端分页） |

### 替代方案（被否决）

- **全部端点统一 cursor 分页**：慢增长表条目极少，增加实现复杂度而收益低。
- **全部端点全量返回**：快增长表在生产环境会触发内存和超时问题，不可接受。
- **offset 分页**：§8.5 明确规定使用 cursor-based pagination，offset 分页在高偏移量时性能差且存在分页漂移问题。

---

## D-008：CP→Agent 异步命令封装为同步 HTTP 响应（30s 等待）

**决策日期**：2026-05-11  
**影响范围**：`controlplane/internal/api/handler`、`controlplane/internal/dirstore`、`controlplane/internal/grpcserver`

### 背景

系统中存在若干 REST 接口，其语义是：Control Plane 向 Agent 发送一条异步 gRPC 命令，再等待 Agent 通过双向流把结果回传。典型场景是：

- `POST /api/v1/agents/:id/list-dir`：Web UI 请求 CP，CP 再通过 gRPC 双向流向 Agent 发 `ListDirectoryCommand`，Agent 执行目录遍历后通过同一流回传 `DirectoryListing`。

若直接返回 `202 Accepted + request_id`，前端需要轮询或维护 WebSocket 连接，实现复杂度高。

### 决策

**将此类 CP→Agent 命令封装为对前端同步等待的 HTTP 请求**，最长等待时间固定为 **30 秒**：

1. REST handler 在发送命令前，向 `dirstore.Store`（内存 `sync.Map`）注册一个带缓冲的 result channel。
2. 通过 gRPC 双向流向 Agent 发送命令。
3. 在注册的 channel 上使用 `select` + `context.WithTimeout(30s)` 等待 Agent 回传结果。
4. Agent 回传结果后，`grpcserver.handleAgentMessage` 调用 `dirstore.Deliver` 将结果写入 channel（非阻塞 `select/default`，防止 goroutine 泄漏）。
5. HTTP 响应码：
   - `200 OK`：Agent 在 30s 内成功回传结果。
   - `502 Bad Gateway`：Agent 报告了错误。
   - `504 Gateway Timeout`：30s 内无响应（Agent 离线或过慢）。
   - `409 Conflict`：Agent 当前离线，命令无法发送。

### 适用场景

仅适用于**用户等待感知强、单次操作耗时预期 < 10s 的命令**，例如：
- 目录浏览（`list-dir`）：Agent 遍历一层目录的 I/O 耗时通常在毫秒级。

**不适用场景**（应改为异步 + 状态轮询/WebSocket）：
- 大目录全量递归扫描（可能耗时数分钟）。
- 批量文件操作（删除、重命名等）。
- Agent 重启/升级等影响 gRPC 流连通性的操作。

### 设计细节

| 组件 | 职责 |
|------|------|
| `controlplane/internal/dirstore.Store` | 注册/投递/取消 result channel；channel 缓冲为 1，防止 Deliver 阻塞 |
| `grpcserver.Server.dirResultStore` | `DirResultDeliverer` 接口，由 `dirstore.Store` 实现注入 |
| `handler.AgentsHandler.dirStore` | `DirListingStore` 接口，由 `dirstore.Store` 实现注入 |

### 替代方案（被否决）

- **纯 fire-and-forget（原 202 方案）**：前端必须轮询，且 Agent 回传结果后没有地方可以接收，结果丢失；实测即导致 `rawData.some is not a function` 崩溃。
- **WebSocket/SSE 推送**：实现复杂（需要连接管理、鉴权），对于仅需 1 次 request-response 的场景过于重量级。
- **超时设为 60s**：目录浏览操作不应让用户等待超过 30s；30s 已覆盖慢速网络下的正常操作，超时后给 504 比 hang 住更好。


---

## D-009：采集规则字段统一命名方案（T3-5）

**决策日期**：2026-05-11  
**影响范围**：DB migrations / proto / controlplane / agent / webui

### 背景

系统在采集规则相关字段命名上存在四层不一致：DB / proto / CP REST / WebUI 使用了四套不同的字段名，导致字段映射断路（规则创建 400）、`upload_bucket` 始终为空等 Bug。

### 决策

对 `collection_rules` 相关的四层字段名进行全面统一。

**完整字段统一映射**（"现状 → 目标"）：

| 概念 | **统一字段名** | DB 现状 | proto 现状 | REST-in 现状 | REST-out 现状 | Frontend 现状 |
|---|---|---|---|---|---|---|
| 监控根目录 | `base_path` | `source_path_template` | `source_path_template` | `source_path` | `source_path` | `source_path` |
| 文件/路径过滤 | `path_pattern` | `file_glob` | `file_glob` | `file_pattern` | `file_pattern` | `file_pattern` |
| 目标路径模板 | `dest_path_template` | `upload_path_template` | `upload_path_template` | `dest_path_template` ✓ | `dest_path_template` ✓ | `dest_path_template` ✓ |
| 目标 Bucket（DB FK） | `bucket_id` | `bucket_id` ✓ | — | `dest_bucket_id` | `dest_bucket_id` | `dest_bucket_id` |
| 目标 Bucket（proto，bucket 名称字符串） | `upload_bucket` | — | `upload_bucket` | — | — | — |
| 是否递归 | `recursive` | `watch_recursive` | `watch_recursive` | `watch_recursive` | `watch_recursive` | 缺失 |
| 子目录过滤 | **删除** | `watch_subdir_pattern` | `watch_subdir_pattern` | `watch_subdir_pattern` | 缺失 | 缺失 |
| 追加模式 | `append_mode` | `append_mode` ✓ | `append_mode` ✓ | `append_mode` ✓ | 缺失 | 缺失 |
| 采集模式 | `mode`（小写） | `mode` enum `('watch','scheduled')` | `mode` string | `mode` string | `mode` string | `mode`（`'WATCH'`/`'SCHEDULED'` 大写，需归一化） |
| 采集规则启用状态 | `enabled bool`（REST/proto 层） | `status rule_status('active','inactive')` **保留枚举** | `enabled bool` ✓ | — | `is_active` bool | `is_active` bool |

### 关键澄清

**`collection_rules.status` vs `agents.status`**：

- **`agents.status`**（5 个值：`pending / approved / online / offline / revoked`）：描述 Agent 生命周期状态。**不改动**。
- **`collection_rules.status`**（2 个值：`active / inactive`）：描述采集规则是否启用。DB 层**保持 `rule_status` 枚举类型不变**（便于后期扩展 `'paused'` 等状态）；应用层在 `toRuleResponse()` 和 `ruleToProto()` 中做枚举→bool 转换：`status='active'` → `enabled=true`；REST 输入 `enabled bool` 写入 DB 前转换回枚举值。

**`upload_bucket` vs `bucket_id`**：

- **proto `upload_bucket`**：存储 MinIO bucket 的**名称字符串**（不是 UUID），Agent 直接用它调用 S3 `PutObject`。Agent 无法通过 UUID 查询 bucket 名（它不访问 CP 数据库）。
- **DB/REST `bucket_id`**：存储 UUID，是 `buckets` 表外键。
- CP 的 `dispatch.go` 负责在下发规则时查出 bucket 名填入 proto `upload_bucket`。

### `append_mode` 值域统一

- DB 默认值保持 `'overwrite'`（不变）
- Agent 侧常量 `AppendModeNone = ""` 重命名为 `AppendModeOverwrite = "overwrite"`
- CP handler（`CreateRule`）当前错误地将空 `append_mode` 默认为 `"none"`，T3-5 修正为 `"overwrite"`
- 统一后三端值域：`'overwrite'` / `'tail'` / `'close_wait'`

### 备选方案（被否决）

- **`append_mode` DB 改为 `'none'`**：与现有数据不兼容，需要额外 migration；`'overwrite'` 语义已足够清晰。
- **逐层单独修复而不全局统一**：每次修复后测试范围难以界定，不如一次性对齐。

### 实施状态（2026-05-12 审计）

**已落实项：**

| 字段 | 层 | 证据 |
|------|---|------|
| `base_path` | DB / proto / CP REST in+out / WebUI | `migrations/000003_rename_rule_fields.up.sql`; `models.go:372`; `agents.go:521,458`; `agents.ts:98` |
| `path_pattern` | DB / proto / CP REST in+out / WebUI | 同上 |
| `dest_path_template` | DB / proto / CP REST in+out / WebUI | 同上 |
| `bucket_id` (DB FK) | DB / CP REST in+out / WebUI | `models.go:367`; `agents.go:518,450`; `agents.ts:103` |
| `upload_bucket` (proto) | proto / CP dispatch | `agent.proto:7`; `dispatch.go:170–184`（lookupBucketName → UploadBucket） |
| `recursive` | DB / proto / CP REST in+out / WebUI | `migrations/000003…`; `models.go:375`; `agents.go:524,459`; `agents.ts:104` |
| `append_mode` 默认值 `'overwrite'` | CP handler / Agent | `agents.go:563–566`; `watcher.go:45`（AppendModeOverwrite = "overwrite"） |
| `mode` 大小写归一化（输入） | CP handler | `agents.go:567`（strings.ToLower） |
| `enabled`/`status` 双向映射 | CP REST / proto / DB | `agents.go:470–471`（toRuleResponse）; `dispatch.go:182`（ruleToProto） |

**未落实项（已知缺口）：**

| 缺口 | 层 | 优先级 | 说明 |
|------|---|--------|------|
| `append_mode` 缺失于 REST 输出 | CP REST out | P1 | `collectionRuleResponse`（`agents.go:448–462`）未包含 `append_mode` 字段，前端列表/回显无法获取该值 |
| `mode` 大小写不一致（回显） | CP REST out / WebUI TS type | P2 | CP 返回 `"watch"/"scheduled"`（lowercase），但 WebUI `CollectionMode` 类型定义为 `'WATCH'/'SCHEDULED'`（uppercase）；编辑回显时类型不匹配 |
| WebUI 路径模板校验白名单过严 | WebUI `pathTemplate.ts` | P1 | `validatePathTemplate` 拒绝 `{agent_name}` 等合法动态字段，导致 2 个单元测试失败（T3-5-IMPL-J） |

**风险/待办项：**

- `append_mode` 回显缺失：需在 `collectionRuleResponse` 添加 `AppendMode string \`json:"append_mode"\`` 字段并在 `toRuleResponse` 赋值（T3-5-IMPL-J 应同时处理）
- `mode` 回显大小写：前端展示层已用大写 label，不影响功能；但 TypeScript 类型需将 `CollectionMode` 扩展为同时接受大小写，或在收到响应时做 `toUpperCase` 规范化

---

## D-010：引入 `pkg/trollsift` 共享路径模板库（T3-4）

**决策日期**：2026-05-11  
**影响范围**：新建 `pkg/trollsift/`，agent / controlplane 引用

### 决策

引入 `pkg/trollsift/` 作为独立 Go 模块（`github.com/byw-dev/fileagent/pkg/trollsift`），
加入 `go.work`，供 agent 和 controlplane 共同 import，以支持结构化路径模板的解析与组合。

**主要能力**：
- `Parse(s)`：从字符串提取格式字段值（支持字符串、整数、LDML 时间）
- `Compose(vals, allowPartial)`：将字段值格式化到模板串
- `Globify()`：将模板串转为 doublestar 兼容的 glob 字符串
- `IsTrollsiftPattern(s)`：判断是否含格式字段（含 `{` 即为 trollsift 模式）
- LDML 时间字段子集（`yyyy`、`MM`、`dd`、`HH`、`mm`、`ss`），时区 `|tz=IANA`
- `AgentContext` 注入（`{agent_name}`、`{agent_id}`）

**模块位置**：`pkg/trollsift/go.mod` module 名为 `github.com/byw-dev/fileagent/pkg/trollsift`，
`go.work` 追加 `use ./pkg/trollsift`。

### 备选方案（被否决）

- **放入 `agent/` 模块内**：controlplane 的 Dry-Run 端点在校验 path_pattern 时也需要 Globify/Validate，共享为独立包更合理。
- **使用现有开源 trollsift 库**：Python 的 trollsift 库无对等 Go 版本，且本项目需要 LDML 时间格式支持，需要自己实现。

---

## D-012：Token 刷新契约——refresh_token 走请求体 + 轮转（止血冲刺第 1 步）

**决策日期**：2026-07-03
**影响范围**：controlplane（`controlplane/internal/api/handler/auth.go` 的 `Refresh`）、webui（`webui/src/services/api.ts`、`webui/src/store/auth.ts`）、sdk/python（`sdk/python/fileagent/auth.py`）
**背景报告**：`docs/reports/design-gap-analysis/`（G-1，06 报告 E-2）

### 背景

`POST /api/auth/refresh` 存在两处契约漂移，会在 SDK 联调（T3-3）时爆发：

1. **令牌位置不一致**：Web UI（`api.ts:109`）与 Python SDK（`auth.py:130`）都把 refresh token
   放在 **JSON 请求体** `{"refresh_token": "..."}`；而 CP 旧实现只读 `Authorization: Bearer` 头，
   实测返回 `MISSING_TOKEN`。→ 客户端 access token 到期后自动刷新 100% 失败。
2. **无轮转**：CP 旧实现吊销了旧 refresh token，却**不在响应中签发新的** refresh token。
   Web UI/SDK 都期望响应包含新 `refresh_token` 并持久化。→ 首次刷新后 refresh token 失效，
   第二次刷新只能回退到用户名密码重新登录。

### 决策

**canonical 契约**：refresh token 通过 **JSON 请求体** 传递，响应执行**令牌轮转**。

- 请求：`POST /api/auth/refresh`，body `{"refresh_token": "<token>"}`
  （OAuth2 refresh-grant 惯例；与 Web UI / SDK 现有实现一致）。
  - 向后兼容：CP 同时接受 `Authorization: Bearer <refresh_token>` 头作为回退，
    body 为空时才读取头。
- 响应（200）：`{access_token, refresh_token, expires_in, token_type}`
  - **必须**返回新的 `refresh_token`（轮转），旧 token 被吊销后不可复用。
  - 生成新令牌对 **先于** 吊销旧令牌，生成失败则旧令牌仍可用。
- TTL：access 2h、refresh 7d（`controlplane/internal/api/handler/auth.go` 包级常量 `accessTokenTTL`/`refreshTokenTTL`，
  login 与 refresh 共用，避免漂移）。

### 契约回归锁

新增跨进程契约测试 `TestRefresh_Integration_BodyContractAndRotation`
（`auth_integration_test.go`，`-tags=integration`）：真起 HTTP server + 真实 Postgres，
用真实 http.Client 复现 SDK/Web UI 的 body 调用，断言 body 契约 + 连续轮转两轮可用。
**这是本项目第一个跨进程契约测试**，用于堵住"单测全 mock、契约漂移不可见"的结构性缺口
（详见 07 报告"回路 1"）。

### 备选方案（被否决）

- **改客户端去适配 header**：需同时改 Web UI + SDK 两处，且违背 OAuth2 惯例；CP 是唯一异类，改 CP 成本最低、面最小。
- **只吊销不轮转（保持无状态）**：Web UI/SDK 均已实现"存储响应中的新 refresh_token"，不轮转会让二者的持久化逻辑写入 undefined，链路更脆。

---

## D-013：Agent Token 生命周期——长效签发 + 重连自愈（止血冲刺第 2 步）

**决策日期**：2026-07-03
**影响范围**：controlplane（config、agent manager）、agent（grpcclient）
**背景报告**：`docs/reports/design-gap-analysis/`（G-2，06 报告 E-1）

### 背景

CP 用 2h 的 access token TTL 给 Agent 签发身份 token（`manager.go` 的 `PollApproval`/
`ApproveAgent`，注释却写着 "long-lived"）。但 Agent 持有长连接、重连时复用同一 token
（`grpcclient` 从不重新取 token），而 gRPC `Connect` 流会校验 token 过期。因此：
签发 2h 后任何一次重连（网络抖动 / CP 重启 / TCP 半开）→ Unauthenticated →
`runLoop` 用同一过期 token 无限重试 → **Agent 永久掉线，只能重启进程**。
凡部署超过 2 小时的 Agent，一次网络抖动就可能永久离线。

### 决策：双重防御

**防御 A（治本，CP 侧）**：给 Agent 签发真正长效的 token。
- 新增配置 `AGENT_TOKEN_TTL`（默认 720h / 30 天），对齐设计 §4.7 与附录 C.1；校验为正值。
- `Manager.accessTTL` 重命名为 `agentTokenTTL`，由 `cfg.AgentTokenTTL` 注入；
  Agent token 不再复用 2h 的 `JWTAccessTokenTTL`。
- 用户/SDK 的 access token 仍是 2h（不受影响）——两类 token 生命周期本就应独立。

**防御 B（自愈网，Agent 侧）**：token 被拒时自动重新获取。
- `grpcclient.Client` 新增可选 `reauthFunc`；`runLoop` 捕获流的终止错误，
  当 `status.Code == Unauthenticated` 时调用它刷新 token 再重连。
- `reauthFunc` 经 `ReAuthenticate()` 调用**豁免 JWT** 的 `PollApproval`
  （已审批 Agent 会立即拿到新 token），成功后持久化并安装；非审批态（如已吊销）
  返回错误、不安装空 token → 已吊销 Agent 不会自愈，无安全回退。
- 严格按 `Unauthenticated` 门控：`Unavailable` 等瞬时错误不触发重新认证（有单测守卫）。

两层独立：A 让 30 天内基本不触发 B；B 保证即便 token 最终过期/被吊销后重新审批，
Agent 也能自愈，使 token 时长不再是单点故障。

### 备选方案（被否决）

- **仅防御 A（超长 / 永不过期 token）**：单靠拉长 TTL 只是把炸弹从 2h 推迟到 30 天，
  到期仍会掉线；且超长 token 削弱吊销时效性。必须配合 B。
- **新增 token 续期 RPC**（设计 §4.7 的原始设想）：proto 无对应 RPC，需改契约；
  而 `PollApproval` 已是幂等的发 token 通道，复用它成本最低、面最小。

---

## D-014：MinIO 事件 webhook 端点鉴权——共享密钥 + 失败即拒（止血冲刺第 3 步）

**决策日期**：2026-07-03
**影响范围**：controlplane（`internal/api/handler/events.go`、`router.go`、`config`）
**背景报告**：`docs/reports/design-gap-analysis/`（G-3，01 报告 §1）

### 背景

`POST /internal/minio-event`（MinIO 上传事件回调）会把外部输入写入 `file_entries`
索引，但**从未校验任何凭据**——路由注释甚至写着"secured by shared secret"，实际却没实现。
任何能访问该端口的人都可伪造上传事件污染文件索引。设计 §6.1.2 / §6.5 本就要求
MinIO 侧配置 `notify_webhook auth_token`，CP 侧却没消费它。

### 决策

以 **MinIO `notify_webhook` 的 `auth_token`（共享密钥）** 鉴权，**失败即拒（fail-closed）**：

- 密钥来源：配置项 `INTERNAL_WEBHOOK_SECRET`（已存在，此前未被消费）。
- 校验：读 `Authorization` 头，兼容 MinIO 不同版本——接受 `Bearer <secret>` 和裸 `<secret>`
  两种形式；用 `crypto/subtle.ConstantTimeCompare` 常量时间比较，避免时序侧信道。
- **未配置密钥时端点拒绝一切请求**（fail-closed），而非放行：一个会改数据库的外部端点
  必须可鉴权，无密钥即无法鉴权，故关闭。构造时打印一次启动告警，提示运维配置密钥。
- 失败请求返回 401 并记 warn 日志（含来源 IP），便于发现伪造/误配。

### 备选方案（被否决）

- **无密钥时放行 + 告警（fail-open）**：日志只是记录漏洞，并未修复它；G-3 的目的就是堵洞，
  必须拒绝未鉴权请求。
- **把 `INTERNAL_WEBHOOK_SECRET` 设为必填、缺失则 CP 启动失败**：影响面过大（每个部署都必须配），
  改为运行时 fail-closed 把影响局限在 webhook 这一个端点，CP 仍能正常启动其余功能。
- **改用 mTLS / IP 白名单**：第一版不引入 mTLS（见 CLAUDE.md 边界）；IP 白名单在容器网络下脆弱。
  共享密钥与设计 §6.5 一致，最简单可靠。

---

## D-015：Agent 心跳遥测落地——填充载荷 + Redis 快照 + API 暴露（P1 G-4）

**决策日期**：2026-07-04
**影响范围**：agent（queue/grpcclient/main）、controlplane（grpcserver/cache/agents handler）
**背景报告**：`docs/reports/design-gap-analysis/`（G-4，02 报告 §2）

### 背景

Agent 心跳一直发送**空 `Heartbeat{}`**（`client.go`、Ping 响应两处），proto 定义的
`queue_depth`/`uptime_seconds`/`version`/`disks`/`upload_bps` 全部为零。CP 侧 `handleHeartbeat`
也只刷新在线 TTL + last_seen，丢弃其余字段。后果：Web UI 无队列深度/版本可展示、
监控 §9.2 的 Agent 指标无数据源、`AgentQueueBacklog` 告警永不触发（典型"空心功能"）。

### 决策

打通"Agent 填充 → CP 落地 → API 暴露"链路，先做**廉价高价值**字段：

- **Agent 端**：心跳填充 `agent_id`、`uptime_seconds`（进程启动至今）、`queue_depth`
  （`queue.CountPending()`）、`version`。经 `SetHeartbeatFunc` 注入构建器，周期心跳与
  Ping 响应共用（`BuildHeartbeat()`）。
- **CP 端**：`handleHeartbeat` 把遥测快照 JSON 写入 Redis `agent:{id}:stats`，
  **共用在线 TTL（90s）**——离线即消失，语义与 `is_online` 一致。
- **API 端**：`agents` 响应经 `toAgentResponseWithOnline` 从 Redis 富化
  `queue_depth`/`uptime_seconds`（指针字段，无快照时 omitempty 省略，不显示误导性 0）。

### 暂缓（本次不做，明确记录避免再次伪装完成）

- **`disks`（磁盘剩余）**：需跨平台磁盘枚举（设计 §4.1 的 `sysinfo/` 模块仍缺），
  是新依赖/平台代码，单列后续。
- **`upload_bps`（上传速率）**：需在 uploader 加吞吐计量，后续。
- **Prometheus 指标导出**：整体推迟（backlog T4-1），本次只打通到 REST API。
- **Web UI 渲染**：API 已暴露字段，前端展示为快速跟进项。

### 备选方案（被否决）

- **遥测写入 `agents` 表**：每 30s 一次 DB 写入、churn 高；且这是易失的实时状态，
  Redis + TTL 更贴合（与在线状态同构）。
- **仅填充 Agent 端不落地 CP**：数据到不了 UI/监控，等于没修（"空心"陷阱）。

---

## D-016：仪表盘统计端点——服务端聚合替代前端抽样估算（P1 G-5）

**决策日期**：2026-07-04
**影响范围**：controlplane（db read_queries、stats handler、router）、webui（Dashboard）、design §5.11.5/§7.3.1
**背景报告**：`docs/reports/design-gap-analysis/`（G-5，03 报告 §3）

### 背景

Dashboard 的"今日上传/存储用量/7 日趋势"一直是**前端拿最近 20 条上传日志硬凑**的假值
（`listUploadLogs({limit:20})` 再本地 reduce/filter）——今日上传上限 20、存储用量与真实
无关、趋势无意义。根因：**设计 §7.3.1 只画了 UI 卡片，§5.11 REST 清单从未定义支撑它的
统计端点**，实现者只能硬凑。这是"设计只画 UI、没定义 API"的典型返工来源。

### 决策

新增 `GET /api/v1/stats/dashboard`，由**服务端一次聚合**返回真实值；设计补齐端点定义（§5.11.5）。

- `total_agents`/`online_agents`/`total_files`/`storage_bytes`/`today_uploads`/`upload_trend`（7 天稠密）。
- 口径：`online_agents` 用 `last_seen_at` 在 90s 内（与在线阈值一致）；`storage_bytes` 用
  `SUM(file_entries.size_bytes)`（CP 索引口径，非 MinIO 物理用量，避免每次刷新调 madmin）；
  `today_uploads`/`upload_trend` 按 `uploaded_at` UTC 统计，趋势在 Go 侧补齐 7 天骨架。
- 实现走 `internal/db/read_queries.go` 手写聚合查询（sqlmock 测试，遵循 D-005），不引入 sqlc 变更。

### 流程约定（防复发）

采纳 07 报告建议：**新功能的"设计完成"判据 = 每个 UI 稿都对应到已定义的 REST 端点**，
否则视为设计未完成。本次即按此补齐 §5.11.5 与 §7.3.1 的数据来源标注。

### 备选方案（被否决）

- **保留前端估算**：数字错误，等于没修（"空心"陷阱）。
- **存储用量用 `madmin.BucketUsageInfo`（§6.6）**：更贴近物理用量，但每次刷新一次 MinIO Admin 调用、
  且与"已索引文件"口径不同；SUM(size_bytes) 更便宜、语义清晰，够用。物理用量可后续单列。
- **用 sqlc 生成聚合查询**：可选过滤/日期分组用 sqlc 表达不便，手写查询 + sqlmock 更直接（与既有 read_queries 一致）。

---

## D-017：MinIO 文件事件补完 —— file_deleted 发布 + 软删除 + webhook 路径复活（CC-1）

**决策日期**：2026-07-04
**影响范围**：controlplane（indexer、events handler）、webui（Events/Create）
**背景**：`docs/tasks/core-completeness.md` CC-1；报告 01 §4

### 背景

`events.file.deleted` 只有订阅方、无发布方，事件规则 UI 可配 `file_deleted` 却永不触发。
排查中另发现两个更深的问题：
1. `MinioEventHandler.Handle` 对所有事件一律 `IndexUpload`，**不区分 ObjectCreated / ObjectRemoved**——删除被当成上传。
2. webhook 索引路径 `IndexUpload` / 新增的 `IndexDeletion` 用 `uuid.Nil` 作为 orgID 查 bucket 与写 file_entries，
   而 bucket/entry 属于默认组织 `…0001`——**该路径其实从未成功过**（bucket 查不到、org_id 还会违反 FK）。
3. webui 事件类型下拉 `EVENT_TYPE_OPTIONS` 用了错误值（`file.uploaded` 点号形式、`file.indexed`/`agent.registered`
   等不存在的类型），且**根本没有 `file_deleted` 选项**。

### 决策

- **按事件类型路由**：`s3:ObjectCreated:*` → 索引上传；`s3:ObjectRemoved:*` → **软删除**（`status='deleted'`，
  保留行以供审计，非物理删行）并发布 `events.file.deleted`。未匹配类型忽略。
- **发布 `events.file.deleted`**，payload：`{file_entry_id, bucket_id, storage_path, file_name}`（契约，事件消费方依赖）。
- **删除不存在/已删对象为幂等 no-op**（`MarkFileEntryDeleted` 返回 `sql.ErrNoRows` 时不报错、不发事件）。
- **orgID 用默认组织**（`…0001`）而非 `uuid.Nil`，修复 webhook 索引路径——此路径此前对 IndexUpload 也是坏的。
- **webui 事件类型对齐 DB enum**（下划线 6 值，含 `file_deleted`）；action 下拉暂只留 `webhook`（`email` 无效、
  `kafka_publish`/`nats_publish` 见 CC-7）。

### 备选方案（被否决）

- **硬删除 file_entries 行**：丢失审计与历史关联（upload_logs 外键指向 file_entry）；软删除更安全，且设计 §3.3 已有 `deleted` 状态。
- **从 UI 移除 file_deleted 选项**（另一条 CC-1 路线）：设计 §6.5 本就要求接入 ObjectRemoved，且 DB/事件引擎均已预留，接通比阉割更符合设计意图。

---

## D-018：API 限流 —— 按用户固定窗口 + Redis 计数（CC-4）

**决策日期**：2026-07-05
**影响范围**：controlplane（`cache`、`api/middleware`、`api/router`、`config`）
**背景**：`docs/tasks/core-completeness.md` CC-4；报告 01 §1；设计 §5.1 + §3.5 Redis Key 表已要求 `ratelimit:api:{user_id}`

### 背景

设计 §5.1 与 §3.5 Redis Key 表都要求对 API 做限流（`ratelimit:api:{user_id}`，TTL 1 分钟），
但 `api/middleware/` 只有 `error.go` / `jwt.go`，**无任何限流实现**——键模式在 `cache/keys.go`
里有 `RateLimitKey` 但从未被调用。

### 决策

- **固定窗口（fixed window）**，非滑动窗口/令牌桶：Redis `INCR` 计数键 `ratelimit:api:{user_id}`，
  首次自增时 `EXPIRE` 60s。INCR+EXPIRE 用**单条 Lua 脚本**保证原子——防止进程在 INCR 与 EXPIRE
  之间崩溃留下无 TTL 的键把用户永久挡死。实现为 `cache.Client.IncrWithWindow`。
- **按用户计数**：中间件在 JWT 之后执行，按 JWT `sub`（user_id）计数，仅作用于 `/api/v1/*`。
- **上限可配**：`API_RATE_LIMIT_PER_MINUTE`，默认 600（10 req/s/用户）；`<= 0` 关闭（中间件不挂载）。
- **失败开放（fail open）**：Redis 出错时放行并记 warn，与 JWT 黑名单查询同策略——限流是保护而非
  强一致门禁，Redis 抖动不应把所有用户挡在门外。无 JWT 声明的请求不计数（同样放行）。
- **响应头**：每个响应带 `X-RateLimit-Limit` / `X-RateLimit-Remaining`；超限 `429 RATE_LIMITED`
  + `Retry-After: 60`，错误体走统一信封（含顶层 `request_id`，见 D 之外的 CC-5）。

### 备选方案（被否决）

- **滑动窗口 / 令牌桶**：更平滑但需 sorted-set 或多键/脚本状态；设计明确写的是单 STRING 计数 + 1 分钟 TTL，
  固定窗口与之一致且实现最简。窗口边界的 2× 突发对私有部署的管理台可接受。
- **失败关闭（Redis 错即 429）**：一次 Redis 抖动即全站不可用，代价远大于限流被短暂绕过。
- **对 `/api/auth/login` 也限流**：登录无 user_id，需改为按 IP 计数，属独立的暴力破解防护议题；
  本次按设计 §3.5（键为 user_id）只做认证后接口，登录防护另行处理。

---

## D-019：事件动作 —— 实现 nats_publish + 拒绝 kafka_publish（CC-7）

**决策日期**：2026-07-05
**影响范围**：controlplane（`event` 引擎、`api/handler/events`）、webui（Events/Create）
**背景**：`docs/tasks/core-completeness.md` CC-7；报告 01 §4

### 背景

DB `action_type` enum = `webhook / nats_publish / kafka_publish`。核查代码发现：
- `webhook` 真正可用（HTTP POST + 重试 + delivery 记录）。
- `nats_publish` **是空心的**：dispatch 只写一条 `status='delivered'` 的 delivery，**从不真正发布**到任何
  NATS 主题——`Engine` 结构体压根没有 publisher 依赖，也没有 `NatsActionConfig`（无 subject）。
- `kafka_publish` 未实现（default 分支只 warn）。

原 CC-7 计划以为"nats_publish 已可用，放回 UI 即可"，实为把另一个空心功能重新暴露。经产品决策：**真正实现
nats_publish**，`kafka_publish` 拒绝（系统固定基础设施是 NATS，无 Kafka）。

### 决策

- **实现 nats_publish**：新增 `NATSActionConfig{subject}`；`Engine` 增加 `NATSConn` publisher 依赖
  （`WithPublisher` 注入，复用 main.go 既有 `natsPublisher`）；`dispatchNATS` 建 pending delivery →
  发布 payload 到 subject → 成功记 `delivered`+`delivered_at`、失败记 `failed`+`next_retry_at`。
- **重试打通**：`retryDelivery` 从"仅 webhook"改为按 `action_type` 分派，nats_publish 失败也走同一套指数退避重试。
- **无 publisher 兜底**：未注入 publisher 时 nats_publish 记 `failed`（进重试），而非静默成功——避免再造空心。
- **API 校验**：Create/Update 事件规则拒绝非 `webhook`/`nats_publish` 的 `action_type`（`400 INVALID_ACTION_TYPE`），
  并校验 `action_config` 必填字段（webhook `url` / nats_publish `subject`，缺失 `400 INVALID_ACTION_CONFIG`）。
- **webui**：动作下拉恢复 `nats_publish`（含 subject 配置示例），不提供 `kafka_publish`，移除 `TODO(CC-7)`。
- **kafka_publish 保留在 DB enum**：Postgres 删除 enum 值代价高且迁移只追加；保留值无害（API 已拒绝），
  待真有 Kafka 需求再实现。

### 备选方案（被否决）

- **也把 nats_publish 当不支持、走 webhook-only**：更省事但放弃了设计 §5.9 既有的 NATS 事件总线复用能力；
  既然基础设施就是 NATS，做实 nats_publish 成本低、价值真实（人工决策 2026-07-05 选 A）。
- **迁移删除 kafka_publish enum 值**：Postgres enum 删值需重建类型 + 转换列，风险高、收益低；API 层拒绝即可。

---

## D-020：采集规则原地编辑 —— PUT 双形态（状态切换 + 全字段编辑）（CC-9 后端）

**决策日期**：2026-07-05
**影响范围**：controlplane（`db/queries/rules.sql` + sqlc 生成、`api/handler/agents` UpdateRule）
**背景**：`docs/tasks/backlog.md` T4-6 / core-completeness CC-9

### 背景

`PUT /api/v1/agents/:id/rules/:rid` 原本只支持 `{status}` 启用/停用，规则内容改不了、须删除重建
（丢失规则 ID 与历史上传日志关联）。webui 的 enable/disable 开关已依赖这个 status-only 契约。

### 决策

- **同一 PUT 端点承载两种形态**，用请求体是否**含 `name` 键**区分（`name` 为 `*string`，显式空串仍走全量
  路径按缺字段 `422`，避免"含 name 但为空"静默回退到状态切换）：无 `name` → 原 status-only 路径
  （`UpdateCollectionRuleStatus`，向后兼容开关）；含 `name` → 全字段更新（新增 sqlc `UpdateCollectionRule`）。
  不新增端点，保持 REST 路径契约不变。
- **全量更新按 `id + agent_id + org_id` 三键定位**（防 IDOR：仅凭猜到的 rule UUID 改他 agent/他 org 的规则；
  不匹配返回 `404` 不泄露存在性）。handler 解析路径 agent id + claims org id 传入。全字段更新影响面
  （bucket/路径）比原 status-only 大，值得收紧。（注：现存 status-only 与 delete 仍是 id-only，属既有面，
  本 PR 未一并改，留作后续。）
- **全字段校验**：`name`/`bucket_id`/`mode`/`base_path`/`path_pattern`/`dest_path_template` 必填，
  缺失 `422 VALIDATION_ERROR`；`mode` 限 `watch`/`scheduled`，否则 `422`；`bucket_id` 非法 `400`。
  status 由 `enabled` 派生（与创建对称），默认 active。
- **重新下发**：结果 active → `DispatchRule`（Agent 端 `PushRuleCommand` 内部 stopRule+重启 watcher 热重载，
  编辑无需先 disable）；inactive → `DispatchRuleCancel`。派发失败只 warn 不阻断（DB 已写成功，Agent 重连
  经 `SyncRulesOnConnect` 兜底）。**离线允许编辑**（backlog 设计约束）。

### 备选方案（被否决）

- **新增独立 PUT/PATCH 端点做全字段更新**：多一个契约面；用 `name` 存在性分流即可复用同一端点。
- **PATCH 部分字段合并**：语义更复杂（需读改写、区分"未提供"与"置空"）；规则字段少且 webui 编辑始终提交全量，
  全量 PUT 更简单可预测。
- **强制先 disable 再编辑**：Agent 已内置热重载，强制 disable 只增操作步骤无收益（backlog 已论证）。

---

## D-021：Agent 重命名 —— PATCH 端点 + 路径安全的名称校验（CC-8）

**决策日期**：2026-07-05
**影响范围**：controlplane（`agents.sql` + sqlc、`api/handler/agents` Rename、router）、webui（Agent 详情）
**背景**：`docs/tasks/backlog.md` T4-5 / core-completeness CC-8

### 决策

- **`PATCH /api/v1/agents/:id`**，body `{"name": "..."}`，`super_admin`（复用 `RequireRole` 中间件）。
  新增 sqlc `UpdateAgentName`。用 PATCH（部分更新单字段）而非 PUT，语义贴合"只改显示名"。
- **名称校验偏严，因 `name` 会注入上传路径模板 `{agent_name}`**：`TrimSpace` 后非空、≤ 64 rune、
  白名单正则 `^[\p{L}\p{N} ._-]+$`（字母任意语种含中文 / 数字 / 空格 / `. _ -`）。排除 `/`、控制字符等，
  防止路径注入/对象键损坏。违规 `422 VALIDATION_ERROR`。存储 trim 后的值。
- **生效时机**：新名在 Agent 重连时才进入路径模板（方案 B 约束）；旧名期间已上传对象路径为写入快照，不追溯。

### 备选方案（被否决）

- **PUT 整个 agent 对象**：agent 多数字段由 Agent 自身上报（hostname/os/version），管理员只应改显示名；
  PATCH 单字段避免误覆盖上报字段。
- **宽松校验（仅非空 + 长度）**：`name` 进路径模板，宽松校验会把 `/`、`..`、控制字符带进对象键，
  有路径注入风险；白名单更安全，且中文显示名仍可用（`\p{L}` 覆盖 CJK）。

---

## D-022：Web UI 嵌入 CP 二进制 —— build tag 双模式 + `make bundle`

**决策日期**：2026-07-05
**影响范围**：controlplane（新增 `internal/webui` 嵌入包、`api/router` SPA 服务、`cmd/server` 注入）、
Makefile（`bundle` 目标）、构建/分发流程
**背景**：简化分发与维护——运维只需一个二进制 + 配置即可跑起完整系统（含 Web 管理界面）

### 决策

- **可选嵌入**：Control Plane 可将 `webui/dist` 经 `//go:embed` 嵌入自身二进制，同源提供 SPA。
  API 全在 `/api/*`、`/internal/*`、`/healthz`；其余路径服务前端静态文件，未命中的客户端路由
  （BrowserRouter）回退 `index.html`。同源 → 无需 CORS；webui 的 API base 已是相对路径 `/`。
- **build tag 双模式**：`internal/webui` 用 `//go:build webui` / `!webui` 两文件门控。
  默认 `go build`（无 tag）→ `FS()` 返回 nil → 纯 API 二进制，**不需要 dist、`go build ./...` 与
  CI/单测恒可编译**；`-tags webui` → 嵌入 dist → 含前端二进制。router 不 import 该包，
  经 `RouterConfig.WebUIFS fs.FS` 由 `main` 注入，保持 router 纯净可测（单测用 `fstest.MapFS`）。
- **构建流程**：新增 `make bundle`（`build-webui` 编译并拷 dist 进 CP 嵌入目录 → `build-controlplane-bundle`
  以 `-tags webui` 编译）。**保留 `make build` 为纯 Go**，后端开发者与 CI 不需要 Node。
  嵌入目录 `controlplane/internal/webui/dist/` 为构建产物，gitignored。
- **SPA 404 语义**：未命中的 API 形状路径（`/api/`、`/internal/`、`/healthz`）返回统一错误信封
  `NOT_FOUND`（复用 `middleware.RespondError`），**不喂 index.html**——错拼的 API 调用要显式失败。

### 备选方案（被否决）

- **`make build` 总是打包前端**：分发最省心，但每次后端构建都要 Node 24 + pnpm + vite build（慢），
  且 CI 纯 Go 流水线需相应改造。独立 `make bundle` 兼顾两者。
- **不用 build tag、始终 embed**：须提交占位 `index.html`（dist 被 gitignore），否则 `go build` 失败；
  build tag 更干净，默认路径零负担。
- **前端独立部署 + 反向代理**：仍是有效的生产形态（§10.4 Caddy），但单二进制对小规模/离线分发更省事，
  二者并存不冲突。
- **第三方 gin 静态中间件**：标准库 `embed` + `http.FS` + `c.FileFromFS` 已足够，不引额外依赖。

---

## D-023：迁移嵌入二进制 + 参考生产部署形态（落成 §10）

**决策日期**：2026-07-06
**影响范围**：controlplane（`internal/db/migrate.go`、`cmd/server`、`internal/config` 去 `MIGRATIONS_PATH`、
新增 `migrations/embed.go`）、部署脚手架（`controlplane/Dockerfile`、`deploy/docker-compose.prod.yml`、
`deploy/caddy/Caddyfile`、`deploy/systemd/*`、`deploy/windows/*`）、运维文档（`docs/ops/`）
**背景**：D-022 让 `make bundle` 把 Web UI 嵌入二进制，但 CP 启动时仍从**文件系统**读取迁移
（`file://` + `MIGRATIONS_PATH`），"单二进制"实际仍需随二进制分发 `migrations/` 目录。T4-3 收尾单文件分发
并把 §10 的部署蓝图落成可运行产物。

### 决策

- **迁移嵌入二进制**：新增 `controlplane/migrations/embed.go`（`//go:embed *.sql`），`db.Migrate` 改用
  golang-migrate 的 `source/iofs` + `NewWithSourceInstance`，签名从 `(dsn, path, logger)` 改为
  `(dsn, fs.FS, logger)`，由 `main` 传入嵌入的 `migrations.FS`。**删除 `MIGRATIONS_PATH` 配置**——
  嵌入后不再需要外部路径，保留 escape-hatch 只会稀释"自包含"的意义。启动时自动幂等应用，日志不变。
  `migrations/` 位置不变（仍是 §"契约文件"里的迁移目录，只追加不改），仅新增一个 embed 声明文件。
- **参考生产形态（容器 all-in-one）**：`controlplane/Dockerfile` 多阶段（node 构建 webui → go `-tags webui`
  静态编译 → alpine 运行镜像；**镜像不含 migrations/**，证明自包含）；`deploy/docker-compose.prod.yml`
  起全栈（PG/Redis/NATS-JS/MinIO + CP + Caddy）；`deploy/caddy/Caddyfile` 终结 TLS，`/`（含 API+SPA）
  反代 CP:8080，agent gRPC 走独立 TLS 监听反代 `h2c://CP:9090`。
- **参考生产形态（主机 systemd）**：`deploy/systemd/controlplane.service` +
  `fileagent-agent.service` 落成 §10.3（`StateDirectory` 承载 bootstrap 凭据/agent 队列；因迁移已嵌入，
  无需 `WorkingDirectory` 指向 migrations）。
- **Windows agent**：`deploy/windows/install-agent.ps1`（NSSM 封装 `agent.exe`，捕获 stdout 到日志文件）+
  `agent/config.windows.toml.example`。Makefile 新增 best-effort `build-agent-windows`
  （CGO/mingw；权威产物仍由 CI `build-agent.yml` 出）。

### 备选方案（被否决）

- **保留 `MIGRATIONS_PATH` 作为 override**：与"自包含单文件"目标相悖，且多一条运行时分支；
  紧急改迁移应走正常发版（迁移只追加）。
- **迁移目录移入 `internal/db`**：`migrations/` 是既有契约路径（CLAUDE.md 列为迁移目录），迁移会破坏引用；
  在原地加一个 `embed.go` 更省事、零迁移风险。
- **distroless 运行镜像**：更小更安全，但无 shell 难以在 all-in-one PoC 里排障 / 读取 bootstrap 凭据；
  alpine 运行镜像（+ 专用非 root 用户 + `/data` 卷）更适合运维上手。生产可自行换 distroless。
- **compose 内自动跑 init-minio**：`init-minio.sh` 依赖 bash + mc + 服务重启时序，塞进一次性容器较脆；
  改为文档化的一次性手动步骤（建桶/webhook 只需跑一次）。

---

## D-024：MinIO internal/public endpoint 拆分（消除 CP↔MinIO hairpin）

**决策日期**：2026-07-07
**影响范围**：controlplane（`internal/config/config.go`、`internal/storage/sts.go`、`cmd/server/main.go`）、
部署（`deploy/docker-compose.prod.yml`、`controlplane/.env.example`）、运维文档（`docs/ops/`、`system-design.md §10`）
**来源**：PR #61（T4-3）review（discussion r3529902760）+ `docs/tasks/backlog.md` follow-up。

**背景**：单一 `MINIO_ENDPOINT` 同时承担两种角色——(a) **internal**：CP 自身调用 MinIO
（STS `AssumeRole`、建桶 admin）；(b) **public**：写入 STS `CredentialsPayload.Endpoint` 交给 agent、
并作为 presign 下载 URL 的签名 host。all-in-one PoC 只能填一个"对内外都可达"的地址，导致 CP↔MinIO
绕宿主 hairpin。

### 决策

- **配置拆分**：新增 `MINIO_PUBLIC_ENDPOINT` / `MINIO_PUBLIC_USE_SSL`，**缺省回落 = 内网
  `MINIO_ENDPOINT` / `MINIO_USE_SSL`**（单端点部署零改动，向后兼容）。
- **STS**：`STSManager` 加 `publicEndpoint`/`publicUseSSL`，`NewSTSManager` 默认置为内网值；
  新增流式 `WithPublicEndpoint(endpoint, useSSL)`（现有 5 处调用点签名不变）。`AssumeRole` 仍走
  internal，返回给 agent 的 payload 用 public。
- **presign**：`main.go` 另建一个 public endpoint 的 MinIO client 供 presigner（本地签名、不发网络，
  只修正 URL 的签名 host）；内网 client 留给建桶 admin。
- **端点角色**（gateway 无关）：internal = CP↔MinIO（AssumeRole + 建桶）；public = STS payload endpoint +
  presign host，须对浏览器/agent 可达。prod compose 固定 `MINIO_ENDPOINT=minio:9000`（内网）+
  必填 `MINIO_PUBLIC_ENDPOINT`（宿主 LAN IP / 网关地址）。

### 备选方案（被否决）

- **一次性上生产网关反代（Caddy `minio.<domain>` 子域 + TLS）**：生产网关未必是 Caddy；MinIO 经网关
  暴露是 gateway 特定改动，与本次"消除 hairpin 的 Go/config 拆分"解耦。本 PR 只做 gateway 无关的端点拆分，
  网关反代留作后续（文档已 gateway 无关地写）。
- **presigner 复用内网 client**：presign 是本地签名，host 必须等于客户端实际访问地址，复用内网 host 会签出
  不可达 URL。故必须用 public endpoint 单独构造 presign client。

---

## D-025：文件元数据采用混合模型 6c（受控标签 + 数据集，分期）

**决策日期**：2026-07-07
**影响范围**：controlplane（`internal/indexer` 打标、`internal/api` 词表/待确认/文件筛选、`internal/db` 迁移+查询、
`classifier.go` 优先级）、webui（round 7 四屏）、SDK（消费方，当前推后）、文档（`system-design.md §3/§5`、
`docs/design/metadata-model.md`、`contracts.md`）
**来源**：Claude Design「前端页面重做计划」round 6–7 的信息架构探索；产品拍板选定 6c。
**权威设计**：[`docs/design/metadata-model.md`](docs/design/metadata-model.md)（Phase 1 工程设计 + Phase 2 留存）。

**背景**：采集数据多维度变种（厂家/型号/站点/传感器/版本 + 加工级别），仅靠 `file_types`（glob）会「类型
爆炸」。需在**采集规则源头**声明元数据，SDK 按维度消费，并要求对账与历史回溯打标。

### 决策

- **采用混合模型 `6c`**（否决 `6a` 纯文件标签 / `6b` 纯数据集），**分期**：
  - **Phase 1（现在做）· 受控标签打底**：`tag_keys` 词表 + `tag_values` 受控取值 + `file_tags` + 待确认队列
    `pending_tag_values` + `tag_audit`；规则在 `collection_rules.metadata JSONB` 声明 `file_type + static_tags +
    path_tag_map`（**不改 rules 表**）。
  - **Phase 2（暂缓）· 数据集注册表**：命名的标签组合谓词，薄层不动文件表，承载对账/血缘/SDK 订阅名。设计已在
    `metadata-model.md` 留存，待明确需求再起。
- **打标在 CP 侧**：复用现有服务端分类路径（`classifier.go`），在索引处理 `UploadResult` 时打标。**Phase 1 不改
  `proto/v1/agent.proto`、不改 agent**。
- **治理**：key 严格受控、value 受控可扩；路径变量提取的未知值进待确认队列由管理员核准（防
  `tokyo/Tokyo/TYO` 漂移），文件仍入库携带原始值但未核准值不进筛选器/规则可选项。
- **`file_types` 降级为兜底**：规则声明类型优先，glob 只兜没声明的旧数据（向后兼容）。
- 路径变量映射**复用 trollsift 模板变量**（`contracts.md` V-3），不另造机制；文件筛选新增可重复 `tag` 查询参数，
  **保持 cursor 分页**（V-2）不改 offset。

### 备选方案（被否决）

- **`6a` 纯文件标签**：无对账/血缘载体（SDK 需求硬伤）。
- **`6b` 纯数据集**：长尾/临时数据成本高（逼着先建模后采集），后端 + UI 改动大。
- **规则声明标签下发到 agent（改 proto）**：Phase 1 打标在 CP 侧即可（路径变量在服务端可从 storage path 抽取），
  下发标签到 agent 属过度设计；留待 Phase 2 若确有 agent 侧需求再评估。
- **文件按标签筛选改 offset 分页**：违反 cursor 分页契约；用可重复 `tag` 参数 + `file_tags` join 即可。

### 补充（2026-07-10，产品讨论后修订设计，未动已实现代码）

1. **Phase 1 补手动/批量打标 API**（初稿遗漏）：`file_tags.source` 定义了 `manual`/`api` 且建了 `tag_audit`，
   但 P1.3 没有写入端点。补 `PUT /api/v1/files/{id}/tags` + `POST /api/v1/files/batch-tag`（按 files 同款
   筛选谓词圈选）。**直接动因**：将有一批历史数据文件入库需人工补标。
2. **Phase 2 血缘从「数据集级」改为「run 模型」**：原 `dataset_lineage`（数据集←数据集）表达不了文件级来源，
   逐条维护文件↔文件边又繁琐（1:1/1:n/n:1 混存）。改为记录**一次加工运行**（`lineage_runs`/`run_inputs`，
   参考 OpenLineage）：SDK 拉取时自动记输入、注册输出时挂 run，文件级血缘可推导、ETL 零额外申报。
3. **明确衍生数据入口**（原设计未回答）：ETL 是未来 SDK 消费方，**禁止直连 MinIO 读写**；写入镜像 agent
   数据面（CP 发 STS → 直传 → 注册携带 tags/level/run_id，source=`api`，受同一词表治理）。minio-event
   只做对账兜底。ETL 在系统第一阶段建设完成前不存在，故仅落设计（`metadata-model.md` P2.2/P2.3）。
4. **实施排期**：Phase 1 提为当前 track（MT-1…MT-6，追踪 `docs/tasks/metadata-phase1.md`），WR-2…10 暂停
   让位（价值优先）。MT-1+MT-2 薄纵切起手。

### 落地记录（MT-1 + MT-2，薄纵切第一刀）

迁移 `000004_metadata_tags`（5 表，只追加）+ indexer 打标 `static_tags`/声明 `file_type` 优先 + 文件按标签筛选。
固化以下**契约细节**（D-025 大盘之下的具体形状）：

- **文件响应新增 `tags`**：形如 `"tags": {"vendor":"omron","site":"tokyo"}`（key→value 对象，`omitempty`；
  与 `file_tags` 主键 `(file_entry_id,key)`「每 key 至多一值」一致）。`GET /api/v1/files` 与 `GET /files/{id}` 均带。
- **单文件读取按 org 收窄**：`GetFileEntryByID` 仅按 id 查，故 `GET /files/{id}`、`/download-url`、
  `batch-download-urls` 在 handler 层校验 `entry.org_id == 调用者 org`，跨 org 一律 **404**（不泄露存在性）。
  纵深防御（v1 单组织尚不可利用，但与规则查询的 org 收窄一致）。
- **`GET /api/v1/files` 可重复 `tag` 参数**：`?tag=key:value`，多条 **AND**（`file_tags` join +
  `HAVING COUNT(*)=N`）；已并入 `RejectUnknownQuery` allowlist；畸形值（缺 `:` 或空 key/value）返回 **400**
  `INVALID_QUERY_PARAM`（不静默忽略，同 CC-5）。因 `file_tags` 主键 `(file_entry_id,key)`「每 key 至多一值」，
  完全相同的 `tag` 去重折叠；同 key 不同值必然无解，**返回 400**（而非静默返回空集）；distinct 谓词数上限 **20**
  （超出 400，防生成 SQL 膨胀）。**cursor 分页与信封 V-2 不变**。
- **打标落点**：CP `internal/indexer` 在 `UpsertFileEntry` 后写 `file_tags`（source=`rule_static`），
  on-conflict `(file_entry_id,key)` 覆盖，重复 `UploadResult` 幂等。声明 `file_type`（名字）经
  `GetFileTypeIDByName` 解析并**优先于 glob**，未解析则回落 glob（既有部署行为不变）。best-effort：
  规则 metadata 缺失/畸形或单条标签写失败仅告警，不使索引失败；超长 key/value（VARCHAR 64/128）
  预校验跳过，避免每次上传都撞 DB 长度错误刷日志。规则查询按 `org_id` 收窄（防跨租户读规则，纵深防御；
  MT-2 时名为 `GetRuleMetadata`，MT-3 起并入 `GetRuleTagInfo` 一并返回 `dest_path_template`）。
- **本刀不含**：路径变量抽取 `path_tag_map` + 待确认队列（MT-3）、词表/手动批量打标 API（MT-4）、
  回溯 worker（MT-5）、webui（MT-6）。tag `source` 优先级（rule_static vs manual/path_var 的覆盖策略）
  随 MT-3/MT-4 再定，本刀只有 rule_static。

### 落地记录（MT-4a，标签词表 CRUD）

**MT-4 拆分**（2026-07-11 产品决策）：`merge`（回溯改写）与 `batch-tag`（异步）依赖 MT-5 回溯 worker，
故 MT-4 先做**同步部分**，分两刀：**MT-4a** 词表 CRUD（本刀）；**MT-4b** 待确认 list/approve/reject +
单文件 `PUT /files/{id}/tags`；merge/batch-tag 随 MT-5 落。

MT-4a 端点（sqlc `tags.sql` + `handler/tagkeys.go`，注入 `RouterConfig.TagKeysDB`）：
- `GET /api/v1/tag-keys`（任意登录读）、`POST`（建）、`PATCH /:key`（改 label/flags，**key 不可改**——是 `file_tags` 引用的身份）、
  `DELETE /:key`；`GET/POST /:key/values`、`DELETE /:key/values/:vid`（按**取值 id** 删，避开取值含特殊字符的路径编码问题）。
- **写全部 super_admin**（`RequireRole`），读开放给任意登录用户（对齐 file-types）。
- `key` 校验 `[a-z0-9_]{1,64}`；`(org_id,key)` / `(tag_key_id,value)` 唯一冲突→**409**；`system_reserved` key 拒删→409；
  删取值不动已打标文件（仅阻止后续采集/选用，设计原则）。删 key 级联删其 `tag_values`/`file_tags`（schema `ON DELETE CASCADE`）。
- 创建 key 的布尔标志用指针区分「未传」与 `false`，未传取 schema 默认（`value_controlled=true`/`allow_path_var=true`/`required_at_collection=false`）。

### 落地记录（MT-4c，手动单文件打标）

`handler/filetags.go` + sqlc（`GetFileTagValue`/`SetFileTag`/`DeleteFileTag`/`TagValueExists`/`FindSimilarTagValue`/
`UpsertPendingTagValueManual`/`CreateTagAudit`）+ 注入 `RouterConfig.FileTagsDB`：
- `PUT /api/v1/files/{id}/tags`（**super_admin**），body `{"tags":{"site":"tokyo","vendor":null}}`——string=set，null=clear，
  key 缺省=不动。source=`manual`，每次改写 `tag_audit`（action=`set`/`clear`，`actor_user_id`=调用者）。
- **前置校验**（改动前一次性）：文件属调用者 org（跨 org→404）；set-key 必须**格式合法且已登记** tag_key
  （honoring「key 严格受控」，未登记→400 `UNKNOWN_TAG_KEY`）；value 非空且 ≤128 rune。clear 不要求登记（
  路径变量可能给未登记 key 打过标，需可清）。校验全过再逐 key 应用（**顺序非事务**，仅基础设施错误才半应用）。
- **治理一致**：受控 key 的未登记值除照写 file_tag 外入 `pending_tag_values`（source=`manual`，suggested=大小写近似），
  不绕过词表——与 path_var 同。响应 200 返回 `{set, cleared, tags}`（tags=改后该文件全量标签）。
- 真 PG 验证 set（file_tag+audit+pending）+ clear（删+audit，共 2 审计行）。

### 落地记录（MT-5a，回溯打标 worker 地基 + pending merge）

**MT-5 拆分**（2026-07-11 拍板）：MT-5a = 回溯任务 outbox + worker + 接线 `merge` 为首个 job 类型；
`batch-tag` 与规则改动回溯随 **MT-5b** 复用同一通道落。

- **新表** `retag_jobs`（迁移 `000005`，只追加）：outbox 队列（`id/org_id/kind/spec JSONB/status/attempts/
  affected_count/last_error/actor_user_id/时间戳`）。单 CP 实例消费；`idx_retag_jobs_pending` 部分索引按
  `created_at` 取最老 pending。**触发信号**：merge/batch_tag/rule_retag 均入此表，重活离请求路径异步跑。
- **sqlc**（`queries/retag.sql`）：`EnqueueRetagJob`、`GetRetagJob`、`ClaimNextRetagJob`（`UPDATE...WHERE id=(SELECT..
  FOR UPDATE SKIP LOCKED)` 原子领取，置 running+attempts+1）、`MarkRetagJobDone/Failed`、`MergeTagValue`
  （**单条 CTE**：`tk` 从 `tag_keys` 解 key 名并校验 org 归属 → `updated` 把该 org 下 `file_tags` 中 `from_value`
  改写为 `to_value` 并 `RETURNING` → 逐文件插 `tag_audit`(action=`merge`,source=`retro`)。`:execrows`=改写文件数。
  幂等：重跑无 `from_value` 行则改写 0、审计 0）。
- **worker** `internal/worker/retag.go`（复用 offline_sweeper 的 ticker 模式，默认 5s poll，启动先 `DrainOnce`
  清积压）：`ClaimNextRetagJob` 领取 → 按 `kind` 分派 → `runMerge` 调 `MergeTagValue` + 删 pending 行（best-effort，
  失败不失败 job）+ `MarkRetagJobDone(affected)`；解码/未知 kind/执行错→`MarkRetagJobFailed`。main.go `go retagWorker.Run`。
- **契约**（新端点）：`POST /api/v1/pending-tag-values/{id}/merge`（**super_admin**）——body `{"into":"Tokyo"}` 可选，
  缺省用该行 `suggested_value`；`into` 必须**已登记**（`TagValueExists`，未登记→400 `UNREGISTERED_MERGE_TARGET`；
  merge 只做规范化，新值提升是 approve 的事）且≠原值（否则 400，提示用 approve）。校验过 `EnqueueRetagJob` 入队，
  **返回 202** `{job_id,status,into}`（异步）。新增只读 `GET /api/v1/retag-jobs/{id}`（任意登录，按 org 收窄，
  跨 org→404）供轮询状态。job kind/spec 契约在 `internal/retag`（enqueuer 与 worker 共享，互不 import）。
- **共享通道对齐设计**：merge 不同步执行而走 worker，与后续 batch-tag 复用同一 outbox（设计 §P1.2(5)）。
- 真 PG15 验证：merge `tokyo→Tokyo` 改写 2 文件 + 2 审计行、`osaka` 不动；**重跑幂等**（0 改写/0 新审计）；
  **跨 org 隔离**（异 org 领域的 merge 改写 0）；迁移 `000005` up/down 往返干净。

### 落地记录（MT-6，webui 四屏 7a–7d）

元数据 6c Phase 1 前端，按屏拆分：**MT-6a** `7c` 标签词表（PR #76）、**MT-6b** `7d` 待确认队列（PR #77）、
**MT-6c** `7b` 文件页标签筛选+批量打标（PR #78）、**MT-6d** `7a` 规则表单元数据步骤。用 WR-1 既有组件「够用一致」。
数据拉取统一走 SWR（规避 `react-hooks/set-state-in-effect`——master 已有 8 处该 lint 违规且无 CI 门控）。

- **7c**（`Settings/TagKeys`）：标签键 ProTable + 新建/编辑弹窗 + 取值抽屉（SWR）。
- **7d**（`Settings/PendingTags`）：待确认取值 list + approve/merge/reject；merge 走 Select 选**已登记**目标值（默认疑似）。
- **7b**（`Files`）：标签列 + faceted 筛选（key/value Select，取值仅列已登记，合治理）+ super_admin 批量打标弹窗
  （按当前**状态+标签**谓词圈选，文件名/日期不参与）。顺带修既有 `FileEntry.status` 类型/`STATUS_OPTIONS` 用错值
  （响应大写 `COMPLETED`，筛选/批量用 lowercase `file_status` 枚举）——新增 `FileStatusFilter` 类型 + 共享 `FILE_STATUS_COLOR`。
- **7d/7b 契约**：`tag` 谓词 axios 用 `paramsSerializer:{indexes:null}` 序列化为 `tag=a&tag=b`。
- **7a**（`Agents/RuleForm` 加第 4 步「元数据」）：声明 `file_type` + 静态标签（key Select+value）+ 路径标签映射
  （key Select + 模板变量），写入 `collection_rules.metadata` JSONB。转换逻辑抽到 `Agents/ruleMetadata.ts`（单测 5 例）。
- **CP 契约增量（MT-6d）**：`collectionRuleResponse` 加 `metadata` 字段（`toRuleResponse` 回填 `r.Metadata`）。
  **必需**——否则编辑规则时前端读不到既有 metadata，`updateRule` 会把缺省 metadata 落成 `{}` **抹掉**声明。additive、安全。
- 全部 **live 验证**（真 CP+浏览器 chrome-devtools）：7c 词表增删；7d merge/approve/reject 出队；7b facet site:tokyo→3、
  批量 vendor=omron worker changed 3、状态筛选；7a 建规则（元数据入库正确）→编辑预填三段→保存不抹除。

### 落地记录（MT-5b，批量打标 POST /files/batch-tag）

MT-5b = 复用 MT-5a 的 retag_jobs 通道做**批量打标**（按 `GET /files` 谓词圈选文件集，统一 set/clear 标签）。
**规则改动回溯推后**（低价值/推测性，体量≈MT-2；批量打标已覆盖历史数据人工补标的近期真实需求）。

- **契约**（新端点）：`POST /api/v1/files/batch-tag`（**super_admin**），body
  `{"filter":{agent_id,bucket_id,file_type_id,status,tags:["site:tokyo"]}, "tags":{"vendor":"omron","obsolete":null}}`。
  filter = 与 `GET /files` 同款谓词（复用 `parseTagPredicates`，同 maxTagFilters/去重/冲突校验）；tags = 应用的
  set(string)/clear(null) 映射。**前置校验**（同单文件 PUT）：key 格式合法且 set-key 已登记（未登记→400
  `UNKNOWN_TAG_KEY`）、value TrimSpace 后非空 ≤128 rune；受控未登记值 upsert `pending_tag_values`（source=manual，
  每 key 一次——值对全体文件相同，不绕过词表）。校验过入队 `batch_tag` job，**返回 202** `{job_id,status}`（异步）。
- **spec 契约**在 `internal/retag`（`BatchTagSpec`{Filter,Tags}）：filter 用 JSON 友好型（UUID/status 为 string，
  tag 谓词已解析为 `[]TagPredicate`），worker 侧 `batchFilter` 反解为 `db.BatchTagFilter`。
- **执行查询**（**手写**，`internal/db/batchtag_queries.go`，因谓词含可选过滤 + 动态 tag-AND 子句，sqlc 表达不了；
  复用 `tagFilterClause` 与 `GET /files` 完全同款选择语义）：
  - `BatchSetFileTag`：单条 CTE——`sel`（选中文件）→ `before`（LEFT JOIN 取旧值快照）→ `upsert`（
    `INSERT..ON CONFLICT DO UPDATE SET .. WHERE value IS DISTINCT FROM EXCLUDED.value`，只改变化行）→
    逐文件写 `tag_audit`（`WHERE old IS DISTINCT FROM new`，action=set/source=manual）。`:execrows`=**变更文件数**。
  - `BatchClearFileTag`：`sel` → `DELETE .. USING sel RETURNING` → 逐删文件写 audit（action=clear）。execrows=清除数。
  - 均**幂等**：重跑改写/删除/审计 0 行。file_tags.source=manual、audit.source=manual（管理员批量动作，仅异步执行）。
- **worker** `runBatchTag`：解 spec → `batchFilter` 反解（畸形 UUID→fail）→ 按 key 排序逐个 set/clear（复用同一
  MarkDone-失败→fail 兜底）→ affected 求和写 MarkDone。unknown kind 仍 fail。
- 单测：worker batch set+clear/apply 错→fail/坏 filter→fail/坏 spec→fail（88%）；handler set/trim/clear/未登记值入队/
  未登记 key 400/非法 key/空值/坏 filter UUID/坏 filter 谓词/非受控 key 不入队/enqueue 错 500/403/501；db sqlmock；
  保护路由 401 纳入 batch-tag。**集成测试**（`-tags=integration`，真 PG15）：site:tokyo 圈选 set vendor→2 文件（osaka 不动）
  + 2 审计、重跑幂等 0、clear→2、再 clear→0。

### 落地记录（MT-4b，待确认取值队列 list/approve/reject）

`handler/pendingtags.go` + sqlc（`ListPendingTagValues` join `tag_keys` 出 key 名、`GetPendingTagValue`、
`CreateTagValueIfAbsent`（`ON CONFLICT DO NOTHING`）、`DeletePendingTagValue`）+ 注入 `RouterConfig.PendingTagsDB`：
- `GET /api/v1/pending-tag-values`（任意登录读，按 `hit_count DESC, first_seen_at ASC` 排）、
  `POST /:id/approve`、`POST /:id/reject`（**super_admin**）。
- **approve** = 把 `extracted_value` 提升进该 key 的 `tag_values` + 删 pending 行。文件已带原始值故**无需重写 file_tags**，
  提升只是让该值成为合法词表/筛选项。两步**顺序非事务**：删失败则行仍留队，重试幂等（value 插入是 DO NOTHING），自愈。
- **reject** = 只删 pending 行（不进词表）；已打标文件不动（设计）。
- 全部按 `org_id` 收窄（`GetPendingTagValue`/`DeletePendingTagValue` 带 org 谓词，防跨租户）。`rows==0`/`ErrNoRows`→404。
- **merge 未做**（依赖 MT-5 回溯 worker：把 tokyo 并入 Tokyo 需重写 file_tags + 审计），随 MT-5 落。
- 真 PG 验证 join 列表 + approve 提升+删除 + reject。

### 落地记录（MT-3，路径变量抽取 + 待确认队列）

CP `internal/indexer` 新增：在 `static_tags` 之后，用规则 `dest_path_template` **trollsift 反解** storage path
抽取 `path_tag_map` 变量，写 `file_tags`（source=`path_var`）。controlplane 新依赖 `pkg/trollsift`（require+replace，
镜像 agent）。固化以下语义：

- **path_var 不覆盖既有标签**：用 `INSERT ... ON CONFLICT (file_entry_id,key) DO NOTHING RETURNING`——显式
  `static_tags` 优先于路径推断；`RETURNING` 是否有行即「本次是否新插入」信号。（rule_static 仍用 DO UPDATE 覆盖。）
- **`allow_path_var` 治理**：已登记 key 若 `allow_path_var=false`（显式禁止路径变量映射），path_var **完全跳过**——
  既不写 `file_tags` 也无入队副作用。未登记 key 默认允许（写标签，不受词表治理）。
- **待确认队列治理**：仅当 key 是**受控** `tag_key`（`value_controlled=true`）且值不在 `tag_values` 时，
  除照写 `file_tags`（原始值）外，upsert `pending_tag_values`（`ON CONFLICT (tag_key_id,extracted_value)` 增 `hit_count`）。
  非受控 / 未登记 key 只写标签、不入队。`suggested_value` = 大小写近似的既有取值（`lower(value)=lower(?)`；
  pg_trgm 模糊留待后续）。
- **幂等**：`pending_tag_values.hit_count` **仅在 file_tag 新插入时** +1（靠上面的 `RETURNING` 信号门控），
  重复 `UploadResult`（行已存在）不重复入队、不重复打标。
- **best-effort**：模板解析失败 / storage path 不匹配模板 / 变量缺失 / 各步 DB 错误一律告警，不使索引失败。
- **path_tag_map 形状**：`{"site":"{site}"}`——值为单个 `{var}` 引用（支持 `{var:fmt}`，取 `var`），
  非此形状跳过。变量名复用 trollsift 模板（V-3）。
- 校验：单测（抽取 / 模板不匹配 / 受控未登记入队 + suggested / 已登记不入队 / 非受控不入队 / 非新插入不入队）
  + 新查询 sqlmock 测 + **真 PG 逐条 SQL 校验**（DO NOTHING 的插入/冲突信号、pending hit_count 1→2、大小写近似）。

---

## D-026：事件投递响应补充诊断字段（WR-5，additive）

**决策日期**：2026-07-15
**影响范围**：controlplane（`internal/api/handler/events.go` 投递响应）、webui（`services/events.ts` + 投递抽屉）
**来源**：WR-5 事件规则页重做，规范 4c「失败行可展开响应体」需要投递失败原因，但当前 REST 投递响应未投影这些字段。

### 决策

`GET /api/v1/event-rules/{id}/deliveries` 的投递响应 `eventDeliveryResponse` **纯追加**四个字段，均来自
已存在的 `db.EventDelivery` 模型（数据早已入库，仅未对外投影）：

- `response_code`（int，webhook 最后一次尝试的 HTTP 状态；nats_publish / 未尝试时**省略**）
- `response_body`（string，最后一次尝试的响应体 / 错误文本；无则省略）
- `next_retry_at`（RFC3339，下次重试时刻；已投递 / 已终止 dead 后省略）
- `delivered_at`（RFC3339，最终成功时刻；失败 / dead 时省略）

均带 `omitempty`——**因这些字段本就可空/不适用**（nats_publish 无 HTTP code、未投递无 `delivered_at` 等），
省略即「无」。注：系统未发布、前后端同版一起部署，**无旧客户端兼容诉求**；这里的 `omitempty` 只表达字段可空，
不是为兼容不存在的老 CP。

### 为何允许这次后端改动（WR track 名义「纯前端」）

- 数据**已存在**于 `EventDelivery` 模型（`response_code`/`response_body`/`next_retry_at`/`delivered_at`），
  只是 handler 响应结构体没投影；补齐是 2 行映射 + struct 字段，**非新建端点 / 新表**。
- 与 WR-2 的 glob 编辑器 descope 形成对比：那里需要**从零建后端 CRUD + 试匹配端点**（破坏纯前端前提，价值低），
  故推后；这里是让**已实现的重试生命周期**（CC-7 / D-019）对 UI 可见，投入极小、让 4c 成为真功能而非空壳。
- 产品 2026-07-15 拍板「小幅补后端字段」。

### 落地记录

- `events.go`：`eventDeliveryResponse` 加 4 字段 + `toEventDeliveryResponse` 按 `Valid` 门控映射（nats_publish /
  未尝试时省略）。
- 测试：`events_test.go` 加 `ListDeliveries_ExposesResponseFields`（failed webhook 带 code/body/next_retry）+
  `ListDeliveries_OmitsAbsentResponseFields`（pending 全省略）。
- 前端：`EventDelivery` 加可选字段；投递抽屉 failed/dead 行 `expandable` 展开 HTTP 状态 / 下次重试 / 响应体。

---

## D-027：上传日志响应补充重试轨迹字段（WR-6，additive）

**决策日期**：2026-07-15
**影响范围**：controlplane（`internal/api/handler/events.go` 上传日志响应）、webui（`services/upload-logs.ts` + 日志页失败行展开）
**来源**：WR-6 上传日志页重做，规范 4e「失败行内嵌错误 + 重试轨迹」需要重试次数/传输进度/时序，但当前 REST 响应未投影这些字段。

### 决策

`GET /api/v1/upload-logs` 的 `uploadLogResponse` **纯追加**四个字段，均来自已存在的 `db.UploadLog` 模型
（数据早已入库，仅未对外投影）：

- `retry_count`（int，agent 重试次数）
- `bytes_transferred`（int64，已传字节；失败时为部分进度）
- `started_at`（RFC3339，尝试开始时刻）
- `finished_at`（RFC3339，结束时刻；`NULL` 直到终态 → `omitempty`）

`retry_count`/`bytes_transferred`/`started_at` **无 `omitempty`、恒返回**（对应 `UploadLog` 的非空列——
`started_at` 是 `NOT NULL`，`retry_count`/`bytes_transferred` 为 0 也是有效值、语义明确）；仅 `finished_at` 可空、
`omitempty`。前端 `UploadLog` 类型据此如实标注（前三者必有、`finished_at?` 可选）——前后端同版部署，**无旧 CP 兼容诉求**。

### 为何允许（与 D-026 同类）

数据**已在** `UploadLog` 模型（`retry_count`/`bytes_transferred`/`started_at`/`finished_at`），handler 响应未投影；
补齐是 struct 字段 + 映射行，**非新端点/新表**。等价于 D-026 的投递响应补字段——让**已有的重试/传输状态**对 UI 可见，
使 4e「失败行展开重试轨迹」成真功能而非空壳。沿用产品对 WR-5「小幅补后端字段」的同一裁量。

### 落地记录

- `events.go`：`uploadLogResponse` 加 4 字段 + `toUploadLogResponse` 映射（`finished_at` 按 `Valid` 门控）。
- 测试：`events_test.go` 加 `List_ExposesRetryTrail`（failed log 带 retry_count/bytes_transferred/timing）。
- 前端：`UploadLog` 加字段；日志页状态筛选改 chip（`CheckableTag`）、状态列 `StatusBadge domain=upload`、时间 `TimeText`、
  **failed 行 `expandable`** 展开 错误信息 / 重试次数 / 已传输 X/Y / 开始→结束。

---

## D-028：上传日志状态过滤补齐 + 修正状态取值（WR-6 后续）

**决策日期**：2026-07-15
**影响范围**：controlplane（`internal/db` List/Count 查询、`internal/api/handler` List）、webui（日志页 chip 取值 + 服务层类型 + `StatusBadge`）
**来源**：WR-6 实测发现上传日志的状态筛选「完全失效」。排查出三处叠加缺陷。

### 背景（三处叠加缺陷）

1. **后端从未实现 status 过滤**：`ListUploadLogs`/`CountUploadLogs` 的 `WHERE` 只有 org + agent + cursor，handler 也没读
   `status` 查询参数——前端一直发 `status` 但后端一律忽略（WR-6 之前就存在的洞）。
2. **前端状态取值错误**：`upload_logs.status` 真实值只有 `completed`/`failed`（indexer `indexer.go:224-227` 写死，
   响应 ToUpper 成 `COMPLETED`/`FAILED`）。但前端把「成功」映射成 `SUCCESS`，永远匹配不上；服务层类型
   `'SUCCESS'|'FAILED'|'PENDING'` 是虚构的。
3. **「待处理」态不存在**：upload_logs 是收到上传结果后才创建的**终态**记录，无「进行中/待处理」；无任何代码路径
   产生 pending 上传日志（进行中的是 `file_entries.uploading`，那是文件不是日志）。

### 决策（产品 2026-07-15 拍板）

- **后端补 status 过滤**：`ListUploadLogsParams`/`CountUploadLogsFilter` 加 `Status sql.NullString`，两条 SQL 的 WHERE
  加 `($n::TEXT IS NULL OR status = $n)`；handler 读 `?status=`、**ToLower 归一**后传入（前端传 UPPER，列存 lower）。
  空参数 = 不过滤。已有 `idx_upload_logs_status` 索引，**零迁移**；cursor 分页不变。
- **前端修正 taxonomy**：chip 只留 `全部 / 成功(COMPLETED) / 失败(FAILED)`，**去掉「待处理」**；服务层类型改
  `'COMPLETED' | 'FAILED'`。清掉 WR-9 时误加的 `StatusBadge` 假映射（`SUCCESS` BASE 项、upload 域 `PENDING` 覆盖）。

### 落地记录

- `read_queries.go`：List/Count 加 Status 参数 + WHERE；`events.go` handler 读参归一。
- 测试：db 层 `TestListUploadLogs_WithStatusFilter`/`TestCountUploadLogs_WithStatusFilter`（sqlmock 断言 status arg）；
  handler 层 `List_StatusFilterNormalized`（FAILED→failed 归一 + 传入 List/Count）/`List_NoStatusFilter`。
- 实机验证：`?status=COMPLETED`→1、`?status=FAILED`→1、`?status=failed`（小写）→1、无参→2，`total` 同步。

---

## D-029：用户禁用/启用端点（WR-8 5b「禁用而非删除」，additive）

**决策日期**：2026-07-17
**影响范围**：controlplane（`internal/api/handler/users.go` + `router.go`）、webui（`services/users.ts` + `Settings/Users`）
**来源**：WR-8 用户管理，规范 5b 要求「禁用而非删除」（软禁用，保账号与审计），但后端 `users.is_active` 有字段
（响应也返回）却**无 setter**——`UpdateUser` 只接受 username/email/role，仅有硬 `DeleteUser`。

### 决策（产品 2026-07-17 拍板「小幅补后端」）

新增 `PUT /api/v1/users/:id/active`（super_admin，与 `/password` 子资源同款风格），body `{"is_active": bool}`：

- 复用**已存在**的 sqlc 查询 `UpdateUserActive`（`users.sql.go`，数据/查询早已生成，仅未接 handler/路由）——非新表/新查询。
- `is_active` 用 `*bool` + `binding:"required"`：拒绝缺字段，同时允许显式 `false`（禁用）。
- **不能禁用自己**：`!is_active && callerID == :id` → 400 `CANNOT_DISABLE_SELF`，避免把自己锁出。
- 写后 `GetUserByID` 回读：`UpdateUserActive` 是 `:exec` 无 not-found 信号，回读既能返回刷新后的 user 又能对不存在 id 返 404。
- 硬 `DeleteUser` 端点**保留**（未删），但 **UI 不再暴露硬删**，改为禁用/启用（活跃可禁、已禁可启，危险确认仅用于禁用）。

### 落地记录

- `users.go`：`SetActive` handler + `UsersDB` 接口加 `UpdateUserActive`；`router.go` 注册 `PUT /users/:id/active`。
- 测试：`SetActive_Disable`（is_active=false 透传 + 回读 body）/`_MissingField`（400）/`_CannotDisableSelf`（自禁 400、DB 未被调）。
- 前端：`setUserActive(id, isActive)`；`Settings/Users` 操作列 删除 → 禁用/启用（`useDangerConfirm`，自禁按钮禁用）。
- 实机：自我禁用→400、缺字段→400。

---

## D-030：文件索引一致性与写入准入模型（STS grant + 注册 outbox + 分片对账）

**决策日期**：2026-09-08
**影响范围**：controlplane（`internal/indexer`、`internal/storage`、`internal/grpcserver`、`internal/api/handler`、
`internal/worker`、`migrations`）、agent（`internal/queue`、`internal/executor`、`internal/uploader`、`cmd/agent`）、
`proto/v1/agent.proto`（只增字段）、SDK（写入协议）、deploy（MinIO 通知与 ILM 配置）、
文档（`system-design.md` §4.5/§4.7/§5.7/§5.8/§6.3/§6.5、`contracts.md`）
**权威设计**：[`docs/design/consistency-and-ingest.md`](docs/design/consistency-and-ingest.md)
**缺陷清单**：[`docs/tasks/bugs/open.md`](docs/tasks/bugs/open.md) IC-BUG-1…IC-BUG-29
**来源**：产品提出两条此前不成立的前提——(1) 必须允许 Agent 之外的进程（ETL）写入 bucket 并记录 tags 与血缘；
(2) 对象数量级为千万/年、3–5 年上亿。据此对写入链路做全面审计，发现**设计与实现存在系统性反转**。

### 背景：审计发现（2026-09-08）

`system-design.md` §4.5 描述的主路径（Agent 上传 → 上报 `UploadResult` → CP 索引）**在实现中完全不存在**，
而 D-025 补充 §3 与 `metadata-model.md` P2.2 明确定位为「只做对账兜底」的 minio-event webhook，
**成了 `file_entries` 的唯一写入者**。根因是四条独立断链（IC-BUG-1…IC-BUG-4），合起来意味着
**Agent 数据面从未端到端跑通过**——单元测试全部 mock 掉 STS 与 gRPC，故长期不可见。

同时确认：全 controlplane 无一处 `ListObjects` / `StatObject`，**不存在任何 MinIO↔PostgreSQL 对账机制**；
事件丢失有三条相互独立的通道（`queue_dir` 在 `/tmp`、`queue_limit` 溢出、索引失败仍返回 200），
且丢失即终态。

### 决策

**一、不更换存储层。** 四项需求中只有一项与存储实现有关，且对象存储在该项上无差别。
详细评估见设计文档 §2（lakeFS / Iceberg-Delta / S3 Object Tagging / 换 PG 均被否决）。

**二、事实来源改为「写入方的持久化意图 + CP 的授权记录」**，MinIO 事件降级为低延迟提示：

```
对账成本 = O(未结算的授权) + O(总量 / 轮转预算)     ← 两项都有界且可配
```

**三、STS 授权即写入意向声明。** 新增 `write_grants` 记录每次 STS 签发的 (principal, bucket, TTL)。
写入方 outbox 清空后调 `POST /api/v1/grants/{id}/settle {count:N}`，CP 比对注册数即结算，
**稳态下不发一次 `ListObjects`**。数量不符或到期未结算时**同样不列举**——改为从 PG 查出该
时间窗口内有写入的分片、置为 `active`，交给 L2 的分片预算核实。grant 记录的是**写入意向的
时间窗口**（谁、哪个桶、什么时段），空间切分由分片承担。

**四、统一写入协议，Agent 与 SDK/ETL 共用**：申请 STS + grant_id → 直传 MinIO（内容不过 CP）→
`POST /api/v1/files/register`（批量幂等，携带 tags / run_id）→ 失败留 outbox 重试 → 结算。
这是 `metadata-model.md` P2.2 的完整化，并把同一协议**回收适用于 Agent**。

**五、宽表/窄表分家**（PostgreSQL 分区表要求唯一约束包含分区键，否则 `(bucket_id, storage_path)`
幂等性会失效）：新增不分区的窄表 `object_keys` 承载幂等键与对账；`file_entries` 按 `uploaded_at`
月 RANGE 分区承载业务查询。对账只扫窄表，不碰宽表。

**六、`file_entries` / `object_keys` 增加 `observed_at` 排序键与 `source` 来源列**，
upsert 加 `WHERE EXCLUDED.observed_at >= 现有值` + 富字段 `COALESCE`，
使两条写入路径可交换（修 IC-BUG-8 / IC-BUG-13）。

**七、对账分三级**：L1 grant 结算（全程零列举）→ L2 分片轮转（长尾兜底，采用外部项目
minio-inventory 已在生产验证的分片审计模型）→ L3 幽灵清理。L2 的八条实施约束见设计文档 §3.5，
其中最关键的一条：**判「分片被写过」只能用对象自身的 mtime，绝不能用 `updated_at`**
（扫描自身的 upsert 会推进后者，导致封存机制完全失效）。

**八、授权宽度与清点成本分开处理（2026-09-09 追加）。** STS session policy **写整桶**
（`arn:aws:s3:::{bucket}/*`），不按前缀收窄；`dest_path_template` **不受任何约束**。理由：

- **授权宽度是管理权限问题**——单组织私有化部署、Agent 需审批接入，已审批 Agent 拿到所属桶
  写权限可接受。
- **清点成本是技术缺陷**——不能靠约束用户解决，必须由机制解决（即第七条的分片+封存）。

副作用是消掉一整类故障：IC-BUG-3（policy 前缀 ≠ 实际对象键）不再可能发生；前缀含日期跨零点
403、Agent 改名后持续 403（`agentCtx.AgentName` 只在审批时取一次、运行中永不更新）、多规则
Agent 的 session policy 体积上限，全部不存在。IC-1 的实现量因此降低一档。

曾考虑的三种模板约束（强制 `agents/{agent_id}/` 前缀 / 首段不得为逐文件变量 / 必须含时间分区）
**全部否决**，其中第三种被 minio-inventory 的生产实测直接推翻，见下方备选方案。

**九、血缘提前实施 `metadata-model.md` P2.3 的 run 模型**（触发信号「ETL 开始建设」已到达），
挂载点为 `batch-download-urls`（记 `run_inputs`）与 `files/register`（挂输出），ETL 零额外申报。

### 备选方案（被否决）

- **更换存储层为 lakeFS / Iceberg / Delta**：见设计文档 §2 逐项理由。核心是这些需求属于**准入协议**
  与 **catalog** 两层，不属于存储层；血缘是图，任何对象存储都无法表达。
- **用 S3 Object Tagging 让对象自描述、PG 退化为可重建缓存**：`ListObjectsV2` 不返回 tag，
  读取需每对象一次 `GetObjectTagging`，把对账从 O(N) 次列举变成 O(N) 次请求，比现状更差。
  保留为「灾难重建的冗余副本」这一有限用途。
- **把 minio-event 做成可靠通道（JetStream + 持久 queue_dir + 失败重投）作为唯一方案**：
  能修补丢失，但仍无法解决贫血写入、覆盖富字段、以及「MinIO 静默不发事件」的残余风险；
  且事件路径无法携带 tags 与血缘。降级为**兜底**而非主路径。
- **周期性全量列举 MinIO 对账**：成本 O(总量)/周期，在目标量级（3–5 年上亿）上不成立，
  换硬件只改常数不改量纲。
- **`file_entries` 直接按时间分区、不拆窄表**：唯一约束被迫包含分区键，
  `(bucket_id, storage_path)` 幂等性失效，同 key 不同时间会插入两行。
- **允许写入方绕过 CP 直连 MinIO**（产品已确认不需要）：准入闸门失效，对账退回 O(总量)。
- **约束 `dest_path_template` 以收窄 grant 前缀**（三个变体均否决，2026-09-09）：
  - *强制 `agents/{agent_id}/` 前缀*：把 producer 身份塞进数据布局。从数据管理角度，
    对象命名空间该按数据语义组织，不该按"谁上传的"组织；且会改写对象键，牵动
    `file_entries.storage_path`、MT-3 的 path_var 反解（`indexer.go:405` 拿模板反解 storage_path，
    前缀必须同步进模板）、webui 模板预览、以及已落盘对象的重铺。
  - *要求首段不得为逐文件变量*：仅为「让 grant 前缀非空」服务，而前缀已不承担对账职责。
  - *要求模板含时间字段以便按时间片扫描*：**被生产实测推翻**。minio-inventory 的 karadar 桶
    260 个分片中 **258 个**首次写入比目录名日期晚 7 天以上、246 个晚 30 天以上、最长晚 232 天，
    且为持续行为而非一次性迁移。任何「路径含日期 → 只扫当天分区」的调度都会把这 246 个
    **正在被写入**的分片判为陈旧、永不扫描。根本理由：命名约定能说明新数据落在哪，
    **不能说明历史分区没被改过**，因此不能充当正确性机制。这也正是「上传一组历史归档数据」
    这类场景的真实形态。见 `~/workspace/minio-inventory` `docs/01-审计可扩展性设计.md` §2.3。

### IC-2a 契约落地（2026-09-10）

追加迁移 000006，为 `file_entries` 增加 `observed_at TIMESTAMPTZ NOT NULL DEFAULT now()`、
`source`（仅 agent/api/minio_event/audit）、可空 `event_seq` 与粘性 `meta_incomplete`。
守卫采用较新时刻优先、相等时任一 NULL sequencer 放行，否则按 32 位补零比较；
软删除同步推进两列，压制的 upsert 回查现有行并返回 suppressed，避免 ErrNoRows 传播。
Agent/API 时刻由 PostgreSQL now() 产生；MinIO 用 eventTime，audit 用列举时刻。
UploadResult 将只增 `task_id` 字段，Agent 持久化 reported 结果并在 ack 后完成，超时重报。
失败只写 upload_logs；拿不到规则元数据时置 meta_incomplete（从不清位），列表与详情暴露该字段。

### 分期实施

| 阶段 | 任务 | 内容 | 说明 |
|---|---|---|---|
| 止血 | IC-1…IC-5 | 修 IC-BUG-1…IC-BUG-7、IC-BUG-9…IC-BUG-12、IC-BUG-16…IC-BUG-17 | 独立可发；完成后数据面首次端到端可用 |
| 地基 | IC-6…IC-7 | `observed_at`/`source`/`grant_id`/`run_id` 列、`object_keys` 拆分、分区、排序键 upsert | **必须趁数据量小完成** |
| 准入 | IC-8…IC-10 | `write_grants` + `register` + `settle` | 依赖地基阶段的列；policy 保持整桶 |
| 对账 | IC-11…IC-13 | 事件传输改 JetStream（D-031）→ L1 → L2 → L3 | 依赖地基（窄表的 mtime 是 L2 唯一可用信号列） |
| 血缘 | IC-14 | run 模型（P2.3） | 随 ETL 建设 |

### 待定（不阻塞止血阶段）

~~`storage_path` 前缀约定的最终形态（A）~~（**2026-09-09 已定：不约束**，见上方第八条）、
分区粒度与归档策略（B）、
文件列表 `total` 去 `COUNT(*)` 的方案（C，改动 D-007 契约需单独决策）、
ETL 是否允许就地覆盖同一 key（D）、SDK outbox 最小形态（E）。详见设计文档 §6。

### 关联

- 前序：**D-014**（minio-event 鉴权）、**D-017**（webhook 索引路径复活）、
  **D-025** 补充 §3（衍生数据入口：ETL 禁止直连 MinIO）、**D-007**（cursor 分页与 `total`）
- 设计：`docs/design/consistency-and-ingest.md`、`docs/design/metadata-model.md` P2.2/P2.3
- 缺陷：`docs/tasks/bugs/open.md` IC-BUG-1…IC-BUG-29

### 落地记录（IC-2a，上报主路径，2026-09-11，PR #98）

**止血阶段的第二刀落地，数据面主路径首次真的通了**（不是「单测通过」——九条 live 验收在 dev 上逐条留证）。
落地的是本决策第三条（`observed_at` 排序键）与第五条（写入来源标记）的**止血子集**，宽表/窄表分家与三级对账仍在后面。

- **迁移** `000006_index_observation`：`file_entries` 增 `observed_at` / `source` / `event_seq` / `meta_incomplete`。
  回填后 **`DROP DEFAULT`**——把 `DEFAULT now()` / `DEFAULT 'minio_event'` 永久留在列上，会让漏传这两个值的
  写入静默拿到错误时钟与错误来源，比报错更坏。
- **守卫的权威是代码，不是文档**：`consistency-and-ingest.md` §3.4 的 SQL 文本连续三轮「新写 → 一执行就碎」
  （谓词恒真 / NULL 吞写 / `RETURNING` 返 0 行 / audit 覆盖），四轮读文档都没读出来。因此本刀第一步是把守卫
  做成仓库里**可执行**的东西：真迁移 + 真 upsert + 表驱动变异矩阵（`db/ingest_integration_test.go`，真 PostgreSQL，
  A1/A2/A3′/P2/A4b/LPAD 六项，七种变异各自被它声称防的那条用例杀掉）。**§3.4 自此降为「意图与不变式」**。
- **`RETURNING` 的语义坑已处理**：加 `WHERE` 后被正确压制的写入返 0 行 → `sql.ErrNoRows`。若当 error 上抛，
  IC-4 ① 会判为处理失败并重投，而 `queue_dir` 是队头阻塞单队列——**一条本该被压制的陈旧事件会让索引 feed
  停摆数分钟**。实现区分「守卫压制」与「行不存在」，两者都不是错误。
- **`meta_incomplete` 只由 `agent`/`api` 源改写**：初版是粘性 OR，但「webhook 先到」是生产常态且 webhook
  永远不知道规则，粘性 OR 会把几乎每个文件永久标成「元数据不完整」。
- **失败上报只写 `upload_logs`**（IC-BUG-33）：另一个选项有损坏真实数据的分支——`DO UPDATE` 无条件覆盖
  `status`，会把「已成功上传、后来重传失败」的活对象标成 `failed`。
- **`rule_id` 归属三分支，宽松处理**（IC-BUG-29）：「规则不存在」是稳态正常情形（队列与规则生命周期解耦），
  严格拒绝等于让「删规则」静默丢弃已在 MinIO 里的文件的索引行，只是把幽灵推给 IC-13。
- **前置**：`init-minio.sh` 把 CP 凭据建成 root 的 **service account**，而 MinIO 不允许 service account 调
  `AssumeRole`——**任何从头 bootstrap 的环境都签不出 STS**（IC-BUG-36，PR #97 先行修掉，改为真实 IAM 用户
  + 具名最小权限 policy + 脚本自断言）。这条与 IC-1「STS 链路接通」的表面冲突已查实：IC-1 当时用的是 dev 上
  手工建的真实 IAM 用户，2026-09-10 为修预签名下载才被换成 svcacct。**「脚本跑完 ≠ 环境可用」现在由脚本自己断言**。

---

## D-031：MinIO 事件传输由 webhook 改为 NATS JetStream（排期对账阶段 IC-11）

**决策日期**：2026-09-08
**影响范围**：deploy（`init-minio.sh`、compose）、controlplane（`internal/event`、`internal/api/handler/events.go`、
`internal/api/router.go`、`config`）、文档（`system-design.md` §1.4/§2.1/§6.5、`docs/ops/`）
**关联**：**D-030**（一致性与写入准入总设计，本条是其对账阶段的前置）、**D-014**（webhook 共享密钥鉴权，将被取代）、
**D-017**（webhook 索引路径复活）
**权威设计**：[`docs/design/consistency-and-ingest.md`](docs/design/consistency-and-ingest.md) §3.5

### 背景：当初并没有做过这个选型

审计发现，**`system-design.md` 从初始导入（`c2341f0`，2026-04-27）起就自相矛盾**：

| 位置 | 说法 |
|---|---|
| §1.4 整体架构图 | `MinIO ──事件通知──► [PostgreSQL │ Redis │ NATS 事件总线]` |
| §2.1 组件职责 | NATS 职责 = 「**MinIO 事件消费**、Webhook 分发、内部异步通信」 |
| §6.5 事件通知配置 | 给出的却是 `notify_webhook` + `POST /internal/minio-event` 的可执行配置 |

实现照抄了 §6.5——因为它是三处里唯一一段**可直接执行的配置片段**（`init-minio.sh` 于 PR #4 落地、
`events.go` 骨架于 PR #6 落地）。架构图与 §2.1 描述的 MinIO→NATS 通路**从未被实现**。
D-014 只讨论「webhook 端点如何鉴权」，把 webhook 视作既成事实，未回头质疑传输选型。

**结论：不存在「当初为什么选 webhook」这个决策，只有「照抄了文档中更具体的那一半」。** 本条补上缺失的选型。

### 决策

**MinIO → Control Plane 的对象事件通道改用 `notify_nats` + JetStream（`jetstream=on`），排期在 D-030 的对账阶段（IC-11）。**

选它而非 webhook 的理由，三条都直接服务于 D-030 的对账模型：

1. **解耦「投递成功」与「处理成功」**。MinIO 拿到 JetStream 的 PubAck 即完成投递；CP 宕机时消息留在 stream 中，
   重启后由 durable consumer 从原位置续读。**这从根上消除 IC-BUG-6**——不再依赖「CP 返回什么 HTTP 状态码」
   这一极易写错的约定（现状正是无条件返回 200 导致 MinIO 丢弃事件），改为「处理成功才 ack，否则按
   `ack_wait` 重投」。
2. **可重放**。retention 期内可从任意序号重放，对重建索引与对账极有价值；webhook 无此能力。
3. **stream sequence 是全局单调序号**。可直接用作 D-030 §3.4 的 `observed_at` 排序键来源；
   更关键的是支持**链路自证**——比较 stream 的 `first_seq` 与 consumer 的 `ack_floor`，即可判断是否有消息
   因 retention 过期而从未被消费，进而定位「哪些分片的核实结论已不可信」。这是 webhook 架构下无法回答的问题。

基础设施已就绪：dev 与 prod 的 NATS 均已启用 JetStream（`docker-compose.dev.yml:62`、
`docker-compose.prod.yml:53` 的 `-js`），但 CP 代码一直只用 core NATS（`conn.Publish` / `conn.Subscribe`），
JetStream 处于闲置状态。

### 明确不解决的（避免误判收益）

- **不消除「MinIO 静默不发事件」的残余风险**——那是 MinIO 内部行为，与传输层无关。
  **D-030 的 L2 分片轮转仍然必需，一项都不能省。**
- **`queue_dir` 的问题原样存在**。`notify_nats` 同样有 `queue_dir` / `queue_limit`，IC-BUG-9 必须照修。
- **对象键的 URL 编码原样存在（IC-BUG-19）——2026-09-10 双向实测补充**。dev 环境对同一个键同时挂
  `notify_nats` 与 `notify_webhook` 各抓一次载荷，**两者字节级同构**：顶层字段均为
  `['EventName','Key','Records']`，而 CP 实际读取的 `Records[].s3.object.key` 两边都是
  `ic19%2Fnested+dir%2F%E4%B8%AD+%E6%96%87.csv`。编码发生在 MinIO **构造事件对象**时而非传输层
  （`%2F` 位于 JSON 字符串**内部**，两种传输都不改写它）。**IC-BUG-19 与本条正交，已拆为独立的 IC-2c（排在 IC-2a 之前），
  不得等 IC-11。** 附带提醒：事件信封有个未编码的顶层 `Key` 字段（**webhook 与 NATS 都有**，CP 当前
  只解析 `Records[]` 所以从没注意到），但它是 `bucket/key` 拼接的 MinIO 私有字段、不在 S3 事件规范内，
  **不要用它绕过 unescape**。
- **新建 bucket 仍需逐个注册通知（IC-BUG-7）**。换传输不改变「`MakeBucket` 之后要不要配通知」这件事，
  只是 ARN 从 `arn:minio:sqs::primary:webhook` 变成 NATS target 的 ARN。**IC-4 ② 的实现须把 ARN 做成可配置**，
  否则 IC-11 落地时会把它打回原形。
- **retention 配置过短 = 静默丢消息**，这恰恰是上面第 3 条「链路自证」存在的理由，不是可选项。

### 全量扫描结论（2026-09-10）

上面这份清单此前是「撞见一条补一条」——IC-BUG-19 与 IC-BUG-7 都是在别的工作里撞上才发现漏了。
既然清单的价值就在于完整，这次拿 **IC-BUG-1…IC-BUG-34 逐条**问同一个问题：
**IC-11 实际改变的六件事碰得到它吗？**（六件事 = 投递语义 PubAck + 显式 ack/重投、跨 CP 宕机的持久化、
可重放、全局单调序号、鉴权载体、`/internal/minio-event` 端点消失）

**结论：34 条里只有 6 条与事件通道有关。**

| 缺陷 | IC-11 的影响 | 结论 |
|---|---|---|
| **IC-BUG-6**（索引失败仍返 200） | ✅ **解决** | 「是否重投」由 ack 语义决定，不再依赖 CP 返回什么 HTTP 状态码 |
| **IC-BUG-7**（新建 bucket 不注册通知） | ❌ 不解决 | 换传输只改 ARN，不改变「要不要配通知」。**IC-4 ② 的 ARN 须做成可配置** |
| **IC-BUG-9**（`queue_dir` 在 `/tmp`） | ❌ 不解决 | `notify_nats` 同样有 `queue_dir` / `queue_limit` |
| **IC-BUG-19**（对象键 URL 编码） | ❌ 不解决 | 双向实测：两种传输载荷字节级同构，编码在 MinIO 构造事件时发生 |
| **IC-BUG-8**（upsert 无排序键） | ❌ 不解决，**且加重** | 初版写「改善但不解决——让排序键来源更可靠」，与下方「更正」自相矛盾（更正后 `minio_event` 源的排序键取 `eventTime`，**与传输无关**）。正确表述：IC-11 的至少一次投递与可重放会**增加**重复 upsert，排序键因此**更必要**，而 SQL 侧的 `WHERE` + `COALESCE` 一行都不能省 |
| **IC-BUG-13**（`content_type` 不赋值） | ◐ 相关但不解决 | 载荷里**本来就有** `contentType`（实测确认，两种传输都有），是 CP 侧 `IndexUpload` 没读它。与传输无关 |
| 其余 **28 条** | 无关 | agent 侧（采集/队列/上传/凭据）、STS policy、gRPC 流与 registry、DB 查询与统计——事件通道碰不到 |

### 更正：stream sequence 不能「直接用作 `observed_at`」

本决策上文第 3 条写的是「stream sequence … 可直接用作 D-030 §3.4 的 `observed_at` 排序键来源」。
**这句话把两个用途混在了一起，落地时会撞墙**：

1. **类型对不上**：`observed_at` 是 `TIMESTAMPTZ NOT NULL`（`consistency-and-ingest.md:231`），
   而 JetStream 的 stream sequence 是 `uint64`。
2. **更根本的是不可比**：`observed_at` 要在 **4 个 source 之间**排序（`agent | api | minio_event | audit`），
   而 stream sequence 只对 `minio_event` 这一路单调。拿它当 `observed_at`，另外三路就没法与之比较——
   排序键会退化成「只在同一 source 内有效」，而 IC-BUG-8 要防的恰恰是**跨 source**的覆盖。

**正解（2026-09-10，两次修订后定稿，前置拍板 F 结案）**：判据是「**这个值客户端能不能左右**」，不是「来自哪个时钟」——
`observed_at` 按 source 取各自最可信且不可被客户端左右的时刻：`minio_event` ← `eventTime`（**MinIO 生成**）、
`agent`/`api` ← **PostgreSQL 的 `now()`**（§3.5 坑 3：所有比较的时间戳须同源，且 CP 进程时钟实测比 MinIO/PG 慢约 16ms）、`audit` ← **列举那一刻**（不是写入事务的 `now()`）。`event_seq`（事件的 `sequencer`）只在 `observed_at`
**相等**时决胜，**任一侧 NULL 必须放行**。

> **一次被证伪的中间版本，记录在此以免重犯**：曾定「四源统一取 CP 受理时刻」。评审用真 PG 证伪——
> 受理时刻由 CP 在处理那一刻取，**后处理的写入其 `observed_at` 必然更大、`>=` 谓词恒真、闸门变摆设**；
> 而 `IndexUpload` 写的 `size_bytes`/`status`/`uploaded_at`/`etag` 都是非空值，**`COALESCE` 一列都保护不到**，
> 等于 IC-BUG-8 根本没修。错因是**过度纠正**：被否掉的是 `UploadResult.uploaded_at`（**agent 提供**），
> 而 `eventTime` 由 MinIO 生成、是基础设施而非客户端，被顺手一起砍了。

**IC-11 的 JetStream consumer 保序取决于配置（`MaxAckPending=1` / ordered consumer），配错即静默失序，
因此不得依赖传输保序——`event_seq` 就是为了让它自证。**

此外，JetStream 消息同时带 sequence 与 timestamp，两者各司其职——
- 事件自身的 `eventTime` 与 `sequencer` 都在载荷里、**与传输无关**，IC-11 落地时不必改（前者即 `minio_event` 源的 `observed_at`，后者即 `event_seq`）；
- stream sequence ← 只喂 `shard_state.last_event_seq`（链路自证，用途仅此一项，见 §3.5）。

即两个字段、两个来源，不是一个。

### 代价

- 新增运维面：stream 定义、retention 策略、durable consumer 配置、磁盘容量规划。
- 鉴权载体更换：D-014 的共享密钥 → NATS creds / nkey / TLS。**fail-closed 原则继续适用**，
  但 D-014 的具体结论在切换完成后作废。
- CP 侧需引入 JetStream context 与 durable consumer，替换现有的 core NATS 订阅。

### 排期与理由：排在对账阶段，不在止血阶段

- 单独更换传输，增量收益仅为「CP 宕机不丢事件」，而修完 IC-BUG-6 + IC-BUG-9 已能取得其中大部分；
- 真正的增量价值（重放、链路自证）须待地基阶段（`observed_at` / `object_keys`，IC-6）与
  对账阶段（IC-12/IC-13）落地后才兑现；
- 现在切换会使止血阶段复杂化，而止血阶段的唯一目标是**先让数据面端到端跑通**。

> **补充：这个顺序不只是排期偏好，也是 L2 结论可信度的前提（2026-09-09 追加）。**
> 没有 JetStream，L2 仍然能工作——minio-inventory 的 L2 就在纯事件 + 扫描的形态下跑在生产上——
> 但它的 `verified` / `sealed` 结论**无法自证**：事件若因 retention 过期而从未被消费，
> 无从判定哪些分片的核实结论已经作废。`shard_state.last_event_seq` 正是为收窄这个失效范围而存在
> （用途仅此一项，见 minio-inventory `docs/01-审计可扩展性设计.md` §6.D），
> 而 HTTP webhook 没有全局单调序号，拿不到它。分期表原本就把 IC-11 排在 IC-12/IC-13 之前，
> 这里补的是该顺序的理由。

> **与 D-030 已否决项的界线**：D-030 否决的是「把 minio-event 做成可靠通道**作为唯一方案**」——
> 因为事件路径无法携带 tags 与血缘，且无法解决贫血写入与富字段覆盖。
> 本条决定的是「**作为兜底通道时，它应当用什么传输实现**」。两者不冲突：
> 事件通道降级为兜底之后**更需要"可重放"**，这反而加强了改用 JetStream 的理由。

### 备选方案（被否决）

- **维持 webhook，仅修 IC-BUG-6 + IC-BUG-9**：能止血，但拿不到重放与全局序号，
  D-030 的链路自证（判断分片核实结论是否可信）将无法实现。
- **改用 core NATS（`jetstream=off`）**：**比 webhook 更差**——fire-and-forget，MinIO 发出即忘，
  无 PubAck、无持久化，无订阅者时消息直接消失。
- **立即切换（放进止血阶段）**：见上「排期与理由」。
- **双通道并行（webhook + JetStream 同时开）**：两条路径写同一张表，在 `observed_at` 排序键
  （地基阶段 IC-6）落地前会互相覆盖；且加倍了鉴权与运维面。切换应是一次性替换。

---

## D-032：proto 生成链钉定——protoc → buf（tools/ 钉版本 + local 插件）

**决策日期**：2026-09-11
**影响范围**：`tools/go.mod`、`buf.yaml`、`buf.gen.yaml`、`Makefile`（`generate-proto`）、
`.github/workflows/ci-proto.yml`、`.gitignore`、`api/v1/*.pb.go`（仅生成器署名行）、文档
（CLAUDE.md、docs/tasks/active.md）
**关联**：CLAUDE.md「契约文件」表（`proto/v1/agent.proto`）、`docs/tasks/active.md`
（原「proto→buf 复现性 follow-up」候选，本条结案）

### 背景：protoc-gen-go 版本是云端环境的隐式残留

`api/v1/agent.pb.go` 文件头写着 `protoc-gen-go v1.36.10 / protoc v7.36.1`——这个版本是早期在
GitHub Copilot 云端 agent 环境里生成时那个固定环境的隐式残留。仓库里**没有任何东西钉住它**：
没有 proto 的 make target，`protoc-gen-go` / `protoc-gen-go-grpc` 在本机根本没装。也就是说
任何人重新生成一次，输出就可能和仓库里的不一致，而且没有任何 CI 检查能发现。

### 决策

proto 生成链全面钉定，机制照抄 sqlc 的现成模式（`tools/` 钉版本 + `make generate-*` + CI drift guard）：

1. **工具钉在 `tools/go.mod`**：`tool` 指令钉 `buf`、`protoc-gen-go`、`protoc-gen-go-grpc`，
   经 `make generate-proto` 运行，挂入 `make generate`。
2. **用 buf 取代 protoc**：buf 自带编译器（纯 Go），完全不需要系统 protoc。
3. **`buf.gen.yaml` 的插件必须 `local:`，严禁 `remote:`**（理由见下 2）。
4. **CI 单起 `ci-proto.yml`**：drift guard（`make generate-proto` 后 `git status --porcelain`
   非空即失败）+ `buf lint` + **`buf breaking`（对 master 比对）**。

### 理由

1. **为什么不钉 protoc 而改用 buf**：protoc 是 C++ 二进制，各平台各一个包（brew/apt 各自的版本），
   无法放进 `tools/go.mod` 这类纯 Go 的钉定机制——正是「跨平台不可钉」的根源。
   buf 自带编译器、纯 Go，可以像 sqlc 一样进 tools/go.mod 的 tool 指令。
2. **为什么禁止 remote 插件**：`remote: buf.build/...` 的插件版本来自 buf 远程注册中心，
   输入不变也会随注册中心漂移——正是本刀要消灭的东西，用了等于白做。`local:` 插件由
   tools/go.mod 钉定的源码构建，任何平台逐字节一致。
3. **版本对齐有一个 Go 工具链的现实约束**：Go 的 tool 指令没有独立版本钉定，工具版本走模块图
   MVS。buf v1.72 与 protoc-gen-go-grpc v1.6.2 都要求 `google.golang.org/protobuf` ≥ v1.36.11，
   而 protoc-gen-go 报告的版本号来自该模块源码常量（模块 v1.36.11 → 署名 v1.36.11）。
   因此 tools/go.mod 用 `replace google.golang.org/protobuf => google.golang.org/protobuf v1.36.10`
   把模块钉回，protoc-gen-go 署名才与现有生成物一致（v1.36.10）。protoc-gen-go-grpc 钉 v1.6.2、
   buf 钉 v1.72.0，均与生成/运行实测一致。
4. **顺带获得契约守卫**：CLAUDE.md 给 `proto/v1/agent.proto` 定的规矩「只增字段，不改字段编号，
   不删除字段」此前完全靠人自觉。`buf breaking`（ci-proto.yml，对 master 比对）把它变成机器强制
   （已实测：把 `Heartbeat.uptime_seconds` 的编号从 2 改成 7，buf breaking 以
   「field "2" was deleted」报错退出）。
5. **lint 豁免记录**（既有事实，不能改 proto）：`PACKAGE_DIRECTORY_MATCH`（文件在 `proto/v1/`，
   包名 `fileagent.v1`，移文件会改生成物署名与落点）、`RPC_REQUEST_STANDARD_NAME` /
   `RPC_RESPONSE_STANDARD_NAME`（Connect 流的消息名 `AgentMessage` / `ServerMessage`，改名即改契约）。

### 一次性 diff 的性质

buf 不模拟 protoc 版本，重新生成的 diff **只有生成物头部两行 protoc 署名**
（`protoc v7.36.1` → `protoc (unknown)`、`protoc v3.21.12` → `protoc (unknown)`），
语义零变更。protoc-gen-go / protoc-gen-go-grpc 版本行与现有一致（v1.36.10 / v1.6.2），不产生 diff。
验证：生成前后 diff 仅此两行；controlplane 与 agent 两模块 `go build ./...` + `go test ./...` 全绿。

### 否决了什么

- **钉 protoc 本身**：C++ 二进制，跨平台不可钉（理由 1）。
- **buf remote 插件**：版本来自注册中心会漂移，破坏复现性（理由 2）。
- **维持现状**：protoc-gen-go 版本是云端环境残留，重新生成不可复现且无人能发现——现状即缺陷。

### 影响

- 重新生成不再依赖任何本机安装（protoc / protoc-gen-go / protoc-gen-go-grpc 都不需要装）。
- `ci-proto.yml` 的 drift guard 同时是**跨平台一致性证明**：本地 macOS 生成并提交的产物，
  CI ubuntu 上 `make generate-proto` 后 `git status --porcelain` 必须为空（逐字节一致才有）。
- proto 契约的破坏性变更从「口头约定」变为「机器强制」。
- tools/go.mod 里 grpc / genproto 等依赖因 buf 的依赖被 MVS 抬升（patch 级），sqlc 输出
  不受影响——`ci-cp.yml` 的 sqlc drift guard 绿为证。

## D-033：规则同步补「全集」语义——ServerMessage oneof 新增 `rules_sync`（IC-BUG-30）

**决策日期**：2026-09-11
**影响范围**：`proto/v1/agent.proto`、`api/v1/*.pb.go`（仅经 make generate）、
`controlplane/internal/agent/dispatch.go`（SyncRulesOnConnect）、
`controlplane/internal/grpcserver/handler.go`（Connect 的同步错误传播）、
`agent/cmd/agent/main.go`（ServerMessage handler 新分支）
**关联**：IC-BUG-30（docs/tasks/bugs/open.md）、D-030（整桶 policy）、D-032（buf 生成链）、
IC-2b ③（docs/tasks/consistency-ingest.md）

### 背景：同步协议只有增量推送，没有全集语义

`SyncRulesOnConnect` 重连时只推 active 规则，从不推 cancel。agent 断连期间管理员
**停用**或**删除**一条规则，cancel 命令被 `DispatchRuleCancel` 的离线分支直接丢弃
（`dispatch.go` 的 `IsOnline` 短路），agent 重连后对规则的死活一无所知：

- **停用那半**：把 inactive 规则连起来一起推即可修——agent 的 `applyRule` 对
  `Enabled == false` 本来就会先 `stopRule` 再 return，复用既有分支，不需要新消息。
- **删除那半**：**只推 inactive 修不掉**——删掉的行根本不在
  `ListCollectionRulesByAgent` 的结果里，没有任何增量消息可以表达「这条已经没了」。
  D-030 整桶 policy 之后陈旧规则的上传**不会被 403 挡**，无界持续到进程重启。

卡片给出的两个方向：全集语义（proto 只增字段）或 CP 侧 tombstone（软删除表）。
**选全集**：tombstone 需要新表 + 迁移 + 清理策略，而本语义只需要一条消息。

### 决策

1. `ServerMessage` oneof **新增成员 `rules_sync`（字段号 17，现用到 16）**，
   payload 为新 message `RulesSyncCommand`：
   ```proto
   message RulesSyncCommand {
     repeated string rule_ids = 1;
   }
   ```
   纯追加：不动任何既有字段编号、不删任何字段，`buf breaking`（对 master 比对，
   D-032 落地）机器强制。
2. **语义钉死**：`rule_ids` 是**本次同步时 DB 中该 agent 的所有规则 ID**
   （active + inactive 都算），**不含已删除的——「不含已删除」正是语义所在**，
   不是「所有曾经存在过的」。agent 收到后停掉自己手里**不在集合内**的任何规则；
   集合内的规则由紧邻的逐条 push 供给（inactive 的 push 即停用，复用 `applyRule`
   既有分支）。消息在 `SyncRulesOnConnect` **逐条推完全量之后**最后发送。
3. **agent 只停内存句柄，不清理 SQLite `rules` 表**——那张表只写不读
   （`GetRule` 仅测试调用，M-2 扫描已记录），清理它超出本语义范围。
4. **`rules_sync` 送达失败必须让这次连接失败（Connect 返回错误结束 RPC），
   而不是只记日志。** 理由：这条消息承载的是删除半边的**全部**修复——它丢了，
   agent 的规则视图就静默退回修复前的样子，且没有任何信号（正是本 track 判据
   第四条要拦的「改动制造新单点」）。agent 此刻手里的规则视图已不可信，
   **带着不可信的视图继续跑，比断开重来危险**；断开是干净的失败模式——重连
   会走一遍完整 `SyncRulesOnConnect`。只告警则等于把修复押在一条 best-effort
   消息上还假装它可靠。逐条规则 push 的失败仍按 IC-BUG-31 ② 接受 best-effort
   （补偿 = 重连后的再同步），**不**因此断连——逐条 push 失败即断连会让缓冲
   瞬时打满的慢链路 agent 陷入重连循环，让原本无害的瞬时拥塞变得有害。

### 否决了什么

- **CP 侧 tombstone / 软删除表**：需新表 + 迁移 + 清理策略；全集语义一条消息即可，
  proto 纯追加零迁移成本。
- **只推 inactive（不加全集消息）**：修不掉删除那半，删除的行推不出来（本决策的出发点）。
- **`rules_sync` 失败只告警不断连**：见决策 4——制造无信号的静默回退单点。
- **把 41 条压成 1 条的「整体规则快照」消息**（一条消息携带 `repeated CollectionRule`）：
  结构上更稳（不受 SendCh 缓冲时序影响），但改动面大得多（agent 整体替换规则集、
  dry-run 语义要重新钉），且缓冲时序在 ④ 修好消费者顺序后经验证不构成实际失败
  （40 条验收重复跑 ×5）。留作后续形态演化的备选，本刀不采。

### 影响

- 断连期间删除/停用的规则，恢复连接后不重启 agent 即停止采集（IC-BUG-30 的两条半边）。
- 同步一次的消息量 = N 条 push + 1 条 rules_sync（N = 该 agent 的规则总数，
  active + inactive）。N 超过 SendCh 缓冲（32）时的送达依赖消费者并发排空
  （④ 把发送 goroutine 提前），40 条验收为直接证据。
- `ci-proto.yml` 的 `buf breaking` 对本变更为纯追加校验；drift guard 保证生成物与源同步。

**补记（2026-09-11，实现评审后改形：逐条 push + 全集 ID → 快照）**：

初版按本决策落地为「SyncRulesOnConnect 逐条 push（active + inactive）+ 最后一条
rules_sync 只携带 `rule_ids`」。行为级验证立即证伪了该形态：
**burst-vs-drain 是结构性丢失，不是时序问题**——40 条 push + 1 条 rules_sync +
1 条 credentials 共 41 条消息经 32 缓冲的非阻塞 Send 入队，即使发送 goroutine
已提前启动（消费者先于生产者，IC-BUG-31 ④ 修复），40 条的紧循环仍 5/5 确定性失败：
恰好送达 32 条，其余 8 条与凭据被 `select/default` 静默丢弃。生产速率是微秒级
循环，消费是逐条 `stream.Send`（gRPC 帧封装 + 流控），生产恒快于消费；
dev 上「看起来能过」只是 bucket lookup 的 DB 往返偶然让了路。

更糟的是**判据第四条拦到了协调者自己的指令**：硬化 1 的「rules_sync 失败即断连
重连」在「一次同步只发 1 条」前提下是干净的失败模式，但与 41 连发组合即
「>32 规则的 agent 每回合必丢 8 条 + 必断一次」——无限重连循环。

**因此 `RulesSyncCommand` 改为快照形态：`repeated CollectionRule rules = 1`**，
`SyncRulesOnConnect` 不再逐条 push，只发这一条；`PushRuleCommand` 保留为增量
通道（单条 create/update，缓冲无压力）。agent 收到后原子替换规则集：停掉集合
外的一切（`stopRulesOutsideSync`），再逐条 upsert+apply 集合内的
（`Enabled == false` 走 `applyRule` 既有停用分支）。语义不变：快照 = DB 真相
（active + inactive，不含已删除——「不含」正是删除半边的语义）。

两条配套语义：

- **快照必须无洞**：某条规则的 bucket lookup 失败时，**整条同步失败**
  （Connect 结束流、agent 重连重同步），而不是跳过该规则——被省略的规则在 DB
  里存在，快照缺了它 agent 就会停掉一条实际还在的规则。
- **快照体积**：随规则数线性增长（几十条 ≈ KBs）。远超 gRPC 默认 4MB 接收上限
  时整条失败（连接错误、重连、重同步）——**响而不是静默部分丢失**，这是有意选择
  的更好失败模式，但运维需知：规则数极大时快照会整体失败，须拆分 agent 而非调大上限。

（另注：`DispatchRule` / `DispatchRuleCancel` 的单条增量推送仍走同一 32 缓冲。
正常单操作 1–2 条消息无压力，但**经 API 批量改动大量规则时同样的 burst 会重现**——
本刀不修，留给协调者决定是否立卡。）

**补记 2（2026-09-11，深度 review F3：快照超限从「响式失败」改为「降级保活」）**：

上一轮补记写「远超 gRPC 4MB 上限时整条失败（断连重连重同步）——响而不是静默」，
这是**错的**：实测把快照撑到 >4MiB，发送必然失败 → 断连 → 重连 → 再发同一条超限快照
→ 再断——**确定性无限重连循环**，同一输入永不收敛。「响式失败」的前提（失败罕见、
重连能修复）对超限这一类根本不成立。

改为两层：

1. **创建期硬上限**：REST `CreateRule` 校验单 agent 规则数 ≤ `MaxRulesPerAgent`（1000，
   ~1KB/条最坏 ≈ 1MB，远低于 4MiB 上限与下发侧 3MiB 预检线），超限 422
   （`RULE_COUNT_LIMIT`）。让降级状态在结构上不可达。
2. **下发前体积预检（权威闸门，覆盖历史数据/病态模板）**：`SyncRulesOnConnect` 对快照
   做 `proto.Size` 预检，超过 `maxRulesSnapshotBytes`（3MiB，留 gRPC 帧与余量）则
   **跳过发送、ERROR 告警（带 agent_id / 规则数 / 字节数）、连接保持**——agent 保留
   旧规则视图（陈旧但可用）的**稳定降级状态**，绝不以同样输入无限重试。

**否决的备选**：分块下发 + 显式结束标记——需要新协议语义（分片序号/结束位），改动面
大且只为服务一个已可结构性避免的病态状态；发送后失败再退避重试——同一输入必然再失败，
退避只是把循环放慢，不解决不收敛。

**补记 3（2026-09-11，深度 review 三轮 R3：超限闸从条数改为字节 + 降级态可观测）**：

条数闸（1000）拦不住**字节数**——单条含 3MiB TEXT 字段的规则 201 条即越过 4MiB
（实测），并发创建、更新路径与直接写 DB 亦能绕过。改为双层：

1. **创建闸按预估序列化字节**（`MaxRulesSnapshotBytes` 同一条 3MiB 预算）：已有规则 +
   新规则的字符串字段字节和 + 每条 512B 固定开销，超预算 422（`RULE_SNAPSHOT_SIZE_LIMIT`）。
   预估刻意放宽（固定开销偏大），**权威闸门仍是下发侧的精确 `proto.Size` 预检**。
   **结构性残留（立卡）**：并发创建的 TOCTOU、更新路径、直接 DB 写入仍可越过——
   根治需原子计数 / DB 约束 / 全写入路径校验，本刀不做。
2. **降级态可查询**：超限降级从「返回 nil（静默）」改为返回独立哨兵
   `ErrRulesSyncDegraded`；gRPC Connect 据此**保持连接**（断流必然循环）并在缓存打
   `AgentSyncDegradedKey`（24h TTL，成功同步清除），agents API 的
   `agentResponse.rule_sync_degraded` 直接可读——不再只有一条 ERROR 日志。

---

## D-034：路径模板保留字 `time` 改名 `submit_time`，注入优先级与系统变量对齐（IC-BUG-50）

**决策日期**：2026-09-13
**影响范围**：`pkg/trollsift`（新增 `InjectSubmitTime` / `UsesDeprecatedTimeField`）、
`agent/cmd/agent/main.go`（`buildStoragePath` / `handleDryRun`）、
`controlplane/internal/api/handler/agents.go`（创建/更新的 deprecation 提示）、
`webui/src/utils/pathTemplate.ts`、`webui/src/pages/Agents/RuleForm.tsx`（默认模板）、
`docs/design/contracts.md` V-3（契约对齐）
**关联**：IC-BUG-50（docs/tasks/bugs/open.md）、D-010（引入 trollsift）、V-3（路径模板契约）

### 背景与三条根因（IC-BUG-50）

1. **`time` 是未声明的保留字**。`buildStoragePath` 里硬编码注入 `fields["time"]`，
   但 `contracts.md` V-3 的系统变量表与 webui 的 `SYSTEM_TEMPLATE_VARIABLES` 都没有它
   ——三端契约只有两端知道它存在。
2. **优先级与同体系变量相反**。`InjectContext` 对 `agent_name`/`agent_id` 是
   **解析结果优先、不覆盖**（`TestInjectContext_NoOverwrite` 钉住），而 `time`
   是 `strings.Contains(DestPathTemplate, "{time")` 命中即**无条件覆盖**解析结果。
   管理员把解析字段命名为 `time`（很自然）想按**数据日期**归档时，会静默拿到
   **采集时刻**——归档到错误日期且无任何提示（D-030 整桶 policy 下连 403 都没有）。
3. **名字误导**。它不是「当前时刻」语义，而是「**该文件被提交上传的那一刻**」
   （`submitFile` 时刻的 `time.Now().UTC()`）；叫 `time` 让人以为是通用时间变量。

且 `strings.Contains(template, "{time")` 是字面前缀匹配，对 `{time_zone}`
这类前缀相同的字段名会误触发注入（`submit_time` 系列同样存在，改名后一并消除）。

### 决策

1. **改名**：保留字 `{time}` → **`{submit_time}`**。语义 = **该文件被提交上传的
   时刻**（`submitFile` 时刻，UTC），与 `agent_name`/`filename` 同为小写下划线名词，
   且与代码自身词汇（`submitFile`）一致。否决 `upload_time`（歧义为 PUT 完成时刻）、
   `now`（查询时刻歧义，任务明令禁用）。
2. **优先级对齐**：与 `agent_name`/`agent_id` 一致改为**解析结果优先**——
   `path_pattern` 解析出同名字段就用解析值，**没有才注入**上传时刻。
   同一体系里不允许两套相反的优先级规则；「想要上传时刻」的用法在解析字段
   不同名时照样成立。权威注入点收敛为 `pkg/trollsift.InjectSubmitTime`
   （`buildStoragePath` 与 `handleDryRun` 共用），不再做模板字符串前缀匹配
   ——无条件注入-if-absent 对 Compose 无副作用（未引用的字段不参与合成），
   从结构上消灭 `{time_zone}` 前缀误伤。
3. **存量兼容**：**同时接受旧名 `{time}` 与 `{submit_time}`**，渲染语义完全等价
   （同样解析结果优先）。**不选一次性数据迁移**：模板存在 `rules.dest_path_template`
   里，迁移要扫全表改写文本且 REST 建的规则无形状约束、无法保证替换不破坏 LDML
   段；而 agent 侧接受旧名的成本是两行 inject-if-absent，风险更低。旧名标记为
   **deprecated（未移除，无移除时间表）**：CP 在创建/更新规则时对含旧名的模板
   返回可读的 `warnings` 提示并记 Warn 日志。
4. **行为变更声明（对存量规则）**：仅当一条规则**同时**满足「`path_pattern`
   解析出名为 `time` 的字段」且「`dest_path_template` 引用 `{time}`」时，
   渲染结果从「上传时刻」变为「解析值」——这正是缺陷本身，属修复而非破坏；
   其余存量规则（模板含 `{time}` 但无同名字段）渲染结果逐字节不变。
   UI 默认模板改用 `{submit_time:yyyy/MM/dd}`，新建规则不再产生旧名。

### 落地约束

- `pkg/trollsift` 为权威（`InjectSubmitTime` / `UsesDeprecatedTimeField`），
  webui `pathTemplate.ts` 镜像同步（V-3 既有约定）。
- `contracts.md` V-3 同步：系统变量表补 `submit_time` + 旧名 deprecation 状态、
  新增优先级说明（`InjectContext` 与 `buildStoragePath` 两个注入点）、
  时间符号表补 `mm`=分 / `MM`=月及「大小写写错在上传时才失败（`month out of range`）」。
- 测试钉住：解析优先（含旧名等价、`time_zone` 不被误伤）、新名注入兜底、
  CP 侧 deprecation 提示。

**补记 1（2026-09-13，PR #106 review：别名镜像、裸形式禁令、filename/ext 统一优先级）**：

首版实现被 review 实测抓出三处语义漏洞，修正如下：

1. **别名必须是互为镜像，不是两个独立字段**。首版 `InjectSubmitTime` 对
   `submit_time` 与 `time` 各判各的 inject-if-absent——存量规则解析出 `time`
   （数据日期）、管理员照 deprecation 提示把 dest 的 `{time}` 改成 `{submit_time}`
   后，`submit_time` 从未被解析 → 注入提交时刻 → **照迁移建议做反而落错位置**。
   修正：单侧已解析时**镜像给缺失的别名**（解析出 `time` 则 `submit_time` 取同值，
   反之亦然）；**双侧都被解析时各用各的、不互相覆盖**——Compose 只读模板引用的
   字段，两个独立的解析结果强制镜像反而会制造意外值；此规则写进 `InjectSubmitTime`
   注释与 V-3，并以交叉别名矩阵测试（4 种输入 × 2 种 dest 引用）逐格钉住。
2. **裸形式禁令（选 b）**：`{submit_time}` / `{time}` **不带 LDML 格式**时，Go 侧把
   无格式字段判为 string 类型（`field.go`），注入的 Time 值必然 compose 失败
   （实测 `expects a string value`）——而首版 UI 清单与契约恰以裸形式宣传，管理员
   照抄即得一个必失败模板。**不选默认序列化（(a)）**：对象键里没有无歧义的默认时间
   格式（RFC3339 带 `:` 冒号），任何默认值都是任意拍板，且会连带改变 parse 侧语义。
   修正：**禁止裸形式**——webui `validatePathTemplate` 拦截并给出带格式示例、
   UI 变量清单改以带格式形式展示（`{submit_time:yyyy/MM/dd}`）、契约写明禁令、
   CP warnings 对裸形式给出可读提示。**弃用提示只进给人看的提示区，不进对象键**
   （首版把 `(deprecated)` 拼进了预览键，已移除）。
3. **filename/ext 统一解析优先**。契约原文宣称「全部系统变量解析优先」，但
   `injectUploadFields` 对 `filename`/`ext` 仍无条件覆盖（实测：解析出 `ext=csv`
   的 `/data/csv/report.bin` 合成 `bin/report.bin`）——契约声称与实现不符
   （本 track 第 11 次）。**选统一 absent-only 而非收窄措辞**：同一体系一套优先级
   规则可让契约逐字成立、不再需要例外清单；行为变更面极小（仅当 path_pattern
   显式命名 `filename`/`ext` 且捕获值 ≠ 本地 basename/ext 时，键以解析值为准——
   那正是管理员的显式意图）。

**补记 2（2026-09-13，PR #106 二轮 review B1/B2：裸保留字 agent 侧无条件拒绝、预览镜像入契约）**：

1. **裸保留时间字段无条件拒绝（B1）**。补记 1 的裸形式禁令只覆盖了
   「裸字段 + 注入的 Time 值」——实测 `path_pattern: 'of/{submit_time:s}/…'`
   解析出 string 值后，裸模板 `{submit_time}/…` **照样合成成功**：三端口径矛盾
   （webui 拦、CP 警、agent 跑通），且保留字被挪用为任意字符串。修正：
   **agent 侧无条件拒绝**——新增 `pkg/trollsift.ValidateReservedTimeUse`，
   在 `buildStoragePath` 与 `handleDryRun` 共用的 gate 处统一校验（IC-BUG-21
   语义：任务失败、可读错误、绝不猜键）。
   **判断：parse 侧一并拒绝**（保留字声明为非时间字段也拒，如 `{submit_time:s}`；
   时间类型 `{submit_time:yyyy/MM}` 仍合法——那是文档支持的数据日期归档）。
   理由：保留字的意义是名字唯一绑定语义；让 path_pattern 拿 `submit_time` 捕获
   任意字符串，V-3 对「`submit_time` = 该文件被提交上传的时刻」的承诺在该规则上
   直接为假，且镜像规则会把错误值传播到另一个别名。

   **存量代价（四轮 review C3/D3 修正——穷尽全部组合并区分两类性质）**：

   **原先能跑 → 现在失败（真实行为变更，共两行）**：

   | 组合 | 禁令前 | 禁令后 | 实测证据 |
   |------|--------|--------|----------|
   | pattern 把保留字解析成 **string/int**（如 `{submit_time:s}`），dest **裸/typed 引用**（`{submit_time}`、`{submit_time:s}`） | **能合成** | **拒绝**（任务失败） | `HELLO/a.csv` |
   | pattern 把保留字解析成 **string/int**，dest **不引用**（保留字摆设） | 能跑 | **拒绝** | — |

   **失败 → 失败（仅错误更可读，含改法；非行为变更，共三行）**：

   | 组合 | 禁令前 | 禁令后 |
   |------|--------|--------|
   | pattern 把保留字解析成 **时间类型**（`{time:yyyy}`），dest **裸/typed 引用** | 失败（Time 值 + string 类型字段：`expects a string value`） | 拒绝（同失败，错误含 LDML 改法） |
   | pattern 把保留字解析成 **非时间类型**，dest **时间类型引用**（`{time:yyyy}`） | 失败（string 值 + 时间类型字段：`expects a time value`） | 拒绝（同失败，错误含改法） |
   | dest 裸/typed 引用，**无解析** | 失败（同上类型不符） | 拒绝（同失败，错误含改法） |

   这两类规则的 path_pattern 本身就把保留字用成了普通字段，属配置语义错位；
   fail-fast（Warn 可读、指明在 path_pattern 里改字段名）优于继续静默跑。
   保留字**时间类型**形态（pattern/dest `{time:yyyy/MM}` 数据日期归档）不受影响。

   （**pattern 语法错误**（`New()` 失败）不在矩阵内——它与保留字无关，且 agent
   对它的处理路径不经过 buildStoragePath：正常 watch/cron 路径由 `matchGlob`
   先行 `New()`，失败即 **Warn 跳过该文件**（`agent: match path failed`），
   dry-run 显式返回错误；「静默当无字段 pattern」仅在 buildStoragePath 内部成立、
   而那里只在文件已匹配后才被调用。禁令前后行为一致（Warn 跳过/显式错误），
   非本刀行为变更；本刀起 CP 在创建/更新时以 warnings 提示，见补记 3 第 1 条。）
2. **预览镜像写进契约（B2）**。前端 `renderPathPreview` 已实现 dynamicFields
   优先但未实现别名镜像——预览与真实合成不一致，正是 P2-C 要消灭的问题换了入口。
   修正：前端实现与 `InjectSubmitTime` 同款镜像规则（单侧解析→镜像给缺失别名；
   双侧→各用各的；都没解析→当前时间），交叉预览测试钉住。**教训落进 V-3**：
   Go 与 `pathTemplate.ts` 的手工双维护是已记录的脆弱点，本次再次咬人——
   别名镜像与裸形式禁令必须写成**三端共享约定**（契约正文），不能只活在 Go 注释里。

**补记 3（2026-09-13，PR #106 三/四轮 review：时区权威校验收窄到 Go 端、dest 提示动词、代价矩阵穷尽）**：

1. **时区有效性不做 TS 镜像，权威校验收归 Go 端（四轮 D1，选 b）**。
   实测跨端不等价：`{time:yyyy|tz=Nope/Bad}`、`{time:yyyy|tz=Asia/Shanghai }`
   （尾空格）、`{time:yyyy|tz=UTC|tz=UTC}`（重复 tz）在 Go `New()` 全部报
   `invalid timezone`，而 TS 只查 `tz=` 非空、三者放行——管理员 UI 保存成功、
   上传时才失败。**不选 (a) TS 补时区校验**：浏览器没有权威 IANA 数据源
   （`Intl.supportedValuesOf('timeZone')` 可用性依环境、集合与 Go tzdata 不重合），
   「重复 tz」「尾空格」需要逐字镜像 `strings.Index(spec, "|tz=")` 切分逻辑——
   正是本契约已三次咬人的「双实现必然漂移」模式。**选 (b)**：TS 声明收窄为
   kind + 基础语法；**CP 创建/更新时用 Go `New()` 对两字段做完整校验**（与 agent
   同库同判定），失败进 `warnings`（不 422——模板形状约束已由 D-030 第八条否决，
   需要拒绝须另立决策）——「UI 放行、保存成功」时错误即在保存响应可见，
   不再等到上传。`ValidateReservedTimeUse` 保持只管 kind（正确，未改）。
2. **dest 提示动词修正（四轮 D2）**：C2 的字段插值让 dest 的 deprecated 提示
   也带上 "parses"——`dest_path_template` 只合成、不解析，文案误导。
   两字段文案分别成立：path_pattern 用「解析出同名字段」，dest 用「渲染为提交
   时刻，除非 path_pattern 解析出同名字段」（解析优先是全局规则，条件在 pattern 侧）。
3. **存量代价矩阵穷尽（四轮 D3 / 五轮 E2 修正）**：三轮修正的四行矩阵仍漏两组
   「失败→失败」组合（pattern=time 解析 + dest 裸/typed 引用；
   pattern=非时间解析 + dest=time 引用），已补入并**显式区分**「原先能跑→现在失败」
   （真实行为变更，**2 行**）与「失败→失败」（仅错误更可读，**3 行**）——
   五轮修正：正文矩阵初版误写「共四行」且把「pattern 语法错误」行混入并以
   错误理由（「静默当无字段 pattern」）佐证「行为未变」；实测该理由不成立——
   正常 watch/cron 路径 `matchGlob` 先行 `New()`，失败即 Warn 跳过、dry-run
   显式报错，「静默」仅在 buildStoragePath 内部成立而那里根本走不到。该行
   与保留字无关、禁令前后行为一致，已移出矩阵并如实注明。

**补记 4（2026-09-13，PR #106 五/六轮 review：warnings 的最后一米 + 测试基建保真）**：

1. **CP 的 warnings 此前到不了任何人眼前（五轮 E1）**。三/四轮把时区权威校验收归 CP 并
   以 `warnings` 返回，但 webui 的 `CollectionRule` 类型里**根本没有 `warnings` 字段**，
   响应体到了前端即被丢弃——「保存成功时错误即可见」这个设计承诺在实现上为空。
   同时 `updateRuleStatus`（status-only PUT）**绕过了 `respondRule`**，激活一条模板
   有问题的规则静默返回无提示。修正：类型补 `warnings?: string[]`；
   `updateRuleStatus` 改走 `respondRule` 与创建/全量更新同一出口；
   UI 选**表单顶部可关闭警告区块**而非 toast，且**有 warnings 时保存成功但不自动跳转**
   ——提示是多行散文（弃用依据、具体非法时区名、LDML 修法示例），toast 一闪而过读不完，
   而常规跳转目标（规则列表）不返回 warnings，跳过去就永远看不到。
2. **「共用一处渲染分支」不等于被覆盖（六轮）**。warnings 的**判定**在 create/update
   两分支各写了一份，测试只钉住 update 那份：实测把 create 分支的整段处理删掉，
   全部用例仍绿——create 路径当时没有任何保护。修正不是补一个 create 测试，而是
   **合并为单一出口**，使 create/update 字面共用同一行代码，覆盖论证由「声称」变为「成立」。
3. **mock 比真实 DB 宽松是一类缺陷，不是一个缺陷（六轮）**。`mockAgentsDB` 的
   `CreateCollectionRule` 丢掉 `cron_expr`/`run_once_on_start`/`append_mode`，
   `UpdateCollectionRuleStatus` 只挑回 `path_pattern`/`dest_path_template`——这类失真
   让「响应体缺字段」的缺陷静默通过且测试永远绿。改为**原样回显全部入参 / 整行拷贝
   存储行后只覆盖本语句真正改动的字段**，消掉这一类而非再补一个字段。

### 落地记录

**PR #106**（squash `e444d27`，2026-09-13 合并）。六轮 review，发现的严重度单调下降：
① 数据落错位置（注入优先级）→ ② 禁令只禁一半 → ③ 三端口径矛盾 → ④ 文本与边界 →
⑤ 可见性（CP 的 warnings 到不了 UI）→ ⑥ 测试债（实现已正确）。第六轮首次出现
「实现本身没有错误」，据此判定收敛并合并。**11 条守卫经变异确认**；
跨端 13 个 kind 形态 Go↔TS **13/13 一致**；CI 3/3 全绿。

> **这一刀最值得留给后人的一条**：`{time}` 这个保留字在代码里活了很久，
> 三端契约只有 agent 一端知道它存在——**它不是被测试发现的，是在讨论 tail 设计时
> 顺手读 `pkg/trollsift/parser_test.go` 撞见的**。隐性契约不会让任何用例变红。

---

## D-035：采集防抖改为普适（overwrite 也防抖，close_wait 降为别名）

**决策日期**：2026-09-25
**影响范围**：`agent/internal/watcher/watcher.go`（三个分流点 + 命名中立化）、
`agent/internal/watcher/watcher_test.go` / `fsnotify_burst_test.go` / `overflow_linux_test.go`（实时路径守卫改挂 tail）、
`deploy/scripts/smoke.sh`（新增 16MB 分块写护栏）、
`docs/design/contracts.md` V-3、`docs/design/system-design.md`（§4.4.3 / 附录 A / 附录 D）、
`agent/internal/queue/queue.go`（`AppendModeCloseWait` 别名注释）、`webui/src/pages/Agents/RuleForm.tsx`（选项文案）
**关联**：AUD-9（A 基线审计 §3「不挡 A 但强烈建议随 A 一起修」）、`docs/tasks/active.md`「下一步」第 1 条（2026-09-24 拍板）、
IC-BUG-53（seen 语义的既有权衡）、IC-BUG-46 / IC-15（tail fail-closed 与正确实现）

### 背景（实证）

A 基线审计实测：把一个 150MB 文件 `cp` 进被监听目录（默认 `append_mode=overwrite`），
`upload_logs` 150 行，其中 147 行是完整 157286400 字节，**3 行读到正在写入的半个文件**；
MinIO `CompleteMultipartUpload` 事件 150 次，实际写入流量 ≈ 22GB。同一文件改用
`close_wait`：`upload_logs` 1 行。

根因：watcher 只有 `close_wait` 走 500ms 空闲防抖（`runCloseWait`）；默认的 `overwrite`
走 `runFsnotify`，每个 Create/Write 事件直接触发一次整文件上传。

### 决策

1. **防抖普适，而不是只翻默认值**。新增谓词 `debounceEnabled()`（=「模式 ≠ tail」），
   watcher 的三个分流点（`Start` 选事件循环、`pollScan` 跳过热文件、`recheckAfterDebounce`
   静默判据）全部改用它。**「不防抖」没有任何正当用途**：只翻默认值等于把枪留在桌上，
   显式选 `overwrite` 的人照样中招。
2. **`close_wait` 降为 `overwrite` 的别名**。已验证 `close_wait` 严格等于
   `overwrite` + 500ms 防抖，下游（executor/uploader/CP）从不按这两个模式分流；
   防抖普适后 watcher 侧对两者也完全一致。**契约值域不变**（三个值保留、不改 proto、
   不加迁移），存量规则与文档不破；等价性由可执行断言钉住
   （`TestDebounced_CloseWaitAndOverwrite_EquivalentDelivery`）。
3. **`tail` 明确排除，且是故意的**。tail 已被 IC-BUG-46 fail-closed 挡掉
   （CP 建规则 422 + executor 拒任务），其事件语义（增量 + 断点续传）归 IC-15。
   本刀不改 tail 的事件循环；`runFsnotify` / `loopFsnotify` 因此成为「只有 tail 才会走」
   的路径——**保留不删**（IC-15 的地基 + 实时路径语义的守卫测试都在那里），并在注释里写明。
4. **防抖窗口仍是代码常量 500ms**，不引入新的配置项/环境变量。

### 权衡 / 已知副作用（review 必问，明写不藏）

#### 其一：`seen` 语义——`overwrite` 失去「溢出重扫兜底重试」

`overwrite` 从实时循环挪到防抖循环，**顺带改变了它的 `seen` 语义**：

- 实时路径（`loopFsnotify`）**故意不写 `seen`**（PR #108 review F1→P1 的裁决）：
  `seen` 意为「已交付」，而 emit 成功只证明事件进了内存 channel，下游 submit 仍可能静默失败
  （IC-BUG-53）；不写 `seen` 给「溢出重扫」保留了**唯一一次重试机会**。
- 防抖循环的 flush 走 `claimDelivery` / `completeDelivery`，**交付即记 `seen`**
  （F3 恰一次仲裁依赖它）。

所以 `overwrite` 从此**失去**「下游静默失败后由溢出重扫兜底重试」这一条路径，换来的是
「不再有写放大、不再上传写了一半的文件」。这与 `close_wait` 早已接受的权衡完全相同
（见 `docs/tasks/bugs/open.md` 的 IC-BUG-53）。该权衡已写在 `watcher.go` 的 flush 注释与
`loopFsnotify` 注释里，两种语义各有可执行守卫
（实时路径：`TestLoopFsnotify_RescanRetriesRealTimeDeliveredFiles`，挂 tail）。

#### 其二：短命文件（最后写入后一个防抖窗口内被删除/改名）不再被采集

**行为对照**：`loopDebounced` 收到 Remove/Rename 会取消该路径的 pending 防抖
（`Stop` + 且回收条目）并只发一个 `remove` 事件，而 agent 对 `remove` 不上传任何内容。
于是相对改动前的 `overwrite`：

| 场景 | 改动前（实时逐事件整传） | 改动后（普适防抖） |
|------|--------------------------|--------------------|
| 写临时文件 → rename 成最终名 | **临时文件也被整份传上去**（写放大与截断上传的一部分），最终名文件再传一次 | 临时文件的 pending 被 rename 取消，**只有最终文件被采集** |
| 最后一次写入后 `< 500ms` 文件被删除 | 与删除**竞态**，可能抢先把内容传上去 | **内容不再被采集**（只发 remove） |

**裁决（协调者，2026-09-25）：可接受且符合预期，不改代码，但必须作为决定被记录**。理由：

1. 真实世界最常见的写入模式是「写临时文件 → rename 成最终名」。对这个主流模式，
   新行为**严格更好**：改动前临时文件本身会被整份上传（正是本刀要消灭的写放大与
   截断上传的一部分），改动后只有最终文件被采集。
2. 剩下的丢失场景是「文件最后一次写入后 500ms 内就被删除，而我们本来想采它」。此时文件
   **已经不存在了**——改动前能传上去，纯属「抢在删除之前读到了」，本身是不可靠的竞态，
   不是可依赖的语义；没有任何采集承诺建立在它之上。
3. 因此这不是「多了 500ms 延迟」这么轻描淡写，而是一条**语义变化**：短命文件不再采集。
   它是一个被记录的决定，不是一个事故。

可执行守卫：`TestDebounced_RemoveWithinWindow_CancellesDelivery`（overwrite 模式下，
Write 后窗口内 Remove ⇒ **只**收到 `remove`，窗口过后也没有内容事件）。将来有人改了这个
行为，CI 会告诉他他改的是一个决定。

#### 其三：防抖 pending 表的有界性（第三轮返工 T-1 定稿；前两版结论均被实测推翻）

防抖事件循环为每个「出现过的唯一路径」持有条目（map key + timer + 闭包），必须回答
「什么时候回收」：

- **被否决的第一版**（第一轮返工 R-1）：flush 后经固定容量通道发回收消息，满了就丢——
  被丢弃的路径若此后再无事件，就永远没有「下一次 firing」，条目**永久滞留**（codex 确定性
  复现：256 路径残留 192 条）。总量无上界。
- **被否决的第二版**（第二轮返工 S-1）：每次扫描后把阈值设为 `2×len+64`。**有界性声明
  被第三轮复审实测推翻**：阈值记录的是「扫描那一刻」的 active，而这批条目随后全部转为
  idle；阈值只会抬高、永不回落，于是「等 idle 归零 → 注入刚好跨阈值的突发」逐轮递推
  `A_{k+1} = A_k + 65`，codex 五轮实测残留 66/131/196/261/326——idle 残留**无界**。
  根因是阈值设计本身（协调者给出），不在实现。
- **定稿机制（T-1）**：`fired` 标记保留（flush 完成后置位、Reset 时清除），触发条件换成
  **按事件计费**——每处理一个 fsnotify 事件计数 +1，每 `sweepEvery`（64）个事件扫描一次
  （删除全部 fired 条目）并把计数归零。**触发条件不含任何历史派生量，不存在自举**：
  过去的 active 峰值不会放大未来的扫描预算。
- **界（如实陈述，不是「不会泄漏」）**：一次扫描只保留它观察到的仍是 active 的条目——
  扫描后 `len ≤ 扫描开始时观察到的 active 数`（**不是等号**：扫描逐项读 `fired` 期间，
  某条目可能刚被观察为未 fired 而保留、其 timer 回调随即把它置为 idle，扫描返回时它
  已 idle 但仍在表里）；到下一次扫描前最多再处理 `sweepEvery` 个事件（每个至多新增一个
  条目），故恒有 `len ≤ max_active + sweepEvery`。
- **摊还成本（如实陈述）**：每 `sweepEvery` 个事件做一次 O(len) 扫描，即每事件
  `O(len/sweepEvery)`，而 len 本身被 `max_active + sweepEvery` 限住。早前「几何阈值
  O(1) 摊还」的说法属于被推翻的第二版，不再成立为完整论证。
- **突发后事件彻底停止 ⇒ 残留停在该突发水位**（没有事件就没有扫描）——这条依然成立，
  是**被那次突发限住的有界残留**，与上面的无界自举是两回事。
- **多轮守卫**（前两版都溜过去正是因为只有单轮测试）：
  `TestDebounced_PendingSweep_MultiRound_ResidueConstant`——6 轮，每轮等全部保留者转为
  idle 后注入恰好一个预算（8）的新突发，并确定性地让本轮第一条在 sweep 前已 fired；
  断言每轮残留 `≤ 8` 且跨轮不增长（本场景实际为 7），而不是把上界误钉成等号。
  变异「把 `LessOrEqual` 改回 `Equal`」稳定变红。

#### 其四：初始扫描把未来 mtime 视为已静默

普适防抖让默认 `overwrite` 的初始扫描也走「热文件跳过」判据；若文件 mtime 因跨机时钟
偏差或 `rsync -t` 落在未来，把负 age 当成「仍在窗口内」会将首次采集推迟到该未来时刻。
因此扫描只跳过 `0 ≤ age < debounceWindow` 的文件，负 age 视为已静默并立即交付。

代价必须如实记录：未来 mtime 的文件现在会被采集一次，并把那个未来时间戳写进 `seen`；
此后偏移窗口内发生的真实修改通常带着较早的“当前”mtime，会被 mtime 单调判重静默跳过，
所以行为从修复前的「压根采不到」变为「先采一次，之后在时钟偏移窗口内盲」。不能把写入
`seen` 的值钳到 `now`，否则同一个未来 mtime 会在后续扫描中反复被判为更新并重复交付，
把漏采改造成写放大。

#### 其五：recheck timer 以生命周期门闩收口

`flush` 现在运行在防抖 timer goroutine 上；它可能越过 pending timer 的 `Stop()`，并在
`Start` 的 `stopAllRechecks` 已经清空 timer 后才发现文件仍热、试图重新安装 recheck。
若放行，这个过期 timer 会携带旧 `ctx` / `seen` 污染顺序复用后的新生命周期。因此调度有
两道栅栏：`rechecksStopped` 在 `stopAllRechecks` 的同一把 `seenMu` 下先关门再清表；
`ctx.Err()` 则在下一轮重新开门后继续拒绝上一轮已取消 context 的迟到 callback。

`startRechecks` 与配对的 `stopAllRechecks` 放在 `Start` 最外层是**防御性加固，不产生当前可观察
行为变化**：此前 fsnotify 成功路径已经在初始扫描前调用 `startRechecks`；而两个 polling
fallback 虽然调用 `pollScan`，却丢弃返回的 skipped paths，只靠下一轮 ticker 重扫，从不安排
recheck，`handleWatchError` 在该路径也不可达。真正承载顺序复用语义的不变量是“每个 fsnotify
生命周期必须调用 `startRechecks`”，对应回归测试钉住调用存在；它不声称钉住调用的具体位置。

同样不存在上一版记录的“ctx 仍存活但 fsnotify channel 先关闭，迟到 flush 因门闩漏采”代价：
fsnotify v1.8.0 的生产后端只在 `Close()` 驱动 read loop 退出时关闭 `Events` / `Errors`，而
`fw.Close()` 是 `Start` 在 loop 返回之后才执行的 defer。`loopDebounced` 的 `ok == false` 分支
仅服务于包内测试注入 channel 的防御处理，生产中的 live loop 观察不到该关闭顺序。

### 理由（为什么不「只翻默认值」）

- 默认值只是「新建规则不选时的兜底」；显式配了 `overwrite` 的存量规则在翻默认值后
  **原样保留写放大**。防抖没有「用户想要每事件整传」的合理场景——那正是审计实测的
  22GB 事故本身。
- 普适之后模式语义收敛为二值：「tail（实时增量，当前停用）」与「其余一切（防抖整传）」，
  `close_wait` 之名不再承载行为差异，只承载兼容。

### 影响面

- **行为**：`overwrite` 的用户可见行为 = 原 `close_wait`（防抖 500ms 后整文件上传一次）。
  初扫对仍在写的文件改为跳过 + recheck 收走；轮询 fallback 下一个 tick 重查。
- **不变**：契约值域三值不变；CP 侧 `append_mode` 校验（tail 422 fail-closed）不动；
  executor / uploader 不动；防抖窗口常量 500ms 不动。
- **测试归属调整**：实时路径守卫（溢出重扫重试、emitBlocking 背压、inotify overflow
  真实内核测试、burst 不丢事件）全部改挂 `tail` 模式——防抖普适后只有 tail 还走实时循环，
  这些守卫钉的是循环本身，不是某个模式。

### 落地记录

**PR #118**（落地进度另见 `docs/tasks/active.md`「下一步」第 1 条）。
红→绿 live 证据（AUD-9 新护栏，16MB 文件 `cp` 直写监听目录，断言 `upload_logs` 恰好 1 行）：
**master（未改 watcher）红：6 行 → 本分支绿：1 行，且 `file_entries.sha256` 与源文件一致**
（12 环全绿，`SMOKE PASSED`）。master 上 6 行即写放大回归被护栏抓住——16MB 的 `cp`
在本地盘上只产生 6 个合并后的 Write 事件，每个事件都触发了一次 16MB 完整上传
（审计里 150MB 文件对应 150 次，同一机制、同一护栏）。6 条新增单测 + 5 条变异测试
全部确认守卫有效；`agent` 覆盖率 75.4% → 75.5%（watcher 91.5% → 92.4%）。

---

## D-036：MinIO 镜像改为自持私有镜像仓 + 按 digest 钉定（上游已无公共通路）

**决策日期**：2026-09-25
**影响范围**：`deploy/docker-compose.{dev,test,prod}.yml`、`.github/workflows/ci-smoke.yml`、
`docs/ops/deployment.md`（新增 §0.1 + A.3 引用）、`docs/ops/operations.md`、
`README.md`（前置依赖）、`docs/tasks/active.md`（「下一步」第 4 条紧迫性）
**关联**：G-A2（A 基线审计：MinIO 镜像从 Docker Hub 下架，PR #114 换 `quay.io`）、
`active.md`「下一步」第 4 条（存储层替代调研）

### 背景：最后一条公共通路也关了

G-A2 当时的结论是「registry 必须是 quay.io，不是 Docker Hub」，`active.md` 也写着
「**quay.io 是最后一条公共通路**」。**这句话已经过期**：2026-09-25 CI 实跑报

```
Unable to find image 'quay.io/minio/minio:RELEASE.2025-04-22T22-12-26Z' locally
docker: Error response from daemon: unauthorized: access to the requested resource is not authorized
Error: Process completed with exit code 125.
```

逐项实测（2026-09-25）：

| 来源 | 结果 |
|------|------|
| `quay.io/minio/minio` | 匿名 token **能签发**，但取 manifest **401**；`quay.io/api/v1/repository/minio/minio` 返回 `{"detail":"Requires authentication"}` |
| `docker.io/minio/minio`、`docker.io/minio/mc` | **401** |
| `ghcr.io/minio/minio`、`ghcr.io/minio/mc` | **403** |
| `docker.io/bitnami/minio` | **404**（Bitnami 也撤了 legacy 镜像） |
| `dl.min.io` 的 `mc` 下载 | 早已只返回一段公告文本（PR #114 已记录） |

即**没有任何公共通路**能拉到这个镜像。有本地缓存的机器察觉不到；任何全新环境、
全新 worktree 或 CI runner 都直接起不来。这不是限流，是仓库不再公开可读。

同时断掉的还有 **`mc`**：`ci-smoke.yml` 与 `docs/ops/deployment.md` 现在都靠
「从服务端镜像里抠 `/usr/bin/mc`」拿它，而这个办法的前提正是能拉到镜像。

### 决策

1. **自持镜像**：把我们手上仅存的那一份推到**自己控制的镜像仓**
   `ghcr.io/byw-dev/minio`，三个 compose 与 CI 全部改指这里。
2. **按 digest 钉，不按 tag**。镜像仓是我们自己的，tag 可被重写，digest 不可以：

   ```
   ghcr.io/byw-dev/minio@sha256:a66e1fd7e5cc10cbbc4d5a24bb4b81ae3a17b4000db6535e450c0efbdc447fee
     ├─ linux/amd64  sha256:a2fe4b45cd4dfab1a1e4e55c0ee425b8c96c17e989c523447c71967444f1c36f
     └─ linux/arm64  sha256:4bfdccb8f63715c3f770bbb4dbce51257ff2c48621f015df61072bbca779d1ad
   ```

3. **必须是多架构 manifest list**。这一条差点被漏掉：本机（Apple Silicon）缓存的
   `quay.io/minio/minio` 是 **`linux/arm64`**，而 GitHub runner 是 **`linux/amd64`**。
   只推 arm64 会让 CI 拉到一个跑不起来的镜像，且症状极难读。所幸两个架构本机都在
   （amd64 那份来自 Docker Hub 的历史缓存），已分别验证**都带 `/usr/bin/mc`**
   （amd64 是 x86-64 ELF、arm64 是 aarch64 ELF，`Created` 均为 `2025-04-22T22:35:01Z`）。
4. **私有 package，不公开**。公开唯一换来的是「org 外的人能匿名 pull」，而本项目
   完全私有化部署、当前无外部贡献者、CI 在同一 org 内（`GITHUB_TOKEN` 足够）——
   收益近于零。代价则是实打实的：公开分发 MinIO 二进制是**最可能引来 takedown** 的做法，
   而 takedown 会让 CI、三个 compose 与所有开发机**同时**断（与今天 quay 401 的症状一致），
   等于把单点依赖换到另一个同样能被第三方摘掉的地方；此外还要承担被第三方当上游引用后的
   兼容责任与出网流量。**否决公开**。
   （相应地，「改名避开 MinIO 商标」这条建议也随之作废——商标风险主要来自公开分发，
   私有包没有这个暴露面，而改名会牺牲运维可读性。包名就叫 `minio`。）
5. **源码自持，且 package 关联到源码仓而不是本仓库**。
   `byw-dev/minio` 是 MinIO 官方源码的 fork（已同步全部 tag，含我们钉定的
   `RELEASE.2025-04-22T22-12-26Z`，commit `0d7408f`；该 tag 与 `master` 的 `LICENSE`
   均为 AGPL-3.0）。**它是公开的**——fork 只能与上游保持一致的可见性。
   **本项目把它选择为对外提供对应源码的途径**；该方案（以及下文的拆分方式）是否满足
   AGPLv3 §6 的全部要求——§6 按 conveyance 方式列了 6(a)–(e) 多条路径，
   且「Corresponding Source」的定义还包括控制生成、安装、运行所需的脚本——
   **属法律判断，待法务确认，本决策只记录技术事实与选择，不构成合规结论**。
   若把它改成私有（删 fork、本地 clone 后 push 进私有 repo），「公开 fork」这条途径
   即不复存在，届时需要另行设计并落实对应的提供方式（例如逐客户随交付附源码归档包）；
   不同方案的成本与充分性比较同样待法务确认。
   **结论：二进制私有、源码公开，是本项目当前选择的拆分（充分性待法务确认）。**

   相应地，镜像的 `org.opencontainers.image.source` 指向 **`byw-dev/minio`**，
   **不是** `byw-dev/fileagent`。最初那版指向 fileagent 是**张冠李戴**：
   在机器可读的元数据里声称「这个 MinIO 二进制的源码是 FileAgent」，
   而这正是本项目反复吃过的那类「记录撒谎」。已纠正。
   ⚠️ **源码提供途径的表述要钉在 tag 上而不是「这个 fork」**：我们的用途只涉及
   `RELEASE.2025-04-22T22-12-26Z` 这一个 tag，不对该 fork 后续/其它 commit 的
   许可状态作任何主张。

6. **加 provenance 标签，不改内容**。用 LABEL-only 构建（`FROM` + `LABEL`，
   **不新增层、不执行任何命令**——「文件系统与上游逐字节相同」说的作用域是
   **我们的构建相对于它 `FROM` 的那个基础镜像**，不是「相对于官方发布的镜像」，
   见下方「已知缺口」第 1 条）打上
   `org.opencontainers.image.source`（→ `byw-dev/minio`）、
   `org.opencontainers.image.revision`、`org.opencontainers.image.licenses=AGPL-3.0-only`、
   `io.byw.mirror.upstream-ref`、`io.byw.mirror.source-offer`（钉到 tag 的 tree URL）、
   `io.byw.mirror.consumer`（→ `byw-dev/fileagent`，即「谁在用」，与「源码在哪」分开表达）、
   `io.byw.mirror.reason`。
7. **CI 给出可读失败**。镜像拉不到原本的症状是 compose 启动阶段一个赤裸的
   `unauthorized` + `exit code 125`，看不出是权限还是网络。现在 `ci-smoke.yml` 有
   独立的预检步骤，失败时明确指向本决策并提示「到 package 设置页把本仓库加入
   Actions 访问（Read）」。
8. **离线交付路径成文**。私有 package 意味着客户现场 `docker compose up` 拉不到镜像，
   所以 `docs/ops/deployment.md` §0.1 写明离线导入步骤与校验方法。这不是可选项——
   **它是交付能力的一部分**。三个决定流程形状的 docker/compose 行为**已实测**
   （2026-09-25，Apple Silicon + Docker Desktop）：`docker save` 按 list digest 引用
   只存当前平台、同一 `name@digest` 引用不能切架构（第二个 `pull --platform` 报
   `cannot overwrite digest`，经典 image store 复现；containerd snapshotter 下未验证）
   ——所以导出用**两个子 manifest digest** 各自 pull/save
   （**一个 tarball 一个架构**）；`docker load` 只恢复镜像内容（Image ID）、
   **不恢复 RepoDigest**，load 后 `name@digest` 在本地解析不到——所以离线侧显式打 tag；
   `export` 的变量只活在当前 shell——所以离线引用写进 **`deploy/.env`**（compose 自动
   加载），配合 `${FA_MINIO_IMAGE:-digest}` 默认值（不设变量时三份 compose 与原样
   逐字节等价）。§0.1 里对每条命令的实测范围做了精确限定，未实测的步骤已显式标出。

### 已知缺口（如实记录，不含糊）

1. **amd64 整体镜像缺少上游 manifest / layer 对照**。arm64 那份的 `repoDigests` 是
   `sha256:a1ea29fa28355559ef137d71fc570e508a214ec84ff8083e39bc5428980b015e`
   （quay 与 Docker Hub 同值），而 **amd64 那份的 `repoDigests` 为空**——它不是带
   digest 记录从 registry 拉下来的，或记录早已丢失。判断依据只剩「`Created`
   与该 release 一致、镜像内 `mc` 是同日期同版本的 x86-64 二进制」+ 下方
   「镜像内二进制已验签」（见其作用域边界）。**这条缺口依然成立**：minisign 验签
   只覆盖镜像内的 `minio` / `mc` 二进制及其 signed comment，**不能**证明该镜像的
   config、entrypoint、其余 rootfs 文件与层就是官方发布的那个镜像；它**只有拿到
   可验证的官方 manifest/config/layer digest 链之后**才能关闭。它是我们能拿到的
   唯一 amd64 副本，而 CI 必须用它。
2. **没有升级路径**。官方不再公开发布，**以后 MinIO 出安全补丁我们没有来源**。

3. **GHCR 的三条行为，全部实测，记在这里免得再踩**：

   ① **「关联仓库」不等于「授予 Actions 读权限」。** 给镜像打
   `org.opencontainers.image.source` **不会**让该仓库的 `GITHUB_TOKEN` 读到一个私有 package；
   必须到 package 设置页的 **Manage Actions access** 显式把仓库加进去（UI-only，无 REST API）。

   ② **症状极具误导性**：无权访问私有 package 时，GHCR 返回的是 **`manifest unknown`**
   而不是 `unauthorized`——看起来像镜像根本不存在。`ci-smoke.yml` 的预检步骤就是为这条存在的。

   ③ **关联仓库只在「当前无关联」时才由标签建立；已有关联时，再推带新标签的版本搬不动它。**
   正确做法是**先在 package 设置页删掉 Repository source，再重推**——下一次推送会重新读标签
   并落到新仓库。本决策就是这么把关联从 `byw-dev/fileagent` 改到 `byw-dev/minio` 的：
   删除后 API 读到 `repository: null`，重推后变为 `byw-dev/minio`，**digest 全程不变**
   （内容与标签都没变，content-addressed）。
   权威查法：`gh api /orgs/<org>/packages/container/<name> --jq .repository.full_name`
   （需 `read:packages` scope）。

   ④ 上述改关联的操作**不影响** Manage Actions access 授权：改完之后 CI 实跑仍能拉到镜像
   （`byw-dev/fileagent` 的 `GITHUB_TOKEN` 依旧有效）。两套设置相互独立。
4. **AGPL-3.0 的分发义务落在本项目头上**。该 release 是 AGPL-3.0，再分发是允许的，
   随交付**应当提供许可副本与对应源码的获取途径**；具体提供什么才充分（§6 有多条
   路径，Corresponding Source 还涵盖构建/安装脚本）**待法务确认**。本项目当前实际
   提供的物项与待确认清单见 `docs/ops/deployment.md` §0.1 的 AGPL 段。上游 GitHub
   仓库已归档，**长期保有那份源码是我们的责任**——不要指望上游还在。本系统是私有化
   交付、交付物本身就含 MinIO，这条义务躲不掉，只是范围是客户而不是公众。

### 镜像内二进制已验签通过（2026-09-25）——作用域边界：二进制 provenance，不是镜像 provenance

**这条验签证明什么、不证明什么，边界如下（第三轮返工 E-2 收窄）：**

- **可以说**：镜像内的 `minio`（linux/amd64 与 linux/arm64）与 `mc`（linux/amd64）
  两个二进制**及其 signed trusted comment** 已用 MinIO 官方 minisign 公钥验证通过，
  因此这几个文件**是该 release 的官方产物**。
- **不可以说**：整个镜像 / 文件系统 / 所有层的 provenance 已关闭或已证明为官方——
  验签没有覆盖镜像的 config、entrypoint、其余 rootfs 文件与层；那部分仍属于
  「已知缺口」第 1 条（amd64 整体镜像缺上游 manifest/layer 对照），只有拿到可验证的
  官方 manifest/config/layer digest 链之后才能关闭。

背景：原先记的缺口是「amd64 那份 `repoDigests` 为空、无法与任何上游 digest 比对，
证据链比 arm64 弱一档」。**验签把这条缺口里「镜像内两个二进制是不是官方产物」的
部分关闭了——而且是密码学签名，比「digest 对得上」更强；但整镜像 provenance 的
缺口仍然开放**，上段边界为准。

关键在于 `byw-dev/minio` 这个源码 fork：它的 `Dockerfile.release` 里写着 MinIO 自己的
minisign 公钥 `RWTx5Zr1tiHQLwG9keckT0c45M3AGeHD6IvimQHpyRywVWGbP1aVSGav`，
而官方镜像**自带** `/usr/bin/minio.minisig`、`/usr/bin/minio.sha256sum`（`mc` 的也带）。
用 `go run aead.dev/minisign/cmd/minisign@v0.2.1`（只写 Go module cache，不装进 PATH）验：

| 文件 | 结果 | signed trusted comment |
|------|------|------------------------|
| `minio`（linux/amd64） | ✅ `Signature and comment signature verified` | `timestamp:1745360335`（2025-04-22T22:18:55Z）`filename:minio.RELEASE.2025-04-22T22-12-26Z` |
| `minio`（linux/arm64） | ✅ 同上 | `timestamp:1745360609`（2025-04-22T22:23:29Z）同一 filename |
| `mc`（linux/amd64，CI 抽的就是它） | ✅ 同上 | `timestamp:1745357500` `filename:mc.RELEASE.2025-04-16T18-13-26Z` |

两点要注意：
- **trusted comment 本身也在签名覆盖范围内**（`comment signature verified`），所以
  `filename:minio.RELEASE.2025-04-22T22-12-26Z` 是**被密码学证实**的——不只是「某个被 MinIO 签过的二进制」，
  而是「**就是那个 release 的二进制**」。这正是 digest 比对给不了的东西。
- 顺带得到一个此前没人记过的事实：镜像内的 `mc` 是 **`RELEASE.2025-04-16T18-13-26Z`**，
  与服务端的 `2025-04-22T22-12-26Z` **不同版本**（官方镜像本来就这样打的）。CI 与
  `init-minio.sh` 用的就是这个 `mc`。

**负向对照**（防止「验签恒真」这种假绿）：把 `minio` 副本第 1000 字节改成 `\x00` 后重验 →
`Error: signature verification failed` / `exit status 1`。验签确实在起作用。

> 复核方法（任何人都能重跑）：从镜像里 `docker cp` 出 `/usr/bin/minio{,.minisig}`，
> 用上面那个公钥 `minisign -V -m minio -x minio.minisig -P <pubkey>`。

### 这条决策抬高了另一件事的紧迫性

`active.md`「下一步」第 4 条（存储层替代调研）原本的定性是「研究任务应在被逼之前开始」。
**现在已经在被逼的那一侧**，而且问题比「CI 拉不到镜像」更大一层：
我们交付的产品依赖一个**已无公开供给、且许可义务落在我们头上**的组件。
判据不变且需要重申：**第一道筛子是 STS `AssumeRole`，不是「S3 兼容」四个字**——
整条数据面（IC-2a）都建在 `AssumeRole` 上，而多数「S3 兼容」实现没有它。

### 不在本刀范围

- **把 `init-minio.sh` 的 `mc` 依赖拆掉**。`mc` 的分发通路同样已经没了，现在靠
  「从服务端镜像里抠二进制」撐着——这是个能用的办法，不是长久之计。改成纯 S3 +
  MinIO admin API over curl 是可行的（脚本本就刻意不用 grep/sed/awk），但**独立一刀**。
  本决策只解决「镜像有来源」。
- **存储层替代选型**本身（见上）。
