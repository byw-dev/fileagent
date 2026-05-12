# active.md — 当前执行入口（轻量）

> **当前阶段**：Phase 3 — 集成联调  
> **Agent 主要输入文件**：本文件 + `docs/tasks/phases/phase-3.md`

---

## 当前任务：T3-2 — Web UI + Control Plane 联调

- T3-2-BUG（A~D）以及 createRule 400 错误修复已全部完成 ✅
- 当前主线任务为 **T3-2：Web UI + Control Plane 端到端联调验收**
- 详细验收标准：[`docs/tasks/phases/phase-3.md`](phases/phase-3.md)

### T3-2 验收清单（快速参考）

- [ ] 登录 → 仪表盘显示正确统计数据
- [ ] 采集器列表：审批 agent → 状态实时更新（含在线状态徽标、最后心跳时间）
- [ ] 采集器详情：创建采集规则 → 规则下发到已连接 agent
- [ ] 文件浏览器：搜索文件 → 获取下载链接 → 链接可访问
- [ ] `pnpm test` 全部通过

---

## 下一块：采集规则重构（T3-4 / T3-5 / T3-6）

T3-2 完成后立即执行，任务规格详见：[`docs/tasks/phases/phase-3-rft.md`](phases/phase-3-rft.md)

| ID | 任务 | 状态 |
|----|------|------|
| T3-4 | `pkg/trollsift` 共享路径模板库 | ✅ |
| T3-5 | 字段统一 + Bug 修复（DB/proto/agent/CP/WebUI） | ⬜ |
| T3-6 | Dry-Run 规则测试功能 | ⬜ |

---

## 执行顺序（摘要）

```text
T3-2-BUG（A~D）✅ 已完成
    ↓
T3-2 Web UI + Control Plane 联调  ← 当前
    ↓
T3-4 → T3-5 → T3-6  采集规则重构
    ↓
T3-3 Python SDK + Control Plane 联调
```

完整主线、依赖与验收标准见：[`docs/tasks/phases/phase-3.md`](phases/phase-3.md)

---

## 关联入口

- 当前 Phase 主线：`docs/tasks/phases/phase-3.md`
- 采集规则重构规格：`docs/tasks/phases/phase-3-rft.md`
- 已关闭 Bug：`docs/tasks/bugs/closed.md`
- 未排期工作：`docs/tasks/backlog.md`
- 历史归档：`docs/tasks/archive/`
- 专项报告：`docs/reports/`

