# phase-0.md — Phase 0 契约定义（已归档）

> **状态**：✅ 已完成

---

## 完成时间

2026-04-26~04-28

---

## 完成项

| 任务 | 产出 |
|------|------|
| T0-1 proto 文件 | `proto/v1/agent.proto`（4 个 RPC，含全部消息类型） |
| T0-2 数据库迁移文件 | `controlplane/migrations/000001_init_schema.up/down.sql`（7 枚举，12 表，全部索引） |
| T0-3 Docker Compose 文件 | `deploy/docker-compose.dev.yml` / `deploy/docker-compose.test.yml` |
| T0-4 MinIO 初始化脚本 | `deploy/scripts/init-minio.sh` |
| T0-5 go.mod 初始化 | Go Workspace（go.work）；有效最低版本 1.24（见 DECISIONS.md D-001） |
