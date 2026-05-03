# FileAgent Web UI

FileAgent 管理后台，基于 React 19 + Ant Design Pro 构建，提供采集器管理、文件浏览、规则配置等功能。

---

## 技术栈

| 工具 | 版本 | 说明 |
|------|------|------|
| Node.js | **24.x LTS (krypton)** | 运行时，使用 Corepack 版本锁定 |
| pnpm | **11.x** | 包管理器，通过 `packageManager` 字段强制版本 |
| React | 19.x | UI 框架 |
| TypeScript | 6.x | 类型系统 |
| Vite | 8.x | 构建工具 |
| Ant Design | 5.x + ProComponents | UI 组件库 |
| Zustand | 5.x | 状态管理 |
| Axios + SWR | — | HTTP 请求 + 数据同步 |
| Vitest | 4.x | 单元测试 |

---

## 版本管理

项目使用 **Corepack + `packageManager` 字段**来锁定 Node.js 和 pnpm 版本，确保所有开发者和 CI 环境一致。

`package.json` 中已声明：

```json
{
  "packageManager": "pnpm@11.0.4",
  "engines": {
    "node": ">=24.0.0 <25.0.0",
    "pnpm": ">=11.0.0 <12.0.0"
  }
}
```

---

## 快速开始

### 1. 启用 Corepack（仅首次，一次性操作）

Corepack 是 Node.js 内置工具（Node.js 16.9+ 自带），负责按 `packageManager` 字段
自动使用正确版本的 pnpm，无需手动安装 pnpm。

```bash
corepack enable
```

> 如果使用 nvm / fnm 管理 Node.js 版本，确保当前激活版本为 **Node.js 24.x**：
> ```bash
> nvm use 24        # 或: fnm use 24
> node --version    # 应输出 v24.x.x
> ```

### 2. 安装依赖

```bash
cd webui
pnpm install
```

Corepack 会自动匹配并使用 `pnpm@11.0.4`，无需手动安装 pnpm。

### 3. 启动开发服务器

```bash
pnpm dev
```

开发服务器默认运行在 `http://localhost:5173`。

---

## 常用命令

| 命令 | 说明 |
|------|------|
| `pnpm dev` | 启动开发服务器（HMR 热更新） |
| `pnpm build` | TypeScript 编译 + Vite 生产构建，产物输出到 `dist/` |
| `pnpm preview` | 本地预览 `dist/` 中的生产构建 |
| `pnpm lint` | ESLint 代码检查 |
| `pnpm test` | 运行所有单元测试（Vitest） |
| `pnpm test:watch` | 以监听模式运行测试 |
| `pnpm test:coverage` | 生成测试覆盖率报告（输出到 `coverage/`） |

---

## 项目结构

```
webui/
├── src/
│   ├── pages/                # 页面组件
│   │   ├── Login/            # 登录页
│   │   ├── Dashboard/        # 仪表盘
│   │   ├── Agents/           # 采集器管理（列表、详情、审批）
│   │   ├── Files/            # 文件浏览器
│   │   ├── FileTypes/        # 文件类型管理
│   │   ├── Events/           # 事件规则
│   │   ├── Buckets/          # 存储桶管理
│   │   ├── Logs/             # 上传日志
│   │   └── Settings/         # 系统设置
│   ├── services/             # API 服务层（Axios 封装）
│   ├── store/                # Zustand 状态 store
│   ├── components/           # 公共组件
│   ├── __tests__/            # 单元测试
│   ├── App.tsx               # 路由与布局
│   └── main.tsx              # 应用入口
├── public/                   # 静态资源
├── dist/                     # 生产构建产物（gitignored）
├── package.json
├── pnpm-lock.yaml            # 锁定依赖版本，请勿手动修改
├── vite.config.ts
├── tsconfig.json
└── eslint.config.js
```

---

## 连接后端

开发时前端默认请求 `http://localhost:8080`（Control Plane REST API）。
如需修改，在 `webui/` 目录下创建 `.env.local` 文件：

```env
VITE_API_BASE_URL=http://localhost:8080
```

---

## CI 注意事项

CI 环境中应使用 `--frozen-lockfile` 以确保安装的依赖与 `pnpm-lock.yaml` 完全一致：

```bash
corepack enable
pnpm install --frozen-lockfile
pnpm build
pnpm test
```

