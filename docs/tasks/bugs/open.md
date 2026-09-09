# bugs/open.md — 未解决 BUG

> 本文件结构化描述所有待修复 Bug，Agent 可直接消费。
> 每个 Bug 包含：根因、精确代码位置、修复方案、验收标准。

---

## 总览

**IC-BUG 系列（数据面写入链路，2026-09-08 审计发现；IC-BUG-16…21 为 2026-09-09 追加：16/17 来自 IC-1 编码期，18/19 是 IC-1 的 live-e2e 中暴露的，20…25 来自 IC-1 的 code review，其中 22/23 已随 IC-1 修复）** —— 关联决策 [`DECISIONS.md`](../../../DECISIONS.md) D-030、
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
| IC-BUG-17 | 缓存 token 重启后 `AgentID`/`AgentName` 恒为空，`dest_path_template` 整体失效 | 🔴 P0 | agent |
| IC-BUG-18 | Agent 只能以 TLS 拨号，而 CP gRPC 是明文，本地永远连不上 | 🔴 P0 | agent |
| IC-BUG-19 | minio-event 索引 URL 编码后的对象键（`%2F`），与真实键不符 | 🟠 P1 | controlplane |
| IC-BUG-20 | bucket 集合变化后凭据不补发，新规则最长约 50 分钟持续 403 | 🟠 P1 | controlplane + agent |
| IC-BUG-21 | 模板解析失败时猜一个对象键写进去，污染对账分片 | 🟡 P2 | agent |
| IC-BUG-22 | `PollApproval` 不校验 fingerprint，凭 agent UUID 即可换取 30 天 token | 🔴 P0 | controlplane |
| IC-BUG-23 | 吊销不生效：被吊销 agent 的 token 仍可用，且重连会把状态刷回 online | 🔴 P0 | controlplane |
| IC-BUG-24 | `handleDryRunResult` 无归属校验，可对他人 rule 投递伪造试运行结果 | 🟡 P2 | controlplane |
| IC-BUG-25 | 吊销切不断已建立的流：被吊销 agent 仍可心跳/上报，UI 显示在线且踢不掉 | 🟠 P1 | controlplane |

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
| **验收** | `mc cp` 一个 `a/b/中 文.csv`，`file_entries.storage_path` 等于 `a/b/中 文.csv`；预签名下载可直接取回该对象 |

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
| **修复** | 兜底改为**任务失败**（可重试 / 可告警）而不是猜键；Warn 按 `rule_id` 去重（首次记录）或采样。建议随 IC-2 一起做——IC-2 正好要改上报与终态语义 |
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
| **修复** | 投递前校验该 rule 归属于流上的 agentID（`handleDryRunResult` 增加 agentID 参数）。归 IC-2 一并做 |
| **验收** | agent A 对 agent B 的 rule_id 投递 DryRunResult 被丢弃并告警 |

## IC-BUG-25 — 吊销切不断已建立的流 🟠 P1

| 字段 | 内容 |
|------|------|
| **根因** | `assertAgentUsable` 只在**建流时**跑一次（IC-1 加的闸门）。已建立的流不受任何约束：`AgentConn.CancelFunc` **全仓库无人调用**（唯一的 `Unregister` 是 `Connect` 自己的 defer），REST 的 `Revoke` 只发一条**协作式**命令；`handleAgentMessage` 的四个分支都没有闸门 |
| **精确位置** | `controlplane/internal/grpcserver/registry.go`（`CancelFunc` 无调用点）；`controlplane/internal/grpcserver/handler.go` `handleAgentMessage`；`controlplane/internal/agent/manager.go` `RevokeAgent` |
| **后果** | 被入侵的 agent 已连接 → 管理员吊销 → 它忽略 `Revoke` 命令、不断开。于是：①继续心跳刷新 `agent:online:<id>` 与 `last_seen_at`，**UI 上这个已吊销的 agent 一直显示在线，管理员没有任何手段把它踢下线**；②继续上报 `UploadResult`（IC-2 之后就是攻击者可控地直接写 `file_entries`/`upload_logs`）；③继续响应 `ListDirectory` |
| **残留窗口（已接受）** | 手里已签发的 STS 会话在 ≤1h 内仍是整桶写。STS 会话本质上不可撤销（除非轮转 MinIO 父用户或加 deny policy），这一条**接受**，但必须在运维文档里写明「吊销不是即时的，最长一个 STS TTL」 |
| **修复** | `RevokeAgent` 发完命令后主动切流：registry 加 `Disconnect(agentID)` 调用该 conn 的 `CancelFunc`。顺带修掉「已吊销却显示在线」。归 IC-2 |
| **验收** | 吊销一个不配合的 agent（不处理 `Revoke` 命令的构造版本）后，流在秒级断开、UI 立即显示离线、后续 `UploadResult` 不再入库 |

---

## 关联入口

- 决策记录：[`DECISIONS.md`](../../../DECISIONS.md) **D-030**（写入准入与一致性模型）
- 设计文档：[`docs/design/consistency-and-ingest.md`](../../design/consistency-and-ingest.md)
- 已关闭 Bug：[`closed.md`](closed.md)
- 当前活跃任务：[`../active.md`](../active.md)
