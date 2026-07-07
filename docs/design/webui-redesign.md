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

## 1. 设计 Token（与 AntD 5 `ConfigProvider` token 一一对应）

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
| 页面背景 | bg-page | `#F5F6F8` | `colorBgLayout` |

**状态语义色**（取自 mockup 调色板，用于状态徽标 / 结果提示）：成功 `#1B8A5A`（浅底 `#E8F6EF`）=
`colorSuccess`；警告 `#B27409`（浅底 `#FCF3E3`）= `colorWarning`；危险 `#D64545`（浅底 `#FBEDED`）=
`colorError`。

### 1.2 状态语义（全站唯一映射）

状态一律用**「圆点 + 文字」徽标**；同一状态在任何页面**同色、同词**。既有 `AgentStatusBadge`
（`webui/src/components/AgentStatusBadge.tsx`）应升级为全站通用徽标并统一取色。

枚举（展示词）：`online` 在线、`offline` 离线、`pending` 待审批、`revoked` 已吊销、`completed` 成功、
`failed` 失败、`uploading` 上传中、`inactive` 已停用。大小写 / 后端枚举映射以
[`contracts.md`](./contracts.md) V-1 为权威（如 AgentStatus `online`⇄`RUNNING`）。

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
| 208px | 侧边导航宽度（固定，**不可折叠**） | `Layout.Sider` |
| 480px | 表单抽屉宽度（**固定一档**） | `Drawer` width |

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

### 4.4 文件类型 — `pages/FileTypes`（含 `Create` / `Detail`）
- `3b` 管理：类型 = 名称 + 一组 **glob 规则**；文件数可点，跳转到文件页**预置筛选**。
- `3c` **类型编辑抽屉**：glob 规则**可排序** + 优先级说明 + 路径**试匹配**。
- `4f` **删除确认弹窗**（定则 2）：红色主按钮 + 明确后果（如「该类型下有 N 个文件」）+ **输入名称确认**。

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
