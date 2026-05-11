# active.md — 当前执行入口（轻量）

> **当前阶段**：Phase 3 — 集成联调  
> **Agent 主要输入文件**：本文件 + `docs/tasks/bugs/open.md`

---

## 当前阻塞与优先级

- T3-2-FIX（A~L）已全部完成 ✅
- T3-2-BUG（A~D）为集成联调中新发现的 Bug，当前阻塞 T3-2
- 修复顺序：**T3-2-BUG-A → T3-2-BUG-B → T3-2-BUG-C / T3-2-BUG-D（可并行）**
- 详细规格（根因、子任务、验收标准）：[`docs/tasks/bugs/open.md`](bugs/open.md)

### 当前活跃 Bug 总览

| ID | 标题 | 优先级 | 涉及模块 |
|----|------|--------|---------|
| T3-2-BUG-A | Agent 审批后无最后心跳时间，无在线状态展示 | 🔴 P0 | controlplane + webui |
| T3-2-BUG-B | 目录浏览请求返回 409，CP 认为采集器 OFFLINE | 🔴 P0 | controlplane + webui |
| T3-2-BUG-C | 新建采集规则最后一步点击"创建规则"跳回第一步 | 🟡 P1 | webui |
| T3-2-BUG-D | 创建 Bucket 时 MinIO 报错，CP 静默忽略返回 201 | 🔴 P0 | controlplane + webui |

---

## 执行顺序（摘要）

```text
T3-2-BUG-A（A-1/A-2 后端可并行）
    ↓
T3-2-BUG-A-3/A-4（在线字段 + WebUI 展示）
    ↓
T3-2-BUG-B（依赖 A-3 的 is_online 字段）
    ↓（与 B 并行）
T3-2-BUG-C + T3-2-BUG-D（互不依赖）
    ↓ 全部完成后
T3-2（Web UI + Control Plane 联调完整验收）
    ↓
T3-3（Python SDK 联调）
```

完整主线、依赖与验收标准见：[`docs/tasks/phases/phase-3.md`](phases/phase-3.md)

---

## 关联入口

- 当前 Phase 主线：`docs/tasks/phases/phase-3.md`
- 未排期工作：`docs/tasks/backlog.md`
- 历史归档：`docs/tasks/archive/`
- 专项报告：`docs/reports/`
