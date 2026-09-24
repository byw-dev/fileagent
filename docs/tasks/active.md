# active.md — 当前执行入口（轻量）

> **当前阶段**：Phase 3 — 集成联调（已插入两轮收尾冲刺）
> **Agent 主要输入文件**：本文件 + [`metadata-phase1.md`](metadata-phase1.md)

---

## ✅ 上一 track 已收官：元数据 6c Phase 1（受控标签）— MT-1…MT-6（PR #69–#79，2026-07-14）

全链路完成：迁移（5 表 + `retag_jobs`）→ indexer 打标 → 词表/待确认/单文件·批量打标 API → 回溯 worker → 四屏 UI（7a–7d）。
**权威追踪**：[`metadata-phase1.md`](metadata-phase1.md)（含收官 banner）。落地：`DECISIONS.md` D-025 各「落地记录」；
架构回填：`system-design.md` §3.3.8/§5.8。**推后**：规则改动回溯（低价值）；Phase 2（数据集/血缘）按信号触发。

## ▶️ 当前 track（2026-09-08 拍板）：写入准入与索引一致性（IC-0…IC-15）

对写入链路做全面审计后发现 **Agent 数据面从未端到端跑通过**：Agent 拿不到 STS 凭据（IC-BUG-1）、
从不上报 `UploadResult`（IC-BUG-2）、STS policy 前缀与实际对象键不匹配（IC-BUG-3）且缺 multipart 权限（IC-BUG-4）。
当前 `file_entries` 的唯一写入者是本应「只做对账兜底」的 minio-event webhook，与 `system-design.md` §4.5
描述的主路径完全相反。同时确认**不存在任何 MinIO↔PostgreSQL 对账机制**。

- **追踪**：[`consistency-ingest.md`](consistency-ingest.md)（IC-0…IC-15，含命名约定与执行纪律）
- **设计**：[`docs/design/consistency-and-ingest.md`](../design/consistency-and-ingest.md)
- **决策**：[`DECISIONS.md`](../../DECISIONS.md) **D-030**（不换存储层；STS grant + 注册 outbox + 分片对账）、
  **D-031**（MinIO 事件传输 webhook → NATS JetStream，排期对账阶段 IC-11）
- **缺陷清单**：[`bugs/open.md`](bugs/open.md) IC-BUG-1…IC-BUG-54（30…32 来自 2026-09-10 的 M-2 类扫描，33/34 来自同日评审，35 来自 IC-2c 的 live 验收，36 是 IC-2a 开工时挡路的那条，37/38 来自 IC-2a 的 live 验收，**39…41 来自 PR #97 的 code review**）。**54 条中已关闭 37 条**：随 IC-2a 关闭 **2/8/28/29/33**（另 13 关掉一半；**31 的两半此后已齐全**——结构半边随 IC-2b/PR #103），随 PR #97 关闭 **35/36**，随 **IC-5（PR #100）关闭 10/11/34/37 并补齐 12 的超时半边**（12 不再是拆半），随 **IC-2b（PR #103）关闭 20/21/30**，随 **IC-3（PR #105）关闭 5/39/47**（IC-3 的 ILM 兜底受上游 MinIO 限制，见卡片），随 **D-034（PR #106）关闭 50**，随 **PR #108 关闭 43/44**，**随 IC-SEC-2（PR #109）关闭 26/27**，随 **IC-4a 关闭 6/9**（webhook 失败语义+毒丸+死信表、`queue_dir` 持久卷；IC-BUG-7 归 IC-4b；**IC-BUG-6 的 (d) 只落 `active=true` 标记位，强制解封归 IC-12**）。**另已撤销 1 条（49）；未关闭 16 条：2 条挡（7/40）、11 条可推、3 条拆半（13、46 与 32）**（32 的拆半判于 PR #109：空闲路径随 IC-SEC-2 关闭、积压下送达保证未排期）（6/7 是「挡（收窄）」，仍算挡；42…45 是 PR #100 两轮 review 新立、**立卡时**均判可推（⚠️ **43/44 已随 PR #108 关闭**，「均判可推」现在只对 42/45 成立）；**46…49 是 IC-3 六轮 review 与 tail 设计讨论新立**——46 为拆半：出血半边已随 IC-3 挡掉、正确实现归 IC-15，**tail 当前不可用**；47 已随 IC-3 关闭；48 归 IC-15；**49 已撤销**——前提被实测证伪，其中成立的产品问题拆入 [`backlog.md`](backlog.md)「同一文件重复采集的覆盖语义」；**50 已随 D-034 关闭**；**51 新立**（🟠 P1，立卡 P2 经 PR #107 review 上调）——`sha256`/`size_bytes`/上传字节来自三次独立读，是 IC-11…13 对账的硬前置，宜与 48 同刀；**52 新立**——`dry_run_limit` 的有效上限恒为 10（CP 只在响应端裁剪、从不下发，agent 硬编码 10），PR #107 review 顺带撞见；⚠️ 立卡时误归为「参数收了不用」，**三轮 review 读码证伪**）——**判据已于 2026-09-11 重判**（「挡」的含义从「挡着能不能跑通」变成「挡着能不能扛住故障」），详见 [`consistency-ingest.md`](consistency-ingest.md) 「分诊结论」一节，**不要沿用旧口径的可推/挡**

**顺序**：IC-0 文档基线 ✅ → 止血 IC-1 ✅ → **IC-2c** ✅（硬前置，PR #96）→ **IC-2a** ✅（PR #98；前置 PR #97 修 IC-BUG-36/35）→ **IC-2b** ✅（IC-BUG-20/21/30/31 结构半边，live 三条验收全过；③ 为快照形态，D-033）→ **IC-5** ✅（采集正确性，PR #100）→ **IC-3** ✅（续传落盘 + 孤儿分片清理，PR #105，六轮 review；**tail 已 fail-closed 挡掉**，IC-BUG-46 的正确实现见 IC-15）→ **D-034** ✅（保留字 `{time}` 改名 `{submit_time}` + 路径模板保留字成文对齐三端，PR #106，六轮 review）→ **IC-4a** ✅（webhook 可靠性止血：失败语义 + 毒丸 + 死信表 + 持久计数器，与 `queue_dir` 迁持久卷同刀，IC-BUG-6/9；**② 归 IC-4b 另开刀**）→ ⚠️ **「下一刀在 IC-4b / IC-15 之间选」已作废**——A 基线审计后重排，见本文「下一步」一节：
IC 残留项在 **CI 绿灯可信 + 账本重判**之前都不动。

> **⚠️ 新会话开工前请读两节**：`consistency-ingest.md` 的「🟢 止血阶段已收官」与
> **「⚠️ 这本账的可信度边界」**（2026-09-13 立）——后者说明**本账本的归纳性表述
> （分类、「只有 N 种情形」的穷尽声明）需回代码核对，事实性表述（行号、计数）可直接用**。
> **未尽事宜**（均已立项，不必重新发现）：IC-BUG-51（三次独立读，IC-11…13 硬前置，宜与 48 同刀）、
> IC-BUG-52（`dry_run_limit` 有效上限恒为 10）、`backlog.md` 的「`phase-3-rft.md` 遗留项逐条核对」
> （该文标「主体已落地」是**抽样结论不是审计结论**）、`backlog.md` 的「同一文件重复采集的覆盖语义」
> （需产品拍板）、open.md 的缺陷模式归纳欠账（42…52 未归类、M-3 类未扫）。
~~→ 地基 IC-6/7 → 准入 IC-8…10 → 对账 IC-11…13 → 血缘 IC-14 → **tail 重做 IC-15**~~
⚠️ **这条 IC 原定执行链已作废**（2026-09-24，A 基线审计后重排）——其中 IC-6/7、IC-11…13、IC-15
均在本文「下一步」一节的**「明确不做」**清单里。别照这条箭头继续往下做。

> ⚠️ 两条硬约束：**IC-1 必须第一个做**（在它之前 Agent 一个文件都传不上去，任何 live 验收都无法执行）；
> **IC-2c 是 IC-2a 的硬前置**（webhook 对象键 `url.QueryUnescape`，IC-BUG-19）——反序会给每个对象造两行。
> **该硬序已于 2026-09-10 满足（PR #96）；IC-2a 已于 2026-09-11 收官（PR #98）。**
>
> **✅ 数据面主路径现在是真的通了**（不是「单测通过」）：dev 上落一个文件 → MinIO 出现对象 →
> `file_entries` 的 `agent_id`/`rule_id`/`sha256` 非空 → `upload_logs` 有行 → NATS 收到
> `events.file.uploaded` → `file_tags` 出现 `source='path_var'` 行。九条 live 验收逐条留证，
> 协调者独立复跑一遍同样全绿。**开工前先修掉了挡路的 IC-BUG-36**（CP 凭据被建成 root 的
> service account，而 MinIO 不允许 service account 调 `AssumeRole`——**全新环境从来签不出 STS**，PR #97）。
> **IC-2a 本身是不可再拆的原子刀**——IC-BUG-8（排序键）、IC-BUG-33（失败上报不写索引行）、
> IC-BUG-29（rule_id 归属）、IC-BUG-31 的 ack 半边、IC-BUG-28（`Unregister` 按 conn 身份，否则每文件一次
> `Send` 会撞上无 recovery 的 CP 崩溃）都必须与「上报」同刀，否则每一条都会**引入新缺陷**而非只是留着
> 旧缺陷。原 IC-2 的 ⑪ 项已于 2026-09-10 拆为 IC-2c / IC-2a / IC-2b / IC-SEC-2，详见 track 文件。
> **地基阶段越晚做越贵**——按 §6.7 换算约 2600 万对象/年，到千万行再拆表/加列/改分区，每步都要锁表或双写迁移。
> ⚠️ **2026-09-24 限定**：这条**风险本身仍然成立**（它是一个关于未来的条件命题），
> 但**时钟尚未开始走**——系统未发布、没有任何数据在累积，也没有存量迁移负担。
> 所以它**不构成「现在就做」的理由**；等真实数据开始累积时重新评估。
> （先前这里写成「该前提不成立」，是把「不紧迫」误写成了「命题为假」。）

**⏸️ WR track（Web UI 重做 WR-2…10）暂停让位**（同 2026-07-10 那次的理由）：WR 是给已能用的页面换皮，
而 IC 修的是「文件根本传不上去、索引可能永久缺失」。WR-1 地基已合并（PR #66/#67）不受影响；
恢复方法见 [`webui-redesign-impl.md`](webui-redesign-impl.md)。
**其余候选（未排期）**：proto→buf 复现性 follow-up（✅ 已落地，见 DECISIONS.md D-032）。

---

## ✅ A 基线审计已收官（2026-09-24，PR #114 / #116 / #115）

**结论：目标 A 的十二环在 dev 上实跑全通。** 一台 agent + CP + Web UI，配一条采集规则，
文件采上去 / 索引查得到 / 能下载，端到端 **sha256 三方一致**（源文件 = `file_entries.sha256`
= 预签名下载回来的内容）。逐环带证据，无「未验证」遗留。
**报告**：[`docs/reports/audit-a-baseline/`](../reports/audit-a-baseline/)（含 7 个机械层脚本）。

**两条挡 A 均已关闭**：
- **G-A1 首次运行路径跑不通**（#114 改 `CLAUDE.md` + #116 改根 `README.md`）
- **G-A2 MinIO 镜像已从 Docker Hub 下架**，三个 compose 在任何全新环境都起不来
  （#114 换 `quay.io`）。**这条是审计漏掉的**，由 CI 首次实跑撞出。

**已落地防回归**：[`deploy/scripts/smoke.sh`](../../deploy/scripts/smoke.sh) +
`.github/workflows/ci-smoke.yml` —— 十二环 + webhook 探针，约 1 分钟。
⚠️ 它带 `paths:` 过滤，**只有命中所列路径的 PR 才会触发**：
`agent/**` `controlplane/**` `webui/**` `pkg/**` `api/**` `proto/**` `deploy/**`、
`go.mod` `go.sum` `go.work` `go.work.sum`、`tools/**`、`Makefile`、**以及 workflow 自身**。
准确边界是「**命中上列 paths**」，不是「碰代码」——例如 `sdk/python/**`、`sdk/java/**`
不在清单里，改它们同样不触发。纯文档 PR 当然也不触发。

> ⚠️ 两条会影响后续判断的事实：
> ① **「CI 绿」的含义被高估**：11 个 `//go:build integration` 文件从未在 CI 跑过，
> 另 4 个依赖 PG 的测试以 `ok`(SKIP) 绿着——[`backlog.md`](backlog.md) 的
> 「**CI 与门槛的执行力缺口**」一节已有账（**按标题查，别记行号**：该文件插入新条目时行号会整体漂移，
> 本 PR 就制造过一次这样的失效引用）。
> ② **`IC-BUG-7` 的前提已被 IC-2a 推翻**——卡片标题仍成立，但「文件永远不会进入索引」
> 实测为假（主路径是 agent 上报，`source='agent'`），其 🟠 P1 定级建立在过期前提上。

---

## ⏭️ 下一步：A 达成之后的执行顺序（2026-09-24 拍板）

> **原则**：先让 A **真能用** → 再让 CI 的**绿灯可信** → 再**重判账本** → 才谈下一个里程碑。
> 每一步都是下一步的前提。
>
> **前提声明（产品，2026-09-24）**：当前开发中，**没有任何生产数据、没有任何外部对接**，
> 可按需及时迭代——不必为「存量兼容」付设计代价。

| # | 事项 | 体量 | 说明 |
|---|------|------|------|
| **1** | **采集写放大：防抖改为普适** | 半天 | 见下 |
| **2** | **让 CI 的绿灯可信** | 一天 | `ci-cp.yml` 加 `services:` + 跑 `-tags=integration`；webui 补 PR 门（22 个 vitest 文件至今无人运行）。照抄 `ci-agent.yml` 里 inotify overflow 那段的 **grep 防静默跳过**写法。已有账：[`backlog.md`](backlog.md)「**CI 与门槛的执行力缺口**」一节 |
| **3** | **IC 账本按 A 的标尺重判** | 半天，纯判断 | 恢复 IC 之前必须做：16 条未关缺陷的定级多在 IC-2a 之前给出，世界观已变（IC-BUG-7 是实证） |
| **4** | **存储层替代调研** | 并行，另开会话 | MinIO 转商用，quay.io 是最后一条公共通路。**研究任务应在被逼之前开始**。第一道筛子是 **STS AssumeRole**，不是「S3 兼容」四个字 |

### 第 1 条的具体拍板

**默认 `append_mode=overwrite` 无防抖**：审计实测一个 150MB 文件 `cp` 进监听目录产生
**150 次完整上传**（≈22GB），其中 3 次读到**正在写入**的半个文件。

已验证：`close_wait` 严格等于 `overwrite` + 500ms 防抖，**下游完全相同**
（差别只在 `agent/internal/watcher/watcher.go` 的三个分支点；uploader/executor 从不按这两个模式分流）。

**拍板：把防抖改成普适**（给 `overwrite` 也加上），而不是只翻默认值——
「不防抖」没有任何正当用途，只翻默认值等于把枪留在桌上，显式选 `overwrite` 的人照样中招。
`close_wait` 保留为可接受的别名，存量规则与文档不破。

### 明确不做（按 A 的标尺）

- **IC-15（tail 重做）**——已 fail-closed，无消费方
- **IC-6/7 地基提前**——「越晚越贵、2600 万对象/年」的**风险成立**，但**时钟尚未开始走**
  （未发布、无数据累积、无存量迁移负担），故不构成「现在就做」的理由；待数据开始累积时重估
- **对账 IC-11…13**——服务于「扛住故障」，而当前问题是「还没人用」
- **WR（Web UI 重做）**——UI 现已实登验证可用
- **覆盖率补到 80%**——差 0.6 个百分点，为凑数写测试正是 CLAUDE.md 禁止的

### 已提出但明确延后

- **合并 `controlplane/migrations/*.sql`** → 已归 [`backlog.md`](backlog.md)「技术债」。
  产品定性（2026-09-24）：**不影响功能逻辑，只影响维护与开发体验**，可延后。

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
2. ~~可选 **proto→buf** 复现性 follow-up~~ ✅ 已落地（DECISIONS.md D-032）。

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

- **A 基线审计**：章程 [`audit-a-baseline.md`](audit-a-baseline.md) ｜ 结论报告 [`docs/reports/audit-a-baseline/`](../reports/audit-a-baseline/)
- **端到端冒烟**：[`deploy/scripts/smoke.sh`](../../deploy/scripts/smoke.sh)（一条命令验证目标 A 全链路）
- 当前 track 追踪：[`consistency-ingest.md`](consistency-ingest.md)
- 前一 track（已收官）：[`metadata-phase1.md`](metadata-phase1.md)
- 前一冲刺（已收官）：[`core-completeness.md`](core-completeness.md)
- Phase 3 主线（含已完成 T3-x）：[`phases/phase-3.md`](phases/phase-3.md)
- 采集规则重构规格：[`phases/phase-3-rft.md`](phases/phase-3-rft.md)
- 已关闭 Bug：[`bugs/closed.md`](bugs/closed.md)
- 未排期工作：[`backlog.md`](backlog.md)
- 历史归档：[`archive/`](archive/)
- 专项报告：[`docs/reports/`](../reports/)
