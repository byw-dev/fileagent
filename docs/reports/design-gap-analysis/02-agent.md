# 02 — Edge Agent 与设计文档对照

> 对照基准：`docs/design/system-design.md` 第四章 + 附录 C.2
> 标记：✅ 一致 ⚠️ 有出入 ❌ 缺失/错误 📝 设计未覆盖

---

## 1. 模块结构（设计 §4.1 / §4.9）

实际结构比设计精简（12 个源文件 vs 设计 ~25 个），多为合理合并（inotify/windows/poller 合入 watcher.go，resume 合入 multipart.go）。**缺失的模块**：

| 设计模块 | 状态 | 说明 |
|----------|------|------|
| `sysinfo/`（磁盘枚举） | ❌ | 完全不存在。心跳中 DiskInfo 永远为空 |
| `metrics/`（Prometheus Exporter） | ❌ | **配置空壳**：`config.go` 完整解析 `[metrics]` 段（enabled/port，含校验），但全代码库没有任何 HTTP server 启动代码。配置了也不会生效 |

## 2. 心跳（设计 §4.3 Heartbeat 消息）

❌ **心跳发送的是空结构体**（`grpcclient/client.go:293`：`hb := &agentv1.Heartbeat{}`）。
设计定义的 6 个字段——agent_id、uptime_seconds、queue_depth、upload_bps、version、disks——**全部未填充**。

影响链：
- Web UI / Dashboard 无法展示队列深度、上传速率、Agent 版本；
- 设计 §9.2 的全部 5 个 Agent 指标无数据来源；
- 告警规则 AgentQueueBacklog（queue_depth>1000）永远无法触发。

这是典型的"看起来完成了"：心跳机制工作正常（在线状态正确），但业务载荷为空。

## 3. 采集模式（设计 §4.4）

| 项 | 状态 | 说明 |
|----|------|------|
| Watch 模式 + inotify 降级轮询 | ✅ | fsnotify + pollInterval 兜底（`watcher.go`） |
| watch_subdir_pattern | 📝 | D-009 已删除该字段（migration 000003），设计 §4.4.1 步骤 3 未回填 |
| Scheduled 模式 + cron | ✅ | scheduler.go |
| append_mode 三态 | ✅ | overwrite/tail/close_wait 均有实现（早期审计 B-5 已修）；tail 断点 offset 的 schema migration 记为技术债（backlog） |
| 时间变量解析 | ⚠️ | 设计 §4.4.2 用 `{yyyy}/{mm}` 自有语法；现实现已改为 trollsift（D-009/T3-4），设计文档未回填 |

## 4. 上传与队列（设计 §4.5 / §4.6）

| 项 | 状态 | 说明 |
|----|------|------|
| ≤64MB PutObject / >64MB 分片 | ✅ | uploader.go / multipart.go |
| 断点续传（ListParts + completed_parts） | ✅ | `multipart.go:117` |
| SHA-256 幂等 | ✅ | CP 侧 UpsertFileEntry |
| 失败退避重试 | ✅ | executor.go 指数退避 |
| **queue_max_size 上限** | ❌ | **配置空壳**：`QueueMaxSize`（默认 10000）只在 config.go 定义和校验，queue.go 无任何容量检查/丢弃最旧任务/告警逻辑。断网久了队列无限增长 |
| Worker 数量动态调整 | ❌ | 设计说"可由 CP 下发配置动态调整"，无此机制（可接受，无决策记录） |

## 5. 凭据管理（设计 §4.7）

| 项 | 状态 | 说明 |
|----|------|------|
| JWT AES-256-GCM 加密落盘 | ✅ | credential.go，密钥派生自 fingerprint |
| STS 内存存储 + 到期前刷新 | ✅（待动态验证） | |
| JWT 剩余 <20% 自动续期 | 待查 | 设计要求通过 gRPC 续期 token；proto 无对应 RPC（只有 STS 的 RefreshCredentials），疑似 ❌ 无 Agent token 续期机制——token 30 天过期后 Agent 是否需要重新注册？需在 e2e/代码中确认 |

## 6. 跨平台（设计 §4.8）

| 项 | 状态 | 说明 |
|----|------|------|
| Windows 服务封装 / WMI 磁盘枚举 | ❌ | 无 windows/svc 代码、无 sysinfo；fsnotify 本身跨平台，但 §4.8 表中 Windows 特有项均未做（部署脚本在 backlog T4-3） |
| 远程升级（§10.7） | ❌ | 设计后续迭代 P1，未实现（可接受） |

## 7. 小结

- 核心采集/上传链路完整且与设计一致，早期审计缺口已修。
- **三个"配置空壳"**（有配置无实现）：metrics 端点、queue_max_size、（CP 侧对应的）METRICS_LISTEN——这类缺口单测无法发现，是"每处看似完成"错觉的直接来源。
- **心跳载荷为空**是最高价值的单点修复：它同时解锁 UI 状态展示与整个监控告警链。
- Agent JWT token 续期机制疑似缺失，30 天后所有 Agent 可能集体掉线，需确认。
