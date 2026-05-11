# bugs/open.md — 未解决 BUG

> 本文件结构化描述所有待修复 Bug，Agent 可直接消费。
> 每个 Bug 包含：根因、精确代码位置、修复方案（拆分为子任务）、验收标准。

---

## 总览

| ID | 标题 | 严重程度 | 状态 | 前置依赖 |
|----|------|---------|------|---------|
| T3-2-BUG-A | Agent 审批后无最后心跳时间，无在线状态展示 | 🔴 P0 | ⬜ | — |
| T3-2-BUG-B | 目录浏览请求返回 409，CP 认为采集器 OFFLINE | 🔴 P0 | ⬜ | T3-2-BUG-A |
| T3-2-BUG-C | 新建采集规则第三步缺少提交按钮，无法创建规则 | 🟡 P1 | ⬜ | — |
| T3-2-BUG-D | 创建 Bucket 时 MinIO 报错，CP 静默忽略返回 201 | 🔴 P0 | ⬜ | — |

**修复顺序建议**：

```
T3-2-BUG-A（后端先行，A-1/A-2 可并行）
    ↓
T3-2-BUG-B（依赖 A-3 的 is_online 字段）
    ↓
T3-2-BUG-C 和 T3-2-BUG-D 可并行（互不依赖）
```

---

## T3-2-BUG-A — Agent 审批后无最后心跳时间，无在线状态展示

**现象**：在 Web UI 上审批通过 Agent 后，采集器列表与详情页的「最后心跳时间」永远为空，也没有实时在线/离线状态标识，无法判断 Agent 是否已建立 gRPC 连接并正常运行。

**设计文档参考**：§3.3.4（agents 表 `last_seen_at`）、§5.2（Agent 连接管理）、§3.5（Redis `agent:{id}:online` 键）

---

### 根因分析

**根因 1（致命）：心跳处理未持久化 `last_seen_at`**

`grpcserver/handler.go:174-184`（`handleHeartbeat`）收到心跳后只刷新 Redis TTL，从未调用数据库层的 `UpdateAgentHeartbeat`。`agents` 表的 `last_seen_at` 字段因此永远为 `NULL`。`toAgentResponse`（`agents.go:142-144`）判断 `a.LastSeenAt.Valid` 为 `false`，`last_seen_at` 字段输出为空字符串。

```go
// 现状（handler.go:174-184）
func (s *Server) handleHeartbeat(ctx context.Context, agentID string, hb *agentv1.Heartbeat) {
    if s.cache != nil {
        if err := s.cache.Set(ctx, cache.AgentOnlineKey(agentID), "1", agentOnlineTTL); err != nil {
            s.logger.Warn("heartbeat: refresh online TTL failed", zap.Error(err))
        }
    }
    // ❌ 缺少 s.db.UpdateAgentHeartbeat(ctx, ...) 调用
}
```

**根因 2（致命）：Connect/Disconnect 未更新 DB 状态**

`Connect()`（`handler.go:41-111`）在 Agent 建立 gRPC 长连接后，只写 Redis 在线键（`agent:{id}:online`）并发布 NATS 事件，从未调用 `db.UpdateAgentStatus(ctx, id, "online")`。断开时的 defer 同理，状态留在 `"approved"` 而非切换到 `"offline"`。

DB 枚举 `agent_status`（§3.3.4）明确包含 `'online'` 和 `'offline'` 状态，设计意图是 DB 跟踪完整生命周期。

**根因 3（中等）：REST 响应不含实时在线字段**

`agentResponse`（`agents.go:73-86`）无 `is_online` 字段；`toAgentResponse` 也不调用 `h.registry.IsOnline()` 或查询 Redis。前端虽可通过 `status == "RUNNING"` 间接判断，但这依赖根因 2 的修复，且不反映心跳超时（Redis TTL 过期但 DB 未及时切 `offline`）后的状态。

---

### 子任务拆分

#### T3-2-BUG-A-1 — CP-后端：`handleHeartbeat` 持久化 `last_seen_at`

| 字段 | 内容 |
|------|------|
| **优先级** | 🔴 P0 |
| **涉及模块** | controlplane |
| **输入** | `agentv1.Heartbeat` 消息 + Agent gRPC 连接上下文 |
| **输出** | PostgreSQL `agents.last_seen_at` = `NOW()` |

**目标**：每次收到 Agent 心跳时将 `last_seen_at` 写入数据库，使 REST API 能返回真实心跳时间。

**修复方案**：

- **文件**：`controlplane/internal/grpcserver/server.go`（Server 结构体）+ `handler.go`（`handleHeartbeat`）

1. `Server` 结构体增加 `db AgentHeartbeatDB` 接口字段：
   ```go
   type AgentHeartbeatDB interface {
       UpdateAgentHeartbeat(ctx context.Context, id uuid.UUID, ip pqtype.Inet) error
   }
   ```
   > 注：`UpdateAgentHeartbeat` 现有签名需要 `ipAddress pqtype.Inet`，可传空值 `pqtype.Inet{}` 以仅更新时间戳（IP 已在注册时固化）；或单独添加 `UpdateAgentLastSeen(ctx, id)` 查询（仅 `SET last_seen_at = NOW()`），在 `queries/*.sql` 和 sqlc 中补齐。

2. `handleHeartbeat` 在刷新 Redis TTL 后调用：
   ```go
   if s.db != nil {
       if id, err := uuid.Parse(agentID); err == nil {
           if dbErr := s.db.UpdateAgentLastSeen(ctx, id); dbErr != nil {
               s.logger.Warn("heartbeat: update last_seen_at failed", zap.Error(dbErr))
           }
       }
   }
   ```

3. `cmd/server/main.go`：将 `queries` 注入到 gRPC Server。

**验收标准**：
- Agent 发送一次心跳 → `SELECT last_seen_at FROM agents WHERE id=...` 返回非 NULL 值
- 心跳时间误差 ≤ 5 秒
- `go test ./controlplane/internal/grpcserver/... -count=1` 新增 `TestHandleHeartbeat_UpdatesLastSeenAt` 用例通过

---

#### T3-2-BUG-A-2 — CP-后端：`Connect` / `Disconnect` 更新 DB `status`

| 字段 | 内容 |
|------|------|
| **优先级** | 🔴 P0 |
| **涉及模块** | controlplane |
| **输入** | gRPC `Connect` 流建立 / 断开事件 |
| **输出** | `agents.status` = `"online"` / `"offline"` |

**目标**：Agent 连接成功后 DB 状态切换为 `"online"`，断开后切换为 `"offline"`，使前端 `status` 字段可直接反映连接状态。

**修复方案**：

- **文件**：`controlplane/internal/grpcserver/handler.go`（`Connect` 函数）

1. 在写 Redis 在线键之后，调用：
   ```go
   if s.db != nil {
       if id, err := uuid.Parse(agentID); err == nil {
           if _, dbErr := s.db.UpdateAgentStatus(ctx, id, db.AgentStatusOnline); dbErr != nil {
               s.logger.Warn("connect: update status to online failed", zap.Error(dbErr))
           }
       }
   }
   ```

2. 在 defer（断开清理）中，调用（使用 `context.Background()` 避免已取消的 ctx）：
   ```go
   if s.db != nil {
       if id, err := uuid.Parse(agentID); err == nil {
           if _, dbErr := s.db.UpdateAgentStatus(context.Background(), id, db.AgentStatusOffline); dbErr != nil {
               s.logger.Warn("disconnect: update status to offline failed", zap.Error(dbErr))
           }
       }
   }
   ```

3. `AgentHeartbeatDB` 接口（与 A-1 共享）中增加 `UpdateAgentStatus`。

**验收标准**：
- Agent 建立 gRPC 连接 → `SELECT status FROM agents WHERE id=...` = `"online"`
- Agent 断开连接 → `SELECT status FROM agents WHERE id=...` = `"offline"`
- `mapDBStatusToFrontend("online")` 返回 `"RUNNING"`（现有逻辑已覆盖）
- `go test ./controlplane/internal/grpcserver/... -count=1` 新增 `TestConnect_UpdatesStatusOnline` 和 `TestConnect_UpdatesStatusOffline` 用例通过

---

#### T3-2-BUG-A-3 — CP-后端：REST 响应新增 `is_online` 字段

| 字段 | 内容 |
|------|------|
| **优先级** | 🟡 P1 |
| **涉及模块** | controlplane |
| **输入** | `GET /api/v1/agents` / `GET /api/v1/agents/:id` 请求 |
| **输出** | 每个 agent 对象包含 `is_online: bool`（来自 Redis 实时查询） |

**目标**：提供独立于 DB `status` 的实时在线布尔字段，供前端渲染在线状态指示灯。

**设计背景**：§5.2 规定在线状态主要存于 Redis（`agent:{id}:online`，TTL 90s）；DB `status` 跟踪生命周期但存在延迟（A-2 修复后延迟降为秒级）。`is_online` 直接查询 Redis，可做实时降级。

**修复方案**：

- **文件**：`controlplane/internal/api/handler/agents.go`

1. `agentResponse` 增加字段：
   ```go
   IsOnline bool `json:"is_online"`
   ```

2. `AgentsHandler` 增加 `cache CacheClient` 依赖（与 gRPC Server 共用接口）。

3. `toAgentResponse` 改为：
   ```go
   func (h *AgentsHandler) toAgentResponse(a *db.Agent) agentResponse {
       r := agentResponse{ /* ... 现有字段 ... */ }
       if h.cache != nil {
           exists, _ := h.cache.Exists(ctx, cache.AgentOnlineKey(a.ID.String()))
           r.IsOnline = exists
       }
       return r
   }
   ```
   > 注：需将 ctx 传入，可通过方法签名或闭包获取。

4. `cmd/server/main.go`：将 `redisClient` 注入到 `AgentsHandler`。

**验收标准**：
- `GET /api/v1/agents` 响应中每个 agent 含 `is_online: true/false`
- Agent 在线时 `is_online: true`；Agent 离线（Redis 无键）时 `is_online: false`
- `agents_test.go` 补充 `is_online` 断言用例

---

#### T3-2-BUG-A-4 — WebUI：采集器列表和详情页展示在线状态与心跳时间

| 字段 | 内容 |
|------|------|
| **优先级** | 🟡 P1 |
| **涉及模块** | webui |
| **输入** | `Agent.is_online: boolean`（来自 A-3 后端）；`Agent.last_seen_at: string \| null` |
| **输出** | 采集器列表显示在线状态绿灯/红灯；详情页「最后心跳时间」有值 |
| **前置依赖** | T3-2-BUG-A-3 |

**目标**：将后端新增的 `is_online` 字段和已存在的 `last_seen_at` 在 UI 上正确呈现。

**修复方案**：

- **文件**：`webui/src/services/agents.ts`、`webui/src/pages/Agents/index.tsx`、`webui/src/pages/Agents/Detail.tsx`、`webui/src/components/AgentStatusBadge.tsx`

1. **`agents.ts`**：`Agent` 接口增加 `is_online: boolean`。

2. **`index.tsx`** 采集器列表表格：在「状态」列旁或内嵌在线状态指示灯（绿点/红点），可用 antd `Badge` 组件；
   `is_online: true` → 绿色徽标；`is_online: false` → 灰色或红色。

3. **`Detail.tsx`** 基本信息 Tab：「最后心跳时间」字段已存在（约第 280 行），当 `agent.last_seen_at` 为 null 时显示 `—` 而非空白；确保心跳修复后此处自动更新。

4. **`AgentStatusBadge.tsx`**：可选地增加 `is_online` 覆盖逻辑（`is_online=true` 时即使 DB `status` 为 `"APPROVED"` 也展示绿色「在线」标识，对两段信息取最优解）。

**验收标准**：
- 采集器列表中在线 Agent 显示绿色在线标识
- 采集器列表中离线 Agent 显示灰色/红色标识
- 详情页「最后心跳时间」在 Agent 发送过心跳后显示正确时间（非空）
- `pnpm test` 通过

---

## T3-2-BUG-B — 目录浏览请求返回 409，CP 认为采集器 OFFLINE

**现象**：在 Web UI 点击已审批的采集器详情页，切换到「目录浏览」Tab，前端发出 `POST /api/v1/agents/{id}/list-dir` 请求，CP 返回 `409 Conflict`，响应体为 `{"error": {"code": "AGENT_OFFLINE", "message": "agent is not online"}}`。

**设计文档参考**：§5.2（Agent 连接管理）、§5.11.2（list-dir API）、§3.5（Redis `agent:{id}:online`）

**前置依赖**：T3-2-BUG-A（在线状态正确更新后，本 Bug 部分自愈）

---

### 根因分析

**根因 1（主因）：`ListDir` 仅检查内存注册表，不检查 Redis**

`AgentsHandler.ListDir`（`agents.go:298-303`）调用 `h.registry.IsOnline(agentID)`，该方法（`registry.go`）只查询当前进程内存中的 `conns` map。CP 实例重启后内存注册表清空；Agent 在 Redis 中仍有有效 TTL（90 秒内），但若 Agent 重连 gRPC 尚未完成，内存注册表为空 → 判定 OFFLINE → 409。

设计文档 §2.5 明确「CP 实例重启后，从 Redis 恢复在线状态，等待 Agent 重连」，但当前实现 `IsOnline` 不查 Redis，与设计不符。

**根因 2（辅因）：前端缺乏实时在线状态保护**

前端「目录浏览」Tab 始终可点击，没有基于 `is_online` 字段的保护判断。Agent 离线时，用户不知情地发出请求，收到 409 后错误信息也不够友好（原始 API 错误直接展示）。

---

### 子任务拆分

#### T3-2-BUG-B-1 — CP-后端：`ListDir` 在线检查增加 Redis 降级

| 字段 | 内容 |
|------|------|
| **优先级** | 🔴 P0 |
| **涉及模块** | controlplane |
| **输入** | `POST /api/v1/agents/{id}/list-dir` 请求 |
| **输出** | Agent 在 Redis 有效期内认定「在线」并下发指令；纯内存检查失败但 Redis 有效时不返回 409 |

**目标**：将 `IsOnline` 语义扩展为：**内存注册表中有连接**（可立即发送） **OR** **Redis 在线键存在**（最近 90 秒内有心跳）。内存注册表优先（直接发送消息），Redis 作为降级（仍尝试 `Send`，返回失败时再 409）。

**修复方案**：

- **文件**：`controlplane/internal/api/handler/agents.go`（`ListDir` 方法）

当前逻辑：
```go
if !h.registry.IsOnline(agentID) {
    c.JSON(http.StatusConflict, ...) // 409
    return
}
```

改为：
```go
inMemory := h.registry.IsOnline(agentID)
inRedis  := false
if !inMemory && h.cache != nil {
    if exists, _ := h.cache.Exists(c.Request.Context(), cache.AgentOnlineKey(agentID)); exists {
        inRedis = true
    }
}
if !inMemory && !inRedis {
    c.JSON(http.StatusConflict, gin.H{
        "error": middleware.NewErrorBody("AGENT_OFFLINE", "agent is not online", nil),
    })
    return
}
// 仍尝试 Send；如果内存连接确实不存在，Send 返回 false → 409
```

> 注：`h.cache` 使用现有 `CacheClient` 接口即可；需确认 `Exists` 方法已在接口定义。若无 `Exists`，使用 `Get` + 判断 `err == nil` 替代。

**验收标准**：
- Agent 在 Redis 有效期内（90 秒），即使 CP 刚重启内存注册表为空，`ListDir` 不立即返回 409（尝试 Send，Send 失败才返回 409 并在日志中注明"disconnected during send"）
- 真正离线（Redis TTL 也过期）时仍返回 409
- `agents_test.go` 补充 `TestListDir_AgentOnlineInRedisNotInMemory` 测试用例

---

#### T3-2-BUG-B-2 — WebUI：目录浏览 Tab 基于在线状态进行保护

| 字段 | 内容 |
|------|------|
| **优先级** | 🟡 P1 |
| **涉及模块** | webui |
| **输入** | `Agent.is_online: boolean`（来自 T3-2-BUG-A-3） |
| **输出** | `is_online=false` 时目录浏览 Tab 显示「采集器当前离线，无法浏览目录」提示，不发出 API 请求 |
| **前置依赖** | T3-2-BUG-A-4 |

**目标**：在 Agent 离线时，禁止目录浏览请求，提供明确的用户提示，避免不必要的 409 错误。

**修复方案**：

- **文件**：`webui/src/pages/Agents/Detail.tsx`

在目录浏览 Tab 内容区域顶部，当 `agent.is_online === false` 时显示 antd `Alert`：

```tsx
{activeTab === 'dir' && !agent.is_online && (
  <Alert
    type="warning"
    message="采集器当前离线"
    description="Agent 未建立 gRPC 连接，无法远程浏览目录。请等待 Agent 上线后重试。"
    showIcon
    style={{ marginBottom: 16 }}
  />
)}
```

同时，「浏览目录」触发按钮在 `!agent.is_online` 时设为 `disabled`；若仍发出请求且收到 409，显示友好的 `message.warning` 而非原始 API 错误。

**验收标准**：
- Agent 离线时目录浏览 Tab 显示 Warning Alert，「浏览目录」按钮禁用
- Agent 在线时 Tab 正常可用
- `pnpm test` 通过

---

## T3-2-BUG-C — 新建采集规则第三步缺少提交按钮，无法创建规则

**现象**：在采集器详情页「采集规则」Tab 点击「新建采集规则」进入三步表单，填完第一、二步正常，进入第三步（上传路径配置）后，页面底部**没有"创建规则"提交按钮**，用户无法提交表单；浏览器控制台也观察不到任何发出的 HTTP 请求，表明问题发生在 UI 渲染层，完全未到达调用 CP API 的阶段。

**设计文档参考**：§5.11.2（`POST /api/v1/agents/{id}/rules` 创建采集规则）

---

### 根因分析

`RuleForm.tsx:132-155` 在 `StepsForm` 上配置了自定义 `submitter.render`：

```typescript
// RuleForm.tsx:133-155
<StepsForm
  submitter={{
    render: (props) => {
      if (props.step === 0) {
        return <Button ...>下一步</Button>
      }
      if (props.step === 1) {
        return <Space>...上一步...下一步...</Space>
      }
      // else：step === 2，应渲染"创建规则"按钮
      return (
        <Space>
          <Button onClick={() => props.onPre?.()}>上一步</Button>
          <Button type="primary" loading={submitting} onClick={() => props.onSubmit?.()}>
            创建规则
          </Button>
        </Space>
      )
    },
  }}
  onFinish={...}
>
```

代码逻辑本身正确：else 分支应为第三步渲染"创建规则"按钮。但在 `@ant-design/pro-components@2.8.x` 中，**`StepsForm.submitter.render` 配置的是整个 `StepsForm` 的最终提交区域，不会被透传给各个 `StepForm` 作为步骤级 submitter。** 各 `StepForm` 依赖自身的 `submitter` prop（或 ProComponents 内置默认按钮）来渲染步骤操作按钮。

由于代码未给每个 `StepForm` 单独设置 `submitter`：
- 步骤 0/1：ProComponents 内置的「下一步」按钮生效，用户可正常前进
- 步骤 2（最后步）：ProComponents 内置会尝试渲染最终提交按钮，但因 `StepsForm.submitter.render` 未能正确下发，导致第三步底部无任何按钮渲染

`StepsForm.onFinish` 始终 `return true`（第 161 行）是次要缺陷——该路径当前根本无法到达，修复按钮渲染后需一并解决，否则 API 失败时表单会重置到第一步。

---

### 子任务拆分

#### T3-2-BUG-C-1 — WebUI：为每个 `StepForm` 单独配置 `submitter`，确保第三步渲染提交按钮

| 字段 | 内容 |
|------|------|
| **优先级** | 🟡 P1 |
| **涉及模块** | webui |
| **输入** | 用户填写完三步表单，进入第三步 |
| **输出** | 第三步底部出现「上一步」+「创建规则」按钮；点击后正确发出 API 请求 |

**目标**：将步骤导航按钮从 `StepsForm.submitter.render` 迁移到各 `StepForm` 的 `submitter` prop，保证每一步都有正确的步骤按钮；同时修正 API 失败时不重置表单的问题。

**修复方案**：

- **文件**：`webui/src/pages/Agents/RuleForm.tsx`

**方案**：移除 `StepsForm` 上的 `submitter.render`，改为在各 `StepForm` 上分别配置 `submitter`：

```typescript
// 移除 StepsForm 上的 submitter prop

<StepsForm
  onFinish={async (values) => {
    const v = values as { step1?: Step1Values; step2?: ...; step3?: Step3Values }
    if (v.step1 && v.step2 && v.step3) {
      return await handleFinish(v.step1, v.step2, v.step3) // 修正：成功返回 true，失败返回 false
    }
    return false
  }}
>
  <StepsForm.StepForm name="step1" title="基本配置"
    submitter={{ render: (props) => <Button type="primary" onClick={() => props.onSubmit?.()}>下一步</Button> }}
  >
    {/* ... */}
  </StepsForm.StepForm>

  <StepsForm.StepForm name="step2" title="源路径配置"
    submitter={{ render: (props) => (
      <Space>
        <Button onClick={() => props.onPre?.()}>上一步</Button>
        <Button type="primary" onClick={() => props.onSubmit?.()}>下一步</Button>
      </Space>
    )}}
  >
    {/* ... */}
  </StepsForm.StepForm>

  <StepsForm.StepForm name="step3" title="上传路径配置"
    submitter={{ render: (props) => (
      <Space>
        <Button onClick={() => props.onPre?.()}>上一步</Button>
        <Button type="primary" loading={submitting} onClick={() => props.onSubmit?.()}>创建规则</Button>
      </Space>
    )}}
  >
    {/* ... */}
  </StepsForm.StepForm>
</StepsForm>
```

同时修正 `handleFinish`，使其在成功时返回 `true`，失败时返回 `false`（让 ProComponents 保持在第三步）：

```typescript
const handleFinish = async (...): Promise<boolean> => {
  if (!agentId) return false
  setSubmitting(true)
  try {
    await createRule(agentId, { /* ... */ })
    message.success('规则创建成功')
    navigate(`/agents/${agentId}`, { state: { tab: 'rules' } })
    return true
  } catch {
    message.error('规则创建失败，请稍后重试')
    return false  // ← 失败时保留第三步，不重置表单
  } finally {
    setSubmitting(false)
  }
}
```

**验收标准**：
- 进入第三步（上传路径配置）后，底部出现「上一步」和「创建规则」两个按钮
- 点击「创建规则」，浏览器控制台可观察到 `POST /api/v1/agents/{id}/rules` 请求发出
- API 成功 → 用户被导航到采集器详情页的规则 Tab，新规则出现在列表中
- API 失败 → 用户停留在第三步，`message.error` 提示显示，填写的字段保持不变
- `pnpm test` 通过（建议新增 `rule-form.test.tsx` 覆盖步骤渲染和提交场景）

---

## T3-2-BUG-D — 创建 Bucket 时 MinIO 报错，CP 静默忽略返回 201

**现象**：通过 Web UI 或 API 创建 Bucket，当 MinIO 侧因 Bucket 名称不合法等原因报错时，CP 后台打印 error 日志但向客户端返回 `201 Created`，客户端（含 Web UI）误以为 Bucket 创建成功；实际上 MinIO 中不存在该 Bucket，后续 Agent 上传文件或 STS 凭据下发时将失败。

```json
{"level":"error","ts":1778486212.653134,"caller":"handler/events.go:135",
 "msg":"create minio bucket","bucket":"radar_data",
 "error":"Bucket name contains invalid characters"}
```

**设计文档参考**：§5.11.4（`POST /api/v1/buckets`）、§6.2（Bucket 命名约定）、§6.3（ACL 策略）

---

### 根因分析

**根因 1（主因）：MinIO 错误被静默忽略**

`events.go:129-142`（`BucketsHandler.Create`）中，创建 MinIO 物理 Bucket 的错误仅记录日志，不影响 HTTP 响应：

```go
if h.minio != nil {
    if mkErr := h.minio.MakeBucket(c.Request.Context(), bucket.Name); mkErr != nil {
        h.logger.Error("create minio bucket",
            zap.String("bucket", bucket.Name),
            zap.Error(mkErr),
        )
        // ❌ 未返回错误；继续向下执行 201
    }
}
c.JSON(http.StatusCreated, toBucketResponse(bucket))  // 永远 201
```

注释「If MinIO is unavailable, the DB record is still returned」描述的是**临时不可用**场景，但命名不合法是**客户端错误**，不应被静默忽略。

**根因 2（辅因）：缺乏前置命名校验**

MinIO Bucket 命名规则（S3 兼容）：
- 长度 3~63 个字符
- 只允许小写字母、数字和连字符 `-`
- 不能以连字符开头或结尾
- 不能包含下划线、大写字母、空格等

CP 目前没有在入库前校验 bucket name，导致非法名称先写入 PostgreSQL，再被 MinIO 拒绝，且 DB 记录无法回滚。

**根因 3（中等）：MinIO 失败后 DB 记录孤立**

命名合法但 MinIO 暂时不可用时，DB 中存在 Bucket 记录但 MinIO 中无物理 Bucket，后续所有依赖该 Bucket 的操作（上传、STS 下发）均会失败，且无告警机制。

---

### 子任务拆分

#### T3-2-BUG-D-1 — CP-后端：`CreateBucket` 入口前校验 Bucket 命名规范

| 字段 | 内容 |
|------|------|
| **优先级** | 🔴 P0 |
| **涉及模块** | controlplane |
| **输入** | `POST /api/v1/buckets` 请求体 `{"name": "..."}`|
| **输出** | 命名不合法时返回 `422 Unprocessable Entity`，不写 DB，不调用 MinIO |

**目标**：在写入 DB 之前拦截非法 Bucket 名称，提前返回清晰的客户端错误。

**修复方案**：

- **文件**：`controlplane/internal/api/handler/events.go`（`BucketsHandler.Create`）

在 `c.ShouldBindJSON(&req)` 之后、`h.db.CreateBucket(...)` 之前，增加命名校验：

```go
var bucketNameRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{1,61}[a-z0-9]$`)

func validateBucketName(name string) error {
    if len(name) < 3 || len(name) > 63 {
        return fmt.Errorf("bucket name must be 3-63 characters")
    }
    if !bucketNameRegexp.MatchString(name) {
        return fmt.Errorf("bucket name must contain only lowercase letters, numbers, and hyphens, and cannot start or end with a hyphen")
    }
    return nil
}
```

校验失败时：
```go
if err := validateBucketName(req.Name); err != nil {
    c.JSON(http.StatusUnprocessableEntity, gin.H{
        "error": middleware.NewErrorBody("INVALID_BUCKET_NAME", err.Error(), nil),
    })
    return
}
```

**验收标准**：
- `POST /api/v1/buckets {"name": "radar_data"}` → `422 INVALID_BUCKET_NAME`，DB 无新记录
- `POST /api/v1/buckets {"name": "UPPER"}` → `422 INVALID_BUCKET_NAME`
- `POST /api/v1/buckets {"name": "ok-bucket"}` → 正常流程（成功或 MinIO 错误）
- `events_test.go` 补充命名校验用例

---

#### T3-2-BUG-D-2 — CP-后端：MinIO `MakeBucket` 失败时回滚 DB 记录并返回 5xx

| 字段 | 内容 |
|------|------|
| **优先级** | 🔴 P0 |
| **涉及模块** | controlplane |
| **输入** | `POST /api/v1/buckets` 请求（命名合法，但 MinIO 调用失败） |
| **输出** | MinIO 错误时回滚 DB 记录并返回 `502 Bad Gateway`（MinIO 不可达）或 `500 Internal Server Error` |

**目标**：命名合法但 MinIO 不可用时，回滚 DB 中已创建的 Bucket 记录，返回明确的服务端错误，避免 DB 与 MinIO 数据不一致。

**修复方案**：

- **文件**：`controlplane/internal/api/handler/events.go`（`BucketsHandler.Create`）

```go
bucket, err := h.db.CreateBucket(c.Request.Context(), params)
if err != nil {
    // 现有错误处理保持不变
}

if h.minio != nil {
    if mkErr := h.minio.MakeBucket(c.Request.Context(), bucket.Name); mkErr != nil {
        h.logger.Error("create minio bucket",
            zap.String("bucket", bucket.Name),
            zap.Error(mkErr),
        )
        // 回滚 DB 记录
        if delErr := h.db.DeleteBucket(context.Background(), bucket.ID); delErr != nil {
            h.logger.Error("rollback: delete bucket record failed",
                zap.String("bucket_id", bucket.ID.String()),
                zap.Error(delErr),
            )
        }
        c.JSON(http.StatusBadGateway, gin.H{
            "error": middleware.NewErrorBody("MINIO_ERROR", "failed to create MinIO bucket: "+mkErr.Error(), nil),
        })
        return
    }
}

c.JSON(http.StatusCreated, toBucketResponse(bucket))
```

> 注：`DeleteBucket` 需要在 `BucketsDB` 接口和 `Queries` 中实现（对应 SQL：`DELETE FROM buckets WHERE id = $1`）。若 MinIO 不可用属于预期降级场景（如 MinIO 可选部署），可通过配置项 `MINIO_REQUIRED=true/false` 控制是否回滚。

**验收标准**：
- MinIO 不可达时：`POST /api/v1/buckets` 返回 `502`，DB 中无新记录
- MinIO 可达但名称合法时：`POST /api/v1/buckets` 返回 `201`，MinIO 物理 Bucket 存在
- `events_test.go` 补充 `TestCreateBucket_MinIOFails_RollsBackDB` 用例
- CP 日志中 MinIO 失败不再出现孤立的 error 日志后跟着 201 响应

---

#### T3-2-BUG-D-3 — WebUI：创建 Bucket 表单增加客户端命名校验

| 字段 | 内容 |
|------|------|
| **优先级** | 🟡 P1 |
| **涉及模块** | webui |
| **输入** | Bucket 创建表单中 `name` 字段输入 |
| **输出** | 非法命名时表单实时提示，不发出 API 请求 |

**目标**：在前端提前拦截非法 Bucket 名称，提升用户体验，减少无效请求。

**修复方案**：

- **文件**：创建 Bucket 的表单组件（根据 UI 实现位置定位，可能在 `webui/src/pages/Buckets/` 或 Modal 内联表单）

在 `name` 字段上添加 antd `validator`：

```typescript
rules={[
  { required: true, message: '请输入 Bucket 名称' },
  {
    validator: (_, value) => {
      if (!value) return Promise.resolve()
      if (!/^[a-z0-9][a-z0-9\-]{1,61}[a-z0-9]$/.test(value)) {
        return Promise.reject(
          'Bucket 名称只能包含小写字母、数字和连字符，长度 3-63，不能以连字符开头或结尾'
        )
      }
      return Promise.resolve()
    },
  },
]}
```

**验收标准**：
- 输入 `radar_data`（含下划线）→ 表单实时显示校验错误，提交按钮不可用
- 输入 `ok-bucket` → 校验通过，可提交
- `pnpm test` 相关用例通过

---

## T3-2-FIX Smoke Test 验收标准（参考）

完成所有 T3-2-FIX-A~L 后，执行以下端到端验证（已于 2026-05-11 前完成）：

1. 启动本地 `deploy/docker-compose.dev.yml` + controlplane + webui
2. 通过 grpcurl 注册一个 agent
3. 打开浏览器 → 采集器列表 → **能看到该 agent 的行**，主机名、IP、状态徽章颜色正确
4. 切换到「待审批」Tab → agent 可见，「审批」按钮出现
5. 点击审批 → agent 状态更新为 APPROVED
6. 点击 agent 名称 → 详情页所有字段有值（无 `Invalid Date`）
7. 规则 Tab 创建一条规则 → 规则出现在列表中
8. 上传文件到 MinIO → 文件浏览器**能看到该文件行**
9. 全局上传日志页**能显示记录**，时间、大小、状态字段均正确
10. 仪表盘「总文件数」> 0，「近期日志」Table 有数据
11. Bucket、文件类型、事件规则、用户列表各页面正常显示数据
