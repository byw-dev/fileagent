# active.md — 当前执行入口（轻量）

> **当前阶段**：Phase 3 — 集成联调  
> **Agent 主要输入文件**：本文件 + `docs/tasks/phases/phase-3.md`

---

## 当前任务：T3-5 — 字段统一 + Bug 修复（审计收尾）

- T3-5 代码改造已完成并提交（`f01598e` / `bbf9378` / `f5537f5`）✅
- 本轮新增工作为 **审计变更与同步任务清单状态**
- 详细规格与验收标准：[`docs/tasks/phases/phase-3-rft.md`](phases/phase-3-rft.md)

### T3-5 审计结论（快速参考）

- [x] DB / proto / agent / controlplane / webui 字段统一改造已落地
- [x] `go test ./...`（controlplane）通过
- [x] `go test ./...`（agent）通过
- [ ] `pnpm test` 全通过（剩余 2 个失败：`pathTemplate.test.ts`，由 **T3-5-J** 修复）

**T3-5-J 待实现**（详细技术规格见 `phase-3-rft.md` §T3-5-J）：

- [ ] 删除 `PATH_TEMPLATE_VARIABLES`，新增 `SYSTEM_TEMPLATE_VARIABLES`（4 个系统变量）
- [ ] `validatePathTemplate`：改为 trollsift 语法校验（无白名单，接受任意合法字段名）
- [ ] `renderPathPreview(template, dynamicFields?)`：支持 LDML 时间格式（方案 A）+ 动态字段 `«name»` 占位
- [ ] 新增 `extractDynamicFields(pathPattern)`：从 `path_pattern` 提取字段名列表
- [ ] `RuleForm.tsx`：Step 3 UI 改为两区提示（系统变量 + path_pattern 动态字段），`initialValue` 改为 `'/{agent_name}/{time:yyyy/MM/dd}/{filename}'`
- [ ] `pathTemplate.test.ts`：全部重写，覆盖新逻辑（自定义字段不再报错，LDML 预览，dynamicFields）

---

## 下一块：采集规则重构（T3-4 / T3-5 / T3-6）

T3-2 完成后立即执行，任务规格详见：[`docs/tasks/phases/phase-3-rft.md`](phases/phase-3-rft.md)

| ID | 任务 | 状态 |
|----|------|------|
| T3-4 | `pkg/trollsift` 共享路径模板库 | ✅ |
| T3-5 | 字段统一 + Bug 修复（DB/proto/agent/CP/WebUI） | ⚠️ |
| T3-6 | Dry-Run 规则测试功能 | ⬜ |

---

## 执行顺序（摘要）

```text
T3-2-BUG（A~D）✅ 已完成
    ↓
T3-2 Web UI + Control Plane 联调  ✅
    ↓
T3-4 ✅ → T3-5 ⚠️（待补齐 WebUI 测试项）→ T3-6
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
