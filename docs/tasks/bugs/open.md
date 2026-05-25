# bugs/open.md — 未解决 BUG

> 本文件结构化描述所有待修复 Bug，Agent 可直接消费。
> 每个 Bug 包含：根因、精确代码位置、修复方案（拆分为子任务）、验收标准。

---

## 总览

| ID | 标题 | 严重程度 | 涉及模块 |
|----|------|---------|---------|
| T3-6-BUG-A | `{agent_name}` 路径模板占位符永远报错 missing field | 🟡 P1 | agent + controlplane + proto |
| T3-6-BUG-B | Dry-Run `parsed_fields` 中时间/整数类型字段值为空字符串 | 🟡 P1 | pkg/trollsift + agent |

---

## T3-6-BUG-A — `{agent_name}` 路径模板占位符永远报错 missing field

| 字段 | 内容 |
|------|------|
| **严重程度** | 🟡 P1 |
| **发现场景** | Dry-Run 规则测试（T3-6），`dest_path_template` 中使用 `{agent_name}` 时，所有文件均返回 `compose_error: "trollsift: missing field \"agent_name\""` |
| **根因** | 链式缺失：(1) `proto/v1/agent.proto` 的 `RegisterResponse` / `PollApprovalResponse` 不含 `agent_name` 字段；(2) `Lifecycle` 结构体（`registration.go:94`）无 `AgentName` 字段，注册成功后无法存储控制平面赋予的名字；(3) `main.go:266` 只赋值 `agentCtx.AgentID = lc.AgentID`，`agentCtx.AgentName` 始终为零值 `""`；(4) `InjectContext`（`context.go:15`）在 `AgentName == ""` 时跳过注入，导致占位符缺失。`{agent_id}` 正常因为步骤 (3) 已赋值。 |
| **修复方案** | 采用**方案 B**——注册响应携带权威名字：<br>① `proto/v1/agent.proto`：`RegisterResponse` 和 `PollApprovalResponse` 各增加 `string agent_name = N`<br>② 重新生成 proto（`make proto`）<br>③ `controlplane` 注册 / 审批 RPC handler：响应填入 DB 中的 `agent.Name`<br>④ `agent/internal/grpcclient/registration.go`：`Lifecycle` 新增 `AgentName string`；`registerWithRetry` 和 `PollApproval` 从响应读取并存入 `l.AgentName`<br>⑤ `agent/cmd/agent/main.go:267` 紧跟 `agentCtx.AgentID` 赋值行，新增 `agentCtx.AgentName = lc.AgentName` |
| **设计约束** | 当前 `agent.Name` 由注册时的 hostname 自动填充，管理员暂不能重命名；重命名功能为独立后续任务（见 backlog T4-5），本 Bug 修复不依赖它。重命名后需 agent 重连才能使新名字生效（方案 B 的已知限制）。 |
| **受影响文件** | `proto/v1/agent.proto`、`controlplane/internal/grpcserver/handler.go`、`agent/internal/grpcclient/registration.go`、`agent/cmd/agent/main.go` |
| **验收标准** | Dry-Run 测试中 `dest_path_template` 含 `{agent_name}` 时，所有文件 `compose_error` 为空，`upload_path` 正确包含 agent hostname |

---

## T3-6-BUG-B — Dry-Run `parsed_fields` 中时间/整数类型字段值为空字符串

| 字段 | 内容 |
|------|------|
| **严重程度** | 🟡 P1 |
| **发现场景** | Dry-Run 测试中，`path_pattern` 含 `{d_dt:yyyy-MM-dd}`（time 类型）和 `{seq:d}`（int 类型）时，响应里 `parsed_fields.d_dt` 和 `parsed_fields.seq` 均为 `""`；只有 string 类型字段（如 `{fn:s}`）能正确显示 |
| **根因** | `agent/cmd/agent/main.go:584`：`fileResult.ParsedFields[k] = v.Str`。`trollsift.Value` 是联合体，`Str` 只对 `kindStr` 有效；`kindTime` 值存在 `v.Time`，`kindInt` 值存在 `v.Int`，对 string 类型字段取 `v.Str` 均为零值 `""`。解析本身正确（`fields[k] = v` 赋值无误，Compose 正常工作），仅展示层错误。 |
| **修复方案** | Raw string 方案，改动最小：<br>① `pkg/trollsift/parser.go`：`Value` struct 新增 `Raw string` 字段；`Parse` 方法在类型转换前将正则捕获的原始子串赋给 `Raw`（约 +3 行）<br>② `agent/cmd/agent/main.go:584`：`v.Str` 改为 `v.Raw`（1 字符改动）<br>③ `pkg/trollsift/parser_test.go`：补充对 time / int 字段 `Raw` 值的断言（约 +10 行）<br><br>**设计理由**：`Raw` 存的是路径中实际匹配的原始文本（如 `"2025-10-30"`、`"1"`），对 dry-run 调试最直观；时区转换为内部实现细节，不应反映在展示层；无需改 proto 或 REST 响应类型。 |
| **受影响文件** | `pkg/trollsift/parser.go`、`agent/cmd/agent/main.go`、`pkg/trollsift/parser_test.go` |
| **验收标准** | Dry-Run 响应中：`{d_dt:yyyy-MM-dd}` 字段显示 `"2025-10-30"`；`{seq:d}` 字段显示 `"1"`；`{date:yyyy-MM-dd\|tz=Asia/Shanghai}` 字段显示路径中的原始日期字符串；string 类型字段行为不变 |

---

## 关联入口

- 已关闭 Bug：[`closed.md`](closed.md)
- 修复子任务规格：[`../phases/phase-3-rft.md`](../phases/phase-3-rft.md)
- 字段命名决策：[`../../../DECISIONS.md`](../../../DECISIONS.md) D-009
- 当前活跃任务：[`../active.md`](../active.md)
