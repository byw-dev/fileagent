# bugs/closed.md — 已关闭 BUG 归档

> 本文件仅供人工阅读，记录已修复的 Bug。Agent 无需读取。
> 最新关闭的 Bug 在最上方。

---

## 2026-05-07 修复 — T3-1-BUGFIX（gRPC 注册链路三个关键 Bug）

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
