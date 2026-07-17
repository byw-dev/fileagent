# webui-redesign-impl.md — Web UI 重做实现（Half A）

> **▶️ 状态（2026-07-14）：恢复 WR track。** 元数据 6c Phase 1（MT-1…6，PR #69–#79）已收官，恢复本 track。
> WR-1 地基已合并（PR #66/#67），其产出（token / `StatusBadge` / 时间 util / `FormDrawer` 等共享外壳）已被元数据
> track 复用。恢复前做了一次**规格校准**（本次更新）：Phase 1 的 4 个元数据屏（7a–7d）改动/新增了 WR-3/4/8
> 触及的页面，故修订这三项（见下方各任务与「规格校准记录」），WR-2/5/6/7/9/10 基本原样。
> **恢复方法**：按「从 mockup 回补规范再落码」教训执行——`webui-redesign.md` 是 mockup 的有损摘要，
> 实现每页前先从本地 mockup HTML（`docs/design/mockups/project/*.html`，gitignore·仅本地）提取精确样式回填规范，再写代码。
> WR-2 一轮未提交的 mock 尝试已回退干净。
>
> **▸ 建议实现顺序**：WR-2（样板页定型）→ WR-9（仪表盘，后端已就绪）→ WR-5/6/7（Phase-1 未触及）→
> WR-3（+7a 润色）→ WR-4（+7b）→ WR-8（+7c/7d）→ WR-10（收尾）。
>
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

### WR-1 — 地基 ✅（已合并 PR #66/#67）
全站单一事实来源，后续每页依赖：
- [x] 主题 token：`src/theme.ts` + `src/main.tsx` 注入 `ConfigProvider theme`（§1.1 色彩 / §1.3 字号 / §1.4 尺寸）
- [x] 重写 `src/index.css` + 暗色闪烁修复：删 Vite 模板；`color-scheme:light`（+ `index.html` meta）+ `html,body,#root{background:#F5F6F8}`；base 16px；去 `#root` 1126px 锁 & `text-align:center`。`App.tsx` PageLoader 给浅底
- [x] 固定侧栏：`src/layouts/BasicLayout.tsx` 208px 不可折叠（`collapsedButtonRender={false}`）、内容区流式
- [x] 统一徽标 `src/components/StatusBadge.tsx`（覆盖 agent/file/upload/rule/delivery，按 contracts.md **V-1** 大写映射）——**浅底泡泡 + 亮圆点 + 深调文字**（复刻 mockup，点色≠字色；见修正后的 §1.2 三元组表）；`AgentStatusBadge` 改为薄包装 → 采集器/仪表盘等既有页自动升级；含单测
- [x] **对 mockup 核验后追加的 token 微差**（PR #67 已回补规范）：`theme.ts` 加 `statusBadgeTones{bg,text,dot}`；`Table.headerBg #FAFBFC`、`Table.borderColor #EEF0F3` + `colorSplit #EEF0F3`（分隔线更浅）、`Menu.itemColor #4E5561`（导航文字）
- [x] 时间 util `src/utils/time.ts`（`formatTime`/`formatTimeFull`/`formatRelative`）+ `TimeText` 组件（`MM-DD HH:mm` + hover 完整 + 相对心跳 + 空值安全）**就绪，含单测**。各页 `toLocaleString` 调用点在**各自页面 WR** 接入（`Invalid Date` 修复随 WR-4 文件页落地）——避免地基 PR 反复触碰 15 个页面文件
- [x] 共享外壳：`FormDrawer`(480px)、`DangerConfirmModal`(红实心+后果+可选输入名确认 4f) + `useDangerConfirm` hook(简单危险确认)、`EmptyState`(一句话+主操作)

> **实机验证（暗色模式）**：`html/body/#root` 背景 `#F5F6F8`（`color-scheme:light`），**无白→黑→白闪烁**；侧栏 208px 无折叠开关；主色 `#2F6BE0`（Tab ink）；状态徽标为**浅底泡泡**（采集器页自动升级，逐色比对 mockup）；base 16px。build + 90 tests + lint（新文件）通过。

### WR-2 — 文件类型（样板页，评审定型） ✅（已合并 PR #82）
用最简单 CRUD 把抽屉/时间/筛选/危险确认范式跑通定型：
- [x] `src/pages/FileTypes/index.tsx`：`TimeText` 创建时间 / 单行名称筛选 / 操作列右对齐 / 空态 `EmptyState`
- [x] 新建/编辑 → `FormDrawer`（删 `/file-types/create` + `/file-types/:id` 整页路由 + `Create.tsx` + `Detail.tsx`；
      详情/新建/编辑全折进抽屉，名称点击即开编辑）
- [x] 删除 → `DangerConfirmModal` 红实心 + 输入名称确认（4f）
- 注：`file_types` 已按 D-025 降级为**兜底粗分类**（变种维度走标签，规则声明类型优先于 glob）；本页无状态字段，不涉及徽标。
- **范式（评审定型）**：表格内可点元素（名称/操作列）用 `Button type="link"`（`padding:0;height:auto` 保持链接外观）
      而非无 `href` 的 `<a onClick>`／`Typography.Link`——后者键盘不可聚焦（a11y）。**WR-3/WR-4 同步**：`Agents/index`·`Agents/Pending`·
      `Files/index` 的操作/名称列仍是 `<a onClick>`，在各自切片一并替换。
- **⚠️ glob 规则编辑器（原 3c）已从本项拆出** → `backlog.md`（**无后端**：`file_type_rules` 无 REST 端点，需先建
      CRUD + 试匹配端点，破坏 WR「纯前端」前提；且 glob 现为低价值兜底）。2026-07-14 用户拍板 descope + 后续按需再做。

### WR-3 — 采集器 ⬜（2026-07-14 校准）
- [ ] 待审批置顶横幅(1c)、行内审批/吊销走 `useDangerConfirm`、详情 tabs 保留、**修列表/详情状态口径 bug**
- [ ] **规则表单保持整页 4 步 `StepsForm`**（**不**抽屉化）——作为定则 1 的合理例外：Phase 1（MT-6d）已把它建成
      4 步整页（基本/源路径/上传路径/元数据，含 dry-run + 实时预览 + 动态列表），历 10 轮 review 稳定，复用现有 `RuleForm` 内核。
      本片只做 **WR token / `StatusBadge` / `TimeText` 润色** + 润色 **7a 元数据步**（静态标签/路径映射行的视觉与空态）。
      〔原规格「三步抽屉」已废——决策见「规格校准记录」〕

### WR-4 — 文件 ⬜（2026-07-14 校准）
- [ ] 勾选批量下载(复用 `BatchDownload`)、类型树变体(4g 可选)、**修 Invalid Date**（`Files/index.tsx:155` 裸 `toLocaleString` 换 `TimeText`）
- [ ] 文件详情 480px 抽屉(3a)——注：当前 `Files/Detail` **仅有路由无实现**，为**新建**（非「替代整页」）
- [ ] **对齐 Phase-1 标签 UI 到 WR 范式（7b）**：`Files/index.tsx` 现有标签列 / faceted 筛选（`TagFacetPicker`）/ 批量打标弹窗
      为「够用一致」建；本片对齐——标签列/筛选 chip 化统一、批量打标改 `FormDrawer`、空态/危险确认、token 色。**不回退**标签功能。

### WR-5 — 事件规则 ✅（本 PR）
- [x] 新建/编辑 → `RuleFormDrawer`（480px 抽屉，交互定则 1；删 `/events/create` 整页 + `Create.tsx`）；名称点击即开编辑
- [x] **4b 动作二选一联动必填**：动作配置不再裸 JSON——选 webhook 显 `url`（`type:url` 校验）、选 nats_publish 显 `subject`，`shouldUpdate` 联动切换必填
- [x] **4c 投递历史抽屉**：`DeliveriesDrawer`（删 `/events/:id/deliveries` 整页 + `Deliveries.tsx`）；状态走 `StatusBadge domain=delivery`（含终态 `dead`）；**failed/dead 行 `expandable` 展开** HTTP 状态 / 下次重试 / 响应体
- [x] 行内启停（`Switch` 直改，4a，原已具备保留）、删除 → `DangerConfirmModal`、创建时间 → `TimeText`
- [x] **小幅补后端字段（D-026，additive）**：投递响应追加 `response_code`/`response_body`/`next_retry_at`/`delivered_at`（数据已在 `EventDelivery` 模型，仅未投影；`omitempty` 向后兼容）——让 4c「展开响应体」成真功能而非空壳。**本项为 WR track 唯一后端触碰**，理由见 D-026
- 测试：`events-page.test.tsx`（列表/创建抽屉默认 webhook 字段/投递抽屉）+ Go `ListDeliveries_ExposesResponseFields`/`_OmitsAbsentResponseFields`

### WR-6 — 上传日志 ✅（本 PR）
- [x] **状态 chip 筛选(4e)**：下拉 `Select` → 单行 `CheckableTag` chip 行（全部/成功/失败，交互定则 3；「待处理」态不存在，见下 D-028）
- [x] 状态列 → `StatusBadge domain=upload`、上传时间 → `TimeText`
- [x] **失败行内嵌错误 + 重试轨迹**：`FAILED` 行 `expandable` 展开 错误信息 / 重试次数 / 已传输 X/Y / 开始→结束
- [x] **小幅补后端字段（D-027，additive）**：上传日志响应追加 `retry_count`/`bytes_transferred`/`started_at`/`finished_at`（数据已在 `UploadLog` 模型仅未投影）——与 D-026 同类，让 4e 重试轨迹成真功能
- [x] **状态过滤补齐 + taxonomy 修正（D-028）**：实测发现 chip 过滤「完全失效」——① 后端从未实现 status 过滤（List/Count 加 `status` 条件 + handler 读参 ToLower 归一）；② 前端取值错（成功=`SUCCESS`→改 `COMPLETED`，真实枚举 completed/failed）；③ 去掉不存在的「待处理」态（upload_logs 是终态记录）。清掉 WR-9 误加的 `StatusBadge` 假映射
- [x] 失败行展开对缺失字段优雅降级（`formatBytes` 防 `NaN undefined`）
- 测试：`logs-page.test.tsx`（chip 改 status 参数）+ Go `List_ExposesRetryTrail` / `List_StatusFilterNormalized` / `List_NoStatusFilter` / db 层 `*_WithStatusFilter`

### WR-7 — Bucket ✅（本 PR）
- [x] 登记视图：`Card>Table` → `ProTable`（read-only 列表）；名称/描述/创建时间；空态 `EmptyState`、时间 `TimeText`
- [x] 创建仅 super_admin(4d)：保留角色门控；新建 `Modal` → `FormDrawer`（480px，交互定则 1）+ `onFinish` 回车提交 + bucket 名校验
- [x] 服务层 `Bucket.description` 后端 `omitempty` → 改可选 `?: string`（如实匹配 JSON）
- **⚠️「通知配置状态可见」descope**：MinIO 事件通知是**部署级全局配置**（`notify_webhook` / `init-minio.sh`），CP 不按 bucket 存通知状态，`bucketResponse` 无相关字段——无后端数据，同 WR-2 glob 处理，注记于页面 JSDoc
- 测试：`buckets-page.test.tsx`（super_admin 见新建/开抽屉、非 super_admin 隐藏、列表请求）

### WR-8 — 设置 ⬜（2026-07-14 校准）
- [ ] 用户管理 tab(5b super_admin，禁用而非删除)、新建用户抽屉(5c 初始密码一次性)、登录页(5a)核对
- [ ] **7c 标签词表**（`Settings/TagKeys`）+ **7d 待确认取值**（`Settings/PendingTags`）润色到 WR 一致性：
      `StatusBadge`（来源/状态）/`TimeText`（首次出现）/token 色/空态一句话+主操作/危险确认（拒绝、删除键）；
      取值抽屉与 merge 弹窗对齐 `FormDrawer`/交互定则。〔均 Phase-1「够用一致」建，本片折入润色〕

### WR-9 — 仪表盘 ✅（本 PR）
- [x] 去硬编码色：统计卡强调色 + 折线 stroke + 网格线改走 `theme.useToken()`（`colorSuccess`/`colorPrimary`/`colorSplit`），不再 `#52c41a`/`#1677ff`
- [x] 骨架屏加载（定则 6）：四张统计卡 + 图表卡 + 采集器卡 + 日志卡改 `Card loading`（骨架），去掉 `Spin` 转圈
- [x] 套 WR 范式：日志状态列改 `StatusBadge`（`domain="upload"`，扩了 `SUCCESS`/upload `PENDING` 映射）、上传时间改 `TimeText`、三处空态改 `EmptyState`
- [x] 接服务端聚合(1b，端点已存在 · commit f6f659f)——WR-1 前既已接 `getDashboardStats`，本片保留
- 注：无 mockup 专屏，遵 `webui-redesign.md` §1.1 token + 定则 6。含 `dashboard-page.test.tsx` 冒烟（骨架→内容 + 聚合被调）

### WR-10 — 收尾 ⬜
- [ ] 全站 Chrome DevTools 实机走查（见「验证」）
- [ ] Vitest 覆盖核心 store/service ≥ 80%
- [ ] 对照 `webui-redesign.md` 逐条核销；如有行为偏差同步 `system-design.md`

---

## 规格校准记录（2026-07-14，恢复 track 前）

Phase 1（元数据 6c，MT-1…6，PR #69–#79）在 WR-2…10 暂停期间落了 4 个元数据屏（7a–7d），改动/新增了 WR-3/4/8
触及的页面。恢复本 track 前做一次校准（用户拍板）：

1. **规则表单保留整页 4 步**（修订 WR-3，废原「三步抽屉」）。理由：MT-6d 已把规则表单建成 4 步整页 `StepsForm`
   （含 dry-run + 实时路径预览 + `ProFormList` 动态列表），历 10 轮 review 稳定；复杂多步向导塞进 480px 抽屉体验更差、
   churn 大。定则 1（增删改走抽屉）对**复杂多步向导**放行例外——与文末「决策」一致（三步向导是要保留的非表现层资产）。
2. **元数据 4 屏顺手润色，折入 WR-3/4/8**（不新增 WR 项）：7a→WR-3、7b→WR-4、7c+7d→WR-8。理由：追求全站一致；
   4 屏为「够用一致」建（直接用 ProTable/ProFormList，标签色 ad-hoc、未全接 `TimeText`/`StatusBadge`），轻度对齐即可。
3. **其余不变**：WR-2（补 file_types 降级注记）、WR-5/6/7/9/10 规格原样。WR 仍**纯前端、无后端契约改动**
   （Phase 1 已加的 rule 响应 `metadata` 字段本 track 不涉及）。

## 验证（每阶段收尾执行）
- `pnpm --dir webui dev` 起本地；CP 在 `:8080`。CP 配置文件 `deploy/config/controlplane.env` 是 **gitignored**（不在仓库，需从 `controlplane/.env.example` 自建）。登录凭据：本地 dev 建议在该 env 里设固定 `BOOTSTRAP_ADMIN_PASSWORD` + `BOOTSTRAP_ADMIN_FORCE_RESET=true` 拿确定密码；否则首启生成随机密码写入 `BOOTSTRAP_ADMIN_CREDENTIALS_FILE` 指向的文件（默认是**进程工作目录相对**的 `bootstrap_admin_credentials.txt`，易因 CWD 不同而找错）。
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
