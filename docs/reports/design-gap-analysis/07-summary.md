# 07 — 总结：僵局根因与破局建议

> 汇总 01~06 的发现，回答用户三个疑问，并给出破局路径。

---

## 一、你说的"僵局"，本质是什么

不是代码写得差，也不是进度落后——单测全绿、编译通过、页面齐全、主链路历史上跑通过。
真正的僵局是**三条反馈回路都断了**，于是每一步"完成"都无法被信任：

1. **"完成"的定义只到单元测试**。单测大量 mock 掉 DB/Redis/MinIO/NATS 和 HTTP，
   而你遇到的所有痛点——契约错位、配置空壳、心跳空载荷、SDK refresh 断裂——
   **全部是 mock 边界之外的集成问题**，单测结构上看不见。于是"绿灯"和"能用"脱钩。

2. **契约靠人手工在 5 层之间同步**（DB→proto→CP REST→WebUI→SDK）。
   已经发作两次（T3-2-FIX 的 12 项、T3-5 的四层字段统一），
   本次又实测到第三次（SDK↔CP refresh，06 报告 E-2）。防复发手段（OpenAPI 校验、
   MSW 契约测试）却被排到 P3 backlog。**病根没治，只是反复擦地板。**

3. **设计文档已不是权威，但流程仍假装它是**。CLAUDE.md 让每个 Agent"先读 system-design.md
   （权威来源）"，而该文档相对代码已有 6+ 处过时、3 处自相矛盾（05 报告）。
   新 Agent（或新人）照着学到的是错误契约，产出又需要下一轮 FIX——文档脱节直接喂养了回路 1、2。

**一句话**：项目不缺实现，缺的是**可信的"完成"判据**和**契约的单一权威**。

---

## 二、问题清单（按处理优先级）

### P0 — 阻断联调/会导致线上故障，必须先修

| 编号 | 问题 | 位置 | 报告 |
|------|------|------|------|
| ~~G-1~~ ✅ | ~~**SDK↔CP refresh 契约断裂**：SDK 放 body，CP 只认 header。~~ **已修复（2026-07-03，止血冲刺第 1 步）**：CP 改读 body（header 回退）+ 补 token 轮转；记 DECISIONS D-012；新增跨进程契约测试 `TestRefresh_Integration_BodyContractAndRotation`（本项目首个）。真实 CP 实测：body 契约✅ / 轮转✅ / 连续刷新✅ / 旧 token 吊销 401✅ | `cp auth.go Refresh` | 06 E-2 |
| ~~G-2~~ ✅ | ~~**Agent token 定时炸弹**：签的是 2h access token，重连超 2h 即被拒、永久掉线无法自愈。~~ **已修复（PR #40，止血冲刺第 2 步）**：双重防御——CP 改签长效 token（`AGENT_TOKEN_TTL`，默认 720h）+ Agent 在 Unauthenticated 时经 `PollApproval` 重连自愈；记 DECISIONS D-013 | `manager.go` / `grpcclient` | 06 E-1 |
| ~~G-3~~ ✅ | ~~**/internal/minio-event 无鉴权**：任何人可伪造上传事件污染 file_entries。~~ **已修复（PR #41，止血冲刺第 3 步）**：用 `INTERNAL_WEBHOOK_SECRET` 校验 MinIO `auth_token`（哈希后常量时间比较），未配置密钥时 fail-closed；记 DECISIONS D-014。真实 CP 实测：无凭据/错密钥 401、正确密钥 200 | `events.go` MinioEventHandler | 01 §1 |

### P1 — "看起来完成了"的空心功能，用户一用就露馅

| 编号 | 问题 | 报告 |
|------|------|------|
| G-4 | **心跳载荷全空**（queue_depth/upload_bps/version/disks 都没填）→ UI 状态、整个监控告警链无数据源 | 02 §2 |
| ~~G-5~~ ✅ | ~~**Dashboard 数字全是假的**：用"最近 20 条日志"推算今日上传/存储用量/7日趋势。根因是设计+CP 缺统计 API。~~ **已修复（PR #44）**：新增 `GET /api/v1/stats/dashboard` 服务端聚合端点（agents/files/storage/today/7日趋势），前端接真实数字；设计补齐 §5.11.5/§7.3.1；记 D-016。真实 CP 实测数字正确 | 03 §3 |
| G-6 | **两个配置空壳**：Agent metrics 端点、queue_max_size 上限——有配置、有校验、无实现 | 02 §1/§4 |
| G-7 | **Prometheus 指标整体缺失**：CP+Agent 均无插桩，设计第九章整章落空 | 01 §7 / 02 §1 |

### P2 — 契约治理（防第三、第四次复发）

| 编号 | 问题 | 报告 |
|------|------|------|
| G-8 | 无契约单一权威、无自动校验（OpenAPI + MSW 躺在 P3） | 05 §5 |
| G-9 | 隐性口头契约无文档：status 大小写映射、列表信封、trollsift 变量表、错误格式 | 05 §4 |
| G-10 | 错误响应缺顶层 request_id；未知 query 参数静默 200 | 06 契约瑕疵 |

### P3 — 文档回填与收尾

| 编号 | 问题 | 报告 |
|------|------|------|
| G-11 | 设计文档 6 处过时（字段/proto/路径语法/REST 清单/token 续期/配置项） | 05 §2 |
| G-12 | 设计文档 3 处自相矛盾（auth 路径、模板变量、离线判定） | 05 §1 |
| G-13 | 任务文档 3 处状态矛盾（CLAUDE.md 说 T3-2 进行中 / T3-6 状态打架） | 05 §3 |
| G-14 | bucket policy/lifecycle 未设置；file_deleted 事件死配置；TTL 驱动离线判定缺失 | 01 §4/§5 |
| G-15 | 凭据文件 `controlplane/bootstrap_admin_credentials.txt` 躺在工作区（应 gitignore） | 00 侦察 |

---

## 三、破局建议：先修回路，再修功能

> **执行进度（截至 2026-07-04）**：三个 P0 已全部修复合并——G-1（PR #39，含本项目首个
> 跨进程契约测试）、G-2（PR #40）、G-3（PR #41）。第 4 步"文档纠偏"进行中（本次同步：
> 回填 design §4.7/§5.3.2/§6.5、修正 CLAUDE.md 定性与当前阶段）。**未做**：第 1 步的
> OpenAPI/契约单一权威治理（G-8/G-9 仍在 backlog）。第 3 步空心功能 G-4（PR #43）、
> G-5（PR #44）已修复。

不建议继续按 T3-3、T3-4… 线性往下推——那只会制造第四次 FIX 循环。建议插入一个
**"止血 + 建立可信判据"的收尾冲刺**，顺序如下：

### 第 1 步：建立"契约单一权威"（半天，最高杠杆）
- 把 G-8/G-9 从 P3 提到现在做。选 proto + 一份 OpenAPI（或直接 CP handler 的 struct tag）
  作为唯一权威，SDK/WebUI 从中生成或对照。
- 立刻消灭 G-1（refresh 契约）：二选一统一 body 或 header，写进契约，加一个横跨
  SDK↔CP 的契约测试（不 mock CP，真起进程）。这类测试就是回路 1 缺的那块。

### 第 2 步：清掉 3 个 P0（1 天）
- G-1 已在第 1 步处理；G-2 决策 agent token 用长 TTL（如复用 720h refresh 语义）
  或加自动续期，并让 Agent 重连失败时能重新走注册兜底；G-3 加 webhook 密钥校验。

### 第 3 步：填实"空心功能"（2~3 天）
- G-4 心跳载荷是最高性价比单点：填上 queue_depth/version/disks，一次性点亮 UI 状态 + 监控。
- G-5 补一个 `/api/v1/stats/dashboard` 聚合端点（设计也要补），前端接真实数字。
- G-6/G-7 视精力：queue_max_size 是数据安全项建议做；Prometheus 可整体推迟但要在
  backlog 明确"设计第九章 = 未实现"，别让它继续伪装成已完成。

### 第 4 步：文档纠偏（半天）
- 修正 CLAUDE.md：明确"代码/DECISIONS.md 为契约权威，system-design.md 为背景设计（可能滞后）"。
- 回填 G-11/G-12/G-13，统一任务状态。删除/忽略 G-15 凭据文件。

### 关于"设计不满足实际需求"
你的第二类痛点（返工重设计）集中在 **Dashboard 统计（G-5）** 和 **路径模板（已做过 T3-4/5）**。
共性是**设计只画了 UI/给了字段，没定义支撑它的 API/语义**。建议今后新功能的"设计完成"
判据加一条：**UI 稿必须对应到已定义的 REST 端点**，否则视为设计未完成——从源头堵住这类返工。

---

## 四、一句话结论

代码底子是好的，主链路是通的。当前不是"实现难题"，而是**工程反馈系统的问题**：
补上"跨进程契约测试 + 契约单一权威 + 诚实的完成判据"这三样，
三个 P0 + 心跳/Dashboard 两个空心功能修掉，僵局即可打破，T3-3 之后的推进会顺畅得多。

---

## 五、冲刺回顾（2026-07-04 收尾）

**P0 与 P1 差异已全部修复合并。** 从"每处看似完成实则空心"的僵局，到主链路契约、
agent 生命周期、端点鉴权、心跳遥测、Dashboard 真实数据全部落地并有测试兜底。

| PR | 差异 | 修复要点 | 决策 |
|----|------|---------|------|
| #39 | G-1 refresh 契约断裂 | body 契约 + 令牌轮转；**本项目首个跨进程契约测试** | D-012 |
| #40 | G-2 Agent token 定时炸弹 | 长效 token（`AGENT_TOKEN_TTL`）+ 重连自愈 | D-013 |
| #41 | G-3 minio-event 无鉴权 | 共享密钥（哈希定长比较）+ fail-closed | D-014 |
| #42 | 文档漂移 | 回填 design §4.7/§5.3.2/§6.5；纠正"design=权威"定性 | — |
| #43 | G-4 心跳载荷全空 | 填 queue_depth/uptime/version → Redis 快照 → agents API | D-015 |
| #44 | G-5 Dashboard 假数据 | 新增 `GET /api/v1/stats/dashboard` 服务端聚合 | D-016 |

**兑现的方法论（三条回路修复）**：
1. **跨进程契约测试**：G-1 引入了不 mock CP 的真实进程测试，堵住"单测全 mock、契约漂移不可见"。
2. **文档权威归位**：PR #42 把 `system-design.md` 从"唯一权威"降为"架构背景"，确立
   代码 + DECISIONS 为契约真相（CLAUDE.md 已写明层级）。
3. **诚实的完成判据**：G-4/G-5 明确列出"暂缓项"（disks/upload_bps/Prometheus/物理用量），
   不再让空心功能伪装成已完成；并确立"UI 稿必须对应已定义 REST 端点才算设计完成"（D-016）。

**每个 PR 都过了 Copilot 多轮 review**——多轮追问逼出了若干第一轮想当然处（如 G-1 的
revoke 可观测降级、G-5 的 UTC 分桶、时序侧信道），这些都已修入。

### 剩余（P2/P3，非阻塞，可从容排期）
- **G-8/G-9**：契约单一权威 / OpenAPI 自动校验（防第三次契约错位；下一个契约引爆点是 T3-3 SDK 联调）。
- **结构性文档重构**：design 去重指针化、CLAUDE.md 减肥（等代码稳定后做，避免文档追着动的代码跑）。
- **监控**：Prometheus 指标导出（T4-1）；Agent `disks`/`upload_bps` 遥测；存储物理用量（`madmin.BucketUsageInfo`）。
- **文档欠账**：附录 C.1 `JWT_ACCESS_TTL` 环境变量名笔误（实际 `JWT_ACCESS_TOKEN_TTL`）。

**主线**回到 T3-3（Python SDK + Control Plane 联调）——建议在其之前先做 G-8（契约测试覆盖 SDK↔CP），
避免重演 T3-2-FIX 那轮契约错位。
