# active.md — 当前执行入口（轻量）

> **当前阶段**：Phase 3 — 集成联调（已插入两轮收尾冲刺）
> **Agent 主要输入文件**：本文件 + `docs/tasks/core-completeness.md`

---

## 当前任务：CC-3 — Bucket Policy + tmp-uploads Lifecycle

**所属冲刺**：core-completeness（核心模块补完备）
**涉及模块**：controlplane（storage 层）
**缺口**：设计 §6.6 要求 **controlplane 创建 Bucket 后**自动 `SetBucketPolicy` + 给 `tmp-uploads` 配 7 天 Lifecycle。现状：**CP 运行时经 API 新建 bucket 的代码路径既不设 Policy 也不配 Lifecycle**；`deploy/scripts/init-minio.sh` 虽*尝试*给种子 `tmp-uploads` 配 Lifecycle，但 e2e 实测该步报错未生效（`../reports/design-gap-analysis/06-e2e-verification.md` D-1，`Unable to read ILM configuration`）。CC-3 应在 CP 侧可靠实现，不依赖初始化脚本。
**权威 backlog 与验收**：[`docs/tasks/core-completeness.md`](core-completeness.md) CC-3（Tier B）

> 每项 CC 独立 PR + Copilot review，改完真跑 e2e 再算完成。

---

## 冲刺进展（core-completeness）

| ID | 模块 | 缺口 | 状态 |
|----|------|------|------|
| CC-1 | CP + webui | `file_deleted` 事件死配置 | ✅ PR #47 / D-017 |
| CC-2 | Agent | `queue_max_size` 未强制 | ✅ PR #48 |
| **CC-3** | CP | Bucket Policy + tmp-uploads Lifecycle | ⬜ **进行中** |
| CC-4~7 | CP | 限流 / request_id / TTL 离线兜底 / kafka_publish | ⬜ |
| CC-8~10 | webui | Agent 重命名 / 规则原地编辑 / 隐性契约文档 | ⬜（视使用价值） |

前序已收官：**止血冲刺（P0+P1）** G-1…G-5（PR #39–#45，D-012…D-016），
回顾见 [`docs/reports/design-gap-analysis/07-summary.md`](../reports/design-gap-analysis/07-summary.md) §五。

---

## 已推后（产品决策 2026-07-04）

- **T3-3 Python SDK + Control Plane 联调** / T4-4 Java SDK：暂无消费方，CP 契约维护好则后期单独开发风险低。
- G-8/G-9 契约单一权威 / OpenAPI 工具化、Prometheus 指标（T4-1）、结构性文档重构。

理由与全清单见 `docs/tasks/core-completeness.md` 文末"明确推后"。

---

## 关联入口

- 当前冲刺 backlog：`docs/tasks/core-completeness.md`
- Phase 3 主线（含已完成 T3-x）：`docs/tasks/phases/phase-3.md`
- 采集规则重构规格：`docs/tasks/phases/phase-3-rft.md`
- 已关闭 Bug：`docs/tasks/bugs/closed.md`
- 未排期工作：`docs/tasks/backlog.md`
- 历史归档：`docs/tasks/archive/`
- 专项报告：`docs/reports/`
