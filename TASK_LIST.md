# TASK_LIST.md — FileAgent 任务总索引

> **当前阶段：Phase 3 — 集成联调**
>
> Agent 开始任务前，阅读顺序：
> 1. `CLAUDE.md`（技术栈与编码规范）
> 2. `docs/tasks/active.md`（当前活跃任务）
> 3. `docs/tasks/bugs/open.md`（如任务涉及 Bug 修复）

---

## 快速导航

| 文件 | 用途 | 读者 |
|------|------|------|
| [`docs/tasks/active.md`](docs/tasks/active.md) | 当前 sprint 活跃任务 | **Agent 主要输入** |
| [`docs/tasks/phases/phase-3.md`](docs/tasks/phases/phase-3.md) | 当前 Phase 主线（依赖与验收） | Agent / 人工 |
| [`docs/tasks/bugs/open.md`](docs/tasks/bugs/open.md) | 未解决 Bug（含完整修复规格） | Agent |
| [`docs/tasks/bugs/closed.md`](docs/tasks/bugs/closed.md) | 已关闭 Bug 归档 | 人工查阅 |
| [`docs/tasks/backlog.md`](docs/tasks/backlog.md) | 待规划任务（Phase 4 等） | 人工规划 |
| [`docs/tasks/archive/`](docs/tasks/archive/) | 按 Phase 归档的历史完成记录 | 人工查阅 |
| [`docs/tasks/changelog.md`](docs/tasks/changelog.md) | 历史记录索引（兼容入口） | 人工查阅 |
| [`docs/tasks/SCHEMA.md`](docs/tasks/SCHEMA.md) | 字段定义、状态枚举、命名约定 | 人工维护 |

---

## 各阶段状态

```
Phase 0  契约定义      ✅ 100%（5/5）
Phase 1  基础骨架      ✅ 100%（15/15）
Phase 2  核心业务逻辑  ✅ 100%（20/20 核心 + 8/8 T2-X 遗留补完）
Phase 3  集成联调      🔄 进行中（core-completeness 冲刺已收官）
Phase 4  完善与收尾    🔄 进行中（T4-3 部署脚手架+运维文档 ✅；D-022 单二进制、D-023 迁移嵌入）
```

### Phase 3 任务明细（最新状态：2026-05-12）

| 任务 | 状态 |
|------|------|
| T3-1 Control Plane + Agent 端到端联调 | ✅ 已完成 |
| T3-1-FIX Agent 生命周期健壮性修复 | ✅ 已完成（`e295c05`） |
| T3-1-BUGFIX gRPC 注册链路三个关键 Bug 修复 | ✅ 已完成（`e295c05`） |
| T3-2-FIX API 契约对齐（A~L 共 12 项） | ✅ 已完成（`2715706`） |
| T3-2-BUG 集成联调新发现 Bug（A~D 共 4 项） | ✅ 已完成 |
| T3-2 Web UI + Control Plane 联调 | ✅ 已完成 |
| T3-4 `pkg/trollsift` 共享路径模板库 | ✅ 已完成 |
| T3-5 字段统一 + Bug 修复（DB/proto/agent/CP/WebUI） | ✅ 已完成 |
| T3-6 Dry-Run 规则测试功能 | ✅ 已完成 |
| T3-3 Python SDK + Control Plane 联调 | ⬜ 待完成（阻塞于 T3-6） |

---

## 状态说明

| 符号 | 含义 |
|------|------|
| ⬜ | 未开始 |
| 🔄 | 进行中 |
| ✅ | 已完成（验收通过） |
| ❌ | 阻塞中 |
| ⚠️ | 部分完成（有已知缺陷） |
