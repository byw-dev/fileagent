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
