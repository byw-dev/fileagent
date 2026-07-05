# active.md — 当前执行入口（轻量）

> **当前阶段**：Phase 3 — 集成联调（已插入两轮收尾冲刺）
> **Agent 主要输入文件**：本文件 + `docs/tasks/core-completeness.md`

---

## 当前任务：CC-8/9（webui，视使用价值）或 optional proto→buf（下一步）

**所属冲刺**：core-completeness（核心模块补完备）
**上一项已收官**：**CC-7** ✅（事件动作）——发现 `nats_publish` 也是空心的，遂**真正实现**：
`NATSActionConfig{subject}` + `Engine.WithPublisher` + `dispatchNATS`（发布到 subject、失败进重试）；
API 拒绝非 `webhook`/`nats_publish` 的 action_type + 校验 config；webui 恢复 nats_publish、去掉 kafka。
live-e2e 通过。设计 §5.9 + D-019，本 PR 提交中。
**下一候选**：CC-8（Agent 重命名）/ CC-9（采集规则原地编辑）——纯 webui 功能增量，视真实使用价值人工决定；
或 optional proto→Go 复现性 follow-up（迁移 buf）。Tier B 健壮性缺口（CC-4/5/6/7）已全部收官。
**权威 backlog**：[`docs/tasks/core-completeness.md`](core-completeness.md) Tier C（CC-8~10）

> **CC-3 已推后**（低价值）：`tmp-uploads` 全代码库未接入（agent 直传目标 bucket，无 staging/ETL），
> bucket policy 对本系统冗余（MinIO 默认私有，访问全走 STS/presigned IAM）。待有 staging workflow 再做。

> 每项 CC 独立 PR + Copilot review，改完真跑 e2e 再算完成。

---

## 冲刺进展（core-completeness）

| ID | 模块 | 缺口 | 状态 |
|----|------|------|------|
| CC-1 | CP + webui | `file_deleted` 事件死配置 | ✅ PR #47 / D-017 |
| CC-2 | Agent | `queue_max_size` 未强制 | ✅ PR #48 |
| CC-3 | CP | Bucket Policy + tmp-uploads Lifecycle | ⏸️ 已推后（低价值，见上） |
| CC-6 | CP | TTL 驱动离线兜底扫描 | ✅ PR #51 |
| CC-5 | CP | 错误响应 `request_id` + 未知参数拒绝 | ✅ PR #53 |
| CC-4 | CP | API 限流（固定窗口，per-user） | ✅ PR #54 |
| **CC-7** | CP + webui | nats_publish 实现 + kafka_publish 拒绝 | ✅ 本 PR |
| CC-8~10 | webui | Agent 重命名 / 规则原地编辑 / 隐性契约文档 | ⬜（视使用价值） |
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
