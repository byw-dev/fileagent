# phase-2.md — Phase 2 核心业务逻辑（已归档）

> **状态**：✅ 已完成

---

## 完成时间

- 2026-04-30~05-02：Phase 2 核心业务逻辑完成
- 2026-05-03~05-05：Phase 2 遗留工作扫除完成（T2-X1~T2-X8）

---

## 核心业务逻辑完成项（2026-04-30~05-02）

### Control Plane（组 A）

T2-A1 JWT 认证模块 / T2-A2 Agent 注册审批 / T2-A3 Agent 连接注册表 / T2-A4 文件索引引擎 / T2-A5 STS 凭据管理 / T2-A6 任务调度下发 / T2-A7 事件规则引擎 / T2-A8 用户认证接口

### Edge Agent（组 B）

T2-B1 注册审批流程 / T2-B2 File Watcher / T2-B3 Scheduler / T2-B4 Upload Engine / T2-B5 Task Executor

### Web UI（组 C）

T2-C1 登录页与认证 / T2-C2 仪表盘 / T2-C3 采集器列表与详情 / T2-C4 采集规则创建表单 / T2-C5 文件浏览器

### Python SDK（组 D）

T2-D1 FilesResource / T2-D2 其他 Resource（FileTypes/Agents/UploadLogs）

---

## 遗留扫除完成项（2026-05-03~05-05）

| 任务 | 内容摘要 |
|------|---------|
| T2-X1 | DB 查询层补全（file_entries/upload_logs/file_types/buckets/event_rules/event_deliveries/users） |
| T2-X2 | Controlplane 组件接线（RefreshCredentials RPC、Indexer、Dispatcher、Event Engine、JWT 黑名单） |
| T2-X3 | 离线规则暂存（已归并至 T2-X2：SyncRulesOnConnect 全量补发） |
| T2-X4 | REST Handler 实现（33 个端点：Agents/Files/FileTypes/Buckets/EventRules/UploadLogs/Users/MinioEvent） |
| T2-X5 | Agent main.go 组装（10 步完整进程组装） |
| T2-X6 | Agent RefreshCredentials 调用（grpcclient + 后台定时刷新 STS 凭据） |
| T2-X7 | Web UI 未实现页面（13 个 placeholder 页面） |
| T2-X8 | 集成测试补建（STS 集成测试 + Upload Engine 集成测试） |
