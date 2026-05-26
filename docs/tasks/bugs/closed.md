# bugs/closed.md — 已关闭 BUG 归档

> 本文件仅供人工阅读，记录已修复的 Bug。Agent 无需读取。
> 最新关闭的 Bug 在最上方。

---

## 2026-05-25 修复 — T3-6-BUG 系列（Dry-Run 两个 P1 Bug）

| ID | 标题 | 严重程度 | 涉及模块 |
|----|------|---------|---------|
| T3-6-BUG-A | `{agent_name}` 路径模板占位符永远报错 missing field | 🟡 P1 | agent + controlplane + proto |
| T3-6-BUG-B | Dry-Run `parsed_fields` 中时间/整数类型字段值为空字符串 | 🟡 P1 | pkg/trollsift + agent |

**修复提交**：`85b5544` (fix(T3-6): address review comments — proto source path, naming, logging, test coverage)

### T3-6-BUG-A — `{agent_name}` 路径模板占位符永远报错 missing field ✅

| 字段 | 内容 |
|------|------|
| **严重程度** | 🟡 P1 |
| **根因** | 链式缺失：(1) proto `RegisterResponse`/`PollApprovalResponse` 不含 `agent_name`；(2) `Lifecycle` 无 `AgentName` 字段；(3) `main.go` 未赋值 `agentCtx.AgentName`；(4) `InjectContext` 在 `AgentName == ""` 时跳过注入 |
| **修复** | 方案 B：proto 两条响应消息各新增 `string agent_name = N`；`controlplane` handler 填入 DB `agent.Name`；`Lifecycle` 新增 `AgentName`，`Register`/`PollApproval` 读取并存储；`main.go` 新增 `agentCtx.AgentName = lc.AgentName` |
| **受影响文件** | `proto/v1/agent.proto`、`api/v1/agent.pb.go`、`controlplane/internal/agent/manager.go`、`agent/internal/grpcclient/registration.go`、`agent/cmd/agent/main.go` |

### T3-6-BUG-B — Dry-Run `parsed_fields` 中时间/整数类型字段值为空字符串 ✅

| 字段 | 内容 |
|------|------|
| **严重程度** | 🟡 P1 |
| **根因** | `main.go` handleDryRun 使用 `v.Str` 填充 ParsedFields；`kindTime`/`kindInt` 的 `Str` 为零值 `""` |
| **修复** | Raw string 方案：`trollsift.Value` 新增 `Raw string` 字段，`Parse` 赋值为正则捕获的原始子串；handleDryRun 改用 `v.Raw`；补充 parser_test.go Raw 断言 |
| **受影响文件** | `pkg/trollsift/parser.go`、`pkg/trollsift/parser_test.go`、`agent/cmd/agent/main.go` |

---

## 2026-05-12 修复 — T3-5-BUG 系列（采集规则字段统一 + BUG 修复）

| ID | 标题 | 严重程度 | 涉及模块 |
|----|------|---------|---------|
| T3-5-BUG-A | 规则字段名不规范：REST-in 使用前端别名而非统一字段名 | 🟡 P2 | controlplane + webui |
| T3-5-BUG-B | `upload_bucket` 始终为空，Agent 无法写入 MinIO | 🔴 P0 | controlplane |
| T3-5-BUG-C | `mode` 大小写不一致导致 DB 写入失败 | 🟡 P1 | controlplane |
| T3-5-BUG-D | `append_mode` 值域三端不一致 | 🟡 P1 | agent + controlplane |

### T3-5-BUG-B — `upload_bucket` 始终为空 ✅

**根因**：`ruleToProto()` 从未填充 `UploadBucket` 字段。  
**修复**：`Dispatcher` 新增 `BucketQuerier` 接口（`GetBucketByID`），在 `DispatchRule` 和 `SyncRulesOnConnect` 中查出 bucket 名后传入 `ruleToProto(rule, bucketName)`。

### T3-5-BUG-C — `mode` 大小写不一致 ✅

**根因**：前端发送 `"WATCH"`/`"SCHEDULED"` 大写，DB enum 只接受小写。  
**修复**：`CreateRule` handler 写入 DB 前调用 `strings.ToLower(req.Mode)`。

### T3-5-BUG-D — `append_mode` 三端不一致 ✅

**根因**：`AppendModeNone = ""`（Agent）vs `'overwrite'`（DB/proto）。  
**修复**：`AppendModeNone` → `AppendModeOverwrite = "overwrite"`；SQLite DDL/migration default `''` → `'overwrite'`；CP handler 默认值 `"none"` → `"overwrite"`。

### T3-5-BUG-A — 规则字段名不规范 ✅

**根因**：REST/前端使用别名（`dest_bucket_id`、`source_path`、`file_pattern`）而非 D-009 统一字段名。  
**修复**：REST JSON tag 与前端接口字段名对齐 D-009：`dest_bucket_id`→`bucket_id`，`source_path`→`base_path`，`file_pattern`→`path_pattern`。

---

## 2026-05-11 修复 — T3-2-BUG 系列（集成联调新发现 Bug A~D）

| ID | 标题 | 严重程度 | 涉及模块 |
|----|------|---------|---------|
| T3-2-BUG-A | Agent 审批后无最后心跳时间，无在线状态展示 | 🔴 P0 | controlplane + webui |
| T3-2-BUG-B | 目录浏览请求返回 409，CP 认为采集器 OFFLINE | 🔴 P0 | controlplane + webui |
| T3-2-BUG-C | 新建采集规则第三步缺少提交按钮，无法创建规则 | 🟡 P1 | webui |
| T3-2-BUG-D | 创建 Bucket 时 MinIO 报错，CP 静默忽略返回 201 | 🔴 P0 | controlplane + webui |

### T3-2-BUG-A — Agent 审批后无最后心跳时间，无在线状态展示 ✅

| 字段 | 内容 |
|------|------|
| **严重程度** | 🔴 P0 |
| **根因** | (A-1) `handleHeartbeat` 只刷新 Redis TTL，从未调用 `UpdateAgentLastSeen`，`agents.last_seen_at` 永远为 NULL；(A-2) `Connect`/`Disconnect` 未调用 `UpdateAgentStatus`，DB `status` 卡在 `approved`；(A-3) REST 响应缺少 `is_online` 字段 |
| **修复** | A-1: `handleHeartbeat` 新增 `db.UpdateAgentLastSeen` 调用；A-2: `Connect` 时调 `UpdateAgentStatus("online")`，defer 断开时调 `UpdateAgentStatus("offline")`；A-3: `agentResponse` 增加 `is_online bool`（实时查 Redis）；A-4: WebUI 采集器列表/详情新增在线状态徽标和心跳时间展示 |
| **受影响文件** | `controlplane/internal/grpcserver/handler.go`、`controlplane/internal/api/handler/agents.go`、`webui/src/pages/Agents/` |

### T3-2-BUG-B — 目录浏览请求返回 409，CP 认为采集器 OFFLINE ✅

| 字段 | 内容 |
|------|------|
| **严重程度** | 🔴 P0 |
| **根因** | (B-1) `ListDir` 仅检查内存注册表（`registry.IsOnline`），CP 重启后内存注册表为空，即使 Redis 有有效 TTL 仍返回 409；(B-2) 前端目录浏览 Tab 不检查 `is_online`，离线时无保护提示 |
| **修复** | B-1: 在内存注册表 miss 时降级检查 Redis（`h.cache.Exists`），两者均无才 409；`dirstore.Deliver` 改为非阻塞，防止边缘情况死锁（提交 `d87fe18`）；B-2: WebUI 目录浏览 Tab 当 `is_online=false` 时显示 Warning Alert 并禁用浏览按钮 |
| **受影响文件** | `controlplane/internal/api/handler/agents.go`、`controlplane/internal/dirstore/store.go`、`webui/src/pages/Agents/Detail.tsx` |

### T3-2-BUG-C — 新建采集规则第三步缺少提交按钮，无法创建规则 ✅

| 字段 | 内容 |
|------|------|
| **严重程度** | 🟡 P1 |
| **根因** | `StepsForm.submitter.render` 配置的是整体提交区域，不透传给各 `StepForm` 子步骤；第三步底部无任何按钮渲染。次要：`onFinish` 始终 `return true`，API 失败时表单重置到第一步 |
| **修复** | 将步骤按钮迁移到各 `StepForm.submitter`（`StepsForm` 全局 `submitter.render` 改为统一控制）；`handleFinish` 失败时 `return false` 保留第三步 |
| **受影响文件** | `webui/src/pages/Agents/RuleForm.tsx` |

### T3-2-BUG-D — 创建 Bucket 时 MinIO 报错，CP 静默忽略返回 201 ✅

| 字段 | 内容 |
|------|------|
| **严重程度** | 🔴 P0 |
| **根因** | (D-1) CP `CreateBucket` 无前置命名校验，非法名称先写 DB 再被 MinIO 拒绝；(D-2) MinIO 错误被静默忽略，始终返回 201，DB 出现孤立记录；(D-3) 前端无 Bucket 命名实时校验 |
| **修复** | D-1: `CreateBucket` 入口增加 S3 命名规范 regexp 校验，非法时 422；D-2: MinIO 失败时回滚 DB 记录并返回 502；D-3: WebUI 创建 Bucket 表单增加客户端 validator |
| **受影响文件** | `controlplane/internal/api/handler/events.go`、`webui/src/pages/Buckets/` |

---

## 2026-05-11 修复 — createRule 400 错误（采集规则创建 JSON 字段名不匹配）

| 字段 | 内容 |
|------|------|
| **严重程度** | 🔴 P0 |
| **修复提交** | `addb3ee` |
| **根因** | CP `createRuleRequest` 结构体 JSON tag 使用的是 DB 内部字段名（`bucket_id`、`source_path_template`、`file_glob`、`upload_path_template`），与 WebUI 发送的字段名（`dest_bucket_id`、`source_path`、`file_pattern`、`dest_path_template`）完全不匹配，gin `binding:"required"` 校验失败返回 400 |
| **附带修复** | (1) Agent `ResolvePath` 新增用户友好变量别名（`{year}`/`{month}`/`{day}`/`{hour}`/`{minute}`），向下兼容旧技术名（`{yyyy}`, `{mm}` 等）；新增 `ResolvePathWithFile` 支持 `{filename}` 变量；(2) WebUI `PATH_TEMPLATE_VARIABLES` 裁剪至 Agent 实际支持的 6 个变量，移除 `{agent_id}`/`{agent_name}`/`{file_type}`/`{ext}` 等未实现变量；(3) `DECISIONS.md` 新增 D-008 记录 CP→Agent 命令同步等待（30s）决策 |
| **受影响文件** | `controlplane/internal/api/handler/agents.go`、`controlplane/internal/api/handler/agents_test.go`、`agent/internal/scheduler/scheduler.go`、`agent/internal/scheduler/scheduler_test.go`、`agent/cmd/agent/main.go`、`webui/src/utils/pathTemplate.ts`、`webui/src/pages/Agents/RuleForm.tsx`、`DECISIONS.md` |

---

## 2026-05-07 修复 — T3-1-BUG（gRPC 注册链路三个关键 Bug）

### Bug A — Register ErrNoRows 判断逻辑颠倒 ✅

| 字段 | 内容 |
|------|------|
| **严重程度** | 🔴 P0 |
| **修复提交** | `e295c05` |
| **根因** | sqlc 生成的 `GetAgentByFingerprint` 在 ErrNoRows 时返回 `&i`（非 nil 指针）；原代码 `if existing != nil` 永为 true，导致首次注册总是走"已注册"分支，返回全零 UUID |
| **修复** | `manager.go:80-83`：改为先判断 `err != nil && !errors.Is(err, sql.ErrNoRows)`，再用 `if err == nil` 判断真正查到了记录 |
| **受影响文件** | `controlplane/internal/agent/manager.go`、`manager_test.go` |

### Bug B — PollApproval 审批通过后不返回 auth_token ✅

| 字段 | 内容 |
|------|------|
| **严重程度** | 🔴 P0 |
| **修复提交** | `e295c05` |
| **根因** | `PollApproval` 响应体缺少 `auth_token` 字段，Agent 轮询拿到空 token，后续 Connect RPC 被 JWT 拦截器拒绝 |
| **修复** | `manager.go:198-214`：approved 状态时调用 `jwtSvc.GenerateAccessToken` 并将 token 写入响应，同时更新 DB `auth_token_hash` |
| **受影响文件** | `controlplane/internal/agent/manager.go`、`manager_test.go` |

### Bug C — gRPC 反射服务未豁免 JWT 认证 ✅

| 字段 | 内容 |
|------|------|
| **严重程度** | 🟡 P1（工具链受阻，不影响真实 Agent） |
| **修复提交** | `e295c05` |
| **根因** | 反射端点未在 `jwtExemptMethods` 白名单中，`grpcurl` 无法解析服务描述符 |
| **修复** | `interceptor.go:34-35`：添加 `grpc.reflection.v1` 和 `grpc.reflection.v1alpha` 两个反射端点豁免 |
| **受影响文件** | `controlplane/internal/grpcserver/interceptor.go`、`interceptor_test.go` |

---

## 2026-05-09 修复 — T3-2-FIX 系列（API 契约对齐 A~L）

| ID | 标题 | 严重程度 | 涉及模块 |
|----|------|---------|---------|
| T3-2-FIX-A | 后端：`GET /api/v1/agents` 响应格式与参数对齐 | 🔴 P0 | controlplane |
| T3-2-FIX-B | 后端：`GET /api/v1/agents/:id/rules` 与上传日志端点信封对齐 | 🔴 P0 | controlplane |
| T3-2-FIX-C | 前端：`Agent` 接口与 `AgentStatus` 对齐 | 🔴 P0 | webui |
| T3-2-FIX-D | 前端：`CollectionRule` 接口字段对齐 | 🔴 P0 | webui |
| T3-2-FIX-E | 后端：`GET /api/v1/files` 响应格式与字段名对齐 | 🔴 P0 | controlplane |
| T3-2-FIX-F | 前端：`FileEntry` 接口字段对齐 | 🔴 P0 | webui |
| T3-2-FIX-G | 后端：upload-logs 端点响应格式与字段对齐 | 🔴 P0 | controlplane |
| T3-2-FIX-H | 前端：`UploadLog` 接口字段对齐 | 🔴 P0 | webui |
| T3-2-FIX-I | 后端+前端：非分页列表端点统一为 `{items, total}` 信封 | 🟡 P1 | controlplane + webui |
| T3-2-FIX-J | 后端：快增长表分页补齐 `has_more` + 真实 `total` | 🔴 P0 | controlplane |
| T3-2-FIX-K | 前端：`Modal.confirm/message` 静态 API 在 React 18 StrictMode 下静默失效 | 🔴 P0 | webui |
| T3-2-FIX-L | 前端：`Detail.tsx` 重构后 `Modal` import 丢失，采集器详情页崩溃 | 🔴 P0 | webui |

**全部完成**，详细根因与修复方案见该系列提交历史（提交 `2715706`）。

---

## 2026-05-06 修复 — P3-P 系列（Phase 3 前置质量关卡）

| ID | 标题 | 类型 | 严重程度 |
|----|------|------|---------|
| P3-P1 | DB 集成测试环境修复（`fileagent_test` DB 不存在） | 测试环境 | 🔴 P0 |
| P3-P2 | DownloadURL bucket 名 Bug（用 UUID 而非 bucket 名调用 MinIO；TTL 5→15min） | 功能 Bug | 🔴 P0 |
| P3-P3 | BucketsCreate 缺少 `madmin.MakeBucket()` 调用，MinIO 中无物理 Bucket | 功能缺口 | 🔴 P0 |
| P3-P4 | append_mode tail/close_wait 完全未实现（watcher/executor/queue） | 功能缺口 | 🔴 P0 |
| P3-P5 | grpcclient 覆盖率不足（61.5% < 80%，T2-X6 新增方法全部 0%） | 测试覆盖 | 🔴 P0 |
| P3-P6 | event 包覆盖率不足（62.7% < 80%，processRetries/retryDelivery/DBAdapter 全 0%） | 测试覆盖 | 🔴 P0 |
| P3-P7 | 事件重试退避表错误（30s×2^n ≠ 设计要求 30s→2min→10min→30min→2h；无5次上限） | 设计偏差 | 🟡 P1 |
| P3-P8 | MinioEventHandler.Handle 只打日志，未调用 Indexer | 功能缺口 | 🟡 P1 |
| P3-P9 | REST handler 覆盖率不足（DownloadURL 47.6%，MinioEventHandler.Handle 0%，authdb 0%） | 测试覆盖 | 🟡 P1 |
| P3-P10 | queue 包覆盖率不足（79.5% < 80%，Open 错误路径 62.5%） | 测试覆盖 | 🟢 P2 |

详细根因与修复方案见 `docs/reports/phase3-readiness-audit.md`（已归档）。
