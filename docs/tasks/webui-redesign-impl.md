# webui-redesign-impl.md — Web UI 重做实现（Half A）

> **性质**：纯前端（`webui/`），**无后端契约改动**——统一既有页面的视觉与交互。
> **设计权威**：[`docs/design/webui-redesign.md`](../design/webui-redesign.md)（token / 7 条交互定则 / 分页意图 §1–§5）。
> **隐性契约**：[`docs/design/contracts.md`](../design/contracts.md) **V-1 状态枚举映射**（前端大写、DB `online`⇄前端 `RUNNING`）。
> **实施方式**：**在现有代码基线上改造，不从零重写**（详见文末「决策」）。任务用 track 本地前缀 `WR-x`；每个 WR-x 独立分支/PR off master + review + 人工合并。
> **Half B（元数据/数据集 6c）不在本 track**，见 [`backlog.md`](backlog.md) 与 [`metadata-model.md`](../design/metadata-model.md)。

---

## 背景：为什么做这个 track

重做规范此前"只落文档、未落代码"。曾有一版在远端环境改出，**实机核验（Chrome DevTools）结论是"皮对骨错"**：
视觉 token 基本到位，但**抽屉化增删改查完全没做、状态徽标全站三套渲染、时间格式全错**，且带一个**暗色主题闪烁 bug**。
故决定忽略那一版，在本仓库 `webui/` 从当前基线按规范正确落地。核验发现构成下方**验收清单来源**。

### 远端版核验发现（= 本 track 验收标准）
| 级别 | 发现 | 规范条款 |
|------|------|---------|
| P0 | 新建/编辑/详情**全是整页路由**，无一抽屉 | 定则 1 |
| P1 | 状态徽标三套并存（采集器中文实心药丸 / 文件·日志英文大写描边），非「圆点+文字」全站唯一映射 | §1.2 |
| P1 | 时间处处 `YYYY/M/D HH:mm:ss`，应 `MM-DD HH:mm` + 心跳用相对时间 | 定则 5 |
| P1 | 侧栏可折叠（应 208px 固定不可折叠） | §1.4 / §3 |
| P1 | 仪表盘统计卡硬编码 `#1677FF`/`#52C41A`，绕过 token | §1.1 |
| P2 | 空态英文 `No data` 不统一；无"一句话+主操作"设计空态 | 定则 6 |
| P2 | 危险确认用 AntD 默认英文 OK/Cancel + 红描边（应红实心主按钮 + 后果 + 高破坏面输入名确认） | 定则 2 / 4f |
| P2 | 表格操作列左对齐（应右对齐、≤3 链接 +「···」） | 定则 4 |
| P2 | 日志状态筛选是下拉（应 chip 形态）；失败行无重试轨迹展开 | 4e |
| P2 | 文件类型创建缺 glob 规则编辑器（可排序+优先级+试匹配） | 3c |
| P2 | 采集器用 Tab 而非「待审批置顶横幅」 | 1c |
| Bug | 文件"上传时间"在 `uploaded_at` 为空时显示 `Invalid Date` | — |
| Bug | 同一采集器：列表显示"在线"、详情显示"离线"（状态口径不一致） | — |

---

## 当前本地基线速览（webui/）
- `src/App.tsx`：React Router v6 + `lazy`/`Suspense`；**Create/Detail 均为整页路由**。
- `src/main.tsx`：裸 `ConfigProvider locale={zhCN}`，**无 theme token**。
- `src/index.css`：**Vite 模板遗留**——`#root{width:1126px;text-align:center}`、`font:18px`、
  `color-scheme:light dark` + `@media(prefers-color-scheme:dark)` 暗色块（**暗色闪烁根因**，见下）。
- `src/layouts/BasicLayout.tsx`：ProLayout，带 `collapsed` 折叠开关。
- `src/components/`：`AgentStatusBadge`（AntD `Tag color=` 实心、仅 agent、大写枚举）、`BatchDownload`、`DirectoryTree`、`AuthGuard`。
- `src/services/*.ts`：8 个资源封装齐全（含 cursor 分页、契约映射）——**改造复用，不动**。
- 时间：~18 处散落 `new Date(v).toLocaleString('zh-CN')`，无共享 util。

### 暗色主题闪烁 · 根因与修复（Phase 0 内处理）
`index.css` 声明 `color-scheme: light dark` + 暗色块，`html/body/#root` 背景全透明。OS 暗色时浏览器把透明画布
渲染成深色 `#16171D`；懒加载切换白色内容卸载瞬间露出深色画布 → **白→黑→白**。
**修复**：`<html>` 强制 `color-scheme: light` + `html,body,#root { background:#F5F6F8 }`。
实机验证：`<html>` 计算背景 `rgb(22,23,29)` → `rgb(245,246,248)`。

---

## 任务拆分（每个 WR-x = 1 分支/PR，off master + review + 人工合并）

> 用 track 本地前缀 **`WR-x`**（WebUI Redesign，仿 core-completeness 的 `CC-x`），避开与全局 Phase（现 3/4）冲突。
> **WR-1 必须先合**（后续每页依赖）；**WR-2 文件类型为样板页，评审定型抽屉/徽标/时间模式后再铺开 WR-3…WR-9**。

### WR-1 — 地基 ⬜
全站单一事实来源，后续每页依赖：
- [ ] 主题 token：`src/main.tsx` 注入 `ConfigProvider theme={{ token, components }}`（§1.1 色彩 / §1.3 字号 / §1.4 尺寸）
- [ ] 重写 `src/index.css` + 暗色闪烁修复：删 Vite 模板；`color-scheme:light` + `html,body,#root{background:#F5F6F8}`；base 16px；去 `#root` 1126px 锁 & `text-align:center`。`App.tsx` PageLoader 给浅底 + 骨架
- [ ] 固定侧栏：`src/layouts/BasicLayout.tsx` 208px 不可折叠、去 `collapsed` 开关、内容区流式
- [ ] 统一徽标 `src/components/StatusBadge.tsx`（升级 `AgentStatusBadge`）：圆点+文字，覆盖 agent/file/upload 三域，按 contracts.md **V-1** 映射，同色同词
- [ ] 时间 util `src/utils/time.ts`（`formatTime`）：`MM-DD HH:mm` + tooltip 完整 + 相对心跳 + 空值安全（修 `Invalid Date`），替换 ~18 处 `toLocaleString`
- [ ] 共享外壳：`FormDrawer`(480px 承载新建/编辑)、`confirmDanger`(红实心+后果+可选输入名确认)、统一 `EmptyState`/内联错误/骨架

### WR-2 — 文件类型（样板页，评审定型） ⬜
用最简单 CRUD 把抽屉/徽标/时间模式跑通定型：
- [ ] `src/pages/FileTypes/index.tsx`：接徽标/时间/单行筛选/操作列右对齐
- [ ] 新建/编辑 → `FormDrawer`（删 `/file-types/create` 整页路由 + `Create.tsx`），glob 规则可排序 + 优先级 + 试匹配（3c）
- [ ] 删除确认弹窗 + 输入名称确认（4f）

### WR-3 — 采集器 ⬜
- [ ] 待审批置顶横幅(1c)、行内审批/吊销走 `confirmDanger`、采集规则三步**抽屉**(2a-2d，复用现有 `RuleForm` 内核 + dry-run)、详情 tabs 保留、**修列表/详情状态口径 bug**

### WR-4 — 文件 ⬜
- [ ] 勾选批量下载(复用 `BatchDownload`)、详情 480px 抽屉(3a，替代整页)、类型树变体(4g 可选)、**修 Invalid Date**

### WR-5 — 事件规则 ⬜
- [ ] 编辑抽屉(4b 动作二选一联动必填)、投递历史抽屉(4c dead 终态 / 失败行展开响应体)、行内启停

### WR-6 — 上传日志 ⬜
- [ ] 状态 chip 筛选(4e)、失败行内嵌错误 + 重试轨迹

### WR-7 — Bucket ⬜
- [ ] 登记视图、创建仅 super_admin(4d)、通知配置状态可见

### WR-8 — 设置 ⬜
- [ ] 用户管理 tab(5b super_admin，禁用而非删除)、新建用户抽屉(5c 初始密码一次性)、登录页(5a)核对

### WR-9 — 仪表盘 ⬜
- [ ] 去硬编码色、卡片 + 骨架、接服务端聚合(1b，端点已存在 · commit f6f659f)

### WR-10 — 收尾 ⬜
- [ ] 全站 Chrome DevTools 实机走查（见「验证」）
- [ ] Vitest 覆盖核心 store/service ≥ 80%
- [ ] 对照 `webui-redesign.md` 逐条核销；如有行为偏差同步 `system-design.md`

---

## 验证（每阶段收尾执行）
- `pnpm --dir webui dev` 起本地；CP 在 `:8080`（`deploy/config/controlplane.env`，admin/见 `bootstrap_admin_credentials.txt`）。
- 库中已有种子数据便于走查：**4 采集器**（online/offline/pending/revoked）、**4 文件**（completed×2/failed/uploading）、
  **2 上传日志**（completed/failed 带 error+retry）、**2 事件规则**（webhook 启用 / nats_publish 停用）、**2 文件类型**（各带 glob）。
- **暗色模式下**实机走查：无白→黑→白闪烁；抽屉承载增删改；徽标全站同色同词圆点+文字；时间 `MM-DD HH:mm`+tooltip；
  空态"一句话+主操作"；危险操作红实心确认。
- `pnpm --dir webui test`（Vitest + RTL）。

---

## 决策：为何改造而非重写
贵/难/已调对的部分全在**非表现层**（`services/*`、契约映射 V-1、三步向导 + dry-run、批量下载、auth/token 刷新、
cursor 分页）——重做**不动**它们。重写等于把这些全部重来、重踩 git log 里一长串已修 bug
（`createRuleRequest 字段不匹配→400`、`Modal is not defined`、`auth 响应契约`、`pathTemplate 重设计` 等）。
**改造是重写的子集，且少了重踩契约坑的风险。** 页面按文件清晰拆分 + service 层分离 + 懒加载路由，可外壳换、内核留。
