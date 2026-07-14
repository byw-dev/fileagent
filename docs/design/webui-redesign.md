# Web UI 重做 · 设计规范与分页意图（Half A）

> **视觉来源（权威原件）**：Claude Design 项目「前端页面重做计划」
> <https://claude.ai/design/p/ee358eac-8672-4a33-8b50-7dcd3d3bc119>
> （原件是登录态私有的可交互稿；未在仓库内留快照——其自包含导出体积超出导入上限会被截断损坏，本文的
> token / 定则 / 分页意图即从该稿提炼的可执行摘要。）
> **数据模型设计（Half B，另文）**：[`metadata-model.md`](./metadata-model.md)（已拍板 6c，D-025）
>
> **权威层级**（同 `CLAUDE.md`）：代码 > `DECISIONS.md` > `system-design.md`。本文件是**设计意图**，
> 用于指导实现，不覆盖既有契约（proto / migrations / handler 响应体 / REST 路径）。本轮**只落文档、不改代码**；
> 实现按 `docs/tasks/backlog.md` 的「前端重做实现」track 分期推进。

本轮重做覆盖 webui **既有页面**的视觉与交互统一（不新增后端能力）。新的「元数据 / 数据集」能力属于 Half B，
涉及 CP/DB/API 契约，见 `metadata-model.md`（已拍板 6c，分期实施，D-025）。

---

## 1. 设计 Token（对齐 AntD 5 `ConfigProvider` token — 多数全局 token 一一对应，少数为组件级 token）

现状：`webui/src/main.tsx` 仅传 `locale={zhCN}` 的裸 `ConfigProvider`，**无任何主题定制**。实现期应在此处注入
`theme={{ token, components }}`，使下列 token 成为全站单一事实来源。

### 1.1 色彩

| 语义 | 设计名 | 值 | AntD token |
|------|--------|----|-----------|
| 主色 | primary | `#2F6BE0` | `colorPrimary` |
| 主色浅底 | primary-bg | `#EBF1FD` | `colorPrimaryBg` |
| 主文本 | text-1 | `#1F2329` | `colorText` |
| 次要文本 | text-2 | `#646A73` | `colorTextSecondary` |
| 弱文本 / 占位 | text-3 | `#8F959E` | `colorTextTertiary` / `colorTextPlaceholder` |
| 边框 | border | `#E3E6EB` | `colorBorder` / `colorBorderSecondary` |
| 分隔线（比 border 更浅） | divider | `#EEF0F3` | `colorSplit`（表格行 / 卡片内 / 侧栏分区分隔） |
| 页面背景 | bg-page | `#F5F6F8` | `colorBgLayout` |
| 表头 / 淡底 | subtle-bg | `#FAFBFC` | `Table.headerBg`（组件级；只读浅底块亦用） |
| 导航文字（介于 text-1/2） | nav-text | `#4E5561` | `Menu.itemColor`（组件级） |

**状态语义色**（取自 mockup 调色板，用于状态徽标 / 结果提示）：成功 `#1B8A5A`（浅底 `#E8F6EF`）=
`colorSuccess`；警告 `#B27409`（浅底 `#FCF3E3`）= `colorWarning`；危险 `#D64545`（浅底 `#FBEDED`）=
`colorError`。

> **注**：徽标里的**圆点**取比文字**更亮**一档的同族色（见 §1.2 三元组表），与 AntD 语义 token（上面这三个，
> 用于按钮 / Alert 等）**解耦**——语义 token 用 §1.1 值，徽标用 §1.2 的 {底 / 文字 / 圆点} 三元组。

### 1.2 状态语义（全站唯一映射）

状态一律用**徽标**：**浅底泡泡 + 亮圆点 + 深调文字**（不是裸的圆点+文字——泡泡是 mockup 的渲染口径，
§1.1 的"浅底"即泡泡底）。同一状态在任何页面**同色、同词、同形**。既有 `AgentStatusBadge`
（`webui/src/components/AgentStatusBadge.tsx`）应升级为全站通用 `StatusBadge` 并统一取色。

**泡泡几何**（复刻 mockup 内联样式）：`display:inline-flex; align-items:center; gap:6px; height:22px;
padding:0 9px; border-radius:11px; font-size:0.75rem`；内层圆点 `width/height:6px; border-radius:50%`。

**每个 tone 的三色**（`底 bg` / `文字 text`，深调 / `圆点 dot`，亮调——**点色≠字色**）：

| tone | 底 bg | 文字 text | 圆点 dot | 用于（语义 slug） |
|------|-------|----------|---------|------------------|
| success | `#E8F6EF` | `#1B8A5A` | `#22A06B` | 在线 online / 成功 completed / 生效 active / 已投递 |
| neutral | `#F2F3F5` | `#646A73` | `#8F959E` | 离线 offline / 已删除 / 已停用 inactive |
| warning | `#FCF3E3` | `#B27409` | `#D98D0B` | 待审批 pending |
| error | `#FBEDED` | `#C03D3D` | `#D64545` | 已吊销 revoked / 失败 failed / 已终止 dead |
| processing | `#EBF1FD` | `#2F6BE0` | `#3D7BE8` | 上传中 uploading / 待投递 / 已审批 approved（processing 无 mockup 参考，按规律派生） |

下列小写值是**语义 slug / 展示口径**（每个状态"同色同词"的统一标识），**不等同于 API / DB 的实际枚举值**：
`online` 在线、`offline` 离线、`pending` 待审批、`revoked` 已吊销、`completed` 成功、`failed` 失败、
`uploading` 上传中、`inactive` 已停用。**实现时必须按 [`contracts.md`](./contracts.md) V-1 做映射**——现行契约的
前端枚举是**大写**（如 AgentStatus `RUNNING | OFFLINE | ...`，且有 DB `online` ⇄ 前端 `RUNNING` 的特例）；
**切勿把这里的小写 slug 直接当成前端/接口枚举值**，否则会与契约不一致。

### 1.3 字体与字号阶梯（rem，root = 16px）

字号一律用 **rem** 声明（禁止绝对 px）；**布局尺寸**（高度 / 间距 / 圆角）仍用 px。

> **实现前置**：下表的 px 等值以 **`:root` 基准字号 = 16px** 为准。当前 `webui/src/index.css` 的 `:root` 是
> `font: 18px/...`（仅窄屏 media query 才降到 16px），与本规范不符——实现期须把 `:root` 基准字号统一改为
> **16px**，否则 18px/15px/13px 的对应关系不成立。

| 用途 | 字号 | 字重 | AntD token |
|------|------|------|-----------|
| 页面标题 | `1.125rem` (18px) | 600 | 页面级，非 token |
| 卡片标题 | `0.9375rem` (15px) | 600 | `fontSizeLG` |
| 正文 / 表格 / 控件 | `0.8125rem` (13px) | 400 | `fontSize` |
| 辅助 / 表头 / 徽标 | `0.75rem` (12px) | 400–500 | `fontSizeSM` |
| 路径 / ID / 模板 | 等宽 | — | monospace |

字体栈：`-apple-system, "PingFang SC", "Microsoft YaHei", system-ui, sans-serif`。

### 1.4 尺寸与间距（4px 基准）

| 值 | 用途 | AntD token |
|----|------|-----------|
| 32px | 控件标准高度（按钮 / 输入 / 选择器） | `controlHeight` |
| 44 / 40px | 表格行高 / 表头高 | `Table` 组件 token |
| 8 / 6px | 卡片圆角 / 控件圆角 | `borderRadiusLG` / `borderRadius` |
| 24px | 页面内容区四周边距 | 布局 |
| 20px | 卡片内边距 | `Card` padding |
| 16px | 卡片之间间距 | 布局 |
| 208px | 侧边导航宽度（固定，**不可折叠**；仅此定宽，其余内容区流式，见 §1.5） | `Layout.Sider` |
| 480px | 表单抽屉宽度（**固定一档**；抽屉之外内容区流式，见 §1.5） | `Drawer` width |

### 1.5 布局自适应策略（桌面端，不同分辨率 / 窗口大小）

上表的 px 分**两类**，别混为一谈：

- **组件固有尺寸**（控件高 32、行高 44/40、圆角 8/6、间距 16/20/24）——是组件**固有尺寸，刻意固定 px、不随
  视口缩放**（与 AntD px token 语义一致：同一个按钮在 1440p 与 4K 上都是 32px 高，6px 圆角就是 6px）。让它们随
  视口缩放（如 vw）反而是错的。
- **布局宽度**——**真正定宽的只有两处 chrome**：侧栏 208px、抽屉 480px。**其余布局容器一律流式（flex/grid）。**

**自适应从何而来**：侧栏与窗口右缘之间的**内容区是流式的**，填满剩余宽度；表格 / 卡片 / 单行筛选栏随窗口宽度
**伸缩、回流**。所以不同桌面分辨率与浏览器窗口大小的适配，来自**流式内容区**，**而非缩放间距或字号**。
"固定侧栏 + 流式内容"是后台管理界面的标准范式，跨桌面分辨率不会崩——整页并非定宽画布。

**现状对齐**：`webui/src/layouts/BasicLayout.tsx` 的 `ProLayout` 已是"固定侧栏 + 流式内容"，实现沿用即可；仅需
把侧栏按本规范固定为 208px 不可折叠（当前带折叠开关，属实现待改项）。

> **实现前置（重要）**：当前 `webui/src/index.css` 的 `#root` 有 `width: 1126px; margin: 0 auto`（Vite 模板遗留），
> 会把整个应用**锁死成 1126px 居中列**——大屏下内容区**无法真正流式**。实现期须**去掉 `#root` 的定宽约束**
> （改为 `width: 100%`），否则即便按本规范改了 ProLayout / token，仍会被根容器宽度限制。

> 本版**不做** rem 化组件尺寸、不加 min-width 断点。若日后需要「浏览器缩放整体等比放大 UI」，再单独评估把组件
> 尺寸也改用 rem（会偏离 AntD 原生 px token，成本另计）。

---

## 2. 交互定则（7 条，全站强制）

1. **新建 / 编辑一律用右侧抽屉（480px）**，保留列表上下文；**不用整页跳转、不用弹窗承载表单**。
2. **危险操作（吊销 / 删除）用确认弹窗**：红色主按钮 + 明确后果描述 + 二次确认（删除文件类型等高破坏面场景
   进一步要求**输入名称确认**，见 `4f`）。
3. **列表筛选统一为顶部单行筛选栏**，条件变更即时查询；**不做折叠式高级搜索面板**。
4. **表格操作列右对齐**，最多 3 个文字链接按钮，更多操作收进「···」菜单。
5. **时间统一 `MM-DD HH:mm` 显示**，悬停 tooltip 给完整时间；相对时间只用于「最后心跳」。
6. **空态** = 一句话说明 + 主操作按钮；**加载** = 骨架屏（不用转圈）；**错误** = 内联红条 + 重试。
7. **每页只有一个蓝色实心主按钮**；其余为描边次按钮或文字按钮。

---

## 3. 导航与信息架构

固定左侧导航（208px，不可折叠），8 个一级入口：
**仪表盘 · 采集器 · 文件 · 文件类型 · Bucket · 事件规则 · 上传日志 · 设置**。

对应 `webui/src/pages/`：`Dashboard` · `Agents` · `Files` · `FileTypes` · `Buckets` · `Events` · `Logs` ·
`Settings`（`Login` 独立于 `AuthLayout`）。

---

## 4. 分页重做意图（映射到既有页面）

> 每条含设计选项编号（`1a`–`5c`）→ 目标页面。仅描述**意图**；像素级以视觉快照为参考。

### 4.1 仪表盘 — `pages/Dashboard`
- `1b` 用**服务端聚合数据**（§5.11.5 Dashboard 统计端点，已实现），**非**前端日志抽样。卡片化关键指标 +
  骨架屏加载。

### 4.2 采集器 — `pages/Agents`（含 `Pending` / `Detail` / `Rules` / `RuleForm` / `Logs`）
- `1c` 列表：**待审批置顶横幅** + 统一状态徽标（§1.2）+ 行内操作（审批 / 吊销走定则 2）。
- `2a–2d` 采集规则 = **三步右侧抽屉**（定则 1），入口在采集器详情 · 规则 Tab：
  - `2a` 抽屉第 1 步（基本信息）；
  - `2b` 第 2 步源路径 · **Watch 模式**；`2c` 第 2 步 · **Scheduled 变体**（cron + 启动即执行）；
  - `2d` 第 3 步上传路径 = **模板 + 实时预览 + 提交前摘要**。
  - 现有 `pages/Agents/RuleForm` 的三步 + dry-run 逻辑保留，改造为抽屉承载。

### 4.3 文件 — `pages/Files`（含 `Detail`）
- `1d` 列表：**单行筛选栏**（定则 3）+ **勾选批量下载**（选中即出工具条，复用
  `components/BatchDownload.tsx`）。
- `3a` **文件详情 = 480px 右侧抽屉**（定则 1），在列表点「详情」打开，不跳页。
- `4g` **变体**：左侧**类型树**（类型即筛选，复用 `components/DirectoryTree.tsx` 思路）替代顶部类型下拉；
  作为 `1d` 的可选信息架构。

### 4.4 文件类型 — `pages/FileTypes`
> **实现终态（WR-2，2026-07-14 用户拍板）**：本页降级为**名称 + 描述**的最简抽屉化 CRUD。
> 原设计的 `3b`/`3c`（glob 规则编辑器）已 **descope 到 backlog**，原因：① `file_type_rules` 表仅被
> indexer classifier **只读**消费，**无任何 REST 端点**，做编辑器须先建后端，破坏 WR「纯前端」前提；
> ② D-025 已把 `file_types` 降级为**兜底粗分类**（变种维度走标签、规则声明类型优先于 glob），UI 里配兜底
> glob 价值低。完整补齐方案与触发信号见 [`backlog.md`](../tasks/backlog.md#file_types-glob-规则管理后端--前端)。
> 下方 `3b`/`3c` 保留为**未来补齐时的目标形态**。

- `3b` 管理〔未落地〕：类型 = 名称 + 一组 **glob 规则**；文件数可点，跳转到文件页**预置筛选**。
- `3c` **类型编辑抽屉**〔未落地〕：glob 规则**可排序** + 优先级说明 + 路径**试匹配**。
- `4f` **删除确认弹窗**（定则 2）✅：红色主按钮 + 明确后果 + **输入名称确认**。

### 4.5 事件规则 — `pages/Events`（含 `Create` / `Deliveries`）
- `4a` 列表：动作只开放 **webhook / nats_publish**（与 D-019 / CC-7 一致）；**启停开关行内直改**。
- `4b` **编辑抽屉**：动作二选一，切换时**联动必填项**（webhook→`url` / nats_publish→`subject`）。
- `4c` **投递历史抽屉**：状态含终态 `dead`（与 CC-7 重试生命周期一致）；失败行可展开响应体。

### 4.6 Bucket — `pages/Buckets`
- `4d` 登记视图；**创建仅 super_admin**；事件通知配置状态可见。

### 4.7 上传日志 — `pages/Logs`
- `4e` 状态筛选用 **chip 形态**；失败行内嵌错误与**重试轨迹**。

### 4.8 登录 — `pages/Login`（`AuthLayout`）
- `5a` 用户名 + 密码 → **双 Token**；**OIDC 为后续预留，不出现在 UI**（与 auth.go 现状一致）。

### 4.9 设置 — `pages/Settings`（含 `Users` / `Profile`）
- `5b` **用户管理**：仅 super_admin 可见此 Tab；**禁用而非删除**（定则 2 语义）。
- `5c` **新建用户抽屉**：角色单选带权限说明；初始密码**一次性展示**。
- （Half B 会在设置下新增「标签词表」`7c` 与「待确认取值队列」`7d`，见 `metadata-model.md`。）

---

## 5. 落地方式

见 `docs/tasks/backlog.md` 的「前端重做实现（Half A）」track：建议先落**主题 token + 布局骨架**
（`main.tsx` 注入 `theme`、208px 固定 Sider、统一徽标），再**按页**推进；每页独立 PR + review，实现后对照
视觉快照与本规范核验。
