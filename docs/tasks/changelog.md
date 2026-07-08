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
- 2026-07-04~05：**core-completeness 冲刺收官**——**CC-6** TTL 驱动离线兜底扫描（PR #51，`internal/worker` OfflineSweeper）；**CC-5** 错误响应顶层 `request_id` + 未知 query 参数拒绝（PR #53，`middleware.RespondError`/`RejectUnknownQuery`）；**CC-4** per-user API 限流（PR #54，D-018）；**CC-7** nats_publish 真实现 + kafka_publish 拒绝（PR #55，D-019；发现 nats_publish 原为空心）；**CC-9** 采集规则原地编辑（PR #56 后端 D-020 + #57 webui）；**CC-8** Agent 重命名（PR #58，D-021）；**CC-10** 隐性契约文档 `docs/design/contracts.md`（PR #59，纯文档无 D 编号）。CC-3 推后。
- 2026-07-05~07：**Phase 4 收尾**——**D-022** Web UI 嵌入 CP 单二进制（PR #60，build-tag 双模式 + `make bundle`）；**D-023** 迁移嵌入二进制 + 部署脚手架/运维文档 T4-3（PR #61，去 `MIGRATIONS_PATH`、Dockerfile/prod compose/Caddyfile/systemd/Windows NSSM/`docs/ops/`）；**D-024** MinIO internal/public endpoint 拆分（PR #62，消除 CP↔MinIO hairpin）；**D-025** 元数据模型 6c 拍板 + Web UI 重做设计落档（PR #63，`docs/design/metadata-model.md` + `webui-redesign.md`，Phase 1 设计/Phase 2 留存，纯文档）。
- 当前态与下一步候选：`docs/tasks/active.md`（core-completeness 收官 + D-022…D-025 已合并；下一步候选=元数据6c Phase 1 实现 / webui 重做实现 / 可选 proto→buf）+ `docs/tasks/core-completeness.md`；Phase 3 主线 `docs/tasks/phases/phase-3.md`
- 已关闭 Bug 归档：`docs/tasks/bugs/closed.md`
