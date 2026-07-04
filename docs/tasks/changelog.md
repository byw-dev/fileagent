# changelog.md — 历史记录索引（兼容入口）

> 本文件保留为兼容入口；历史完成记录已迁移至 `docs/tasks/archive/` 按 Phase 维护。

---

## 历史归档导航

- [Phase 0 — 契约定义](archive/phase-0.md)
- [Phase 1 — 基础骨架](archive/phase-1.md)
- [Phase 2 — 核心业务逻辑](archive/phase-2.md)

---

## Phase 3 关键历史（进行中）

- 2026-05-07：T3-1 / T3-1-FIX / T3-1-BUG 完成（提交 `e295c05`）
- 2026-05-09：T3-2-FIX 系列（A~L，API 契约对齐 + WebUI Modal/崩溃修复）完成（提交 `2715706`）
- 2026-05-11：T3-2-BUG 系列（A~D，集成联调新发现 Bug）全部修复完成；另修复 `createRuleRequest` JSON 字段名不匹配导致的 400 错误，对齐 Agent 路径模板变量，新增 D-008 决策记录（提交 `addb3ee`）
- 2026-05-12：T3-5（字段统一 + Bug 修复）代码改造完成并审计；`go test`（controlplane/agent）通过，`pnpm test` 仍有 2 个既有失败（`pathTemplate.test.ts`），任务状态更新为 ⚠️（提交 `f01598e`、`bbf9378`、`f5537f5`）
- 2026-05-12：T3-5-IMPL-J 完成（WebUI 路径模板重设计：`SYSTEM_TEMPLATE_VARIABLES`、trollsift 语法校验、LDML 预览、`extractDynamicFields`；`append_mode` 补入 REST 响应）；`pnpm test` 68/68 通过，controlplane 覆盖率 80.5%；T3-5 全部验收项通过，状态更新为 ✅（提交 `3646f7e`、`1753bfa`）
- 2026-05~06：T3-6 Dry-Run 规则测试功能完成（CP `dryRunStore` + REST、agent `handleDryRun`、webui RuleForm Step 3），状态 ✅
- 2026-07-03~04：**止血冲刺**收官——差异分析（`docs/reports/design-gap-analysis/`）后 P0+P1 全部修复合并：G-1 refresh 契约（PR #39，D-012，本项目首个跨进程契约测试）、G-2 Agent token 生命周期（PR #40，D-013）、G-3 minio-event 鉴权（PR #41，D-014）、文档纠偏（PR #42）、G-4 心跳遥测（PR #43，D-015）、G-5 Dashboard 统计（PR #44，D-016）、冲刺回顾（PR #45）。详见 `docs/reports/design-gap-analysis/07-summary.md` §五
- 2026-07-04：**core-completeness 冲刺**启动——补完备清单（PR #46，`docs/tasks/core-completeness.md`）；**CC-1** file_deleted 事件补完（PR #47，D-017：`IndexDeletion`+`publishFileDeleted`+webhook ObjectCreated/Removed 路由）；**CC-2** queue_max_size 强制（PR #48，多轮 Copilot review：`IN` 索引化计数、SELECT/DELETE 竞态守卫、驱逐失败任务的日志降噪、入队后回收+排除新任务、`retryWg.Add` 前置）
- 当前主线与验收：`docs/tasks/active.md`（当前 = CC-3）+ `docs/tasks/core-completeness.md`；Phase 3 主线 `docs/tasks/phases/phase-3.md`
- 已关闭 Bug 归档：`docs/tasks/bugs/closed.md`
