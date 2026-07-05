# active.md — 当前执行入口（轻量）

> **当前阶段**：Phase 3 — 集成联调（已插入两轮收尾冲刺）
> **Agent 主要输入文件**：本文件 + `docs/tasks/core-completeness.md`

---

## 当前任务：core-completeness 冲刺收官（仅剩 optional proto→buf）

**所属冲刺**：core-completeness（核心模块补完备）
**上一项已收官**：**CC-10** ✅ 隐性契约文档（本 PR）——新建 [`docs/design/contracts.md`](../design/contracts.md)
汇总 V-1…V-4（status/mode/append_mode 枚举大小写映射、列表三种信封、trollsift 变量+LDML 符号表、
错误响应格式+错误码表），每条指向权威代码 `file:line`；CLAUDE.md 契约块 + system-design §5.11 加指针。
纯文档、未改业务代码。
**Tier A/B/C 主线全部收官**：CC-1/2/4/5/6/7/8/9/10 完成（CC-3 推后）。
**剩余候选**：仅 optional proto→buf 复现性 follow-up（视价值人工决定）。核心补完备冲刺至此收官。
**权威 backlog**：`docs/tasks/backlog.md` + [`core-completeness.md`](core-completeness.md)。

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
| **CC-7** | CP + webui | nats_publish 实现 + kafka_publish 拒绝 | ✅ PR #55 |
| CC-8 | CP + webui | Agent 重命名 | ✅ PR #58 / D-021 |
| CC-9 | CP + webui | 采集规则原地编辑 | ✅ PR #56–#57 / D-020 |
| CC-10 | docs | 隐性契约文档（`docs/design/contracts.md`） | ✅ 本 PR |

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
