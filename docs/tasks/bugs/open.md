# bugs/open.md — 未解决 BUG

> 本文件结构化描述所有待修复 Bug，Agent 可直接消费。
> 每个 Bug 包含：根因、精确代码位置、修复方案（拆分为子任务）、验收标准。

---

## 总览

| ID | 标题 | 严重程度 | 涉及模块 | 修复子任务 |
|----|------|---------|---------|-----------|
| T3-5-BUG-1 | 规则字段名不规范：REST-in 使用前端别名而非统一字段名 | 🟡 P2 | controlplane + webui | T3-5-G |
| T3-5-BUG-2 | `upload_bucket` 始终为空，Agent 无法写入 MinIO | 🔴 P0 | controlplane | T3-5-H |
| T3-5-BUG-3 | `mode` 大小写不一致导致 DB 写入失败 | 🟡 P1 | controlplane | T3-5-G |
| T3-5-BUG-4 | `append_mode` 值域三端不一致 | 🟡 P1 | agent | T3-5-D |

所有 Bug 均在 T3-5 中修复，详细子任务规格见：`docs/tasks/phases/phase-3-rft.md`

---

## T3-5-BUG-1 — 规则字段名不规范：REST-in 使用前端别名而非统一字段名

**严重程度**：🟡 P2（字段命名不规范，规则创建功能本身可用）  
**涉及模块**：controlplane + webui  
**修复子任务**：T3-5-G

### 当前状态

`createRuleRequest`（`controlplane/internal/api/handler/agents.go`）的 JSON 标签**已匹配前端字段名**（`dest_bucket_id` / `source_path` / `file_pattern` / `dest_path_template`），与 `collectionRuleResponse` 输出保持对称，Gin binding 不会失败。

**规则创建请求不会因字段名不匹配而返回 400**。

T3-5 的目标是将四层（DB / proto / REST / Frontend）统一到规范字段名（`bucket_id` / `base_path` / `path_pattern` / `dest_path_template`），而非修复现有的绑定错误。

> ⚠️ 若 T3-5 变更前 `create rule` 请求仍然失败，请检查 BUG-3（mode 大小写）。

### 当前字段名实际状态（审计于 2026-05-12）

| 概念 | REST-in 现状 | Frontend 现状 | T3-5 统一目标 |
|------|------------|-------------|-------------|
| 目标 Bucket | `dest_bucket_id` | `dest_bucket_id` | `bucket_id` |
| 监控根目录 | `source_path` | `source_path` | `base_path` |
| 文件过滤 | `file_pattern` | `file_pattern` | `path_pattern` |
| 目标路径模板 | `dest_path_template` ✓ | `dest_path_template` ✓ | `dest_path_template` ✓（已是目标名） |

### 代码位置

- `controlplane/internal/api/handler/agents.go`：`createRuleRequest` JSON tag（当前使用前端别名）
- `controlplane/internal/api/handler/agents.go`：`collectionRuleResponse` JSON tag（同上）
- `webui/src/services/agents.ts`：`CollectionRulePayload` 接口字段名

### 修复方案

T3-5-G 将同步变更 REST handler JSON tag 与前端接口字段名至统一命名，参见 `DECISIONS.md` D-009 完整字段映射表。

### 验收标准

T3-5 完成后，`POST /api/v1/agents/:id/rules` 使用统一字段名（`base_path` / `path_pattern` 等）返回 201，旧别名（`source_path` / `file_pattern`）不再被接受。

---

## T3-5-BUG-2 — `upload_bucket` 始终为空，Agent 无法写入 MinIO

**严重程度**：🔴 P0（Agent 收到空 bucket 名，S3 PutObject 必然失败）  
**涉及模块**：controlplane（`dispatch.go`）  
**修复子任务**：T3-5-H

### 根因

`controlplane/internal/dispatch/dispatch.go` 的 `ruleToProto()` 函数从未填充 `UploadBucket` 字段：

```go
// 现状：UploadBucket 字段缺失，proto 下发时该字段为零值（空字符串）
return &agentv1.CollectionRule{
    RuleId: rule.ID.String(),
    Name:   rule.Name,
    // UploadBucket: ???  ← 从未赋值
    ...
}
```

Agent 收到空 bucket 名后，以空字符串调用 MinIO S3 API，写入必然失败。

### 代码位置

- `controlplane/internal/dispatch/dispatch.go`：`ruleToProto()` 函数，`UploadBucket` 字段缺失

### 修复方案

给 `Dispatcher` 注入 `BucketQuerier` 接口（含 `GetBucketByID` 方法），在 `ruleToProto()` 中查出 bucket 名再填入：

```go
bucket, err := d.buckets.GetBucketByID(ctx, rule.BucketID)
// ...
UploadBucket: bucket.Name,
```

注意：proto `upload_bucket` 存储 bucket **名称字符串**（非 UUID），Agent 直接用它调用 S3。详见 `DECISIONS.md` D-009"关键澄清"。

### 验收标准

规则下发后 Agent 日志中 `upload_bucket` 字段非空，文件可正常上传至 MinIO。

---

## T3-5-BUG-3 — `mode` 大小写不一致导致 DB 写入失败

**严重程度**：🟡 P1（前端传 `"WATCH"` 时规则创建失败）  
**涉及模块**：controlplane  
**修复子任务**：T3-5-G

### 根因

前端发送 `mode: "WATCH"` 或 `mode: "SCHEDULED"`（大写），但 DB `mode` 字段为 enum 类型，只接受 `'watch'` / `'scheduled'`（小写）。handler 中未做归一化，直接将大写值写入 DB，触发 enum 约束错误。

### 代码位置

- `controlplane/internal/api/handler/rules.go`：`createRule` handler，`mode` 字段写入前未 `strings.ToLower()`
- `webui/src/pages/Agents/RuleForm.tsx`：表单提交时 `mode` 值为大写字符串

### 修复方案

在 handler 写入 DB 前对 `mode` 字段做归一化：

```go
mode = strings.ToLower(req.Mode)
```

### 验收标准

前端传 `"WATCH"` 时，DB 存储 `'watch'`，规则创建成功。

---

## T3-5-BUG-4 — `append_mode` 值域三端不一致

**严重程度**：🟡 P1（语义歧义，潜在逻辑错误）  
**涉及模块**：agent  
**修复子任务**：T3-5-D

### 根因

Agent 侧常量 `AppendModeNone = ""`（空字符串）语义上等价于"全量覆盖上传"，但：
- DB 默认值为 `'overwrite'`（非空字符串）
- proto 下发的 `append_mode` 默认值为 `'overwrite'`
- SQLite 队列表 `append_mode` 默认值为 `''`（空字符串）

三端值域不一致，Agent 比较 `appendMode == ""` 的判断逻辑在接收到 proto 下发的 `'overwrite'` 时会失效。

### 代码位置

- `agent/internal/watcher/watcher.go`：`AppendModeNone = ""` 常量定义及其判断逻辑
- `agent/internal/queue/queue.go`：SQLite DDL `append_mode DEFAULT ''`
- `agent/cmd/agent/main.go`：`AppendModeNone` 引用处
- `controlplane/internal/api/handler/agents.go`：`CreateRule` handler，line 564-566，`appendMode = "none"` 为无效默认值（DB 接受 `'overwrite'`，不接受 `'none'`）

### 修复方案

Agent 侧统一到 `'overwrite'`：
- `AppendModeNone = ""` → `AppendModeOverwrite = "overwrite"`
- SQLite DDL：`DEFAULT ''` → `DEFAULT 'overwrite'`
- 所有 `appendMode == ""` 判断改为 `appendMode == AppendModeOverwrite`

**同时修复 handler**：
```go
// controlplane/internal/api/handler/agents.go
appendMode := req.AppendMode
if appendMode == "" {
    appendMode = "overwrite"  // 原为 "none"，修正为合法默认值
}
```

DB 侧默认值 `'overwrite'` 保持不变。统一后三端值域：`'overwrite'` / `'tail'` / `'close_wait'`。

### 验收标准

Agent 接收 proto 下发的 `append_mode: "overwrite"` 时，覆盖上传逻辑正常触发；`go test ./...` 通过。

---

## 关联入口

- 已关闭 Bug：[`closed.md`](closed.md)
- 修复子任务规格：[`../phases/phase-3-rft.md`](../phases/phase-3-rft.md)
- 字段命名决策：[`../../../DECISIONS.md`](../../../DECISIONS.md) D-009
- 当前活跃任务：[`../active.md`](../active.md)
