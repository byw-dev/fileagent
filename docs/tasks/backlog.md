# backlog.md — 待规划任务

> 本文件记录尚未排期的低优先级任务。Agent 无需读取，仅供人工规划参考。

---

## Phase 4 — 完善与收尾（可并行）

**前置依赖**：Phase 3 全部完成

| ID | 任务 | 内容摘要 | 状态 |
|----|------|---------|------|
| T4-1 | 监控配置 | Prometheus 抓取端点 + Grafana Dashboard 模板 + 告警规则 + Loki/Promtail 日志采集 | ⬜ |
| T4-2 | CI 配置 | GitHub Actions Workflows：controlplane / agent / webui / sdk-python 四条流水线（lint + test + build） | ⬜ |
| T4-3 | 部署脚本与文档 | systemd service 文件；Windows 安装脚本（NSSM 封装）；README 快速上手指南；运维手册 | ⬜ |
| T4-4 | Java SDK | OkHttp3 + Jackson + Lombok；JUnit 5 + MockWebServer；功能对齐 Python SDK | ⬜ |

---

## 已知技术债（低优先级，Phase 4 可处理）

| 描述 | 来源 | 优先级 |
|------|------|--------|
| `auth.go OIDCCallback` 始终返回 501（设计文档标注为"预留扩展"，可接受） | phase3-readiness-audit.md B-7 | ⚪ P3 |
| `append_mode=tail` 断点续传的 SQLite schema migration（当前直接修改 schema const） | P3-P4 | ⚪ P3 |
| 前后端 JSON 契约应引入 OpenAPI spec 自动校验，避免再次出现 T3-2-FIX 类问题 | T3-2-FIX 根本原因复盘 | ⚪ P3 |
| Web UI 页面使用 MSW（Mock Service Worker）补充真实 API 格式的集成测试 | T3-2-FIX 根本原因复盘 | ⚪ P3 |
