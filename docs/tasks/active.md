# active.md — 当前 Sprint 活跃任务

> **当前阶段：Phase 3 — 集成联调**
> **Agent 主要输入文件**：本文件 + `docs/tasks/bugs/open.md`

---

## 执行顺序

```
T3-2-FIX（API 契约对齐，见 bugs/open.md）← 当前阻塞
    ↓
T3-2 Web UI + Control Plane 联调
    ↓
T3-3 Python SDK + Control Plane 联调
```

---

## 当前优先：T3-2-FIX API 契约对齐

**状态**：⬜（全部 9 项均待完成）  
**阻塞原因**：前后端 JSON 契约错位导致采集器列表、文件浏览器、上传日志页面全部为空

详细规格见：[`docs/tasks/bugs/open.md`](bugs/open.md)

| Bug ID | 标题 | 类型 | 严重程度 |
|--------|------|------|---------|
| T3-2-FIX-A | 后端 `GET /api/v1/agents` 响应格式与参数对齐 | 后端 | 🔴 P0 |
| T3-2-FIX-B | 后端 `GET /api/v1/agents/:id/rules` 与上传日志端点信封对齐 | 后端 | 🔴 P0 |
| T3-2-FIX-C | 前端 `Agent` 接口与 `AgentStatus` 对齐 | 前端 | 🔴 P0 |
| T3-2-FIX-D | 前端 `CollectionRule` 接口字段对齐 | 前端 | 🔴 P0 |
| T3-2-FIX-E | 后端 `GET /api/v1/files` 响应格式与字段名对齐 | 后端 | 🔴 P0 |
| T3-2-FIX-F | 前端 `FileEntry` 接口字段对齐 | 前端 | 🔴 P0 |
| T3-2-FIX-G | 后端 upload-logs 端点响应格式与字段对齐 | 后端 | 🔴 P0 |
| T3-2-FIX-H | 前端 `UploadLog` 接口字段对齐 | 前端 | 🔴 P0 |
| T3-2-FIX-I | 后端+前端非分页列表端点统一为 `{items, total}` 信封 | 双侧 | 🟡 P1 |

---

## T3-2 — Web UI + Control Plane 联调 ⬜

**前置依赖**：T3-2-FIX-A~I 全部完成  
**涉及模块**：webui、controlplane

### 任务说明

以真实运行的 Control Plane 为后端，验证 Web UI 核心使用流程端到端全部畅通。

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

### 任务说明

以真实运行的 Control Plane 为后端，验证 Python SDK 核心使用流程端到端全部畅通。

### 验收标准

- [ ] 登录（`FileAgentClient.login()`）→ 获取 token，`me()` 返回正确用户信息
- [ ] 查询文件列表（`files.list()`）→ 返回 `PaginatedResponse`，`items` 非空（若有数据）
- [ ] 分页迭代（`files.iter()`）→ 能遍历全部文件（游标正确跳转）
- [ ] 下载文件（`files.download(id, path)`）→ 文件内容 SHA-256 与 MinIO 一致
- [ ] Token 自动刷新验证：使用过期 token 发起请求 → SDK 自动刷新后重试，用户无感知
- [ ] `poetry run pytest` 全部通过（含 integration 标记用例）
