# consistency-ingest.md — 写入准入与索引一致性（IC track）

> **目的**：修复数据面写入链路的系统性缺陷，并把「文件如何进入索引、索引如何与 MinIO 保持一致」
> 从「靠事件运气」改造为「有界成本可验证」。
> **权威设计**：[`docs/design/consistency-and-ingest.md`](../design/consistency-and-ingest.md)
> **决策**：`DECISIONS.md` **D-030**（总设计）、**D-031**（事件传输改 JetStream）
> **缺陷清单**：[`bugs/open.md`](bugs/open.md) IC-BUG-1…IC-BUG-45
> **来由**（2026-09-08）：产品提出两条此前不成立的前提——必须允许 ETL 等非 Agent 进程写入并记录
> tags 与血缘；对象量级为千万/年、3–5 年上亿。据此审计发现 **Agent 数据面从未端到端跑通过**。

---

## 🟢 止血阶段已收官（2026-09-11）—— 新会话从这里开工

**数据面主路径是通的，这一点已有 live 证据，不要再当成待验证的假设。**
dev 上落一个文件 → MinIO 出现对象 → `file_entries` 的 `agent_id`/`rule_id`/`sha256` 非空 →
`upload_logs` 有行 → NATS 收到 `events.file.uploaded` → `file_tags` 出现 `source='path_var'` 行。

已合并：`#93`(IC-1) → `#94`(IC-SEC-1) → `#96`(IC-2c) → **`#97`**(IC-BUG-36/35) → **`#98`**(IC-2a)。

### 先接住这几个坑，别重新踩一遍

1. **`consistency-and-ingest.md` §3.4 的 SQL 不是权威，代码才是。** 那段守卫连续三轮「新写 → 一执行就碎」，
   读文档四轮都没读出来。权威在
   [`controlplane/internal/db/queries/ingest.sql`](../../controlplane/internal/db/queries/ingest.sql) 与迁移
   `000006_index_observation`。**要改守卫就改那里，然后跑变异矩阵。**
2. **改守卫必须跑变异矩阵**，它是这一刀留下的地基：
   ```
   TEST_DATABASE_URL=postgres://fileagent:fileagent@localhost:5432/fileagent_test?sslmode=disable \
     go test ./controlplane/internal/db/ -tags=integration -run TestObservationMutationMatrix -v
   ```
   六项基线（A1/A2/A3′/P2/A4b/LPAD）+ 七种变异开关，**每种变异都会被它声称防的那条用例杀掉**。
   如果你改了守卫而它仍然全绿，先怀疑你的改动没生效，而不是庆祝。
3. **live 端到端有现成的骨架**，别重新造：
   `controlplane/internal/grpcserver/upload_live_integration_test.go`（`IC2A_LIVE=1` 开关，
   需要 `make build` 出的真二进制 + dev compose 四件套）。它带一个**转发真实 RPC 的代理**用来
   故意丢 ack，不是 mock。
4. **写 live 用例时：刺激文件必须在 agent 起来之后再落。** fsnotify 分支没有初始扫描
   （**IC-BUG-37**），先落文件再起 agent 会永远等不到上传，症状是一个毫无信息量的超时。
5. **dev 的 CP 凭据是真实 IAM 用户，不是 service account。** MinIO **不允许 service account 调
   `AssumeRole`**（IC-BUG-36）。如果哪天 STS 又开始 `Access Denied`，先查这个，别去查 gRPC。
6. **本机 `localhost` 与 `127.0.0.1` 不等价**，且预签名 URL 的 Host 参与签名——用
   `curl --connect-to localhost:9000:127.0.0.1:9000 "$URL"`，不要改写 host。
   **症状**：直接改 host 会得到 `SignatureDoesNotMatch`（很容易误判成凭据/policy 问题，也就是去查第 5 条）；
   不改而直连 `localhost` 则可能连到 `::1` 上超时或被拒。**看到 `SignatureDoesNotMatch` 先想这一条。**

### 下一刀怎么选

**IC-5 已于 2026-09-11 收官（PR #100）。下一刀在 IC-2b / IC-3 / IC-4 / IC-SEC-2 之间选，四者彼此可并行**，
之后才是有时间窗口的地基 IC-6/7。**推荐 IC-2b**——重判时点名的两条「稳态就错」缺陷里，
IC-BUG-10 已随 IC-5 关闭，只剩它手上的 **IC-BUG-21**（整桶 policy 之后错键不再被 403 挡住，静默写错位置）。
**这五刀各做什么，看下方「任务清单」里对应的行**（每行都有子项与验收），不要只看名字猜。
选之前**读一遍下方「分诊结论」的 2026-09-11 重判**——判据已经从「挡着能不能跑通」变成
「挡着能不能扛住故障」，上一轮按旧口径给的「可推」不能直接沿用（尤其 IC-SEC-2 那三条，
爆炸半径因 PR #97 而扩大了）。

**另有一条已排期在 IC 主线之外**：部署脚本的自动化测试（shellcheck + 行为矩阵），
产品已定在 #97/#98 合并后开工，见 [`backlog.md`](backlog.md)。

---

## ⚠️ 开工前必读

1. **IC-1 是一切 live 验收的前提。** 在 STS 链路接通之前，Agent 拿不到凭据、一个文件都传不上去，
   任何「改完真跑 e2e」的纪律都无法执行。因此 IC-1 必须第一个做，且**只有 live 验证算通过**。
2. **IC-2a 是一把不可再拆的原子刀。** 原 IC-2 有 ⑪ 个子项，2026-09-10 拆成 IC-2a / IC-2b / IC-SEC-2
   （理由见下方任务表）。留在 IC-2a 里的**五条**缺陷共享同一个时刻——**「上报成为索引主路径」的那一刻**：
   - **IC-BUG-8**（upsert 无排序键）：不修则 webhook 晚到会把 Agent 写的富字段覆盖为 NULL；
   - **IC-BUG-33**（失败上报仍写索引行）：⑥ 让 `success=false` 上报成为常规行为，从此每个耗尽重试的
     任务都在 `file_entries` 留一行「DB 有、对象无」的反向幽灵；更糟的是 upsert 无条件覆盖 `status`，
     会把「已成功上传、后来重传失败」的**活对象标成 `failed`**。已定案：失败只写 `upload_logs`；
   - **IC-BUG-29**（`UploadResult.rule_id` 无归属校验）：不修则该刀把死代码变成可利用。注意它的
     「规则不存在」分支是**稳态正常情形**（队列与规则生命周期解耦），不是 IC-BUG-30 的副产品，
     IC-2b 消不掉——定案宽松处理，见卡片；
   - **IC-BUG-31 的 ack 半边**：ack 走 best-effort `Send`，丢一个就有一个任务永久卡在 `reported`；
   - **IC-BUG-28**（`Unregister` 按 key 删）：② 把 `registry.Send` 从「每条连接几次」变成
     「每个文件一次」，而 gRPC 侧**没有 recovery interceptor**——重连竞态下 `send on closed channel`
     会让**整个 CP 进程崩溃**，且上传越密集越容易命中；

   五者都不是「顺手带上」，而是**不带上就会引入新缺陷**。不可拆成多个 PR。
   **IC-BUG-19 原也在此列，2026-09-10 拆为独立的 IC-2c**——它同样必须在上报开启前到位，
   但**顺序约束不等于打包约束**，单独一刀可独立 live 验证。IC-2c 是 IC-2a 的硬前置。
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
| 缺陷 | **`IC-BUG-1` … `IC-BUG-45`** | 见 [`bugs/open.md`](bugs/open.md)，沿用 `T3-5-BUG-x` 的既有形状。**新编号接着往后取**，不要以为序号只到某个旧上限 |
| 拆刀 | **`IC-2a` / `IC-2b`**、**`IC-SEC-n`** | 一刀过大时就地加后缀，**不消耗新序号**——`IC-15` 留给真正的新任务，否则「按依赖顺序编号」的性质会被破坏 |
| 阶段 | **不编号** | 仅作描述性小标题（止血 / 地基 / 准入 / 对账 / 血缘） |

三条纪律：

- **不引入第二条阶段轴。** 初稿曾用 `S0…S4`，与项目既有的 "Phase 3 / Phase 4" 并存会产生歧义，已废弃。
  跨文档引用一律写「对账阶段（IC-11…IC-13）」而非「S3」。
- 缺陷与任务是**多对多**（`IC-1` 一次关闭 `IC-BUG-1/3/4`）。两套编号并存有先例——
  当初 `G-1…G-5`（差异分析发现）与止血冲刺任务就是分开的。
- **新增前缀前先 `grep` 全仓库确认不冲突。** 初稿用过 `CI-x`，与仓库既有的 Continuous Integration
  （`.github/workflows/ci-cp.yml`、CLAUDE.md 多处）直接撞车。

---

## 分诊结论（2026-09-10）

**判据**（三条缺一即算「挡着数据面可用」）：**落得进**（按模板落到正确的对象键）/ **记得准**
（`file_entries` 有且只有一行、富字段不被清空）/ **不倒退**（重启·重连·重试·改文件都不丢不重）。
第四条是**耦合性**——IC-2a 让上报成为主路径的那一刻，某些死代码里的缺陷会立刻变成可利用或有害，
这类必须同刀，判据是「**这次改动让原本无害的东西变得有害了吗**」。

> **⚠️ 2026-09-11 重判（不是把数字减一减）**：止血阶段收官后，有几条的**性质**变了而不只是状态变了，
> 数字对上而性质没重判，比数字不对更能骗人。逐条见下表与其后的「本次重判了什么」。

**45 条中已关闭 23 条**：IC-1 关 1/3/4/16/17/18/22/23，IC-SEC-1 关 24/25，IC-2c 关 19，
**IC-2a（PR #98）关 2/8/28/29/33**，**PR #97 关 35/36**，**IC-5（PR #100）关 10/11/34/37 并补上 12 的超时半边**。
**未关闭 22 条：8 条挡、12 条可推、2 条拆半**（13/31 各关掉一半；**12 已两半齐全、不再是拆半**）。
「挡」的 8 条是 5/6/7/9/20/21/30/40——**其中 6/7 是「挡（收窄）」，仍算挡，不要当成可推**。
**IC-BUG-42…45 是 PR #100 两轮 code review 新立的**，四条均判为可推并写明了刻意不修的理由，见各自卡片：
42（`EnqueueIfNoActive` 不拦 `failed`）、43（close_wait 跳过的文件无兜底）、44（阻塞式初始扫描先于事件循环，
inotify 队列可能溢出）、45（tail 偏移按发出而非确认推进）。
**其中 43/44 是 PR #100 第一轮修复的次生问题，44 是 F1 修复的代价而非退步**（改成阻塞之前，同样的积压是必然被丢且无声的）。
**42/45 同源**（失败后的状态推进），**R1/R2/45 同源**（检测时刻与上传时刻的大小被混用）——
后者建议作为独立一刀处理，不要继续在 IC-5 上叠。

**这 23 条里没有一条再挡着「首次写入能否落地并记准」**——`file_entries` 有且只有一行、富字段不被清空、
上报与 webhook 两个写入方不打架，这些在 IC-1 + IC-2c + IC-2a + PR #97 之后已有 live 证据。
**剩下的「挡」大多是挡着「能不能扛住故障」**（重试、断连、崩溃恢复、事件丢失）。

> **⚠️ 但不要据此认为「正确性已经做完、只剩鲁棒性」——原有两条例外是产品语义缺口而非容错缺口**：
> **IC-BUG-10**（`IsProcessed` 忽略 mtime/size，**文件改了内容永远不会被重新采集**）与
> **IC-BUG-21**（模板解析失败时猜一个对象键写进去）。
> **IC-BUG-10 已随 IC-5（PR #100）关闭**，同刀还关掉了 IC-BUG-37（既有文件永不采集）。
> **剩下的那条是 IC-BUG-21，归 IC-2b**——IC-1 把 policy 改成整桶之后，错键不再被 403 挡住，
> 退化成**静默写错位置**。把 IC-2b 往后排之前，先看这一条。

| ID | 判定 | 归属 | 理由 |
|---|---|---|---|
| 2 | ✅ **已关闭** | **IC-2a ③（PR #98）** | 索引主路径是死代码，富字段恒 NULL、无 NATS 事件、6c 声明式打标从不触发 |
| 5 | 🔴 挡 | IC-3 | >64MB 每次从头重传；孤儿分片**无界泄漏**（无 Abort 调用、无 ILM 兜底） |
| 6 | 🟠 挡（收窄）| IC-4 ① | IC-2a 后 agent 路径有 ack 兜底，但非 agent 写入只有 webhook 一条命 |
| 7 | 🟠 挡（收窄）| IC-4 ② | IC-2a 前是「UI 建的桶文件永不入索引」，之后降级为补偿通道缺失 |
| 8 | ✅ **已关闭** | **IC-2a ⑤（PR #98）** | **不与 2 同刀即数据损坏**——webhook 晚到清空富字段 |
| 9 | 🟠 挡 | IC-4 ③ | `/tmp` 易失，事件丢了无补偿。**⚠️ 2026-09-10 改判**：初版写「IC-11 会连 `queue_dir` 一起删掉，别花第二次力气」——**实测证伪**，`mc admin config get myminio notify_nats` 显示它同样有 `queue_dir=` / `queue_limit=`。D-031 的全量扫描结论才是对的（`❌ 不解决`），此处从「挡但一行·别修」改判为「挡·照修」 |
| 10 | ✅ **已关闭** | **IC-5 ①（PR #100）** | 改了内容不重采 = 一次性上传器，不是同步系统 |
| 19 | ✅ **已关闭** | **IC-2c（PR #96）** | **与 2 互相制造重复行**——两个写入方的键编码不同，进不了同一个 conflict target。**须早于 IC-2a**。2026-09-10 已修并 live 验证；「同一对象经两条路径只有一行」那条回归**仍欠在 IC-2a**（agent 尚不上报，本刀执行不了） |
| 20 | 🔴 挡 | IC-2b ① | 新建指向新桶的规则 → 最长约 50 分钟持续 403，重试耗尽不自愈 |
| 21 | 🟠 挡 | IC-2b ② | 整桶 policy 后错键不再被 403 挡住，退化成**静默写错位置** |
| 28 | ✅ **已关闭** | **IC-2a ⑧（PR #98）** | 每文件一次 `Send` + `Register` 按 key 覆盖 + gRPC 无 recovery = **CP 进程崩溃** |
| 29 | ✅ **已关闭** | **IC-2a ⑦（PR #98）** | 上报活过来即可利用：借他人规则元数据打标，污染 6c 词表 |
| 30 | 🟠 挡 | IC-2b ③ | 断连期间删掉的规则 agent 继续跑，**无界**直到进程重启 |
| 31 | ◐ 拆半 | **IC-2a ④（PR #98，ack 半边已关）** / IC-2b ④ | ack 半边已修（`reported` 超时回退重发，**不是**让 `Send` 变可靠——后者正是 M-2 的错误方向），live 回归「故意丢一个 ack 后任务自愈」通过；**结构半边（`SendCh` 满即静默丢弃）仍开着，归 IC-2b** |
| 33 | ✅ **已关闭** | **IC-2a ⑥（PR #98）** | ⑥ 一落地即常态产出「DB 有、对象无」的反向幽灵，污染 IC-13 对账输入 |
| 34 | ✅ **已关闭** | **IC-5 ④（PR #100）** | 崩溃时在传的任务永久卡死，既不重传也不上报，且**无任何信号**。`reported` 刻意不复位（对象已在 MinIO，复位即全量重传），走 IC-2a 的重报路径 |
| 11 | ✅ **已关闭** | **IC-5 ②（PR #100）** | 只影响 `append_mode=tail` 规则、当前无消费方，**但修法只是给 `UploadTask` 补两个字段赋值**，比单独排期便宜。**⚠️ 2026-09-10 改判**：初版写「可推到准入阶段之后」，与任务表把它排进 IC-5 ②（并给了验收、算进止血归档范围）矛盾，按后者对齐 |
| 12 | ✅ **已关闭（两半齐全）** | **IC-2a ⑥（PR #98）+ IC-5 ③（PR #100）** | 重试耗尽以 `success=false` 上报（IC-2a）；per-upload timeout 已落地（IC-5）。**⚠️ review 修正**：deadline 按 `FileSize` 而非 tail 增量推导——multipart 忽略 offset 且传输前先对全文件算 SHA256，按增量算会让大文件永远超时重试 |
| 13 | ◐ 拆半 | **IC-2a ⑤（PR #98，防清空半边已关）** / 未排期 | `COALESCE` 已落地，后到的 webhook 不再清空已有值；**填值半边仍开着**——webhook 那一路今天就能做（载荷里本来就有 `contentType`），agent 那一路需 proto 增字段 |
| 14 | ⬜ 可推 | IC-7 ① | 百万行内无感 |
| 15 | ⬜ 可推 | IC-7 ③ | ⚠️ 但 19 不修则 presign 本来就 404，TTL 长短无意义 |
| 26 | ⬜ 可推 | IC-SEC-2 | 单组织 + 管理员鉴权下是越权面而非可利用漏洞 |
| 27 | ⬜ 可推 | IC-SEC-2 | 能力型随机 ID，需 30s 内猜中 UUIDv4 |
| 32 | ⬜ 可推 | IC-SEC-2 | 稳态下命令实际能送达（评审实测证伪了初版「约半数丢失」） |
| 35 | ✅ **已关闭** | **PR #97** | `init-minio.sh` 默认 access key 21 字符。**⚠️ 关闭时的限定**：20 字符上限只对 **service account** 成立，`mc admin user add` 收 21 字符（2026-09-11 实测）——改用 IAM 用户后该根因已不适用，脚本保留 3–20 校验是主动收敛，不是 MinIO 的要求 |
| 36 | ✅ **已关闭** | **PR #97** | CP 凭据被建成 root 的 **service account**，而 MinIO 不允许 service account 调 `AssumeRole` → **全新环境从来签不出 STS**。与 IC-1「STS 链路接通」的表面冲突已查实：IC-1 当时用的是 dev 上手工建的真实 IAM 用户，后来被换成 svcacct |
| 37 | ✅ **已关闭** | **IC-5 ⑤（PR #100，与 10 同刀）** | watcher 的 fsnotify 分支**无初始扫描**，规则指向的既有文件永不采集；polling 回退分支却会扫——**同一条规则的行为取决于 fsnotify 是否可用**。不挡「能不能跑通」，挡「规则建好了为什么什么都没发生」 |
| 38 | ⬜ 可推 | 未排期 | agent 的 `log.output`/`max_size_mb`/`max_backups` 收了不用，日志只落 stdout。部署形态下等于没有日志留存 |
| 39 | ⬜ 可推 | 宜并入下一次动 policy 的刀（IC-3 / IC-8） | `init-minio.sh` 硬编码的 session policy 与 `storage/policy.go` 无联动。**今天逐字一致所以无症状**，改一边不改另一边会在交集处静默削权 |
| 40 | 🟠 挡（诊断性）| 未排期 | CP 启动**不校验 MinIO 凭据**，凭据错了照常起，故障延后到 agent 连接、甚至要等 STS 会话过期（≤1h）才爆。**它不制造故障，它放大所有 MinIO 侧故障的排查成本**——IC-BUG-36 当初难查有它一份 |
| 41 | ⬜ 可推 | 未排期 | `init-minio.sh` 三处把 secret 放进 argv，`ps` 可见。执行窗口短、通常在运维自己机器上，实际风险低 |

> **这张表的用途是可复核**：下一次类扫描或下一刀改期时，应当对照它说明「哪一条的判定变了、为什么」，
> 而不是重新拍一遍。2026-09-10 的评审已经改过两条——**28 从「可推」改判为「挡」**（IC-2a 把
> `registry.Send` 从每连接几次变成每文件一次，原判据没有回应这一点），**33 是评审新增**。

#### 本次重判了什么（2026-09-11，止血收官）

**判据本身变了。** 原来的三条（落得进 / 记得准 / 不倒退）是在「数据面从未跑通」的前提下写的，
现在前提没了——那三条已经全部满足并有 live 证据。所以「挡」的含义从**「挡着能不能跑通」**
变成**「挡着能不能扛住故障」**。按新口径逐条：

- **6 / 7 / 9（webhook 可靠性，IC-4）**：判定不变，但**理由要改**。原来是「事件丢了索引就缺」，
  现在 agent 路径有上报 + ack 兜底，**webhook 只剩非 agent 写入那一条命**（UI 建桶、外部工具 `mc cp`）。
  收窄了，但没消失——而且 IC-2a 让「上报」和「webhook」成为**两个都会写同一行**的写入方，
  webhook 侧的丢失现在会表现为「富字段有、`source` 却停在 agent」这种更难看出来的形态。
- **5 / 20 / 21 / 30 / 34（IC-3 / IC-2b / IC-5）**：判定不变。这些是真正的「扛不住故障」：
  分片泄漏、换桶后 403 不自愈、错键静默写错位置、断连期间删规则仍在跑、崩溃后任务永久卡死。
- **37（新）**：判为**「挡（收窄）」**而不是可推。它不影响已在采集的规则，但「新建规则指向一个
  已有文件的目录 → 什么都没发生、且没有任何日志解释」是会被当成产品故障报上来的。
- **40（新）**：判为**「挡（诊断性）」**——这是本表里第一条**自己不制造故障、只放大别人故障**的条目。
  单列出来是因为它的收益不在自己身上：IC-BUG-36 之所以难查，一半原因是 CP 拿着坏凭据照常启动。
- **26 / 27 / 32（IC-SEC-2）**：仍可推，但**爆炸半径已变**——见 22/23 的卡片注记：PR #97 之后
  STS 链路对**任何**新环境生效，原来「dev 独有」的可利用面现在对所有新部署成立。**严重度不变，
  排期理由变强**。下一次选刀时这一条要摆上台面，不要因为它上次被判「可推」就自动跳过。

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
| **IC-1** | CP + agent | **STS 链路接通 + 身份/模板归一化**（IC-BUG-1 + IC-BUG-3 + IC-BUG-4 + IC-BUG-16 + IC-BUG-17）。① CP 在 `Connect` 建流后推送一次 `ServerMessage_Credentials`（与 `SyncRulesOnConnect` 同时机）；② 对齐 `RefreshCredentialsRequest` 契约——允许省略 `rule_id`，按该 Agent 已下发规则涉及的 bucket 集合签发；③ **policy 资源改为整桶** `arn:aws:s3:::{bucket}/*`（修 IC-BUG-3；D-030 第八条：授权宽度是管理问题、清点成本由分片对账解决，**不约束 `dest_path_template`**）；④ **policy 拆两个 statement**（修 IC-BUG-4，对齐 §6.3）：桶级 `s3:ListBucketMultipartUploads` → `arn:aws:s3:::{bucket}`（不带 `/*`）；对象级 `s3:PutObject` / `s3:AbortMultipartUpload` / `s3:ListMultipartUploadParts` → `arn:aws:s3:::{bucket}/*`；**移除 `DeleteObject` / `GetObject` / `ListBucket`**（产品批的是「写整桶」，agent 从不调 Get/List，读与列举属超授）；删掉 `policy.go:41-43` 的 `arn:aws:s3:::*` 空桶兜底。**注意**：当前 `ListBucket` 配对象级 ARN，是一条从未生效的空转授权，只补 Action 不改 ARN 层级修不掉；⑤ Agent 刷新 goroutine 去掉 `sts == nil` 短路；⑥ **模板归一化**（修 IC-BUG-16）：收敛到一个函数，**agent 拼路径 / CP 反解 / webui 预览三端共用**。**方向必须是「CP 与 webui 侧剥模板的前导 `/`」，不是「agent 停止剥路径」**——后者会改写所有对象键、需全量重铺；⑦ **缓存 token 分支补齐 `AgentID`/`AgentName`**（修 IC-BUG-17，从 JWT claims 还原），并给 `buildStoragePath` 的 Compose 失败路径加 `logger.Warn`；⑧ **`server.tls_insecure` 开关**（修 IC-BUG-18，**零值即安全**：不设置就是 TLS；不加这条则本地 agent 根本连不上明文 CP，live-e2e 无法执行）；⑨ **`PollApproval` 校验 fingerprint**（修 IC-BUG-22——该 RPC 免认证，指纹是唯一身份凭证）；⑩ **`Connect`/`RefreshCredentials` 查 agent 状态闸门**（修 IC-BUG-23——吊销此前完全不生效） | **live-e2e**：dev 环境起 CP + agent，落一个文件 → MinIO 出现对象 → `file_entries` 有行；`mc ls --incomplete` 可执行（不 403）——**IC-3 的验收依赖这一条**；模板以 `/` 开头的规则对象键正确（IC-BUG-16 的 agent 侧；**CP 侧打标须待 IC-2**——`applyPathVarTags` 只在 `HandleUploadResult` 里调用，webhook 路径不打标，而 agent 尚不上报 `UploadResult`）；**Agent 重启后**再落一个文件，对象键仍符合模板而非裸 basename（IC-BUG-17 回归）；用签发的 STS 做 `GetObject` / `ListObjects` 须 403。**不接受仅单测通过** | ✅ PR #93（live-e2e 通过；path_var 打标回归改期至 IC-2a，见验收注） |
| **IC-2c** | CP | **webhook 对象键 URL 解码**（IC-BUG-19）。对 `MinioEventHandler.Handle` 解析出的 `s3.object.key` 做一次 `url.QueryUnescape`（S3 事件用的是 `+`-as-space 的 **query** 编码，不是 path 编码），失败时退回原值并告警。**必须早于 IC-2a**——不修则 agent 上报写 `a/b/c.csv`、webhook 仍写 `a%2Fb%2Fc.csv`，两个 `storage_path` 进不了同一个 conflict target，IC-2a ⑤ 的 `observed_at` 排序键**永不触发**，同一对象变两行并沿 `object_keys` 传染进 IC-13 的对账输入。**与 IC-11 正交**（2026-09-10 双向实测：`notify_nats` 与 `notify_webhook` 载荷字节级同构，编码发生在 MinIO 构造事件时而非传输层），不得等 IC-11。**历史脏行不清洗**——系统未发布，按既定前提等 dev 库重建时自然消失，**不写迁移** | `mc cp` 一个 `a/b/中 文.csv`，`file_entries.storage_path` 等于 `a/b/中 文.csv`；预签名下载可直接取回该对象；单测覆盖带层级键、含空格与中文的键 | ✅ PR #96（live-e2e 通过：`storage_path` 未编码、预签名下载 200、`mc rm` 带层级键能命中软删；**字面加号往返实证** MinIO 编成 `%2B`。变异矩阵验证单测能证伪。**剩余一条回归归 IC-2a**：「同一对象经两条路径各写一次只有一行」，agent 尚不上报，本刀执行不了）|
| **IC-2a** | proto + CP + agent + 迁移 | **上报主路径（原子刀，不可再拆）**（IC-BUG-2 + IC-BUG-8 + IC-BUG-28 + IC-BUG-29 + IC-BUG-33，含 IC-BUG-12 的上报半边、IC-BUG-13 的防清空半边、IC-BUG-31 的 ack 半边）。**⓪ 先落 PoC，让代码当规格**（2026-09-10 第五轮评审后定）：本刀的第一步是把 `observed_at`/`event_seq` 守卫做成仓库里**可执行**的东西——真迁移 + 真 upsert + 表驱动测试（含下方五条验收与变异开关），而不是继续在文档里改 SQL 文本。**理由是实测出来的**：`consistency-and-ingest.md` §3.4 那段守卫连续三轮「新写 → 一执行就碎」（谓词恒真 / NULL 吞写 / `RETURNING` 返 0 行 / audit 覆盖），每一条都是跑起来才发现的，读文档四轮都没读出来。PoC 落地后 **§3.4 降为「意图与不变式」，SQL 文本以代码为准**（符合 CLAUDE.md 的权威顺序：代码是最终真相）。① `proto` 给 `UploadResult` 加 `task_id`（只增字段）；② CP 处理完后回发 `Acknowledgement`（消息已存在，只是无人发送）；③ `UploadFunc` 改为返回 `(*uploader.UploadResult, error)`，队列增 `reported` 状态，**收到 ack 才置 completed**；④ **`reported` 重报超时**（M-2 扫描新增，IC-BUG-31 的 ack 半边）：ack 走的是 best-effort 的 `registry.Send`（队满即丢），丢一个就有一个任务永久卡在 `reported`、outbox 永不清空——**修法是超时回退重发，不是让 `Send` 变可靠**（后者正是 M-2 的错误方向）；幂等由 ① 的 `task_id` + `(bucket_id, storage_path)` + ⑤ 的 `observed_at` 三者保证；⑤ **迁移：`file_entries` 加 `observed_at` + `source` + `event_seq` + `meta_incomplete`**（后两列为评审补，见下）。`observed_at` **按 source 取各自最可信、且客户端左右不了的时刻**：`minio_event` ← 载荷的 `eventTime`（**MinIO 生成**）；`agent`/`api` ← **PostgreSQL 的 `now()`**（§3.5 坑 3 的硬约束，不是 CP 进程时钟）；`audit` ← **列举那一刻**（不是写入事务的 `now()`——否则陈旧列举会覆盖更新数据并污染封存判据，见 §3.4 实测）。**⚠️ 绝不采信 `UploadResult.uploaded_at`**（agent 提供，报 `2099` 即可永久冻结该行）；**⚠️ 也不要把 `minio_event` 一并改成 CP 受理时刻**——那会让 `>=` 谓词恒真、闸门变摆设，而 `IndexUpload` 写的 `size_bytes`/`status`/`uploaded_at`/`etag` 都是非空值、`COALESCE` 一列都保护不到（2026-09-10 曾如此定案，评审用真 PG 证伪后撤回）。`agent`/`api` 的时刻**必须取 PostgreSQL 的 `now()`、不是 CP 进程的 `time.Now()`**（§3.5 坑 3 的硬约束；dev 实测 CP 进程时钟比 MinIO/PG 慢约 16ms，用进程时钟会让**每一次合法 agent 上报都被守卫拦掉**、`agent_id`/`sha256` 恒 NULL，且症状间歇性）。写入守卫的**完整 SQL、NULL 语义、以及「守卫落败恒返一行」的处理见 `consistency-and-ingest.md` §3.4，照抄，不要自创**。**⚠️ 特别注意 `RETURNING` 语义**：加 `WHERE` 后被正确压制的写入返 0 行 → `sql.ErrNoRows` → 若当 error 上抛，IC-4 ① 会判为处理失败并重投，而 `queue_dir` 是队头阻塞单队列，**一条本该被压制的陈旧事件会让索引 feed 停摆数分钟**——关键点：`event_seq` 只在 `observed_at` **相等**时决胜，且**任一侧为 NULL 必须放行**（写成 AND 比较会让每一次合法 agent 上报被静默吞掉，因为「webhook 先到、agent 后到」是生产常态）；⑤c **迁移写法**：`observed_at TIMESTAMPTZ NOT NULL` 加在已有数据的表上须带 `DEFAULT now()` 或分步回填（dev 仅 19 行，实操无痛，但迁移要写对）；⑤b **软删除同样加守卫，且必须推进 `observed_at`/`event_seq`**——它走的是独立的 `MarkFileEntryDeleted`（`indexer/queries.go:209-216`，当前**无任何时间围栏**），不是 upsert，实现者不会自动改到它。**两件事缺一不可**：(1) 加守卫，否则「删除事件滞留重试 → agent 重传同 key → 删除事件重投」会把刚建的活对象标成 `deleted`（L3 管的是反方向，检不出来）；(2) **新增两个入参并 `SET observed_at=$3, event_seq=$4`**——只加 WHERE 不推进这两列，则删除后行上仍是创建时的值，此后任一次 create 事件重投都会打平并自比自相等而通过守卫，**把已删除的行复活成 `completed`**（实测确认）；⑥ 重试耗尽时以 `success=false` 上报（IC-BUG-12 的上报半边），**且失败时不写 `file_entries`、只写 `upload_logs`**（修 IC-BUG-33，**✅ 2026-09-10 定案选 A**：当前 `HandleUploadResult` 无论成败都 upsert（调用在 `indexer.go:194`），只有 NATS 事件判 `success`。定案理由不是简洁，而是另一个选项有**损坏真实数据**的分支——upsert 的 `DO UPDATE` 无条件覆盖 `status`，于是「已成功上传过的路径后来重传失败」会把**那行活着的对象标成 `failed`**。实现细节：`CreateUploadLog` 现传 `FileEntryID: {fileEntry.ID, Valid:true}`（`indexer.go:233`），跳过 upsert 后置 `Valid:false`（该列可空）。**连带**：摘掉 Files 页的「失败」筛选项（`webui/src/pages/Files/index.tsx:37`），失败信号统一走已有的 Logs 页 / `upload_logs`——那张表才有 `error_message`/`retry_count`/`started_at`/`finished_at`）；⑦ **`UploadResult.rule_id` 归属校验，三分支**（修 IC-BUG-29——**必须与 ③ 同刀**：③ 让上报成为索引主路径的那一刻，它就从死代码变成可利用）。**分支是三个而非两个**：①规则不存在 / ②规则属于别的 agent / ③合法。**① 是稳态下的正常情形**——队列与规则生命周期解耦（`stopRule` 不碰 `upload_tasks`、`DequeuePending` 无 rule 过滤、无按 rule 删任务的语句），任务一旦入队就必然被上传上报，管理员每删一次规则只要名下还有在途任务就会产生一批；**IC-2b 修好 IC-BUG-30 也消不掉这一支**（它只消掉「无界产生」那条路径）。**✅ 定案：宽松——清空 `rule_id`、文件照常入索引**（严格拒绝等于让「删规则」静默丢弃已在 MinIO 里的文件的索引行，只是把幽灵推给 IC-13）。**告警分级**：① 按 `rule_id` 去重或降 Info（否则重蹈 IC-BUG-21 的「5000 条 Warn 被运维关掉」），**② 是唯一值得响的一支**。**代价须可见**：① 分支的文件永久无「规则声明的 `file_type` 覆盖」/ `static_tags` / `path_tag_map`（规则已删，retag 无可回溯声明），须置 ⑤ 迁移里的 **`meta_incomplete BOOLEAN NOT NULL DEFAULT false`** 列——**不得塞进 `source` 枚举**（`agent|api|minio_event|audit` 已被 D-030 与 IC-13 依赖）。**该列的语义须一次定死，否则就是一个没人写没人读的永久迁移债**：**置位条件** = 索引该行时**拿不到规则声明的元数据**，三条路径统一适用（① 规则不存在的上报、webhook 建的行、IC-9 `files/register` 未带 rule 的行）；**从不清位**（规则已删则永不可恢复）；**消费方**：`GET /api/v1/files` 的响应体与文件详情返回该字段，Files 页详情抽屉展示「元数据不完整」标记（UI 部分若不在本刀，须在 WR backlog 立项，**不得只加列不加消费方**）。**六条路径**的完整枚举见 `bugs/open.md` IC-BUG-29 卡片（第 6 条是 IC-BUG-26 自己制造的：`DeleteRule` 把 cancel 发给 URL 里的 agent 而非规则真正的属主，**唯一一条在全在线稳态下无界产生**）。⑧ **`registry.Unregister` 按 conn 身份删除**（修 IC-BUG-28，**2026-09-10 评审后从 IC-SEC-2 移入**：② 把 `Send` 从「每条连接几次」变成「每个文件一次」，叠加 `Register` 按 key 覆盖 + gRPC 侧无 recovery interceptor，重连竞态下 `send on closed channel` 会让**整个 CP 进程崩溃**，上传越密集越易命中） | **live**：上传后 `agent_id`/`rule_id`/`sha256` 非空、`upload_logs` 有行、NATS 收到 `events.file.uploaded`；**同一对象经 agent 上报与 webhook 各写一次后 `file_entries` 只有一行**（IC-BUG-19 回归）；停 CP 再恢复，任务重发且不产生重复行；**人为丢弃一个 ack 后任务仍能自愈**（④ 的回归）；**补 IC-1 欠下的 path_var 打标回归**——模板带前导 `/` 的规则上传后 `file_tags` 出现 `source='path_var'` 行。**并发**：`-race` 下同一 agent 快速重连 + 持续上报，无 panic、`IsOnline` 保持 true（⑧ 的回归）。**⚠️ 守卫的单测必须能证伪，且下列五条是评审用变异矩阵实测过的**（我此前自拟的四条里有三条杀不掉它们自己声称防的变异——`=` 分支被删、NULL 改收紧、去 `lpad` 三种变异下四条全绿。**不要再自拟**）：**A1** agent 写 `size_bytes=100/etag=E2` → 陈旧 webhook 带 `55/E1` → 仍为 `100/E2`（杀「守卫全删」）；**A2** webhook 先建行 → agent 以**更大** `observed_at` 上报 → 富字段落库（杀「删 `>` 分支」）；**A3′** 三条事件**同一个 `observed_at`**：`create(seqA) → delete(seqB>A) → create(seqA) 重投` → 仍为 `deleted`（杀「删 `=` 分支」「守卫全删」「软删不推进」）；**P2** webhook 先建行 → agent 以**相等** `observed_at` + `seq=NULL` 上报 → 富字段必须落库（杀「删 `=` 分支」「NULL 改收紧」）；**A4b** 先有更新写入、**再重投旧事件** → 调用方拿到**成功**而非 `ErrNoRows`（杀「无恒返一行」；注意**背靠背重投同一事件杀不掉它**，守卫 `>=` 恒真）。**关于 `lpad`**：真实 `sequencer` 恒 16 位十六进制，去掉 `lpad` 对生产数据是**等价变异**，只能用合成的不等长 seq（如 `'9F'` vs `'10A'`）测出——保留 `lpad` 是因为 prod 钉的 MinIO 版本与 dev 不同，别因为「测不出」就删。另覆盖「上报携带不存在的 `rule_id` → 文件仍入索引且 `rule_id` 为空、`meta_incomplete=true`」+「上报携带他人的 `rule_id` → 拒绝该 rule_id、`file_tags` 不出现他人规则声明的标签、告警」 | ✅ PR #98（**live-e2e 九条全绿**：富字段非空 / `upload_logs` 有行 / NATS 收到事件 / 双写只有一行 / 停 CP 重启后重发不重复 / 故意丢 ack 后自愈 / 前导 `/` 模板的 `path_var` 打标 / `-race` 重连 10,000 次无 panic / `rule_id` 三分支。协调者独立复跑一遍同样全绿。**⓪ 的守卫变异矩阵在真 PG 上六项基线全绿、七种变异各自被杀**。**开工前挡路的 IC-BUG-36 已先行修掉**——CP 凭据是 root 的 service account，MinIO 不允许其 `AssumeRole`，全新环境从来签不出 STS，见 PR #97） |
| **IC-SEC-2** | CP | **归属与并发**（IC-BUG-26 + IC-BUG-27 + IC-BUG-28 已移入 IC-2a ⑧ + IC-BUG-32）。① `DeleteCollectionRule` 加 `AND agent_id AND org_id`，`:execrows`=0 → 404，**并让 `DeleteRule` 从 DB 读回真实 `agent_id` 再发 cancel**（当前发给 URL 里的 agent，是 IC-BUG-29 第 6 条路径的另一半）；② `dirstore.Store` 照抄 IC-SEC-1 给 `dryrun.Store` 的形状记收件人（`Register(reqID, agentID)` + `Deliver` 比对）；③ `Revoke` 的 `Send`→`Disconnect` 加**有界**短窗（≤1s）等发送确认，超时直接切——上限必须是硬的，不能等一个不读流的 agent | 用 agent A 的路径删 B 的 rule 返回 404 且 B 的规则仍在、**且不发出任何 cancel**（注意：① 前半落地后「cancel 发错收件人」在合法路径上已不可复现——URL agent 必然等于规则属主，所以**不要写「cancel 发给 B 而非 A」这种无法证伪的验收**；「从 DB 读回 `agent_id`」保留为纵深防御，用单测直接断言 dispatch 入参来自 DB 行而非 `c.Param`）；agent A 对发给 B 的 `request_id` 投递被丢弃且合法目录列举仍送达；bufconn：正常 agent 被吊销时先收到 `RevokeCommand` 流才断，不读流的 agent 在上限内仍被切断 | ⬜ |
| **IC-2b** | CP + agent | **下发链路鲁棒性**（IC-BUG-20 + IC-BUG-21 + IC-BUG-30 + IC-BUG-31 的结构半边）。① **凭据随 bucket 集合变化补发**（修 IC-BUG-20）：**CP 侧** `DispatchRule` 成功后若该 Agent 的 bucket 集合变了就重推一次 `Credentials`；**agent 侧**上传遇 `AccessDenied` 时作废凭据、刷新并重试一次，二次仍 403 则落终态告警；② **模板解析失败改为任务失败而非猜键**（修 IC-BUG-21）；③ **规则同步补全集语义**（修 IC-BUG-30，M-2 扫描新增）：`SyncRulesOnConnect` 连 inactive 一起推（agent 的 `applyRule` 对 `Enabled == false` 已会 `stopRule`，复用既有分支），**并另下发一条「本次同步的 `rule_id` 全集」让 agent 停掉集合外的规则**——删除的行已不在 `ListCollectionRulesByAgent` 里，只推 inactive 修不掉删除那半；④ **`Connect` 里把发送 goroutine 提到 `SyncRulesOnConnect` / `pushCredentials` 之前启动**（IC-BUG-31 的结构半边：当前两处都在无消费者时入队，≥32 条 active 规则即静默丢弃），并让 `DispatchRule` 不再吞掉 `Send` 失败 | agent 与 CP 断连期间删除/停用规则，恢复连接后**不重启 agent** 也不再产生上传；运行中新建指向新 bucket 的规则，首个文件即成功（不出现 403）；配 40 条 active 规则重连后 40 条全部生效且凭据到达 | ⬜ |
| **IC-3** | agent + deploy | **续传落盘 + 分片清理**（IC-BUG-5）。① 新增 `Queue.SaveMultipartProgress(id, uploadID, partsJSON)`，每片完成即落盘；② 任务进入终态（completed / 放弃）时调 `AbortMultipartUpload`；③ 数据桶加 `AbortIncompleteMultipartUpload` 的 ILM 规则兜底。**依赖 IC-1 已签发桶级 `s3:ListBucketMultipartUploads`**，否则本条验收会 403 卡住 | >64MB 文件传输中途 kill agent，重启后从断点续传（日志可见跳过分片数）；放弃的任务在 `mc ls --incomplete` 无残留 | ⬜ |
| **IC-4** | CP + deploy | **webhook 可靠性止血**（IC-BUG-6 + IC-BUG-7 + IC-BUG-9；**IC-BUG-19 已于 2026-09-10 拆为独立的 IC-2c，排在 IC-2a 之前**）。① **失败语义 + 毒丸**。**⚠️ 不得按 4xx/5xx 分流**——dev 实测 MinIO 对 400 与 500 **一视同仁**（都走 `sendSync.func1()` 失败分支，日志 `returned '400 Bad Request'`）。原方案「解析失败返回 400，坏载荷不该无限重投」**不成立**：配上本刀 ③ 的持久 `queue_dir` 后，400 会被无限重投并**队头阻塞整条流**（实测 `queue_dir` 是队头阻塞单队列，失败事件重试期间后续 delete/create 全部排队、约 3s 一轮）。**统一走一条路径**：失败 → 计数 → 未超限返 5xx（MinIO 重投）→ 超限落死信 + 返 200 放行队列。三个必须定义清楚的实现点：**(a) 事件身份**——实测 webhook 请求头只有 `Host / User-Agent / Content-Length / Authorization / Content-Type`，**没有事件 ID、没有重试计数**，须用 `bucket + key + sequencer` 构造去重键；**(b) 计数器必须持久化**（Redis 或表）——放进程内存则毒丸打崩 CP 或运维重启后计数归零，永远到不了上限、毒丸永不失效；**(c) 死信要落表并写明谁 redrive**，只落日志等于无声丢数据。**(c-2) 顺带把 IC-2c 的解码失败兜底改判到死信**（PR #96 评审建议，写在这里免得靠遗忘变成永久现状）：IC-2c 现在对解码失败的键「索引原值 + Warn」，理由是丢 create 比索引怪键更糟。该兜底**从 MinIO 实际上不可达**（`QueryEscape` 的输出必然是合法转义，解码失败即意味着载荷不是 MinIO 发的）**且是对称的**（create 与后续 delete 退回同一个原值串，软删仍能精确命中，不制造删不掉的幽灵）——所以当前形态无害。但本刀有了死信表之后，解码失败应当进死信而不是进索引，属严格改进。**⚠️ (d) 落死信的 `ObjectRemoved` 必须把对应分片强制置 `active`**——「靠 IC-13 兜底」对删除类事件**不成立**：丢失的 create 是「MinIO 有 / PG 无」，L2 兜底列举能找回；而丢失的 delete 是「PG 有 / MinIO 无」，属 L3 方向，但**删除不推进 `object_keys.last_modified`**（§3.5 坑 1 只准用它判「分片被写过」），已封存的归档分片将**永不解封**，那些行成为永久幽灵（UI 可见、预签名 404、三级对账一级都检不出）。死信表里有 `bucket+key`，强制置 active 的成本近乎零。**⚠️ 与本刀 ③ 的硬序**：③ 把 `queue_dir` 改成持久卷之后，失败事件才会真正无限重投——**① 必须与 ③ 同时或先于 ③ 生效**，否则中间态就是无限重投 + 队头阻塞。**注意与本条既有验收的张力**：「断开 PG → 5xx → 恢复后补齐」在上限过小时会失败，上限须大于预期的故障时长 ÷ 3s；② `MakeBucket` 后调 `SetBucketNotification`，并在启动时对 `buckets` 表逐个 ensure（幂等补注册）。**ensure 必须是幂等替换而非追加**（否则配置越攒越乱）。**⚠️ 2026-09-10 更正**：初版称「dev 上两条重叠订阅导致删除事件双投」——**实测证伪**，订阅确实是两条（同一 ARN），但 6 次 `mc rm` 精确产生 6 个 `ObjectRemoved` 事件，MinIO 按 ARN 归并、**没有双投**。要求本身保留，但**不要照着「双投」去设计幂等去重，那个问题不存在**。**ARN 须做成可配置**——IC-11 换 NATS 后 ARN 会变（`arn:minio:sqs::primary:webhook` → NATS target 的 ARN），写死会让 IC-11 把这条打回原形；③ `queue_dir` 迁至持久卷。**⚠️ 改脚本不等于生效**——dev 实测 `notify_webhook:primary` 的 `queue_dir` 实际为**空**，与 `init-minio.sh:125` 写的 `/tmp/minio-webhook-queue` 不符（`queue_dir` 为空时 MinIO 走 `sendSync`，投递失败即丢，日志可见 `Error: not connected to target server/service`）。验收须查**生效值**（`mc admin config get`）而非脚本文本 | 断开 PG 触发 ObjectCreated → 端点 5xx → 恢复 PG 后 MinIO 重投、`file_entries` 补齐；通过 API 新建 bucket 后直接 `mc cp` 一个对象，索引出现该行 | ⬜ |
| **IC-5** | agent | **采集正确性**（IC-BUG-10 + IC-BUG-11 + IC-BUG-34 + IC-BUG-12 的超时半边）。① `IsProcessed` 改为比较 `(rule_id, local_path, file_mtime, file_size)`，并修正与实现不符的注释；② `submitFile` 补齐 `FileOffset` / `AppendMode` 赋值，tail 偏移随任务落盘；③ 按文件大小推导 per-upload timeout（可配置下限）；④ **启动时把 `running` 复位为 `pending`**（修 IC-BUG-34——当前无任何复位，崩溃时在传的任务永久卡死、既不重传也不上报；`reported` 落地后同样需要，且**必须在启动路径上做，不能只靠运行时定时器**。**⚠️ `reported` 的复位是「重发上报」不是「重传」**——对象已经在 MinIO 里了，复位成 `pending` 会让 >64MB 文件完整重传一遍；它应走 IC-2a ④ 的重报路径） | 改文件内容后能被重新采集；`append_mode=tail` 规则第二次只传增量且重启后偏移不丢；不可达 MinIO 下 worker 会超时释放而非永久占用；上传中途 kill agent，重启后任务被重新 dequeue 并完成、`queue_depth` 回落到 0；**既有文件在 agent 启动前落盘也必须被采集**（IC-BUG-37），且**积压远超事件缓冲时一个都不能丢**（PR #100 review F1） | ✅ PR #100 |

> **IC-2c 与 IC-11 正交**（2026-09-10 实测确认）：`notify_nats` 载荷的对象键编码与 webhook 完全相同，
> 编码发生在 MinIO 构造事件时而非传输层——**不能等 IC-11 一起解决**，理由与实测数据见 `bugs/open.md` IC-BUG-19。
>
> **✅ 已采纳：unescape 拆为独立的 IC-2c**（2026-09-10）。**顺序约束不等于打包约束**——它只需在上报开启
> **之前**到位，不必与 ③ 同一个 PR。单独落盘能独立 live 验证（`mc cp` 一个带层级的键即可，不依赖 agent），
> 并把 IC-2a 的子项收敛（当前 ①…⑧ + ⑤b）。**IC-2a 的验收仍须包含「同一对象经两条路径各写一次只有一行」这条回归**——
> 拆刀不等于免验，那条回归验的是两刀合起来的效果。
>
> **止血阶段顺序**：IC-1 ✅ → **IC-2c** ✅（PR #96）→ **IC-2a** ✅（PR #98，前置 PR #97 修 IC-BUG-36/35）→
> **IC-5** ✅（PR #100）→（IC-2b / IC-3 / IC-4 / IC-SEC-2 可并行）。
> **IC-2c 必须早于 IC-2a**（不是「宜早」，是硬序：见 IC-2c 一栏）——**该硬序已满足，IC-2a 已于 2026-09-11 收官**。
> **下一刀在 IC-2b / IC-3 / IC-4 / IC-5 / IC-SEC-2 之间选**（彼此可并行）。IC-2a 的 live 验收过程中新立了
> **IC-BUG-37**（watcher 的 fsnotify 分支无初始扫描，既有文件永不采集——归 IC-5，与 IC-BUG-10 同刀）与
> **IC-BUG-38**（agent 的 `log.output`/`max_size_mb`/`max_backups` 收了不用），见 `bugs/open.md`。
>
> **IC-SEC-1（插入刀，PR #94，已合并）**：IC-BUG-24 + IC-BUG-25。二者是 IC-1 的 code review 顺带挖出的
> 安全缺陷，与上报链路无关，拆出单独一刀，避免两个不相干的安全改动混进同一次 review。（**2026-09-10 更正**：原文写「原挂在 IC-2 的 ⑧⑨ 上」有误——原 IC-2 的 ⑧⑨ 是 IC-BUG-26/27，24/25 从来不在编号子项里。）
>
> **IC-SEC-2（插入刀）**：IC-BUG-26（`DeleteCollectionRule` 归属约束，**并让 `DeleteRule` 从 DB 读回真实
> `agent_id` 再发 cancel**——当前 handler 把 cancel 发给 URL 里的 agent，是 IC-BUG-29 第 6 条路径的另一半）
> + IC-BUG-27（`handleDirectoryListing` 校验收件人，照抄 IC-SEC-1 给 `dryrun.Store` 的形状）
> + IC-BUG-32（`Revoke` 的 `Send`/`Disconnect` 竞态）。三条都不挡数据面可用，沿 IC-SEC-1 的先例单独成刀。
> **IC-BUG-28 原在此刀，2026-09-10 评审后移入 IC-2a ⑧**（理由见 IC-2a 与该卡片）。
> **⚠️ 改 `Unregister` 时必须对着当前 master 的 `Disconnect` 行为验**——IC-BUG-25 已随 IC-SEC-1 合并，
> `Disconnect` **只 cancel、不 `close(SendCh)`**，`SendCh` 的所有权在 `Connect` 的 `defer Unregister` 上；
> 若顺手在 `Disconnect` 里也 close 就是重复关闭 panic（IC-SEC-1 变异测试实证过）。
>
> **IC-2 的拆分（2026-09-10）**：原 IC-2 有 ⑪ 个子项，拆为 **IC-2a**（上报主路径，原子）/ **IC-2b**
> （下发链路鲁棒性）/ **IC-SEC-2**（归属与并发）。拆分依据是「是否共享『上报成为索引主路径』这一时刻」，
> 不是按模块或按严重度。M-2 类扫描（见 `bugs/open.md`）在拆分**之前**完成，因此 IC-BUG-30/31 对 IC-2a
> 边界的两处影响（⑦ 的三分支、④ 的重报超时）已经并进来了——**先扫完再定边界，避免回头拆已关掉的刀**。
>
> **止血阶段收尾产出**：`docs/tasks/bugs/closed.md` 归档 IC-BUG-1…IC-BUG-11、IC-BUG-14…IC-BUG-34
> 中已完成者；`system-design.md` §4.5/§4.7/§5.7/§5.8/§6.3/§6.5 的「实现状态 / 实现偏差」告警块随之删除或改写。
>
> **⚠️ 归档纪律：IC-BUG-12 与 IC-BUG-13 是拆半的，不得按 ID 批量关闭。**
> 12 的上报半边随 IC-2a ⑥、超时半边留在 IC-5 ③；13 的防清空半边随 IC-2a ⑤、填值半边仍开着。
> **两半都完成前卡片保持 open**——这个 track 上已经出现过一次「标记先于验收」，卡片里写明拆法就是为了防它复发。

### 地基 — 表结构（有时间窗口，宜早不宜迟）（IC-6…IC-7）

| ID | 模块 | 内容 | 验收要点 | 状态 |
|----|------|------|----------|------|
| **IC-6** | 迁移 + CP | **宽表 / 窄表分家 + 分区**。① 新建不分区窄表 `object_keys(bucket_id, storage_path, file_entry_id, last_modified, observed_at)`，主键 `(bucket_id, storage_path)`，并从现有 `file_entries` 回填；② `file_entries` 改为按 `uploaded_at` 月 RANGE 分区（幂等键已外置，不再受「唯一约束须含分区键」约束）；③ 索引重整，去掉与唯一约束隐式索引重复的 `idx_file_entries_bucket`。**前置拍板：待定 B（分区粒度与归档策略）**。④ **`object_keys` 同样需要 `event_seq`**（§3.4 的写入守卫对两张表同规则）。⑤ **⚠️ 本刀会推翻 IC-2a 的守卫载体，不只是加列**：② 把 `file_entries` 按 `uploaded_at` 分区、幂等键外置到 `object_keys` 之后，**`UNIQUE (bucket_id, storage_path)` 从 `file_entries` 上消失**——而 IC-2a 的整个守卫建立在 `ON CONFLICT (bucket_id, storage_path)` 上，**那个 conflict target 届时不存在**。必须重写为「先 upsert `object_keys` 拿 `file_entry_id`，再按主键更新宽表」，守卫随之搬到窄表侧。⑥ **三个已查证的分区 blocker**（dev PG 实测）：(a) `file_entries` 当前 `PRIMARY KEY (id)`，分区表主键必须含分区键 → 只能变 `(id, uploaded_at)`；(b) `upload_logs` / `file_tags` / `tag_audit` **三张表的外键指向 `file_entries(id)`**，(a) 之后**无法保留**，`object_keys.file_entry_id` 同理——须先决定改为「应用层约束」还是别的形态；(c) `uploaded_at` 当前**可空**，不能直接做 RANGE 分区键。**这三条是本刀的前置，须在开工前有结论**。**⚠️ 硬前置一：重建 dev 库**——IC-2c 定案「历史脏行不清洗」，而 dev 现有 7/19 行编码键，本刀开工前必须先重建（这是一个显式步骤，不是「注意事项」）。**⚠️ 硬前置二：回填前须确认 `file_entries` 无 URL 编码残留**——IC-2c 定案「历史脏行不清洗」，而 `IndexDeletion` 对找不到的行是静默 no-op，旧的 `a%2Fb.csv` 行既不会被新事件覆盖（键不同、不冲突）也删不掉，会成为冻结僵尸行；一旦被回填进 `object_keys`，IC-13 的 L2 按 key 叶子目录切分片时它是个**无分隔符的扁平键**，必然落错分片并被判成幽灵。验收命令：`SELECT count(*) FROM file_entries WHERE storage_path LIKE '%\%2F%'` 须为 0（或 dev 库已重建）| 造 1000 万行：幂等 upsert 正确、cursor 分页 P95 达标、按 `(bucket_id, prefix)` 范围扫只命中窄表；迁移 up/down 干净且可在有数据时执行 | ⬜ |
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
| **IC-11** | deploy + CP | **事件传输 webhook → NATS JetStream**（**D-031**，对账阶段前置）。**⚠️ consumer 必须保序**：webhook 路径实测是队头阻塞单队列、天然有序，而 JetStream 的保序取决于配置（`MaxAckPending=1` 或 ordered consumer），**配错即静默失序**。索引侧不得依赖这条——同 key 因果由 IC-2a ⑤b 的 `event_seq`（`sequencer`）自证。**换传输 = 换消费入口，以下已落地的逻辑必须一并搬进 JetStream consumer，否则被静默回退**：**IC-2c 的 `url.QueryUnescape`**（D-031 扫描表把 IC-BUG-19 记为「❌ 不解决」，含义是「IC-11 不修它」，**不是「IC-11 不用管它」**）。**⚠️ 但搬过去之前先确认消费入口**（PR #96 评审实测）：MinIO 的 `ToEvent(escape)` 有两个调用方——**配置型 notification target**（webhook / `notify_nats`，`escape=true`，键是编码的）和 **`ListenBucketNotification` 流式 API**（`escape=false`，键**不编码**）。D-031 定的是 `notify_nats` **target**，所以照搬 unescape 是对的；但如果实现者图省事改用 `minio-go` 的 `ListenBucketNotification`「不用 webhook 也能收事件」，再套上 unescape 就会把 `reports/q1+q2.csv` 静默改成 `reports/q1 q2.csv`、把 `a/b%2Fc` 改成 `a/b/c`——**正好重新制造 IC-2c 刚清掉的那类脏行**。live 实证：同一个桶 `mc watch --json` 报的是字面 `+` 与字面空格，而 webhook target 收到的是 `%2B` / `+`、IC-4 ② 的 bucket 通知 ensure（ARN 改为 NATS target）、以及 IC-4 ① 的失败语义（由 HTTP 5xx 换成 nak/不 ack）。① MinIO 改配 `notify_nats` + `jetstream=on`；② CP 改用 JetStream durable consumer，**处理成功才 ack**；③ 鉴权载体由共享密钥换为 NATS creds/nkey/TLS（D-014 作废）；④ **stream sequence 只喂 `shard_state.last_event_seq`（链路自证），不得用来填 `observed_at`**——`observed_at` 是 `TIMESTAMPTZ` 而 sequence 是 `uint64`，且它只对 `minio_event` 一路单调、与另外三个 source 不可比。**D-031 原文「可直接用作 `observed_at` 来源」已就地更正**，见该决策的「更正」一节。**`observed_at` 按 source 取值见 §3.4**（`minio_event` ← `eventTime`；其余 ← PG `now()`）——**不是「一律 CP 受理时刻」**，那个中间版本已撤回；同 key 因果用 IC-2a **⑤** 的 `event_seq` | 停 CP 期间写入的对象，CP 重启后从 stream 续读并补齐索引；能按序号重放一段历史 | ⬜ |
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
| **G** | **「受理时刻」语义的固有代价**：`observed_at` 记的是 CP 何时**得知**，不是事实何时**发生**。于是 IC-2a ④ 的重发上报天然赢过删除——实测 `agent 上报(10:00:00) → 删除事件(10:00:05) → ack 丢失后超时重发(10:00:30)` 会把已删除的行复活成 `completed`。方向是反向幽灵、**L3 可收敛**，但在 L3 跑到之前 UI 上已删文件会重新出现 | 不阻塞 | 倾向接受并写进「本设计不解决什么」；若不接受则需在重发时携带原始事实时刻 |
| ~~F~~ | ~~`observed_at` 的时钟归属~~ | ~~IC-2a~~ | ✅ **已定（2026-09-10，两次修订后定稿）：按 source 取各自最可信且客户端左右不了的时刻**——`minio_event` ← 载荷的 `eventTime`（**MinIO 生成**）；`agent`/`api` ← **PostgreSQL 的 `now()`**（§3.5 坑 3 的硬约束，不是 CP 进程时钟）；`audit` ← **列举那一刻**（不是写入事务的 `now()`——否则陈旧列举会覆盖更新数据并污染封存判据，见 §3.4 实测）。删除/创建因果另用事件的 `sequencer`（`event_seq`，仅在 `observed_at` 相等时决胜）。**中间曾定「四源统一取 CP 受理时刻」，评审用真 PG 证伪后撤回**（谓词恒真、闸门变摆设）。完整模型、守卫 SQL 与实测数据见 `consistency-and-ingest.md` §3.4 |

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
