# backlog.md — 待规划任务

> 本文件记录尚未排期的低优先级任务。Agent 无需读取，仅供人工规划参考。

---

## Phase 4 — 完善与收尾（可并行）

**前置依赖**：Phase 3 全部完成

| ID | 任务 | 内容摘要 | 状态 |
|----|------|---------|------|
| T4-1 | 监控配置 | Prometheus 抓取端点 + Grafana Dashboard 模板 + 告警规则 + Loki/Promtail 日志采集 | ⬜ |
| T4-2 | CI 配置 | GitHub Actions Workflows：controlplane / agent / webui / sdk-python 四条流水线（lint + test + build） | ◐ 部分：controlplane 已有 `ci-cp.yml`；agent 已交付 `ci-agent.yml`（build + vet + test，ubuntu/windows 矩阵，见 PR chore/t4-2-agent-ci）；webui / sdk-python 仍缺 |
| T4-3 | 部署脚本与文档 | 迁移嵌入二进制（D-023）；Dockerfile + prod compose + Caddyfile；systemd service 文件；Windows NSSM 脚本；README 生产部署段；`docs/ops/` 部署+运维手册 | ✅ |
| T4-4 | Java SDK | OkHttp3 + Jackson + Lombok；JUnit 5 + MockWebServer；功能对齐 Python SDK | ⬜ |
| T4-5 | Agent 重命名 | 管理员可在 Web UI 为采集器设置自定义显示名 | ✅ 已作为 CC-8 交付（PR #58 / D-021） |
| T4-6 | 采集规则原地编辑 | 规则列表编辑入口 + 复用三步 Wizard 回填 + 后端 PUT 全字段更新 | ✅ 已作为 CC-9 交付（PR #56–#57 / D-020） |

---

## T4-5 — Agent 重命名 ✅ 已交付（CC-8）

已作为 **CC-8** 实现并合并：`PATCH /api/v1/agents/:id`（super_admin）+ sqlc `UpdateAgentName` + webui 详情页
重命名 Modal。见 PR **#58** / **D-021** 与 `docs/tasks/core-completeness.md` CC-8 行。原详细规格已随实现落地，不再在此维护。

---

## T4-6 — 采集规则原地编辑 ✅ 已交付（CC-9）

已作为 **CC-9** 实现并合并：`PUT .../rules/{rid}` 全字段双形态更新（含 active 热重载）+ webui RuleForm edit 模式。
见 PR **#56–#57** / **D-020** 与 `docs/tasks/core-completeness.md` CC-9 行。原详细规格已随实现落地，不再在此维护。

---

## Follow-up：生产 MinIO 经网关反代暴露（TLS）

**来源**：PR #61（T4-3）review。

**✅ 已完成（D-024）**：internal/public endpoint 拆分——`MINIO_PUBLIC_ENDPOINT` /
`MINIO_PUBLIC_USE_SSL`（缺省回落内网值），`STSManager.WithPublicEndpoint`，`main.go` 独立 presign
client；prod compose 固定 `MINIO_ENDPOINT=minio:9000`（内网）+ 必填 `MINIO_PUBLIC_ENDPOINT`。
CP↔MinIO 不再 hairpin。

**残留（未做，gateway 特定）**：生产把 MinIO 置于 TLS 网关之后（不直发宿主机），`MINIO_PUBLIC_ENDPOINT`
指向网关地址。这是 gateway 特定改动（网关未必是 Caddy），端点拆分已就绪、gateway 无关地支持任意终结代理；
待生产网关选型确定后落地（`deploy/caddy/Caddyfile` 加 `minio.<domain>` 反代站点或等价物）。

---

## 前端重做实现（Half A）→ ⏸️ 已暂停（WR-1 已合并，WR-2…10 让位元数据 track）

**来源**：Claude Design 项目「前端页面重做计划」（2026-07-07，登录态私有）。设计规范：
[`docs/design/webui-redesign.md`](../design/webui-redesign.md)。**性质**：纯前端（webui），无后端契约改动。

**WR-1 地基已合并**（PR #66/#67：主题 token / 暗色闪烁修复 / 固定侧栏 / `StatusBadge` / 时间 util / 共享外壳），
被元数据 track 的 MT-6 复用。**WR-2…WR-10 于 2026-07-10 暂停**（价值优先：先建元数据核心，见
[`metadata-phase1.md`](metadata-phase1.md)）。分期、验收清单、恢复方法均在权威追踪文件
[`webui-redesign-impl.md`](webui-redesign-impl.md)。实施采用**在现有基线上改造（非重写）**。

## 元数据 / 标签 / 数据集能力（epic · 已拍板 6c，D-025）→ Phase 1 已排期，见专项追踪文件

**来源**：Claude Design round 6–7；已拍板混合模型 `6c`（D-025 + 2026-07-10 补充）。**权威设计**：
[`docs/design/metadata-model.md`](../design/metadata-model.md)（Phase 1 工程设计 + Phase 2 留存）。

**Phase 1 · 受控标签**已从"待规划"升级为**当前选定 track**（2026-07-10 价值优先决策，WR-2…10 暂停让位）：
任务拆分 **MT-1…MT-6**、验收要点、执行纪律均落于权威追踪文件
[`metadata-phase1.md`](metadata-phase1.md)。起手 MT-1+MT-2 薄纵切。

**Phase 2 · 数据集注册表 + 衍生数据入口 + 血缘 run 模型（deferred）** —— 薄层不动文件表。
**设计已完整留存**于 `metadata-model.md` P2.1–P2.3（2026-07-10 修订：血缘从数据集级改为 run 模型；
衍生数据 = 未来 ETL 经 SDK/CP 注册，禁止直连 MinIO）；出现对账/SDK 订阅需求或 ETL 起建时再起，届时补
D- 子决策与迁移。

---

## 已知技术债（低优先级，Phase 4 可处理）

| 描述 | 来源 | 优先级 |
|------|------|--------|
| `auth.go OIDCCallback` 始终返回 501（设计文档标注为"预留扩展"，可接受） | phase3-readiness-audit.md B-7 | ⚪ P3 |
| `append_mode=tail` 断点续传的 SQLite schema migration（当前直接修改 schema const） | P3-P4 | ⚪ P3 |
| 前后端 JSON 契约应引入 OpenAPI spec 自动校验，避免再次出现 T3-2-FIX 类问题 | T3-2-FIX 根本原因复盘 | ⚪ P3 |
| Web UI 页面使用 MSW（Mock Service Worker）补充真实 API 格式的集成测试 | T3-2-FIX 根本原因复盘 | ⚪ P3 |

---

## file_types glob 规则管理（后端 + 前端）

**来源**：WR-2 落地（2026-07-14）发现——mockup 的 file_types「glob 规则编辑器（可排序 + 优先级 + 试匹配，3c）」
**无后端支撑**：`file_type_rules` 表仅被 indexer classifier 只读（`ListFileTypeRules`），靠迁移/种子存在，无任何 REST 端点；
WR 禁改后端，故从 WR-2 拆出。

**内容**（真有「配兜底分类规则」需求时再做）：
- CP：`file_type_rules` CRUD 端点（按 org，绑定 file_type，pattern/priority）+「试匹配」端点（给定 storage path 返回命中类型）+ sqlc 查询/迁移。
- webui：file_types 详情/编辑抽屉内嵌 glob 规则编辑器（可排序拖拽 + 优先级 + 试匹配预览）。

**优先级**：⚪ 低。D-025 已把 `file_types` 降级为**兜底粗分类**，变种维度走标签；兜底 glob 规则很少变、可迁移/种子管理，
UI 现价值低。触发信号：出现「需在 UI 配/调兜底 glob 规则」的真实需求。

---

## 文件列表 filename / 日期范围 服务端过滤（后端 + 前端）

**来源**：WR-4 落地（2026-07-17）发现——文件页一直提供「文件名搜索」+「日期范围」筛选控件，但 `GET /api/v1/files`
的 `RejectUnknownQuery` 允许清单只有 `cursor/limit/agent_id/bucket_id/file_type_id/status/tag`（`files.go:132`），
发 `filename`/`since`/`until` 会 **400**（预存 bug，非 WR-4 引入）。WR-4 已改为**不发这些参数、在已加载页（limit 100，
status/tag 服务端预筛后）内客户端过滤**，止血且不 400。

**内容**（需在大数据量下跨 100 条精确搜索时再做）：
- CP：`files.go` List 允许清单加 `filename`/`since`/`until`；`ListFileEntriesParams`/`CountFileEntriesFilter` 加字段；
  list+count SQL 加 `filename ILIKE` + `created_at`（或 `uploaded_at`）范围（参考 D-028 status 过滤同款做法）。
- webui：`Files/index.tsx` request 改回把 filename/date 交服务端，去掉客户端缓存过滤。

**优先级**：⚪ 低（当前列表 limit 100 + 客户端分页，客户端过滤与 UI 展示范围一致；无真实大数据集）。触发信号：单类目文件 > 100
且需按名/日期精确检索。

---

## 抽取 + 加固共享 formatBytes（跨页去重）

**来源**：WR-4 评审（2026-07-17）第七轮——`formatBytes` 在 ~6 个页面（Dashboard / Logs / Files/index /
FileDetailDrawer / Agents/Detail / Agents/Logs）各自复制；单位表 `['B','KB','MB','GB','TB']` 只到 TB，
理论上 ≥1PB 会输出 `X undefined`，且未防非有限/负输入。

**现状判断**：`size`/`bytes_transferred` 均为后端非负有限 int64，边缘采集单文件 ≥1PB 不现实，故当前无实际影响。

**内容**（若做 UI 一致性清理时）：抽 `src/utils/formatBytes.ts`（clamp 单位下标到最后一档 + 非有限/负值回退 `-`），
6 处改为引用，去掉重复定义。

**优先级**：⚪ 低（纯健壮性/去重，无真实触发场景）。

---

## `agent/cmd/agent` 包的覆盖率欠账（历史，非本刀引入）

**来源**：IC-2a 收尾（2026-09-11）量覆盖率时确认——agent 模块整体 **66.0%（master 基线）/ 67.4%（IC-2a 后）**，
低于 `CLAUDE.md` 要求的 80%。缺口集中在 **`agent/cmd/agent/main.go`（753 行）**：进程装配、信号处理、
gRPC 长连接主循环、凭据刷新 goroutine 这些「只有跑起来才走得到」的路径，几乎没有单测。
**这不是 IC-2a 引入的**——IC-2a 让该模块从 66.0% 升到 67.4%，是净改善。

**为什么当时没顺手补**：补它意味着把 `main.go` 拆成可注入的小单元，那是一次独立的重构，
会静默把一把已经很大的原子刀撑得更大（PR 评审纪律不允许）。

**内容**（真做时）：把 `main.go` 的装配逻辑按职责拆出可测单元（凭据刷新时序、uploadFn 组装、
规则同步回调、优雅退出），保留 `main()` 作为薄壳；补齐单测到 80%。
**注意**：`buildLogger` 附近还有 **IC-BUG-38**（`log.output`/`max_size_mb`/`max_backups` 收了不用），
两件事碰同一块代码，宜同刀。

**优先级**：⚪ 低。数字欠账不影响正确性，且 IC track 的每一刀都在补真实回归（live e2e + 变异矩阵），
比把 `main.go` 的装配代码硬凑进单测更有价值。触发信号：`main.go` 因别的原因需要重构时顺带做掉。

---

## 部署脚本的自动化测试（shellcheck + 行为矩阵）

**来源**：PR #97（IC-BUG-36/35）的 code review（2026-09-11）暴露——`deploy/` 下的脚本**没有任何 CI 覆盖**
（`ci-cp.yml` 只在 `controlplane/**`/`tools/**`/`Makefile` 变更时触发）。该轮评审在一个 250 行的 bash 脚本里
挖出四条必修，其中两条会让「全新环境 bootstrap 跑不完」——**全部是人工实跑才发现的**。

**⚠️ 形态上有一个反直觉的约束，先想清楚再动手**：不要专门造一个装满 `mc`/`curl`/`shellcheck` 的「测试工具镜像」。
MF-1 的本质就是**「文档教运维用的那个镜像里没有 curl」**；如果测试 runner 什么都有，CI 会全绿而运维照样卡死——
把刚修的坑换个姿势又挖一遍。**行为测试的 runner 必须就是部署文档教人用的那个镜像**
（当前是已 pin 的 `minio/minio:RELEASE.2025-04-22T22-12-26Z`，产品决定不解 pin，作为测试基线反而稳），
这样测试顺带回答了「我们文档教的那条路今天还能跑吗」。

**拆成两件，它们需要的东西不一样**：

1. **shellcheck（静态 lint，不需要 MinIO）**：容器跑，不依赖宿主机——
   `docker run --rm -v "$PWD:/mnt" koalaman/shellcheck-alpine shellcheck deploy/scripts/*.sh`。成本几分钟。
2. **行为矩阵（真跑脚本）**：加在 `deploy/docker-compose.test.yml`，用 `profiles: ["tools"]` 门控成
   `docker compose run --rm` 的一次性 runner，不要做成常驻服务。

**用例矩阵**（都是 PR #97 那轮「先红后绿」验过的场景，直接沉淀成回归，否则测试会退化成「跑一遍没报错」）：

- 干净环境 bootstrap → exit 0；连跑两次仍 exit 0（幂等）
- 换个 secret 重跑且**不带** `CP_ADMIN_ROTATE=1` → **exit 1 且原凭据仍可用**（MF-3）
- `CP_ADMIN_ROTATE=1` → 确实轮换，且结论里明说
- policy 里删掉 `s3:CreateBucket` → **脚本必须 exit 非零**（S-1；修之前是 exit 0）
- **svcacct 负向对照** → `AssumeRole` 被拒（IC-BUG-36 的看门狗，防止哪天有人「顺手简化」回 service account）
- 21 字符 access key → 在**任何集群变更之前**退出
- **非 localhost 端点**跑通（MF-2 的那一半）

**⚠️ 有一类 CI 抓不到，别假装抓得到**：MF-2（`set -u` 下展开空数组）**只在 bash 3.2 复现**，
Linux CI 是 bash 5，**跑绿也证明不了任何事**。GitHub 的 macOS runner 确实是 `/bin/bash` 3.2，
但那上面没有 Docker，起不了 MinIO，两个条件凑不到一起。所以这一类只能靠 lint + 约定兜：
先实测 shellcheck 会不会报（它不建模 bash 版本差异，**不确定**），兜底是加一条 grep 断言
「脚本里所有数组展开必须写成 `${arr[@]+"${arr[@]}"}`」。笨，但诚实——比一个在 bash 5 上永远绿的行为测试有用。

**落地形态**：`deploy/scripts/test-init-minio.sh`（矩阵）+ compose 的 tools profile +
`.github/workflows/ci-deploy.yml`（`paths: deploy/**`，跑 shellcheck + 矩阵）。

**优先级**：🟡 中。不是纯健壮性——`deploy/` 是**唯一一处「改坏了单测和 live e2e 都不会红」的地方**，
而它恰恰决定新环境能不能起来。**排期**：产品已定在 PR #97 + #98 合并之后开工——**两者均已于 2026-09-11 合并（`13eb730` / `91c1628`），条件已满足，可开工**。

---

## proto 布局规范化（包名 / 目录 / 生成落点对齐）

**来源**：PR #102（D-032，protoc → buf）的实测取舍。**不是缺陷，是布局债**——今天没有任何症状。

**现状**：`proto/v1/agent.proto` 里 `package fileagent.v1`，而文件在 `proto/v1/`；生成物落在 `api/v1/`。
包名与目录不匹配，因此 `buf.yaml` 长期豁免 `PACKAGE_DIRECTORY_MATCH`。buf 的标准布局要求
`proto/fileagent/v1/agent.proto`。另因源目录（`proto/`）与产物目录（`api/`）不同，
`make generate-proto` 需要两行 `mv` 把产物从 scratch 目录搬回 `api/v1/`。

**⚠️ 已实测否决的「省事」改法**（别再试一遍）：把 buf 模块根设成 `proto/`、`out: api`
确实能一步到位、去掉 `mv`，但代价是 **202 行生成物 diff**——
`// source:` 从 `proto/v1/agent.proto` 变成 `v1/agent.proto`，
连带满篇 `file_proto_v1_agent_proto_*` → `file_v1_agent_proto_*` 符号重命名。
那个 `// source:` **不是注释，是描述符的文件名、protobuf 全局注册表的键**，改它是真实语义变更。
而且**它并不能消掉 lint 豁免**（实测：包名仍是 `fileagent.v1`、目录仍是 `v1`，照样报
`must be within a directory "fileagent/v1"`）。**用 2 行 shell 换 202 行 diff + 注册表键变更，纯亏。**

**真正的解**：`proto/fileagent/v1/agent.proto` + `go_package` 指向 `api/fileagent/v1` +
两个模块的 import path 全改。一次跨模块重构。

**优先级**：🔵 **非常低**。今天零症状，收益只有「少两条 lint 豁免 + 少两行 mv」。
**触发条件**：等 proto 真的要拆多文件时再做——例如 IC-9 的 `files/register` 契约、
IC-14 的 lineage。那时布局问题才开始真的疼，跟拆文件一起做才划算。
**在此之前不要单独为它开刀。**
