# phase-3.md — Phase 3 集成联调主线

> **阶段状态**：🔄 进行中  
> **阶段目标**：完成 Control Plane、Agent、Web UI、Python SDK 的端到端联调闭环。

---

## 主线执行顺序

```text
T3-2-BUG（集成联调新发现 Bug A~D）✅ 已完成
    ↓
T3-2 Web UI + Control Plane 联调  ✅ 已完成
    ↓
T3-4 pkg/trollsift 共享路径模板库 ✅
    ↓
T3-5 字段统一 + Bug 修复 ⚠️（待补齐 WebUI 测试验收）
    ↓
T3-6 Dry-Run 规则测试功能
    ↓
T3-3 Python SDK + Control Plane 联调
```

---

## 当前优先：T3-5 — 字段统一 + Bug 修复（审计收尾）

**前置依赖**：T3-4 ✅  
**涉及模块**：controlplane、agent、proto、webui

### 验收标准

- [x] DB 迁移字段重命名落地（`000003_rename_rule_fields.*.sql`）
- [x] proto 字段统一与 dry-run 预留消息落地
- [x] `go test ./...`（controlplane）通过
- [x] `go test ./...`（agent）通过
- [ ] `pnpm test` 全通过（剩余 2 个失败：`pathTemplate.test.ts`，由 T3-5-IMPL-J 修复）

---

## T3-4 / T3-5 / T3-6 — 采集规则重构 ⬜

**前置依赖**：T3-2 完成  
**详细规格**：[`phase-3-rft.md`](phase-3-rft.md)

| ID | 任务 | 状态 |
|----|------|------|
| T3-4 | `pkg/trollsift` 共享路径模板库 | ✅ |
| T3-5 | 字段统一 + Bug 修复（DB/proto/agent/CP/WebUI） | ⚠️ |
| T3-6 | Dry-Run 规则测试功能 | ⬜ |

---

## T3-3 — Python SDK + Control Plane 联调 ⬜

**前置依赖**：T3-2 完成  
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

### T3-5 字段统一 + Bug 修复（审计）⚠️

**状态**：⚠️（核心改造已完成，WebUI 测试验收项尚未通过，另有 P1 级缺口待修复）  
**关联提交**：`f01598e`、`bbf9378`、`f5537f5`  
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
- 未完成验收项：
  - `pnpm test` 未全绿，剩余 2 个失败（`pathTemplate.test.ts`），待 T3-5-IMPL-J 修复
- 审计发现的缺口：
  - **P1** `append_mode` 未包含在 REST 响应（`collectionRuleResponse` 缺字段，`agents.go:448–462`）
  - **P1** WebUI `validatePathTemplate` 使用白名单拒绝合法动态字段（`pathTemplate.ts:54`）
  - **P2** `mode` 回显大小写：CP 返回 lowercase，TS 类型期望 uppercase

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
