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

### 路径规则：go.work 与 go.mod 中只允许使用相对路径

`go.work` 的 `use` 指令和 `go.mod` 的 `replace` 指令均支持绝对路径，但**本项目禁止使用绝对路径**：

- 绝对路径与机器环境绑定，提交到仓库后任何其他开发者 clone 均会立即构建失败。
- 相对路径相对于 `go.work` / `go.mod` 所在目录解析，在任意机器上都可重现。

**强制规则**：
- `go.work` 中的 `use` 指令只能写 `./子目录` 形式（如 `./agent`、`./controlplane`）。
- 任何 `go.mod` 中若需要 `replace`，目标路径也必须是相对路径（如 `replace foo => ../foo`）。

### 备选方案（被否决）——D-001

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

## D-004：Agent Token 哈希算法改用 SHA-256

- **日期**：2026-04-30
- **状态**：已接受

### 背景

JWT 访问令牌长度通常超过 72 字节。bcrypt 在处理超过 72 字节的输入时会静默截断，
导致不同 token 可能哈希到相同值，产生碰撞风险。

### 决策

在 `internal/agent/manager.go` 中，存储 Agent Token 时改用 `SHA-256` hex 哈希
（而非 bcrypt）。SHA-256 输出固定 64 字节 hex 字符串，不存在截断问题，且计算速度更快。

验证时同样计算 SHA-256 hex 与存储值对比，不再使用 bcrypt.CompareHashAndPassword。

### 备选方案

- **继续用 bcrypt，截断到 72 字节**：不可接受，不同 token 可能碰撞。
- **用 bcrypt，先 base64 encode**：引入复杂度，且 base64 输出仍可能超 72 字节。

---

## D-005：controlplane 覆盖率统计口径

- **日期**：2026-04-30
- **状态**：已接受（修订于 2026-05-01）

### 背景

`internal/db` 包是 **sqlc 自动生成代码**，所有函数都需要真实 PostgreSQL 连接，
单元测试环境无法覆盖（原始状态 0%）。若将其计入总覆盖率，整体数字会被拉低至 ~54%。

### 决策

1. 采用 `go-sqlmock v1.5.2` 为 sqlc 生成的 query 函数编写单元测试（使用 `sqlmock.New()` 返回
   实现了 `database/sql` 标准接口的 mock DB，与 sqlc 生成代码的 `DBTX` 接口完全兼容）。
2. 同步对 `internal/event`、`internal/indexer` 等包抽象 `EngineStore` / `IndexerStore` 接口，
   通过 mock 实现完成业务逻辑单元测试覆盖。
3. 当前状态（Phase 2 Group A 完成后，含 go-sqlmock 测试）：
   - 总覆盖率（含 `internal/db`）：**81.2%**（≥ 80% 阈值 ✓）
   - `internal/db`：80.8%，`internal/indexer`：91.4%，`internal/event`：87.5%
   - `internal/grpcserver`：87.0%，核心业务包均 ≥ 80%
4. 集成测试（`go test -tags=integration`）覆盖真实 DB 路径，满足端到端验证要求。

### 备选方案

- **排除 `internal/db` 统计**：考虑过但未采纳；通过 go-sqlmock 实现了包含 DB 层的完整覆盖。
- **使用 `go-sqlmock/v2`**：v2 不存在于代理缓存中，改用 v1.5.2（API 稳定，无已知 CVE）。
- **接受 <80% 总覆盖率**：不符合 AGENTS.md 要求，已通过 sqlmock 测试解决。

---

## D-006：Web UI 运行时版本选型（Node.js + pnpm）

- **日期**：2026-05-03
- **状态**：已接受

### 背景

项目初始时使用 Node.js 20 LTS（Iron）+ pnpm 9，但 Node.js 20 LTS 的 EOL 已于
2026 年 4 月到来，继续使用会失去安全维护。同时评估了是否直接跳至最新版本。

### 决策

| 工具 | 版本 | 说明 |
|------|------|------|
| Node.js | **24.15.0 (LTS krypton)** | 当前活跃 LTS，EOL ≈ 2030 年 4 月，提供最长支持窗口 |
| pnpm | **11.0.4** | 与 Node.js 24 一同发布的最新稳定版，lockfile 格式（v9.0）与 pnpm 9.x 兼容，无破坏性迁移 |
| Corepack | 内置于 Node.js 16.9+ | 通过 `package.json` 的 `packageManager` 字段强制版本一致性 |

版本通过 `webui/package.json` 的以下字段锁定，Corepack 会在执行 `pnpm install` 时
自动检测并强制使用 `pnpm@11.0.4`：

```json
{
  "packageManager": "pnpm@11.0.4",
  "engines": {
    "node": ">=24.0.0 <25.0.0",
    "pnpm": ">=11.0.0 <12.0.0"
  }
}
```

### 升级可行性评估

**优势：**
- Node.js 24 LTS 支持周期到 2030 年，比 Node.js 20（已 EOL）或 22（2027 年）更长
- V8 12.4 引擎提升：原生 `fetch`、更好的 ESM 模块支持、内存性能改善
- Node.js 24 开始默认启用 Corepack，`corepack enable` 后无需额外安装 pnpm
- pnpm 11 改进了 workspace 协议处理和 peer deps 解析
- 当前所有依赖（Vite 8、React 19、TypeScript 6）均明确支持 Node.js 24

**无副作用确认：**
- pnpm 11 的 lockfile 格式（`lockfileVersion: '9.0'`）与 pnpm 9.x 相同，无迁移成本
- `pnpm install --frozen-lockfile`、`pnpm build`、`pnpm test`（48/48）全部通过
- 无任何依赖兼容性错误或警告

### 开发者一致性机制

1. 运行 `corepack enable`（一次性操作，或通过 CI 脚本自动完成）
2. `pnpm install` — Corepack 自动匹配并使用 `pnpm@11.0.4`，无需手动安装 pnpm
3. CI 中使用 `pnpm install --frozen-lockfile` 防止 lockfile 意外变更

### 备选方案（被否决）

- **继续用 Node.js 20 + pnpm 9**：Node 20 已 EOL，无安全更新，不可接受。
- **升级到 Node.js 22 LTS（Jod）**：支持到 2027 年 4 月，比 Node 24 短 3 年；既然升级，直接到更长生命周期版本更合理。
- **使用 node-version-file（.nvmrc / .node-version）**：与 `packageManager` + Corepack 方案相比，需要额外工具（nvm/fnm），且对 pnpm 版本无约束力。

---
