# active.md — 当前执行入口（轻量）

> **当前阶段**：Phase 3 — 集成联调（已插入两轮收尾冲刺）
> **Agent 主要输入文件**：本文件 + [`metadata-phase1.md`](metadata-phase1.md)

---

## ✅ 上一 track 已收官：元数据 6c Phase 1（受控标签）— MT-1…MT-6（PR #69–#79，2026-07-14）

全链路完成：迁移（5 表 + `retag_jobs`）→ indexer 打标 → 词表/待确认/单文件·批量打标 API → 回溯 worker → 四屏 UI（7a–7d）。
**权威追踪**：[`metadata-phase1.md`](metadata-phase1.md)（含收官 banner）。落地：`DECISIONS.md` D-025 各「落地记录」；
架构回填：`system-design.md` §3.3.8/§5.8。**推后**：规则改动回溯（低价值）；Phase 2（数据集/血缘）按信号触发。

## ▶️ 当前 track（2026-09-08 拍板）：写入准入与索引一致性（IC-0…IC-14）

对写入链路做全面审计后发现 **Agent 数据面从未端到端跑通过**：Agent 拿不到 STS 凭据（IC-BUG-1）、
从不上报 `UploadResult`（IC-BUG-2）、STS policy 前缀与实际对象键不匹配（IC-BUG-3）且缺 multipart 权限（IC-BUG-4）。
当前 `file_entries` 的唯一写入者是本应「只做对账兜底」的 minio-event webhook，与 `system-design.md` §4.5
描述的主路径完全相反。同时确认**不存在任何 MinIO↔PostgreSQL 对账机制**。

- **追踪**：[`consistency-ingest.md`](consistency-ingest.md)（IC-0…IC-14，含命名约定与执行纪律）
- **设计**：[`docs/design/consistency-and-ingest.md`](../design/consistency-and-ingest.md)
- **决策**：[`DECISIONS.md`](../../DECISIONS.md) **D-030**（不换存储层；STS grant + 注册 outbox + 分片对账）、
  **D-031**（MinIO 事件传输 webhook → NATS JetStream，排期对账阶段 IC-11）
- **缺陷清单**：[`bugs/open.md`](bugs/open.md) IC-BUG-1…IC-BUG-21（6 个 P0 / 8 个 P1 / 7 个 P2）

**顺序**：IC-0 文档基线 → 止血 IC-1…IC-5 → 地基 IC-6/7 → 准入 IC-8…10 → 对账 IC-11…13 → 血缘 IC-14。

> ⚠️ 两条硬约束：**IC-1 必须第一个做**（在它之前 Agent 一个文件都传不上去，任何 live 验收都无法执行）；
> **IC-2 必须把 IC-BUG-8 一起带上**（修好上报而不修排序键会立刻造成数据损坏）。
> **地基阶段越晚做越贵**——按 §6.7 换算约 2600 万对象/年，到千万行再拆表/加列/改分区，每步都要锁表或双写迁移。

**⏸️ WR track（Web UI 重做 WR-2…10）暂停让位**（同 2026-07-10 那次的理由）：WR 是给已能用的页面换皮，
而 IC 修的是「文件根本传不上去、索引可能永久缺失」。WR-1 地基已合并（PR #66/#67）不受影响；
恢复方法见 [`webui-redesign-impl.md`](webui-redesign-impl.md)。
**其余候选（未排期）**：proto→buf 复现性 follow-up。

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

> 上述为历史记录。**当前 track 见本文顶部（IC）**；元数据 Phase 1 与 WR 的状态分别见
> [`metadata-phase1.md`](metadata-phase1.md) 与 [`webui-redesign-impl.md`](webui-redesign-impl.md)。

**其余候选（未排期）**：
1. **Web UI 重做实现 WR-2…WR-10**（⏸️ 已暂停，恢复条件与方法见 [`webui-redesign-impl.md`](webui-redesign-impl.md)）。
2. 可选 **proto→buf** 复现性 follow-up。

> **CC-3 已推后**（低价值）：`tmp-uploads` 全代码库未接入（agent 直传目标 bucket，无 staging/ETL），
> bucket policy 对本系统冗余（MinIO 默认私有，访问全走 STS/presigned IAM）。待有 staging workflow 再做。

> 每项任务独立 PR + code review（Copilot 已不可用，改由其他渠道 review），改完真跑 e2e 再算完成。

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

- 当前 track 追踪：[`consistency-ingest.md`](consistency-ingest.md)
- 前一 track（已收官）：[`metadata-phase1.md`](metadata-phase1.md)
- 前一冲刺（已收官）：[`core-completeness.md`](core-completeness.md)
- Phase 3 主线（含已完成 T3-x）：[`phases/phase-3.md`](phases/phase-3.md)
- 采集规则重构规格：[`phases/phase-3-rft.md`](phases/phase-3-rft.md)
- 已关闭 Bug：[`bugs/closed.md`](bugs/closed.md)
- 未排期工作：[`backlog.md`](backlog.md)
- 历史归档：[`archive/`](archive/)
- 专项报告：[`docs/reports/`](../reports/)
