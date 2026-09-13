# phase-3-rft.md — 采集规则重构（Rule Field & Template）任务规格

> **状态**：📦 **历史规格（主体已落地，勿照此执行）**——主要交付物已存在
> （`webui/src/utils/pathTemplate.ts` 的 `validatePathTemplate`/`renderPathPreview`、`pkg/trollsift`），
> 但**并非逐条落地**：例如本文 §「`config.toml` 新增」要求的 `[collection].dry_run_limit`
> **至今未实现**（agent 硬编码 `defaultDryRunLimit = 10`，CP 收了该参数却从不下发
> ——已立 **IC-BUG-52**）。**部分细节另已被后续决策取代**（见下方警告）。
> 保留全文作为当时的规格原文，**不作为执行目标**；若要清理遗留项，请以代码为准逐条核对。  
> **父阶段**：Phase 3 集成联调  
> **优先级**：~~在 T3-2 完成后立即执行，阻塞 T3-3~~（已失效，见上方状态）  
> **设计参考**：`DECISIONS.md` D-009（字段统一命名）、D-010（trollsift 引入）、`docs/design/system-design.md` §§4.1, 4.5, 5.5, 5.6, 5.8

> ⚠️ **保留字已改名（2026-09-13，D-034 / PR #106）**：本文全篇约 15 处 `{time:…}` 示例现为
> **已废弃的别名**，正式名是 **`{submit_time:…}`**（旧名仍可渲染，语义等价）。另外，保留字的
> **裸形式**（`{submit_time}` / `{time}`，不带 LDML 格式）与**非时间类型引用**（如 `{time:s}`、
> `{time:3d}`）**已被三端一致拒绝**。⚠️ **注意区分**：本文示例里带 LDML 的 `{time:yyyy/MM/dd}` **仍可正常渲染**
> （deprecated 别名，语义等价于 `{submit_time:yyyy/MM/dd}`），**不会失败**；失败的只有裸形式与
> 非时间类型这两种写法。因此本文那段「`{字段名:LDML格式}`」的说明对普通字段名仍成立，
> 只是对保留字**少了「必须带格式、且必须是时间类型」这条约束**。
> **路径模板保留字的权威清单在
> [`docs/design/contracts.md`](../../design/contracts.md) V-3**（另见 `DECISIONS.md` D-034）。
> 本文未逐处回填——改写历史规格原文等于篡改当时的规格。

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
// 时间字段：解析后存为 time.Time（内部统一 UTC，但保留原始时区信息用于 Compose）。
// 若字段含 |tz=，解析时按该时区解释输入字符串，转换为 UTC 后存储。
func (p *Parser) Parse(s string) (map[string]Value, error)

// Compose 将字段值 map 格式化到格式串中，返回最终路径字符串。
// 时间字段：若 parser 中字段含 |tz=，则将时间值（UTC）转换为该时区后格式化输出。
// allowPartial=true：缺失字段保留原始占位符（适用于预览）。
// allowPartial=false：任何字段缺失均返回 error。
func (p *Parser) Compose(vals map[string]Value, allowPartial bool) (string, error)

// Globify 将格式串转为 doublestar 兼容的 glob 字符串。
// 转换规则：
//   {field}        未指定类型（默认字符串）→ "*"
//   {field:s}      字符串，无宽度          → "*"
//   {field:Ns}     字符串，固定宽度 N       → N 个 "?"
//   {field:d}      十进制整数，无宽度      → "*"
//   {field:Nd}     十进制整数，固定宽度 N  → N 个 "?"
//   {field:0Nd}    十进制整数，零填充宽度 N → N 个 "?"（例：{seq:05d} → "?????"）
//   {field:LDML}   时间，LDML 格式串       → 每个 LDML 符号替换为等宽 "?" 序列
//                  （路径中的 "/" 分隔符保留，非 LDML 字面量保留）
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

### 字段类型格式说明

| 格式串语法 | 说明 | Parse | Globify | Compose |
|-----------|------|-------|---------|---------|
| `{field}` 或 `{field:s}` | 字符串，无宽度限制 | 任意字符串 → `Str` | `*` | 原样输出 |
| `{field:Ns}` | 字符串，固定宽度 N（精确匹配 N 个字符）| 长度=N 的字符串 → `Str` | N 个 `?` | 原样输出（不填充）|
| `{field:d}` | 十进制整数，无宽度限制 | `[0-9]+` → `Int` | `*` | `strconv.Itoa(n)` |
| `{field:Nd}` | 十进制整数，固定宽度 N | `[0-9]{N}` → `Int` | N 个 `?` | `fmt.Sprintf("%Nd", n)` |
| `{field:0Nd}` | 十进制整数，零填充宽度 N（`printf` `%0Nd` 语义）| `[0-9]{N}` → `Int` | N 个 `?` | `fmt.Sprintf("%0*d", N, n)` |
| `{field:LDML}` | 时间字段（见 LDML 表）| 按 Go layout 解析 → `Time`（UTC） | LDML 符号→等宽 `?` | 转回 parser 时区再格式化 |

> **零填充整数示例**：`{seq:05d}` — Parse `"00001"` → `I(1)`；Compose `I(1)` → `"00001"`；Globify → `"?????"（5 个 "?"）`。

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

| LDML 符号 | 含义 | Go layout | Glob 宽度 |
|-----------|------|-----------|-----------|
| `yyyy` | 4 位年 | `2006` | `????` |
| `yy` | 2 位年 | `06` | `??` |
| `MM` | 月（01-12） | `01` | `??` |
| `dd` | 日（01-31） | `02` | `??` |
| `HH` | 时（00-23） | `15` | `??` |
| `mm` | 分（00-59） | `04` | `??` |
| `ss` | 秒（00-59） | `05` | `??` |

时区：`|tz=IANA`（如 `|tz=Asia/Shanghai`）；未指定则 UTC。

**Globify 规则**：LDML 格式串中每个符号替换为等宽 `?` 序列，格式串中的 `/` 分隔符和字面量字符保留。
例：`yyyy/MM/dd` → `????/??/??`；`yyyyMMdd` → `????????`；`yyyy/MM/dd/HHmmss` → `????/??/??/??????`。

### 验收标准

#### Globify 测试用例

以下测试全部断言 `New(pattern)` 无错误，且 `p.Globify()` 输出与期望一致：

| # | 输入 pattern | 期望 Globify 输出 | 说明 |
|---|---|---|---|
| G-1 | `{agent_name}/{time:yyyy/MM/dd}/{filename}` | `*/????/??/??/*` | 基础路径：字符串字段、时间字段、字符串字段 |
| G-2 | `{device}/{date:yyyy/MM/dd}/{seq:d}/{record_id:s}.csv` | `*/????/??/??/*/*.csv` | 时间 + 无宽度整数 + 无宽度字符串 |
| G-3 | `{device}/{date:yyyy/MM/dd/HHmmss}.csv` | `*/????/??/??/??????.csv` | 时间格式含路径分隔符；`HHmmss`= HH(2)+mm(2)+ss(2)=6 个 `?` |
| G-4 | `{device:3s}/data.csv` | `???/data.csv` | 固定宽度字符串字段（`3s`=3个`?`） |
| G-5 | `data/{date:yyyyMMdd}.csv` | `data/????????.csv` | 无路径分隔符的时间格式：4+2+2=8 个 `?` |
| G-6 | `{device}/{date:yyyy/MM/dd}/report-{seq:d}.csv` | `*/????/??/??/report-*.csv` | 字面量前缀 + 无宽度整数 |
| G-7 | `static/file.csv`（无 `{`）| `static/file.csv` | 纯字面量：原样返回 |
| G-8 | `{n:2d}/{m:4s}/{date:yy-MM}.log` | `??/????/??-??.log` | 固定宽度整数 + 固定宽度字符串 + 2位年 |
| G-9 | `{device}/{seq:05d}.csv` | `*/?????.csv` | 零填充整数（`05d`=5位 "?"）；Parse 接受 `"00001"`，Compose 输出 `"00001"` |

#### Parse / Compose 基础测试用例

以 `New("{agent_name}/{date:yyyy/MM/dd}/{filename}")` 创建 parser `p`：

| # | 操作 | 期望结果 |
|---|---|---|
| PC-1 | `p.Parse("prod-agent/2024/03/15/data.csv")` | `{agent_name:"prod-agent", date:time.Time(2024-03-15 00:00:00 UTC), filename:"data.csv"}` |
| PC-2 | `p.Compose({agent_name:"a", date:T(t₀), filename:"f.csv"}, false)` 其中 `t₀=2024-03-15 UTC` | `"a/2024/03/15/f.csv"` |
| PC-3 | `p.Compose({agent_name:"a", date:T(t₀)}, true)`（缺 filename） | `"a/2024/03/15/{filename}"`（allowPartial 保留占位符）|
| PC-4 | `p.Compose({agent_name:"a", date:T(t₀)}, false)`（缺 filename） | 返回包含 `"filename"` 的 error |

#### 时区 Parse 测试用例

| # | Pattern | 输入字符串 | 期望解析出的 UTC 时间 | 说明 |
|---|---|---|---|---|
| TZ-1 | `{device}/{date:yyyy/MM/dd/HHmmss\|tz=Asia/Shanghai}.csv` | `"sensor1/2024/01/01/120000.csv"` | `2024-01-01 04:00:00 UTC` | CST (UTC+8)：12:00-8=04:00 UTC |
| TZ-2 | `{device}/{date:yyyy-MM-dd\|tz=America/New_York}.csv` | `"sensor1/2024-01-01.csv"` | `2024-01-01 05:00:00 UTC` | EST (UTC-5)：00:00+5=05:00 UTC |
| TZ-3 | `{device}/{date:yyyy/MM/dd/HHmmss\|tz=UTC}.csv` | `"sensor1/2024/01/01/120000.csv"` | `2024-01-01 12:00:00 UTC` | UTC 直接使用 |

> **注**：TZ-2 中 `2024-01-01` 只有日期，时间分量视为 `00:00:00`；New York 1月份为 EST（UTC-5）。

#### 跨时区 Compose 测试用例

目的：验证 Parse 内部以 UTC 存储时间，Compose 按目标 parser 的时区重新格式化输出。

```
ParserA = New("{device}/{date:yyyy/MM/dd/HHmmss|tz=Asia/Shanghai}.csv")
ParserB = New("{device}/{date:yyyy/MM/dd/HHmmss|tz=Asia/Tokyo}.csv")

input = "sensor1/2024/01/01/120000.csv"
valsA, _ = ParserA.Parse(input)
// valsA["date"] = T(2024-01-01 04:00:00 UTC)  [CST 12:00 → UTC 04:00]

outA, _ = ParserA.Compose(valsA, false)
// 期望：outA = "sensor1/2024/01/01/120000.csv"  [UTC 04:00 → CST 12:00 → 输出不变]

outB, _ = ParserB.Compose(valsA, false)
// 期望：outB = "sensor1/2024/01/01/130000.csv"  [UTC 04:00 → JST 13:00]
```

验收：`outA == "sensor1/2024/01/01/120000.csv"` 且 `outB == "sensor1/2024/01/01/130000.csv"`。

#### 边界 / 错误测试用例

| # | 输入 | 期望结果 |
|---|---|---|
| E-1 | `New("{foo")`（不平衡括号）| 返回语法错误 |
| E-2 | `New("{}")`（空字段名）| 返回语法错误 |
| E-3 | `New("{t:yyyy-MM-dd\|tz=}")`（tz 值为空）| 返回语法错误 |
| E-4 | `New("{t:yyyy-MM-dd\|tz=Invalid/Zone}")`（无效 IANA）| `New` 成功（延迟到 Parse/Compose 时校验），或立即返回错误（实现可选） |
| E-5 | `InjectContext` 不覆盖已有字段 | `vals["agent_name"]` 已有值时不被替换 |

#### 零填充整数（`0Nd`）测试用例

以 `New("{device}/{seq:05d}.csv")` 创建 parser `pz`：

| # | 操作 | 期望结果 |
|---|---|---|
| ZP-1 | `pz.Globify()` | `"*/?????.csv"`（5 个 `?`）|
| ZP-2 | `pz.Parse("sensor1/00001.csv")` | `{device:"sensor1", seq:I(1)}`（整数值 1）|
| ZP-3 | `pz.Parse("sensor1/99999.csv")` | `{device:"sensor1", seq:I(99999)}`（整数值 99999）|
| ZP-4 | `pz.Compose({device:S("sensor1"), seq:I(1)}, false)` | `"sensor1/00001.csv"`（零填充还原）|
| ZP-5 | `pz.Compose({device:S("sensor1"), seq:I(99999)}, false)` | `"sensor1/99999.csv"` |
| ZP-6 | `pz.Parse("sensor1/abc.csv")`（非数字）| 返回 parse 错误 |

| 覆盖率 | `go test ./...` in `pkg/trollsift/` | 行覆盖率 ≥ 90% |

---

## T3-5 — 字段统一 + Bug 修复

**涉及模块**：controlplane（DB migrations + sqlc + handler + dispatch）、agent（main + scheduler + watcher + queue）、proto、webui  
**前置依赖**：T3-4 完成（trollsift 库可引用）  
**参考章节**：`system-design.md` §§5.5, 5.6, 4.1, 4.4

### 子任务

#### T3-5-IMPL-A：DB 迁移

**文件**：`controlplane/migrations/`

新增迁移文件 `000002_rename_rule_fields.up.sql`（不修改 000001，保持迁移历史）。

**重要约束**：`collection_rules.status` 字段的类型 `rule_status`（枚举值 `'active'`/`'inactive'`）**保留不变**，以便后期扩展更多状态（如 `'paused'`）。`enabled` bool 语义仅在应用层转换，不写入 DB。

```sql
-- T3-5-IMPL-A: 采集规则字段重命名，保留 status rule_status 枚举类型
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

-- status rule_status 枚举类型保留，不改为 bool
-- 应用层转换：status='active' → enabled=true；status='inactive' → enabled=false

COMMIT;
```

同步创建 `000002_rename_rule_fields.down.sql`（回滚脚本）。

**sqlc 生成代码同步更新**：
- `controlplane/internal/db/queries/rules.sql`：字段名同步
- `controlplane/internal/db/rules.sql.go`：手动更新（沙箱内无法运行 sqlc）
- `db.CollectionRule` 结构体仍保留 `Status RuleStatus` 字段

**`enabled` 的转换位置**：
- `toRuleResponse()`（REST 输出）：`Enabled: rule.Status == db.RuleStatusActive`
- `ruleToProto()`（proto 下发）：`Enabled: rule.Status == db.RuleStatusActive`
- `createRuleRequest.Enabled *bool`（REST 输入）：写入 DB 时 `true → 'active'`，`false → 'inactive'`

**验收**：`migrate up` 后 `collection_rules` 表保留 `status rule_status` 字段，新字段名正确；`migrate down` 可完整回滚。

#### T3-5-IMPL-B：Proto 字段重命名

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

#### T3-5-IMPL-C：Agent 字段名同步

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

#### T3-5-IMPL-D：Bug 4 修复 — `append_mode` 值域统一（Agent 侧）

**文件**：`agent/internal/watcher/watcher.go`、`agent/internal/queue/queue.go`、`agent/cmd/agent/main.go`

- 常量 `AppendModeNone = ""` → `AppendModeOverwrite = "overwrite"`
- SQLite DDL 默认值：`DEFAULT ''` → `DEFAULT 'overwrite'`
- 所有 `appendMode == ""` 的判断改为 `appendMode == AppendModeOverwrite`
- 测试中 `"none"` 字面量改为 `"overwrite"`（`main_test.go` 等）

#### T3-5-IMPL-E：`matchGlob` / `walkAndSubmit` 升级为相对路径匹配

**文件**：`agent/cmd/agent/main.go`（`matchGlob`、`walkAndSubmit`）、`agent/go.mod`

引入 `github.com/bmatcuk/doublestar/v4`，替换 `filepath.Match` → `doublestar.Match`。

**匹配逻辑**：
1. 计算 `relPath = filepath.Rel(rule.BasePath, absPath)`（使用 `/` 分隔符）
2. 若 `IsTrollsiftPattern(rule.PathPattern)`：`globPat = parser.Globify()`，用 `doublestar.Match(globPat, relPath)`
3. 否则：直接 `doublestar.Match(rule.PathPattern, relPath)`

**walkAndSubmit 同步修改**：将 `filepath.Base` 匹配改为 relPath 匹配，逻辑同上。

#### T3-5-IMPL-F：`buildStoragePath` 重写为 trollsift Compose 流程

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

#### T3-5-IMPL-G：CP `createRuleRequest` / `toRuleResponse` 字段统一（Bug 1 + Bug 3）

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
- `is_active`（旧） → `enabled: rule.Status == db.RuleStatusActive`（枚举→bool，DB status 字段保留不变）
- `dest_bucket_id` → `bucket_id`（REST-out 前端展示字段）

`createRuleRequest` 输入时，`enabled` bool → 写入 DB 时转换为 `RuleStatus`：`true → 'active'`，`false → 'inactive'`（nil 默认 `'active'`）。

#### T3-5-IMPL-H：CP `dispatch.go` 修复 Bug 2（`upload_bucket` 始终为空）

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
        UploadBucket:     bucket.Name,                          // ← 修复 Bug 2
        Recursive:        rule.Recursive,
        CronExpr:         rule.CronExpr.String,
        RunOnceOnStart:   rule.RunOnceOnStart,
        AppendMode:       rule.AppendMode,
        Enabled:          rule.Status == db.RuleStatusActive,   // ← 枚举→bool 转换
    }, nil
}
```

#### T3-5-IMPL-I：WebUI 字段映射修复

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

#### T3-5-IMPL-J：WebUI 路径模板工具库重设计

> **背景**：trollsift 引入后，`dest_path_template` 的可注入变量来自四个来源——`InjectContext`
> 注入的 `{agent_name}`/`{agent_id}`、`buildStoragePath` 显式注入的 `{filename}`/`{ext}`、
> `path_pattern` 解析出的任意用户自定义字段（如 `{sensor_id}`、`{device}`）、以及 LDML
> 时间字段（如 `{time:yyyy/MM/dd}`）。
> 旧的 `PATH_TEMPLATE_VARIABLES` 白名单（`{year}`/`{month}` 等）与 trollsift 语法完全不兼容，
> 验证逻辑错误地拒绝合法模板，预览逻辑也无法正确处理 LDML 格式。

**涉及文件**：
- `webui/src/utils/pathTemplate.ts`
- `webui/src/pages/Agents/RuleForm.tsx`
- `webui/src/__tests__/pathTemplate.test.ts`

---

##### J-1：重写 `pathTemplate.ts`

**删除**：`PATH_TEMPLATE_VARIABLES`（整个导出常量）。

**新增**：`SYSTEM_TEMPLATE_VARIABLES`——始终可注入的 4 个系统变量（对应 Agent 侧
`InjectContext` + `buildStoragePath` 的显式注入）：

```ts
export const SYSTEM_TEMPLATE_VARIABLES: ReadonlyArray<{ key: string; desc: string }> = [
  { key: '{agent_name}', desc: 'Agent 名称，例如 prod-sensor-01' },
  { key: '{agent_id}',   desc: 'Agent UUID，例如 a1b2c3...' },
  { key: '{filename}',   desc: '原始文件名（含扩展名），例如 data.csv' },
  { key: '{ext}',        desc: '文件扩展名（不含点），例如 csv' },
]
```

**重写 `validatePathTemplate(template: string): string | null`**

不再做白名单检查，改为 **trollsift 语法校验**（对应 Go 侧 `Parser.Validate()`）：

| # | 规则 | 错误提示 |
|---|------|---------|
| V-1 | `template` 为空 | `'路径模板不能为空'` |
| V-2 | 不以 `/` 开头 | `'路径模板必须以 / 开头'` |
| V-3 | 包含 `//` | `'路径模板不能包含连续的 //'` |
| V-4 | `{` 与 `}` 不平衡（出现次数不相等，或存在 `{` 未配对） | `'模板括号不平衡'` |
| V-5 | `{}` 内字段名为空（即 `{}`） | `'模板变量名不能为空'` |
| V-6 | 包含 `\|tz=` 但其后值为空（如 `{t:yyyy\|tz=}`） | `'时区（tz=）值不能为空'` |

> **不做**字段名白名单检查——用户自定义字段（来自 `path_pattern`）在运行时才能确定，前端无法穷举。

**重写 `renderPathPreview(template: string, dynamicFields?: string[]): string`**

签名增加可选参数 `dynamicFields`（从 Step 2 的 `path_pattern` 解析出的字段名列表）。

替换优先级（从高到低）：

| 优先级 | 匹配条件 | 替换结果 |
|--------|---------|---------|
| 1 | `{fieldname:LDML}` 或 `{fieldname:LDML\|tz=...}` | 用 UTC 当前时间按 LDML 格式化（方案 A，见下）|
| 2 | `{agent_name}` / `{agent_id}` / `{filename}` / `{ext}` | 固定示例值（如 `'my-agent'`、`'data.csv'`、`'csv'`）|
| 3 | `{fieldname}`（出现在 `dynamicFields` 中）| `«fieldname»`（书名号，表示运行时由 path_pattern 解析填入）|
| 4 | 其他未知 `{fieldname}` | 原样保留（语法已通过 V-4/V-5 验证，运行时才能确定）|

**方案 A（轻量 LDML 时间格式化）**：在 TS 内实现以下 LDML 符号的替换即可覆盖主要用例，
无需完整 LDML parser（完整预览由 T3-6 Dry-Run 服务端承担）：

| LDML 符号 | 输出（对应 UTC 当前时间） |
|-----------|----------------------|
| `yyyy`    | 4 位年 |
| `yy`      | 2 位年 |
| `MM`      | 2 位月（01-12） |
| `dd`      | 2 位日（01-31） |
| `HH`      | 2 位时（00-23） |
| `mm`      | 2 位分（00-59） |
| `ss`      | 2 位秒（00-59） |

实现策略：提取 `{fieldname:LDML}` 后，将 LDML 字符串中上述符号替换为当前 UTC 时间对应值，
其余字面量（如 `-`、`/`）原样保留。`|tz=...` 部分在预览中忽略（始终用 UTC）。

**新增辅助函数 `extractDynamicFields(pathPattern: string): string[]`**

从 `path_pattern` 字符串中提取所有 `{fieldname}` 和 `{fieldname:type}` 的 `fieldname` 列表，
用于 Step 3 动态变量提示和 `renderPathPreview` 的 `dynamicFields` 参数。

---

##### J-2：更新 `RuleForm.tsx` Step 3

**Step 2 字段联动**：

由于 StepsForm 的每个 StepForm 维护独立的 Form 实例（ProComponents 实现），无法跨步骤
使用单一 `Form.useWatch`。采用如下方案在不增加冗余外部 state 的前提下实现联动：

- 在父组件 `AgentRuleFormPage` 中增加 `const [pathPattern, setPathPattern] = useState('*')`（初始值与 `path_pattern` 字段 `initialValue` 一致）。
- Step 2 的 `path_pattern` 字段已有 `tooltip` onChange 处理逻辑；在该 `onChange` 中追加调用 `setPathPattern(e.target.value)`。
- 该 state 的唯一职责是让 Step 3 知道 Step 2 的当前 `path_pattern`，从中提取动态字段名，与"额外维护父组件 state" 语义不同（它是 form 数据的派生投影，不是独立业务状态）。

> **为什么不用 formRef**：StepsForm 的 `formRef` 是 `StepsFormActionType`，不暴露
> `getFieldValue`；每个 StepForm 的 `formRef` 可以读取，但要触发 Step 3 重新渲染
> 仍需通知机制。在没有共享 Form 实例的情况下，`onChange + setState` 是 ProComponents
> 官方推荐的跨步骤联动方式，与 "form-driven" 语义一致（数据来自表单，不来自业务逻辑）。

**Step 3 UI 改造**：

将"可用模板变量"提示卡从读取 `PATH_TEMPLATE_VARIABLES` 改为两区展示：

**第一区：系统变量（始终可用）**

```tsx
{SYSTEM_TEMPLATE_VARIABLES.map(({ key, desc }) => (
  <div key={key}>
    <Text code>{key}</Text>
    <Text type="secondary" style={{ marginLeft: 4, fontSize: 12 }}>{desc}</Text>
  </div>
))}
```

时间字段单独展示语法提示（非枚举，仅说明格式规则）：

```
{字段名:LDML格式}   — 时间字段，例如 {time:yyyy/MM/dd}、{ts:HH:mm:ss|tz=Asia/Shanghai}
```

**第二区：来自采集模式的字段（动态，仅在 path_pattern 含结构化字段时显示）**

```tsx
{dynamicFields.length > 0 && (
  <div>
    <Text type="secondary" style={{ fontSize: 12 }}>
      来自文件过滤模式（path_pattern）的字段：
    </Text>
    {dynamicFields.map((f) => (
      <span key={f}>
        <Text code>&#123;{f}&#125;</Text>
      </span>
    ))}
  </div>
)}
```

其中 `dynamicFields = extractDynamicFields(pathPattern)` 在渲染时计算。

**`dest_path_template` 字段**：
- `initialValue` 从 `'/{year}/{month}/{agent_name}/{filename}'` 改为 `'/{agent_name}/{time:yyyy/MM/dd}/{filename}'`
- `validator` 改用新的 `validatePathTemplate`（语法校验，无白名单）
- `renderPathPreview` 调用改为 `renderPathPreview(pathTemplate, dynamicFields)`

---

##### J-3：重写 `pathTemplate.test.ts`

全部重写，覆盖新逻辑：

| # | 测试场景 | 期望 |
|---|---------|------|
| R-1 | `renderPathPreview('/{agent_name}/{time:yyyy/MM/dd}/{filename}')` | 含当前年份 `\d{4}`、月份 `\d{2}`、日期 `\d{2}`、`'my-agent'`、`'data.csv'` |
| R-2 | `renderPathPreview('/{agent_name}/{sensor_id}/{filename}', ['sensor_id'])` | 含 `'my-agent'`、`'«sensor_id»'`、`'data.csv'` |
| R-3 | `renderPathPreview('/static/path')` | 返回 `'/static/path'` 不变 |
| R-4 | `renderPathPreview('')` | 返回 `''` |
| R-5 | `renderPathPreview('/{agent_name}/{time:yyyy/MM/dd/HH}/{filename}')` | 路径含 `/YYYY/MM/DD/HH/` 格式片段 |
| V-1 | `validatePathTemplate('')` | truthy（空串） |
| V-2 | `validatePathTemplate('year/month')` | truthy（不以 `/` 开头） |
| V-3 | `validatePathTemplate('//year')` | truthy（双斜杠） |
| V-4 | `validatePathTemplate('/{agent_name/{filename}')` | truthy（括号不平衡）|
| V-5 | `validatePathTemplate('/{}/{filename}')` | truthy（字段名为空）|
| V-6 | `validatePathTemplate('/{t:yyyy\|tz=}/{filename}')` | truthy（tz 值为空） |
| V-7 | `validatePathTemplate('/{agent_name}/{time:yyyy/MM/dd}/{filename}')` | `null`（合法） |
| V-8 | `validatePathTemplate('/{sensor_id}/{device}/{filename}')` | `null`（合法，自定义字段不再报错）|
| V-9 | `validatePathTemplate('/{agent_name}/{unknown_var}')` | `null`（合法，不做白名单检查）|
| E-1 | `extractDynamicFields('{sensor_id}/{date:yyyy/MM/dd}/{filename}')` | `['sensor_id', 'date', 'filename']` |
| E-2 | `extractDynamicFields('*.csv')` | `[]`（无 `{}`）|

> 注：V-8、V-9 与旧测试行为**相反**（旧版报"未知变量"，新版通过），这正是本次修复的核心目的。

### T3-5 验收标准

| # | 验收项 | 方法 |
|---|--------|------|
| T3-5-1 | DB 迁移 up/down 均无错误 | `migrate up` + `migrate down` |
| T3-5-2 | `go build ./...` 三个模块无编译错误 | `make build` |
| T3-5-3 | 创建采集规则 API（POST /api/v1/rules）返回 201 | curl 测试 |
| T3-5-4 | 规则下发：`upload_bucket` 字段非空 | Agent 日志确认 |
| T3-5-5 | `append_mode` 三端一致：DB `'overwrite'`，Agent `AppendModeOverwrite`，proto `'overwrite'` | 单元测试 |
| T3-5-6 | `mode` 前端传 `"WATCH"`，DB 存 `'watch'` | 集成测试 |
| T3-5-7 | `pnpm test` 全通过（含新增 pathTemplate 测试，无白名单报错） | WebUI 测试 |
| T3-5-8 | `go test ./...` agent + controlplane 全通过 | 单元测试 |
| T3-5-9 | Step 3 路径预览正确渲染 LDML 时间字段（如 `{time:yyyy/MM/dd}` → 当前日期） | 手动确认 |
| T3-5-10 | Step 3 路径预览在 `path_pattern` 含结构化字段时显示 `«fieldname»` 占位 | 手动确认 |

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

- 字段统一命名决策：`DECISIONS.md` D-009
- trollsift 引入决策：`DECISIONS.md` D-010
- 待修复 Bug 列表：`docs/tasks/bugs/open.md`
- 当前 Phase 主线：`docs/tasks/phases/phase-3.md`
- 系统设计：`docs/design/system-design.md`
