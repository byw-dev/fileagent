# phase-3.md — Phase 3 集成联调主线

> **阶段状态**：🔄 进行中（T3-x 主线已完成 T3-1/T3-2/T3-4/T3-5/T3-6；随后插入两轮收尾冲刺）
> **阶段目标**：完成 Control Plane、Agent、Web UI 的端到端联调闭环（**Python SDK 联调 T3-3 已按产品决策推后**，不再是本阶段收口的必要条件）。
>
> **阶段现状（2026-07-04）**：T3-x 采集规则重构完成后，主线**未**线性推进到 T3-3，而是插入了
> **止血冲刺**（P0+P1 差异，G-1…G-5；见 `docs/reports/design-gap-analysis/` 与 07-summary §五）
> 和 **core-completeness 冲刺**（CC-1…CC-10；当前入口 `docs/tasks/active.md` + `docs/tasks/core-completeness.md`）。
> **T3-3 Python SDK 已按产品决策推后**（详见下方 T3-3 段与 `docs/tasks/core-completeness.md` 文末）。

---

## 主线执行顺序

```text
T3-2-BUG（集成联调新发现 Bug A~D）✅ 已完成
    ↓
T3-2 Web UI + Control Plane 联调  ✅ 已完成
    ↓
T3-4 pkg/trollsift 共享路径模板库 ✅
    ↓
T3-5 字段统一 + Bug 修复 ✅
    ↓
T3-6 Dry-Run 规则测试功能 ✅
    ↓
（插入）止血冲刺 G-1…G-5 ✅ → core-completeness 冲刺 CC-1…CC-10 ✅（CC-3 推后）已收官
    ↓
Phase 4 收尾：D-022 单二进制#60 / D-023 迁移嵌入+T4-3#61 / D-024 MinIO 拆分#62 / D-025 元数据6c+webui重做#63 ✅ 已合并
    ↓
T3-3 Python SDK + Control Plane 联调 ⏸️ 已推后
```

> 当前态与下一步候选见 `docs/tasks/active.md`（无进行中实现任务；候选=元数据6c Phase 1 / webui 重做 / proto→buf）。

---

## T3-4 / T3-5 / T3-6 — 采集规则重构 ✅

**前置依赖**：T3-2 完成  
**详细规格**：[`phase-3-rft.md`](phase-3-rft.md)

| ID | 任务 | 状态 |
|----|------|------|
| T3-4 | `pkg/trollsift` 共享路径模板库 | ✅ |
| T3-5 | 字段统一 + Bug 修复（DB/proto/agent/CP/WebUI） | ✅ |
| T3-6 | Dry-Run 规则测试功能 | ✅ |

---

## T3-3 — Python SDK + Control Plane 联调 ⏸️ 已推后

> **状态**：按产品决策推后（2026-07-04）——暂无消费方，CP 契约维护好则后期单独开发风险低。
> 理由与全清单见 `docs/tasks/core-completeness.md` 文末"明确推后"。下列验收标准保留，供日后恢复时使用。

**涉及模块**：sdk/python、controlplane

### 验收标准

- [ ] 登录（`FileAgentClient.login()`）→ 获取 token，`me()` 返回正确用户信息
- [ ] 查询文件列表（`files.list()`）→ 返回 `PaginatedResponse`，`items` 非空（若有数据）
- [ ] 分页迭代（`files.iter()`）→ 能遍历全部文件（游标正确跳转）
- [ ] 下载文件（`files.download(id, path)`）→ 文件内容 SHA-256 与 MinIO 一致
- [ ] Token 自动刷新验证：使用过期 token 发起请求 → SDK 自动刷新后重试，用户无感知
- [ ] `poetry run pytest` 全部通过（含 integration 标记用例）

---

## 已完成

### T3-2-BUG 系列 Bug 修复 ✅

**状态**：✅（A~D 共 4 项已全部完成，另含 createRule 400 错误修复，提交 `addb3ee`）

| ID | 标题 | 严重程度 | 涉及模块 |
|----|------|---------|---------|
| T3-2-BUG-A | Agent 审批后无最后心跳时间，无在线状态展示 | 🔴 P0 | controlplane + webui |
| T3-2-BUG-B | 目录浏览请求返回 409，CP 认为采集器 OFFLINE | 🔴 P0 | controlplane + webui |
| T3-2-BUG-C | 新建采集规则第三步缺少提交按钮，无法创建规则 | 🟡 P1 | webui |
| T3-2-BUG-D | 创建 Bucket 时 MinIO 报错，CP 静默忽略返回 201 | 🔴 P0 | controlplane + webui |
| createRule 400 | `createRuleRequest` JSON 字段名不匹配（字段对齐 + 路径模板变量修复） | 🔴 P0 | controlplane + webui + agent |

详细根因与修复方案：`docs/tasks/bugs/closed.md`

---

### T3-5 字段统一 + Bug 修复（审计）✅

**状态**：✅（全部验收项通过，含 T3-5-IMPL-J WebUI 重设计）  
**关联提交**：`f01598e`、`bbf9378`、`f5537f5`（原始实现）+ T3-5-IMPL-J 提交  
**审计日期**：2026-05-12（报告：`docs/reports/t3-5-audit-2026-05-12.md`）

- 已完成：
  - DB 字段迁移与 sqlc 模型同步（`migrations/000003_rename_rule_fields`）
  - proto `CollectionRule` 字段统一 + `DryRunResult` 预留
  - Agent 相对路径匹配（doublestar）与 trollsift Compose 存储路径
  - Control Plane `createRule`/`toRuleResponse`/dispatch 字段统一
  - WebUI 规则表单与接口字段统一（`enabled`/`recursive`/`append_mode`）
  - `append_mode` 默认值修正为 `"overwrite"`（`agents.go:563–566`）
  - `mode` 输入大小写归一化（`agents.go:567`）
  - `bucket_id → upload_bucket` CP dispatch 转换稳定（`dispatch.go:lookupBucketName`）
  - `append_mode` 补入 `collectionRuleResponse`（P1 缺口修复）
  - T3-5-IMPL-J：`pathTemplate.ts` 重设计（SYSTEM_TEMPLATE_VARIABLES、trollsift 语法校验、LDML 预览、extractDynamicFields）
  - T3-5-IMPL-J：`RuleForm.tsx` Step 3 更新（两区变量提示、新 initialValue、dynamicFields 联动）
  - T3-5-IMPL-J：`pathTemplate.test.ts` 全部重写（21 个测试全绿）
  - `pnpm test`：68 个测试全部通过
  - `go test ./...`（controlplane）：80.5% 总覆盖率
  - `go test ./...`（agent）：全部通过

**验收边界**（T3-5 关闭判定）：  
- 上述 P1 缺口修复 **且** `pnpm test` 全绿（含 `pathTemplate.test.ts`）→ T3-5 可关闭为 ✅  
- 仅 P2（mode 大小写）未修复可豁免关闭，须在 T3-6 前补充决策记录

---

### T3-2-FIX API 契约对齐 ✅

**状态**：✅（A~L 共 12 项已全部完成，提交 `2715706`）

- **A/B**：后端列表端点响应信封统一为 `{items, total, next_cursor}`
- **C/D**：前端 `Agent` / `CollectionRule` 接口字段对齐
- **E/F**：后端+前端 `FileEntry` 字段对齐
- **G/H**：后端+前端 `UploadLog` 字段对齐
- **I**：非分页列表端点统一 `{items, total}` 信封
- **J**：快增长表分页补齐 `has_more` + 真实 `total`
- **K**：前端 `Modal.confirm/message` 静态 API 改用 `App.useApp()` hooks
- **L**：`Detail.tsx` `Modal` import 丢失修复

---

### T3-1 Control Plane + Agent 端到端联调 ✅

- T3-1 主链路联调完成
- T3-1-FIX Agent 生命周期健壮性修复完成
- T3-1-BUG gRPC 注册链路关键问题修复完成

参考：`docs/tasks/changelog.md` 中 2026-05-07 记录（提交 `e295c05`）。
