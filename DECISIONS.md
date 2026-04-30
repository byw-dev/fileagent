# DECISIONS.md — 技术决策记录

> 此文件记录项目中所有重要的技术决策，包括选择原因和替代方案。
> 修改任何契约文件（proto / 迁移 / Docker Compose 端口）前，必须在此记录。

---

## D-001 Go 模块结构与工作空间（T0-5）

**决策日期**：2026-04-30  
**影响范围**：controlplane / agent / api/v1

### 决策

采用 Go Workspace（go.work）将三个独立 Go 模块整合到同一个 Monorepo：

| 模块路径 | 模块名称 |
|---------|---------|
| `/`（根目录） | `github.com/byw-dev/fileagent` |
| `controlplane/` | `github.com/byw-dev/fileagent/controlplane` |
| `agent/` | `github.com/byw-dev/fileagent/agent` |

根模块（`github.com/byw-dev/fileagent`）负责管理 `api/v1/` 下的 gRPC 生成代码。  
`controlplane` 和 `agent` 在 Phase 1 实现 gRPC 调用时，通过 go.work 的本地替换机制引用根模块，无需发布。

### 有效 Go 最低版本

计划目标版本为 Go 1.22，但由于 `google.golang.org/grpc@v1.79.3`（安全补丁版本）
的传递依赖 `golang.org/x/crypto@v0.46.0` 要求 Go 1.24，三个模块的 `go` 指令均
自动设置为 **go 1.24.x**。

**已知安全约束**：
- `google.golang.org/grpc@v1.79.3`：CVE 修复版本（路径权限绕过漏洞）；<1.79.3 受影响。
- `github.com/jackc/pgx/v5@v5.9.0`：CVE 修复版本（内存安全）；需 Go 1.25，**暂未引入**。
  Phase 1 T1-A2 运行 sqlc 代码生成后，待确认 pgx 可用的兼容版本后再添加。

### 备选方案（被否决）

- **单一根 go.mod**：会将 controlplane 和 agent 的依赖混在一起，不利于二进制隔离。
- **offset 参数 -go=1.22 强制保留**：go mod tidy 会因传递依赖版本冲突而报错，无法满足验收标准。

---

## D-002 依赖锚定机制（T0-5）

**决策日期**：2026-04-30  
**影响范围**：controlplane / agent

### 决策

在 `controlplane/tools.go` 和 `agent/tools.go` 中使用 `//go:build tools` 构建标签
对所有 Phase 1 将要用到的依赖进行空白导入（`_ "pkg"`）。

此方式确保：
- `go mod tidy` 不会删除这些依赖（tidy 会读取所有构建约束文件）。
- 正常 `go build ./...` 不会编译这些占位文件（构建标签 `tools` 不在默认标签集内）。
- Phase 1 开发者可以直接 `import` 相应包，无需再 `go get`。

---

## D-003 统一构建约定：Makefile + bin/ 输出目录

**决策日期**：2026-04-30  
**影响范围**：controlplane / agent / CI / 开发者工作流

### 背景

Go 编译产物在 Linux/macOS 上没有固定扩展名，仅靠 `.gitignore` 中的 `*.exe`
等后缀规则无法拦截；历史上曾发生过将二进制直接提交进 git 的情况。

### 决策

1. **所有二进制输出统一写入项目根目录的 `bin/` 目录**。  
   `.gitignore` 追加 `/bin/`，确保编译产物不进入版本控制。

2. **根目录创建 `Makefile` 作为统一构建入口**，提供以下 target：

   | target | 说明 |
   |--------|------|
   | `make build` | 构建全部二进制（controlplane + agent） |
   | `make build-controlplane` | 仅构建 controlplane，输出 `bin/controlplane` |
   | `make build-agent` | 仅构建 agent，输出 `bin/agent` |
   | `make test` | 运行全部单元测试 |
   | `make tidy` | 整理所有模块 `go.mod` / `go.sum` |
   | `make clean` | 删除 `bin/` 目录 |

3. **`cmd/` 目录结构保持不变**（`controlplane/cmd/server/`、`agent/cmd/agent/`）。  
   这符合 Go 社区 [golang-standards/project-layout](https://github.com/golang-standards/project-layout) 的事实标准，
   被 Kubernetes、Prometheus、etcd 等主流项目采用：每个子目录名即二进制名。

### 备选方案（被否决）

- **各模块目录内各自 `go build`，输出到模块自身目录**：产物位置分散，CI 脚本难以统一收集。
- **根目录 `go build ./...`**：多模块 workspace 下行为不直观，无法控制各二进制输出名称。

---
