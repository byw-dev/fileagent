# active.md — 当前执行入口（轻量）

> **当前阶段**：Phase 3 — 集成联调  
> **Agent 主要输入文件**：本文件 + `docs/tasks/phases/phase-3.md`

---

## 当前任务：T3-5 — 字段统一 + Bug 修复（审计收尾）

- T3-5 代码改造已完成并提交（`f01598e` / `bbf9378` / `f5537f5`）✅
- T3-5-IMPL-J 已完成（WebUI 路径模板重设计）✅
- P1 缺口（append_mode REST 响应缺字段）已修复 ✅
- T3-5 全部验收项通过 ✅

### T3-5 审计结论（2026-05-12）

- [x] DB 字段统一（`000003_rename_rule_fields.up.sql`）
- [x] proto `CollectionRule` 字段统一（`base_path`/`path_pattern`/`dest_path_template`/`upload_bucket`/`recursive`/`append_mode`/`enabled`）
- [x] CP REST 请求字段统一（`createRuleRequest`：`base_path`/`path_pattern`/`dest_path_template`/`bucket_id`/`recursive`/`append_mode`/`enabled`）
- [x] CP REST 响应字段统一（`collectionRuleResponse`：`base_path`/`path_pattern`/`dest_path_template`/`bucket_id`/`recursive`/`enabled`/`append_mode`）
- [x] `append_mode` 默认值为 `"overwrite"`（`agents.go:563–566`）
- [x] `mode` 大小写在输入时归一化（`agents.go:567` `strings.ToLower`）
- [x] `bucket_id → upload_bucket` 由 CP dispatch 稳定执行（`dispatch.go:lookupBucketName`）
- [x] `enabled ↔ status` 双向一致（`toRuleResponse` + `CreateRule`）
- [x] `pkg/trollsift` 已落地（`pkg/trollsift/`），Parse/Compose/Globify/IsTrollsiftPattern 均已实现
- [x] Agent 使用 `AppendModeOverwrite = "overwrite"`（`watcher.go:45`）
- [x] `go test ./...`（controlplane）通过（总覆盖率 80.5%）
- [x] `go test ./...`（agent）通过
- [x] `pnpm test` 全通过（68 个测试，pathTemplate.test.ts 全绿）

**T3-5-IMPL-J 实现完毕（2026-05-12）：**

- [x] 删除 `PATH_TEMPLATE_VARIABLES`，新增 `SYSTEM_TEMPLATE_VARIABLES`（4 个系统变量）
- [x] `validatePathTemplate`：改为 trollsift 语法校验（无白名单，接受任意合法字段名）
- [x] `renderPathPreview(template, dynamicFields?)`：支持 LDML 时间格式 + 动态字段 `«name»` 占位
- [x] 新增 `extractDynamicFields(pathPattern)`：从 `path_pattern` 提取字段名列表
- [x] `RuleForm.tsx`：Step 3 UI 改为两区提示（系统变量 + path_pattern 动态字段），`initialValue` 改为 `'/{agent_name}/{time:yyyy/MM/dd}/{filename}'`
- [x] `pathTemplate.test.ts`：全部重写（21 个测试，覆盖 R-1~R-5, V-1~V-9, E-1~E-2）
- [x] `append_mode` 补入 `collectionRuleResponse`（P1 缺口修复）
- [x] `services.test.ts` 同步更新（pathTemplate 相关测试更新为新语义）

**T3-5 状态：✅ 已完成，可关闭**

---

## 下一块：采集规则重构（T3-4 / T3-5 / T3-6）

T3-2 完成后立即执行，任务规格详见：[`docs/tasks/phases/phase-3-rft.md`](phases/phase-3-rft.md)

| ID | 任务 | 状态 |
|----|------|------|
| T3-4 | `pkg/trollsift` 共享路径模板库 | ✅ |
| T3-5 | 字段统一 + Bug 修复（DB/proto/agent/CP/WebUI） | ✅ |
| T3-6 | Dry-Run 规则测试功能 | ⬜ |

---

## 执行顺序（摘要）

```text
T3-2-BUG（A~D）✅ 已完成
    ↓
T3-2 Web UI + Control Plane 联调  ✅
    ↓
T3-4 ✅ → T3-5 ✅ → T3-6
    ↓
T3-3 Python SDK + Control Plane 联调
```

完整主线、依赖与验收标准见：[`docs/tasks/phases/phase-3.md`](phases/phase-3.md)

---

## 关联入口

- 当前 Phase 主线：`docs/tasks/phases/phase-3.md`
- 采集规则重构规格：`docs/tasks/phases/phase-3-rft.md`
- 已关闭 Bug：`docs/tasks/bugs/closed.md`
- 未排期工作：`docs/tasks/backlog.md`
- 历史归档：`docs/tasks/archive/`
- 专项报告：`docs/reports/`
