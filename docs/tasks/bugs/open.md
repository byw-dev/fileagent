# bugs/open.md — 未解决 BUG

> 本文件结构化描述所有待修复 Bug，Agent 可直接消费。
> 每个 Bug 包含：根因、精确代码位置、修复方案、验收标准。

---

## 总览

**IC-BUG 系列（数据面写入链路，2026-09-08 审计发现；IC-BUG-16…34 为 2026-09-09 起陆续追加：16/17 来自 IC-1 编码期，18/19 是 IC-1 的 live-e2e 中暴露的，20…25 来自 IC-1 的 code review，26…28 来自 IC-SEC-1 的 code review，29 来自 M-1 类扫描，30…32 来自 M-2 类扫描，33/34 来自同日 PR #95 的评审，其中 22/23 随 IC-1 修复、24/25 随 IC-SEC-1 修复）** —— 关联决策 [`DECISIONS.md`](../../../DECISIONS.md) D-030、
设计 [`docs/design/consistency-and-ingest.md`](../../design/consistency-and-ingest.md)。

> ⚠️ **IC-BUG-1…IC-BUG-4 合起来意味着：Agent 数据面从未端到端跑通过。** 单元测试全部 mock 掉了 STS 与 gRPC，
> 因此这些缺陷长期不可见。当前 `file_entries` 的唯一写入者是 MinIO webhook（`/internal/minio-event`），
> 与 `system-design.md` §4.5 描述的主路径完全相反。

### 缺陷模式（2026-09-10 归纳）

34 条里**有 30 条归得进三类成因**（M-1/M-2/M-3）。M-1 与 M-2 的类扫描均已完成（结论见下方两小节），
M-3 待扫。**剩下 4 条不属于任何一类**——IC-BUG-33/34 是「持久化状态缺少终态处理」，IC-BUG-5/11 是
「参数收了不用」，两者实例都太少，暂不立类，但下次归纳时应重新审视。
**逐条等评审撞见是最贵的发现方式**——两次扫描各自挖出了评审没撞见的实例，
且都直接改变了下一刀的边界，这正是「先扫完再发刀」的收益：

| # | 模式 | 已知实例 | 未扫描的面 |
|---|---|---|---|
| **M-1** | **客户端指定资源 ID，无人校验归属**——「这个资源是它的吗」这个问题反复没被问 | IC-BUG-24（dry-run，已修）、IC-BUG-26（`DeleteCollectionRule`）、IC-BUG-27（目录列举）、IC-BUG-29（`UploadResult.rule_id`）；IC-1 修掉的 `rule_id` 越权同源 | ✅ **2026-09-10 已完成类扫描**，见下方「M-1 扫描结论」。REST 与 gRPC 两侧均已过一遍，无其余实例 |
| **M-2** | **协作式机制被当成强制手段**——发个命令就认为对方会照做 | IC-BUG-25（吊销靠 agent 自觉删 token）、**IC-BUG-30**（cancel 无补偿通道）、**IC-BUG-31**（`SendCh` 满即静默丢弃）、**IC-BUG-32**（Revoke 的 Send/Disconnect 竞态）；IC-SEC-1 评审中的 MF-2 / MF-4（以为 cancel 能中断 `Recv`、以为等 `sendErr` 会返回） | ✅ **2026-09-10 已完成类扫描**，见下方「M-2 扫描结论」。`registry.Send` 的 8 个调用点已逐个过完 |
| **M-3** | **鉴权检查不在使用点**——签发时验过，使用时不验 | IC-BUG-22（`PollApproval` 不验指纹）、IC-BUG-23（吊销后 token 仍可用） | 长生命周期凭据的每个消费点；STS 会话本身仍是 ≤1h 不可撤销（已接受） |

> **「新缺陷」与「老缺陷」不是有用的分类。** IC-BUG-22/23 在代码里存在数月，但 IC-1 之前
> agent token 拿到也没用（STS 链路不通），是 IC-1 让它们从死代码里的瑕疵变成「零凭据换
> 整桶写权限」。正确的问法是：**这次改动让原本无害的东西变得可利用了吗**。
> 这也是 `.claude/agents/code-reviewer.md` 要求「越过 diff 边界看」的由来。

#### M-1 扫描结论（2026-09-10）

方法：比对 `db/queries/*.sql` 里同表查询的 `WHERE` 收窄差异（仓库自己暴露了意图——
兄弟查询带 `org_id`/`agent_id` 而某一条只按主键），再逐个回溯调用方是否用了客户端提供的 ID；
gRPC 侧逐个检查 `handleAgentMessage` 的四个分支。

**结论：M-1 在 REST 层只有 1 个实例，比预期少得多；但 gRPC 侧挖出一个新的。**

| 检查面 | 结果 |
|---|---|
| 嵌套路由 `/agents/:id/rules/:rid` | ❌ `DELETE` 未校验父子关系（**IC-BUG-26**）。同路由 `PUT` 是安全的——`UpdateCollectionRule` 带 `AND agent_id AND org_id` |
| 嵌套路由 `/tag-keys/:key/values/:vid` | ✅ 安全，`DeleteTagValue` 带 `AND tag_key_id = $2` |
| gRPC `DryRunResult` | ✅ 已修（IC-SEC-1，收件人绑在 store 上） |
| gRPC `DirectoryListing` | ❌ 拿到 agentID 只打日志（**IC-BUG-27**） |
| gRPC `UploadResult` | ❌ **新发现（IC-BUG-29）** |
| gRPC `Heartbeat` | ✅ 不含资源 ID |

**一个不是缺陷、但该记的架构事实**：单条按 ID 的 `Get`/`Update`/`Delete` **普遍不带 `org_id`**
（`GetEventRuleByID`、`UpdateFileType`、`GetFileTypeByID`、`DeleteBucket`、`DeleteUser` …），
而 `List*` 一律带。所以这不是「DELETE 漏了」，是**单条访问统一不做 org 隔离**。
第一版单组织 + 管理员鉴权下不可利用（`org_id` 本就是预留字段，见 `DECISIONS.md` 多租户条），
但多租户落地时会全线失效。**这是一条架构待办，不是缺陷**——建议在多租户开工前做一次统一收窄，
而不是逐条登记成 bug。

#### M-2 扫描结论（2026-09-10）

方法：以 `registry.Send` 的 8 个调用点为入口（`grep -rn "registry.Send" controlplane/`），对每条
「CP 下发命令」问两个问题——**命令丢了有没有补偿通道**、**命令到了对方会不会照做**；
再逐个回溯 agent 侧是否真有对应的状态变更。

**结论：M-2 有 3 个新实例，其中 2 个直接改变 IC-2a 的边界。**

| 检查面 | 结果 |
|---|---|
| `PushRule`（`DispatchRule`，规则新建/启用） | ✅ **有补偿**——离线时跳过，重连由 `SyncRulesOnConnect` 补推 |
| `CancelRule`（`DispatchRuleCancel`，停用/删除） | ❌ **无补偿通道（IC-BUG-30）**——离线即放弃，而 `SyncRulesOnConnect` 只推 active 规则，从不推 cancel |
| `SyncRulesOnConnect` / `pushCredentials` 的投递容量 | ❌ **静默丢弃（IC-BUG-31）**——`SendCh` cap 32，且这两处都在发送 goroutine 启动**之前**入队 |
| `Revoke` 的 `Send` → `Disconnect` | ❌ **竞态（IC-BUG-32）**——原埋在 IC-BUG-25 的「备注（本刀未处理）」里，本次升格为独立卡片 |
| `TestRule`（dry-run `PushRule`） | ✅ 请求-响应型：30s 超时 + store 记收件人（`dryrun/store.go:40` `Register(reqID, agentID)`），`Send` 失败立即 409 |
| `ListDirectory` | ◐ **M-2 视角安全**（30s 超时 + `Send` 失败立即 409，不存在「以为送到了」），但 **M-1 视角有洞**——`dirstore/store.go:44` 是 `Register(requestID)`，不记收件人，即仍开着的 IC-BUG-27。初版把这格写成「store 记收件人」是错的（评审证伪） |
| `Ping` | ⚠️ **幻影检查面**：8 个 `Send` 调用点里根本没有 Ping。`proto/v1/agent.proto:118,128` 声明了 `PingCommand`，但 **CP 无任何 `ServerMessage_Ping` 构造点**，agent 侧的 `case *agentv1.ServerMessage_Ping` 是死分支。两侧皆死，不构成 M-2 检查面 |
| agent 侧 `rules` 表 | ⚠️ **不是 M-2，但该记**：有 `UpsertRule`（`queue.go:419`）与 `GetRule`（`:436`），但 `GetRule` **只有测试调用**（`queue_test.go:230/245/252`），生产代码无人读——**一张只写不读的死表**。它同时决定了 IC-BUG-30 的爆炸半径止于 agent 进程重启（规则只从 `SyncRulesOnConnect` 来） |

**两条改变 IC-2a 边界的结论**——这正是「先扫完再发刀」的收益，与 M-1 挖出 IC-BUG-29 同理：

1. **IC-BUG-30 让 IC-BUG-29 的「规则不存在」分支变得无界。** `HandleUploadResult` 面对的不是
   「属于我 / 属于别人」两种情况，而是三种：**规则不存在 / 规则属于别的 agent / 规则合法**。
   ⚠️ **归因要点**：这一支**不是 IC-BUG-30 造成的**——它的根在「队列与规则生命周期解耦」这个
   结构事实上（`stopRule` 不碰 `upload_tasks`、`DequeuePending` 无 rule 过滤），因此**即使
   IC-2b 修好 IC-BUG-30，这一支依然存在**。IC-BUG-30 的贡献是把它从「一次排空」放大成
   「持续产生、直到进程重启」。**六条**路径与定案见 IC-BUG-29 卡片。
2. **IC-BUG-31 决定 ack 的可靠性模型。** IC-2a 新增的 `Acknowledgement` 走的是同一个
   best-effort `registry.Send`——丢一个 ack 就有一个任务永久停在 `reported`、outbox 永不清空。
   **修法不是把 `Send` 改成可靠投递**，那恰恰是 M-2 的错误方向；而是让 agent 侧带**重报超时**：
   `reported` 超时回退重发，幂等由 `task_id` + `(bucket_id, storage_path)` + `observed_at` 保证。
   **这一条当前不在 IC-2 的 ③ 里，必须补进 IC-2a。**

---

| ID | 标题 | 严重程度 | 涉及模块 |
|----|------|---------|---------|
| IC-BUG-1 | Agent 永远拿不到 STS 凭据，所有上传直接失败 | 🔴 P0 | controlplane + agent |
| IC-BUG-2 | Agent 从不上报 `UploadResult`，索引主路径是死代码 | 🔴 P0 | agent |
| IC-BUG-3 | STS session policy 前缀与实际对象键不匹配 | 🔴 P0 | controlplane |
| IC-BUG-4 | STS session policy 缺 multipart 权限、多授 DeleteObject | 🔴 P0 | controlplane |
| IC-BUG-5 | 断点续传状态从未落盘，重试永远从头重传 + 孤儿分片累积 | 🟠 P1 | agent |
| IC-BUG-6 | minio-event 索引失败仍返回 200，MinIO 丢弃事件 | 🟠 P1 | controlplane |
| IC-BUG-7 | 通过 API 新建的 bucket 不注册事件通知，文件永不入索引 | 🟠 P1 | controlplane + deploy |
| IC-BUG-8 | `UpsertFileEntry` 无排序键，webhook 会把 agent 富字段覆盖为 NULL | 🟠 P1 | controlplane |
| IC-BUG-9 | webhook `queue_dir` 位于 `/tmp`，MinIO 重启即丢未投递事件 | 🟠 P1 | deploy |
| IC-BUG-10 | `IsProcessed` 忽略 mtime/size，文件修改后永不重传 | 🟡 P2 | agent |
| IC-BUG-11 | tail 模式 `file_offset` / `append_mode` 是死参数 | 🟡 P2 | agent |
| IC-BUG-12 | 上传无超时；重试耗尽后不通知 Control Plane | 🟡 P2 | agent |
| IC-BUG-13 | `content_type` 两条索引路径都不赋值，且会被 upsert 清空 | 🟡 P2 | controlplane |
| IC-BUG-14 | Dashboard `COUNT(*)` / `SUM` 全表扫描（规模隐患） | 🟡 P2 | controlplane |
| IC-BUG-15 | 预签名下载 URL TTL 硬编码 15 分钟，大文件不够用 | 🟡 P2 | controlplane |
| IC-BUG-16 | 模板前导 `/` 使 MT-3 的 path_var 打标对多数规则静默失效 | 🟠 P1 | agent + controlplane |
| IC-BUG-17 | 缓存 token 重启后 `AgentID`/`AgentName` 恒为空，`dest_path_template` 整体失效 | 🔴 P0 | agent |
| IC-BUG-18 | Agent 只能以 TLS 拨号，而 CP gRPC 是明文，本地永远连不上 | 🔴 P0 | agent |
| IC-BUG-19 | minio-event 索引 URL 编码后的对象键（`%2F`），与真实键不符 | 🟠 P1 | controlplane |
| IC-BUG-20 | bucket 集合变化后凭据不补发，新规则最长约 50 分钟持续 403 | 🟠 P1 | controlplane + agent |
| IC-BUG-21 | 模板解析失败时猜一个对象键写进去，污染对账分片 | 🟡 P2 | agent |
| IC-BUG-22 | `PollApproval` 不校验 fingerprint，凭 agent UUID 即可换取 30 天 token | 🔴 P0 | controlplane |
| IC-BUG-23 | 吊销不生效：被吊销 agent 的 token 仍可用，且重连会把状态刷回 online | 🔴 P0 | controlplane |
| IC-BUG-24 | `handleDryRunResult` 无归属校验，可对他人 rule 投递伪造试运行结果 | 🟡 P2 | controlplane |
| IC-BUG-25 | 吊销切不断已建立的流：被吊销 agent 仍可心跳/上报，UI 显示在线且踢不掉 | 🟠 P1 | controlplane |
| IC-BUG-26 | `DeleteCollectionRule` 无归属约束，可删掉别的 agent 的规则 | 🟠 P1 | controlplane |
| IC-BUG-27 | `handleDirectoryListing` 拿到 agentID 却只用于打日志，不校验归属 | 🟡 P2 | controlplane |
| IC-BUG-28 | `registry.Register` 覆盖 map，重连时陈旧流的 defer 会关掉新连接的 SendCh | 🟠 P1 | controlplane |
| IC-BUG-29 | `UploadResult.rule_id` 无归属校验，agent 可把上传记到别人的规则上并借其元数据打标 | 🟠 P1 | controlplane |
| IC-BUG-30 | 规则 cancel 无补偿通道：断连期间删除的规则，agent 重连后继续采集上传 | 🟠 P1 | controlplane + agent |
| IC-BUG-31 | `registry.Send` 队列满即静默丢弃，且 Connect 在消费者启动前入队 | 🟠 P1 | controlplane |
| IC-BUG-32 | `Revoke` 的 `Send` 与 `Disconnect` 存在竞态窗口，命令可能在切流前被丢弃 | 🟡 P2 | controlplane |
| IC-BUG-33 | 失败的上报仍写 `file_entries` 行，制造「DB 有、对象无」的反向幽灵 | 🟠 P1 | controlplane |
| IC-BUG-34 | agent 重启后 `running` 态任务无复位，永久孤儿：不重传也不上报 | 🟠 P1 | agent |

---

## IC-BUG-1 — Agent 永远拿不到 STS 凭据，所有上传直接失败 🔴 P0

| 字段 | 内容 |
|------|------|
| **根因** | 双重断链。(1) **CP 从不推送凭据**：全 controlplane 仅 7 处构造 `agentv1.ServerMessage`（PushRule×3 / CancelRule / Revoke / ListDirectory×2），**没有一处是 `ServerMessage_Credentials`**，与 `system-design.md` §6.6「STS AssumeRole … Agent 连接时」不符。(2) **刷新 RPC 从不发起、发起也会被拒**：Agent 后台 goroutine 的前置判断是 `if sts == nil \|\| 剩余 > 10min { continue }`，而 `sts` 恒为 nil，故 RPC 永不触发；即便触发，Agent 只发送 `AgentId`，而 CP 强制要求 `rule_id` 并对空串做 `uuid.Parse` → `InvalidArgument` |
| **精确位置** | `controlplane/internal/agent/dispatch.go:88,106,142`、`controlplane/internal/api/handler/agents.go:395,460,483,591`（7 处构造点，无 Credentials）；`agent/cmd/agent/main.go:329-331`（nil 短路）、`:223`（唯一接收点）；`agent/internal/grpcclient/client.go:153-155`（只传 AgentId）；`controlplane/internal/grpcserver/handler.go:134-140`（要求 rule_id） |
| **后果** | `currentUploaderCfg` 恒为 nil → `uploadFn` 直接返回 `"agent: no upload credentials available yet"`（`main.go:135-137`）→ **任何文件都不会被上传**。这是 IC-BUG-2 之外、`file_entries` 只由 webhook 写入的第二个根因 |
| **修复** | (1) CP 在 `Connect` 建流成功后（与 `SyncRulesOnConnect` 同一时机）推送一次 `ServerMessage_Credentials`；(2) 对齐 `RefreshCredentialsRequest` 契约——要么 Agent 补传 `rule_id`，要么 CP 允许省略 `rule_id` 并按 Agent 已下发规则的 bucket 集合签发（推荐后者，见 IC-BUG-3）；(3) Agent 首次连接后主动请求一次，不再依赖 `sts != nil` 前置 |
| **验收** | 真实 dev 环境（`deploy/docker-compose.dev.yml`）起 CP + agent，落一个文件到规则的 `base_path`，MinIO 中出现对象且 `file_entries` 有对应行。**不接受仅单测通过** |

## IC-BUG-2 — Agent 从不上报 `UploadResult`，索引主路径是死代码 🔴 P0

| 字段 | 内容 |
|------|------|
| **根因** | `uploadFn` 丢弃 `UploadFile` 的返回值；`UploadFunc` 的签名 `func(ctx, *queue.UploadTask) error` 本身就没有回传结果的通道；全 `agent/` 无一处构造 `AgentMessage_UploadResult` |
| **精确位置** | `agent/cmd/agent/main.go:142`（`_, err = u.UploadFile(...)`）、`agent/internal/executor/executor.go:20`（UploadFunc 签名） |
| **后果** | CP 侧 `grpcserver/handler.go:247` 与 `indexer.HandleUploadResult`（`indexer.go:147`）是死代码，仅测试可达。连锁：`file_entries` 的 `agent_id` / `rule_id` / `sha256` / `original_path` / `file_mtime` **恒为 NULL**；`upload_logs` 恒空；`events.file.uploaded` **从未发布**，所有 `file_uploaded` 事件规则是死配置；D-025 规则声明的 `static_tags` / `path_tag_map` 在真实链路上不触发（只能靠 retag worker 事后补） |
| **修复** | `UploadFunc` 改为返回 `(*uploader.UploadResult, error)`；executor 成功后经回调上报；配合 D-030 的 outbox 语义（见 IC-BUG-12 与设计文档 §4 止血阶段） |
| **验收** | 上传一个文件后，`file_entries` 的 `agent_id`/`rule_id`/`sha256` 非空，`upload_logs` 有对应行，NATS 上能收到 `events.file.uploaded` |

## IC-BUG-3 — STS session policy 前缀与实际对象键不匹配 🔴 P0

| 字段 | 内容 |
|------|------|
| **根因** | CP 签发的 policy 资源固定为 `arn:aws:s3:::{bucket}/agents/{agent_id}/*`，而实际 `storage_path` **完全由规则的 `dest_path_template` 决定**，没有任何机制保证它落在该前缀下（测试夹具里就是 `/{year}/{filename}`、`out/{filename}`） |
| **精确位置** | `controlplane/internal/grpcserver/handler.go:153`；`agent/cmd/agent/main.go:509`（`buildStoragePath`，纯模板展开） |
| **文档冲突** | `system-design.md` §6.2 约定对象键为 `/{prefix}/{yyyy}/{mm}/{dd}/{filename}`，§6.3 的 policy 示例是 `data-sensor/var/2025/*`——**代码自造了一个文档里不存在的 `agents/{id}/` 前缀** |
| **后果** | 即使修好 IC-BUG-1，上传仍会 403 |
| **修复** | **已定（D-030 第八条，2026-09-09）**：policy 资源改为整桶 `arn:aws:s3:::{bucket}/*`，`dest_path_template` 不受任何约束。此前考虑的两个方案（强制模板前缀 / 按静态前缀动态生成）均已否决，理由见 D-030 备选方案 |
| **验收** | 用签发的 STS 凭据直接 `PutObject` 到规则实际生成的 `storage_path`，返回 200（任意模板形状均成立，包括以 `/` 开头与首段为 `{filename}` 的） |

## IC-BUG-4 — STS session policy 缺 multipart 权限、多授 DeleteObject 🔴 P0

| 字段 | 内容 |
|------|------|
| **根因** | 两个同源问题：① Action 列表为 `PutObject / GetObject / DeleteObject / ListBucket`，缺 `s3:AbortMultipartUpload` 与 `s3:ListMultipartUploadParts`；② **Action 层级与 Resource ARN 层级不匹配**——所有 Resource 都构造成对象级 ARN（`arn:aws:s3:::{bucket}/{prefix}/*`），却把桶级 action `s3:ListBucket` 塞进同一个 statement，该条授权从来就是空转的 |
| **精确位置** | `controlplane/internal/storage/policy.go:30-39`（Resource 全为对象级 ARN）、`:51-55`（Action 列表） |
| **文档冲突** | `system-design.md` §6.3 的 policy 明确要求包含 multipart 两个 Action，且**不包含** `s3:DeleteObject` |
| **后果** | >64MB 文件走 multipart：`verifyRemoteParts` 的 ListParts 会 403（续传路径不可用，与 IC-BUG-5 叠加）；失败后无法 Abort，孤儿分片无法清理。IC-BUG-5 验收要用的 `mc ls --incomplete` 需要 `s3:ListBucketMultipartUploads`，同为桶级 action，当前既没列出、列出了也会因 ARN 层级不匹配而空转。另外多授的 `DeleteObject` 让被入侵的 Agent 可删除已归档数据，与最小权限原则不符 |
| **修复** | 拆成两个 statement（前缀部分随 D-030 第八条改为整桶）：**桶级** `s3:ListBucketMultipartUploads` → `arn:aws:s3:::{bucket}`（不带 `/*`）；**对象级** `s3:PutObject` / `s3:AbortMultipartUpload` / `s3:ListMultipartUploadParts` → `arn:aws:s3:::{bucket}/*`。**移除 `DeleteObject`、`GetObject`、`ListBucket`**——agent 全仓库从不调用 `GetObject` / `StatObject` / `ListObjects`（已 grep 确认），产品批准的是「写整桶」，读与列举不在其内，按最小权限一并砍掉 |
| **验收** | 上传一个 >64MB 文件成功；中断后重试能走续传；`mc ls --incomplete` 可执行（不 403）且无残留；用签发的 STS 做 `GetObject` 与 `ListObjects` 均须 403（确认未超授） |

## IC-BUG-5 — 断点续传状态从未落盘，重试永远从头重传 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `multipart.go` 只把 `uploadID` 和已完成分片写进**内存中的** `*queue.UploadTask`；`upload_tasks` 表上仅有 3 条 UPDATE 语句，全部只更新 `status` / `retry_count` / `last_error`，**没有任何语句持久化 `upload_id` 与 `completed_parts`** |
| **精确位置** | `agent/internal/uploader/multipart.go:45,86`（只写内存）；`agent/internal/queue/queue.go:316,330,347`（3 条 UPDATE，均不含这两列） |
| **后果** | 重试时 `DequeuePending` 从 SQLite 重新 scan 出空值 → `multipart.go:28` 的续传分支永不命中 → **每次从 part 1 重传**；旧 uploadID 成为孤儿分片，既无 `AbortMultipartUpload` 调用（全仓库 0 处），MinIO 侧也无 `AbortIncompleteMultipartUpload` 的 ILM 规则（`deploy/scripts/init-minio.sh:70-94` 只对 tmp-uploads 配了 Expiration）→ **空间无限泄漏** |
| **文档冲突** | `system-design.md` §4.5 详细描述了续传流程，CLAUDE.md「关键实现模式」也写着「断点续传状态保存在 SQLite」——设计正确，实现缺失 |
| **修复** | (1) 新增 `Queue.SaveMultipartProgress(id, uploadID, partsJSON)`，每片完成后落盘；(2) 任务终态（completed / 放弃）时调用 `AbortMultipartUpload`；(3) 给数据桶加 `AbortIncompleteMultipartUpload` ILM 规则兜底 |
| **验收** | 上传 >64MB 文件，中途 kill agent，重启后从断点续传（日志可见跳过的分片数）；放弃的任务在 MinIO 侧无残留分片 |

## IC-BUG-6 — minio-event 索引失败仍返回 200，MinIO 丢弃事件 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `IndexUpload` / `IndexDeletion` 出错只 `logger.Warn`，处理器最终无条件 `c.Status(http.StatusOK)`；JSON 解析失败同样返回 200 |
| **精确位置** | `controlplane/internal/api/handler/events.go:803-812`、`:818`、`:779-784` |
| **后果** | MinIO 看到 200 即从 `queue_dir` 删除该事件、永不重投。一次瞬时 DB 抖动 = 永久丢失文件记录，且无任何机制能发现（当前无对账） |
| **修复** | 索引失败返回 5xx 让 MinIO 重投。**⚠️ 不得按 4xx/5xx 分流**——初版写「解析失败返回 400，坏载荷不该无限重投」，**dev 实测证伪**：MinIO 对 400 与 500 一视同仁（都走 `sendSync.func1()` 失败分支，日志 `returned '400 Bad Request'`），配上 IC-4 ③ 的持久 `queue_dir` 后 400 会被无限重投并队头阻塞整条流。统一走「计数 → 未超限 5xx → 超限落死信 + 返 200 放行」。注意保持幂等——重投会重复索引，由 `UNIQUE (bucket_id, storage_path)` upsert 兜住 |
| **⚠️ 5xx 会阻塞整条事件流（2026-09-10 dev 实测）** | MinIO 的 `queue_dir` 是**队头阻塞的单队列**：对前 3 次投递返回 500，实测后续的 delete 与 create **全部排队等待**（约 3s 一次重试），直到那条失败事件成功才按原序一次性放行。**含义**：一个持久失败的事件（如 bucket 行缺失导致 `IndexUpload` 恒错）会让该 target 的**索引 feed 无限期停摆**，止血变断流。**因此 IC-4 ① 必须带毒丸处理**：同一事件重试超过上限 → 落死信（日志/表）→ 返 200 放行队列，而不是无限 5xx。**副作用**（有序性）见 IC-BUG-8 卡片的 `observed_at` 定案 |
| **验收** | 断开 PG 后触发一次 ObjectCreated，端点返回 5xx；恢复 PG 后 MinIO 重投，`file_entries` 出现该行 |

> 📌 本条是**止血修法**。结构性修法是 **D-031**——改用 `notify_nats` + JetStream 后，
> 「是否重投」由 ack 语义决定，而不再依赖「CP 返回什么 HTTP 状态码」这一易错约定。排期在对账阶段（IC-11）。

## IC-BUG-7 — 通过 API 新建的 bucket 不注册事件通知 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `mc event add` 只在初始化脚本里对 `${BUCKET_DATA}` 执行了一次；`BucketsHandler.Create` 只调 `MakeBucket`，不注册通知规则 |
| **精确位置** | `deploy/scripts/init-minio.sh:148-150`；`controlplane/internal/api/handler/events.go`（Create → `minioBucketMaker.MakeBucket`，`controlplane/cmd/server/main.go:57-60`） |
| **后果** | 在当前架构下（webhook 是唯一写入路径），**通过 Web UI 创建的任何 bucket，其文件都永远不会进入索引** |
| **修复** | `MakeBucket` 后调用 `SetBucketNotification` 注册与初始化脚本相同的 ARN 与事件类型；对已存在的 bucket 提供一次性补注册（启动时对 `buckets` 表逐个 ensure，幂等） |
| **验收** | 通过 API 建一个新 bucket，直接 `mc cp` 一个对象进去，`file_entries` 出现该行 |

## IC-BUG-8 — `UpsertFileEntry` 无排序键，webhook 覆盖 agent 富字段 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `ON CONFLICT ... DO UPDATE` 无条件用 `EXCLUDED` 覆盖 `file_type_id / agent_id / rule_id / sha256 / file_mtime / content_type / status`，而 webhook 路径这些值全是 NULL |
| **精确位置** | `controlplane/internal/indexer/queries.go:48-60`（DO UPDATE 子句）；`controlplane/internal/indexer/indexer.go:538-548`（IndexUpload 只填 5 个字段） |
| **后果** | 今天不可见（因 IC-BUG-2，agent 路径是死的）。**一旦修好 IC-BUG-2 就会立刻变成数据损坏**：同一对象的 webhook 事件晚于 agent 上报到达时，会把 agent 写入的 `agent_id` / `rule_id` / `sha256` / `file_mtime` 全部清成 NULL |
| **修复** | 按 D-030：`file_entries` 增加 `observed_at`（排序键）与 `source`；`DO UPDATE` 加守卫（**形态是 `>` OR（`=` AND event_seq 决胜）**，完整 SQL 与 NULL 语义见 `consistency-and-ingest.md` §3.4，**不是单个 `>=`**），富字段一律 `COALESCE(EXCLUDED.x, file_entries.x)`。软删除同样加守卫**并推进 `observed_at`/`event_seq`** |
| **`observed_at` 取值来源（2026-09-10 两次修订后定稿）** | 判据是「**这个值客户端能不能左右**」：`minio_event` ← 载荷的 `eventTime`（**MinIO 生成**）；`agent` / `api` ← **CP 受理时刻**；`audit` ← 扫描时刻。**⚠️ 绝不采信 `UploadResult.uploaded_at`**（agent 提供，报 `2099` 即可永久冻结该行，此后连 IC-13 对账都写不进去）。**⚠️ 也不要因此把 `minio_event` 一并改成 CP 受理时刻**——中间版本曾如此定案，评审用真 PG 证伪：四源统一取受理时刻会让 `>=` 谓词**恒真**、闸门变摆设，而 `IndexUpload` 写的 `size_bytes`/`status`/`uploaded_at`/`etag` 都是非空值、`COALESCE` 一列都保护不到，等于本条根本没修。**完整 SQL 守卫与 NULL 语义见 `consistency-and-ingest.md` §3.4** |
| **⚠️ 软删除是独立 UPDATE，必须单独补守卫** | `MarkFileEntryDeleted`（`indexer/queries.go:209-216`）是 `UPDATE … WHERE bucket_id AND storage_path AND status != 'deleted'`，**无任何时间围栏**，且不是 upsert——只改 `ON CONFLICT` 子句碰不到它。不补则「删除事件滞留重试 → agent 重传同 key → 删除事件重投」会把刚建的活对象标成 `deleted`，而 L3 幽灵清理管的是反方向（PG 有 / MinIO 无），**检不出来** |
| **IC-11 只改善不解决** | 换 JetStream 让重复投递变多（至少一次 + 可重放），排序键因此**更必要**；SQL 侧一行都不能省。**不要用 stream sequence 充当 `observed_at`**（`uint64` vs `TIMESTAMPTZ`，且只对一路单调）——它只喂 `shard_state.last_event_seq` |
| **⚠️ 验收必须能证伪** | 「富字段不被清空」这条**测不出守卫**——富字段由 `COALESCE` 独立保证，实测把守卫整条删掉（= 今天的 master）该用例照样通过。必须测 `COALESCE` **保护不到**的 `size_bytes`/`status`/`uploaded_at`/`etag`：(1) agent 写 `size_bytes=100/etag=E2` → 陈旧 webhook 带 `55/E1` → 仍为 `100/E2`；(2) webhook 先建行 → agent 以更大 `observed_at` 上报 → 富字段落库；(3) 同 `observed_at`、`seq` 更大的 delete 先到、更小的 create 后到 → 结果 `deleted`；(4) 同一事件重复投递 → 调用方拿到成功而非 `ErrNoRows`。**每条都应能在对应变异下失败** |

## IC-BUG-9 — webhook `queue_dir` 位于 `/tmp` 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `queue_dir=/tmp/minio-webhook-queue` 在容器内是易失路径 |
| **精确位置** | `deploy/scripts/init-minio.sh:125`；`docs/design/system-design.md:1799`（同样的示例配置） |
| **后果** | MinIO 容器重启/重建 → 未投递事件全部丢失，无任何补偿。叠加 IC-BUG-6 后，事件丢失有两条独立通道 |
| **⚠️ 实测：脚本值与生效值不符（2026-09-10）** | dev 环境 `mc admin config get myminio notify_webhook:primary` 显示 `queue_dir=`（**空**），与 `init-minio.sh:125` 写的 `/tmp/minio-webhook-queue` 不符。`queue_dir` 为空时 MinIO 走 `sendSync`——**投递失败直接丢弃，连队列都没有**（容器日志可见 `Error: not connected to target server/service`）。因此 IC-4 ③ 的验收必须查**生效值**（`mc admin config get`）而非脚本文本 |
| **IC-11 不解决** | `notify_nats` 同样有 `queue_dir` / `queue_limit`（实测确认）。分诊表初版曾写「IC-11 会连 `queue_dir` 一起删掉、别修」，**已于 2026-09-10 改判**，见 D-031「全量扫描结论」|
| **修复** | 改为持久卷路径；同步更新 §6.5 的示例配置 |
| **验收** | 停 CP → 写入若干对象 → 重启 MinIO 容器 → 启 CP，事件仍被投递、`file_entries` 补齐 |

## IC-BUG-10 — `IsProcessed` 忽略 mtime/size，文件修改后永不重传 🟡 P2

| 字段 | 内容 |
|------|------|
| **根因** | 判重查询只有 `WHERE rule_id=? AND local_path=?`，尽管 `processed_files` 存了 `file_size` / `file_mtime` / `sha256` 且 upsert 已是 `ON CONFLICT DO UPDATE`。`executor.go:196` 的注释写着 "same path+mtime+size"，与实现不符 |
| **精确位置** | `agent/internal/queue/queue.go:404-414`；调用点 `agent/cmd/agent/main.go:479`、`agent/internal/executor/executor.go:197` |
| **后果** | 一个路径首次上传后，内容再修改也不会重新采集（watch 与 cron 两种模式都受影响） |
| **修复** | `IsProcessed` 改为比较 `(rule_id, local_path, file_mtime, file_size)`；同步修正注释 |
| **验收** | 上传一个文件后修改其内容并触发再次采集，MinIO 中对象被更新、`file_entries.size_bytes` 随之变化 |

## IC-BUG-11 — tail 模式 `file_offset` / `append_mode` 是死参数 🟡 P2

| 字段 | 内容 |
|------|------|
| **根因** | `submitFile` 接收 `fileOffset int64, appendMode string` 并由 watcher 传入真实值，但构造 `queue.UploadTask` 时**两个字段都未赋值**；另外 `watcher.tailOffsets` 只存在于内存，从不写入 `upload_tasks.file_offset` |
| **精确位置** | `agent/cmd/agent/main.go:477-495`（参数进、字段不出）、`:440`（调用点）；`agent/internal/watcher/watcher.go:262-264,313-315`（内存 map） |
| **后果** | `uploader.UploadFile` 的 tail 分支（`uploader.go:165-177`）永不触发，追加写入文件每次全量重传；重启后偏移量归零 |
| **修复** | 在 `UploadTask` 字面量中补 `FileOffset` / `AppendMode`；tail 偏移随任务落盘 |
| **验收** | 配一条 `append_mode=tail` 的规则，向文件追加两次，第二次只上传增量 |

## IC-BUG-12 — 上传无超时；重试耗尽后不通知 Control Plane 🟡 P2

| 字段 | 内容 |
|------|------|
| **根因** | `uploadFn` 收到的是 executor worker 的 ctx（Agent 根 ctx），**没有 per-upload timeout**；`handleFailure` 超过 `maxRetries=10` 后只打日志，任务永久停在 `failed` 且不上报 |
| **精确位置** | `agent/cmd/agent/main.go:131-143`；`agent/internal/executor/executor.go:32,241-247` |
| **文档冲突** | `system-design.md` §4.6「超过重试上限的任务标记为 failed，**上报 Control Plane**」——未实现 |
| **后果** | 卡死的连接会永久占用一个 worker（默认只有 3 个）；管理员在 Web UI 上看不到任何失败信号 |
| **修复** | 按文件大小推导 per-upload timeout（可配置下限）；重试耗尽时通过 `UploadResult{success=false, error_message}` 上报（依赖 IC-BUG-2） |
| **⚠️ 拆分（2026-09-10）——归档时不得整条关闭** | **上报半边**（重试耗尽以 `UploadResult{success=false}` 上报）随 **IC-2a ⑥** 关闭，它依赖 IC-BUG-2 的上报通道；**超时半边**（per-upload timeout）留在 **IC-5 ③**，与上报链路无关，属资源保护。**两半都完成前本卡片保持 open** |
| **验收** | 制造一个不可达的 MinIO，任务重试耗尽后 Web UI 的上传日志出现 failed 记录（上报半边）；worker 在超时后释放而非永久占用（超时半边） |

## IC-BUG-13 — `content_type` 两条索引路径都不赋值，且会被清空 🟡 P2

| 字段 | 内容 |
|------|------|
| **根因** | `UpsertFileEntryParams` 有 `ContentType` 字段，但 `HandleUploadResult` 与 `IndexUpload` 都不填充；而 `DO UPDATE` 又无条件写 `content_type = EXCLUDED.content_type` |
| **精确位置** | `controlplane/internal/indexer/indexer.go`（全文无 `ContentType`）；`controlplane/internal/indexer/queries.go:56` |
| **后果** | 该列恒为 NULL；将来任何补齐它的路径都会被另一条路径清空（与 IC-BUG-8 同源） |
| **修复** | 随 IC-BUG-8 的 `COALESCE` 一并修；MinIO 事件载荷含 `contentType` 时填入，agent 侧由 `UploadResult` 带上（需 proto 增字段，只增不改编号） |
| **载荷里本来就有（2026-09-10 实测）** | webhook 与 NATS 载荷均含 `"contentType":"text/plain"`，是 CP 侧 `IndexUpload` 没读它——**填值半边的 webhook 那一路今天就能做，无需任何前置**。与传输无关，IC-11 不改变这一点 |
| **⚠️ 拆分（2026-09-10）——归档时不得整条关闭** | **防清空半边**（`DO UPDATE` 的 `COALESCE`）随 **IC-2a ⑤** 免费带上——它就是 IC-BUG-8 的同一条 SQL；**填值半边**（agent 侧经 `UploadResult` 带 `content_type`、webhook 侧从事件载荷取）**仍开着**，需 proto 增字段，可推到准入阶段之后。**两半都完成前本卡片保持 open** |
| **验收** | 上传一个 `.csv`，`file_entries.content_type` 为 `text/csv`（填值半边），随后到达的 webhook 事件不会清空它（防清空半边） |

## IC-BUG-14 — Dashboard `COUNT(*)` / `SUM` 全表扫描 🟡 P2

| 字段 | 内容 |
|------|------|
| **根因** | 统计端点对 `file_entries` 做 `COUNT(*)` 与 `SUM(size_bytes)`——三条查询均带 `WHERE org_id = $1`，但第一版单组织下该条件不筛掉任何行，**实际等同全表扫描**（多租户落地后仍需索引/物化，不是加个 `org_id` 条件就修好了）；文件列表按 D-007 还要每次返回 `total` |
| **精确位置** | `controlplane/internal/db/read_queries.go:514,517,522` |
| **后果** | 百万行尚可，按 §6.7 推算的量级（高频小文件 50 台 ≈ 2600 万对象/年，3–5 年上亿）会成为主要的 DB 负载来源 |
| **修复** | 见 D-030 地基阶段（IC-7）：增量维护的计数表，或 `pg_class.reltuples` 估算 + 明确标注为估算值。改动 `total` 契约需新开决策记录（D-007 关联） |
| **验收** | 造 1000 万行后 Dashboard 首屏 P95 < 500ms |

## IC-BUG-15 — 预签名下载 URL TTL 硬编码 15 分钟 🟡 P2

| 字段 | 内容 |
|------|------|
| **根因** | `const presignTTL = 15 * time.Minute` 写死在 handler 内 |
| **精确位置** | `controlplane/internal/api/handler/files.go:342`（`DownloadURL`）、`:401`（`BatchDownloadURLs`） |
| **后果** | 违反「禁止 hardcode 配置项」；ETL 拉取 GB 级文件时 15 分钟不足，下载中途 403 |
| **修复** | 提为配置项（默认保持 15min），并在响应的 `expires_in` 中如实返回 |
| **验收** | 改配置后 `expires_in` 随之变化 |

## IC-BUG-16 — 模板前导 `/` 使 path_var 打标对多数规则静默失效 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | Agent 侧 `buildStoragePath` 最后一行 `strings.TrimPrefix(storagePath, "/")` 剥掉了前导斜杠，而 CP 侧 `applyPathVarTags` 用**未经归一化的原始模板**去反解 `storage_path`。模板以 `/` 开头时，字面量 `/` 无法与已剥离的路径匹配 |
| **精确位置** | `agent/cmd/agent/main.go:545`（TrimPrefix）；`controlplane/internal/indexer/indexer.go:409-417`（`trollsift.New(destTemplate)` + `parser.Parse(storagePath)`） |
| **实测** | `tmpl="/{year}/{filename}" path="2026/x.csv"` → `does not match pattern`；去掉模板前导 `/` 或给路径加回 `/` 均可匹配 |
| **后果** | **凡是模板以 `/` 开头的规则，MT-3 的 path_var 打标从未生效过**——`applyPathVarTags` 只 `logger.Warn("storage path does not match template")` 后 return，索引照常成功，缺陷完全静默。两条可验证的事实说明影响面：① **webui 新建规则的默认模板就带前导 `/`**（`webui/src/pages/Agents/RuleForm.tsx:104` = `/{agent_name}/{time:yyyy/MM/dd}/{filename}`），经 UI 创建的规则**全部命中**；② CP 侧 path_var 的 7 个单测夹具（`indexer_test.go`）**无一带前导 `/`**——这正是缺陷从未被测出的原因 |
| **同源** | 与 IC-BUG-3 是同一类病：同一个模板在 agent 与 CP 两端各自解释，没有任何机制保证一致 |
| **修复** | 抽一个模板归一化函数（去前导 `/`，其余规则集中），**agent 拼路径 / CP 反解 / webui 预览三端共用**；放在 `pkg/trollsift` 或其相邻位置，使各端不可能再分叉。**方向必须是「CP 与 webui 剥模板的前导 `/`」，不是「agent 停止剥路径」**——后者会改写所有对象键、需全量重铺。约定写入 [`contracts.md`](../../design/contracts.md) V-3 |
| **验收** | 建一条 `path_tag_map` 非空、模板以 `/` 开头的规则，上传文件后 `file_tags` 中出现 `source='path_var'` 的行；单测覆盖「模板带/不带前导 `/`」两种写法均能反解；webui 预览与实际对象键一致（都不带前导 `/`） |

## IC-BUG-17 — 缓存 token 重启后 `AgentID`/`AgentName` 恒为空，`dest_path_template` 整体失效 🔴 P0

| 字段 | 内容 |
|------|------|
| **根因** | `Lifecycle.Start` 的「已有有效 token」分支直接 `return nil`，**从不给 `l.AgentID` / `l.AgentName` 赋值**——赋值只发生在注册分支。于是 `main.go:271-272` 把两个空串写进 `agentCtx` |
| **精确位置** | `agent/internal/grpcclient/registration.go:122-126`（提前 return）vs `:135-136` / `:145`（唯一赋值处）；消费方 `agent/cmd/agent/main.go:271-272`、`:288`（`SetAgentID`）、`:299`（`Heartbeat.AgentId`） |
| **实测** | `InjectContext` 对空值跳过注入（`pkg/trollsift/context.go:14,19`），于是 `{agent_name}` 缺失 → `Compose(fields, false)` 返回 `missing field "agent_name"` → `buildStoragePath` 走 `main.go:536-541` 的兜底 `return filepath.Base(localPath)` |
| **后果** | **Agent 每次正常重启后，所有文件平铺到桶根、对象键退化为裸 basename，`dest_path_template` 被完全忽略，且全程静默**（Compose 失败不打日志）。跨 Agent 还会互相撞 key。webui 新建规则的默认模板 `/{agent_name}/{time:yyyy/MM/dd}/{filename}`（`webui/src/pages/Agents/RuleForm.tsx:104`）必然命中。gRPC 流本身不受影响——CP 从 JWT claims 取 agent_id（`grpcserver/handler.go:52`），所以故障不会以连接失败的形式暴露 |
| **与 D-030 的关系** | 整桶 policy 让这类「对象键完全跑偏」的写入**不再被 403 挡住**，缺陷会从「上传失败」退化成「静默写错位置」。因此必须与 IC-1 同刀修 |
| **修复** | 缓存 token 分支补齐身份：从 JWT claims 还原 `agent_id` / `agent_name`（token 里已有，CP 侧就是这么取的），或把两者与 token 一起持久化。另外给 `buildStoragePath` 的 Compose 失败路径加 `logger.Warn`——它现在完全静默 |
| **验收** | Agent 首次注册后**重启**，落一个文件：对象键仍符合 `dest_path_template`（不是裸 basename）；心跳的 `agent_id` 非空；日志中无 `missing field` |

## IC-BUG-18 — Agent 只能以 TLS 拨号，而 CP gRPC 是明文，本地永远连不上 🔴 P0

| 字段 | 内容 |
|------|------|
| **根因** | `buildDialOpts` 无条件使用 TLS：`TLSCACert` 为空时退回系统根证书池，仍是 TLS。而 CP 的 `grpc.NewServer` 不配 `Creds`，监听明文。`InsecureDialOpts` 存在但注释写明「intended for testing only」，没有任何配置项能启用它 |
| **精确位置** | `agent/internal/grpcclient/client.go:378-403`（`buildDialOpts`）；`controlplane/internal/grpcserver/server.go:180,202`（`grpc.NewServer` 无 `Creds`） |
| **实测** | 按 `agent/config.toml.example` 原样配置连本地 CP：`transport: authentication handshake failed: tls: first record does not look like a TLS handshake`。**该示例配置文件本身就是不可用的** |
| **后果** | **本地开发环境下 Agent 与 CP 之间不存在任何可用的连接方式**——这正是「Agent 数据面从未端到端跑通过」的直接原因之一：没人跑得起来。所有 Agent 侧缺陷（IC-BUG-1…5、10…12、16、17）因此长期不可见 |
| **修复** | ✅ **已随 IC-1 修**：新增 `server.tls_insecure`（env `AGENT_SERVER_TLS_INSECURE`）。置 `true` 时走 `InsecureDialOpts` 并打印醒目 Warn（bearer token 明文传输）。**刻意用否定式命名**：零值必须是安全的那个，否则在代码里直接构造 `config.Config{}`（不走 `Load`）会静默失去 TLS——初版写成 `tls_enabled bool` 时正是这个 fail-open 缺陷，被既有单测 `TestClient_BuildDialOpts_MissingCACert` 当场逮住 |
| **验收** | ✅ 已验证：`tls_insecure = true` 时 agent 成功连上本地明文 CP 并完成注册→审批→建流；缺省配置仍走 TLS |

## IC-BUG-19 — minio-event 索引 URL 编码后的对象键（`%2F`）🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | S3 事件通知里的 `s3.object.key` 是 **URL 编码**的（`a/b/c.csv` → `a%2Fb%2Fc.csv`），而 CP 直接把它当作 `storage_path` 落库。全 `controlplane/internal/` 无一处 `url.QueryUnescape` / `PathUnescape`（已 grep 确认） |
| **精确位置** | `controlplane/internal/api/handler/events.go:764`（`Key` 字段）、`:800`（`bucket, key := rec.S3.Bucket.Name, rec.S3.Object.Key`）→ 传入 `IndexUpload` |
| **实测** | live-e2e 中 agent 上传 `Miru/tokyo/tokyo_001.csv`，`file_entries.storage_path` 落为 `Miru%2Ftokyo%2Ftokyo_001.csv`；直接 `mc cp` 到 `nested/dir/probe.txt` 同样落为 `nested%2Fdir%2Fprobe.txt` |
| **后果** | 凡是带层级的对象键（正常情况）索引值都与真实键不符。①预签名下载用 `storage_path` 作 key，必然 404；②path_var 反解拿不到分隔符，打标失效；③IC-6 之后 `object_keys` 的幂等键与对账会把这些行全判成幽灵。**当前 `file_entries` 的唯一写入者就是这条路径**，所以影响是全量的 |
| **修复** | 在 `MinioEventHandler.Handle` 解析后对 key 做一次 `url.QueryUnescape`（S3 事件用的是 `+`-as-space 的 query 编码，不是 path 编码），失败时退回原值并告警；补带层级键与含空格/中文键的单测 |
| **归属（2026-09-10 两次改期：IC-4 → IC-2a ⑥ → 独立的 IC-2c）** | **必须早于 IC-BUG-2 的那一刀**，因为二者会**互相制造重复行**。拆成独立一刀的理由是**顺序约束不等于打包约束**——它只需在上报开启之前到位，且能独立 live 验证（`mc cp` 一个带层级的键即可，不依赖 agent）。证据：`migrations/000001_init_schema.up.sql:165` 的 `UNIQUE (bucket_id, storage_path)`、`controlplane/internal/indexer/queries.go:48` 的 `ON CONFLICT (bucket_id, storage_path)`。IC-2 之后 agent 上报写 `a/b/c.csv`、webhook 仍写 `a%2Fb%2Fc.csv`——**两个不同的 `storage_path`，进不了同一个 conflict target**，于是同一个对象变成两行，IC-2a ⑤ 新加的 `observed_at` 排序键**永远不会被触发**。两个写入方在 IC-11（D-031 换传输）之前一直并存 |
| **下游传染** | 更严重的是往下游走：IC-6 的 `object_keys` 窄表用**同一个键形状**，重复会被带进对账的输入——L2 分片扫描会把其中一行判成幽灵、另一行判成真的。**等 IC-6 之后再修就要连带清洗历史行** |
| **⚠️ IC-11（NATS）不解决它——2026-09-10 双向实测** | 曾被问「D-031 换成 NATS JetStream 后是不是就没这问题了」。**不是。** dev 环境对同一个键（`ic19/nested dir/中 文.csv`）同时挂 `notify_nats` 与 `notify_webhook` 两个 target 各抓一次载荷，**两者字节级同构**：<br>顶层字段均为 `['EventName','Key','Records']`；<br>`Records[].s3.object.key`（**CP 实际读的那个**）两边都是 `ic19%2Fnested+dir%2F%E4%B8%AD+%E6%96%87.csv`。<br>编码发生在 MinIO **构造事件对象**时，不在传输层——`%2F` 位于 JSON 字符串字段**内部**，HTTP 与 NATS 都不会改写 JSON 字符串的内容。**因此本条与 IC-11 完全正交，不能等 IC-11 一起解决** |
| **⚠️ 别用顶层 `Key` 字段绕过** | 同次实测发现 MinIO 的事件信封有个**未编码**的顶层 `"Key":"data-sensor/ic19/nested dir/中 文.csv"`，**webhook 与 NATS 都有**（不是 NATS 独有——CP 当前的 `minioEventRecord` 只解析 `Records[]`，所以从没注意到它）。**仍然不要用它**：它是 `bucket/key` 拼接、且属 MinIO 私有信封字段，不在 S3 事件通知规范内。正解是对 `s3.object.key` 做 `QueryUnescape` |
| **✅ 历史脏行不清洗（2026-09-10 定案）** | 现存行已是编码键（live PG 里有 `probe%2Fsts.txt`）。**不写迁移**——系统未发布，等 dev 库重建时自然消失。若将来改变前提（保留 dev 数据作 e2e 基线），需另行清洗，届时再开决策 |
| **⚠️ 归属理由不得省略** | 本条当初被推到 IC-4，正是因为卡片上看不出上面这层交互。**「一行 `url.QueryUnescape` 的事」是它被反复推走的原因，不是它可以被推走的理由**——放错刀就是每个对象两行脏数据 |
| **验收** | `mc cp` 一个 `a/b/中 文.csv`，`file_entries.storage_path` 等于 `a/b/中 文.csv`；预签名下载可直接取回该对象；**新增**：同一对象经 agent 上报与 webhook 两条路径各写一次后，`file_entries` 只有一行 |

## IC-BUG-20 — bucket 集合变化后凭据不补发，新规则最长约 50 分钟持续 403 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | 凭据只在 `Connect` 建流时按当时的 active 规则推一次（`pushCredentials`）。`DispatchRule` 推送新规则时**不带凭据**；agent 侧刷新只看过期时间（`agent/cmd/agent/main.go`：tick 1min，剩余 >10min 即 `continue`）；上传失败路径**没有任何 AccessDenied 触发刷新**的逻辑 |
| **精确位置** | `controlplane/internal/grpcserver/handler.go`（`pushCredentials` 仅 Connect 调用）；`controlplane/internal/agent/dispatch.go`（`DispatchRule` 不推凭据）；`agent/cmd/agent/main.go` 刷新 goroutine；`agent/internal/uploader/`（无 403 分支） |
| **后果** | agent 已连接、持有覆盖 bucket A 的 1h 会话 → 管理员新建一条指向 **bucket B** 的规则 → 规则立即下发、agent 立即开始 PutObject 到 B → **403**，直到会话剩余 10 分钟才刷新，**最坏约 50 分钟**。期间重试耗尽的任务直接失败，不会自愈。IC-1 任务卡 ② 写的是「按该 Agent **已下发规则**涉及的 bucket 集合签发」——该集合是动态的，当前实现只在建流时快照了一次 |
| **修复** | 两条都做：① CP 在规则下发/启用导致 bucket 集合变化时重推凭据（`DispatchRule` 成功后调 `pushCredentials`）；② agent 侧上传遇 `AccessDenied` 时作废当前凭据、立即刷新一次并重试**一次**——第二次仍 403 则以独特错误落终态，不做无限重试（否则会把 policy 前缀错配那类缺陷变成静默循环）。②同时是设计文档 §3.2 要求的通用兜底 |
| **验收** | agent 运行中新建一条指向新 bucket 的规则，首个文件即上传成功（不出现 403）；断开 policy 授权后上传失败两次即落终态并告警 |

## IC-BUG-21 — 模板解析失败时猜一个对象键写进去，污染对账分片 🟡 P2

| 字段 | 内容 |
|------|------|
| **根因** | `buildStoragePath` 在模板无效 / `Compose` 失败 / 结果为空时一律 `return filepath.Base(localPath)`，即把文件平铺到桶根。IC-1 给这三条路径加了 `logger.Warn`，但**行为本身没变** |
| **精确位置** | `agent/cmd/agent/main.go` `buildStoragePath` 的三条兜底 return |
| **后果** | ①写入一个**错误的**对象键比让任务失败更糟：IC-6 之后 `object_keys` 按前缀分片对账，桶根平铺的对象会污染分片树，且这些对象的 path_var 永远反解不出来；②Warn 是 per-file 的——规则模板配错时一次投 5000 个文件就是 5000 条 Warn + 5000 个根目录对象，运维会把它当噪音关掉，等于退回静默 |
| **修复** | 兜底改为**任务失败**（可重试 / 可告警）而不是猜键；Warn 按 `rule_id` 去重（首次记录）或采样。归 **IC-2b**（agent 侧鲁棒性一刀，与 IC-BUG-20 同刀）——它改的是任务终态语义，与上报通道本身无关 |
| **验收** | 模板解析不出来时任务进入失败态并可在 UI 看到原因；同一规则连续 N 个文件失败只产生一条 Warn |

## IC-BUG-22 — `PollApproval` 不校验 fingerprint，凭 agent UUID 即可换取 30 天 token 🔴 P0

| 字段 | 内容 |
|------|------|
| **根因** | `PollApprovalRequest` 有 `fingerprint` 字段（`proto/v1/agent.proto:204`），但 `Manager.PollApproval` **从头到尾没读过它**——只 parse agent_id、查库、若 `status == approved` 就签发新 token。而 `PollApproval` 在 `jwtExemptMethods` 里（`interceptor.go:32`），**不需要任何认证** |
| **精确位置** | `controlplane/internal/agent/manager.go` `PollApproval`；`controlplane/internal/grpcserver/interceptor.go:31-36` |
| **实测** | dev 环境 grpcurl 用 `"fingerprint":"totally-wrong-fingerprint"` 直接换到 `authToken`，再用它调 `RefreshCredentials` 拿到 `data-sensor` 整桶写的 STS 会话。**攻击者除一个 agent UUID 外什么都不需要** |
| **后果** | agent UUID 不是秘密——它出现在对象键、日志、NATS 事件、webui 响应里；`Register`（同样免认证）对已存在 fingerprint 还会回吐 agent_id。触发条件是 agent 状态恰为 `approved`（已审批、尚未首次连接），这是每个新 agent 的必经状态，窗口长度由现场决定。**IC-1 之前拿到 agent JWT 基本没用（STS 链路不通），之后它直接等于数据湖整桶写** |
| **修复** | ✅ **已随 IC-1 修**：`PollApproval` 比对 `agent.Fingerprint`，空或不匹配返回 `PermissionDenied`。校验放在状态检查**之前**，避免向未认证调用方泄露 agent 状态 |
| **验收** | ✅ 单测覆盖（错误指纹 / 空指纹均拒绝，正确指纹签发）；变异测试确认去掉校验后用例失败 |

## IC-BUG-23 — 吊销不生效：token 仍可用，且重连会把状态刷回 online 🔴 P0

| 字段 | 内容 |
|------|------|
| **根因** | 三处叠加：① `RevokeAgent` 只置 DB 状态 + 删 Redis 键 + 发事件，**从不调 `jwtSvc.RevokeToken`**；② `Connect` / `RefreshCredentials` / `pushCredentials` **都不查 agent 的 DB 状态**，拦截器只验签 + 黑名单；③ `Connect` 用**无条件**的 `UpdateAgentStatus(online)`，而 `MarkAgentOnlineIfOffline` 的 SQL 注释明写「must never resurrect terminal states such as 'revoked'」——`Connect` 恰恰在做这件事 |
| **精确位置** | `controlplane/internal/agent/manager.go` `RevokeAgent`；`controlplane/internal/grpcserver/handler.go` `Connect` / `RefreshCredentials`；`controlplane/internal/db/queries/agents.sql:60-70`（那条被绕过的约束） |
| **实测** | 把已连接 agent 在库里置为 `revoked` 后调 `RefreshCredentials{}`，**照常返回 STS 凭据**；重连后状态被刷回 `online`，UI 上看不出曾被吊销 |
| **后果** | `AGENT_TOKEN_TTL` 默认 **720h = 30 天**，吊销一个**被入侵的** agent 完全依赖它自愿执行 `handleRevokeCommand` 删本地 token——而被入侵的 agent 正是不会照做的那个。D-030 第八条「授权宽度是管理权限问题」所依赖的管理手段本身失效 |
| **修复** | ✅ **已随 IC-1 修**：新增 `Server.assertAgentUsable`，`Connect` 与 `RefreshCredentials` 入口查一次 `agents.status`，仅 `approved/online/offline` 放行；查不到 agent 行一律拒绝（fail-closed）。并把 `Connect` 的 online 写入换成条件 SQL `MarkAgentOnlineIfUsable`（`WHERE status IN ('approved','offline','online')`）——**不变式钉在 SQL 里而不是靠「同一函数里更早的一行」**，否则吊销恰好落在闸门与写入之间就会被这条无条件 UPDATE 撤销、此后闸门永久放行。**未做**按 jti 吊销 JWT——CP 只存 token 的 sha256、无法还原 jti，真要做需引入「按 agent 维度的令牌版本号」，成本远高于状态闸门 |
| **验收** | ✅ 单测覆盖（revoked 拒绝 + 三种可用状态放行 + 查库失败拒绝）；变异测试确认去掉闸门后用例失败 |

## IC-BUG-24 — `handleDryRunResult` 无归属校验 🟡 P2

| 字段 | 内容 |
|------|------|
| **根因** | `handleAgentMessage` 里 `DryRunResult` 是**唯一不传 agentID** 的分支，按 body 里的 `rule_id` 投递进共享的 `dryRunStore` |
| **精确位置** | `controlplane/internal/grpcserver/handler.go` `handleAgentMessage` 的 `AgentMessage_DryRunResult` 分支 |
| **后果** | 任一已连接 agent 可对**别人的 `rule_id`** 投递伪造的试运行结果，管理员在 UI 上看到的预览是伪造的。只读、影响面小，但与 IC-BUG-22/23 是同一个模式：**凡是客户端指定资源 ID 的接口，都要问一句「这个资源是它的吗」** |
| **根因修正** | 初版把 `rule_id` 当成 `collection_rules` 的主键去查库——**错了**。它是 CP 为单次试运行 `uuid.New()` 生成的**临时关联 ID**（`agents.go:599`），从不入库，agent 只是原样回显。按 rule 查会让**每一次合法试运行都 fail-closed 被丢弃**，REST 30s 后返回 504，等于打死「试运行」功能 |
| **威胁模型修正** | 该 ID 是 CP 生成的不可猜随机值（能力型），攻击者需在 30s 窗口内猜中一个 UUIDv4，并非「任一 agent 都能对别人的 rule_id 投递」。真正的不变量是**「这个关联 ID 是不是发给你的」** |
| **修复** | ✅ **已修（IC-SEC-1，PR #94）**：归属绑在 store 上——`dryrun.Store.Register(reqID, agentID)` 记录收件人，`Deliver(reqID, agentID, result)` 比对后才投递并返回是否接受。零 DB 查询，且校验的是真正的不变量 |
| **验收** | ✅ agent A 对发给 B 的关联 ID 投递被丢弃并告警；**合法试运行仍能送达**（这条是初版缺的关键回归）；经 `handleAgentMessage` 的用例断言闸门拿到的是流上的 agentID |

## IC-BUG-25 — 吊销切不断已建立的流 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `assertAgentUsable` 只在**建流时**跑一次（IC-1 加的闸门）。已建立的流不受任何约束：`AgentConn.CancelFunc` **全仓库无人调用**（唯一的 `Unregister` 是 `Connect` 自己的 defer），REST 的 `Revoke` 只发一条**协作式**命令；`handleAgentMessage` 的四个分支都没有闸门 |
| **精确位置** | `controlplane/internal/grpcserver/registry.go`（`CancelFunc` 无调用点）；`controlplane/internal/grpcserver/handler.go` `handleAgentMessage`；`controlplane/internal/agent/manager.go` `RevokeAgent` |
| **后果** | 被入侵的 agent 已连接 → 管理员吊销 → 它忽略 `Revoke` 命令、不断开。于是：①继续心跳刷新 `agent:online:<id>` 与 `last_seen_at`，**UI 上这个已吊销的 agent 一直显示在线，管理员没有任何手段把它踢下线**；②继续上报 `UploadResult`（IC-2a 之后就是攻击者可控地直接写 `file_entries`/`upload_logs`）；③继续响应 `ListDirectory` |
| **残留窗口（已接受）** | 手里已签发的 STS 会话在 ≤1h 内仍是整桶写。STS 会话本质上不可撤销（除非轮转 MinIO 父用户或加 deny policy），这一条**接受**，但必须在运维文档里写明「吊销不是即时的，最长一个 STS TTL」 |
| **根因修正** | 初版只加了 `Disconnect`（取消 `stream.Context()` 的子 context）就宣称「切断流」——**不成立**。接收循环阻塞在 `stream.Recv()`，它不观察那个 context；handler 永不返回 → defer `Unregister` 永不执行 → **goroutine 与 registry 条目永久泄漏**，`IsOnline` 恒为 true |
| **修复** | ✅ **已修（IC-SEC-1，PR #94）**：① registry 加 `Disconnect`，只 cancel、**不** `close(SendCh)`（所有权在 Connect 的 defer 上，重复关闭会 panic，变异测试实证）；② **`Connect` 的接收循环重构**——`Recv` 移入 goroutine 喂 channel，主循环 `select` 同时等 `ctx.Done()`，取消时 handler 真正 `return`（只有返回才终止 RPC，随后阻塞中的 Recv 出错退出，不泄漏）。**`ctx.Done()` 分支刻意不等发送 goroutine**：它只在空闲时观察 ctx，一旦停在 `stream.Send` 里就再也不写 `sendErr`，而 `Send` 阻塞与否由客户端读不读决定——等它会让 handler 以完全相同的形态泄漏（复审 MF-4，已用「客户端不读流」的用例钉住）；**`Recv` 出错分支同样不能等**——agent 可以「灌满流控窗口 + 半关闭」把 handler 钉在该分支之外，此后它根本观察不到 `ctx.Done()`，即 **agent 能把自己变成踢不掉的**（该行 master 就有，随本刀一并删掉，并用半关闭场景的用例钉住）；③ `Revoke` 发完协作式命令后无条件切流 |
| **备注（本刀未处理）** | `Revoke` 里先 `Send(RevokeCommand)` 再立刻 `Disconnect`，二者存在竞态：发送 goroutine 的 `select` 在 `ctx.Done()` 与 `SendCh` 同时就绪时随机取分支，协作式命令可能被丢弃，agent 因此不清理本地 token。影响很小（命令本就尽力而为，切流才是硬手段）。**已于 2026-09-10 的 M-2 类扫描升格为独立卡片 IC-BUG-32**，原「随 IC-2 顺手处理」的处置作废——一个已知实例埋在别的卡片注释里而没被当成一类去扫，正是 M-2 长期未被扫描的原因 |
| **验收** | ✅ bufconn 端到端用例：`Disconnect` 后客户端 `Recv` 返回 `PermissionDenied`（非阻塞），registry 条目被清空（证明 handler 已返回、defer 已跑）。变异验证：退回旧的阻塞 `Recv` 结构后该用例在 5s 超时处失败。**残留**：UI 在线状态靠 Redis 90s TTL 过期而非立即，`handleDirectoryListing` 不带 ctx——见下方备注 |

## IC-BUG-26 — `DeleteCollectionRule` 无归属约束 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `DELETE FROM collection_rules WHERE id = $1`（`db/queries/rules.sql:58`）——**只按主键**。同一文件的 `UpdateCollectionRule`（`:54`）却是 `WHERE id = $1 AND agent_id = $14 AND org_id = $15`，说明这条约束是有意的，只是 DELETE 漏了 |
| **精确位置** | `controlplane/internal/db/queries/rules.sql:57-58`；调用方 `controlplane/internal/api/handler/agents.go`（`DeleteRule`） |
| **后果** | `DELETE /api/v1/agents/<A>/rules/<属于 B 的 rule_id>` 会删掉 **B 的**规则，然后把 cancel 命令发给 A——B 那边的采集静默停止，且路径里的 agent id 与实际受影响的 agent 不符，事后排查会指向错误的 agent。当前是单组织 + 管理员鉴权，所以是**越权面**而非直接可利用漏洞，但与 IC-BUG-24 是同一模式 |
| **修复** | DELETE 加 `AND agent_id = $2 AND org_id = $3`，`:execrows` 返回 0 时 handler 返回 404，与 Update 对齐；经 `make generate` |
| **归属（2026-09-10）** | 从 IC-2 ⑧ 移到 **IC-SEC-2**（与 IC-BUG-27/32 同刀；IC-BUG-28 已改判移入 IC-2a ⑧）。**不挡数据面可用**：单组织 + 管理员鉴权下是越权面而非可利用漏洞，不该混进 IC-2a 的上报链路 review |
| **验收** | 用 agent A 的路径删 B 的 rule 返回 404 且 B 的规则仍在；删自己的规则仍正常 |

## IC-BUG-27 — `handleDirectoryListing` 不校验归属 🟡 P2

| 字段 | 内容 |
|------|------|
| **根因** | 该分支**拿到了** `agentID`，但只用于打日志，投递仍只按 body 里的 `request_id`（`grpcserver/handler.go` `handleDirectoryListing`）。IC-BUG-24 的卡片称 DryRunResult 是「唯一不传 agentID 的分支」——字面成立，但「传了不校验」是同一个洞 |
| **精确位置** | `controlplane/internal/grpcserver/handler.go` `handleDirectoryListing`；`controlplane/internal/dirstore/store.go` |
| **后果** | 任一已连接 agent 可对别人的 `request_id` 投递伪造的目录列表，管理员看到的是伪造内容。与 IC-BUG-24 同为「能力型随机 ID」，需在 30s 窗口内猜中 UUIDv4，实际可利用性低 |
| **修复** | 与 IC-BUG-24 同解：`dirstore.Store.Register(reqID, agentID)` + `Deliver(reqID, agentID, result)` 比对收件人。IC-SEC-1 已经把 `dryrun.Store` 改成这个形状，照抄即可 |
| **归属（2026-09-10）** | 从 IC-2 ⑨ 移到 **IC-SEC-2**（与 IC-BUG-26/32 同刀；IC-BUG-28 已改判移入 IC-2a ⑧）|
| **验收** | agent A 对发给 B 的 request_id 投递被丢弃并告警；合法目录列举仍能送达 |

## IC-BUG-28 — `registry.Register` 覆盖 map，重连时陈旧流会关掉新连接 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `Register` 无条件 `r.conns[agentID] = conn`（`grpcserver/registry.go`）。同一 agent 重连时新条目覆盖旧条目，而**陈旧流最终返回时它的 defer `Unregister(agentID)` 关的是新连接的 `SendCh`** 并把新连接从 registry 删除 |
| **精确位置** | `controlplane/internal/grpcserver/registry.go` `Register` / `Unregister` |
| **实测（评审）** | `fresh conn SendCh closed by the STALE unregister: closed=true`、`IsOnline=false`，随后再 `Send` → `panic: send on closed channel` |
| **后果** | 网络抖动导致的重连即可触发：新连接被静默踢出 registry 且 `SendCh` 被关闭，此后规则下发/凭据推送全部失败；`Send` 在 RLock 之外发送、与 `Unregister` 的 `close` 并发还有同一崩溃窗口，而 `SyncRulesOnConnect` 路径上的 panic 在 gRPC handler goroutine 里**没有 recover** |
| **⚠️ 归属（2026-09-10 评审后修正：IC-SEC-2 → IC-2a ⑧）** | 初版判它「不挡数据面可用、单独成刀」——**错判**。理由没有回应一个事实：**IC-2a 把 `registry.Send` 从「每条连接几次」变成「每个文件一次」**（② 每处理完一次上报就回发 `Acknowledgement`）。三个已核实的条件叠起来是进程级崩溃：<br>① `Send` 在 **RLock 之外**写 channel（`registry.go:96-108`），`Unregister` 在写锁内 `close`（`:54-61`）——窗口真实存在；<br>② `Register` 按 key 覆盖 map（`:47-49`），陈旧流的 `defer Unregister`（`handler.go:66`）关掉的是**新**连接的 `SendCh`；<br>③ **CP 的 gRPC server 没有任何 recovery interceptor**（已 grep 全仓库确认：REST 侧有 `router.go:68 gin.Recovery()`，gRPC 侧没有）。<br>**失败场景**：网络抖动 → agent 重连 → 陈旧 handler 返回并关掉新连接的 `SendCh` → 新 handler 正为刚上传的文件发 ack（`handleUploadResult` 就在 Connect 的 handler goroutine 里）→ `panic: send on closed channel` → **整个 Control Plane 进程崩溃**。**上传越密集命中概率越高，而 IC-2a 的全部意义就是让上传变密集**——完全符合本 track 的同刀判据「这次改动让原本无害的东西变得有害了吗」，比 IC-BUG-29 更符合 |
| **⚠️ 前置：对着当前 master 验，不要照旧假设写** | IC-BUG-25 **已随 IC-SEC-1 合并**，评审当时说的「两件事宜一起处理」已经过时。当前 master 的事实是：**`Disconnect` 只 cancel、不 `close(SendCh)`**，`SendCh` 的所有权在 `Connect` 的 `defer Unregister` 上（`registry.go` 的 `Disconnect` 注释已写明这一点）。改 `Unregister` 时若顺手在 `Disconnect` 里也 close，就是重复关闭 panic——IC-SEC-1 的变异测试实证过 |
| **修复** | `Unregister` 改为按 conn 身份而非按 key 删除（比对指针/世代号，只在仍是自己那条时才 close + delete）；`Send` 改为在锁内取 conn 后用 `select` + 关闭标志，或改用 per-conn 的关闭同步 |
| **验收** | 同一 agent 快速重连后，旧流返回不影响新连接：`IsOnline` 仍为 true、`SendCh` 未关闭、后续 Send 成功；`-race` 下并发 Send/Unregister 无 panic |

## IC-BUG-29 — `UploadResult.rule_id` 无归属校验 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `HandleUploadResult` 直接 `uuid.Parse(result.GetRuleId())` 就用（`indexer.go:154-160`），**全程不校验该规则是否属于上报的 agent**——该文件里 `AgentID` 只出现在写入参数中（`:197`、`:233`），从未参与校验 |
| **精确位置** | `controlplane/internal/indexer/indexer.go:154-160`（取用）、`:167`（`loadRuleMetadata(ctx, orgID, ruleID)`）、`:198`/`:235`/`:495`（写入 `file_entries.rule_id` / `upload_logs` / `SourceRuleID`） |
| **后果** | 恶意或有缺陷的 agent 可以：①**把自己的上传记到别人的规则名下**，污染 `file_entries.rule_id` 与 `upload_logs` 的归因，事后排查会指向错误的采集器；②更糟的是 `loadRuleMetadata` 按这个 ID 加载 `file_type` / `static_tags` / `path_tag_map`——于是它能**借用任意规则的元数据声明给自己的文件打标**，直接污染 6c 受控标签体系（这些标签会进筛选器、进事件规则、进下游 ETL） |
| **可利用性** | **当前为零**——agent 从不上报 `UploadResult`（IC-BUG-2），这条路径是死代码。**IC-2a 把它变成索引主路径的那一刻起就成立**，与 IC-1 让 IC-BUG-22/23 变得可利用是同一个机制 |
| **修复** | 在 `HandleUploadResult` 里校验归属（`agentID` 已经是流上的可信身份，函数签名里就有）。**分支是三个，不是两个**——见下行 |
| **⚠️ 三分支** | 只写 `rule.AgentID == agentID` 不够。实际要处理 ①**规则不存在** ②**规则属于别的 agent** ③**合法**，且**不能让 `loadRuleMetadata` 查不到就默默走空 metadata**——那等于把 ① 静默当成「无规则来源」，正是这条缺陷的静默版本 |
| **① 的来源：队列与规则生命周期解耦（结构性，非缺陷）** | 三个已确认的事实：`stopRule` 只做 `sched.RemoveRule` + cancel ruleCtx（`agent/cmd/agent/main.go:159-167`），**从不触碰 `upload_tasks`**；`DequeuePending` 只按 `WHERE status = ?` 取（`queue.go:293`），**无 rule 过滤**；全仓库无「按 rule 删任务」语句（唯一的 `DELETE FROM upload_tasks` 是容量淘汰 `queue.go:277`）。**任务一旦入队，规则怎么变都不影响它被上传和上报**。于是有六条路径：<br>1. **队列滞留**——任务已入队、`CancelRule` 正常送达并停掉 watcher/cron，但队列照常排空。窗口 = 排空时间；叠加重试（`maxRetries=10`，退避 1/5/15/60min 封顶）可达数小时，大文件再叠加 IC-BUG-5 更久<br>2. **离线积压**——离线期间持续写本地队列（设计行为），期间规则被删，重连补传整批。窗口 = 断线时长<br>3. **IC-BUG-30**——断连期间删规则，重连后 agent 不知道规则没了，**持续产生新任务**。无界，直到进程重启<br>4. **重启残留**——SQLite 队列跨重启存活，规则却只从 `SyncRulesOnConnect` 来<br>5. **恶意/有缺陷的 agent** 伪造 rule_id（本卡片要防的那条）<br>6. **IC-BUG-26 自己制造的那条（评审补，最难堪的一条）**——`DeleteCollectionRule` 只按 `id` 删（`db/queries/rules.sql:58`），而 `DeleteRule` handler 把 cancel 发给 **URL 里的 agent**（`api/handler/agents.go:1026` 取 `agentID := c.Param("id")`，`:1041` 用它发 cancel）。于是 `DELETE /api/v1/agents/<A>/rules/<属于 B 的 rule>` 会删掉 B 的规则、把 cancel 发给 A——**B 全程在线、连接正常、没有任何断连窗口**，却永远收不到 cancel。这是**唯一一条在全在线稳态下无界产生**的路径<br>**只有第 3 条能被 IC-2b 消掉；第 6 条 IC-SEC-2 的 IC-BUG-26 只能消掉一半**（另一半是收件人错了，需要 `DeleteRule` 先从 DB 读回真实 `agent_id` 再发 cancel）。**1/2/4 是持久化队列 + 可变规则集的必然结果**——换句话说，**「规则不存在」是稳态下的正常情形，不是异常**：管理员每删一次规则，只要名下还有在途任务就会产生一批 |
| **✅ 定案（2026-09-10）：宽松——清空 `rule_id`，文件照常入索引** | **严格（整条拒绝）会让「删除一条规则」变成「静默丢弃若干已在 MinIO 里的文件的索引行」**。对象已经写进去了，拒绝入索引只是制造一批要等 IC-13 对账才发现的幽灵，而 L2 那时只能补回存在性、补不回 tags/sha256。一个日常管理动作不该有这种后果 |
| **⚠️ 告警分级** | ① 与 ② 的告警等级**必须分开**：① 是路径 1/2/4 的正常产物，per-file 告警就是 IC-BUG-21 里「5000 条 Warn 被运维关掉」的翻版，应按 `rule_id` 去重或降为 Info；**② 永远不合法，是唯一值得响的那一支** |
| **⚠️ 宽松的代价，须可见** | 走 ① 分支的文件拿不到 `loadRuleMetadata` 的规则声明——**永久丢失的是「规则声明的 `file_type` 覆盖」+ `static_tags` + `path_var` 标签**，规则已删，retag worker 也没有可回溯的声明。**注意不是「无类型」**：`indexer.go:333` 仍会走 glob `classifier.Classify` 兜底，文件类型按后缀正常判定（评审证伪，初版措辞过重）。这是接受的代价，但要让它可查（在 `source` 之外记一个「元数据缺失」标记），而不是当正常行写完了事 |
| **验收** | agent A 上报携带 B 的 rule_id → `file_entries.rule_id` 不被写成 B 的规则、`file_tags` 里不出现 B 规则声明的标签、日志有告警；A 用自己的 rule_id 上报一切正常 |


## IC-BUG-30 — 规则 cancel 无补偿通道，断连期间删掉的规则 agent 继续跑 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | 同步协议**只有增量推送，没有全集语义**。`DispatchRuleCancel` 在 agent 离线时直接 `return nil`（`dispatch.go:101-103`），而重连时的 `SyncRulesOnConnect` **只推 `status == active` 的规则**（`dispatch.go:130`），从不推 cancel。agent 侧 `stopRule` 有两个调用点——`applyRule` 开头（`agent/cmd/agent/main.go:170`）与 `CancelRule` 分支（`:238`），**没有一个来自「同步全集」**，没有「本次同步的全集之外的规则一律停掉」这条语义 |
| **精确位置** | `controlplane/internal/agent/dispatch.go:101-115`（离线即放弃）、`:129-132`（只推 active）；`agent/cmd/agent/main.go:159-167`（`stopRule`）、`:169-209`（`applyRule`） |
| **后果** | agent 断连期间（网络抖动 / CP 重启）管理员停用或删除一条规则 → cancel 命令被丢弃 → agent 重连后**继续按这条已经不存在的规则采集并上传**，且因 D-030 整桶 policy **传得上去**（不会被 403 挡）。DB 与 UI 上该规则已消失，运维没有任何线索。持续到 agent 进程重启为止——`rules` 表只写不读，重启后规则只从 `SyncRulesOnConnect` 来 |
| **与 IC-2a 的关系** | 上报活过来之后，这些上传会带着一个**已删除的 `rule_id`** 到达 `HandleUploadResult`，因此 IC-BUG-29 的归属校验必须处理三分支。**但注意归因**：那一支的根是「队列与规则生命周期解耦」（见 IC-BUG-29 卡片的五条路径），本条只是把它从「一次排空」放大成「无界产生」。**修好本条不会消掉那一支**，IC-2a ⑦ 仍必须独立处理 |
| **修复** | 两半，缺一不可：① **停用**：`SyncRulesOnConnect` 连 inactive 一起推——agent 的 `applyRule` 对 `Enabled == false` 已经会先 `stopRule` 再 return，复用既有分支即可；② **删除**：删掉的行已不在 `ListCollectionRulesByAgent` 的结果里，**推不出来**，必须给同步加全集语义（下发一条「本次同步的 `rule_id` 全集」，agent 停掉集合外的规则）或 CP 侧留 tombstone。推荐全集语义——proto 只增字段，不必引入软删除表 |
| **验收** | agent 与 CP 断连 → 删除一条规则、停用另一条 → 恢复连接 → 两条都不再产生上传，且 **agent 进程不重启**也成立；单测覆盖「全集里缺失的规则被停掉」 |

## IC-BUG-31 — `registry.Send` 队列满即静默丢弃，且 Connect 在消费者启动前入队 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `AgentRegistry.Send` 是 `select { case conn.SendCh <- msg: ...; default: return false }`——**队满即丢**，`SendCh` cap 32（`registry.go:43`）。8 个调用点对 `false` 的处理清一色是 `logger.Warn` 后继续，无一重试或落 outbox；`DispatchRule` 更是在 `Send` 失败时仍 `return nil`，调用方连判断的机会都没有。叠加一个顺序问题：`Connect` 里 `SyncRulesOnConnect`（`handler.go:105`）与 `pushCredentials`（`:110`）都在**发送 goroutine 启动之前**（`:113`）入队，此时通道**没有任何消费者** |
| **精确位置** | `controlplane/internal/grpcserver/registry.go` `Send` 的 `default:` 分支、`:43`（`make(chan *agentv1.ServerMessage, 32)`）；`controlplane/internal/grpcserver/handler.go:105`（Sync）、`:111`（pushCredentials）、`:115`（发送 goroutine 才启动）；`controlplane/internal/agent/dispatch.go:94-97`（Send 失败仍 `return nil`） |
| **实测（静态推导）** | **恰好 32 条** active 规则时：规则全部进队，但紧随其后的 `pushCredentials` **被丢弃**；**≥33 条**时规则本身也开始丢（第 33 条起）|
| **后果** | ① **规则丢失是永久的**——agent 只在建流时拿规则，下次重连会以完全相同的方式再丢一次；② 凭据丢失可自愈，窗口 ≤1 分钟（agent 侧 1min tick 的刷新 goroutine，IC-1 ⑤ 已去掉 `sts == nil` 短路）；③ **IC-2a 引入 `Acknowledgement` 后，丢一个 ack 就有一个任务永久停在 `reported`、outbox 永不清空** |
| **修复** | 分两层：① **结构**——`Connect` 里把发送 goroutine 提到 `SyncRulesOnConnect` / `pushCredentials` **之前**启动（顺序调整，消除「无消费者时入队」）；② **语义**——接受 `Send` 仍是 best-effort，但每个消费方自带补偿：规则靠 IC-BUG-30 的全集同步，ack 靠 IC-2a 的重报超时。**不要试图把 `Send` 改成可靠投递**——那正是 M-2 的错误方向（把协作式机制当成强制手段）；可靠性应当由接收方的重试提供，而不是由发送方的保证提供 |
| **验收** | 给一个 agent 配 40 条 active 规则，重连后 40 条全部生效且凭据到达；单测覆盖「SendCh 满时调用方可观测到失败」（`DispatchRule` 不再吞掉） |

## IC-BUG-32 — `Revoke` 的 `Send` 与 `Disconnect` 存在竞态窗口，命令可能在切流前被丢弃 🟡 P2

| 字段 | 内容 |
|------|------|
| **根因** | `Revoke` 先 `registry.Send(RevokeCommand)` 入队，紧接着调 `registry.Disconnect` cancel 掉 context，二者之间没有任何同步 |
| **⚠️ 量级修正（2026-09-10 评审实测）** | 初版写「约半数丢失」——**证伪**。复刻 `handler.go:115-132` 的 select 结构 + `agents.go:401/410` 的调用序实测：发送 goroutine **已 park 在 select 上**（`SendCh` 空）时，channel 直接交接，`SendCh` 分支在 `Send` 返回前就已提交，随后的 `cancel()` 追不上——**20000 次 100% 送达**；只有 goroutine **不在 select** 时（正卡在前一条消息的 `stream.Send` 里，或尚未启动）两分支才同时就绪，此时 49.6%。**稳态下的空闲连接正是前者**，所以「约半数」不成立 |
| **真正的主窗口** | 不是 select 的随机选择，而是：`ctx.Done()` 让 `Connect` 的主循环 `return`、RPC 就此结束，**后续的 `stream.Send` 必然失败**。即命令是否送出取决于「发送 goroutine 有没有抢在 handler 返回之前把它写进流」——这是个时间赛跑，不是 50/50 的掷硬币 |
| **精确位置** | `controlplane/internal/api/handler/agents.go:399-413`（`Send` 后立即 `Disconnect`）；`controlplane/internal/grpcserver/handler.go:114-129`（发送 goroutine 的 select） |
| **后果** | 有限。切流是硬手段且已经生效（IC-SEC-1），协作式命令本就尽力而为。真实损失是**守规矩的 agent 收不到 `Revoke` 就不执行 `handleRevokeCommand`**，本地 token 留在磁盘上直到 30 天 TTL 到期。安全上不构成新暴露面——状态闸门（IC-BUG-23 修复）已拦住它重连 |
| **来源** | 原埋在 IC-BUG-25 的「备注（本刀未处理）」里，处置写的是「随 IC-2 顺手处理」。2026-09-10 的 M-2 类扫描把它升格为独立卡片 |
| **修复** | 让切流等一个**有界**的短窗：`Send` 之后不立刻 cancel，等发送 goroutine 确认写出（或固定等 ≤1s）再 `Disconnect`，超时则直接切。**上限必须是硬的**——绝不能无限等一个不读流的 agent，那会重蹈 IC-BUG-25 复审里 MF-4 的覆辙 |
| **验收** | bufconn 用例：正常 agent 被吊销时先收到 `RevokeCommand`、流随后才断；**不读流的 agent 在上限时间内仍被切断**（不因等待而挂住 handler） |

## IC-BUG-33 — 失败的上报仍写 `file_entries` 行，制造反向幽灵 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `HandleUploadResult` **无论 `success` 与否都调 `UpsertFileEntry`**（`indexer.go:194-210`），只把 `status` 置为 `failed`；只有 NATS 事件被 `:251` 的 `result.GetSuccess()` 抑制。于是「上传失败」与「上传成功」在 `file_entries` 里的差别仅是一个 `status` 列 |
| **精确位置** | `controlplane/internal/indexer/indexer.go:194-210`（无条件 upsert）、`:251`（事件才判 success） |
| **可利用性 / 触发条件** | **当前为零**——agent 从不上报（IC-BUG-2），这条路径是死代码。**IC-2a ⑥ 让「重试耗尽以 `success=false` 上报」成为常规行为的那一刻起就成立**，与 IC-BUG-29 同一个机制 |
| **后果** | MinIO 里没有对象，`file_entries` 里却有行。这是 IC-6 `object_keys` 与 IC-13 L2 分片对账的**反向幽灵**（DB 有、对象无）——L3 幽灵清理找的是「DB 有而对象无」，这些行会被当成真幽灵反复核实、浪费对账预算；若清理逻辑真删了它们，又会把「上传失败」这个运维信号一并抹掉 |
| **✅ 定案（2026-09-10）：`success=false` 时不写 `file_entries`，只写 `upload_logs`** | 归 **IC-2a ⑥**。定案理由**不是「更简洁」，而是另一个选项有损坏真实数据的分支**——见下行 |
| **为什么不选「照写 + 对账排除 `status='failed'`」** | ① upsert 的 `DO UPDATE` **无条件覆盖 `status`**（IC-BUG-8 的同一条 SQL）。于是「某路径已成功上传（行是 `completed`、MinIO 里对象好好的）→ 后来同路径重传失败」会把**那行活着的对象标成 `failed`**——这比反向幽灵更糟，是把真实存在的对象标成失败。**且不止 `status`**：失败上报里 `sha256` / `etag` / `content_type` 都是空串，经 `Valid: x != ""`（`indexer.go:203-206`）落成 NULL，会把这行活对象的校验和一并抹掉（评审复核时补强）；② 那个过滤条件要被记住的地方不止两处：IC-6 `object_keys` 回填、IC-13 L2 分片扫描、L3 幽灵清理，**以及 IC-7 的 Dashboard 统计**（`COUNT(*)`/`SUM(size_bytes)` 会把失败上传算进「总文件数」与「总存储量」）。把不变式换成一个必须被记住的隐性契约，代价太高 |
| **实现细节** | `CreateUploadLog` 现传 `FileEntryID: {fileEntry.ID, Valid: true}`（`indexer.go:234`），跳过 upsert 后没有该 ID——置 `Valid: false` 即可，`upload_logs.file_entry_id` 可空（`REFERENCES file_entries(id)`，无 NOT NULL）|
| **连带改动** | 摘掉 Files 页的「失败」筛选项（`webui/src/pages/Files/index.tsx:37`）——它将永远返回空；`file_status` 枚举里的 `failed` 成为死值（**不删枚举**，迁移只追加）。失败信号统一走已有的 Logs 页 / `upload_logs`，那张表才有 `error_message` / `retry_count` / `started_at` / `finished_at`，本就是为此建的 |
| **保住的不变式** | **`file_entries` 一行 = MinIO 里一个对象**。IC-6 的 `object_keys` 回填与 IC-13 的 L2/L3 全都白捡这个前提，不必记任何过滤条件 |
| **验收** | 制造一个不可达的 MinIO，任务重试耗尽后：`upload_logs` 有失败行；`file_entries` 或无该行、或该行被对账显式排除（按选定方案二选一断言） |

## IC-BUG-34 — agent 重启后 `running` 态任务永久孤儿 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `DequeuePending` 把任务从 `pending` 置为 `running`（`queue.go:292-319`），但**启动时没有任何 `running → pending` 的复位**，且 `DequeuePending` 只取 `pending`（`:300`）。`upload_tasks` 是持久化的，进程重启后这些行原样留在 `running` |
| **精确位置** | `agent/internal/queue/queue.go:292-319`（置 running）、`:300`（只取 pending）；全文件无复位语句 |
| **后果** | agent 崩溃 / 被 kill / 正常重启时正在上传的任务**永久卡死**：不会被重新 dequeue（状态不是 pending），也不会被上报（IC-2a 之后同样够不着）。文件既没进 MinIO 也没进索引，且**没有任何信号**——`CountActive` 会把它算进队列深度，心跳里的 `queue_depth` 只会显示一个不下降的数字 |
| **与 IC-2a 的关系** | IC-2a 引入 `reported` 后会**多一个同类终态**：ack 丢失 + 进程重启 = 卡在 `reported` 的孤儿。IC-2a ④ 的重报超时若只在内存里计时，重启后同样失效——**复位必须落在启动路径上，不能只靠运行时定时器** |
| **修复** | agent 启动时（`Queue` 初始化后、executor 启动前）把 `running` 复位为 `pending`；IC-2a 落地后 `reported` 同样需要复位或纳入重报超时的持久化判据。注意与 IC-3 的续传落盘协同——复位后应保留 `upload_id` / `completed_parts` 以便续传而非重传 |
| **验收** | 上传中途 kill agent，重启后该任务被重新 dequeue 并完成（日志可见）；`queue_depth` 能回落到 0 |

---

## 关联入口

- 决策记录：[`DECISIONS.md`](../../../DECISIONS.md) **D-030**（写入准入与一致性模型）
- 设计文档：[`docs/design/consistency-and-ingest.md`](../../design/consistency-and-ingest.md)
- 已关闭 Bug：[`closed.md`](closed.md)
- 当前活跃任务：[`../active.md`](../active.md)
