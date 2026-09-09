# consistency-ingest.md — 写入准入与索引一致性（IC track）

> **目的**：修复数据面写入链路的系统性缺陷，并把「文件如何进入索引、索引如何与 MinIO 保持一致」
> 从「靠事件运气」改造为「有界成本可验证」。
> **权威设计**：[`docs/design/consistency-and-ingest.md`](../design/consistency-and-ingest.md)
> **决策**：`DECISIONS.md` **D-030**（总设计）、**D-031**（事件传输改 JetStream）
> **缺陷清单**：[`bugs/open.md`](bugs/open.md) IC-BUG-1…IC-BUG-29
> **来由**（2026-09-08）：产品提出两条此前不成立的前提——必须允许 ETL 等非 Agent 进程写入并记录
> tags 与血缘；对象量级为千万/年、3–5 年上亿。据此审计发现 **Agent 数据面从未端到端跑通过**。

---

## ⚠️ 开工前必读

1. **IC-1 是一切 live 验收的前提。** 在 STS 链路接通之前，Agent 拿不到凭据、一个文件都传不上去，
   任何「改完真跑 e2e」的纪律都无法执行。因此 IC-1 必须第一个做，且**只有 live 验证算通过**。
2. **IC-2 必须把 IC-BUG-8 一起带上。** IC-BUG-8（upsert 无排序键）今天不可见——因为 Agent 上报路径是死的。
   **一旦 IC-2 修好上报、IC-BUG-8 就立刻变成数据损坏**：webhook 事件晚到会把 Agent 写入的
   `agent_id` / `rule_id` / `sha256` / `file_mtime` 全部覆盖为 NULL。二者不可拆成两个 PR。
3. **地基阶段（IC-6/IC-7）有时间窗口。** 按 `system-design.md` §6.7 换算约 2600 万对象/年，
   到千万行再拆表 / 加列 / 改分区，每一步都要锁表或双写迁移。止血阶段做完应尽快推进地基，
   不要让它排在准入 / 对账之后。

---

## 命名约定

仓库既有规律是 `前缀 + 序号`，前缀按 track 走：`D-0xx`（决策）、`T0/T3/T4-x`（Phase 任务）、
`G-x`（差异分析发现）、`CC-x`、`WR-x`、`MT-x`。本 track 沿用：

| 类别 | 编号 | 说明 |
|---|---|---|
| 任务 | **`IC-0` … `IC-14`** | Ingest & Consistency，对应 `consistency-and-ingest.md`。按依赖顺序编号 |
| 缺陷 | **`IC-BUG-1` … `IC-BUG-15`** | 见 [`bugs/open.md`](bugs/open.md)，沿用 `T3-5-BUG-x` 的既有形状 |
| 阶段 | **不编号** | 仅作描述性小标题（止血 / 地基 / 准入 / 对账 / 血缘） |

三条纪律：

- **不引入第二条阶段轴。** 初稿曾用 `S0…S4`，与项目既有的 "Phase 3 / Phase 4" 并存会产生歧义，已废弃。
  跨文档引用一律写「对账阶段（IC-11…IC-13）」而非「S3」。
- 缺陷与任务是**多对多**（`IC-1` 一次关闭 `IC-BUG-1/3/4`）。两套编号并存有先例——
  当初 `G-1…G-5`（差异分析发现）与止血冲刺任务就是分开的。
- **新增前缀前先 `grep` 全仓库确认不冲突。** 初稿用过 `CI-x`，与仓库既有的 Continuous Integration
  （`.github/workflows/ci-cp.yml`、CLAUDE.md 多处）直接撞车。

---

## 任务清单

### 文档基线

| ID | 模块 | 内容 | 验收要点 | 状态 |
|----|------|------|----------|------|
| **IC-0** | docs | **编号统一 + track 切换标记**。① 本文与 `bugs/open.md`、`DECISIONS.md`、`consistency-and-ingest.md`、`system-design.md`、`active.md` 中的 `CI-x`→`IC-x`、`DP-x`→`IC-BUG-x`、`S0…S4`→描述性阶段名；② 本文补「命名约定」小节；③ `active.md`、`TASK_LIST.md` 与 `webui-redesign-impl.md` 标记 WR 暂停、IC 接替为主线（`TASK_LIST.md` 导航表补 `consistency-ingest.md` 条目） | 见下方验收命令（无输出即通过）；所有 markdown 链接可解析 | ✅ PR #91 |

**IC-0 验收命令**（无输出即通过）：

```bash
grep -rEn '\b(CI|DP)-[0-9]+|\b(CI|DP)\s*系列' --include='*.md' .
```

> 正则同时覆盖「前缀 + 空格 + 系列」这类无数字形态；不放宽成 `[- ]`，否则会误伤大量合法的
> Continuous Integration 提及（`CI 配置` / `CI 脚本` / `CI 环境` 等 15 处）。

> 单独成一刀：纯文本替换、零代码风险，且后续每个 PR 的描述都会引用这些编号，越早统一越少返工。

### 止血 — 让数据面首次端到端跑通（IC-1…IC-5）

| ID | 模块 | 内容 | 验收要点 | 状态 |
|----|------|------|----------|------|
| **IC-1** | CP + agent | **STS 链路接通 + 身份/模板归一化**（IC-BUG-1 + IC-BUG-3 + IC-BUG-4 + IC-BUG-16 + IC-BUG-17）。① CP 在 `Connect` 建流后推送一次 `ServerMessage_Credentials`（与 `SyncRulesOnConnect` 同时机）；② 对齐 `RefreshCredentialsRequest` 契约——允许省略 `rule_id`，按该 Agent 已下发规则涉及的 bucket 集合签发；③ **policy 资源改为整桶** `arn:aws:s3:::{bucket}/*`（修 IC-BUG-3；D-030 第八条：授权宽度是管理问题、清点成本由分片对账解决，**不约束 `dest_path_template`**）；④ **policy 拆两个 statement**（修 IC-BUG-4，对齐 §6.3）：桶级 `s3:ListBucketMultipartUploads` → `arn:aws:s3:::{bucket}`（不带 `/*`）；对象级 `s3:PutObject` / `s3:AbortMultipartUpload` / `s3:ListMultipartUploadParts` → `arn:aws:s3:::{bucket}/*`；**移除 `DeleteObject` / `GetObject` / `ListBucket`**（产品批的是「写整桶」，agent 从不调 Get/List，读与列举属超授）；删掉 `policy.go:41-43` 的 `arn:aws:s3:::*` 空桶兜底。**注意**：当前 `ListBucket` 配对象级 ARN，是一条从未生效的空转授权，只补 Action 不改 ARN 层级修不掉；⑤ Agent 刷新 goroutine 去掉 `sts == nil` 短路；⑥ **模板归一化**（修 IC-BUG-16）：收敛到一个函数，**agent 拼路径 / CP 反解 / webui 预览三端共用**。**方向必须是「CP 与 webui 侧剥模板的前导 `/`」，不是「agent 停止剥路径」**——后者会改写所有对象键、需全量重铺；⑦ **缓存 token 分支补齐 `AgentID`/`AgentName`**（修 IC-BUG-17，从 JWT claims 还原），并给 `buildStoragePath` 的 Compose 失败路径加 `logger.Warn`；⑧ **`server.tls_insecure` 开关**（修 IC-BUG-18，**零值即安全**：不设置就是 TLS；不加这条则本地 agent 根本连不上明文 CP，live-e2e 无法执行）；⑨ **`PollApproval` 校验 fingerprint**（修 IC-BUG-22——该 RPC 免认证，指纹是唯一身份凭证）；⑩ **`Connect`/`RefreshCredentials` 查 agent 状态闸门**（修 IC-BUG-23——吊销此前完全不生效） | **live-e2e**：dev 环境起 CP + agent，落一个文件 → MinIO 出现对象 → `file_entries` 有行；`mc ls --incomplete` 可执行（不 403）——**IC-3 的验收依赖这一条**；模板以 `/` 开头的规则对象键正确（IC-BUG-16 的 agent 侧；**CP 侧打标须待 IC-2**——`applyPathVarTags` 只在 `HandleUploadResult` 里调用，webhook 路径不打标，而 agent 尚不上报 `UploadResult`）；**Agent 重启后**再落一个文件，对象键仍符合模板而非裸 basename（IC-BUG-17 回归）；用签发的 STS 做 `GetObject` / `ListObjects` 须 403。**不接受仅单测通过** | ✅ PR #93（live-e2e 通过；path_var 打标回归改期至 IC-2，见验收注） |
| **IC-2** | proto + CP + agent + 迁移 | **上报 + ack outbox + 排序键**（IC-BUG-2 + IC-BUG-8 + IC-BUG-13，含 IC-BUG-12 的上报部分）。① `proto` 给 `UploadResult` 加 `task_id`（只增字段）；② CP 处理完后回发 `Acknowledgement`（消息已存在，只是无人发送）；③ `UploadFunc` 改为返回 `(*uploader.UploadResult, error)`，队列增 `reported` 状态，**收到 ack 才置 completed**；④ **迁移：`file_entries` 加 `observed_at` + `source`**（`agent`/`api`/`minio_event`/`audit`），upsert 加 `WHERE EXCLUDED.observed_at >= 现有值` + 富字段 `COALESCE`；⑤ 重试耗尽时以 `success=false` 上报；⑥ **凭据随 bucket 集合变化补发**（修 IC-BUG-20）：**CP 侧** `DispatchRule` 成功后若该 Agent 的 bucket 集合变了就重推一次 `Credentials`；**agent 侧**上传遇 `AccessDenied` 时作废凭据、刷新并重试一次，二次仍 403 则落终态告警；⑦ **模板解析失败改为任务失败而非猜键**（修 IC-BUG-21）；⑧ **`DeleteCollectionRule` 加归属约束**（修 IC-BUG-26）；⑨ **`handleDirectoryListing` 校验收件人**（修 IC-BUG-27，照抄 IC-SEC-1 给 `dryrun.Store` 的形状）；⑩ **`registry.Unregister` 按 conn 身份删除**（修 IC-BUG-28） | **live**：上传后 `agent_id`/`rule_id`/`sha256` 非空、`upload_logs` 有行、NATS 收到 `events.file.uploaded`；停 CP 再恢复，任务重发且不产生重复行；单测覆盖「webhook 更早/更晚 `observed_at` 均不清空富字段」 | ⬜ |
| **IC-3** | agent + deploy | **续传落盘 + 分片清理**（IC-BUG-5）。① 新增 `Queue.SaveMultipartProgress(id, uploadID, partsJSON)`，每片完成即落盘；② 任务进入终态（completed / 放弃）时调 `AbortMultipartUpload`；③ 数据桶加 `AbortIncompleteMultipartUpload` 的 ILM 规则兜底。**依赖 IC-1 已签发桶级 `s3:ListBucketMultipartUploads`**，否则本条验收会 403 卡住 | >64MB 文件传输中途 kill agent，重启后从断点续传（日志可见跳过分片数）；放弃的任务在 `mc ls --incomplete` 无残留 | ⬜ |
| **IC-4** | CP + deploy | **webhook 可靠性止血**（IC-BUG-6 + IC-BUG-7 + IC-BUG-9 + IC-BUG-19）。① 索引失败返回 5xx 让 MinIO 重投、解析失败返回 400；② `MakeBucket` 后调 `SetBucketNotification`，并在启动时对 `buckets` 表逐个 ensure（幂等补注册）；③ `queue_dir` 迁至持久卷；④ **对事件里的 `object.key` 做 `url.QueryUnescape`**（修 IC-BUG-19——S3 事件的 key 是 URL 编码的，当前直接落库，凡带层级的键索引值都与真实键不符） | 断开 PG 触发 ObjectCreated → 端点 5xx → 恢复 PG 后 MinIO 重投、`file_entries` 补齐；通过 API 新建 bucket 后直接 `mc cp` 一个对象，索引出现该行 | ⬜ |
| **IC-5** | agent | **采集正确性**（IC-BUG-10 + IC-BUG-11 + IC-BUG-12 剩余）。① `IsProcessed` 改为比较 `(rule_id, local_path, file_mtime, file_size)`，并修正与实现不符的注释；② `submitFile` 补齐 `FileOffset` / `AppendMode` 赋值，tail 偏移随任务落盘；③ 按文件大小推导 per-upload timeout（可配置下限） | 改文件内容后能被重新采集；`append_mode=tail` 规则第二次只传增量且重启后偏移不丢；不可达 MinIO 下 worker 会超时释放而非永久占用 | ⬜ |

> **止血阶段顺序**：IC-1 → IC-2 →（IC-3 / IC-4 / IC-5 可并行）。
>
> **IC-SEC-1（插入刀，PR #94）**：IC-BUG-24 + IC-BUG-25。二者是 IC-1 的 code review 顺带挖出的安全缺陷，
> 原挂在 IC-2 的 ⑧⑨ 上，但与上报链路无关，拆出单独一刀，避免两个不相干的安全改动混进同一次 review。
> **止血阶段收尾产出**：`docs/tasks/bugs/closed.md` 归档 IC-BUG-1…IC-BUG-13、IC-BUG-16…IC-BUG-25；`system-design.md` §4.5/§4.7/§5.7/§5.8/§6.3/§6.5
> 的「实现状态 / 实现偏差」告警块随之删除或改写。

### 地基 — 表结构（有时间窗口，宜早不宜迟）（IC-6…IC-7）

| ID | 模块 | 内容 | 验收要点 | 状态 |
|----|------|------|----------|------|
| **IC-6** | 迁移 + CP | **宽表 / 窄表分家 + 分区**。① 新建不分区窄表 `object_keys(bucket_id, storage_path, file_entry_id, last_modified, observed_at)`，主键 `(bucket_id, storage_path)`，并从现有 `file_entries` 回填；② `file_entries` 改为按 `uploaded_at` 月 RANGE 分区（幂等键已外置，不再受「唯一约束须含分区键」约束）；③ 索引重整，去掉与唯一约束隐式索引重复的 `idx_file_entries_bucket`。**前置拍板：待定 B（分区粒度与归档策略）** | 造 1000 万行：幂等 upsert 正确、cursor 分页 P95 达标、按 `(bucket_id, prefix)` 范围扫只命中窄表；迁移 up/down 干净且可在有数据时执行 | ⬜ |
| **IC-7** | CP + 决策 | **列表 / 统计去 `COUNT(*)`**（IC-BUG-14 + IC-BUG-15）。① Dashboard 的 `COUNT(*)` / `SUM(size_bytes)` 改为增量维护的计数表或 `pg_class` 估算（明确标注估算口径）；② 文件列表 `total` 契约调整——**需新开决策记录**（关联 D-007）；③ presign TTL 提为配置项 | 1000 万行下 Dashboard 首屏 P95 < 500ms；`expires_in` 随配置变化 | ⬜ |

### 准入 — 统一写入协议（IC-8…IC-10）

| ID | 模块 | 内容 | 验收要点 | 状态 |
|----|------|------|----------|------|
| **IC-8** | 迁移 + CP | **`write_grants` 表 + 签发登记**。每次签发 STS 时写入 `(principal_type, principal_id, bucket_id, issued_at, expires_at, state)`。**无 `prefix` 列**——D-030 第八条已定 policy 写整桶，grant 记录的是写入意向的**时间窗口**，空间切分由分片承担 | 每次凭据下发都有对应 grant 行；过期 grant 可被查询出来 | ⬜ |
| **IC-9** | CP | **注册与结算端点**。① `POST /api/v1/files/register`（批量、幂等，携带 `storage_path/size/sha256/etag/file_mtime/tags/run_id/grant_id`，`source=api`，未登记 tag 值入 `pending_tag_values`）；② `POST /api/v1/grants/{id}/settle {count:N}`，比对 `registered_count == N` 即标记 settled | 重复注册不产生重复行且不倒退 `observed_at`；结算后 grant 状态正确；数量不符时进入待对账队列 | ⬜ |
| **IC-10** | agent + SDK | **写入方接入协议**。① Agent 上报携带 `grant_id`，outbox 清空后调结算；② SDK 写入侧（Python 优先）提供 STS 申请 → 直传 → 注册 → 结算的封装与本地 outbox。**前置拍板：待定 E（SDK outbox 最小形态）** | Agent 正常运行时 grant 全部 settled；SDK 端到端跑通一次「拉取 → 加工 → 回写注册」 | ⬜ |

### 对账（IC-11…IC-13）

| ID | 模块 | 内容 | 验收要点 | 状态 |
|----|------|------|----------|------|
| **IC-11** | deploy + CP | **事件传输 webhook → NATS JetStream**（**D-031**，对账阶段前置）。① MinIO 改配 `notify_nats` + `jetstream=on`；② CP 改用 JetStream durable consumer，**处理成功才 ack**；③ 鉴权载体由共享密钥换为 NATS creds/nkey/TLS（D-014 作废）；④ 用 stream sequence 填 `observed_at` | 停 CP 期间写入的对象，CP 重启后从 stream 续读并补齐索引；能按序号重放一段历史 | ⬜ |
| **IC-12** | CP worker | **L1 grant 结算对账**。到期未结算 / 数量不符的 grant → **不列举**，改为按该 grant 的时间窗口查 `object_keys.last_modified` 得出受影响分片 → 置 `active` 交 L2 预算核实 → **同时告警**（富字段 tags/run_id/sha256 已永久丢失，L2 只能补回存在性，无法恢复，必须让人知道）| 人为丢弃一次注册后，grant 到期时被检出、相关分片转 `active`、告警产生；**L1 全程不发一次 `ListObjects`**（不只是稳态） | ⬜ |
| **IC-13** | 迁移 + CP worker | **L2 分片轮转 + L3 幽灵清理**。`shard_state` + `bucket_audit_policy`；分片取 key 叶子目录；主通道从 `object_keys` 推导前缀、兜底通道真实列举；按预算调度。`shard_state` 含 `last_event_seq`（供链路自证收窄失效范围）。**严格遵守设计 §3.5 的八条约束**，尤其：判「分片被写过」只能用 `object_keys.last_modified`，**绝不能用 `observed_at` / `updated_at`**；批预算按对象数而非分片数；`verified` 分片有重查下限但首轮核实不受限 | 在目标分片与相邻分片各注入一条幽灵：目标分片的被清理、**相邻分片的存活**、真实对象一行未动；长期无写入的分片能成功封存；封存分片被写后能解封 | ⬜ |

### 血缘（IC-14）

| ID | 模块 | 内容 | 验收要点 | 状态 |
|----|------|------|----------|------|
| **IC-14** | 迁移 + CP | **run 模型**（`metadata-model.md` P2.3）。`lineage_runs` / `run_inputs` / `file_entries.run_id`；挂载点：`batch-download-urls` 带 `run_id` 自动记 `run_inputs`，`files/register` 带 `run_id` 挂输出 | 一次「拉 N 个文件 → 产出 M 个文件」后，任一输出文件可反查出全部 N 个输入；1:1 / 1:n / n:1 均正确 | ⬜ |

---

## 前置拍板（阻塞对应任务，不阻塞止血阶段其余部分）

> 正文权威在 [`consistency-and-ingest.md`](../design/consistency-and-ingest.md) §6；本表只做「阻塞哪个任务」的索引。
> 改动倾向或补充细节请改设计文档那一份，避免两份副本再次漂移。

| # | 问题 | 阻塞 | 倾向 |
|---|------|------|------|
| ~~A~~ | ~~`storage_path` 前缀约定~~ | ~~IC-1~~ | ✅ **已定（2026-09-09）：不约束**。policy 写整桶，模板完全自由；清点成本由 L2 分片+封存解决。见 `DECISIONS.md` D-030 第八条 |
| B | `file_entries` 分区粒度（月 / 周）与归档策略；同时估算 `object_keys` 主键索引体积（按真实 `storage_path` 长度算，决定索引能否常驻内存） | IC-6 | 按真实增速估算后定 |
| C | 文件列表 `total` 去 `COUNT(*)` 的方案 | IC-7 | 需新决策记录（改动 D-007 契约） |
| D | ETL 是否允许就地覆盖同一 key（决定是否需要对象版本） | IC-9 | 待产品确认 |
| E | SDK outbox 最小形态（进程内重试 vs 本地持久化） | IC-10 | 待定 |

---

## 执行纪律

- 每个 IC-x **独立分支 off master + PR + code review + 人工合并**（沿用 CC / MT 冲刺惯例；
  Copilot 已不可用，review 改由其他渠道进行）。
- **改完真跑 live-e2e 再算完成。** 本 track 的存在本身就是「空心功能通过 mock 单测」的后果——
  IC-BUG-1…IC-BUG-4 长期不可见，正因为单测把 STS 与 gRPC 全 mock 了。
- 契约改动先记 `DECISIONS.md`：`proto` 只增字段不改编号；迁移只追加；cursor 分页不得改 offset。
- 覆盖率不低于 CLAUDE.md 阈值；禁止手改 `*.sql.go`，一律 `make generate`。
- 每完成一个 S 阶段，回填 `system-design.md` 对应章节并清理该阶段已消解的告警块。

## 与其它 track 的关系

- **WR-2…WR-10（Web UI 重做）暂停让位**——理由与 2026-07-10 那次相同：WR 是给已能用的页面换皮，
  而本 track 修的是「文件根本传不上去、索引可能永久缺失」。恢复方法见
  [`webui-redesign-impl.md`](webui-redesign-impl.md)。
- **元数据 Phase 1（MT-1…6）已收官**，本 track 的 IC-9 / IC-14 是其 Phase 2 的实际落地
  （`metadata-model.md` P2.2 衍生数据入口 / P2.3 run 血缘），触发信号「ETL 开始建设」已到达。
- **CC-3（tmp-uploads + bucket policy）** 仍推后；若 IC-9 的 ETL 写入引入 staging 需求再重启。
