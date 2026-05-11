# changelog.md — 历史记录索引（兼容入口）

> 本文件保留为兼容入口；历史完成记录已迁移至 `docs/tasks/archive/` 按 Phase 维护。

---

## 历史归档导航

- [Phase 0 — 契约定义](archive/phase-0.md)
- [Phase 1 — 基础骨架](archive/phase-1.md)
- [Phase 2 — 核心业务逻辑](archive/phase-2.md)

---

## Phase 3 关键历史（进行中）

- 2026-05-07：T3-1 / T3-1-FIX / T3-1-BUGFIX 完成（提交 `e295c05`）
- 2026-05-11：修正 `bugs/open.md` 中 T3-2-BUG-C 的现象与根因描述——实际现象为第三步缺少提交按钮（非"跳回第一步"），根因为 `StepsForm.submitter.render` 在 pro-components 2.8.x 中不透传给子 `StepForm`（提交 `0090419`）
- 当前主线与验收：`docs/tasks/phases/phase-3.md`
- 当前待修复 Bug：`docs/tasks/bugs/open.md`
