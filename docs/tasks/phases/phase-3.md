# phase-3.md — Phase 3 集成联调主线

> **阶段状态**：🔄 进行中  
> **阶段目标**：完成 Control Plane、Agent、Web UI、Python SDK 的端到端联调闭环。

---

## 主线执行顺序

```text
T3-2-BUG（集成联调新发现 Bug A~D）✅ 已完成
    ↓
T3-2 Web UI + Control Plane 联调  ← 当前任务
    ↓
T3-3 Python SDK + Control Plane 联调
```

---

## 当前优先：T3-2 — Web UI + Control Plane 联调

**前置依赖**：T3-2-BUG-A~D ✅（全部完成）  
**涉及模块**：webui、controlplane

### 验收标准

- [ ] 登录 → 仪表盘显示正确统计数据（文件数 > 0 时不为 0）
- [ ] 采集器列表页：审批一个 agent → 状态更新为 APPROVED/RUNNING
- [ ] 采集器详情页：创建采集规则 → 规则下发到已连接的 agent（日志可见）
- [ ] 文件浏览器：搜索文件 → 单文件获取下载链接 → 链接可访问
- [ ] `pnpm test` 全部通过

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
- T3-1-BUGFIX gRPC 注册链路关键问题修复完成

参考：`docs/tasks/changelog.md` 中 2026-05-07 记录（提交 `e295c05`）。

