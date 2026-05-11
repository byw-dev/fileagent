你已经丢失过往讨论的上下文。我在这里手动补充：
1. 你给的最后一版完整方案：
```
<plan>

## 完整更新方案（第三版）

---

### 核心数据流总览

```
source_path_template ──(trollsift.Globify() / 直接当glob)──► base_path + glob_str ──► 文件列表
file_glob ──(trollsift.Parse() / filepath.Match)──► 匹配过滤 + 提取字段
target_path_template ──(trollsift.Compose())──► MinIO 上传路径
```

---

### 一、`pkg/trollsift/` 共享库

#### 1.1 包位置

```
fileagent/pkg/trollsift/
├── parser.go       # Parser: New, Parse, Compose, Globify, Validate
├── field.go        # field 类型 / parseField / parseFormat
├── regex.go        # buildRegex / fieldRegex
├── format.go       # convertLDML / formatValue
├── ldml.go         # LDML → Go time layout；ldmlGlobify；dtRegexFromLDML
├── context.go      # AgentContext / InjectContext
└── parser_test.go
```

以 Go workspace（go.work）方式，agent 和 controlplane 均直接 import `github.com/byw-dev/fileagent/pkg/trollsift`。

#### 1.2 模式检测函数（跨模块复用）

```go
// IsTrollsiftPattern returns true if s contains '{', i.e. is a trollsift pattern,
// false if it should be treated as a plain glob string.
func IsTrollsiftPattern(s string) bool { return strings.ContainsRune(s, '{') }
```

所有双模式字段（`source_path_template`、`file_glob`）均调用此函数判断。

#### 1.3 API 接口

```go
// New 解析格式串，返回 Parser 或错误（格式串非法时）
func New(pattern string) (*Parser, error)

// Validate 检测格式串是否合法（不执行 Parse）
func (p *Parser) Validate() error

// Parse 从字符串 s 提取字段值，返回 map
func (p *Parser) Parse(s string) (map[string]Value, error)

// Compose 将字段值 map 格式化到格式串中，allowPartial=true 时允许缺失字段
func (p *Parser) Compose(vals map[string]Value, allowPartial bool) (string, error)

// Globify 将格式串转为 glob 字符串，用于文件系统 glob 操作
// 时间字段 → 等宽 ? 序列（e.g. yyyy → ????, MM → ??）
// 字符串字段 → *
// 整数字段（有宽度限制）→ 等宽 ? 序列；无宽度 → *
func (p *Parser) Globify() string
```

#### 1.4 LDML 时间字段格式

字段格式串支持 Unicode LDML (UTS #35) 子集（单个 `{time:...}` 字段内）：

| 符号 | 含义 | glob宽度 |
|------|------|----------|
| `yyyy` | 4 位年 | `????` |
| `yy` | 2 位年 | `??` |
| `MM` | 月（01-12） | `??` |
| `dd` | 日（01-31） | `??` |
| `HH` | 时（00-23） | `??` |
| `mm` | 分（00-59） | `??` |
| `ss` | 秒（00-59） | `??` |

时区：字段格式串末尾可附加 `|tz=IANA`，例如 `{time:yyyy/MM/dd_HHmmss|tz=Asia/Shanghai}`。未指定则 UTC。

#### 1.5 AgentContext 注入

```go
type AgentContext struct {
    AgentName string  // → {agent_name}
    AgentID   string  // → {agent_id}
}

// InjectContext merges agent metadata into vals; existing keys win.
func InjectContext(ctx AgentContext, vals map[string]Value) map[string]Value
```

#### 1.6 `Value` 类型

```go
type Value struct {
    Str   string;   IsStr   bool
    Int   int;      IsInt   bool
    Float float64;  IsFloat bool
    Time  time.Time; IsTime bool
}
func S(s string) Value { return Value{Str: s, IsStr: true} }
func I(n int) Value    { return Value{Int: n, IsInt: true} }
func T(t time.Time) Value { return Value{Time: t, IsTime: true} }
```

---

### 二、`source_path_template` — 路径定位逻辑

按用户最终确认方案：

**判断**：
- `source_path_template` 含 `{` → trollsift pattern → 调用 `parser.Globify()` 得到 `glob_str`
- `source_path_template` 不含 `{` → 直接作为 `glob_str`（原样使用，此时字段充当相对 glob 路径）

**Agent 端执行**：
```
resolved = base_path + "/" + glob_str
files = doublestar.Glob(resolved)   // 支持 ** 递归
```

现有 `scheduler.ResolvePath()` 函数标记为 deprecated，保留但不再被核心路径调用（避免破坏现有单元测试）。Agent 的 `applyRule` 函数在构建 Watcher 时改用新逻辑。

**文件基名过滤（`file_glob`）**：
- 含 `{` → `trollsift.New(file_glob).Validate()` 判断是否合法模式，然后用 `trollsift.Parse(basename)` 匹配并提取字段
- 不含 `{` → `doublestar.Match(file_glob, basename)`（替换现有 `filepath.Match`，以支持 `**/*.csv`）

---

### 三、`target_path_template` — 上传路径构建

`target_path_template` **始终**是 trollsift Compose 格式串。Agent 在 `buildStoragePath` 中：

1. 从 `file_glob`（若为 trollsift 模式）的 `Parse()` 结果取字段 map
2. 调用 `InjectContext(agentCtx, fields)` 注入 `{agent_name}`、`{agent_id}`
3. 注入文件元数据：`{filename}`（`filepath.Base(localPath)`）、`{ext}`（扩展名）、`{time:...}`（当前时间，时区信息从字段 spec 读取）
4. 调用 `parser.Compose(vals, allowPartial=false)` 得到最终 S3 路径

**废弃**：现有 `buildStoragePath` 中的 `scheduler.ResolvePath(rule.UploadPathTemplate, ...)` + 手动拼 base 的逻辑，完整替换为上述 trollsift 流程。

---

### 四、Dry-Run（规则测试）

#### 4.1 Proto 扩展

在 `CollectionRule` 消息中增加一个字段：

```protobuf
message CollectionRule {
    // ... 现有字段 1-13 不变 ...
    bool dry_run = 14;  // when true, agent performs match+compose only, no upload
    int32 dry_run_limit = 15;  // max files to process in dry_run (default 5 when 0)
}
```

`DryRunResult` 上行消息：

```protobuf
message DryRunResult {
    string rule_id = 1;
    repeated DryRunFileResult files = 2;
    string error = 3;
}

message DryRunFileResult {
    string local_path = 1;          // 匹配到的本地文件路径
    string upload_path = 2;         // Compose 结果
    map<string, string> parsed_fields = 3;  // 从 file_glob 解析到的字段（展示用）
    string compose_error = 4;       // 若 Compose 失败，错误信息
}
```

`AgentMessage.payload` 增加：
```protobuf
DryRunResult dry_run_result = 15;
```

`ServerMessage.payload` 复用现有 `PushRuleCommand`（其内的 `CollectionRule.dry_run=true` 传递），无需新消息类型。

#### 4.2 Agent 侧处理

`main.go` 中处理 `ServerMessage_PushRule` 时：

```go
case *agentv1.ServerMessage_PushRule:
    rule := protoToSchedulerRule(p.PushRule.GetRule())
    if p.PushRule.GetRule().GetDryRun() {
        go handleDryRun(p.PushRule.GetRule(), grpcClient, logger)
    } else {
        applyRule(rule)
    }
```

`handleDryRun` 函数：
1. 用新的路径解析逻辑（source_path_template → glob_str）在文件系统查找文件，**至多返回 `dry_run_limit`（默认 5）个**，用 `WalkDir` + early return 实现，不全量遍历
2. 对每个匹配文件执行 trollsift `Parse(basename)` + `InjectContext` + `Compose(target_path_template)`
3. 封装 `DryRunResult` 发回 CP

#### 4.3 CP 侧处理（request-response）

类似现有 `ListDir` 的 pending-map 模式：

```go
// 内部 dryRunStore（类似 dirstore），key=rule_id（临时 UUID），value=chan *agentv1.DryRunResult，TTL=30s
```

`handleAgentMessage` 增加 `AgentMessage_DryRunResult` case，将结果 deliver 到 pending channel。

`grpcserver/handler.go` 中也需要处理 `AgentMessage_DryRunResult`（向 dryRunStore 投递）。

#### 4.4 REST 端点

```
POST /api/v1/agents/:id/test-rule
Authorization: ******

Body（规则参数，rule_id 不需要，agent 使用临时 UUID）:
{
    "source_path_template": "/data/sensors",
    "file_glob": "{platform:4s}{num:2s}_{time:yyyyMMdd_HHmm}.csv",
    "target_path_template": "/{agent_name}/{time:yyyy/MM/dd|tz=Asia/Shanghai}/{filename}",
    "watch_recursive": false,
    "dry_run_limit": 5
}

Response 200:
{
    "files": [
        {
            "local_path": "/data/sensors/noaa16_20240210_1004.csv",
            "upload_path": "/my-agent/2024/02/10/noaa16_20240210_1004.csv",
            "parsed_fields": { "platform": "noaa", "num": "16", "time": "2024-02-10T10:04:00Z" }
        }
    ]
}
Response 409: { "error": "AGENT_OFFLINE" }
Response 504: { "error": "TIMEOUT" }   // 等待 30s
```

CP 端处理：
1. 生成临时 rule_id（UUID）
2. 向 agent 推送带 `dry_run=true` 的 `PushRuleCommand`
3. 等待 `dryRunStore` 中对应 channel，最多 30s
4. 返回结果

无需在 DB 存储，无副作用，可在规则创建前使用（不依赖已有 rule_id）。

---

### 五、WebUI 更新

#### 5.1 `utils/pathTemplate.ts` 重写

- 移除白名单变量枚举校验
- 改为轻量级语法校验：`{}` 平衡、字段名非空、LDML 格式串中 `|tz=` 后为非空字符串
- `renderPathPreview` 使用示例数据（不再调用 CP）做前端预览，时间字段按 LDML 格式展示示例

#### 5.2 RuleForm Step 2 更新

`file_pattern` 字段（对应 `file_glob`）：
- Tooltip 更新：支持两种模式——普通 glob（`*.csv`、`**/*.csv`）和 trollsift 模式（含 `{}` 则为结构化模式，可提取字段）
- 增加一个折叠面板"字段语法帮助"，列出 LDML 时间符号 + 类型格式说明

#### 5.3 RuleForm Step 3 新增"测试规则"面板

在 Step 3 的路径模板输入框下方，增加"测试规则"区域：
- 一个"立即测试"按钮（始终可点击，不需要先保存）
- 点击后 → 调用 `POST /api/v1/agents/:id/test-rule`，传入当前 Step2/Step3 表单值
- 结果展示：一个简洁表格，列出「本地路径 / 解析字段 / 上传路径」，每行一个匹配文件（最多 5 条）
- 状态：loading 中 / 成功 / Agent 离线 / 无匹配文件 / 超时

#### 5.4 可用变量帮助面板

在 Step 3 中，将现有"可用模板变量"Card 替换为分组展示：
- **上下文变量（自动注入）**：`{agent_name}` / `{agent_id}`
- **文件元数据**：`{filename}` / `{ext}`
- **日期时间示例**：`{time:yyyyMMdd_HHmm}` / `{time:yyyy/MM/dd|tz=Asia/Shanghai}`
- **从 file_glob 解析**：当 Step2 的 `file_pattern` 为 trollsift 模式时，实时解析并展示字段名列表（前端用简版 parser 提取 `{}` 内的字段名部分）

---

### 六、旧代码迁移点（不破坏已有行为）

| 现有代码 | 迁移方式 |
|---|---|
| `scheduler.ResolvePath()` | 保留，标注 deprecated；`buildStoragePath` 改为 trollsift Compose；`AddRule` callback 改为传原始 source_path_template 而不是 resolved path，改由 agent 内部 glob 时解析 |
| `watcher.matchGlob()` 中 `filepath.Match` | 替换为 `doublestar.Match`，支持 `**` |
| `walkAndSubmit` 中 `filepath.Match` | 同上 |
| `buildStoragePath` | 重写为 trollsift Compose 流程 |

---

### 七、实施顺序

1. `pkg/trollsift/` 核心库（含 LDML、Parse/Compose/Globify/Validate、时区、AgentContext）+ 测试 ≥ 90%
2. agent：引入 `doublestar`；`watcher.matchGlob`、`walkAndSubmit` 替换；`buildStoragePath` 改写
3. Proto：增加 `dry_run`/`dry_run_limit` 字段 + `DryRunResult`/`DryRunFileResult` 消息 + 重新生成 Go 代码
4. Agent：`handleDryRun` 函数实现（含 early-stop glob + trollsift compose）
5. CP：`dryRunStore` + `handleAgentMessage` 增加 `DryRunResult` case + `TestRule` REST 端点
6. WebUI：`pathTemplate.ts` 重写 + RuleForm Step2/Step3 更新 + 测试规则面板
7. `DECISIONS.md` 更新

---

### 八、还需你确认的点

只剩一个小问题：

**`source_path_template` 的语义**：你的描述是 `base_path` 单独配置、`source_path_template` 作为相对 glob 路径。但现有 DB 和 proto 中只有 `source_path_template` 一个字段，当前 agent 把它当做绝对目录路径直接传给 `watcher.New()`。

两个选项：
- **A**：拆分为 `base_path`（绝对目录）+ `source_path_template`（相对 glob 或 trollsift pattern），需要加 DB 列和 proto 字段。
- **B**：保持单字段，约定：若 `source_path_template` 不含 `{}`，则它本身就是绝对目录（现有行为）；若含 `{}`，则视为"绝对目录前缀 + trollsift 相对 pattern"的组合，即允许用户写 `/data/sensors/{device}/{date:yyyy/MM/dd}`（含绝对前缀），agent 从左至右找到第一个 `{` 的上层目录作为 base，其余作为 trollsift pattern 传给 Globify。

**B** 方案改动最小，无需改 DB/proto，建议采用。请确认。

</plan>
```
2. 我紧接着的回复：
```
我的意见：
1. 按照 `agent/cmd/agent/main.go:357` 这里的调用，`rule.SourcePathTemplate` 实际上就是 base_path，`rule.FileGlob` 就是 filename_pattern 字符串了。那么，按我的理解，无需改动了才对；
2. proto 里扩展 CollectionRule 消息，这是否意味着 dry_run_limit 实际上是由CP 下发通知给agent 的？可以的话，我们不要传递这个字段，将它作为agent 侧的一个配置项，从 config.toml 读取，默认是10；
3. 这样的改动，对原有采集规则里的字段以及关联逻辑，有很多出入。从DB 到CP 侧handlers 里的json 字段定义、proto 里的字段定义，非常混乱，如果可以的话，统一一下比较好；

以 `controlplane/internal/db/rules.sql.go:38` 为例：
```go
type CreateCollectionRuleParams struct {
	OrgID              uuid.UUID       `db:"org_id" json:"org_id"`
	AgentID            uuid.UUID       `db:"agent_id" json:"agent_id"`
	BucketID           uuid.UUID       `db:"bucket_id" json:"bucket_id"`
	Name               string          `db:"name" json:"name"`
	Mode               UploadMode      `db:"mode" json:"mode"`
	SourcePathTemplate string          `db:"source_path_template" json:"source_path_template"`
	FileGlob           string          `db:"file_glob" json:"file_glob"`
	UploadPathTemplate string          `db:"upload_path_template" json:"upload_path_template"`
	WatchRecursive     bool            `db:"watch_recursive" json:"watch_recursive"`
	WatchSubdirPattern sql.NullString  `db:"watch_subdir_pattern" json:"watch_subdir_pattern"`
	CronExpr           sql.NullString  `db:"cron_expr" json:"cron_expr"`
	RunOnceOnStart     bool            `db:"run_once_on_start" json:"run_once_on_start"`
	AppendMode         string          `db:"append_mode" json:"append_mode"`
	Metadata           json.RawMessage `db:"metadata" json:"metadata"`
}
```
分别解释下列字段的含义：
```
mode
source_path_template
file_glob
upload_path_template
watch_recursive
watch_subdir_pattern
cron_expr
run_once_on_start
append_mode
```
以及为什么我没能在 webui 上看到对应的配置功能？

注意：仍然不要做任何代码、文档的改动。直到我确定了方案
```
3. 你的回复：
```
## 问题 1：`SourcePathTemplate` = base_path，`FileGlob` = filename_pattern，无需改动？

**字段语义层面**：你的理解是对的。从 `runWatcher`（line 357）可以看到：

```go
watcher.New(rule.SourcePathTemplate, rule.FileGlob, rule.WatchRecursive, ...)
//          ↑ 作为 sourcePath（目录）      ↑ 作为 fileGlob（文件名过滤）
```

字段语义确实是 `SourcePathTemplate` = 监控目录，`FileGlob` = 文件名过滤模式，这两个字段不需要改。

**但实现层面有一处需要改动**：当前 `watcher.matchGlob()` 只对 `filepath.Base(path)`（即**纯文件名**）做匹配，这意味着 `FileGlob` 不能包含路径分隔符。如果你希望用 trollsift 模式匹配**跨目录的相对路径**（比如 `{device}/{date:yyyy/MM/dd}/{seq:d}/*.csv`），匹配逻辑就需要改为：

- 用 `Globify()` 将 trollsift pattern 转成 glob 字符串
- 将 glob 匹配的对象从"文件名"改为"相对于 `SourcePathTemplate` 的相对路径"
- 使用 `doublestar.Match` 替换 `filepath.Match`，支持跨目录的 `**` 和 `?` 通配

所以结论是：**字段名不变，但 agent 侧的匹配实现需要升级**。

---

## 问题 2：`dry_run_limit` 作为 agent 侧配置，从 config.toml 读取

**完全同意**，这更合理：

- proto `CollectionRule` 中只增加一个 `bool dry_run = 14;`（表示本次是测试调用）
- `dry_run_limit`（最多测试 N 个文件）作为 `config.toml` 的 `[collection]` 段配置项，默认值为 `10`
- CP 下发规则时无需传 limit，agent 自己控制上限

---

## 问题 3：各字段含义，以及为什么 WebUI 看不到？

### 字段含义逐一解释

| 字段 | 含义 |
|------|------|
| `mode` | 采集模式：`WATCH`（实时监控，文件变化即触发）或 `SCHEDULED`（定时任务，按 cron 周期扫描） |
| `source_path_template` | Agent 上要监控的**绝对目录路径**（目前实际起的是 base_path 的作用，名字里的"template"因历史原因保留） |
| `file_glob` | 文件匹配模式，当前只对**文件名**（basename）做 glob 匹配，例如 `*.csv`，`data_*.log` |
| `upload_path_template` | 上传到 S3 的对象路径模板，运行时替换变量得到最终 S3 key，例如 `/{year}/{month}/{filename}` |
| `watch_recursive` | 是否递归监控子目录（仅 WATCH 模式有效）；为 false 时只监控 source_path_template 直属文件 |
| `watch_subdir_pattern` | （NULLABLE）可选的子目录过滤 glob，用于在递归场景下只进入匹配的子目录（当前 agent 实现暂未使用此字段） |
| `cron_expr` | （NULLABLE）标准 5 段 cron 表达式，仅 SCHEDULED 模式需要，例如 `0 * * * *`（每小时） |
| `run_once_on_start` | SCHEDULED 模式下，agent 启动时是否立即触发一次扫描，而不等到第一个 cron 周期到来 |
| `append_mode` | 追加模式：`none`（每次上传完整文件）/ `tail`（只上传新增字节，适合追加写日志）/ `close_wait`（防抖，等文件停止写入后再上传） |

### 为什么 WebUI 看不到？

存在**两个层面**的问题：

**① 前端表单缺字段**：WebUI `RuleForm.tsx` Step2 根本没有渲染以下控件：

- `watch_recursive`（是否递归）
- `watch_subdir_pattern`（子目录过滤）
- `append_mode`（追加模式）

这三个字段在 DB 和 proto 中都有，但 WebUI 创建规则时始终用默认值（`false` / 空 / `none`），用户无法配置。

**② 前后端字段名不一致（存在 Bug）**：`agents.ts` 的 `CollectionRulePayload` 里用的是前端自定义的字段名，而 CP 的 `createRuleRequest` 期望的是另一套字段名，**两者不匹配**：

| 前端发送的字段名 | 后端期望的字段名 |
|---|---|
| `dest_bucket_id` | `bucket_id` |
| `source_path` | `source_path_template` |
| `file_pattern` | `file_glob` |
| `dest_path_template` | `upload_path_template` |

这意味着当前"创建规则"的接口实际上会因字段名不匹配而失败（所有必填字段都会 binding 失败，返回 400）。这是一个已存在的 Bug，需要在本次统一整理时修复。

---

## 综合建议

基于以上分析，本次整理涉及以下几个方向（但不做任何改动，等你确认）：

1. **字段命名统一**：三层（DB / CP REST API / WebUI）统一到同一套字段名，推荐以 DB 为准（`source_path_template`、`file_glob`、`upload_path_template`、`bucket_id`），CP REST handler 和 WebUI service 同步修改
2. **WebUI 补全缺失字段**：Step2 增加 `watch_recursive`、`append_mode`（`watch_subdir_pattern` 为高级选项，可暂时隐藏）
3. **`upload_path_template` 改用 trollsift Compose**：升级 agent 端 `buildStoragePath`，`pathTemplate.ts` 的变量格式同步改为 trollsift 格式（如 `{agent_name}` → 保持，`{year}` → `{time:yyyy}` 或保持旧格式向下兼容）
4. **dry_run**：proto 只加 `bool dry_run = 14`，agent config 加 `[collection] dry_run_limit = 10`

请你确认这个方向，或者针对任何一条提出调整意见。
```
4. 我的回复：
```
关于问题3的字段定义，看起来 watch_subdir_pattern + file_glob 才算接近正在讨论的这套 `{device}/{date:yyyy/MM/dd}/{seq:d}/*.csv` 匹配方式。
例如：`source_path_template = "/mnt/datadir", watch_subdir_pattern = "*/????/??/??/*", file_glob = "*.csv" `
如果是这样的话，我们应该是把watch_subdir_pattern 也用起来，还是怎样，哪种方式更好？

另外，我还有一个问题：考虑到现在整个系统正在开发中，甚至不满足联调测试条件，我们能否积极大胆地直接改动原始的 proto 字段名称, DB 字段，前端字段？反正数据库里没有多少数据，也没有正在运行的系统。与其在开发阶段就生产太多历史遗留，不如一开始就解决掉一部分
```
5. 你的回复：
```
## 问题 1：`watch_subdir_pattern` + `file_glob` 组合，还是合并成一个字段？

先说关键发现：**`watch_subdir_pattern` 在整个 agent 代码里是死代码**。

- `scheduler.CollectionRule` 有这个字段（line 36）
- proto 有这个字段
- DB 有这个列
- 但 `watcher.go` 的 `matchGlob()` 完全不用它（line 340-350），`walkAndSubmit` 也不用它，agent 侧**从未读取过它**

所以你举的例子 `watch_subdir_pattern = "*/????/??/??/*"` 虽然语义上是对的，但当前实现根本不会生效。

**两种路径选择：**

| 方案 | 描述 | 优劣 |
|---|---|---|
| **A：激活 `watch_subdir_pattern`** | 保留两个字段，分别控制"进入哪些子目录"和"选中哪些文件名" | 语义分离清晰，但用户要填两个字段，且历史上已经是死代码了 |
| **B：合并为单个 `path_pattern`** | 删掉 `watch_subdir_pattern`，把 `file_glob` 改成对 **base_path 下的相对路径** 做匹配，改用 `doublestar.Match` 支持 `**`、`?`、`[]` 等通配 | 一个字段完成全部匹配，例如 `*/????/??/??/*/*.csv`；标准 glob 语义开发者熟悉；watcher 的匹配逻辑改动是一处集中的改动 |

**我的建议是 B**。`watch_subdir_pattern` 从未被使用过，证明这个两字段方案在实现层面就没有被跟上。合并为一个字段反而更干净，匹配逻辑也集中在一处。

---

## 问题 2：系统还在开发中，能否直接大胆地改字段名？

**完全可以，而且应该现在改。** 理由：

- 没有生产数据，Migration 直接 `DROP COLUMN`/`ADD COLUMN` 即可（或整张表重建）
- 没有在跑的外部系统依赖这些字段
- 一旦进入联调甚至上线，字段名改动就会变成破坏性变更，成本指数级增加
- 现在的字段命名确实存在不一致（DB vs REST JSON vs proto vs frontend 四套命名），这正是你感到"混乱"的根源

---

## 建议的统一字段命名方案

以下是三层（DB / proto / REST+Frontend）统一到同一套名称的提案：

| 概念 | **统一字段名** | 现在的 DB 字段 | 现在的 proto 字段 | 现在的 REST JSON |
|---|---|---|---|---|
| 采集模式 | `mode` | `mode` | `mode` | `mode` ✓ |
| 监控根目录 | `base_path` | `source_path_template` | `source_path_template` | `source_path_template` |
| 文件/路径过滤 | `path_pattern` | `file_glob` | `file_glob` | `file_glob` |
| S3 对象路径模板 | `dest_path_template` | `upload_path_template` | `upload_path_template` | `upload_path_template` |
| 递归监控 | `recursive` | `watch_recursive` | `watch_recursive` | `watch_recursive` |
| 子目录过滤 | ~~删除~~ | `watch_subdir_pattern` | `watch_subdir_pattern` | `watch_subdir_pattern` |
| Cron 表达式 | `cron_expr` | `cron_expr` | `cron_expr` | `cron_expr` ✓ |
| 启动时立即触发 | `run_once_on_start` | `run_once_on_start` | `run_once_on_start` | `run_once_on_start` ✓ |
| 追加模式 | `append_mode` | `append_mode` | `append_mode` | `append_mode` ✓ |
| 目标 Bucket | `bucket_id` | `bucket_id` | `upload_bucket` | 前端用 `dest_bucket_id` → 改为 `bucket_id` |

变化小结：
- `source_path_template` → `base_path`（去掉"Template"后缀，它本来就不是模板字符串，是实际路径）
- `file_glob` → `path_pattern`（语义扩展为相对路径 glob，而不只是文件名 glob）
- `upload_path_template` → `dest_path_template`（前端已经叫这个名字了，对齐它）
- `watch_recursive` → `recursive`（去掉冗余前缀，因为已经在 collection rule 上下文里）
- `watch_subdir_pattern` → **删除**（合并进 `path_pattern`）
- `upload_bucket`（proto 里叫这个） → `bucket_id`（统一用 ID，不用名称字符串）

---

如果你同意这个方案，后续改动的范围是：

1. **DB**：新建一个 migration，重命名/删除对应列（直接 `DROP + ADD` 即可，无历史数据顾虑）
2. **proto**：直接改字段名（field number 不变，字段名重命名不影响序列化兼容性——但既然没有在跑的系统，就连字段编号也可以一并整理）
3. **CP handler**：`createRuleRequest`、`collectionRuleResponse`、`toRuleResponse()` 对应字段名同步更新
4. **agent watcher**：`matchGlob` 改为对相对路径做 doublestar 匹配
5. **Frontend**：`CollectionRulePayload`、`RuleForm.tsx` 字段名同步，以及补上 `append_mode`、`recursive` 两个缺失的表单字段

请确认方向，我来执行。
```

6. 我的回复：
```
很好，我的意见：
1. 关于问题1，完全统一方案B；
2. 关于问题2，为什么不能直接原地修改呢？
3. 我怀疑还有更多字段名称不统一的问题
```

7. 你的回复：
```
## 问题 2：为什么不能直接原地改迁移文件？

**可以改，前提是同时重建数据库。**

`golang-migrate` 通过一张 `schema_migrations` 表记录哪些版本号已经执行过，**并不校验文件内容的 hash**（和 Flyway 不同）。所以直接改 `000001_init_schema.up.sql` 完全可行，只需要：

```bash
# 重置 DB，重新从头 apply
migrate -database "$DATABASE_URL" -path ./migrations force 0
migrate -database "$DATABASE_URL" -path ./migrations up
```

不需要新增 `000002_rename_xxx.sql`，直接原地改就是最干净的。

---

## 问题 3：还有多少命名不一致（完整审计）

做完全四层扫描后，发现的问题比预想多得多，而且有几个是**功能性 Bug**，不只是命名问题：

### `collection_rules` 字段的四层对比

| 概念 | DB | proto | REST 接收 (`createRuleRequest`) | REST 返回 (`collectionRuleResponse`) | Frontend |
|---|---|---|---|---|---|
| 源目录 | `source_path_template` | `source_path_template` | `source_path_template` | `source_path` | `source_path` |
| 文件过滤 | `file_glob` | `file_glob` | `file_glob` | `file_pattern` | `file_pattern` |
| 目标路径 | `upload_path_template` | `upload_path_template` | `upload_path_template` | `dest_path_template` | `dest_path_template` |
| 目标 Bucket | `bucket_id` (FK UUID) | `upload_bucket` (string) | `bucket_id` | `dest_bucket_id` | `dest_bucket_id` |
| 是否递归 | `watch_recursive` | `watch_recursive` | `watch_recursive` | `watch_recursive` | 缺失 |
| 子目录过滤 | `watch_subdir_pattern` | `watch_subdir_pattern` | `watch_subdir_pattern` | 缺失 | 缺失 |
| 启用状态 | `status` enum | `enabled` bool | — | `is_active` bool | `is_active` bool |
| 追加模式 | `append_mode` | `append_mode` | `append_mode` | 缺失 | 缺失 |
| 采集模式 | `mode` enum ('watch','scheduled') | `mode` string | `mode` string | `mode` string | `mode` ('WATCH','SCHEDULED' 大写) |

### 发现的 Bug（不只是命名问题）

**Bug 1 - 规则创建当前是断路的（frontend ↔ REST 字段名不匹配）**

前端发送：
```json
{ "dest_bucket_id": "...", "source_path": "...", "file_pattern": "...", "dest_path_template": "..." }
```
但后端 `createRuleRequest` 期望：
```json
{ "bucket_id": "...", "source_path_template": "...", "file_glob": "...", "upload_path_template": "..." }
```
后端收到的这些必填字段全是空字符串，`binding:"required"` 校验直接失败，规则永远创建不成功。

**Bug 2 - proto `upload_bucket` 永远为空**

`dispatch.go` 的 `ruleToProto()` 从来不设 `UploadBucket`，Agent 收到的 proto 里这个字段始终是空字符串，Agent 侧上传时用的 bucket 名为空。

**Bug 3 - Mode 大小写不一致**

前端发送 `"WATCH"` / `"SCHEDULED"`，但 DB enum 只接受 `'watch'` / `'scheduled'`。Handler 不做转换，直接写 `db.UploadMode("WATCH")`，会被 PostgreSQL 报错。

**Bug 4 - `append_mode` 值域不一致**

DB 默认值是 `'overwrite'`，Agent watcher 认识的值是 `''`/`'tail'`/`'close_wait'`，两边对不上。

---

## 建议：统一命名方案（直接原地改）

结合方案 B，所有层统一到这套命名：

| 概念 | **统一命名** | 各层现状 |
|---|---|---|
| 源目录 | `base_path` | DB:`source_path_template` / proto:同 / REST-in:同 / REST-out:`source_path` / FE:`source_path` |
| 文件/路径过滤 | `path_pattern` | DB:`file_glob` / proto:同 / REST-in:同 / REST-out:`file_pattern` / FE:`file_pattern` |
| 目标路径模板 | `dest_path_template` | DB:`upload_path_template` / proto:同 / REST-in:同 / REST-out:`dest_path_template` ✓ / FE:✓ |
| 目标 Bucket | `bucket_id` | DB:`bucket_id` ✓ / proto:`upload_bucket`(改成传UUID) / REST-in:`bucket_id` ✓ / REST-out:`dest_bucket_id` / FE:`dest_bucket_id` |
| 是否递归 | `recursive` | DB:`watch_recursive` / proto:同 / 去掉 `watch_` 前缀 |
| 子目录过滤 | 删除 | 合并入 `path_pattern` |
| 追加模式 | `append_mode` | 统一值域：`''`/`'tail'`/`'close_wait'` |
| 采集模式 | `mode` | 统一用小写 'watch'/'scheduled'，前后端一致 |
| 启用状态 | `enabled` | DB:`status` enum → 改成 `enabled` bool 字段 |

> **`enabled` vs `status`**：当前 `rule_status` 只有 `active`/`inactive` 两个值，本质是 boolean，改成 `enabled BOOLEAN` 更直接，也和 proto 的 `enabled` 字段、前端的 `is_active` 对齐。

---

如果你确认这个方案，改动范围涉及：

1. **Migration 000001** — 直接原地改列名：`source_path_template`→`base_path`、`file_glob`→`path_pattern`、`upload_path_template`→`dest_path_template`、`watch_recursive`→`recursive`、删除`watch_subdir_pattern`、`status rule_status`→`enabled BOOLEAN`，修正 `append_mode` 默认值
2. **proto** — 同步改字段名，`upload_bucket` 改为传 `bucket_id`（UUID string），加 `bucket_id` 字段
3. **DB models + sqlc** — 更新 Go 结构体字段名
4. **CP `dispatch.go`** — `ruleToProto` 补上 `BucketId`，字段名同步
5. **CP `agents.go`** — `createRuleRequest` 字段名统一（消除当前 Bug 1），`toRuleResponse` 字段名统一，增加 mode 大小写归一化（消除 Bug 3）
6. **Agent `main.go` + `scheduler.go`** — 字段名同步
7. **Frontend `agents.ts` + `RuleForm.tsx` + `Rules.tsx`** — 字段名同步

要开始执行吗？
```

8. 我的回复：
```
不要执行，我仍然有疑问：
1. 关于 “proto — 同步改字段名，upload_bucket 改为传 bucket_id（UUID string），加 bucket_id 字段” 这段，我有一个疑问，agent 端拿到 bucket_id(UUID) 怎么使用呢？按设计文档，是使用 STS 地址进行上传，可是 STS 地址是由CP 下发给agent 的吧？
```

9. 你的回复：
```
## 你的判断完全正确

`upload_bucket` 字段存的是 **MinIO bucket 的名称字符串**（不是 UUID），而且它是必须留在 proto 里的。原因如下：

### STS 凭据的实际分工

```
CP → Agent（Connect 流）：CredentialsPayload
  ├── endpoint      （MinIO 地址，如 minio:9000）
  ├── access_key    （STS AK）
  ├── secret_key    （STS SK）
  ├── session_token （STS token）
  └── expires_at    （过期时间）

CP → Agent（Connect 流）：PushRuleCommand → CollectionRule
  └── upload_bucket  ← Agent 用这个字段告诉 MinIO 写哪个 bucket
```

Agent 侧调用 `PutObject` 的代码（`uploader.go:224`）：
```go
info, err := u.store.PutObject(ctx, task.Bucket, task.StoragePath, ...)
//                                   ^^^^^^^^^^
//                           task.Bucket 来自 rule.UploadBucket
```

`task.Bucket` 的赋值在 `main.go:424`：
```go
task := &queue.UploadTask{
    ...
    Bucket: rule.UploadBucket,  // ← 直接用规则里的 bucket 名
}
```

**STS 凭据里没有 bucket 名**。Agent 无法从 STS token 推断该写哪个 bucket，必须从规则里获取。

### 我之前"改成 bucket_id"的建议是错的

Agent 只会 S3 API，不会查 CP 的数据库，无法把 UUID 转换为 bucket 名。把 `upload_bucket` 改成传 UUID 会让 Agent 无法工作。

### 真正的 Bug 是：dispatch.go 根本没有填这个字段

```go
// dispatch.go ruleToProto()
func ruleToProto(rule *db.CollectionRule) *agentv1.CollectionRule {
    return &agentv1.CollectionRule{
        RuleId:             rule.ID.String(),
        ...
        // UploadBucket 没有设置！rule 里只有 BucketID (UUID)，没有 bucket 名
        WatchRecursive:     rule.WatchRecursive,
        ...
    }
}
```

`db.CollectionRule` 只存了 `BucketID (UUID)`，dispatcher 不查 DB 就拿不到 bucket 名，所以一直发空字符串给 Agent。

### 正确修法

**proto `upload_bucket` 保持现状（bucket 名字符串），不改字段含义**。真正要修的是：

1. 给 `Dispatcher` 注入一个 `GetBucketByID` 的 DB 方法
2. `ruleToProto` 改为先查 bucket 名，再填入 `UploadBucket`

或者更简洁的方案：把 `RefreshCredentials` 的请求也改成只传 `agent_id`（去掉 `rule_id`），让 CP 自己维护 agent→bucket 的关联，同时在 `PushRuleCommand` 里把 bucket 名正确传下去。

---

## 修订后的统一命名方案

综合上面分析，proto 里的字段调整应该是：

| 字段 | 修改 |
|---|---|
| `upload_bucket` | **保留，字段语义不变**（bucket 名字符串）；但要修 dispatch.go 实际填充它 |
| `source_path_template` | 改名为 `base_path` |
| `file_glob` | 改名为 `path_pattern` |
| `upload_path_template` | 改名为 `dest_path_template` |
| `watch_recursive` | 改名为 `recursive`（去掉 `watch_` 前缀） |
| `watch_subdir_pattern` | 可保留或合并入 `path_pattern` |
| `enabled` | 已正确，无需改 |

你确认后我开始执行吗？
```