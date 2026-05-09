# phase-3.md — Phase 3 集成联调主线

> **阶段状态**：🔄 进行中  
> **阶段目标**：完成 Control Plane、Agent、Web UI、Python SDK 的端到端联调闭环。

---

## 主线执行顺序

```text
T3-2-FIX（API 契约对齐，见 bugs/open.md）← 当前阻塞
    ↓
T3-2 Web UI + Control Plane 联调
    ↓
T3-3 Python SDK + Control Plane 联调
```

---

## 已完成

### T3-1 Control Plane + Agent 端到端联调 ✅

- T3-1 主链路联调完成
- T3-1-FIX Agent 生命周期健壮性修复完成
- T3-1-BUGFIX gRPC 注册链路关键问题修复完成

参考：`docs/tasks/changelog.md` 中 2026-05-07 记录（提交 `e295c05`）。

---

## 当前优先：T3-2-FIX API 契约对齐

**状态**：✅（A~L 共 12 项已全部完成）

最新两项（T3-2-FIX-K/L）：
- **K**：前端 `Modal.confirm/message` 静态 API 在 React 18 StrictMode 下静默失效 → 全站改用 `App.useApp()` hooks + `main.tsx` 加 `<App>` 包裹
- **L**：`Detail.tsx` 重构后 `Modal` import 丢失 → 采集器详情页崩溃 → 恢复 `Modal` import

详细规格与逐项验收：`docs/tasks/bugs/open.md`

---

## T3-2 — Web UI + Control Plane 联调 ⬜

**前置依赖**：T3-2-FIX-A~L 全部完成  
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
