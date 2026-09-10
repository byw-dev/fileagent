# 文件索引一致性与写入准入设计

> **状态**：设计（2026-09-08 拍板方向，尚未实施）
> **决策记录**：[`DECISIONS.md`](../../DECISIONS.md) **D-030** / **D-031**（事件传输改 JetStream）
> **缺陷清单**：[`docs/tasks/bugs/open.md`](../tasks/bugs/open.md) IC-BUG-1…IC-BUG-34
> **关联**：`system-design.md` §4.5（上传流程）/ §4.7（凭据轮转）/ §5.7（STS）/ §5.8（索引）/ §6.3（Policy）/ §6.5（事件通知）；
> [`metadata-model.md`](./metadata-model.md) P2.2（衍生数据入口）/ P2.3（run 血缘）；D-014、D-017、D-025
>
> 本文只讨论**对象如何进入索引、索引如何与 MinIO 保持一致**。标签词表治理、UI、SDK 接口形状不在范围内。

---

## 1. 为什么必须改

### 1.1 触发点

产品明确了两件此前不成立的前提：

1. **必须允许 Agent 之外的进程写入 bucket**——ETL 对数据文件加工后要回写，并且要记录 tags 与血缘；
2. **对象数量级**：按 `system-design.md` §6.7 第一行（高频小文件、50 台采集器）换算约 **2600 万对象/年**，
   3–5 年到**上亿**。§6.7 只按字节规划过容量，从未按对象数推演过。

在此之前，索引一致性被隐式假设为「事件到了就写、没到就算了」。这两条前提让该假设不再可接受。

### 1.2 现状：唯一的写入路径是兜底路径

代码里有两条写 `file_entries` 的路径，**只有一条是活的，而且是设计中作为兜底的那条**：

| 路径 | 入口 | 设计意图 | 实际 |
|---|---|---|---|
| ① Agent gRPC `UploadResult` → `indexer.HandleUploadResult` | `grpcserver/handler.go:247` | **主路径** | **死代码**（IC-BUG-1 + IC-BUG-2） |
| ② MinIO webhook → `indexer.IndexUpload` / `IndexDeletion` | `api/handler/events.go:770` | 「只做对账兜底」（D-025 补充 §3） | **唯一活着的写入者** |

补充事实：

- **NATS 不是入站通道。** MinIO 用 `notify_webhook` 走 HTTP 打到 CP（`init-minio.sh:121`）。
  NATS 只是 CP 自己的出站总线 + 自订阅的规则引擎（`event/engine.go:116`），且用的是 **core NATS**
  （`conn.Publish`）而非 JetStream——无持久化、无重放、无 ack。
  **目标形态见 D-031**：改用 `notify_nats` + JetStream，排期本文 §4 的对账阶段（IC-11）。
- **没有任何对账。** `worker/` 只有 `offline_sweeper`（Agent 在线状态）与 `retag`（改 `file_tags`）；
  全 controlplane 无一处 `ListObjects` / `StatObject`——**CP 从不验证对象是否真的存在**。
- 因此 `events.file.uploaded` **从未被发布过**（唯一发布者在死掉的 ①，活着的 ② 为避免重复发射刻意不发），
  所有 `file_uploaded` 事件规则是死配置。

### 1.3 事件作为事实来源的三个不足

**A. 事件丢了就是永久丢，且无人能发现。** 丢失通道有三条且相互独立：
`queue_dir` 位于 `/tmp`（IC-BUG-9）、`queue_limit=10000` 溢出、CP 索引失败仍返回 200（IC-BUG-6）。
叠加「无对账」，任何一次丢失都是终态。

**B. 事件路径写入的行是贫血的，且会覆盖富数据。** `IndexUpload` 只有 bucket/key/size/etag，
而 upsert 是**无排序键的 last-writer-wins**（IC-BUG-8）——修好 ① 之后会立刻变成数据损坏。

**C. 删除没有因果保护。** 「删旧对象 → 同 key 重传 → 删除事件晚到」会把新对象标成 deleted，且永不纠正。

---

## 2. 为什么不换存储层

产品曾提出是否改用其他存储层。结论是**换存储层解决不了这里的任何一个需求**，理由如下。

需求分解后，只有一条与存储实现有关：

| 需求 | 所属层 | 对象存储能否解决 |
|---|---|---|
| 允许非 Agent 写入方 | **准入协议** | 否 |
| 对象带 tags | 目录 / catalog | 勉强，且有致命限制（见下） |
| 血缘关系 | 目录 / catalog（且是**图**） | 否 |
| 数量剧增下的索引一致性 | **对账成本的量纲** | 否 |

**被评估并否决的替代方案：**

| 方案 | 能解决 | 否决理由 |
|---|---|---|
| 换其他 S3 实现（Ceph RGW 等） | 无 | 同样的事件与一致性模型，纯换皮 |
| **lakeFS**（叠在 MinIO 上） | 原子多对象提交、分支、commit 历史≈粗粒度血缘 | 进数据面（代理/网关），引入一个重组件；它的 catalog 是自有的，本系统的受控标签与 run 血缘仍需自建；commit 语义对「每分钟一张图的持续流」不成立——每文件一 commit 则元数据爆炸，批 commit 则重新引入延迟与一致性问题 |
| **Iceberg / Delta** | 事务性目录、快照、时间旅行 | 是**表格式**，管的是行不是任意二进制文件，采集载荷（图像/日志/二进制）无法纳入。退化用法「存对象清单表」等于把 PG 换到对象存储上，而 PG 在 1e8 行并非瓶颈 |
| **S3 Object Tagging**（让对象自描述） | PG 可从 MinIO 重建，作为冗余副本有价值 | 上限 10 tag / key≤128 / value≤256；**血缘是图，无法表达**；致命点：`ListObjectsV2` **不返回 tag**，读取需每对象一次 `GetObjectTagging`，把对账从 O(N) 次列举变成 O(N) 次请求，比现状更差。可作灾难重建的冗余，不能作主索引 |
| 换掉 PostgreSQL（ClickHouse 等） | 仅解决「行多查询慢」 | 那是第二顺位问题；标签与血缘的写入需要事务，换掉更难。分区表足以应对 |

唯一会让换存储成立的条件是「MinIO 在目标对象量级上元数据操作退化到不可用」。
但该情形下正确的应对仍是**减少列举**而非更换实现——列举成本是 O(总量)，换硬件只改常数、不改量纲。

---

## 3. 目标架构

### 3.1 原则

```
事实来源 = 写入方的持久化意图（outbox） + CP 的授权记录（grant）
MinIO 事件 = 低延迟提示，不是事实来源
对账成本 = O(未结算的授权) + O(总量 / 轮转预算)     ← 两项都有界且可配
```

### 3.2 写入准入：STS 授权即写入意向声明

**产品已确认可接受的运维约束**：数据桶不下发长期 access key，所有写入必须先向 CP 申请 STS。
于是「绕过索引写入」从默认行为变成需要 root 凭据的显式运维动作。

CP 本来就在签发 STS，即 **CP 在对象被写之前就知道有人要往哪个桶写、写多久**——
这个信息目前被丢弃了。记下来即可：

```sql
write_grants(
    id, org_id,
    principal_type,        -- agent | api
    principal_id,
    bucket_id,
    issued_at, expires_at,
    state,                 -- active | settled | unsettled
    declared_count,        -- 写入方结算时申报的对象数（可空）
    registered_count,      -- CP 侧实际收到的注册数
    settled_at
)
```

**结算语义让稳态下零列举**：写入方 outbox 清空时调 `POST /api/v1/grants/{id}/settle {count: N}`，
CP 比对 `registered_count == N` → 标记 `settled`，**不发一次 ListObjects**。

**未结算时也不列举。** grant 数量不符或到期未结算，只说明「这个窗口里可能有对象没注册上来」。
此时从 PG 自身查出该窗口有写入的分片，置为 `active` 交给 L2 的分片预算去核实：

```sql
SELECT DISTINCT <分片前缀> FROM object_keys
WHERE bucket_id = $1 AND last_modified >= $2 AND last_modified < $3
```

`<分片前缀>` 必须与 §3.5 的分片定义**用同一个函数**（叶子目录，按桶可配层数覆盖），
否则 L1 置 `active` 的前缀会落不到 `shard_state` 的任何一行上。
零列举，走 `(bucket_id, last_modified)` 索引，与 §3.5 的解封检测是同一个查询形状。
**L1 只负责低延迟发现分歧，实际核实一律走 L2 的预算**——避免「O(1) 的信号触发 O(总量) 的响应」。

时间窗口 `$2/$3` 取 grant 的 `[issued_at, expires_at]` **并向两端各留一个时钟偏移余量**——
grant 时间来自 CP 时钟，而 `last_modified` 来自 MinIO 时钟或事件时刻，两者不同源。

> **这条升级路径有一个已知缺口，必须写明而不是掩盖**：若该 grant 写入的对象**一条都没进
> `object_keys`**，上面的查询返回空集，L1 检出了分歧却产出零工作量。实践中这个缺口比看上去窄——
> minio-event 通道通常已把对象以贫血行写入 `object_keys`，所以窗口内一般查得到前缀；
> 只有「register 与 event 双双丢失」才落空。此时由 §3.5 第 8 条的**兜底通道**（真实 delimiter
> 列举）覆盖，这是已接受的残余风险，与「MinIO 静默不发事件」同级。
>
> **未结算的 grant 必须告警，不能只静默置 `active`。** L2 补得回对象的**存在性**，
> 补不回 register 本该携带的 `tags` / `run_id` / `sha256`——那些随注册请求一起永久丢失了。

#### 授权宽度与扫描成本是两件事

**policy 资源写整桶**（`arn:aws:s3:::{bucket}/*`），不按前缀收窄。这是一次明确的产品取舍：

- **授权宽度是管理权限问题**。单组织私有化部署、Agent 需审批才能接入，一个已审批 Agent
  拿到所属桶的**写**权限是可接受的。
- **清点成本是技术缺陷**。它不能靠约束用户来解决，必须由机制解决——即 §3.5 的分片 + 封存。

> ⚠️ **放开的只有「写」，不含「读」与「列举」。** 产品批准的是写整桶，不能借机顺带放开其余。
> Agent 全仓库从不调用 `GetObject` / `StatObject` / `ListObjects`（已 grep 确认，只用 `PutObject`
> 与 multipart），因此 `s3:GetObject` 与 `s3:ListBucket` 在整桶形态下是**纯超授**——
> 若保留，任一已审批 Agent 可枚举并下载整个数据湖。按最小权限一并砍掉，与砍 `DeleteObject`
> 是同一条论证。最终 Action 集合见 IC-BUG-4。
>
> `policy.go:41-43` 在 `buckets` 为空时兜底成 `arn:aws:s3:::*`（**全部桶**）。当前调用方恒传
> 一个桶所以不可达，但 IC-1 重构时应删掉——别在「整桶」决策之上留一个「整集群」后门。

这条取舍消掉了一整类故障，且大幅简化 IC-1：

**最直接的一条**：按字面的「模板**静态**前缀」计算，webui 新建规则的默认模板
`/{agent_name}/{time:yyyy/MM/dd}/{filename}`（`webui/src/pages/Agents/RuleForm.tsx:104`）的静态前缀
是**空串**——收窄当场退化成整桶。也就是说「按静态前缀收窄」对经 UI 创建的规则根本不产生收窄效果，
只是把整桶写成了一个更复杂的表达式。

若改用「签发时求值」的变体（把 `{agent_name}` / `{time:…}` 代入后再取前缀），前缀确实变窄了，
但会引入下面这些故障——整桶 policy 一并消掉：

| 「签发时求值」变体会带来的问题 | 整桶 policy |
|---|---|
| IC-BUG-3（policy 前缀 ≠ 实际对象键） | 不存在——整桶必然覆盖模板产出的任何键 |
| 前缀含日期时跨零点 403（STS TTL 1h，刷新只在剩余 <10min 触发） | 不存在 |
| Agent 改名后持续 403（`agentCtx.AgentName` 只在审批时取一次，永不更新） | 不存在 |
| 多规则 Agent 的 session policy 体积上限（未实测，属推测） | 不存在 |

> **`dest_path_template` 不受任何约束**——不强制前缀、不要求首段可解析、不要求含时间字段。
> 对象键就是模板渲染的结果，CP 不往里面注入任何东西。
>
> 曾考虑过三种约束（强制 `agents/{agent_id}/` 前缀 / 首段不得为逐文件变量 / 必须含时间分区），
> **全部否决**。前两者只为「让 grant 前缀非空」服务，而前缀已不承担对账职责；第三者被
> minio-inventory 的生产实测直接推翻：karadar 桶 260 个分片里 **258 个**的首次写入比目录名上的
> 日期晚 7 天以上、246 个晚 30 天以上、最长晚 232 天，且是持续行为。任何「路径含日期 →
> 只扫当天分区」的调度都会把这 246 个正在被写入的分片判为陈旧、永不扫描。
> **根本理由**：命名约定能说明新数据落在哪，**不能说明历史分区没被改过**，因此不能充当正确性机制。
> 见 `~/workspace/minio-inventory` `docs/01-审计可扩展性设计.md` §2.3。

> STS 仍然重要，但它提供的是**写入意向的时间窗口**（谁、哪个桶、什么时段），不是空间范围。
> 对账的空间切分由分片承担。

### 3.3 统一写入协议（Agent / SDK / ETL 共用）

1. **申请**：CP 签发 STS + `grant_id`（Agent 走现有 gRPC `CredentialsPayload`，SDK 走 REST）
2. **直传**：写入方直传 MinIO，文件内容**不经过 CP**（架构原则不变）
3. **注册**：`POST /api/v1/files/register`（批量、幂等），携带
   `storage_path / size / sha256 / etag / file_mtime / tags / run_id / grant_id`
4. **重试**：注册未成功则留在写入方本地 outbox 重试；Agent 复用现有 SQLite 队列，
   SDK 需在客户端提供同等语义
5. **结算**：outbox 清空后调 `POST /api/v1/grants/{id}/settle`

`minio-event` 退回设计中的角色：**低延迟提示 + 删除检测 + 对账兜底**。
它写入的行标记 `source=minio_event`，且不得覆盖已注册的富字段（见 3.4）。

> 与 [`metadata-model.md`](./metadata-model.md) P2.2 的关系：P2.2 已经定下「ETL 禁止直连 MinIO 读写，
> 走 CP 发 STS → 直传 → 注册携带 tags，注册即打标」。本节是它的完整化——补上 grant 与结算，
> 并把同一协议**回收适用于 Agent**（原本 Agent 走的是 gRPC 上报，语义相同、通道不同）。

### 3.4 表结构：宽表 / 窄表分家

`file_entries` 在 1e8 行时无法同时满足两种访问模式：

- 业务查询：按 `org_id` + 时间倒序翻页 + tag 筛选
- 对账：按 `(bucket_id, prefix)` 范围扫 key

二者对分区键的要求冲突，而 **PostgreSQL 要求分区表的唯一约束必须包含分区键**——
`UNIQUE (bucket_id, storage_path)` 一旦按时间分区就退化为 `(bucket_id, storage_path, uploaded_at)`，
**幂等性直接失效**（同 key 不同时间会插入两行）。

因此拆表：

```sql
-- 窄表：幂等键 + 对账专用，不分区，索引可常驻内存
object_keys(
    bucket_id      UUID NOT NULL,
    storage_path   TEXT NOT NULL,
    file_entry_id  UUID NOT NULL,
    last_modified  TIMESTAMPTZ,     -- 对象自身的 mtime，对账信号列（见 3.5 警告）
    observed_at    TIMESTAMPTZ NOT NULL,
    event_seq      TEXT,            -- 事件 sequencer，与 file_entries 同规则参与守卫
    PRIMARY KEY (bucket_id, storage_path)
);

-- 宽表：业务查询，按 uploaded_at 月 RANGE 分区
file_entries(..., observed_at, source, grant_id, run_id, ...);
```

收益：

- 幂等键完整保留在不分区的窄表上
- `file_entries` 可按时间分区 → 列表/cursor 分页最优，老数据可归档冷存
- **对账只扫 `object_keys`，完全不碰宽表**——该窄表只有 6 列（`bucket_id / storage_path / file_entry_id / last_modified / observed_at / event_seq`），
  行宽远小于 `file_entries`；但 `storage_path` 是变长 TEXT，主键 btree 的体积高度依赖真实 key 长度，
  **具体容量与「索引能否常驻内存」须随 §6-B 一并实测估算，不要直接引用某个拍脑袋的数字**
- `object_keys` 在角色上等同于 minio-inventory 项目的 `minio_objects`

> ### ⚠️ 本节的 SQL 以代码为准（2026-09-10，第五轮评审后）
>
> 下面这段守卫连续三轮「新写 → 一执行就碎」——谓词恒真、NULL 吞写、`RETURNING` 返 0 行、
> `audit` 覆盖更新数据，**每一条都是在真 PG 上跑起来才发现的，读文档四轮都没读出来**。
> 因此 **IC-2a 的第一步（⓪）是把它落成仓库里的真迁移 + 真 upsert + 表驱动测试（含变异开关）**，
> 此后**本节只表达意图与不变式，SQL 文本以代码为准**（CLAUDE.md：代码是最终真相）。
> 下面的 SQL 是 PoC 的输入，不是权威。

**排序与因果：三个字段各司其职（2026-09-10 定案，多次修订后定稿）**——最初的错误是让一个字段兼两职。

| 用途 | 字段 | 来源 | 说明 |
|---|---|---|---|
| **防覆盖闸门** | `observed_at TIMESTAMPTZ NOT NULL` | **每个 source 取自己能提供的、最可信的「对象处于此状态」时刻**（见下表）| 判据是「**这个值客户端能不能左右**」，不是「来自哪个时钟」|
| **同 key 并列时的决胜** | `event_seq TEXT`（可空，仅 `minio_event` 写）| 事件的 **`sequencer`** | 仅在 `observed_at` **相等**且两侧均非 NULL 时参与比较。S3 专为同 key 排序定义，补零后字典序，**不可跨 key 比较** |
| **业务事实「文件何时上传」** | `uploaded_at`（列已存在）| agent 上报值 | **只作数据，不作排序键** |

`observed_at` 的取值来源：

| source | 取自 | 客户端可控？ |
|---|---|---|
| `minio_event` | 事件载荷的 **`eventTime`** | ❌ MinIO 生成 |
| `agent` | **PostgreSQL 的 `now()`**（写入事务内取），**不是 CP 进程时钟** | ❌ |
| `api` | 同上 | ❌ |
| `audit` | **列举那一刻**（或对象自身的 `LastModified`，与 `eventTime` 同时钟域）——**不是写入事务的 `now()`** | ❌ |

> **⚠️ `audit` 取写入时刻会让陈旧列举覆盖更新的数据（实测）**：L2 于 10:00 列举到 K = v1(size 55)
> → 10:01 agent 上报 v2(size 100) → 10:02 审计批次落库，若 `observed_at` 取写入事务的 `now()`=10:02
> 则 **> 10:01、守卫放行**，`size_bytes` 被写回 55、`etag` 写回 E1、`source` 变 `audit`——正是守卫本该
> 保护的那几列。更糟的是同一条写入会把 `object_keys.last_modified` 也写回旧值，而 §3.5 坑 1 规定封存
> 判据**只能看 `last_modified`** → 该分片被误判「未被写过」而封存，真实状态一级对账都查不出。
> 列举与落库之间天然有延迟（批量对账尤甚），所以这不是边界情形。

> **⚠️ 必须用 PG `now()` 而不是 CP 进程的 `time.Now()`**——这不是风格问题。§3.5「坑 3」已经定死
> 「**所有参与比较的时间戳必须来自同一个时钟，统一由 PostgreSQL 的 `now()` 产生**」，本表遵从它。
> **dev 实测（2026-09-10，5 次采样）**：MinIO 的 `eventTime` 比 CP 进程时钟快 **16.0–16.6 ms**，
> 而 PG 的 `clock_timestamp()` 与 MinIO 同属 docker VM 时钟（比宿主机快 ~11.5 ms）——**掉队的是跑在
> 宿主机上的 CP 进程**。若 `agent` 源用 CP 进程时钟，则在「agent PUT 完成 → MinIO 立刻发事件 →
> agent 随后经 gRPC 上报」这条**生产常态**路径上，webhook 的 `observed_at` 恒大于 agent 的，
> **每一次合法 agent 上报都会被守卫拦掉**，`agent_id`/`rule_id`/`sha256` 恒 NULL——而这正是
> IC-2a live 验收第一条要断言非空的那几列，症状还是间歇性的，实现者会去查根本不存在的 gRPC 问题。
>
> **部署约束（须写进运维文档）**：`minio_event` 用 MinIO 时钟、其余三路用 PG 时钟，
> **PostgreSQL 与 MinIO 必须处于同一时钟域**（同一宿主/VM，或同一 NTP 源）。当前 compose 拓扑天然满足。
> 若将来二者分处不同时钟域，本模型须重新评估。

> **⚠️ 绝不采信 `UploadResult.uploaded_at`**（`proto/v1/agent.proto:65`）。它由 agent 提供，一台时钟
> 跑飞或被入侵的 agent 报 `2099-01-01` 就能把该行永久冻结，此后 webhook / `register` /
> **甚至 IC-13 对账**的写入全被静默丢弃、且对账无法纠正。这是 M-1「客户端指定，无人校验」那一类。
>
> **⚠️ 但也不能因此把 `minio_event` 一并改成 CP 受理时刻**（2026-09-10 曾如此定案，评审证伪后撤回）。
> `eventTime` 由 **MinIO** 生成——MinIO 是基础设施，不是客户端，与「被入侵的 agent」不是同一类风险。
> 若四源统一取 CP 受理时刻，则**后处理的写入其 `observed_at` 必然更大、`>=` 谓词恒真、闸门变摆设**。
> 而 `IndexUpload` 写入的 `size_bytes` / `status` / `uploaded_at` / `etag` 都是非空值
> （`indexer/indexer.go:542-551`），**`COALESCE` 一列都保护不到**——闸门失效即等于 IC-BUG-8 没修。
> 真实失败场景：滞留重试中的旧事件 E1(v1) 在 agent 写入 v2 之后到达，把 `size_bytes`/`etag` 写回 v1，
> 而 `sha256` 因 `COALESCE` 停在 v2 —— 一行自相矛盾的数据，且 IC-13 对账会据此误判。
>
> **代价（显式记录的假设）**：`minio_event` 用 MinIO 时钟、其余三路用 CP 时钟，**跨两个时钟比较**。
> 二者都是我们自己的基础设施、同一 docker 网络 / 同一 NTP 源，这个假设比「信任任意边缘 agent」
> 弱一个数量级，接受。若将来 MinIO 与 CP 分处不同时钟域，须重新评估。

**写入守卫**（`file_entries` 与 `object_keys` 同规则）：

```sql
-- upsert：ON CONFLICT ... DO UPDATE ... WHERE
    EXCLUDED.observed_at > fe.observed_at
 OR (EXCLUDED.observed_at = fe.observed_at
     AND (EXCLUDED.event_seq IS NULL OR fe.event_seq IS NULL          -- 任一侧无 seq → 不用它决胜
          OR lpad(EXCLUDED.event_seq,32,'0') >= lpad(fe.event_seq,32,'0')))
```

> **⚠️ NULL 必须放行，不能收紧。** 若写成 `AND lpad(EXCLUDED.event_seq,…) >= lpad(fe.event_seq,…)`，
> 则 agent 上报（`event_seq` 恒 NULL）撞上 webhook 先建的行时 `NULL >= '18D3…'` 求值为 NULL、
> `WHERE` 不为真 → **每一次合法的 agent 上报都被静默吞掉**。而「webhook 先到、agent 后到」是
> 生产上的常态（MinIO 在 PUT 完成即发事件，agent 随后才走 gRPC 上报）。这属于
> 「拦住攻击者也拦住所有合法调用方」，`code-reviewer.md` 明确要求防的那一类。
>
> 补零宽度取 **32**：实测 MinIO 的 `sequencer` 恒为 16 位十六进制，取 32 留冗余且不影响正确性。
>
> SQL 里的 `fe` 是 `INSERT INTO file_entries AS fe` 的别名，照抄时别漏掉 `AS fe`。

> ### ⚠️ 守卫落败**不是错误**——`RETURNING` 会返回 0 行
>
> 现有 upsert 带 `RETURNING`，`UpsertFileEntry` 用 `QueryRowContext` + `row.Scan` 读**恰好一行**
> （`indexer/queries.go:61-68,86`）。加上 `WHERE` 守卫后，**被正确压制的写入会返回 0 行 →
> `sql.ErrNoRows` → `indexer.go:553` 包成 error 上抛**。
>
> 后果不是「多一条日志」，而是**索引 feed 停摆**：IC-4 ① 会把这个 error 判为「处理失败」→ 计数 → 返 5xx
> → 而 MinIO 的 `queue_dir` 是**队头阻塞单队列**（实测：立即重试 1 次 + 之后每 3.0s），这条**本来就该被
> 压制**的陈旧事件会卡在队头，让该 target 的所有新事件排队数分钟到数十分钟，直到它被落死信放行。
>
> **规格要求**：`UpsertFileEntry` 必须**恒返一行**，调用方按成功处理、**不计入 IC-4 ① 的失败计数**。
> 二选一：
> - **(推荐) 调用方处理**：`ErrNoRows` 时回查现有行返回，并标记 `suppressed=true` 供日志/指标；
>   SQL 保持可读。压制是罕见路径，多一次查询可接受。
> - **纯 SQL**：把每列改成 `col = CASE WHEN <守卫> THEN EXCLUDED.col ELSE fe.col END`、去掉 `WHERE`，
>   则永远走 UPDATE 分支、恒返一行。代价是 12 列都要重复一遍守卫表达式。
>
> **无论选哪个，`ErrNoRows` 都不得作为错误传播到 webhook / gRPC 的响应路径上。**
>
> **⚠️ 压制路径还必须跳过副作用。** upsert 之后调用方紧接着会 `applyStaticTags` / `applyPathVarTags`
> （`indexer.go:220-224`）并 `publishFileUploaded`（`indexer.go:252`）。按字面实现，每一条**被正确压制**的
> 陈旧事件或重发上报都会照常发一条 `events.file.uploaded`，而载荷是**当前那行更新后的数据**——下游
> 6c 打标与将来的 ETL 拿到的是幽灵事件。**压制路径只记指标，不打标、不发事件。**

**软删除同样要守卫，且它不是 upsert——改动面比「加个 WHERE」大。** 软删除走的是独立的
`MarkFileEntryDeleted`（`indexer/queries.go:209-216`），当前是 `UPDATE … SET status='deleted',
updated_at=NOW() WHERE bucket_id=$1 AND storage_path=$2 AND status != 'deleted'`——**没有任何时间围栏**。

需要做两件事，**缺第二件会造成已删除的行被复活**：

1. **加守卫**（同上）：否则 key `K` 的删除事件 E_del 滞留重试 → agent 重新采集并上传 `K` →
   E_del 重投成功 → 把**刚建的活对象**标成 `deleted`。文件在 MinIO 里活着、在 UI 里消失，
   而 L3 幽灵清理管的是反方向（PG 有 / MinIO 无），**检不出来**。
2. **新增 `observed_at` / `event_seq` 两个入参并写回**（`SET … observed_at = $3, event_seq = $4`）。
   只加 `WHERE` 不推进这两列，则删除后行上仍是**创建时**的值，此后任何一次 create 事件重投
   （IC-11「处理成功才 ack」，重投是设计出来的常态）都会 `observed_at` 打平、`event_seq` 自比自相等
   而通过守卫，**把已删除的行复活成 `completed`**。实测确认：`create(T1,seqA) → delete(T3,seqB) →
   create 重投` 在只加 WHERE 的实现下复活。

> **⚠️ 剩余缺口（本节不闭合，由 L3 兜）**：删除事件落在**不存在的行**上时 UPDATE 空转、**不留墓碑**，
> 随后旧的 create 事件被重放会直接走 INSERT 分支建出一行 `completed`——守卫对 INSERT 无能为力
> （没有旧 `observed_at` 可比）。触发路径真实存在：`queue_dir` 为空时 MinIO `sendSync` 直接丢事件
> （实测容器日志 `not connected to target server/service`），create 丢、delete 到达即成立；IC-11 的
> 可重放会让它更易命中。方向是**反向幽灵**（PG 有 / MinIO 无），**由 §3.5 的 L3 收敛**。
> 若将来要闭合，需引入删除墓碑行，代价另议。

富字段一律 `COALESCE(EXCLUDED.x, 现有值)`。两条写入路径从此可交换、不再互相踩（修 IC-BUG-8 / IC-BUG-13）。

`source` 取值：`agent | api | minio_event | audit`。**该枚举被 D-030 与 IC-13 依赖，不得塞入其他语义**
（如「元数据缺失」须另立列，见 IC-2a ⑤ 的 `meta_incomplete`）。

> **实测记录（2026-09-10 dev）**：① 基线（`queue_dir` 空、走 `sendSync`）同一 key 反复 create/delete
> 12 次，到达顺序与 `eventTime`、`sequencer` 三者完全一致、无重复值；② 重试路径（配 `queue_dir`、
> 对前 3 次投递返回 500）MinIO 的队列是**队头阻塞的单队列**，失败事件重试期间（约 3s 一轮）后续事件
> 全部排队，成功后按原序放行——**重试不产生乱序**。
>
> ⚠️ **但不要把正确性押在「传输保序」上**。IC-11 换 JetStream 后保序取决于 consumer 配置
> （`MaxAckPending=1` 或 ordered consumer），配错即静默失序。`event_seq` 的存在就是让这条不变式
> **自证**而非依赖配置——这正是本 track 反复吃亏的地方。

> ⚠️ **不要用 JetStream 的 stream sequence 充当 `observed_at`**。D-031 上文曾写「可直接用作 `observed_at`
> 来源」，**该说法已在 D-031 就地更正**：sequence 是 `uint64` 与本列的 `TIMESTAMPTZ` 类型不符，
> 且它只对 `minio_event` 一路单调。sequence 的正确用途只有一个：喂 `shard_state.last_event_seq`
> 做链路自证（见 §3.5）。事件的 `sequencer`（`event_seq`）与它是两回事，不要混淆。

### 3.5 对账三级

| 级别 | 触发 | 覆盖场景 | 成本 |
|---|---|---|---|
| **L1 grant 结算** | grant 过期或数量不符 | 正常写入丢注册 | O(写入速率)，**全程零列举**（异常时置分片 active，不自己扫） |
| **L2 分片轮转** | 后台预算驱动 | grant 丢失 / 绕过写入 / MinIO 静默不发事件 | O(总量 / 预算) |
| **L3 幽灵清理** | 随 L2 分片扫描顺带（L1 已不列举） | PG 有行、MinIO 无对象 | 附带 |

L2 直接采用外部项目 minio-inventory 已在生产验证过的分片审计模型
（`shard_state(bucket_id, prefix, state, last_verified_at, last_event_seq, sealed_at, object_count)`，
state ∈ `active | verified | sealed`；分片取 key 的叶子目录；
主通道从 PG 自身推导前缀，兜底通道才真去 `ListObjects`）。

该模型 2026-09-08 上线、2026-09-09 完成首轮核实，实测 radar 桶每轮扫描量从 171 万对象降到
7.2 万（封存率 95.8%），一轮从 5h31m 降到约 14 分钟。**成本量纲**：

```
O(未结算的授权) + O(总量 / 轮转预算)      ← 两项都有界且可配，不再是 O(总量)/周期
```

**必须原样避开它踩过的坑**：

1. **判「分片被写过」只能用对象自身的 mtime**（`object_keys.last_modified`），
   **绝不能用 `updated_at` / `observed_at`**——扫描自身的 upsert 会推进后者，
   导致刚核实完的分片立刻显示「被写过」，整个封存机制失效、收益归零。
2. **`recursive` 的判据是分片之间的包含关系，不是层数**——
   一个分片拥有其前缀下全部内容，当且仅当没有别的分片前缀是它的严格延伸。
3. **所有参与比较的时间戳必须来自同一个时钟**，统一由 PostgreSQL 的 `now()` 产生；
   幽灵清理必须按 key 范围收窄 + 时间围栏，否则一次分片扫描会删掉整个桶。
4. **封存静默期按桶从运行数据反推**，不写死全局值（不同桶的安全值可相差 10 倍）。
5. **轮转配置的是列举预算，周期是结果**——固定周期会在列举性能退化时让占用率不受控上升。
6. **批大小按对象数限，不按分片数限**。分片大小可跨三个数量级，按分片数限会让单批达数十万对象、
   持锁数十分钟，而按批加锁的全部意义就是批间释放。
7. **`verified` 分片需要重查下限**（默认 1h）。只按「陈旧程度」排序而无下限时，小桶的全部分片
   一批装得下、扫完立刻又全部合格，实测出现过每 9 秒重新完整列举一遍。**从未核实过的分片不受此限**，
   否则首轮追平会被拖慢。
8. **双通道发现有明确盲区**：主通道从 PG 推导前缀，发现不了「全部对象都不在 PG 里」的分片——
   这正是兜底通道（真实 delimiter 列举）存在的唯一理由，其周期须与 L3 轮转对齐。

**`last_event_seq` 是 D-031 的硬依赖。** 「这个分片是否已知完整」需要二维状态回答——
*扫描核实到 T 时刻，且事件已消费到序号 X*。只有扫描时间不够：扫描期间与之后的变更由事件覆盖，
必须知道事件消费位置才能推断覆盖范围。**HTTP webhook 没有单调序号，拿不到 X**，
因此 D-031（webhook → NATS JetStream）不是优化项而是 L2 的前置条件，须在 IC-11 先落地。

CP 已持有一个 root 权限的 MinIO client（`cmd/server/main.go:206`），可直接复用作列举通道。

### 3.6 血缘

将 [`metadata-model.md`](./metadata-model.md) P2.3 的 run 模型提前实施
（触发信号「ETL 开始建设」已到达）：`lineage_runs` / `run_inputs` / `file_entries.run_id`。

两个天然挂载点，使 ETL 零额外申报：

- **读**：`POST /api/v1/files/batch-download-urls` 携带 `run_id` → 自动写 `run_inputs`
- **写**：`POST /api/v1/files/register` 携带 `run_id` → 输出文件挂到同一 run

`输出文件 → run → 输入文件集合` 即文件级血缘，1:1 / 1:n / n:1 统一表达。

---

## 4. 分期实施

| 阶段 | 内容 | 依赖 / 说明 |
|---|---|---|
| **止血**（IC-1…IC-5） | 修 IC-BUG-1…IC-BUG-7、IC-BUG-9…IC-BUG-12、IC-BUG-16…IC-BUG-17：接通 STS 链路、Agent 上报 `UploadResult` + `Acknowledgement`、队列增 `reported` 状态、续传状态落盘 + Abort + ILM、新建 bucket 注册通知、webhook 失败返回 5xx + `queue_dir` 持久化、判重比较 mtime/size | 独立可发；**此前数据面从未端到端跑通** |
| **地基**（IC-6…IC-7） | `observed_at` / `source` / `grant_id` / `run_id` 列、`object_keys` 拆分、分区、排序键 upsert（修 IC-BUG-8 / IC-BUG-13） | ⚠️ **必须趁数据量小完成**——到千万行再拆表、加列、改分区，每步都要锁表或双写迁移 |
| **准入**（IC-8…IC-10） | `write_grants` + `POST /files/register` + `POST /grants/{id}/settle` + SDK 写入协议 | 依赖地基阶段的列；policy 保持整桶，grant 记录的是**时间窗口**而非空间范围（§3.2） |
| **对账**（IC-11…IC-13） | 事件传输改 JetStream（**D-031**，前置）→ L1 → L2 → L3 | 依赖地基（`object_keys.last_modified` 是 L2 唯一可用的信号列）；JetStream 提供重放与全局单调序号，是 L2「链路自证」的前提 |
| **血缘**（IC-14） | run 模型（P2.3） | 随 ETL 建设 |

> **`Acknowledgement` 无需新增 proto 消息**——它早已定义（`proto/v1/agent.proto:124,174-178`，
> 含 `ref_message_id` / `success` / `error`），只是两端都未接：CP 从不发送，Agent 侧是显式 no-op
> （`agent/cmd/agent/main.go:254-255`）。`UploadResult` 需补 `task_id`（只增字段，符合契约规则）。

---

## 5. 本设计不解决什么

诚实列出，避免日后误认为是银弹：

- **不消除「MinIO 静默不发事件」的残余风险**，只是把发现成本从无界降到有界；
  封存分片的**发现延迟反而变长**（要等兜底轮转）。这是用检测延迟换可扩展性的明确取舍。
- **不减少一次性核实的总量**。每个对象仍要被核实至少一次，只是摊销到生命周期里、不重复。
- **不改善单次列举的性能**。那是 MinIO 侧的约束，本设计只是少发请求。
- **不解决备份的时点一致性**。`docs/ops/operations.md:90-98` 把 PostgreSQL 与 MinIO 列为两个独立
  备份目标，未说明恢复到不同时间点会导致索引与对象错位——需单独补运维说明（对账可收敛，但有延迟）。

---

## 6. 待定问题

| # | 问题 | 状态 |
|---|---|---|
| A | ~~`storage_path` 的前缀约定~~ | ✅ **已定（2026-09-09）：不约束**。policy 写整桶，`dest_path_template` 完全自由。授权宽度（管理问题）与清点成本（技术缺陷）分开处理，后者由 §3.5 的分片+封存解决。三种候选约束的否决理由见 §3.2 |
| B | `file_entries` 分区粒度（月 / 周）与归档策略；**同时估算 `object_keys` 主键索引体积**（按真实 `storage_path` 长度算，决定索引能否常驻内存） | 地基阶段（IC-6）前需按真实增速估算 |
| C | 文件列表 `total` 的去 `COUNT(*)` 方案（增量计数表 vs `reltuples` 估算） | 改动 D-007 契约，需单独决策记录 |
| D | ETL 是否允许就地覆盖同一 key（决定是否需要对象版本） | 待产品确认 |
| E | SDK 侧 outbox 的最小实现形态（进程内重试 vs 本地持久化） | 准入阶段（IC-10）时定 |
