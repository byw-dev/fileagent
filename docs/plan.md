# docs/plan.md — 采集规则重构完整更新方案

> 本文档由对话记录（`docs/chatLog.md`）整理归档，记录所有已确认决策与待执行改动。
> 
> 状态：**等待执行**（最终字段命名方案已确认，bucket 字段修正已勘误）

---

## 一、背景与目标

### 1.1 问题来源

当前系统在采集规则相关的字段命名、功能实现层面存在多处不一致与 Bug：

- **四层命名不统一**：DB / proto / CP REST / WebUI 使用了四套不同的字段名
- **规则创建功能断路**：前端字段名与后端期望字段名不匹配，导致所有规则创建请求均以 400 失败
- **`upload_bucket` 始终为空**：`dispatch.go` 的 `ruleToProto()` 从未填充该字段，Agent 收不到 bucket 名
- **`watch_subdir_pattern` 是死代码**：DB/proto 存在此字段，但 agent 侧从未读取使用
- **路径模板能力缺失**：`upload_path_template` 当前使用简单字符串替换，无法支持结构化字段提取与时间格式

### 1.2 目标

1. 引入 `pkg/trollsift/` 共享库，支持结构化路径模板
2. 统一四层（DB / proto / REST / Frontend）字段命名
3. 修复已知 Bug（字段映射断路、bucket 名为空、mode 大小写、append_mode 值域）
4. 新增 Dry-Run（规则测试）功能
5. WebUI 补全缺失字段并新增规则测试面板

---

## 二、字段统一命名方案（已确认）

系统当前各层字段命名混乱，统一方案如下：

| 概念 | **统一字段名** | DB 现状 | proto 现状 | REST-in 现状 | REST-out 现状 | Frontend 现状 |
|---|---|---|---|---|---|---|
| 监控根目录 | `base_path` | `source_path_template` | `source_path_template` | `source_path_template` | `source_path` | `source_path` |
| 文件/路径过滤 | `path_pattern` | `file_glob` | `file_glob` | `file_glob` | `file_pattern` | `file_pattern` |
| 目标路径模板 | `dest_path_template` | `upload_path_template` | `upload_path_template` | `upload_path_template` | `dest_path_template` ✓ | `dest_path_template` ✓ |
| 目标 Bucket | `bucket_id`（DB FK）/ `upload_bucket`（proto，bucket 名称字符串） | `bucket_id` ✓ | `upload_bucket`（字符串，**保留**） | `bucket_id` ✓ | `dest_bucket_id` | `dest_bucket_id` |
| 是否递归 | `recursive` | `watch_recursive` | `watch_recursive` | `watch_recursive` | `watch_recursive` | 缺失 |
| 子目录过滤 | **删除** | `watch_subdir_pattern` | `watch_subdir_pattern` | `watch_subdir_pattern` | 缺失 | 缺失 |
| 追加模式 | `append_mode` | `append_mode` ✓ | `append_mode` ✓ | `append_mode` ✓ | 缺失 | 缺失 |
| 采集模式 | `mode`（小写）| `mode` enum ('watch','scheduled') | `mode` string | `mode` string | `mode` string | `mode`（'WATCH'/'SCHEDULED' 大写，需归一化） |
| 启用状态（采集规则） | `enabled`（REST/proto bool） | `collection_rules.status` enum ('active','inactive') **保留枚举，不改 bool** | `enabled` bool ✓ | — | `is_active` bool | `is_active` bool |
| 目标 Bucket 前端 | `bucket_id` | — | — | — | — | `dest_bucket_id`（改） |

### ⚠️ 重要澄清：`collection_rules.status` vs `agents.status`

这是两个完全不同的概念，**不能混淆**：

- **`agents.status`**（5 个值：`pending / approved / online / offline / revoked`）：描述采集器程序的注册审批与在线状态。**不改动**。
- **`collection_rules.status`**（2 个值：`active / inactive`）：描述采集规则在 Agent 端是否被启用。本质是 boolean，**改为 `enabled bool`**（DB 删除 `rule_status` enum，改为 `BOOLEAN NOT NULL DEFAULT TRUE`）。

### 关键澄清：`upload_bucket` vs `bucket_id`
- **proto `upload_bucket`** 存储 MinIO bucket 的**名称字符串**（不是 UUID），Agent 直接用它调用 S3 `PutObject`
- Agent 无法通过 UUID 查询 bucket 名（它不访问 CP 数据库），因此 proto 里必须传名称字符串
- **真正的 Bug**：`dispatch.go` 的 `ruleToProto()` 从未填充 `UploadBucket`，需修复

---

## 三、已确认的 Bug 修复

### Bug 1 — 规则创建断路（Frontend ↔ REST 字段名不匹配）

前端发送：
```json
{ "dest_bucket_id": "...", "source_path": "...", "file_pattern": "...", "dest_path_template": "..." }
```
后端期望：
```json
{ "bucket_id": "...", "source_path_template": "...", "file_glob": "...", "upload_path_template": "..." }
```
→ **修复方式**：统一字段命名后，前后端一致

### Bug 2 — `upload_bucket` 始终为空

`dispatch.go` `ruleToProto()` 未填充 `UploadBucket`，Agent 用空字符串写 MinIO。

→ **修复方式**：给 `Dispatcher` 注入 `GetBucketByID` DB 方法，在 `ruleToProto` 中查出 bucket 名再填入

### Bug 3 — Mode 大小写不一致

前端发送 `"WATCH"` / `"SCHEDULED"`，DB enum 只接受 `'watch'` / `'scheduled'`，handler 不转换。

→ **修复方式**：handler 中对 `mode` 字段做 `strings.ToLower()` 归一化

### Bug 4 — `append_mode` 值域不一致

DB 默认值 `'overwrite'`，Agent 常量 `AppendModeNone = ""` 语义等价于"全量覆盖上传"（即 overwrite），但字符串值为空串，与 DB 不一致。

→ **修复方式（已确认）**：**不改 DB 默认值**，改 Agent 侧：
- 将常量 `AppendModeNone = ""` 重命名为 `AppendModeOverwrite = "overwrite"`
- `watcher.go`、`queue.go`、`main.go` 中所有 `""` 的 append_mode 判断改为 `"overwrite"`
- SQLite `append_mode` 默认值从 `''` 改为 `'overwrite'`
- 统一后值域：`'overwrite'` / `'tail'` / `'close_wait'`，三端一致

---

## 四、trollsift 共享库（`pkg/trollsift/`）

### 4.1 包结构

```
fileagent/pkg/trollsift/
├── parser.go       # Parser: New, Parse, Compose, Globify, Validate, IsTrollsiftPattern
├── field.go        # field 类型 / parseField / parseFormat
├── regex.go        # buildRegex / fieldRegex
├── format.go       # convertLDML / formatValue
├── ldml.go         # LDML → Go time layout；ldmlGlobify；dtRegexFromLDML
├── context.go      # AgentContext / InjectContext
└── parser_test.go
```

以 Go workspace（`go.work`）方式，`agent` 和 `controlplane` 均直接 import `github.com/byw-dev/fileagent/pkg/trollsift`。

### 4.2 核心 API

```go
// IsTrollsiftPattern 判断字符串是否为 trollsift 模式（含 '{' 字符）
func IsTrollsiftPattern(s string) bool

// New 解析格式串，返回 Parser 或错误
func New(pattern string) (*Parser, error)

// Validate 校验格式串是否合法（不执行 Parse）
func (p *Parser) Validate() error

// Parse 从字符串 s 提取字段值，返回 map
func (p *Parser) Parse(s string) (map[string]Value, error)

// Compose 将字段值 map 格式化到格式串中，allowPartial=true 允许缺失字段
func (p *Parser) Compose(vals map[string]Value, allowPartial bool) (string, error)

// Globify 将格式串转为 glob 字符串（时间字段→等宽?序列，字符串字段→*，整数字段→按宽度）
func (p *Parser) Globify() string
```

### 4.3 Value 类型

```go
type Value struct {
    Str   string;    IsStr   bool
    Int   int;       IsInt   bool
    Float float64;   IsFloat bool
    Time  time.Time; IsTime  bool
}
func S(s string) Value
func I(n int) Value
func T(t time.Time) Value
```

### 4.4 LDML 时间字段

字段格式串支持 Unicode LDML (UTS #35) 子集：

| 符号 | 含义 | Glob 宽度 |
|------|------|-----------|
| `yyyy` | 4 位年 | `????` |
| `yy` | 2 位年 | `??` |
| `MM` | 月（01-12） | `??` |
| `dd` | 日（01-31） | `??` |
| `HH` | 时（00-23） | `??` |
| `mm` | 分（00-59） | `??` |
| `ss` | 秒（00-59） | `??` |

时区：字段格式串末尾附加 `|tz=IANA`，如 `{time:yyyy/MM/dd|tz=Asia/Shanghai}`；未指定则 UTC。

### 4.5 AgentContext 注入

```go
type AgentContext struct {
    AgentName string  // → {agent_name}
    AgentID   string  // → {agent_id}
}
func InjectContext(ctx AgentContext, vals map[string]Value) map[string]Value
```

---

## 五、Agent 端改动

### 5.1 `path_pattern` 匹配逻辑升级

当前 `watcher.matchGlob()` 只对 `filepath.Base(path)`（纯文件名）做 `filepath.Match`，无法支持跨目录匹配或 trollsift 结构化提取。

**改动**：
- 将匹配对象从"文件名"改为"相对于 `base_path` 的相对路径"
- 引入 `doublestar` 包，替换 `filepath.Match` → `doublestar.Match`，支持 `**`、`?`、`[]`
- 若 `path_pattern` 含 `{`（trollsift 模式），先用 `Globify()` 转为 glob 做路径匹配，再用 `Parse(relPath)` 提取字段
- 若不含 `{`，直接 `doublestar.Match(path_pattern, relPath)`

**`walkAndSubmit` 同步修改**：同样替换 `filepath.Match` → `doublestar.Match`

### 5.2 `buildStoragePath` 重写

将现有简单字符串替换逻辑替换为 trollsift Compose 流程：

1. 若 `path_pattern` 为 trollsift 模式，从 `Parse(relPath)` 结果取字段 map
2. 调用 `InjectContext(agentCtx, fields)` 注入 `{agent_name}`、`{agent_id}`
3. 注入文件元数据：`{filename}`（`filepath.Base(localPath)`）、`{ext}`（扩展名）、`{time:...}`（当前时间）
4. 调用 `parser.Compose(vals, allowPartial=false)` 得到最终 S3 路径

**`scheduler.ResolvePath()`**：标注 deprecated，保留但不再被核心路径调用

### 5.3 config.toml 新增配置

```toml
[collection]
dry_run_limit = 10  # 规则测试时最多返回的文件数，默认 10
```

### 5.4 Dry-Run 处理（`handleDryRun` 函数）

当收到 `dry_run=true` 的 `PushRuleCommand` 时：

1. 用 `base_path + Globify(path_pattern)` 在文件系统查找文件（`WalkDir` + early return，最多 `dry_run_limit` 个）
2. 对每个匹配文件执行 trollsift `Parse(relPath)` + `InjectContext` + `Compose(dest_path_template)`
3. 封装 `DryRunResult` 发回 CP

---

## 六、Proto 改动（`proto/v1/agent.proto`）

### 6.1 字段重命名（直接原地改，field number 可一并整理）

```protobuf
message CollectionRule {
    string rule_id          = 1;
    string name             = 2;
    string mode             = 3;
    string base_path        = 4;   // 原 source_path_template
    string path_pattern     = 5;   // 原 file_glob（合并 watch_subdir_pattern）
    string dest_path_template = 6; // 原 upload_path_template
    string upload_bucket    = 7;   // 保留，存 bucket 名称字符串（非 UUID）
    bool   recursive        = 8;   // 原 watch_recursive
    // watch_subdir_pattern 删除
    string cron_expr        = 9;
    bool   run_once_on_start = 10;
    string append_mode      = 11;
    bool   enabled          = 12;
    bool   dry_run          = 13;  // 新增：true 时 agent 只做匹配+Compose，不上传
}
```

### 6.2 新增消息（Dry-Run 结果上行）

```protobuf
message DryRunResult {
    string rule_id = 1;
    repeated DryRunFileResult files = 2;
    string error = 3;
}

message DryRunFileResult {
    string local_path    = 1;
    string upload_path   = 2;
    map<string, string> parsed_fields = 3;
    string compose_error = 4;
}
```

`AgentMessage.payload` 增加：
```protobuf
DryRunResult dry_run_result = 15;
```

---

## 七、DB 改动（直接原地修改 `000001_init_schema.up.sql`）

```sql
-- collection_rules 表字段改动
-- 重命名
source_path_template → base_path
file_glob → path_pattern
upload_path_template → dest_path_template
watch_recursive → recursive
-- 删除
DROP COLUMN watch_subdir_pattern
-- status rule_status 枚举类型保留（不改为 bool）——便于后期扩展更多状态
-- 应用层转换：status='active' → enabled=true；status='inactive' → enabled=false
-- append_mode 默认值保持 'overwrite'（DB 侧不改，Agent 侧统一到此值）
```

> **重建 DB 方式**：
> ```bash
> migrate -database "$DATABASE_URL" -path ./migrations force 0
> migrate -database "$DATABASE_URL" -path ./migrations up
> ```

DB model (`db.CollectionRule`) 和 sqlc 生成代码同步更新。

---

## 八、Control Plane 改动

### 8.1 `createRuleRequest` / `collectionRuleResponse` / `toRuleResponse()`

统一字段名（详见第二节），修复 Bug 1 和 Bug 3：

```go
type createRuleRequest struct {
    BucketID       string `json:"bucket_id" binding:"required"`
    Name           string `json:"name" binding:"required"`
    Mode           string `json:"mode" binding:"required"`    // 强制 ToLower
    BasePath       string `json:"base_path" binding:"required"`
    PathPattern    string `json:"path_pattern" binding:"required"`
    DestPathTemplate string `json:"dest_path_template" binding:"required"`
    Recursive      bool   `json:"recursive"`
    CronExpr       string `json:"cron_expr"`
    RunOnceOnStart bool   `json:"run_once_on_start"`
    AppendMode     string `json:"append_mode"`
}
```

`toRuleResponse()` 同步更新，修复 REST-out 字段名与 DB 字段名不一致问题。

### 8.2 `dispatch.go` — 修复 Bug 2（`upload_bucket` 始终为空）

给 `Dispatcher` 注入 `BucketQuerier`（包含 `GetBucketByID` 方法）；  
`ruleToProto()` 先查 bucket 名，再填入 `UploadBucket`：

```go
func (d *Dispatcher) ruleToProto(ctx context.Context, rule *db.CollectionRule) (*agentv1.CollectionRule, error) {
    bucket, err := d.buckets.GetBucketByID(ctx, rule.BucketID)
    if err != nil {
        return nil, err
    }
    return &agentv1.CollectionRule{
        ...
        UploadBucket: bucket.Name,
        ...
    }, nil
}
```

### 8.3 `dryRunStore` + `TestRule` REST 端点

类似现有 `dirstore`（pending-map 模式），key=临时 rule_id（UUID），value=chan，TTL=30s。

```
POST /api/v1/agents/:id/test-rule
Body: { base_path, path_pattern, dest_path_template, recursive, dry_run_limit }

Response 200: { "files": [{ local_path, upload_path, parsed_fields }] }
Response 409: { "error": "AGENT_OFFLINE" }
Response 504: { "error": "TIMEOUT" }
```

处理流程：
1. 生成临时 rule_id（UUID）
2. 向 agent 推送 `dry_run=true` 的 `PushRuleCommand`
3. 等待 `dryRunStore` channel，最多 30s
4. 返回结果（无副作用，不写 DB）

---

## 九、Agent 端字段名同步

`main.go`、`scheduler.go`、`scheduler/rule.go` 中的 `CollectionRule` Go 结构体字段名同步更新：

| 原字段 | 新字段 |
|---|---|
| `SourcePathTemplate` | `BasePath` |
| `FileGlob` | `PathPattern` |
| `UploadPathTemplate` | `DestPathTemplate` |
| `WatchRecursive` | `Recursive` |
| `WatchSubdirPattern` | 删除 |

---

## 十、WebUI 改动

### 10.1 `src/services/agents.ts`

- `CollectionRulePayload` 字段名同步（修复 Bug 1）
- `dest_bucket_id` → `bucket_id`
- `source_path` → `base_path`
- `file_pattern` → `path_pattern`
- `dest_path_template` ✓（已经正确）
- 新增 `recursive`、`append_mode` 字段

### 10.2 `RuleForm.tsx` Step 2 补全缺失字段

新增：
- `recursive`：Switch 控件（是否递归监控子目录）
- `append_mode`：Select 控件，选项：`none` / `tail` / `close_wait`
- 更新 `path_pattern` Tooltip：说明支持普通 glob（`*.csv`、`**/*.csv`）和 trollsift 结构化模式（含 `{}` 时）
- 增加折叠面板"字段语法帮助"：列出 LDML 时间符号 + 类型格式说明

### 10.3 `RuleForm.tsx` Step 3 新增规则测试面板

在路径模板输入框下方新增"规则测试"区域：
- "立即测试"按钮（无需先保存规则）
- 点击 → 调用 `POST /api/v1/agents/:id/test-rule`，传入当前 Step2/Step3 表单值
- 结果：简洁表格，列出「本地路径 / 解析字段 / 上传路径」（最多 10 条）
- 状态处理：loading / 成功 / Agent 离线 / 无匹配 / 超时

### 10.4 `src/utils/pathTemplate.ts` 重写

- 移除白名单变量枚举校验
- 改为轻量级语法校验：`{}` 平衡、字段名非空、`|tz=` 后为非空字符串
- `renderPathPreview` 使用示例数据做前端本地预览（不调 CP），时间字段按 LDML 格式展示示例
- 更新"可用变量"面板为分组展示：上下文变量 / 文件元数据 / 日期时间示例 / 从 path_pattern 解析的字段名

---

## 十一、实施顺序（已确认）

> 确认原则：先做独立可测试的库，再做存量代码改造，最后做新功能。

1. **`pkg/trollsift/`**：实现核心库（独立模块，单独可测），单元测试覆盖率 ≥ 90%
2. **DB**：直接原地修改 `controlplane/migrations/000001_init_schema.up.sql`，同步更新 sqlc 生成代码（`rules.sql.go` 等）
3. **Proto**：原地修改 `proto/v1/agent.proto`（字段重命名 + Dry-Run 新增消息），重新生成 Go 代码
4. **Agent**：
   - 字段名同步（`BasePath`、`PathPattern` 等）
   - Bug 4：`AppendModeNone = ""` → `AppendModeOverwrite = "overwrite"`，SQLite 默认值同步
   - `matchGlob`、`walkAndSubmit` 改用 `doublestar.Match`（相对路径）
   - `buildStoragePath` 改写为 trollsift Compose 流程
   - `handleDryRun` 函数实现
   - `config.toml` 新增 `dry_run_limit`
5. **CP**：
   - `createRuleRequest` / `toRuleResponse` 字段名统一（修复 Bug 1 / Bug 3）
   - `dispatch.go` `ruleToProto()` 填充 `UploadBucket`（修复 Bug 2）
   - `dryRunStore` + `handleAgentMessage DryRunResult` case
   - `TestRule` REST 端点（`POST /api/v1/agents/:id/test-rule`）
6. **WebUI**：`pathTemplate.ts` 重写 + `RuleForm.tsx` Step2/Step3 更新（补全字段 + 测试面板）+ `agents.ts` 字段映射修复
7. **DECISIONS.md**：记录字段统一命名决策（D-009）和 trollsift 引入决策（D-010）

---

## 十二、不动的部分

| 现有代码 | 保持不变的理由 |
|---|---|
| `scheduler.ResolvePath()` | 保留但标注 deprecated，避免破坏现有单元测试 |
| STS 凭据流程 | 与 bucket 字段无关，无需修改 |
| `upload_bucket`（proto 字段语义） | 确认保留为 bucket 名称字符串，不改为 UUID |
| 心跳 / gRPC 连接流 | 与采集规则字段无关 |
| 分页逻辑 | 与本次改动无关 |
