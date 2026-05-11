# phase-3-rft.md — 采集规则重构（Rule Field & Template）任务规格

> **状态**：⬜ 待执行  
> **父阶段**：Phase 3 集成联调  
> **优先级**：在 T3-2 完成后立即执行，阻塞 T3-3  
> **设计参考**：`docs/plan.md`、`docs/design/system-design.md` §§4.1, 4.5, 5.5, 5.6, 5.8

---

## 任务树（执行顺序严格）

```text
T3-4  pkg/trollsift 共享库（独立，无依赖）
    ↓
T3-5  字段统一 + Bug 修复（依赖 T3-4 及新 proto）
    ↓
T3-6  Dry-Run 规则测试功能（依赖 T3-5）
```

---

## T3-4 — `pkg/trollsift` 共享库

**涉及模块**：新建 `pkg/trollsift/`（独立 Go 模块）  
**参考章节**：`system-design.md` §4.5（上传路径模板）

### 背景

当前 Agent 的 `buildStoragePath()` 使用简单字符串替换（仅 `{agent_name}` 等有限变量），
无法支持结构化路径提取（如从 `path_pattern` 解析日期/仪器编号）或 LDML 时间格式。
引入 `pkg/trollsift` 作为 agent 和 controlplane 共享的路径模板工具库。

### 模块结构

```
fileagent/pkg/trollsift/
├── go.mod              # module github.com/byw-dev/fileagent/pkg/trollsift
├── parser.go           # Parser 核心逻辑
├── field.go            # field 类型解析
├── regex.go            # buildRegex / fieldRegex
├── format.go           # convertLDML / formatValue
├── ldml.go             # LDML ↔ Go time layout 转换；dtRegexFromLDML
├── context.go          # AgentContext / InjectContext
└── parser_test.go      # 单元测试
```

`go.work` 追加 `use ./pkg/trollsift`；
`agent/go.mod` 追加 `require github.com/byw-dev/fileagent/pkg/trollsift v0.0.0`。

### API 规格

```go
package trollsift

// IsTrollsiftPattern 返回 true 当且仅当 s 含 '{' 字符（即有格式字段）。
func IsTrollsiftPattern(s string) bool

// New 解析格式串，返回 Parser 或语法错误。
// 格式串示例："{agent_name}/{time:yyyy/MM/dd}/{filename}"
func New(pattern string) (*Parser, error)

// Validate 校验格式串语法（不执行 Parse/Compose）。
// 规则：'{}' 必须平衡；字段名非空；'|tz=' 后必须是非空 IANA 时区字符串。
func (p *Parser) Validate() error

// Parse 从字符串 s 提取字段值，返回 map[字段名]Value。
// 若格式串含 '{time:...}'，提取时间字符串并解析为 time.Time（UTC，除非字段含 |tz=）。
func (p *Parser) Parse(s string) (map[string]Value, error)

// Compose 将字段值 map 格式化到格式串中，返回最终路径字符串。
// allowPartial=true：缺失字段保留原始占位符（适用于预览）。
// allowPartial=false：任何字段缺失均返回 error。
func (p *Parser) Compose(vals map[string]Value, allowPartial bool) (string, error)

// Globify 将格式串转为 doublestar 兼容的 glob 字符串：
//   {字符串字段}         → "*"
//   {整数字段:width=N}   → N 个 "?"
//   {time:yyyy/MM/dd}   → "????/??/??"（每个 LDML 符号→等宽 ? 序列）
func (p *Parser) Globify() string
```

### Value 类型

```go
type Value struct {
    Str   string
    IsStr bool
    Int   int
    IsInt bool
    Time  time.Time
    IsTime bool
}

// 构造函数
func S(s string) Value
func I(n int) Value
func T(t time.Time) Value
```

### AgentContext

```go
type AgentContext struct {
    AgentName string // 注入 {agent_name}
    AgentID   string // 注入 {agent_id}
}

// InjectContext 将 AgentContext 中的字段合并到 vals map 中（已有同名字段不覆盖）。
func InjectContext(ctx AgentContext, vals map[string]Value) map[string]Value
```

### LDML 时间字段格式串（支持子集）

字段内含时间格式时写法：`{time:yyyy/MM/dd}` 或 `{created_at:yyyy-MM-dd HH:mm|tz=Asia/Shanghai}`

| LDML 符号 | 含义 | Go layout | Glob |
|-----------|------|-----------|------|
| `yyyy` | 4 位年 | `2006` | `????` |
| `yy` | 2 位年 | `06` | `??` |
| `MM` | 月（01-12） | `01` | `??` |
| `dd` | 日（01-31） | `02` | `??` |
| `HH` | 时（00-23） | `15` | `??` |
| `mm` | 分（00-59） | `04` | `??` |
| `ss` | 秒（00-59） | `05` | `??` |

时区：`|tz=IANA`（如 `|tz=Asia/Shanghai`）；未指定则 UTC。

### 验收标准

| # | 测试用例 | 期望结果 |
|---|----------|---------|
| T3-4-1 | `New("{agent_name}/{time:yyyy/MM/dd}/{filename}")` | 返回 Parser，无错误 |
| T3-4-2 | `p.Parse("prod-agent/2024/03/15/data.csv")` | `{agent_name: "prod-agent", time: 2024-03-15, filename: "data.csv"}` |
| T3-4-3 | `p.Compose({agent_name:"a", time:T(t), filename:"f.csv"}, false)` | `"a/2024/03/15/f.csv"` |
| T3-4-4 | `p.Globify()` | `"*/???? /??/??/*"` |
| T3-4-5 | 时区字段：`New("{t:yyyy-MM-dd|tz=Asia/Shanghai}")` → `Parse("2024-01-01")` | time 已折算到 UTC |
| T3-4-6 | `allowPartial=true`，缺少 filename | 输出含原始占位符，无 error |
| T3-4-7 | `allowPartial=false`，缺少字段 | 返回包含字段名的错误 |
| T3-4-8 | 非 trollsift 格式串（无 `{`）| `New` 返回只含字面量的 Parser，`Compose` 原样返回 |
| T3-4-9 | 不平衡括号 `"{foo"` | `New` 返回语法错误 |
| T3-4-10 | `InjectContext` 不覆盖已有字段 | 现有 agent_name 不被替换 |
| 覆盖率 | `go test ./...` | 行覆盖率 ≥ 90% |

---

## T3-5 — 字段统一 + Bug 修复

**涉及模块**：controlplane（DB migrations + sqlc + handler + dispatch）、agent（main + scheduler + watcher + queue）、proto、webui  
**前置依赖**：T3-4 完成（trollsift 库可引用）  
**参考章节**：`system-design.md` §§5.5, 5.6, 4.1, 4.4

### 子任务

#### T3-5-A：DB 迁移

**文件**：`controlplane/migrations/`

新增迁移文件 `000002_rename_rule_fields.up.sql`（不修改 000001，保持迁移历史）：

```sql
-- T3-5-A: 采集规则字段重命名 + status → enabled
BEGIN;

ALTER TABLE collection_rules
    RENAME COLUMN source_path_template TO base_path;

ALTER TABLE collection_rules
    RENAME COLUMN file_glob TO path_pattern;

ALTER TABLE collection_rules
    RENAME COLUMN upload_path_template TO dest_path_template;

ALTER TABLE collection_rules
    RENAME COLUMN watch_recursive TO recursive;

ALTER TABLE collection_rules
    DROP COLUMN IF EXISTS watch_subdir_pattern;

-- status rule_status → enabled bool
ALTER TABLE collection_rules
    ADD COLUMN enabled BOOLEAN NOT NULL DEFAULT TRUE;

UPDATE collection_rules SET enabled = (status = 'active');

ALTER TABLE collection_rules
    DROP COLUMN status;

COMMIT;
```

同步创建 `000002_rename_rule_fields.down.sql`（回滚脚本）。

**sqlc 生成代码同步更新**：
- `controlplane/internal/db/queries/rules.sql`：字段名同步
- `controlplane/internal/db/rules.sql.go`：手动更新（沙箱内无法运行 sqlc）

**验收**：`migrate up` 后 `collection_rules` 表结构符合新字段名；`migrate down` 可完整回滚。

#### T3-5-B：Proto 字段重命名

**文件**：`proto/v1/agent.proto`

`CollectionRule` 消息字段重命名（field number 同步整理）：

```protobuf
message CollectionRule {
    string rule_id            = 1;
    string name               = 2;
    string mode               = 3;
    string base_path          = 4;   // 原 source_path_template
    string path_pattern       = 5;   // 原 file_glob（删 watch_subdir_pattern）
    string dest_path_template = 6;   // 原 upload_path_template
    string upload_bucket      = 7;   // 保留，bucket 名称字符串
    bool   recursive          = 8;   // 原 watch_recursive
    string cron_expr          = 9;
    bool   run_once_on_start  = 10;
    string append_mode        = 11;
    bool   enabled            = 12;
    bool   dry_run            = 13;  // 新增（T3-6 使用）
}
```

新增 Dry-Run 上行消息（T3-6 需要，此处预留）：

```protobuf
message DryRunResult {
    string rule_id                    = 1;
    repeated DryRunFileResult files   = 2;
    string error                      = 3;
}

message DryRunFileResult {
    string local_path               = 1;
    string upload_path              = 2;
    map<string, string> parsed_fields = 3;
    string compose_error            = 4;
}
```

`AgentMessage.payload` 追加：

```protobuf
DryRunResult dry_run_result = 15;
```

重新生成 Go pb 代码：`protoc` 生成 `api/v1/agent.pb.go` + `agent_grpc.pb.go`。

**验收**：`go build ./...`（根、agent、controlplane 三模块）全部通过。

#### T3-5-C：Agent 字段名同步

**文件**：`agent/internal/scheduler/scheduler.go`、`agent/cmd/agent/main.go`

`CollectionRule` Go 结构体字段重命名：

| 原字段名 | 新字段名 |
|---------|---------|
| `SourcePathTemplate` | `BasePath` |
| `FileGlob` | `PathPattern` |
| `UploadPathTemplate` | `DestPathTemplate` |
| `WatchRecursive` | `Recursive` |
| `WatchSubdirPattern` | 删除 |

`main.go` `protoToRule()` 同步更新：

```go
func protoToRule(r *agentv1.CollectionRule) scheduler.CollectionRule {
    return scheduler.CollectionRule{
        RuleID:           r.GetRuleId(),
        Name:             r.GetName(),
        Mode:             r.GetMode(),
        BasePath:         r.GetBasePath(),
        PathPattern:      r.GetPathPattern(),
        UploadBucket:     r.GetUploadBucket(),
        DestPathTemplate: r.GetDestPathTemplate(),
        Recursive:        r.GetRecursive(),
        CronExpr:         r.GetCronExpr(),
        RunOnceOnStart:   r.GetRunOnceOnStart(),
        AppendMode:       r.GetAppendMode(),
        Enabled:          r.GetEnabled(),
    }
}
```

#### T3-5-D：Bug 4 修复 — `append_mode` 值域统一（Agent 侧）

**文件**：`agent/internal/watcher/watcher.go`、`agent/internal/queue/queue.go`、`agent/cmd/agent/main.go`

- 常量 `AppendModeNone = ""` → `AppendModeOverwrite = "overwrite"`
- SQLite DDL 默认值：`DEFAULT ''` → `DEFAULT 'overwrite'`
- 所有 `appendMode == ""` 的判断改为 `appendMode == AppendModeOverwrite`
- 测试中 `"none"` 字面量改为 `"overwrite"`（`main_test.go` 等）

#### T3-5-E：`matchGlob` / `walkAndSubmit` 升级为相对路径匹配

**文件**：`agent/cmd/agent/main.go`（`matchGlob`、`walkAndSubmit`）、`agent/go.mod`

引入 `github.com/bmatcuk/doublestar/v4`，替换 `filepath.Match` → `doublestar.Match`。

**匹配逻辑**：
1. 计算 `relPath = filepath.Rel(rule.BasePath, absPath)`（使用 `/` 分隔符）
2. 若 `IsTrollsiftPattern(rule.PathPattern)`：`globPat = parser.Globify()`，用 `doublestar.Match(globPat, relPath)`
3. 否则：直接 `doublestar.Match(rule.PathPattern, relPath)`

**walkAndSubmit 同步修改**：将 `filepath.Base` 匹配改为 relPath 匹配，逻辑同上。

#### T3-5-F：`buildStoragePath` 重写为 trollsift Compose 流程

**文件**：`agent/cmd/agent/main.go`（新函数 `buildStoragePath`，原 `buildStoragePath` 或 `scheduler.ResolvePath` 标注 deprecated）

```
输入：rule.DestPathTemplate, rule.BasePath, rule.PathPattern, absPath, agentCtx, now time.Time
输出：storagePath string, error

流程：
  1. relPath = filepath.Rel(rule.BasePath, absPath)，统一 '/' 分隔
  2. fields = map[string]Value{}
  3. 若 IsTrollsiftPattern(rule.PathPattern)：
       p, _ = trollsift.New(rule.PathPattern)
       parsed, _ = p.Parse(relPath)
       for k,v in parsed: fields[k] = v
  4. fields = InjectContext(agentCtx, fields)
  5. fields["filename"] = S(filepath.Base(absPath))
  6. fields["ext"] = S(strings.TrimPrefix(filepath.Ext(absPath), "."))
  7. 若 DestPathTemplate 含 "{time"：fields["time"] = T(now)（或相应字段名）
  8. p2, _ = trollsift.New(rule.DestPathTemplate)
  9. return p2.Compose(fields, false)
```

`scheduler.ResolvePath()` 添加 `// Deprecated: use buildStoragePath instead` 注释。

#### T3-5-G：CP `createRuleRequest` / `toRuleResponse` 字段统一（Bug 1 + Bug 3）

**文件**：`controlplane/internal/api/handler/rules.go`（或同等位置）

```go
type createRuleRequest struct {
    BucketID         string `json:"bucket_id" binding:"required"`
    Name             string `json:"name" binding:"required"`
    Mode             string `json:"mode" binding:"required"`
    BasePath         string `json:"base_path" binding:"required"`
    PathPattern      string `json:"path_pattern" binding:"required"`
    DestPathTemplate string `json:"dest_path_template" binding:"required"`
    Recursive        bool   `json:"recursive"`
    CronExpr         string `json:"cron_expr"`
    RunOnceOnStart   bool   `json:"run_once_on_start"`
    AppendMode       string `json:"append_mode"`
    Enabled          *bool  `json:"enabled"`  // nil → 默认 true
}
```

Bug 3 修复：handler 中 `mode = strings.ToLower(req.Mode)`，写入 DB 前归一化。

`toRuleResponse()` 同步更新输出字段名：
- `source_path` / `source_path_template` → `base_path`
- `file_pattern` / `file_glob` → `path_pattern`
- `upload_path_template` → `dest_path_template`
- `watch_recursive` → `recursive`
- `is_active` → `enabled`
- `dest_bucket_id` → `bucket_id`（REST-out 前端展示字段）

#### T3-5-H：CP `dispatch.go` 修复 Bug 2（`upload_bucket` 始终为空）

**文件**：`controlplane/internal/dispatch/dispatch.go`（及同模块接口）

给 `Dispatcher` 注入 `BucketQuerier` 接口（包含 `GetBucketByID` 方法）：

```go
type BucketQuerier interface {
    GetBucketByID(ctx context.Context, id uuid.UUID) (*db.Bucket, error)
}
```

`ruleToProto()` 变更为异步查询：

```go
func (d *Dispatcher) ruleToProto(ctx context.Context, rule *db.CollectionRule) (*agentv1.CollectionRule, error) {
    bucket, err := d.buckets.GetBucketByID(ctx, rule.BucketID)
    if err != nil {
        return nil, fmt.Errorf("dispatch: get bucket for rule %s: %w", rule.ID, err)
    }
    return &agentv1.CollectionRule{
        RuleId:           rule.ID.String(),
        Name:             rule.Name,
        Mode:             string(rule.Mode),
        BasePath:         rule.BasePath,
        PathPattern:      rule.PathPattern,
        DestPathTemplate: rule.DestPathTemplate,
        UploadBucket:     bucket.Name,     // ← 修复
        Recursive:        rule.Recursive,
        CronExpr:         rule.CronExpr.String,
        RunOnceOnStart:   rule.RunOnceOnStart,
        AppendMode:       rule.AppendMode,
        Enabled:          rule.Enabled,
    }, nil
}
```

#### T3-5-I：WebUI 字段映射修复

**文件**：`webui/src/services/agents.ts`、`webui/src/pages/Agents/RuleForm.tsx`

`agents.ts`：`CollectionRulePayload` / `CollectionRule` 接口同步字段名：
- `dest_bucket_id` → `bucket_id`
- `source_path` → `base_path`
- `file_pattern` → `path_pattern`
- `upload_path_template` → `dest_path_template`（已正确，确认）
- `is_active` → `enabled`
- 新增 `recursive: boolean`
- 新增 `append_mode: string`（可选，默认 `'overwrite'`）

`RuleForm.tsx` Step 2 补全缺失字段：
- 新增 `recursive` Switch 控件（"递归监控子目录"）
- 新增 `append_mode` Select 控件，选项：`overwrite`（全量）/ `tail`（追加尾部）/ `close_wait`（写完后上传）
- `path_pattern` 字段 Tooltip 说明：支持 glob（`*.csv`、`**/*.csv`）和 trollsift 结构化模式（含 `{}` 时）

`RuleForm.tsx` Step 3 WebUI 字段名同步（`dest_path_template` 等）。

### T3-5 验收标准

| # | 验收项 | 方法 |
|---|--------|------|
| T3-5-1 | DB 迁移 up/down 均无错误 | `migrate up` + `migrate down` |
| T3-5-2 | `go build ./...` 三个模块无编译错误 | `make build` |
| T3-5-3 | 创建采集规则 API（POST /api/v1/rules）返回 201 | curl 测试 |
| T3-5-4 | 规则下发：`upload_bucket` 字段非空 | Agent 日志确认 |
| T3-5-5 | `append_mode` 三端一致：DB `'overwrite'`，Agent `AppendModeOverwrite`，proto `'overwrite'` | 单元测试 |
| T3-5-6 | `mode` 前端传 `"WATCH"`，DB 存 `'watch'` | 集成测试 |
| T3-5-7 | `pnpm test` 全通过 | WebUI 测试 |
| T3-5-8 | `go test ./...` agent + controlplane 全通过 | 单元测试 |

---

## T3-6 — Dry-Run 规则测试功能

**涉及模块**：controlplane（dryRunStore + REST 端点）、agent（handleDryRun）、webui（RuleForm Step 3 测试面板）  
**前置依赖**：T3-5 完成（新 proto 字段 `dry_run` 已就绪）  
**参考章节**：`system-design.md` §§5.5（规则分发），4.1（Agent 指令处理）

### 背景

允许运营人员在**保存规则前**，直接在 WebUI 看到当前填写的路径模板会匹配哪些文件、以及上传后的路径是什么，无需先部署再验证。

### REST 端点规格

```
POST /api/v1/agents/:id/test-rule
Authorization: Bearer <token>
Content-Type: application/json

Request Body:
{
  "base_path":          "/data/sensors",
  "path_pattern":       "{sensor_id}/{time:yyyy-MM-dd}/*.csv",
  "dest_path_template": "raw/{sensor_id}/{time:yyyy/MM/dd}/{filename}",
  "recursive":          true,
  "dry_run_limit":      10   // 可选，默认 10，最大 50
}

Response 200:
{
  "files": [
    {
      "local_path":    "/data/sensors/A001/2024-03-15/temp.csv",
      "upload_path":   "raw/A001/2024/03/15/temp.csv",
      "parsed_fields": { "sensor_id": "A001", "time": "2024-03-15" }
    }
  ]
}

Response 409: { "error": "AGENT_OFFLINE" }
Response 504: { "error": "TIMEOUT", "message": "agent did not respond within 30s" }
Response 422: { "error": "INVALID_PATTERN", "message": "..." }
```

### CP 实现：`dryRunStore`

仿 `dirstore` 模式（`controlplane/internal/dirstore/store.go`）：

```go
// dryRunStore 保存 dry-run 请求的 pending channel，key = 临时 rule_id（UUID）
type dryRunStore struct {
    mu      sync.Mutex
    pending map[string]chan *agentv1.DryRunResult
}

func (s *dryRunStore) register(reqID string) chan *agentv1.DryRunResult
func (s *dryRunStore) deliver(reqID string, result *agentv1.DryRunResult) bool
func (s *dryRunStore) cancel(reqID string)
```

**`TestRule` handler 流程**（`controlplane/internal/api/handler/agents.go`）：

1. 解析请求 body，校验字段合法性
2. 检查 agent 在线（Redis TTL），离线返回 409
3. 生成临时 `ruleID = uuid.New()`
4. `dryRunStore.register(ruleID)` 注册 channel
5. 构建 `PushRuleCommand{..., DryRun: true}`，通过 gRPC 推送给 agent
6. `select` 等待 channel 或 30s 超时
7. 返回 `DryRunResult` 内容（转换字段名）；无副作用，不写 DB

**gRPC handler 追加 case**（`controlplane/internal/grpcserver/handler.go`）：

```go
case *agentv1.AgentMessage_DryRunResult:
    d := payload.DryRunResult
    if !s.dryRunStore.deliver(d.GetRuleId(), d) {
        logger.Warn("dry_run: no pending request", zap.String("rule_id", d.GetRuleId()))
    }
```

### Agent 实现：`handleDryRun`

**文件**：`agent/cmd/agent/main.go`

当收到 `dry_run=true` 的 `PushRuleCommand` 时，调用 `handleDryRun` 而不是 `runWatcher`/`runScheduled`：

```go
func handleDryRun(
    ctx context.Context,
    stream agentv1.AgentService_ConnectClient,
    rule scheduler.CollectionRule,
    limit int,
    agentCtx trollsift.AgentContext,
    logger *zap.Logger,
) {
    result := &agentv1.DryRunResult{RuleId: rule.RuleID}

    p, err := trollsift.New(rule.PathPattern)   // 可为 glob 或 trollsift
    if err != nil {
        result.Error = err.Error()
        sendDryRunResult(stream, result)
        return
    }
    globPat := p.Globify()

    count := 0
    _ = filepath.WalkDir(rule.BasePath, func(path string, d fs.DirEntry, err error) error {
        if err != nil || d.IsDir() { return nil }
        if count >= limit { return filepath.SkipAll }

        relPath, _ := filepath.Rel(rule.BasePath, path)
        relPath = filepath.ToSlash(relPath)

        matched, _ := doublestar.Match(globPat, relPath)
        if !matched { return nil }

        fileResult := &agentv1.DryRunFileResult{LocalPath: path}

        fields := map[string]trollsift.Value{}
        if trollsift.IsTrollsiftPattern(rule.PathPattern) {
            parsed, pErr := p.Parse(relPath)
            if pErr == nil {
                fields = parsed
            }
        }
        fields = trollsift.InjectContext(agentCtx, fields)
        fields["filename"] = trollsift.S(filepath.Base(path))
        fields["ext"] = trollsift.S(strings.TrimPrefix(filepath.Ext(path), "."))

        destParser, dErr := trollsift.New(rule.DestPathTemplate)
        if dErr == nil {
            uploadPath, cErr := destParser.Compose(fields, false)
            if cErr != nil {
                fileResult.ComposeError = cErr.Error()
            } else {
                fileResult.UploadPath = uploadPath
            }
        } else {
            fileResult.ComposeError = dErr.Error()
        }

        for k, v := range fields {
            fileResult.ParsedFields[k] = v.Str  // 简化为字符串展示
        }

        result.Files = append(result.Files, fileResult)
        count++
        return nil
    })

    sendDryRunResult(stream, result)
}
```

**`config.toml` 新增**：

```toml
[collection]
dry_run_limit = 10   # 单次 dry-run 最多返回的文件数；上限 50
```

### WebUI：RuleForm.tsx Step 3 测试面板

在"路径配置"步骤（Step 3）的 `dest_path_template` 输入框下方新增：

**"规则测试"折叠区域（Collapse 默认收起）**：
- "立即测试"按钮（loading 状态）
- 点击 → 调用 `POST /api/v1/agents/:id/test-rule`，传入 Step 2+3 当前表单值
- 结果区域：

  | 本地路径 | 解析字段 | 上传路径 | 状态 |
  |---------|---------|---------|------|

- 状态处理：
  - loading：Button loading + Spin
  - 成功且有文件：表格展示（最多 10 行）
  - 成功无匹配：`<Empty description="未找到匹配文件" />`
  - Agent 离线（409）：`<Alert type="warning" message="采集器当前离线，无法预览" />`
  - 超时（504）：`<Alert type="error" message="采集器响应超时（30s）" />`
  - 模式错误（422）：`<Alert type="error" message={error} />`

新增 `agents.ts` 方法：

```ts
export function testRule(agentId: string, params: TestRuleParams): Promise<TestRuleResult> {
    return request<TestRuleResult>(`/api/v1/agents/${agentId}/test-rule`, {
        method: 'POST',
        data: params,
    });
}
```

### T3-6 验收标准

| # | 验收项 | 方法 |
|---|--------|------|
| T3-6-1 | Agent 在线时，TestRule 返回 200 含匹配文件列表 | curl / WebUI 操作 |
| T3-6-2 | Agent 离线时，TestRule 返回 409 | 停止 agent 后 curl |
| T3-6-3 | 30s 无响应，TestRule 返回 504 | mock agent 不回复 |
| T3-6-4 | `dry_run_limit=3` 时最多返回 3 条 | curl 测试 |
| T3-6-5 | WebUI Step 3 显示测试面板，结果正常渲染 | 手动验收 |
| T3-6-6 | dry-run 不写 DB（file_entries / upload_logs 无新增行） | DB 查询确认 |
| T3-6-7 | `go test ./...` 含 dryRunStore 单元测试 | 单元测试 |

---

## 关联入口

- 设计方案：`docs/plan.md`
- 当前 Phase 主线：`docs/tasks/phases/phase-3.md`
- 系统设计：`docs/design/system-design.md`
- 技术决策：`DECISIONS.md`（D-009、D-010）
