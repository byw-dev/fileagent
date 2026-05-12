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

## D-006 upload-log 响应不返回 agent_name 字段（T3-2-FIX-G）

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
