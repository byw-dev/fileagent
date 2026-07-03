# 05 — 跨模块契约与文档一致性

> 本章回答第三个问题："文档与代码脱节在哪"。
> 结论先行：**设计文档已不再是权威来源，但 CLAUDE.md 仍宣称它是**；
> 契约的真实权威已经迁移到 DECISIONS.md + 代码本身，且存在三处文档互相矛盾。

---

## 1. 设计文档自身矛盾（写下时就不一致）

| # | 位置 | 矛盾 |
|---|------|------|
| S-1 | §5.3.2 vs 附录 B.2 | 认证路径 `/api/auth/*` vs `/api/v1/auth/*`。实现遵循前者。B.2 需订正 |
| S-2 | §4.4.2 vs §6.2 | 路径模板变量：`{yyyy}{yy}{mm}{dd}{HH}{MM}` vs `{yyyy}...{agent}{filename}{stem}{ext}`，两份变量表不一致；且均已被 trollsift 语法（D-010）取代 |
| S-3 | §5.2 vs 实现语义 | "Redis TTL 过期→判定离线→触发事件"描述的是 TTL 驱动模型，但同章伪码只在 Connect/断流时写状态，设计内部就没说清谁负责 TTL 过期后的动作 |

## 2. 设计文档 vs 代码的漂移（设计已过时、未回填）

| # | 设计章节 | 现状 |
|---|---------|------|
| C-1 | §3.3.6 collection_rules DDL | 字段已按 D-009 重命名 + 删列（migration 000003），设计未回填 |
| C-2 | §4.3 完整 proto | CollectionRule 字段名/编号已重排、新增 dry_run 与 DryRunResult；设计中的 proto 与 `proto/v1/agent.proto` 是两个版本 |
| C-3 | §4.4.2/§6.2/§7.3.2 | 路径模板语法已换成 trollsift，三处描述全部过时 |
| C-4 | §5.11 REST 清单 | 新增 `POST /agents/:id/test-rule`（T3-6）未回填；缺 Dashboard 统计端点（见 03 报告） |
| C-5 | §4.7 | Agent JWT "剩余<20% 通过 gRPC 续期" ——协议里根本没有 token 续期 RPC，实现走"重启时重注册"路径。设计与实现两边都有问题（见 06 报告 E-1） |
| C-6 | 附录 C.1 | `AGENT_TOKEN_TTL=720h` 配置项在 CP 中不存在，agent token 实际接的是 `JWT_ACCESS_TTL`（2h） |

## 3. CLAUDE.md / 任务文档互相矛盾

| # | 文件 | 陈述 | 事实 |
|---|------|------|------|
| M-1 | CLAUDE.md「当前阶段」 | "T3-2 进行中" | T3-2 已完成，当前是 T3-3 |
| M-2 | phase-3.md「当前优先」 | "T3-6 ⬜" | 同文件下方表格和 active.md 都说 T3-6 ✅ |
| M-3 | CLAUDE.md 契约表 | proto "只增字段，不改字段编号；不删除字段" | D-009 已重排编号并删字段（有决策记录，但 CLAUDE.md 规则未更新，等于规则形同虚设） |
| M-4 | CLAUDE.md | "详细设计见 system-design.md（权威来源）" | 权威已事实迁移至 DECISIONS.md D-007~D-011 + 代码；新 Agent 按 CLAUDE.md 指引先读设计文档，会学到错误契约 |

## 4. 无文档的"口头契约"（复发风险最高）

| # | 契约 | 现居住地 |
|---|------|---------|
| V-1 | 前端 status 大写（RUNNING/PENDING...）↔ DB 小写（online/pending...）双向映射 | 仅存在于 `agents.go:130-148` 代码中 |
| V-2 | 列表响应信封 `{items,total,next_cursor,has_more}`（T3-2-FIX 统一） | DECISIONS.md D-007 有部分记录，设计 §8.5 只有 files 一例 |
| V-3 | trollsift 模板变量清单（SYSTEM_TEMPLATE_VARIABLES） | webui `pathTemplate.ts` 与 pkg/trollsift 各自维护，无单一权威表 |
| V-4 | REST 错误响应格式 | 设计 §5.11 定义了 `{error:{code,...},request_id}`，实际格式未验证、无契约测试 |

## 5. 结构性判断

T3-2-FIX 爆出 12 项契约错位、T3-5 又做一轮四层字段统一——**同一类病灶发作了两次**，
而防复发手段（OpenAPI schema 校验、MSW 契约测试）在 backlog 里躺着标 P3。
只要契约仍靠人（或 AI）手工在 4 层之间同步，第三次发作是时间问题；
Python SDK（第 5 层）联调即为下一个引爆点。

**建议**（详见 07 总结）：把"契约单一权威 + 自动校验"从 P3 提为 Phase 3 收尾的准入条件，
先于 T3-3 执行。
