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
| [`docs/tasks/consistency-ingest.md`](docs/tasks/consistency-ingest.md) | **当前 track**：写入准入与索引一致性（IC-0…IC-14） | **Agent 主要输入** |
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
Phase 3  集成联调      🔄 进行中（core-completeness 冲刺 CC-1…CC-10 已收官，CC-3 推后）
Phase 4  完善与收尾    🔄 进行中（D-022 单二进制#60、D-023 迁移嵌入+T4-3#61、D-024 MinIO 拆分#62、D-025 元数据6c+webui重做设计#63）
```

> **进度补充（2026-07-07）**：T3-x 主线后插入并收官了止血冲刺（G-1…G-5）与 core-completeness 冲刺
> （CC-1…CC-10，CC-3 推后）；其后 Phase 4 收尾 D-022…D-025 均已合并（详见 `docs/tasks/changelog.md`）。
>
> **元数据 6c Phase 1 已收官（2026-07-14）**：MT-1…MT-6 全部合并（PR #69–#79），追踪 `docs/tasks/metadata-phase1.md`。
> **当前 track（2026-09-08 拍板）**：**写入准入与索引一致性（IC-0…IC-14）**——审计发现 Agent 数据面从未端到端
> 跑通过，且不存在 MinIO↔PostgreSQL 对账机制。决策 D-030 / D-031，设计 `docs/design/consistency-and-ingest.md`，
> 追踪 `docs/tasks/consistency-ingest.md`，缺陷 `docs/tasks/bugs/open.md`（IC-BUG-1…IC-BUG-24）。
> **WR track（Web UI 重做 WR-2…10）暂停让位**（WR-1 地基已合并 #66/#67，恢复方法见
> `docs/tasks/webui-redesign-impl.md`）。未排期：proto→buf ／ Phase 2（按信号）。当前态见 `docs/tasks/active.md`。

### Phase 3 任务明细（T3-x 主线，2026-05-12）

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
| T3-3 Python SDK + Control Plane 联调 | ⏸️ 已按产品决策推后（2026-07-04；暂无消费方） |

---

## 状态说明

| 符号 | 含义 |
|------|------|
| ⬜ | 未开始 |
| 🔄 | 进行中 |
| ✅ | 已完成（验收通过） |
| ❌ | 阻塞中 |
| ⚠️ | 部分完成（有已知缺陷） |
