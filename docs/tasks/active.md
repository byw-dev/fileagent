# active.md — 当前执行入口（轻量）

> **当前阶段**：Phase 3 — 集成联调（已插入两轮收尾冲刺）
> **Agent 主要输入文件**：本文件 + [`metadata-phase1.md`](metadata-phase1.md)

---

## ✅ 上一 track 已收官：元数据 6c Phase 1（受控标签）— MT-1…MT-6（PR #69–#79，2026-07-14）

全链路完成：迁移（5 表 + `retag_jobs`）→ indexer 打标 → 词表/待确认/单文件·批量打标 API → 回溯 worker → 四屏 UI（7a–7d）。
**权威追踪**：[`metadata-phase1.md`](metadata-phase1.md)（含收官 banner）。落地：`DECISIONS.md` D-025 各「落地记录」；
架构回填：`system-design.md` §3.3.8/§5.8。**推后**：规则改动回溯（低价值）；Phase 2（数据集/血缘）按信号触发。

## ▶️ 当前 track（2026-07-14 拍板）：恢复 Web UI 重做（WR-2…10）

Phase 1 收官后恢复 WR track（之前为 Phase 1 让位暂停，WR-1 地基已合并可复用）。**追踪**：
[`webui-redesign-impl.md`](webui-redesign-impl.md)（含 2026-07-14 规格校准：规则表单保留整页 4 步、元数据 4 屏折入 WR-3/4/8）。
**顺序**：WR-2 样板页 → WR-9 → WR-5/6/7 → WR-3/4/8 → WR-10。纯前端、无后端契约改动；每片独立 PR + review。
**其余候选（未排期）**：proto→buf 复现性 follow-up ／ Phase 2（数据集/血缘，触发信号出现时）。

**⏸️ 前一 track「Web UI 重做实现（Half A）」已暂停**（2026-07-10 价值优先决策）：WR-1 地基已合并（PR #66/#67）
并被本 track 复用（token / `StatusBadge` / 时间 util / 共享外壳）；WR-2…WR-10 暂停，追踪与恢复方法见
[`webui-redesign-impl.md`](webui-redesign-impl.md)。理由：WR 是给已能用的页面换皮，不产出核心价值；
给文件维护元数据/标签才是采集系统的核心，而这套还没建。

---

## 背景：近期落地为设计/文档

**所属阶段**：Phase 3 — 集成联调（Phase 4 收尾事项已合并）。**core-completeness 冲刺已全部收官**，其后一批 Phase 4 项也已合并：

| 里程碑 | PR | 决策 |
|--------|----|------|
| CC-1/2/4/5/6/7/8/9/10（CC-3 推后） | #47–#59 | D-017…D-021 |
| Web UI 嵌入 CP 单二进制 | #60 | D-022 |
| 迁移嵌入 + 部署脚手架/运维文档（T4-3） | #61 | D-023 |
| MinIO internal/public endpoint 拆分 | #62 | D-024 |
| 元数据模型 6c 拍板 + Web UI 重做设计 | #63 | D-025 |

**已选定 track**：**元数据 6c Phase 1 实现**（见顶部），追踪 [`metadata-phase1.md`](metadata-phase1.md)。

**其余候选（未排期）**：
1. **Web UI 重做实现 WR-2…WR-10**（⏸️ 已暂停，恢复条件与方法见 [`webui-redesign-impl.md`](webui-redesign-impl.md)）。
2. 可选 **proto→buf** 复现性 follow-up。

> **CC-3 已推后**（低价值）：`tmp-uploads` 全代码库未接入（agent 直传目标 bucket，无 staging/ETL），
> bucket policy 对本系统冗余（MinIO 默认私有，访问全走 STS/presigned IAM）。待有 staging workflow 再做。

> 每项任务独立 PR + Copilot review，改完真跑 e2e 再算完成。

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
| CC-10 | docs | 隐性契约文档（`docs/design/contracts.md`） | ✅ PR #59 |

前序已收官：**止血冲刺（P0+P1）** G-1…G-5（PR #39–#45，D-012…D-016），
回顾见 [`docs/reports/design-gap-analysis/07-summary.md`](../reports/design-gap-analysis/07-summary.md) §五。

---

## 已推后（产品决策 2026-07-04）

- **T3-3 Python SDK + Control Plane 联调** / T4-4 Java SDK：暂无消费方，CP 契约维护好则后期单独开发风险低。
- G-8/G-9 契约单一权威 / OpenAPI 工具化、Prometheus 指标（T4-1）、结构性文档重构。

理由与全清单见 [`core-completeness.md`](core-completeness.md) 文末"明确推后"。

---

## 关联入口

- 当前 track 追踪：[`metadata-phase1.md`](metadata-phase1.md)
- 前一冲刺（已收官）：[`core-completeness.md`](core-completeness.md)
- Phase 3 主线（含已完成 T3-x）：[`phases/phase-3.md`](phases/phase-3.md)
- 采集规则重构规格：[`phases/phase-3-rft.md`](phases/phase-3-rft.md)
- 已关闭 Bug：[`bugs/closed.md`](bugs/closed.md)
- 未排期工作：[`backlog.md`](backlog.md)
- 历史归档：[`archive/`](archive/)
- 专项报告：[`docs/reports/`](../reports/)
