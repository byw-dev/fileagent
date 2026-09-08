# bugs/open.md — 未解决 BUG

> 本文件结构化描述所有待修复 Bug，Agent 可直接消费。
> 每个 Bug 包含：根因、精确代码位置、修复方案、验收标准。

---

## 总览

**IC-BUG 系列（数据面写入链路，2026-09-08 审计发现；IC-BUG-16 为 2026-09-09 追加）** —— 关联决策 [`DECISIONS.md`](../../../DECISIONS.md) D-030、
设计 [`docs/design/consistency-and-ingest.md`](../../design/consistency-and-ingest.md)。

> ⚠️ **IC-BUG-1…IC-BUG-4 合起来意味着：Agent 数据面从未端到端跑通过。** 单元测试全部 mock 掉了 STS 与 gRPC，
> 因此这些缺陷长期不可见。当前 `file_entries` 的唯一写入者是 MinIO webhook（`/internal/minio-event`），
> 与 `system-design.md` §4.5 描述的主路径完全相反。

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
| **后果** | 即使修好 IC-BUG-1，上传仍会 403。且该前缀直接决定 D-030 中 grant 的对账粒度，必须先定死 |
| **修复** | 二选一并在 D-030 记录：(a) 强制 `dest_path_template` 前缀，由 CP 在规则保存时校验/补齐；(b) policy 按规则的 `dest_path_template` 的**静态前缀部分**动态生成（推荐——同时天然收窄 grant 范围）。无论哪种，`storage_path` 的前缀约定必须写入 `contracts.md` |
| **验收** | 用签发的 STS 凭据直接 `PutObject` 到规则实际生成的 `storage_path`，返回 200；写到前缀之外返回 403 |

## IC-BUG-4 — STS session policy 缺 multipart 权限、多授 DeleteObject 🔴 P0

| 字段 | 内容 |
|------|------|
| **根因** | 两个同源问题：① Action 列表为 `PutObject / GetObject / DeleteObject / ListBucket`，缺 `s3:AbortMultipartUpload` 与 `s3:ListMultipartUploadParts`；② **Action 层级与 Resource ARN 层级不匹配**——所有 Resource 都构造成对象级 ARN（`arn:aws:s3:::{bucket}/{prefix}/*`），却把桶级 action `s3:ListBucket` 塞进同一个 statement，该条授权从来就是空转的 |
| **精确位置** | `controlplane/internal/storage/policy.go:30-39`（Resource 全为对象级 ARN）、`:51-55`（Action 列表） |
| **文档冲突** | `system-design.md` §6.3 的 policy 明确要求包含 multipart 两个 Action，且**不包含** `s3:DeleteObject` |
| **后果** | >64MB 文件走 multipart：`verifyRemoteParts` 的 ListParts 会 403（续传路径不可用，与 IC-BUG-5 叠加）；失败后无法 Abort，孤儿分片无法清理。IC-BUG-5 验收要用的 `mc ls --incomplete` 需要 `s3:ListBucketMultipartUploads`，同为桶级 action，当前既没列出、列出了也会因 ARN 层级不匹配而空转。另外多授的 `DeleteObject` 让被入侵的 Agent 可删除已归档数据，与最小权限原则不符 |
| **修复** | 拆成两个 statement：**桶级** `s3:ListBucket` / `s3:ListBucketMultipartUploads` → `arn:aws:s3:::{bucket}`（不带 `/*`），需收窄时加 `s3:prefix` condition；**对象级** `s3:PutObject` / `s3:GetObject` / `s3:AbortMultipartUpload` / `s3:ListMultipartUploadParts` → `arn:aws:s3:::{bucket}/{prefix}/*`。移除 `DeleteObject`（若某处确需删除，单独签发或走 CP） |
| **验收** | 上传一个 >64MB 文件成功；中断后重试能走续传；`mc ls --incomplete` 可执行（不 403）且无残留；用签发的 STS 对桶做 `ListBucket` 能返回前缀内对象、前缀外须 403 |

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
| **修复** | 索引失败返回 5xx 让 MinIO 重投；解析失败返回 400（真正的坏载荷不该无限重投）。注意保持幂等——重投会重复索引，由 `UNIQUE (bucket_id, storage_path)` upsert 兜住 |
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
| **修复** | 按 D-030：`file_entries` 增加 `observed_at`（排序键）与 `source`；`DO UPDATE` 加 `WHERE EXCLUDED.observed_at >= file_entries.observed_at`，富字段一律 `COALESCE(EXCLUDED.x, file_entries.x)`。软删除同样加时间围栏 |
| **验收** | 单测：先以 `source=agent` 写入完整行，再以 `source=minio_event` 用更早/更晚的 `observed_at` 各写一次，富字段均不被清空 |

## IC-BUG-9 — webhook `queue_dir` 位于 `/tmp` 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `queue_dir=/tmp/minio-webhook-queue` 在容器内是易失路径 |
| **精确位置** | `deploy/scripts/init-minio.sh:125`；`docs/design/system-design.md:1731`（同样的示例配置） |
| **后果** | MinIO 容器重启/重建 → 未投递事件全部丢失，无任何补偿。叠加 IC-BUG-6 后，事件丢失有两条独立通道 |
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
| **验收** | 制造一个不可达的 MinIO，任务重试耗尽后 Web UI 的上传日志出现 failed 记录 |

## IC-BUG-13 — `content_type` 两条索引路径都不赋值，且会被清空 🟡 P2

| 字段 | 内容 |
|------|------|
| **根因** | `UpsertFileEntryParams` 有 `ContentType` 字段，但 `HandleUploadResult` 与 `IndexUpload` 都不填充；而 `DO UPDATE` 又无条件写 `content_type = EXCLUDED.content_type` |
| **精确位置** | `controlplane/internal/indexer/indexer.go`（全文无 `ContentType`）；`controlplane/internal/indexer/queries.go:56` |
| **后果** | 该列恒为 NULL；将来任何补齐它的路径都会被另一条路径清空（与 IC-BUG-8 同源） |
| **修复** | 随 IC-BUG-8 的 `COALESCE` 一并修；MinIO 事件载荷含 `contentType` 时填入，agent 侧由 `UploadResult` 带上（需 proto 增字段，只增不改编号） |
| **验收** | 上传一个 `.csv`，`file_entries.content_type` 为 `text/csv`，随后到达的 webhook 事件不会清空它 |

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
| **后果** | **凡是模板以 `/` 开头的规则，MT-3 的 path_var 打标从未生效过**——`applyPathVarTags` 只 `logger.Warn("storage path does not match template")` 后 return，索引照常成功，缺陷完全静默。现有 4 个模板夹具里 3 个以 `/` 开头 |
| **同源** | 与 IC-BUG-3 是同一类病：同一个模板在 agent 与 CP 两端各自解释，没有任何机制保证一致 |
| **修复** | 抽一个模板归一化函数（去前导 `/`，其余规则集中），**agent 拼路径与 CP 反解共用同一个**；放在 `pkg/trollsift` 或其相邻位置，使两端不可能再分叉 |
| **验收** | 建一条 `path_tag_map` 非空、模板以 `/` 开头的规则，上传文件后 `file_tags` 中出现 `source='path_var'` 的行；单测覆盖「模板带/不带前导 `/`」两种写法均能反解 |

---

## 关联入口

- 决策记录：[`DECISIONS.md`](../../../DECISIONS.md) **D-030**（写入准入与一致性模型）
- 设计文档：[`docs/design/consistency-and-ingest.md`](../../design/consistency-and-ingest.md)
- 已关闭 Bug：[`closed.md`](closed.md)
- 当前活跃任务：[`../active.md`](../active.md)
