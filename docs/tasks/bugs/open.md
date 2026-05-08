# bugs/open.md — 未解决 BUG

> 本文件结构化描述所有待修复 Bug，Agent 可直接消费。
> 每个 Bug 包含：根因、精确代码位置、修复方案、验收标准。

---

## 总览

| ID | 标题 | 严重程度 | 状态 | 前置依赖 |
|----|------|---------|------|---------|
| T3-2-FIX-A | 后端：`GET /api/v1/agents` 响应格式与参数对齐 | 🔴 P0 | ⬜ | — |
| T3-2-FIX-B | 后端：`GET /api/v1/agents/:id/rules` 与上传日志端点信封对齐 | 🔴 P0 | ⬜ | — |
| T3-2-FIX-C | 前端：`Agent` 接口与 `AgentStatus` 对齐 | 🔴 P0 | ⬜ | T3-2-FIX-A |
| T3-2-FIX-D | 前端：`CollectionRule` 接口字段对齐 | 🔴 P0 | ⬜ | T3-2-FIX-B |
| T3-2-FIX-E | 后端：`GET /api/v1/files` 响应格式与字段名对齐 | 🔴 P0 | ⬜ | — |
| T3-2-FIX-F | 前端：`FileEntry` 接口字段对齐 | 🔴 P0 | ⬜ | T3-2-FIX-E |
| T3-2-FIX-G | 后端：upload-logs 端点响应格式与字段对齐 | 🔴 P0 | ⬜ | — |
| T3-2-FIX-H | 前端：`UploadLog` 接口字段对齐 | 🔴 P0 | ⬜ | T3-2-FIX-G |
| T3-2-FIX-I | 后端+前端：非分页列表端点统一为 `{items, total}` 信封 | 🟡 P1 | ⬜ | T3-2-FIX-A~H |

**修复顺序建议**：
```
T3-2-FIX-A/B/E/G（后端 P0，可并行）
    ↓
T3-2-FIX-C/D/F/H（前端 P0，依赖对应后端，可并行）
    ↓
T3-2-FIX-I（P1，最后做）
    ↓
T3-2 整体 Smoke Test 验收
```

---

## T3-2-FIX-A — 后端：`GET /api/v1/agents` 响应格式与参数对齐 ⬜

**严重程度**：🔴 P0 — 采集器列表页表格永远为空；状态徽章色彩错误；审批/吊销按钮消失

### 根因分析

**根因 1（致命）：响应信封不匹配**

后端返回 `{"data":[...]}` 但前端 `listAgents()`（`services/agents.ts:45`）把 `response.data` 当作 `PaginatedResponse<Agent>` 处理，`response.data.items` 为 `undefined`，ProTable 渲染空行。

设计文档 §8.5 规定响应格式为 `{"items":[...],"total":N,"next_cursor":"...","has_more":true}`。

**根因 2：状态枚举大小写不一致**

DB 存 `"pending"` 等小写，前端 `AgentStatus` 用 `'PENDING'` 等大写，`agent.status === 'PENDING'` 永远为 false，审批按钮永不显示。

另：`db.AgentStatusOnline = "online"` 而前端用 `'RUNNING'`（无 `'ONLINE'`）。

**根因 3：`agentResponse` 字段嵌套**

| 前端字段 | 后端实际返回 | 影响 |
|---------|------------|------|
| `hostname` | `os_info.hostname`（嵌套） | 显示空 |
| `os_type` | `os_info.os_type`（嵌套，且前端字段名为 `os`） | 显示空 |
| `agent_version` | `os_info.agent_version`（嵌套，且前端字段名为 `version`） | 显示空 |
| `last_seen_at` | `last_seen_at`（字段名，前端用 `last_heartbeat_at`） | 显示 `-` |
| `created_at` | `created_at`（字段名，前端用 `registered_at`） | `Invalid Date` |

**根因 4：List 端点忽略查询参数**

`AgentsHandler.List`（`agents.go:102-121`）完全忽略 `status`、`cursor`、`limit` 查询参数，前端过滤无效。

### 修复方案

**文件**：`controlplane/internal/api/handler/agents.go` + `agents_test.go`

1. **响应信封改为 `{items, total, next_cursor}`**：
   ```go
   c.JSON(http.StatusOK, gin.H{"items": resp, "total": len(resp), "next_cursor": nil})
   ```

2. **支持 `status` 查询参数**：读取 `c.Query("status")`，非空时 `strings.ToLower` 后调用 `db.ListAgentsByStatus`

3. **支持 `limit` 查询参数**：使用已有的 `parseLimitParam(c)` 截断

4. **`agentResponse` 展开 `os_info` 字段**：
   ```go
   type agentResponse struct {
       ID           string `json:"id"`
       OrgID        string `json:"org_id"`
       Name         string `json:"name"`
       Status       string `json:"status"`
       IpAddress    string `json:"ip_address,omitempty"`
       Hostname     string `json:"hostname,omitempty"`
       OsType       string `json:"os_type,omitempty"`
       OsVersion    string `json:"os_version,omitempty"`
       AgentVersion string `json:"agent_version,omitempty"`
       LastSeenAt   string `json:"last_seen_at,omitempty"`
       CreatedAt    string `json:"created_at,omitempty"`
   }
   ```
   `toAgentResponse` 中解析 `a.OsInfo`（`[]byte`）为 map 并展开各字段。

5. **状态值大小写**：`toAgentResponse` 中 `strings.ToUpper(string(a.Status))`；`"online"` 映射为 `"RUNNING"`；`status` 查询参数接受时 `strings.ToLower` 转换后查 DB。

### 验收标准

- `GET /api/v1/agents` 返回 `{"items":[...],"total":N,"next_cursor":null}`
- `GET /api/v1/agents?status=PENDING` 只返回 pending 状态的 agent
- 每个 agent 对象包含顶层 `hostname`、`os_type`、`agent_version` 字段（非嵌套 `os_info`）
- 每个 agent 对象的 `status` 值为大写（`"PENDING"` 等）；`"online"` 显示为 `"RUNNING"`
- `agents_test.go` 的 `List` 相关测试用例覆盖：全量、按状态过滤、按 limit 截断

---

## T3-2-FIX-B — 后端：`GET /api/v1/agents/:id/rules` 与上传日志端点信封对齐 ⬜

**严重程度**：🔴 P0 — 详情页规则 Tab 列表为空；规则字段全部错乱

### 根因分析

**根因 1：响应信封**

`ListRules`（`agents.go:332`）返回 `{"data":[...]}` 而非 `{"items":[...],"total":N,"next_cursor":null}`，前端 `PaginatedResponse.items` 为 `undefined`。

**根因 2：`collectionRuleResponse` 字段名与前端接口不一致**

| 后端 JSON tag | 前端期望字段 |
|-------------|------------|
| `source_path_template` | `source_path` |
| `file_glob` | `file_pattern` |
| `bucket_id` | `dest_bucket_id` |
| `upload_path_template` | `dest_path_template` |
| `status` string | `is_active` bool（`"active"` → `true`） |
| 缺失 | `run_once_on_start` bool |

**根因 3：`ListUploadLogs`（`agents.go:580`）信封同样为 `{"data":[...]}`**

### 修复方案

**文件**：`controlplane/internal/api/handler/agents.go` + `agents_test.go`

1. `ListRules` 响应改为 `{"items": resp, "total": len(resp), "next_cursor": nil}`
2. `collectionRuleResponse` 字段重命名：
   - `SourcePathTemplate` → JSON tag `"source_path"`
   - `FileGlob` → JSON tag `"file_pattern"`
   - `BucketID` → JSON tag `"dest_bucket_id"`
   - `UploadPathTemplate` → JSON tag `"dest_path_template"`
   - 移除 `Status string`，改为 `IsActive bool`（`rule.Status == "active"`）
   - 增加 `RunOnceOnStart bool` 字段（从 `rule.RunOnceOnStart` 获取）
3. `ListUploadLogs`（agents.go）响应改为 `{"items": resp, "total": len(resp), "next_cursor": nil}`

### 验收标准

- `GET /api/v1/agents/:id/rules` 返回 `{"items":[...],"total":N,"next_cursor":null}`
- 每条规则包含 `source_path`、`file_pattern`、`dest_bucket_id`、`dest_path_template`、`is_active`（bool）、`run_once_on_start`（bool）
- `agents_test.go` 补充 `ListRules` 响应形状断言

---

## T3-2-FIX-C — 前端：`Agent` 接口与 `AgentStatus` 对齐 ⬜

**严重程度**：🔴 P0  
**前置依赖**：T3-2-FIX-A

### 修复方案

**文件**：`webui/src/services/agents.ts`、`webui/src/components/AgentStatusBadge.tsx`、`webui/src/pages/Agents/index.tsx`、`webui/src/pages/Agents/Detail.tsx`、`webui/src/pages/Agents/Pending.tsx`

1. **`AgentStatus` 保持大写**（已对齐修复后后端），确认 `"RUNNING"` 映射（后端 `"online"` → `"RUNNING"`）已处理

2. **`Agent` 接口字段对齐**：
   ```typescript
   export interface Agent {
     id: string
     org_id: string
     name: string
     hostname: string          // 来自后端顶层 hostname
     ip_address: string
     os_type: string           // 改名：原 os → os_type
     os_version: string
     agent_version: string     // 改名：原 version → agent_version
     status: AgentStatus
     last_seen_at: string | null   // 改名：原 last_heartbeat_at → last_seen_at
     created_at: string            // 改名：原 registered_at → created_at
   }
   ```

3. **更新所有字段引用**：`index.tsx`、`Detail.tsx`、`Pending.tsx` 中的 `dataIndex` 和 `render` 回调

### 验收标准

- 采集器列表页表格显示 agent 行，主机名、IP、状态徽章颜色正确
- 待审批 Tab 过滤出 pending agent，审批/吊销按钮正常显示
- 详情页基本信息 Tab 所有字段有值（无空值、无 `Invalid Date`）
- `pnpm test` 相关用例通过

---

## T3-2-FIX-D — 前端：`CollectionRule` 接口字段对齐 ⬜

**严重程度**：🔴 P0  
**前置依赖**：T3-2-FIX-B

### 修复方案

**文件**：`webui/src/services/agents.ts`、`webui/src/pages/Agents/Detail.tsx`、`webui/src/pages/Agents/Rules.tsx`、`webui/src/pages/Agents/RuleForm.tsx`

1. **`CollectionRule` 接口字段重命名**（与 T3-2-FIX-B 后端改动保持一致）：
   ```typescript
   export interface CollectionRule {
     id: string
     agent_id: string
     dest_bucket_id: string
     name: string
     mode: CollectionMode
     source_path: string
     file_pattern: string
     cron_expr: string | null
     run_once_on_start: boolean
     dest_path_template: string
     is_active: boolean
     created_at: string
     updated_at: string
   }
   ```

2. **`listRules` 返回 `PaginatedResponse<CollectionRule>`**（后端改后 `items` 信封对应）

3. **`Detail.tsx` 规则 Tab 列定义**确认字段名与接口一致

### 验收标准

- 详情页规则 Tab 正确显示规则列表（源路径、文件过滤、cron 表达式、激活状态）
- `pnpm test` 相关用例通过

---

## T3-2-FIX-E — 后端：`GET /api/v1/files` 响应格式与字段名对齐 ⬜

**严重程度**：🔴 P0 — 文件浏览器页面永远为空；仪表盘「总文件数」永远为 0

### 根因分析

文件列表端点（`files.go:157`）返回 `{"data":[...],"next_cursor":""}` 信封，前端 `listFiles()`（`services/files.ts:44`）把 `response.data` 当作 `PaginatedResponse<FileEntry>` 处理，`response.data.items` 为 `undefined`。

同时，`fileEntryResponse` 字段名与前端 `FileEntry` 接口有 5 处不匹配：

| 后端 JSON tag | 前端 `FileEntry` 字段 | 影响 |
|-------------|----------------------|------|
| `file_name` | `filename` | 列表文件名列空白 |
| `size_bytes` | `size` | 全显示 `"0 B"` |
| `content_type` | `mime_type` | MIME 类型列为空 |
| `storage_path` | `storage_key` | 存储路径为空 |
| `status` 小写（`"indexed"`） | 大写枚举（`'INDEXED'`） | 状态徽章 fallback |

### 修复方案

**文件**：`controlplane/internal/api/handler/files.go` + `files_test.go`

1. **响应信封**改为 `{"items": resp, "total": len(resp), "next_cursor": nextCursor}`
2. **`fileEntryResponse` 字段重命名**（JSON tag）：
   - `file_name` → `filename`
   - `size_bytes` → `size`
   - `content_type` → `mime_type`
   - `storage_path` → `storage_key`
3. **状态值大写**：`Status: strings.ToUpper(string(e.Status))`

### 验收标准

- `GET /api/v1/files` 返回 `{"items":[...],"total":N,"next_cursor":null或字符串}`
- 每条记录含顶层 `filename`、`size`（整数）、`mime_type`、`storage_key`、`status`（大写）
- `go test ./controlplane/internal/api/handler/... -count=1` 全部通过

---

## T3-2-FIX-F — 前端：`FileEntry` 接口字段对齐 ⬜

**严重程度**：🔴 P0  
**前置依赖**：T3-2-FIX-E

### 修复方案

**文件**：`webui/src/services/files.ts`、`webui/src/pages/Files/index.tsx`、`webui/src/pages/Files/Detail.tsx`

1. 确认 `FileEntry` 接口字段与 T3-2-FIX-E 后端改动一致
2. 移除不存在于后端的字段 `indexed_at`、`metadata`（或标记为可选 `string | null`，UI 降级显示 `'—'`）
3. `Files/Detail.tsx:135` 的「索引时间」改为显示 `'—'`（后端无此字段）
4. 确认 `file.size`、`file.mime_type`、`file.storage_key` 字段名与新接口一致

### 验收标准

- 文件列表能看到文件行，文件名、大小、MIME 类型、状态正确
- 详情页显示完整元数据（不出现空白或 `Invalid Date`）
- 「获取下载链接」能弹出预签名 URL
- `pnpm test` 相关用例通过

---

## T3-2-FIX-G — 后端：upload-logs 端点响应格式与字段对齐 ⬜

**严重程度**：🔴 P0 — 全局上传日志页、采集器日志 Tab、仪表盘近期日志全部为空；仪表盘统计卡片数据错误

### 根因分析

两个端点（`GET /api/v1/upload-logs`、`GET /api/v1/agents/:id/upload-logs`）均返回 `{"data":[...],"next_cursor":""}` 信封，前端期望 `PaginatedResponse.items`。

同时 `uploadLogResponse`（`events.go:450`）字段名与前端 `UploadLog` 接口不匹配：

| 后端 JSON tag | 前端 `UploadLog` 字段 |
|-------------|----------------------|
| `file_entry_id` | `file_id` |
| `size_bytes` | `size` |
| `created_at` | `uploaded_at` |
| 无 | `filename`（前端期望，后端需从 storage_path 提取） |
| 无 | `agent_name`（推荐不返回，前端改为不依赖） |

### 修复方案

**文件**：`controlplane/internal/api/handler/events.go`（全局日志）和 `agents.go`（按采集器日志）

1. **响应信封**改为 `{"items": resp, "total": len(resp), "next_cursor": nextCursor}`
2. **`uploadLogResponse` 字段重命名**（JSON tag）：
   - `file_entry_id` → `file_id`
   - `size_bytes` → `size`
   - `created_at` → `uploaded_at`
3. **`filename` 字段**：从 `storage_path` 中提取最后路径段（`path.Base(l.StoragePath)`）作为 `filename` 返回
4. **`agent_name` 字段**：不返回（方案 A，最小改动）；在 DECISIONS.md 中记录此决策
5. **状态值大小写**：`strings.ToUpper(string(l.Status))`（如果 DB 存小写）

### 验收标准

- `GET /api/v1/upload-logs` 返回 `{"items":[...],"total":N,"next_cursor":null或字符串}`
- 每条记录含 `id`、`agent_id`、`file_id`、`filename`、`size`（整数）、`status`（大写）、`error_message`、`uploaded_at`
- `go test ./controlplane/internal/api/handler/... -count=1` 全部通过

---

## T3-2-FIX-H — 前端：`UploadLog` 接口字段对齐 ⬜

**严重程度**：🔴 P0  
**前置依赖**：T3-2-FIX-G

### 修复方案

**文件**：`webui/src/services/upload-logs.ts`、`webui/src/pages/Logs/index.tsx`、`webui/src/pages/Agents/Logs.tsx`、`webui/src/pages/Dashboard/index.tsx`

1. **`UploadLog` 接口更新**：
   ```typescript
   export interface UploadLog {
     id: string
     agent_id: string
     file_id: string | null
     filename: string              // 来自后端（从 storage_path 提取）
     size: number
     status: 'SUCCESS' | 'FAILED' | 'PENDING'
     error_message: string | null
     uploaded_at: string           // 改名：原 created_at
   }
   ```
2. **`Logs/index.tsx`**：移除 `agent_name` 列，改为显示 `agent_id`（缩短 UUID）
3. **`Agents/Logs.tsx`**：内联类型改为与服务层 `UploadLog` 一致
4. **`Dashboard/index.tsx`**：`uploadLogColumns` 中移除 `agent_name` 列
5. **`agents.ts listAgentUploadLogs()` 返回类型**：改为 `PaginatedResponse<UploadLog>`

### 验收标准

- 全局日志页正确显示文件名、大小、状态、上传时间
- 仪表盘「今日上传」统计卡数值正确（非 0）；「近7日趋势」图表显示实际数据
- `pnpm test` 相关用例通过

---

## T3-2-FIX-I — 后端+前端：非分页列表端点统一为 `{items, total}` 信封 ⬜

**严重程度**：🟡 P1（当前功能正常，但违反 §8.5 规范）  
**前置依赖**：T3-2-FIX-A~H 完成

### 根因分析

以下 4 个端点返回 `{"data":[...]}` 信封，各前端 service 函数手动提取 `.data.data`（脆弱的双层适配）：

| 端点 | 后端信封 | 前端适配 |
|------|---------|---------|
| `GET /api/v1/buckets` | `{"data":[...]}` | `response.data.data` |
| `GET /api/v1/file-types` | `{"data":[...]}` | `response.data.data` |
| `GET /api/v1/event-rules` | `{"data":[...]}` | `response.data.data` |
| `GET /api/v1/users` | `{"data":[...]}` | `response.data.data` |

另：`GET /api/v1/event-rules/:id/deliveries` 使用 `data` 字段（非 `items`），内部一致但违反规范。

### 修复方案

1. **后端**（`events.go`、`files.go`、`users.go`）：
   - 上述 4 个列表端点改为 `{"items": [...], "total": N}`（全量列表，无 `next_cursor`）
   - `GET /api/v1/event-rules/:id/deliveries`：改为 `{"items":[...],"total":N,"next_cursor":"..."}`

2. **前端 service 层**（`buckets.ts`、`file-types.ts`、`events.ts`、`users.ts`）：
   - 移除手动 `.data.data` 适配，改为接收 `{items: T[], total: number}`
   - `listRuleDeliveries()` 的响应类型改为 `{items: EventDelivery[], total: number, next_cursor: string | null}`

### 验收标准

- 4 个列表端点均返回 `{"items":[...],"total":N}`
- `GET /api/v1/event-rules/:id/deliveries` 返回 `{"items":[...],"total":N,"next_cursor":...}`
- Buckets/FileTypes/Events/Users/Deliveries 页面功能不受影响
- `go test ./controlplane/internal/api/handler/... -count=1` 全部通过
- `pnpm test` 全部通过

---

## T3-2-FIX 整体 Smoke Test 验收标准

完成所有 T3-2-FIX-A~I 后，执行以下端到端验证：

1. 启动本地 `deploy/docker-compose.dev.yml` + controlplane + webui
2. 通过 grpcurl 注册一个 agent
3. 打开浏览器 → 采集器列表 → **能看到该 agent 的行**，主机名、IP、状态徽章颜色正确
4. 切换到「待审批」Tab → agent 可见，「审批」按钮出现
5. 点击审批 → agent 状态更新为 APPROVED
6. 点击 agent 名称 → 详情页所有字段有值（无 `Invalid Date`）
7. 规则 Tab 创建一条规则 → 规则出现在列表中
8. 上传文件到 MinIO → 文件浏览器**能看到该文件行**
9. 全局上传日志页**能显示记录**，时间、大小、状态字段均正确
10. 仪表盘「总文件数」> 0，「近期日志」Table 有数据
11. Bucket、文件类型、事件规则、用户列表各页面正常显示数据
12. `go test ./controlplane/internal/api/handler/... -count=1` 全部通过
13. `pnpm test` 全部通过
