# 文件采集同步存储分发系统

系统设计文档

v1.0  第一版共十章

2025

---

## 目录

- 第一章 系统概述
  - 1.1 背景与目标
  - 1.2 核心功能范围
  - 1.3 系统边界与角色定义
  - 1.4 整体架构图
- 第二章 架构设计
  - 2.1 组件职责划分
  - 2.2 控制平面与数据平面分离
  - 2.3 通信协议
  - 2.4 TLS 加密策略
  - 2.5 高可用与扩展路径
- 第三章 数据模型
  - 3.1 设计原则
  - 3.2 实体关系概述
  - 3.3 完整 PostgreSQL Schema
  - 3.4 索引策略
  - 3.5 Redis 数据结构设计
- 第四章 Agent 设计
  - 4.1 Agent 架构与模块划分
  - 4.2 生命周期状态机
  - 4.3 gRPC 协议定义（完整 .proto）
  - 4.4 采集模式实现
  - 4.5 文件上传流程
  - 4.6 本地任务队列（SQLite）
  - 4.7 凭据管理与轮转
  - 4.8 跨平台适配要点
  - 4.9 Go 项目结构
- 第五章 Control Plane 设计
  - 5.1 模块划分
  - 5.2 Agent 连接管理
  - 5.3 用户认证模块
  - 5.4 用户与权限模型
  - 5.5 Agent 注册审批流程
  - 5.6 任务调度与指令下发
  - 5.7 STS 凭据管理与轮转
  - 5.8 文件索引与归类引擎
  - 5.9 事件规则引擎
  - 5.10 上传日志记录
  - 5.11 对外 REST API 设计
  - 5.12 Go 项目结构
- 第六章 存储层设计
  - 6.1 MinIO 部署模式
  - 6.2 Bucket 规划与命名约定
  - 6.3 ACL 与 Policy 设计
  - 6.4 STS AssumeRole 凭据下发机制
  - 6.5 事件通知配置
  - 6.6 MinIO 管理功能在后台的集成
  - 6.7 存储容量规划参考
- 第七章 Web UI 设计
  - 7.1 技术选型
  - 7.2 页面模块清单
  - 7.3 核心页面设计
  - 7.4 文件下载实现
  - 7.5 事件规则配置界面
  - 7.6 日志查询界面
  - 7.7 前端项目结构
- 第八章 Client SDK 设计
  - 8.1 SDK 功能范围
  - 8.2 Python SDK
  - 8.3 Java SDK
  - 8.4 Token 管理实现细节
  - 8.5 分页迭代器实现
- 第九章 监控与运维
  - 9.1 监控组件栈
  - 9.2 各组件 Metrics 指标清单
  - 9.3 核心告警规则
  - 9.4 日志采集方案
  - 9.5 Grafana Dashboard 规划
- 第十章 部署方案
  - 10.1 All-in-One 单机部署（开发/小规模）
  - 10.2 生产最小化部署（节点规划）
  - 10.3 systemd Service 文件
  - 10.4 Caddy 网关配置
  - 10.5 内网 TLS 方案（step-ca）
  - 10.6 数据库初始化与迁移方案
  - 10.7 版本升级策略
- 附录 A 名词表
- 附录 B 接口速查
- 附录 C 配置项速查
- 附录 D 已知限制与后续迭代计划

---

# 第一章 系统概述

## 1.1 背景与目标

本系统旨在构建一套完全私有化部署的文件采集、同步、存储与分发平台，解决分布于不同地理位置的边缘设备上产生的文件数据难以统一管理、采集和分发的问题。

核心目标如下：

- 将运行在边缘设备（Windows/Linux）上的采集器统一纳管，支持远程下发采集规则与指令；
- 采集到的文件上传至私有化对象存储，具备完善的访问控制与事件通知能力；
- 提供 Web 管理界面，支持采集器注册审批、状态监控、文件检索与下载；
- 对外提供 REST API 与 Client SDK，支持内部系统集成；
- 所有组件均可私有化部署，无外部依赖。

## 1.2 核心功能范围

**MVP（第一版）范围：**

| 模块 | 功能 | 说明 |
|------|------|------|
| **采集器管理** | 注册、审批、吊销、状态监控 | 支持数十台，可扩展至数百台 |
| **指令下发** | 列目录、下发采集任务、配置规则 | gRPC 双向流 |
| **文件采集** | Watch 模式 / Scheduled 模式 | 支持 inotify 与定时轮询混合 |
| **文件上传** | 分片上传、断点续传、幂等去重 | Agent 直传 MinIO |
| **对象存储** | Bucket 管理、ACL、事件通知 | MinIO MNMD |
| **文件索引** | 路径规则归类、文件条目查询 | PostgreSQL |
| **Web UI** | 管理后台、文件浏览与下载 | React + Ant Design Pro |
| **事件系统** | 监听上传/采集器上下线，Webhook/MQ | NATS 事件总线 |
| **用户认证** | 内置账号 + JWT，预留 OIDC 扩展 | |
| **Client SDK** | Python / Java，文件查询与下载 | 对内部系统开放 |
| **监控运维** | Prometheus + Grafana + Loki | 指标、日志、告警 |

**后续迭代（MVP 不包含）：**

- 多租户 / 多组织隔离（数据模型已预留 org_id）
- LDAP / OIDC / SSO 对接
- Electron 桌面端文件浏览器
- Agent 端 mTLS 双向证书认证
- Client SDK Go 语言版本

## 1.3 系统边界与角色定义

| 角色             | 描述            | 典型操作               |
|----------------|---------------|--------------------|
| **超级管理员**      | 系统唯一最高权限用户    | 用户管理、采集器审批、系统配置    |
| **普通管理员**      | 操作采集器与文件规则    | 下发任务、查看日志、配置事件规则   |
| **只读用户**       | 查询和下载文件       | 文件搜索、预签名下载         |
| **Edge Agent** | 运行在边缘设备上的采集程序 | 接收指令、扫描文件、上传 MinIO |
| **SDK 调用方**    | 内部系统通过 SDK 接入 | 查询文件条目、获取下载链接      |

## 1.4 整体架构图

系统由五个层次组成：

```
┌─────────────────────────────────────────────────────────────────┐
│                      私有化部署边界                             │
│                                                                 │
│  ┌──────────────┐   HTTPS    ┌──────────────────────────────┐   │
│  │   Web UI     │──────────►│       Control Plane (Go)      │   │
│  │ React/AntD   │           │  - Agent 注册审批 & 连接管理   │  │
│  └──────────────┘           │  - 任务调度 & 指令下发         │  │
│                             │  - 文件索引 & 查询 API         │  │
│  ┌──────────────┐   HTTPS   │  - 事件规则引擎                │  │
│  │ Client SDK   │──────────►│  - STS 凭据管理 & 轮转         │  │
│  │ Python/Java  │           └──┬────────┬──────────────▲────┘   │
│  └──────────────┘              │        │              │        │
│                       gRPC/TLS │        │ Admin API    │ 事件   │
│  ┌──────────────┐              │        │              │ 通知   │
│  │  Edge Agent  │◄─────────────┘   ┌────▼────────┐     │        │
│  │  (Go 单二进制)│                  │    MinIO    │─────┘       │
│  │  Win/Linux   │  S3 API/TLS(直传) │  对象存储   │             │
│  └──────────────┘─────────────────►│  MNMD 集群  │              │
│                                    └─────────────┘              │
│                                                                 │
│  ┌───────────────────────────────────┐                          │
│  │  PostgreSQL  │ Redis │ NATS       │◄── 仅 Control Plane 读写 │
│  │  主数据库    │ 缓存  │ 事件总线   │                          │
│  └───────────────────────────────────┘                          │
│                                                                 │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │        Prometheus + Grafana + Loki + Alertmanager         │  │
│  └───────────────────────────────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
```

> **对象事件通路（现状 / 目标）**：MinIO 的 `ObjectCreated` / `ObjectRemoved` 事件经
> **HTTP webhook** 直接投递给 Control Plane（`POST /internal/minio-event`，见 §6.5），
> **不经过 NATS**。NATS 在本系统中只承担 Control Plane 内部的事件发布与规则引擎消费。
> 本文早期版本的架构图与 §2.1 曾描述为「MinIO → NATS」，与 §6.5 的配置自相矛盾且从未实现——
> 已按实际链路更正。目标形态是改用 `notify_nats` + JetStream，见 **D-031**（排期在对账阶段 IC-11）。

---

# 第二章 架构设计

## 2.1 组件职责划分

| 组件                | 技术选型             | 职责                                              |
|-------------------|------------------|-------------------------------------------------|
| **Control Plane** | Go (Gin)         | 核心业务逻辑：Agent 管理、任务调度、文件索引、API、事件引擎              |
| **Edge Agent**    | Go (单二进制)        | 文件采集、上传、本地任务队列、与 Control Plane 长连接              |
| **MinIO**         | MinIO MNMD       | 对象存储：Bucket 管理、ACL、STS、事件通知（→ CP webhook，D-031 后改投 JetStream） |
| **PostgreSQL**    | v15+             | 持久化：采集器、用户、文件索引、上传日志、事件规则                       |
| **Redis**         | v7+              | Token 缓存、限流、短期状态、任务锁                            |
| **NATS**          | v2.x（已开 JetStream） | 事件总线：Control Plane 内部事件发布 + 规则引擎消费、Webhook 分发。**不接收 MinIO 事件**（现状为 HTTP webhook，见 §6.5；目标改 `notify_nats`+JetStream，见 D-031）。当前代码仅用 core NATS |
| **Nginx/Caddy**   | Caddy v2         | TLS 终止、反向代理、自动证书（公网 Let's Encrypt / 内网 ACME CA） |
| **Web UI**        | React + AntD Pro | 管理后台、文件浏览器、事件规则配置                               |
| **Prometheus**    | + Alertmanager   | 指标采集与告警                                         |
| **Grafana**       | + Loki           | 指标可视化、日志聚合与查询                                   |

## 2.2 控制平面与数据平面分离

系统严格区分控制平面（Control Plane）与数据平面（Data Plane）：

- **控制平面**：负责所有管理类通信，包括 Agent 注册、指令下发、状态同步、凭据管理。流量经过 Control Plane 服务器。
- **数据平面**：文件实际上传流量。Agent 获得短期 STS 凭据后，直接向 MinIO 上传，不经过 Control Plane 中转，避免服务器成为带宽瓶颈。

```
控制平面流量：Agent ──gRPC/TLS──► Control Plane ◄──HTTPS── Web UI / SDK
数据平面流量：Agent ──S3 API/TLS──────────────────────────► MinIO

指令下发路径：Control Plane ──gRPC 服务端推送──► Agent
凭据下发路径：Control Plane ──gRPC──► Agent（含 STS AK/SK/Token，TTL 1h）
日志上报路径：Agent ──gRPC 流──► Control Plane ──写入──► PostgreSQL
```

## 2.3 通信协议

| 通信链路                         | 协议                 | 说明                                         |
|------------------------------|--------------------|--------------------------------------------|
| Agent ↔ Control Plane 控制信道   | gRPC / HTTP2 / TLS | 双向流：服务端推送指令，Agent 上报状态和日志                  |
| Agent → MinIO 上传             | HTTPS (S3 API)     | 分片上传，Agent 持有短期 STS 凭据，直传不中转               |
| Web UI / SDK → Control Plane | HTTPS REST         | JSON，JWT Bearer 认证                         |
| MinIO → NATS 事件              | Webhook (HTTP)     | MinIO 将对象事件推送到 NATS HTTP 接入点               |
| Control Plane → NATS         | NATS Client        | 发布 Agent 上下线等内部事件，触发事件规则                   |
| Agent 下载预签名 URL              | HTTPS              | Control Plane 生成 MinIO 预签名 URL，SDK/浏览器直接下载 |

**选用 gRPC 的核心原因：**

- Protobuf 强类型定义，协议即文档，便于 Agent 与服务端独立演进；
- 原生支持服务端推送（Server Streaming），适合指令下发场景；
- 双向流支持 Agent 状态上报与日志推送；
- Go 生态对 gRPC 支持成熟，性能优秀；
- 单实例可维持数万并发连接，满足数百台 Agent 的规模需求。

## 2.4 TLS 加密策略

所有外部通信均要求 TLS 加密。根据部署环境分两种模式：

### 公网部署

- 使用 Caddy 作为边缘网关，自动申请并续期 Let's Encrypt 证书；
- Control Plane 和 MinIO 在内网监听，Caddy 反向代理后对外暴露 HTTPS；
- Agent 内置根 CA 信任链（操作系统默认信任），连接时验证服务端证书。

### 内网/离线部署

- 使用 step-ca 自建内部 CA，实现 ACME 协议，支持证书自动申请和续期；
- Caddy 对接 step-ca ACME 接口，流程与公网一致，只是 CA 换成内部；
- Agent 安装包内置内部根证书，部署时植入操作系统信任链；
- MinIO 配置 step-ca 签发的服务端证书，Control Plane 信任同一根 CA。

```
内网 TLS 信任链：
  step-ca (Root CA)
    └── 签发 ──► Control Plane 服务端证书（CN: control.internal）
    └── 签发 ──► MinIO 服务端证书（CN: minio.internal）
    └── 签发 ──► Caddy 网关证书
  Agent 内置 Root CA 证书 → 信任所有上述服务端证书
```

**关于 mTLS（双向证书认证）：**

第一版不启用 mTLS，Agent 身份通过注册审批流程颁发的 Auth Token（JWT）验证。后续版本可为每台 Agent 签发客户端证书，实现双向认证，作为 Token 之外的第二层防护。

## 2.5 高可用与扩展路径

系统设计遵循"单实例起步，平滑扩展"原则：

| 组件                | 单实例（MVP）       | HA 扩展方式               | 注意事项                    |
|-------------------|----------------|-----------------------|-------------------------|
| **Control Plane** | 1 实例 + systemd | 多实例 + HAProxy L4      | 需无状态设计，gRPC 会话状态存 Redis |
| **PostgreSQL**    | 单主实例           | 主从 + Patroni 自动故障转移   | 只读查询走从库                 |
| **Redis**         | 单实例            | Redis Sentinel（3节点）   |                         |
| **MinIO**         | 4节点 MNMD 起步    | 追加 Server Pool        | Pool 内节点数固定，新 Pool 无缝追加 |
| **NATS**          | 单实例            | 3节点 JetStream Cluster |                         |
| **Caddy**         | 单实例            | DNS 轮询 + 多实例          | 证书需共享存储或集中管理            |

**Control Plane 无状态化设计要点：**

- gRPC 长连接的 Agent 在线状态存 Redis（Key: `agent:{id}:online`，TTL 由心跳刷新）；
- JWT 验证无需查库，密钥存 Redis 便于多实例共享；
- 任务调度使用 Redis 分布式锁防止多实例重复下发；
- 上传文件后的回调写入任务队列（NATS），由任意 Control Plane 实例消费处理。

---

# 第三章 数据模型

## 3.1 设计原则

- 所有核心业务实体均携带 `org_id` 字段，第一版只有一个默认组织（id=1），多租户功能启用时无需迁移 Schema；
- 主键统一使用 UUID v4，避免分布式场景下的 ID 冲突；
- 时间字段统一使用 `TIMESTAMPTZ`（带时区），存储 UTC；
- 可变配置、扩展属性使用 JSONB 存储，保持 Schema 稳定性；
- 敏感字段（Token、密码）仅存哈希值，原文不落库。

## 3.2 实体关系概述

```
organizations (1)
    ├── users (N)                    # 管理后台用户
    ├── agents (N)                   # 采集器
    │       ├── collection_rules (N) # 采集规则（绑定到 Agent）
    │       ├── upload_logs (N)      # 上传日志
    │       └── agent_events (N)     # 上下线事件
    ├── buckets (N)                  # MinIO Bucket 登记
    ├── file_types (N)               # 文件逻辑分类
    │       └── file_type_rules (N)  # 路径匹配规则
    ├── file_entries (N)             # 文件索引条目
    └── event_rules (N)              # 事件响应规则
```

## 3.3 完整 PostgreSQL Schema（DDL）

### 3.3.1 基础扩展与初始化

```sql
CREATE EXTENSION IF NOT EXISTS "uuid-ossp";
CREATE EXTENSION IF NOT EXISTS "pg_trgm"; -- 用于文件路径模糊搜索

-- 枚举类型
CREATE TYPE agent_status AS ENUM ('pending','approved','online','offline','revoked');
CREATE TYPE upload_mode   AS ENUM ('watch','scheduled');
CREATE TYPE rule_status   AS ENUM ('active','inactive');
CREATE TYPE file_status   AS ENUM ('uploading','completed','failed','deleted');
CREATE TYPE event_type    AS ENUM ('file_uploaded','file_deleted','agent_online','agent_offline','agent_approved','agent_revoked');
CREATE TYPE action_type   AS ENUM ('webhook','nats_publish','kafka_publish');
CREATE TYPE user_role     AS ENUM ('super_admin','org_admin','org_viewer');
```

### 3.3.2 组织表（多租户预留）

```sql
CREATE TABLE organizations (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    name        VARCHAR(128) NOT NULL UNIQUE,
    description TEXT,
    metadata    JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 第一版默认组织（部署初始化时插入）
INSERT INTO organizations (id, name) VALUES
    ('00000000-0000-0000-0000-000000000001', 'default');
```

### 3.3.3 用户表

```sql
CREATE TABLE users (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id),
    username        VARCHAR(64) NOT NULL,
    email           VARCHAR(256),
    password_hash   VARCHAR(256) NOT NULL,      -- bcrypt
    role            user_role NOT NULL DEFAULT 'org_viewer',
    is_active       BOOLEAN NOT NULL DEFAULT TRUE,
    last_login_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, username)
);

-- OIDC 扩展预留：后续对接 SSO 时追加
-- ALTER TABLE users ADD COLUMN oidc_sub VARCHAR(256);
-- ALTER TABLE users ADD COLUMN oidc_provider VARCHAR(64);
```

### 3.3.4 采集器表

```sql
CREATE TABLE agents (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id              UUID NOT NULL REFERENCES organizations(id),
    name                VARCHAR(128) NOT NULL,
    fingerprint         VARCHAR(256) NOT NULL UNIQUE, -- 设备唯一标识
    status              agent_status NOT NULL DEFAULT 'pending',
    auth_token_hash     VARCHAR(256),                -- bcrypt(JWT jti)
    token_expires_at    TIMESTAMPTZ,
    os_info             JSONB NOT NULL DEFAULT '{}', -- OS类型、版本、主机名
    ip_address          INET,                        -- 最后一次连接 IP
    approved_by         UUID REFERENCES users(id),
    approved_at         TIMESTAMPTZ,
    revoked_by          UUID REFERENCES users(id),
    revoked_at          TIMESTAMPTZ,
    last_seen_at        TIMESTAMPTZ,
    metadata            JSONB NOT NULL DEFAULT '{}',
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### 3.3.5 MinIO Bucket 登记表

```sql
CREATE TABLE buckets (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id),
    name            VARCHAR(128) NOT NULL UNIQUE, -- MinIO bucket name
    description     TEXT,
    policy_json     JSONB,                        -- 存储 IAM Policy 快照
    sts_role_arn    VARCHAR(256),                 -- STS AssumeRole ARN
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### 3.3.6 采集规则表

```sql
CREATE TABLE collection_rules (
    id                    UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id                UUID NOT NULL REFERENCES organizations(id),
    agent_id              UUID NOT NULL REFERENCES agents(id) ON DELETE CASCADE,
    bucket_id             UUID NOT NULL REFERENCES buckets(id),
    name                  VARCHAR(128) NOT NULL,
    mode                  upload_mode NOT NULL,
    status                rule_status NOT NULL DEFAULT 'active',
    source_path_template  TEXT NOT NULL,
    file_glob             VARCHAR(256) NOT NULL DEFAULT '*',
    upload_path_template  TEXT NOT NULL,
    watch_recursive       BOOLEAN NOT NULL DEFAULT FALSE,
    watch_subdir_pattern  VARCHAR(256),
    cron_expr             VARCHAR(64),
    run_once_on_start     BOOLEAN NOT NULL DEFAULT FALSE,
    append_mode           VARCHAR(32) NOT NULL DEFAULT 'overwrite',
    metadata              JSONB NOT NULL DEFAULT '{}',
    created_at            TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at            TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### 3.3.7 文件类型表（逻辑归类）

```sql
CREATE TABLE file_types (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id      UUID NOT NULL REFERENCES organizations(id),
    name        VARCHAR(128) NOT NULL,
    description TEXT,
    created_by  UUID REFERENCES users(id),
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (org_id, name)
);

CREATE TABLE file_type_rules (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    file_type_id    UUID NOT NULL REFERENCES file_types(id) ON DELETE CASCADE,
    path_pattern    TEXT NOT NULL,
    priority        INT NOT NULL DEFAULT 0,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### 3.3.8 文件条目索引表

```sql
CREATE TABLE file_entries (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id),
    file_type_id    UUID REFERENCES file_types(id),
    agent_id        UUID REFERENCES agents(id),
    rule_id         UUID REFERENCES collection_rules(id),
    bucket_id       UUID NOT NULL REFERENCES buckets(id),
    storage_path    TEXT NOT NULL,
    original_path   TEXT,
    file_name       VARCHAR(512) NOT NULL,
    size_bytes      BIGINT NOT NULL DEFAULT 0,
    sha256          VARCHAR(64),
    etag            VARCHAR(128),
    content_type    VARCHAR(128),
    file_mtime      TIMESTAMPTZ,
    status          file_status NOT NULL DEFAULT 'uploading',
    uploaded_at     TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (bucket_id, storage_path)
);
```

> **文件元数据 / 标签模型（6c，Phase 1 · 已实现，D-025，PR #69–#79）**：在 `file_types`（粗分类，降级为兜底）之上
> 引入**受控标签**——`tag_keys` 词表 / `tag_values` 受控取值 / `file_tags`（文件↔key:value）/ `pending_tag_values`
> 待确认队列 / `tag_audit` 审计（迁移 `000004`），及回溯任务 outbox `retag_jobs`（迁移 `000005`）。规则在
> `collection_rules.metadata` 声明 `file_type + static_tags + path_tag_map`，CP 在索引阶段打标（不改 agent/proto）。
> 治理：**key 严格受控、value 受控可扩**，路径变量抽到的未登记值进待确认队列，核准/并入/拒绝后才进筛选器与规则可选项。
> **Phase 2（数据集注册表 / 衍生数据 / 血缘 run 模型）设计留存、按信号触发**。完整数据模型 DDL 见
> [`metadata-model.md`](./metadata-model.md)，落地细节见 `DECISIONS.md` D-025 各「落地记录」。

### 3.3.9 上传日志表

```sql
CREATE TABLE upload_logs (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id              UUID NOT NULL REFERENCES organizations(id),
    agent_id            UUID NOT NULL REFERENCES agents(id),
    file_entry_id       UUID REFERENCES file_entries(id),
    rule_id             UUID REFERENCES collection_rules(id),
    original_path       TEXT NOT NULL,
    storage_path        TEXT NOT NULL,
    size_bytes          BIGINT NOT NULL DEFAULT 0,
    bytes_transferred   BIGINT NOT NULL DEFAULT 0,
    status              VARCHAR(32) NOT NULL,
    error_message       TEXT,
    retry_count         INT NOT NULL DEFAULT 0,
    started_at          TIMESTAMPTZ NOT NULL,
    finished_at         TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

### 3.3.10 事件规则表

```sql
CREATE TABLE event_rules (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    org_id          UUID NOT NULL REFERENCES organizations(id),
    name            VARCHAR(128) NOT NULL,
    event_type      event_type NOT NULL,
    filter          JSONB NOT NULL DEFAULT '{}',
    action_type     action_type NOT NULL,
    action_config   JSONB NOT NULL,
    enabled         BOOLEAN NOT NULL DEFAULT TRUE,
    created_by      UUID REFERENCES users(id),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE event_deliveries (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    event_rule_id   UUID NOT NULL REFERENCES event_rules(id),
    event_type      event_type NOT NULL,
    payload         JSONB NOT NULL,
    status          VARCHAR(32) NOT NULL,
    response_code   INT,
    response_body   TEXT,
    attempt_count   INT NOT NULL DEFAULT 0,
    next_retry_at   TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    delivered_at    TIMESTAMPTZ
);
```

### 3.3.11 Agent 上下线事件表

```sql
CREATE TABLE agent_events (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    agent_id    UUID NOT NULL REFERENCES agents(id),
    event_type  VARCHAR(32) NOT NULL,
    ip_address  INET,
    detail      JSONB NOT NULL DEFAULT '{}',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
```

## 3.4 索引策略

```sql
-- 采集器查询
CREATE INDEX idx_agents_org_status   ON agents (org_id, status);
CREATE INDEX idx_agents_fingerprint  ON agents (fingerprint);
CREATE INDEX idx_agents_last_seen    ON agents (last_seen_at DESC);

-- 文件条目查询（高频）
CREATE INDEX idx_file_entries_org_type   ON file_entries (org_id, file_type_id, uploaded_at DESC);
CREATE INDEX idx_file_entries_agent      ON file_entries (agent_id, uploaded_at DESC);
CREATE INDEX idx_file_entries_bucket     ON file_entries (bucket_id, storage_path);
CREATE INDEX idx_file_entries_status     ON file_entries (status) WHERE status != 'completed';

-- 路径模糊搜索
CREATE INDEX idx_file_entries_path_trgm  ON file_entries USING gin (storage_path gin_trgm_ops);

-- 上传日志
CREATE INDEX idx_upload_logs_agent_time  ON upload_logs (agent_id, created_at DESC);
CREATE INDEX idx_upload_logs_status      ON upload_logs (status, created_at DESC);

-- 事件分发
CREATE INDEX idx_event_deliveries_retry  ON event_deliveries (next_retry_at) WHERE status = 'failed';
```

## 3.5 Redis 数据结构设计

| Key 模式                    | 类型     | TTL       | 用途              |
|---------------------------|--------|-----------|-----------------|
| `agent:{id}:online`       | STRING | 心跳间隔×3    | Agent 在线状态，心跳刷新 |
| `agent:{id}:sts`          | HASH   | STS 过期时间  | 当前下发的 STS 凭据摘要  |
| `jwt:jti:{jti}`           | STRING | Token 有效期 | 已吊销 Token 黑名单   |
| `ratelimit:api:{user_id}` | STRING | 1 分钟      | API 限流计数        |
| `lock:task:{rule_id}`     | STRING | 任务超时时间    | 定时任务分布式锁        |
| `session:{token}`         | HASH   | 30 分钟     | Web UI 用户会话（可选） |

---

# 第四章 Agent 设计

## 4.1 Agent 架构与模块划分

Edge Agent 是运行在边缘设备上的单一可执行二进制文件，使用 Go 编译，无外部运行时依赖。内部由以下模块组成：

```
┌─────────────────────────────────────────────────────────────┐
│                      Edge Agent 进程                        │
│                                                             │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────────┐  │
│  │  gRPC Client │  │ File Watcher │  │ Scheduler        │  │
│  │  连接管理    │  │ inotify/poll │  │ cron 定时任务    │  │
│  │  心跳/重连   │  │ 目录监控     │  │ scheduled 模式   │  │
│  └──────┬──────┘  └──────┬───────┘  └────────┬─────────┘  │
│         │                │                    │            │
│         └────────────────┴────────────────────┘            │
│                          │                                  │
│                 ┌────────▼────────┐                        │
│                 │  Task Executor  │                        │
│                 │  任务执行引擎   │                        │
│                 └────────┬────────┘                        │
│                          │                                  │
│          ┌───────────────┼───────────────┐                 │
│          │               │               │                 │
│  ┌───────▼──────┐ ┌──────▼──────┐ ┌─────▼──────────┐     │
│  │ Upload Engine│ │ Local Queue  │ │ Credential Mgr │     │
│  │ 分片/断点续传│ │ SQLite 缓冲  │ │ Token/STS 管理 │     │
│  └───────┬──────┘ └─────────────┘ └────────────────┘     │
│          │                                                  │
│  ┌───────▼──────┐                                          │
│  │ MinIO Client │  S3 API / TLS 直传                       │
│  └──────────────┘                                          │
│                                                             │
│  ┌─────────────────────────────────────────────────────┐   │
│  │  Config Manager │ Logger │ Metrics Exporter         │   │
│  └─────────────────────────────────────────────────────┘   │
└─────────────────────────────────────────────────────────────┘
```

| 模块                   | 职责                                                              |
|----------------------|-----------------------------------------------------------------|
| **gRPC Client**      | 维护与 Control Plane 的长连接，处理心跳、断线重连、指令接收、状态上报                      |
| **File Watcher**     | 基于 inotify(Linux) / ReadDirectoryChangesW(Windows) 监控目录变更，降级为轮询 |
| **Scheduler**        | 管理 Scheduled 模式的 cron 任务，到期触发目录扫描                               |
| **Task Executor**    | 接收来自 Watcher/Scheduler/gRPC 的任务，路由到 Upload Engine，写入本地队列        |
| **Upload Engine**    | 文件分片、计算 hash、调用 MinIO S3 API 上传，支持断点续传，完成后上报 Control Plane      |
| **Local Queue**      | SQLite 本地任务持久化队列，断网时缓冲待上传任务，恢复后自动补传                             |
| **Credential Mgr**   | 管理 Auth Token 和 MinIO STS 凭据的本地存储、有效期检测与主动续期                    |
| **Config Manager**   | 管理本地配置文件（TOML），以及从 Control Plane 下发的运行时规则配置                     |
| **Metrics Exporter** | 暴露 Prometheus /metrics 端点，上报本地队列深度、上传速率、连接状态等指标                 |

## 4.2 生命周期状态机

```
              ┌─────────────┐
              │   INIT      │  首次启动，无 token
              └──────┬──────┘
                     │ 发送注册请求（fingerprint + 设备信息）
                     ▼
              ┌─────────────┐
              │   PENDING   │  等待管理员审批
              └──────┬──────┘
      管理员拒绝 ◄────┤────► 管理员审批
                     │
                     ▼
              ┌─────────────┐
token 过期/吊销│   APPROVED  │  收到 Auth Token，持久化
─────────────►└──────┬──────┘
                     │ 建立 gRPC 长连接
                     ▼
     ┌──────── ┌─────────────┐ ────────────┐
     │         │   RUNNING   │             │
网络断开│         └──────┬──────┘     收到吊销指令│
     ▼                │                    ▼
┌─────────────┐       │            ┌─────────────┐
│  OFFLINE    │       │            │   REVOKED   │
│  本地队列   │       │            │  清除 token │
│  继续工作   │       │            └─────────────┘
└──────┬──────┘       │
       │重连成功       │ 接收指令：
       └────────►     │  - list_directory
                     │  - push_rule（采集规则）
                     │  - cancel_rule
                     │  - rotate_credentials
                     │  - ping
                     ▼
              上报事件：
              - upload_result
              - heartbeat
              - rule_status
              - directory_listing
```

**状态说明：**

- INIT → PENDING：Agent 每 30 秒轮询一次审批状态，直到收到 token 或被拒绝；
- RUNNING → OFFLINE：gRPC 连接断开后立即切换，本地队列继续接收 Watcher/Scheduler 产生的任务；
- OFFLINE → RUNNING：重连成功后，先同步规则配置，再开始消费本地队列中的积压任务；
- 任何状态 → REVOKED：清除本地 token 和 STS 凭据，停止所有上传，等待重新注册。

## 4.3 gRPC 协议定义（完整 .proto）

```protobuf
syntax = "proto3";
package fileagent.v1;
option go_package = "github.com/yourorg/fileagent/api/v1;agentv1";

import "google/protobuf/timestamp.proto";
import "google/protobuf/empty.proto";

// ─────────────────────────────────────────────
// 服务定义
// ─────────────────────────────────────────────

service AgentService {
  // Agent 主连接：建立后服务端持续推送指令，Agent 持续上报事件
  rpc Connect(stream AgentMessage) returns (stream ServerMessage);
  // 注册申请（无需 token，TLS 即可）
  rpc Register(RegisterRequest) returns (RegisterResponse);
  // 轮询审批结果（PENDING 阶段使用）
  rpc PollApproval(PollApprovalRequest) returns (PollApprovalResponse);
  // 主动续期 STS 凭据
  rpc RefreshCredentials(RefreshCredentialsRequest) returns (RefreshCredentialsResponse);
}

// ─────────────────────────────────────────────
// Agent → Server 消息（Connect 流上行）
// ─────────────────────────────────────────────

message AgentMessage {
  string message_id = 1;
  oneof payload {
    Heartbeat         heartbeat          = 10;
    UploadResult      upload_result      = 11;
    DirectoryListing  directory_listing  = 12;
    RuleStatusReport  rule_status        = 13;
    ErrorReport       error_report       = 14;
  }
}

message Heartbeat {
  string  agent_id        = 1;
  int64   uptime_seconds  = 2;
  int32   queue_depth     = 3;
  float   upload_bps      = 4;
  string  version         = 5;
  repeated DiskInfo disks = 6;
}

message DiskInfo {
  string path        = 1;
  uint64 total_bytes = 2;
  uint64 free_bytes  = 3;
  string fs_type     = 4;
}

message UploadResult {
  string  rule_id       = 1;
  string  local_path    = 2;
  string  storage_path  = 3;
  string  bucket        = 4;
  int64   size_bytes    = 5;
  string  sha256        = 6;
  string  etag          = 7;
  bool    success       = 8;
  string  error_message = 9;
  google.protobuf.Timestamp file_mtime   = 10;
  google.protobuf.Timestamp uploaded_at  = 11;
  int32   retry_count   = 12;
}

message DirectoryListing {
  string          request_id = 1;
  string          path       = 2;
  repeated FsEntry entries   = 3;
  string          error      = 4;
}

message FsEntry {
  string name       = 1;
  bool   is_dir     = 2;
  int64  size_bytes = 3;
  google.protobuf.Timestamp modified_at = 4;
  string permissions = 5;
}

message RuleStatusReport {
  string rule_id    = 1;
  string status     = 2;
  string error_msg  = 3;
  int64  files_processed = 4;
  int64  bytes_uploaded  = 5;
}

message ErrorReport {
  string code     = 1;
  string message  = 2;
  string detail   = 3;
}

// ─────────────────────────────────────────────
// Server → Agent 消息（Connect 流下行）
// ─────────────────────────────────────────────

message ServerMessage {
  string message_id = 1;
  oneof payload {
    PingCommand          ping               = 10;
    ListDirectoryCommand list_directory     = 11;
    PushRuleCommand      push_rule          = 12;
    CancelRuleCommand    cancel_rule        = 13;
    CredentialsPayload   credentials        = 14;
    RevokeCommand        revoke             = 15;
    Acknowledgement      ack                = 16;
  }
}

message PingCommand {}

message ListDirectoryCommand {
  string request_id = 1;
  string path       = 2;
  bool   recursive  = 3;
  int32  max_depth  = 4;
}

message PushRuleCommand {
  CollectionRule rule = 1;
}

message CollectionRule {
  string rule_id              = 1;
  string name                 = 2;
  string mode                 = 3;
  string source_path_template = 4;
  string file_glob            = 5;
  string upload_bucket        = 6;
  string upload_path_template = 7;
  bool   watch_recursive      = 8;
  string watch_subdir_pattern = 9;
  string cron_expr            = 10;
  bool   run_once_on_start    = 11;
  string append_mode          = 12;
  bool   enabled              = 13;
}

message CancelRuleCommand {
  string rule_id = 1;
}

message CredentialsPayload {
  string access_key    = 1;
  string secret_key    = 2;
  string session_token = 3;
  string endpoint      = 4;
  bool   use_ssl       = 5;
  google.protobuf.Timestamp expires_at = 6;
}

message RevokeCommand {
  string reason = 1;
}

message Acknowledgement {
  string ref_message_id = 1;
  bool   success        = 2;
  string error          = 3;
}

// ─────────────────────────────────────────────
// 注册 / 审批轮询
// ─────────────────────────────────────────────

message RegisterRequest {
  string fingerprint   = 1;
  string hostname      = 2;
  string os_type       = 3;
  string os_version    = 4;
  string arch          = 5;
  string agent_version = 6;
  string ip_address    = 7;
}

message RegisterResponse {
  string agent_id   = 1;
  string status     = 2;
  string auth_token = 3;
  string message    = 4;
}

message PollApprovalRequest {
  string agent_id    = 1;
  string fingerprint = 2;
}

message PollApprovalResponse {
  string status     = 1;
  string auth_token = 2;
  string message    = 3;
}

message RefreshCredentialsRequest {
  string agent_id = 1;
  string rule_id  = 2;
}

message RefreshCredentialsResponse {
  CredentialsPayload credentials = 1;
}
```

## 4.4 采集模式实现

### 4.4.1 Watch 模式

Watch 模式持续监控指定目录，实时响应文件变更事件。由于边缘设备的目标目录可能是挂载盘（无法使用 inotify），系统自动降级为轮询：

```
Watch 模式执行流程：
  1. 启动时：解析 source_path_template（替换时间变量），确定监控根路径
  2. 尝试注册 inotify/FSEvents/ReadDirectoryChangesW 监听器
     ├── 成功：事件驱动，文件关闭(CLOSE_WRITE)或创建时触发
     └── 失败（挂载盘/网络盘等）：降级为轮询，默认间隔 30s
  3. 若 watch_recursive=true，递归监控所有子目录
     └── 若配置了 watch_subdir_pattern，仅监控匹配的新增子目录
  4. 文件事件到达后，用 file_glob 过滤文件名
  5. 对通过过滤的文件，提交到 Task Executor
  6. Task Executor 去重（对比本地 SQLite 中的已处理记录）后入队
```

**跨平台文件系统事件适配：**

| 平台      | 机制                    | Go 库              | 挂载盘支持       |
|---------|-----------------------|-------------------|-------------|
| Linux   | inotify               | fsnotify/fsnotify | 不支持（自动降级轮询） |
| Windows | ReadDirectoryChangesW | fsnotify/fsnotify | 本地盘支持，网络盘降级 |
| 降级轮询    | 定时 stat() 扫描          | 内部实现              | 全平台，所有挂载类型  |

### 4.4.2 Scheduled 模式

```
Scheduled 模式执行流程：
  1. 按 cron_expr 注册定时任务（支持标准5段 cron）
  2. 触发时：解析 source_path_template 中的时间变量
     变量列表：{yyyy} {yy} {mm} {dd} {HH} {MM}
     示例：/data/{yyyy}/{mm}/{dd} → /data/2025/04/24
  3. 检查路径是否存在，不存在则记录日志，跳过本次执行
  4. 递归扫描目录，用 file_glob 过滤文件
  5. 对每个匹配文件：
     a. 查询本地 SQLite：是否已上传（通过路径+mtime+size 三元组判断）
     b. 未上传或有变更 → 提交到 Upload Engine
     c. 已上传且无变更 → 跳过（幂等）
  6. 若 run_once_on_start=true，Agent 启动时立即执行一次，不等 cron
```

### 4.4.3 追加写入文件处理（append_mode）

| 模式             | 行为                               | 适用场景                    |
|----------------|----------------------------------|-------------------------|
| **overwrite**  | 每次触发完整上传，覆盖存储端对象                 | 文件较小（< 200MB），追加不频繁     |
| **close_wait** | 等待文件句柄关闭（CLOSE_WRITE 事件）后上传      | 日志轮转类文件，写完即关闭           |
| **tail**       | 记录上次上传的文件 offset，仅上传新增字节，以追加分片存储 | 大文件持续追加（> 200MB），带宽敏感场景 |

## 4.5 文件上传流程

> ⚠️ **实现状态（2026-09-08 审计，D-030）**：本节描述的**主路径尚未实现**。Agent 从不上报 `UploadResult`
> （`agent/cmd/agent/main.go:142` 丢弃结果，IC-BUG-2），断点续传状态也从未落盘（`upload_tasks` 的三条 UPDATE
> 均不含 `upload_id`/`completed_parts`，IC-BUG-5）。当前 `file_entries` 的唯一写入者是 §6.5 的 MinIO webhook。
> 目标写入协议见 [`consistency-and-ingest.md`](./consistency-and-ingest.md) §3.3；缺陷见
> [`bugs/open.md`](../tasks/bugs/open.md) IC-BUG-2 / IC-BUG-5。

```
上传决策流程：
  file_size ≤ 64MB ?
    ├── YES → PutObject（单次上传）
    └── NO  → Multipart Upload（分片上传）
                ├── 分片大小：64MB（最后一片可 < 64MB）
                ├── 并发分片数：3（可配置）
                └── Complete Multipart → 对象原子可见

断点续传实现：
  1. 上传开始前，在本地 SQLite 中创建 upload_tasks 记录
     字段：file_path, storage_path, upload_id（Multipart ID）,
           total_parts, completed_parts（JSON数组）, status
  2. 每个分片上传成功后，更新 completed_parts
  3. 若上传中断（网络断开/进程重启）：
     a. 重启后扫描 status=in_progress 的记录
     b. 用 ListParts API 查询 MinIO 已完成的分片
     c. 跳过已完成分片，从断点继续
  4. 完成后调用 CompleteMultipartUpload
     └── 更新本地记录 status=completed，上报 UploadResult 给 Control Plane

幂等性保证：
  - 上传前计算文件 SHA-256
  - Control Plane 收到 UploadResult 后检查 (bucket, storage_path, sha256)
  - 重复上报同一文件时，幂等更新 file_entries，不创建重复记录
```

## 4.6 本地任务队列（SQLite）

```sql
CREATE TABLE upload_tasks (
    id              TEXT PRIMARY KEY,
    rule_id         TEXT NOT NULL,
    local_path      TEXT NOT NULL,
    storage_path    TEXT NOT NULL,
    bucket          TEXT NOT NULL,
    upload_id       TEXT,
    completed_parts TEXT,
    file_size       INTEGER NOT NULL DEFAULT 0,
    file_mtime      INTEGER NOT NULL DEFAULT 0,
    sha256          TEXT,
    status          TEXT NOT NULL DEFAULT "pending",
    retry_count     INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    created_at      INTEGER NOT NULL,
    updated_at      INTEGER NOT NULL
);

CREATE TABLE processed_files (
    id          TEXT PRIMARY KEY,
    rule_id     TEXT NOT NULL,
    local_path  TEXT NOT NULL,
    file_size   INTEGER NOT NULL,
    file_mtime  INTEGER NOT NULL,
    sha256      TEXT,
    uploaded_at INTEGER NOT NULL,
    UNIQUE (rule_id, local_path)
);

CREATE TABLE rules (
    id         TEXT PRIMARY KEY,
    payload    TEXT NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX idx_upload_tasks_status ON upload_tasks (status, created_at);
CREATE INDEX idx_processed_files_rule ON processed_files (rule_id, local_path);
```

**队列消费策略：**

- Worker goroutine 数量默认为 3，可由 Control Plane 下发配置动态调整；
- 失败任务按指数退避重试：1min → 5min → 15min → 60min，最多重试 10 次；
- 超过重试上限的任务标记为 failed，上报 Control Plane，正常重试流程中**不主动删除**
  （唯一例外是下方 `queue_max_size` 容量驱逐：容量吃紧时最旧的 failed 行也可能被驱逐）；
- 本地队列总大小上限可配置（`queue_max_size`，默认 10000 条；config 校验要求 **> 0**，即始终有界）。
  **新任务先入队、再回收**：入队成功后，当活跃任务数（pending + running + failed，不含 completed）
  超过上限时，丢弃**最旧的可驱逐任务**（状态为 pending 或 failed；running 为在途上传不驱逐，
  其数量受 worker 并发数约束）并告警。先入队后回收可避免"已驱逐旧任务却因入队失败而白白丢数据"，
  且刚入队的新任务被排除在驱逐之外，保证新采集的文件不会被自己的 submit 挤掉。
  失败任务必须可驱逐——上传持续中断时任务会在 pending→running→failed 间循环，待清理的积压主要处于 failed 态，
  若只驱逐 pending 则断网久了队列仍会无限增长。

## 4.7 凭据管理与轮转

> ⚠️ **实现状态（D-030）**：STS 链路当前是断的（IC-BUG-1）——Control Plane 从不推送
> `ServerMessage_Credentials`，而 Agent 的刷新 goroutine 因 `sts == nil` 短路从不发起 RPC，
> 发起也会因缺 `rule_id` 被拒。后果是 Agent 永远拿不到上传凭据。见
> [`bugs/open.md`](../tasks/bugs/open.md) IC-BUG-1。

```
Auth Token（JWT）管理：
  - 存储：加密后写入本地文件（AES-256-GCM，密钥派生自 fingerprint）
  - 有效期：由 Control Plane 配置项 AGENT_TOKEN_TTL 控制，默认 30 天（720h）；
    刻意长效——Agent 复用同一 token 跨重连，短 TTL 会在过期后令重连被拒（见 D-013）
  - 重连自愈：无专门的续期 RPC。当 Control Plane 以 gRPC Unauthenticated 拒绝
    token（过期/被吊销后重新审批）时，Agent 通过免鉴权的 PollApproval 重新领取 token
    再重连；非审批态不发新 token，已吊销 Agent 不会自愈（见 D-013）
  - 吊销感知：服务端推送 RevokeCommand，立即清除本地 token

STS 凭据（MinIO 临时访问密钥）管理：
  - 存储：内存中，不落磁盘（进程重启后重新请求）
  - 有效期：默认 1 小时（由 Control Plane 生成时设置）
  - 续期触发：剩余有效期 < 10 分钟时，发送 RefreshCredentials RPC
  - 续期失败处理：
      └── 重试 3 次（间隔 30s），仍失败则暂停上传，等待重连
```

## 4.8 跨平台适配要点

| 差异点           | Linux                           | Windows                                       |
|---------------|---------------------------------|-----------------------------------------------|
| **路径分隔符**     | /                               | \，代码统一用 `filepath.ToSlash()` 处理               |
| **文件系统事件**    | inotify                         | ReadDirectoryChangesW，通过 fsnotify 统一封装        |
| **磁盘挂载枚举**    | /proc/mounts 解析                 | WMI Win32_LogicalDisk 查询                      |
| **服务安装**      | systemd .service 文件             | Windows Service（golang.org/x/sys/windows/svc） |
| **配置文件路径**    | /etc/fileagent/ 或 ~/.fileagent/ | %PROGRAMDATA%\FileAgent\                      |
| **日志路径**      | /var/log/fileagent/             | %PROGRAMDATA%\FileAgent\logs\                 |
| **SQLite 路径** | /var/lib/fileagent/queue.db     | %PROGRAMDATA%\FileAgent\queue.db              |

**编译方式：**

```bash
# Linux amd64
GOOS=linux  GOARCH=amd64 go build -o dist/fileagent-linux-amd64  ./cmd/agent
# Linux arm64
GOOS=linux  GOARCH=arm64 go build -o dist/fileagent-linux-arm64  ./cmd/agent
# Windows amd64
GOOS=windows GOARCH=amd64 go build -o dist/fileagent-windows-amd64.exe ./cmd/agent
```

## 4.9 Go 项目结构

```
fileagent/
├── cmd/
│   └── agent/
│       └── main.go
├── internal/
│   ├── config/
│   │   ├── config.go
│   │   └── defaults.go
│   ├── grpcclient/
│   │   ├── client.go
│   │   ├── reconnect.go
│   │   └── handler.go
│   ├── watcher/
│   │   ├── watcher.go
│   │   ├── inotify.go
│   │   ├── windows.go
│   │   └── poller.go
│   ├── scheduler/
│   │   └── scheduler.go
│   ├── executor/
│   │   ├── executor.go
│   │   └── dedup.go
│   ├── uploader/
│   │   ├── uploader.go
│   │   ├── multipart.go
│   │   └── resume.go
│   ├── queue/
│   │   ├── queue.go
│   │   └── schema.go
│   ├── credential/
│   │   ├── manager.go
│   │   └── storage.go
│   ├── sysinfo/
│   │   ├── disk_linux.go
│   │   └── disk_windows.go
│   └── metrics/
│       └── metrics.go
├── api/
│   └── v1/
│       ├── agent.pb.go
│       └── agent_grpc.pb.go
├── proto/
│   └── v1/
│       └── agent.proto
├── build/
│   ├── Makefile
│   └── package.sh
├── deploy/
│   ├── fileagent.service
│   ├── fileagent-windows.ps1
│   └── config.toml.example
├── go.mod
└── go.sum
```

**关键配置项（config.toml）：**

```toml
[server]
endpoint    = "control.example.com:443"
tls_ca_cert = "/etc/fileagent/ca.crt"

[agent]
fingerprint_file = "/var/lib/fileagent/fingerprint"
token_file       = "/var/lib/fileagent/token.enc"
data_dir         = "/var/lib/fileagent"

[upload]
concurrency       = 3
part_size_mb      = 64
queue_max_size    = 10000
retry_max         = 10

[metrics]
enabled = true
port    = 9100

[log]
level  = "info"
output = "/var/log/fileagent/agent.log"
max_size_mb  = 100
max_backups  = 7
```

---

# 第五章 Control Plane 设计

## 5.1 模块划分

```
┌──────────────────────────────────────────────────────────────────┐
│                    Control Plane (Go / Gin)                      │
│                                                                  │
│  ┌──────────────┐  ┌──────────────┐  ┌────────────────────────┐ │
│  │  gRPC Server │  │  REST API    │  │  Background Workers    │ │
│  │  Agent连接管  │  │  Gin Router  │  │  凭据轮转监控          │ │
│  │  指令下发    │  │  JWT中间件   │  │  事件规则处理          │ │
│  │  状态维护    │  │  限流/鉴权   │  │  Webhook重试           │ │
│  └──────┬───────┘  └──────┬───────┘  └──────────┬─────────────┘ │
│         │                 │                      │              │
│         └─────────────────┴──────────────────────┘              │
│                           │                                      │
│          ┌────────────────┼────────────────────┐                │
│          │                │                    │                │
│  ┌───────▼──────┐ ┌───────▼──────┐ ┌──────────▼──────────┐    │
│  │ Agent Manager│ │ File Indexer │ │  Event Rule Engine  │    │
│  │ 注册/审批    │ │ 路径规则归类  │ │  NATS 订阅/分发     │    │
│  │ 连接注册表   │ │ 文件条目写入  │ │  Webhook/MQ 投递    │    │
│  └──────┬───────┘ └──────┬───────┘ └──────────┬──────────┘    │
│         │                │                      │              │
│  ┌──────▼───────┐ ┌──────▼────────┐  ┌─────────▼──────────┐   │
│  │ Task Dispatch│ │ MinIO Manager │  │  Credential Manager│   │
│  │ 规则下发/取消│ │ Bucket/Policy │  │  STS 生成/轮转      │   │
│  └──────────────┘ └───────────────┘  └────────────────────┘   │
│                                                                  │
│  ┌───────────────────────────────────────────────────────────┐  │
│  │       PostgreSQL    │    Redis    │    NATS JetStream      │  │
│  └───────────────────────────────────────────────────────────┘  │
└──────────────────────────────────────────────────────────────────┘
```

## 5.2 Agent 连接管理

```go
type AgentRegistry struct {
    mu    sync.RWMutex
    conns map[string]*AgentConn
}

type AgentConn struct {
    AgentID     string
    Stream      agentv1.AgentService_ConnectServer
    SendCh      chan *agentv1.ServerMessage
    ConnectedAt time.Time
    CancelFunc  context.CancelFunc
}

func (s *AgentServer) Connect(stream AgentService_ConnectServer) error {
    agentID := extractAgentID(stream.Context())
    conn := s.registry.Register(agentID, stream)
    defer s.registry.Unregister(agentID)
    s.redis.Set("agent:"+agentID+":online", "1", heartbeatTTL*3)
    go s.recvLoop(conn)
    return s.sendLoop(conn)
}
```

**心跳机制：**

- Agent 每 30 秒发送一次 Heartbeat 消息；
- Control Plane 收到 Heartbeat 后刷新 Redis Key TTL（90 秒）；
- **离线判定与自愈（三条路径）**：
  1. gRPC 流断开时立即置离线（`agents.status=offline` + Del Redis Key + 发 `events.agent.offline`）；
  2. **TTL 兜底扫描**（CC-6，`internal/worker` OfflineSweeper）：CP 崩溃/重启、TCP 半开时上面的 defer 不执行，
     Redis Key 仍会过期但 DB 状态与离线事件会残留 online——后台每 30s 扫描"DB=online 但 Redis Key 已过期"的
     Agent，兜底置 `offline` 并补发 `events.agent.offline`。属最终一致的兜底，非精确即时判定。
  3. **心跳自愈**：兜底扫描在"EXISTS→UPDATE"极窄窗口内可能误判一个刚重连的 Agent 为离线；心跳处理据此
     自愈——收到心跳时若 DB 状态非 online 则条件式恢复为 online 并补发 `events.agent.online`（稳态下 0 行、不刷事件），
     使误判不可持久。
- Control Plane 实例重启后，从 Redis 恢复在线状态，等待 Agent 重连；真正已离线的 Agent 由上面的 TTL 兜底扫描收敛。

## 5.3 用户认证模块

### 5.3.1 JWT 设计

```go
type Claims struct {
    jwt.RegisteredClaims
    OrgID    string `json:"org_id"`
    Role     string `json:"role"`
    Username string `json:"username"`
}

AccessTokenTTL  = 2 * time.Hour
RefreshTokenTTL = 30 * 24 * time.Hour

func RevokeToken(jti string, ttl time.Duration) {
    redis.Set("jwt:jti:"+jti, "revoked", ttl)
}

func IsRevoked(jti string) bool {
    return redis.Exists("jwt:jti:"+jti)
}
```

### 5.3.2 认证接口

| Method | Path              | 说明                              | 认证要求          |
|--------|-------------------|---------------------------------|---------------|
| POST   | /api/auth/login   | 用户名密码登录，返回双 Token               | 无             |
| POST   | /api/auth/refresh | 用 Refresh Token 换新令牌对（见下方契约） | Refresh Token（请求体，或 Bearer 头回退） |
| POST   | /api/auth/logout  | 吊销当前 Token                      | Access Token  |
| GET    | /api/auth/me      | 返回当前用户信息                        | Access Token  |

**`/api/auth/refresh` 契约（D-012）**：refresh token 通过 **JSON 请求体**
`{"refresh_token": "..."}` 传递（OAuth2 refresh-grant 惯例；Bearer 头作为向后兼容回退），
响应执行**令牌轮转**——返回新的 `{access_token, refresh_token, expires_in, token_type}`
并吊销旧 refresh token。Web UI 与 SDK 均依赖此契约（历史上因 CP 只读 header、不轮转而断裂）。
旧 token 吊销是**尽力而为**：依赖 Redis 黑名单，黑名单不可用时吊销及其校验会降级
（放行并记 Warn 日志，非硬失败），此时旧 refresh token 可能仍短暂可用——与整个 auth 层
"Redis 不可用时优雅降级"一致（见 D-012 及 auth.go 的可观测降级逻辑）。

### 5.3.3 OIDC 扩展预留

- 认证中间件设计为接口（Authenticator interface），内置账号和 OIDC 实现可互换；
- users 表预留 oidc_sub、oidc_provider 字段（注释状态）；
- 登录接口预留 /api/auth/oidc/callback 路由占位，返回 501 Not Implemented。

## 5.4 用户与权限模型

| 角色              | 第一版状态    | 多租户启用后 | 权限范围                       |
|-----------------|----------|--------|----------------------------|
| **super_admin** | 启用       | 不变     | 全系统所有资源                    |
| **org_admin**   | 预留（不暴露）  | 启用     | 本 org 内的采集器、bucket、文件、事件规则 |
| **org_viewer**  | 作为普通用户启用 | 不变     | 只读：查询文件、下载、查看采集器状态         |

```go
func RequireRole(roles ...string) gin.HandlerFunc {
    return func(c *gin.Context) {
        claims := GetClaims(c)
        for _, r := range roles {
            if claims.Role == r { c.Next(); return }
        }
        c.AbortWithStatusJSON(403, gin.H{"error": "forbidden"})
    }
}
```

## 5.5 Agent 注册审批流程

```
Agent 首次启动                    Control Plane                  管理员
     │                                  │                           │
     │── Register RPC ────────────────► │                           │
     │   (fingerprint, hostname, os)    │                           │
     │                                  │ 写入 agents(status=pending)│
     │◄── RegisterResponse(pending) ─── │──── 通知（Web UI 标记）──► │
     │                                  │                           │
     │ 每30s: PollApproval RPC ────────► │    POST /api/agents/{id}/approve
     │◄── status=pending ─────────────── │ ◄───────────────────────│
     │                                  │ 生成 JWT                  │
     │◄── status=approved + token ────── │                           │
     │                                  │                           │
     │ 持久化 token                      │                           │
     │── Connect RPC (Bearer token) ───► │                           │
```

## 5.6 任务调度与指令下发

```
POST /api/agents/{agent_id}/rules    → 创建规则并立即下发
PUT  /api/agents/{agent_id}/rules/{rule_id} → 更新规则并重新下发
DELETE /api/agents/{agent_id}/rules/{rule_id} → 取消规则
```

**分布式锁防止多实例重复下发：**

```go
lockKey := "lock:rule_dispatch:" + ruleID
ok := redis.SetNX(lockKey, instanceID, 10*time.Second)
if !ok { return }
defer redis.Del(lockKey)
```

## 5.7 STS 凭据管理与轮转

> ⚠️ **实现偏差（D-030）**：实现签发的资源前缀是 `agents/{agent_id}/*`，而实际 `storage_path` 完全由规则的
> `dest_path_template` 决定，二者不匹配（IC-BUG-3）；Action 列表缺 §6.3 要求的 `s3:AbortMultipartUpload` /
> `s3:ListMultipartUploadParts`，且多授了 `s3:DeleteObject`（IC-BUG-4）。
> 目标形态（D-030 第八条）：policy 资源改为**整桶** `arn:aws:s3:::{bucket}/*`，`dest_path_template`
> 不受任何约束——授权宽度是管理权限问题，清点成本由分片+封存对账解决，不靠约束用户路径。见
> [`consistency-and-ingest.md`](./consistency-and-ingest.md) §3.2 / §3.5。

```go
func (s *STSManager) IssueCredentials(agentID string,
    buckets []BucketAccess) (*CredentialsPayload, error) {
    policy := buildSessionPolicy(buckets)
    policyJSON, _ := json.Marshal(policy)
    li := credentials.NewSTSAssumeRole(endpoint, credentials.STSAssumeRoleOptions{
        AccessKey:       s.adminAK,
        SecretKey:       s.adminSK,
        RoleARN:         "arn:aws:iam:::role/agent-role",
        RoleSessionName: "agent-" + agentID,
        Policy:          string(policyJSON),
        DurationSeconds: 3600,
    })
    val, err := li.Get()
    if err != nil { return nil, err }
    return &CredentialsPayload{
        AccessKey:    val.AccessKeyID,
        SecretKey:    val.SecretAccessKey,
        SessionToken: val.SessionToken,
        Endpoint:     endpoint,
        UseSSL:       true,
        ExpiresAt:    timestamppb.New(time.Now().Add(3600*time.Second)),
    }, nil
}
```

## 5.8 文件索引与归类引擎

> ✅ **实现状态（D-030；IC-2a 于 2026-09-11 落地，PR #98）**：`HandleUploadResult` **已是索引主路径**，
> 不再是死代码。下面的伪代码是意图，**权威以代码为准**（`controlplane/internal/indexer/indexer.go`
> 与 `controlplane/internal/db/queries/ingest.sql`）。相对本节伪代码，实现多了四件事：
>
> 1. **写入守卫与来源标记**：`file_entries` 增 `observed_at` / `source` / `event_seq` / `meta_incomplete`
>    四列（迁移 `000006_index_observation`）。upsert 的 `DO UPDATE` 带守卫——**较新的观测才准覆盖**
>    （`observed_at` 更大，或相等时由 `event_seq` 决胜；任一侧为 NULL 一律放行），且富字段一律
>    `COALESCE(EXCLUDED.x, file_entries.x)`。**软删除走独立的带守卫 UPDATE 并推进这两列**，
>    否则删除事件重投会把刚重建的活对象再标成 `deleted`。修 IC-BUG-8 / IC-BUG-13 的防清空半边。
> 2. **`observed_at` 按来源取各自最可信、且客户端左右不了的时刻**：`minio_event` ← 载荷的 `eventTime`
>    （MinIO 生成）；`agent` / `api` ← **PostgreSQL 的 `now()`**（不是 CP 进程时钟——dev 实测进程时钟
>    比 PG/MinIO 慢约 16ms，用它会让每一次合法上报都被守卫拦掉）；`audit` ← 列举那一刻。
>    **绝不采信 `UploadResult.uploaded_at`**（由 agent 提供，报 `2099` 即可永久冻结该行）。
> 3. **失败上报不写 `file_entries`，只写 `upload_logs`**（`file_entry_id` 置空）。原实现无论成败都 upsert，
>    而 `DO UPDATE` 会无条件覆盖 `status`，能把「已成功上传、后来重传失败」的**活对象标成 `failed`**（IC-BUG-33）。
> 4. **`UploadResult.rule_id` 做归属校验，三分支**：规则不存在（**稳态正常情形**，队列与规则生命周期解耦）
>    → 清空 `rule_id`、文件照常入索引、置 `meta_incomplete=true`；规则属于别的 agent → 拒绝该 `rule_id`
>    并告警；合法 → 正常打标（IC-BUG-29）。`meta_incomplete` 的语义是「索引该行时拿不到规则声明的元数据」，
>    **从不清位**，由 `GET /api/v1/files` 与文件详情向 UI 暴露。
>
> 目标模型的其余部分（宽表/窄表分家 + 三级对账）尚未落地，见
> [`consistency-and-ingest.md`](./consistency-and-ingest.md) §3.4–§3.5。

```go
func (e *FileIndexer) HandleUploadResult(result *UploadResult) error {
    entry := &FileEntry{
        OrgID:       agentOrgID,
        AgentID:     result.AgentID,
        BucketID:    bucketID,
        StoragePath: result.StoragePath,
        FileName:    filepath.Base(result.StoragePath),
        SizeBytes:   result.SizeBytes,
        SHA256:      result.SHA256,
        ETag:        result.ETag,
        FileMtime:   result.FileMtime,
        Status:      "completed",
        UploadedAt:  result.UploadedAt,
    }
    entry.FileTypeID = e.matchFileType(result.StoragePath)
    err := e.db.UpsertFileEntry(entry)
    e.db.CreateUploadLog(result)
    e.nats.Publish("events.file.uploaded", eventPayload)
    return err
}
```

> **打标引擎（6c，Phase 1 · 已实现，D-025，PR #69–#75）**：`internal/indexer` 在 `UpsertFileEntry` 后打标——从规则
> `metadata` 写静态标签（source=`rule_static`）、按 `path_tag_map` 从 storage path 用 trollsift 反解抽取路径标签
> （source=`path_var`，未登记值进 `pending_tag_values` 待确认队列），幂等写入 `file_tags`；classifier 改为**规则声明
> 类型优先、glob 兜底**。REST 侧：文件查询增可重复 `tag` 谓词筛选（保持 cursor 分页）；词表 CRUD、待确认队列
> approve/merge/reject、单文件 `PUT /files/{id}/tags` 与 `POST /files/batch-tag`（super_admin）；merge / batch-tag 走
> 回溯 worker（`worker.RetagWorker` 消费 `retag_jobs`）异步改写 + 审计。设计见 [`metadata-model.md`](./metadata-model.md)。

## 5.9 事件规则引擎

```
NATS 主题规划：
  events.file.uploaded
  events.file.deleted
  events.agent.online
  events.agent.offline
  events.agent.approved
  events.agent.revoked
```

**动作类型（`action_type`，CC-7）：**

- `webhook`：HTTP POST 事件 payload 到 `action_config.url`。
- `nats_publish`：把事件 payload 原样重新发布到 `action_config.subject` 指定的 NATS 主题，
  供内部下游消费者订阅。引擎持有一个 NATS publisher（`Engine.WithPublisher`）；未配置 publisher
  时该动作记为 `failed` 并进入重试，而非静默"成功"。
- `kafka_publish`：DB enum 中保留但**无实现**（部署固定基础设施不含 Kafka）。API 创建/更新事件规则时
  对非 `webhook`/`nats_publish` 的 `action_type` 返回 `400 INVALID_ACTION_TYPE`；`action_config`
  缺少必填字段（webhook 的 `url` / nats_publish 的 `subject`）返回 `400 INVALID_ACTION_CONFIG`。

**投递与重试：**

- webhook：HTTP POST，超时 10 秒，2xx 视为成功；nats_publish：publisher 返回 nil 视为成功（`delivered`）。
- 两种动作失败后共用指数退避重试：30s → 2min → 10min → 30min → 2h，最多 5 次；
- Background Worker 每 30s 扫描 `next_retry_at <= now()` 的 `pending`/`failed` 记录执行重试；
- 重试耗尽或动作类型不可投递（如历史遗留的 `kafka_publish` 投递）时置**终态 `dead`**，
  从重试扫描中剔除——避免终态记录（`next_retry_at` 为空被视为"立即到期"）被每 30s 反复重投。

## 5.10 上传日志记录

| Method | Path                         | 说明           | 过滤参数                                                |
|--------|------------------------------|--------------|-----------------------------------------------------|
| GET    | /api/upload-logs             | 查询上传日志列表     | agent_id, status, start_time, end_time, page, limit |
| GET    | /api/upload-logs/{id}        | 查询单条日志详情     | -                                                   |
| GET    | /api/agents/{id}/upload-logs | 查询指定采集器的上传日志 | status, start_time, end_time                        |

## 5.11 对外 REST API 设计

### 5.11.1 用户管理

| Method | Path                        | 说明                  |
|--------|-----------------------------|---------------------|
| GET    | /api/v1/users               | 列出用户（super_admin）   |
| POST   | /api/v1/users               | 创建用户（super_admin）   |
| PUT    | /api/v1/users/{id}          | 更新用户信息（super_admin） |
| DELETE | /api/v1/users/{id}          | 禁用用户（super_admin）   |
| PUT    | /api/v1/users/{id}/password | 修改密码                |

### 5.11.2 采集器管理

| Method | Path                            | 说明                 |
|--------|---------------------------------|--------------------|
| GET    | /api/v1/agents                  | 列出所有采集器            |
| GET    | /api/v1/agents/{id}             | 获取采集器详情            |
| PATCH  | /api/v1/agents/{id}             | 重命名采集器（super_admin，见下） |
| POST   | /api/v1/agents/{id}/approve     | 审批通过（super_admin）  |
| POST   | /api/v1/agents/{id}/revoke      | 吊销采集器（super_admin） |
| POST   | /api/v1/agents/{id}/list-dir    | 下发列目录指令            |
| GET    | /api/v1/agents/{id}/rules       | 列出采集规则             |
| POST   | /api/v1/agents/{id}/rules       | 创建并下发采集规则          |
| PUT    | /api/v1/agents/{id}/rules/{rid} | 更新采集规则（见下）        |
| DELETE | /api/v1/agents/{id}/rules/{rid} | 取消采集规则             |

**`PUT .../rules/{rid}` 双形态（CC-9）**：

- **仅状态切换**：`{"status":"active"|"inactive"}` —— 启用/停用。
- **全字段编辑**：请求体提供 **`name` 字符串**时视为全量更新（`name` 为 `*string`，空串 `""` 仍走全量路径
  并按缺字段报错，不会静默回退到状态切换；**`name` 缺省或显式 `null`**——`null` 经 JSON 反序列化为 `nil`，
  与缺省等价——走 status-only 路径）。全量更新应用 `name` / `bucket_id` / `mode` / `base_path` /
  `path_pattern` / `dest_path_template` / `recursive` / `cron_expr` / `run_once_on_start` /
  `append_mode` / `enabled`（→ status）。缺任一必填字段返回 `422 VALIDATION_ERROR`，
  `mode` 非 `watch`/`scheduled` 返回 `422`，`bucket_id` 非法返回 `400 INVALID_BUCKET_ID`。
  全量更新的 SQL 以 `id + agent_id + org_id` 三键定位（防越权改他人/他组织规则；不匹配返回 `404`）。
- 两种形态在结果为 active 时都会重新 `DispatchRule`（Agent 收到 `PushRuleCommand` 内部 `stopRule`+重启
  watcher，热重载，无需先 disable）；结果为 inactive 时 `DispatchRuleCancel`。Agent 离线时更新照常写库，
  重连时经 `SyncRulesOnConnect` 自动同步。规则 ID 不变，历史上传日志保持关联。

**`PATCH .../agents/{id}` 重命名（CC-8，super_admin）**：

- body `{"name": "自定义显示名"}`；校验：`TrimSpace` 后非空、长度 ≤ 64 rune、只允许字母（任意语种含中文）/
  数字/空格/`. _ -`（正则 `^[\p{L}\p{N} ._-]+$`）——排除 `/` 等路径分隔与控制字符，因为 `name` 会注入
  上传路径模板 `{agent_name}`。违规返回 `422 VALIDATION_ERROR`，采集器不存在返回 `404`，成功返回更新后的 agent。
- 新名在 Agent **重连**时才反映到路径模板（方案 B 已知约束）；旧名期间已上传对象的路径是写入快照，不追溯修改。

### 5.11.3 文件管理

| Method | Path                              | 说明                    |
|--------|-----------------------------------|-----------------------|
| GET    | /api/v1/files                     | 查询文件条目                |
| GET    | /api/v1/files/{id}                | 获取文件详情                |
| GET    | /api/v1/files/{id}/download-url   | 生成预签名下载 URL（TTL 15分钟） |
| POST   | /api/v1/files/batch-download-urls | 批量生成预签名 URL           |
| GET    | /api/v1/file-types                | 列出文件类型                |
| POST   | /api/v1/file-types                | 创建文件类型                |
| PUT    | /api/v1/file-types/{id}           | 更新文件类型                |
| DELETE | /api/v1/file-types/{id}           | 删除文件类型                |

### 5.11.4 Bucket 与事件规则

| Method | Path                                | 说明                     |
|--------|-------------------------------------|------------------------|
| GET    | /api/v1/buckets                     | 列出所有 Bucket            |
| POST   | /api/v1/buckets                     | 创建 Bucket（super_admin） |
| GET    | /api/v1/event-rules                 | 列出事件规则                 |
| POST   | /api/v1/event-rules                 | 创建事件规则                 |
| PUT    | /api/v1/event-rules/{id}            | 更新事件规则                 |
| DELETE | /api/v1/event-rules/{id}            | 删除事件规则                 |
| GET    | /api/v1/event-rules/{id}/deliveries | 查看事件投递历史               |

### 5.11.5 仪表盘统计

| Method | Path                     | 说明                       |
|--------|--------------------------|--------------------------|
| GET    | /api/v1/stats/dashboard  | 仪表盘聚合数据（见下方响应） |

仪表盘所需的聚合**由服务端一次算好**返回，前端不再从"最近若干条日志"抽样估算
（避免 §7.3.1 出现的失真，见 D-016）。响应：

```json
{
  "total_agents": 15,
  "online_agents": 12,
  "total_files": 89234,
  "storage_bytes": 2528876743884,
  "today_uploads": 1234,
  "upload_trend": [ {"date": "2026-06-28", "count": 0}, ... ]  // 最近 7 天，UTC，最旧在前
}
```

- `online_agents`：`last_seen_at` 在离线阈值（90s）内的采集器数。
- `storage_bytes`：`file_entries.size_bytes` 之和（CP 索引口径，非 MinIO 物理用量）。
- `today_uploads` / `upload_trend`：按 `uploaded_at`（UTC）统计；趋势补齐为稠密 7 天。

**统一错误响应格式：**

> 完整错误码清单与跨模块隐性契约（状态枚举大小写映射、列表信封、模板变量）见
> `docs/design/contracts.md`（参考索引，权威以代码为准）。

```json
{
  "error": {
    "code":    "AGENT_OFFLINE",
    "message": "Agent is not connected",
    "detail":  {}
  },
  "request_id": "uuid"
}
```

- 顶层 `request_id` 由 `RequestID` 中间件注入（复用请求头 `X-Request-ID`，缺失则生成 UUID v4），
  并回写到响应头 `X-Request-ID`；**所有**错误响应（handler + 认证/鉴权中间件）都经统一封装带上它，
  用于链路追踪。成功响应仅在响应头携带 `X-Request-ID`。
- 未知/拼错的 query 过滤参数不再静默按 200 忽略：`GET /api/v1/files` 对不在白名单
  （`cursor` / `limit` / `agent_id` / `bucket_id` / `file_type_id` / `status`）内的参数返回
  `400 INVALID_QUERY_PARAM`，`detail.unknown_params` 列出违规键（CC-5）。

**API 限流（CC-4）：**

- 认证后的 `/api/v1/*` 接口按用户做**固定窗口**限流：Redis 计数键 `ratelimit:api:{user_id}`，
  窗口 1 分钟（键 TTL=60s，§3.5）。上限由 `API_RATE_LIMIT_PER_MINUTE` 配置（默认 600，即
  10 req/s/用户；`<= 0` 关闭）。限流中间件在 JWT 之后执行，故按 JWT `sub`（user_id）计数。
- 被计数的响应带 `X-RateLimit-Limit` / `X-RateLimit-Remaining` 头。超限返回 `429 RATE_LIMITED`
  并附 `Retry-After: 60`。
- **失败开放**：Redis 不可用、或请求无 JWT 声明时放行且**不计数、不带**限流头（无可靠计数可报），
  记 warn 日志，避免 Redis 抖动把所有用户挡在门外（与 JWT 黑名单查询同策略）。
- `/api/auth/*`（登录/刷新）为**前置认证**接口，无 user_id 可计数，本版不在此限流范围
  （登录暴力破解防护为独立议题，另行处理）。

## 5.12 Go 项目结构

```
controlplane/
├── cmd/
│   └── server/
│       └── main.go
├── internal/
│   ├── config/
│   ├── db/
│   │   ├── postgres.go
│   │   ├── migrate.go
│   │   └── queries/
│   ├── cache/
│   ├── grpcserver/
│   │   ├── server.go
│   │   ├── registry.go
│   │   └── handler.go
│   ├── api/
│   │   ├── router.go
│   │   ├── middleware/
│   │   └── handler/
│   │       ├── auth.go
│   │       ├── agents.go
│   │       ├── files.go
│   │       ├── buckets.go
│   │       └── events.go
│   ├── agent/
│   │   ├── manager.go
│   │   ├── approval.go
│   │   └── dispatch.go
│   ├── indexer/
│   │   ├── indexer.go
│   │   └── classifier.go
│   ├── storage/
│   │   ├── minio.go
│   │   ├── sts.go
│   │   └── policy.go
│   ├── event/
│   │   ├── engine.go
│   │   ├── publisher.go
│   │   └── webhook.go
│   ├── auth/
│   │   ├── jwt.go
│   │   └── oidc.go
│   └── worker/
│       ├── offline_sweeper.go   # CC-6：Agent 在线状态 TTL 兜底扫描（已实现）
│       ├── credential_rotator.go # 规划，未实现（STS 续期现由 Agent 主动发起，见 01 §6）
│       └── event_retry.go        # 规划，未实现（重试逻辑内嵌于 event/engine.go）
├── api/
├── migrations/
│   ├── 000001_init.up.sql
│   └── 000001_init.down.sql
├── deploy/
├── go.mod
└── go.sum
```

---

# 第六章 存储层设计

## 6.1 MinIO 部署模式

### 6.1.1 模式对比

| 模式        | 单节点 SNSD | 多节点 MNMD（推荐）   | 说明                        |
|-----------|----------|----------------|---------------------------|
| **数据冗余**  | 无        | EC 纠删码         | MNMD 默认 EC:4，丢失 N/2 块仍可恢复 |
| **水平扩容**  | 不支持      | 追加 Server Pool | 新 Pool 无缝接入               |
| **最小节点数** | 1        | 4（推荐）          | 4节点起步满足生产可用性              |
| **适用场景**  | 开发/测试    | 生产环境           |                           |

### 6.1.2 生产部署配置（4节点 MNMD）

```ini
# /etc/default/minio
MINIO_VOLUMES="https://minio{1...4}.internal:9000/data{1...4}"
MINIO_OPTS="--address :9000"
MINIO_ROOT_USER=minioadmin
MINIO_ROOT_PASSWORD=<强密码>
MINIO_CERT_FILE=/etc/minio/tls/public.crt
MINIO_KEY_FILE=/etc/minio/tls/private.key
MINIO_NOTIFY_WEBHOOK_ENABLE_PRIMARY=on
MINIO_NOTIFY_WEBHOOK_ENDPOINT_PRIMARY=http://controlplane.internal:8080/internal/minio-event
MINIO_NOTIFY_WEBHOOK_AUTH_TOKEN_PRIMARY=<内部共享密钥>
```

### 6.1.3 扩容步骤（追加 Server Pool）

```bash
MINIO_VOLUMES="https://minio{1...4}.internal:9000/data{1...4} \
               https://minio{5...8}.internal:9000/data{1...4}"
# 滚动重启所有节点（逐台重启，服务不中断）
```

## 6.2 Bucket 规划与命名约定

| Bucket 名称模式      | 用途     | 备注                      |
|------------------|--------|-------------------------|
| `data-{domain}`  | 业务数据采集 | 如 data-sensor、data-logs |
| `archive-{year}` | 归档数据   | 冷数据，可配置 Lifecycle       |
| `tmp-uploads`    | 临时上传暂存 | 配置 7 天 Lifecycle 自动清理   |

**对象键命名约定：**

```
# 推荐格式：/{prefix}/{yyyy}/{mm}/{dd}/{filename}
# 采集规则变量：
{yyyy} {yy} {mm} {dd} {HH} {agent} {filename} {stem} {ext}
```

> ⚠️ **本节与实现不符（既存漂移，随 IC-1 修正）**：
> 1. 「推荐格式」只是**建议**，不是约束——D-030 第八条已定 `dest_path_template` 不受任何约束。
> 2. 变量清单与权威定义不一致：实际支持的系统变量是 `{agent_name}` / `{agent_id}` / `{filename}` / `{ext}`
>    （`pkg/trollsift/context.go`，索引见 [`contracts.md`](./contracts.md) V-3），本节的 `{agent}` / `{stem}` 不存在。
> 3. **前导 `/` 是陷阱**：agent 在拼对象键时会剥掉它（`buildStoragePath`），
>    而 CP 反解与 webui 预览都不剥——即 IC-BUG-16。归一化规则见 `contracts.md` V-3。

## 6.3 ACL 与 Policy 设计

> ⚠️ **实现偏差（D-030）**：`storage/policy.go` 的 Action 为 `PutObject / GetObject / DeleteObject / ListBucket`
> ——缺下方要求的两个 multipart Action，且多授 `DeleteObject`（IC-BUG-4）。资源前缀亦与本文 §6.2 的对象键约定
> 不一致（IC-BUG-3）。另外 `ListBucket` 是**桶级** action 却配了对象级 ARN，是一条从未生效的空转授权。
>
> 📌 **目标形态（D-030 第八条，2026-09-09）**：下方示例已按新决策改写——**拆两个 statement**
> （桶级 / 对象级 ARN 层级必须匹配），**资源为整桶**（不按前缀收窄；授权宽度是管理权限问题，
> 清点成本由分片对账解决），**只授「写」**（agent 从不调 `GetObject` / `ListObjects`，
> 二者属超授，一并砍掉）。见 [`consistency-and-ingest.md`](./consistency-and-ingest.md) §3.2。

| 账号类型            | 权限范围                   | 用途                 |
|-----------------|------------------------|--------------------|
| **controlplane-user** | 最小权限：AssumeRole、预签名 GET、建桶及委派写入 | Control Plane 真实 IAM 用户 |
| **agent-role**  | STS AssumeRole 角色      | Agent 临时扮演         |

> Control Plane 必须使用真实 IAM 用户：MinIO service account 不能调用 `AssumeRole`。其父 policy
> 只包含 CP 直接需要的 `CreateBucket` / `GetObject`，以及下方 session policy 可委派的四个写入 Action；
> 不使用 `readwrite` / `consoleAdmin`，避免额外授出删除、列举与管理权限。资源使用 bucket 通配是因为
> `POST /api/v1/buckets` 可在运行时动态建桶，静态桶清单会令新桶上的预签名下载和 STS 委派失效。

**STS Session Policy（按 Agent 已下发规则涉及的 bucket 集合生成）：**

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": ["s3:ListBucketMultipartUploads"],
      "Resource": ["arn:aws:s3:::data-sensor"]
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:AbortMultipartUpload", "s3:ListMultipartUploadParts"
      ],
      "Resource": ["arn:aws:s3:::data-sensor/*"]
    }
  ]
}
```

> 桶级 action（`ListBucketMultipartUploads`）的 Resource **不带 `/*`**，对象级 action 的**带**——
> 层级不匹配的授权不会报错，只是静默失效。

## 6.4 STS AssumeRole 凭据下发机制

（见第五章 5.7 节代码实现）

## 6.5 事件通知配置

> ⚠️ **定位澄清（D-025 补充 §3 / D-030）**：minio-event **只应作为对账兜底**，不承担主索引职责。
> 当前实现与此相反——它是唯一的写入路径。另有两处必须修的缺陷：`queue_dir` 位于易失的 `/tmp`
> （MinIO 重启即丢未投递事件，IC-BUG-9）；CP 索引失败仍返回 200，MinIO 据此丢弃事件、永不重投（IC-BUG-6）。
> 且通过 API 新建的 bucket 不会注册通知规则（IC-BUG-7）。见
> [`consistency-and-ingest.md`](./consistency-and-ingest.md) §1.3。
>
> 📌 **目标形态（D-031）**：本节的 `notify_webhook` 将改为 `notify_nats` + JetStream，
> 以获得「投递与处理解耦 / 可重放 / 全局单调序号（供排序键与链路自证）」三项能力，排期在对账阶段（IC-11）。
> 切换后 D-014 的共享密钥鉴权由 NATS creds/nkey/TLS 取代。

```bash
mc admin config set myminio notify_webhook:primary \
    endpoint="http://controlplane.internal:8080/internal/minio-event" \
    auth_token="<共享密钥>" \
    queue_limit="10000" \
    queue_dir="/tmp/minio-webhook-queue"

mc event add myminio/data-sensor arn:minio:sqs::primary:webhook \
    --ignore-existing \
    --event "put,delete"
```

**Control Plane 侧鉴权（D-014）**：`/internal/minio-event` 会把外部输入写入
`file_entries`，故必须鉴权。CP 用配置项 `INTERNAL_WEBHOOK_SECRET` 校验 MinIO 发来的
`auth_token`（`Authorization` 头，兼容 `Bearer <token>` 与裸 token，常量时间比较）。
**未配置密钥时端点失败即拒（fail-closed）**，拒绝一切请求而非放行，避免未鉴权写入。

## 6.6 MinIO 管理功能在后台的集成

| 功能                    | 实现方式                                 | 触发时机                 |
|-----------------------|--------------------------------------|----------------------|
| **创建 Bucket**         | madmin.MakeBucket()                  | POST /api/v1/buckets |
| **设置 Policy**         | madmin.SetBucketPolicy()             | Bucket 创建后           |
| **创建 IAM 用户 + 最小 policy** | `mc admin user add` + `policy create/attach` | `init-minio.sh` 系统初始化 |
| **STS AssumeRole**    | credentials.NewSTSAssumeRole()       | Agent 连接时            |
| **Bucket 存储用量**       | madmin.BucketUsageInfo()             | Dashboard，每 5 分钟缓存   |
| **Lifecycle 规则**      | s3.PutBucketLifecycleConfiguration() | tmp-uploads 7天清理     |

## 6.7 存储容量规划参考

> 📌 **补充（D-030）**：下表只按字节规划，未按**对象数**推演。按第一行（高频小文件、50 台采集器）换算
> 约 **2600 万对象/年**，3–5 年到**上亿**——每个对象对应一行 `file_entries`。该量级下
> 「周期性全量列举 MinIO 对账」不成立，索引表也需要分区。见
> [`consistency-and-ingest.md`](./consistency-and-ingest.md) §1.1 / §3.4。

| 场景         | 文件频率  | 单文件大小    | 单采集器/天 | 50台/年   |
|------------|-------|----------|--------|---------|
| **高频小文件**  | 每分钟1个 | 5 MB     | 7.2 GB | ~130 TB |
| **低频大文件**  | 每小时1个 | 500 MB   | 12 GB  | ~219 TB |
| **追加日志文件** | 每天1个  | 200 MB/天 | 200 MB | ~3.6 TB |

*MinIO MNMD 纠删码开销：EC:4 配置下，实际可用容量约为原始容量的 50%。*

---

# 第七章 Web UI 设计

## 7.1 技术选型

| 层次           | 选型                           | 说明                         |
|--------------|------------------------------|----------------------------|
| **UI 框架**    | React 18                     | hooks 生态成熟                 |
| **组件库**      | Ant Design 5 + ProComponents | ProTable、ProForm、ProLayout |
| **路由**       | React Router v6              | 嵌套路由，权限路由守卫                |
| **状态管理**     | Zustand                      | 轻量，全局 Token、用户信息           |
| **HTTP 客户端** | Axios + SWR                  | Token 注入、401 刷新、数据缓存       |
| **多文件下载**    | StreamSaver.js               | 浏览器端流式打包 zip               |
| **构建工具**     | Vite                         | 开发热更新，生产静态文件               |

## 7.2 页面模块清单

```
/login
/dashboard
/agents
/agents/pending
/agents/:id
/agents/:id/rules
/agents/:id/rules/create
/agents/:id/logs
/files
/files/:id
/file-types
/file-types/create
/file-types/:id
/buckets
/events
/logs
/settings
/settings/users
/settings/profile
```

## 7.3 核心页面设计

### 7.3.1 仪表盘

```
┌─────────────────────────────────────────────────────────────────┐
│  统计卡片行                                                      │
│  ┌──────────┐ ┌──────────┐ ┌──────────┐ ┌──────────┐          │
│  │ 在线采集器│ │ 今日上传 │ │ 总文件数 │ │ 存储用量 │          │
│  │   12/15  │ │ 1,234个  │ │ 89,234   │ │ 2.3 TB   │          │
│  └──────────┘ └──────────┘ └──────────┘ └──────────┘          │
├─────────────────────────────────────────────────────────────────┤
│  左侧：近7日上传量趋势图  │  右侧：采集器在线状态列表            │
├─────────────────────────────────────────────────────────────────┤
│  底部：最近上传日志（最新20条）                                   │
└─────────────────────────────────────────────────────────────────┘
```

统计卡片与 7 日趋势的数据来自 `GET /api/v1/stats/dashboard`（§5.11.5），
由服务端聚合返回真实值；采集器在线状态列表来自 `GET /api/v1/agents`，
最近上传日志来自 `GET /api/v1/upload-logs`。

### 7.3.2 采集规则创建表单（分步，共3步）

- 第1步：基本配置（规则名称、采集模式、目标 Bucket）
- 第2步：源路径配置（Watch 模式 / Scheduled 模式字段不同）
- 第3步：上传路径配置（模板变量 + 实时预览）

### 7.3.3 文件浏览器

支持多维度检索（文件类型、采集器、时间范围、路径前缀）+ 批量下载。

## 7.4 文件下载实现

### 7.4.1 单文件下载（预签名 URL）

```javascript
async function downloadSingle(fileId) {
  const { url } = await api.get(`/files/${fileId}/download-url`)
  const a = document.createElement("a")
  a.href = url; a.download = fileName; a.click()
}
```

### 7.4.2 多文件批量下载（StreamSaver.js）

```javascript
import streamSaver from "streamsaver"
import { zip } from "fflate"

async function downloadBatch(fileIds) {
  const urls = await api.post("/files/batch-download-urls", { ids: fileIds })
  const fileStream = streamSaver.createWriteStream("download.zip")
  const writer = fileStream.getWriter()
  const zipStream = new zip.Zip((err, chunk, final) => {
    if (err) { writer.abort(); return }
    writer.write(chunk)
    if (final) writer.close()
  })
  for (const { fileName, url } of urls) {
    const response = await fetch(url)
    const file = new zipStream.ZipDeflate(fileName, { level: 0 })
    zipStream.add(file)
    const reader = response.body.getReader()
    while (true) {
      const { done, value } = await reader.read()
      if (done) { file.push(new Uint8Array(0), true); break }
      file.push(value)
    }
  }
  zipStream.end()
}
```

**注意事项：**
- 依赖 Service Worker，需要 HTTPS；
- 单次批量建议不超过 200 个文件；
- 浏览器兼容：Chrome 57+、Firefox 65+、Edge 79+，Safari 不支持。

## 7.5 前端项目结构

```
webui/
├── src/
│   ├── main.tsx
│   ├── App.tsx
│   ├── layouts/
│   │   ├── BasicLayout.tsx
│   │   └── AuthLayout.tsx
│   ├── pages/
│   │   ├── Dashboard/
│   │   ├── Agents/
│   │   │   ├── index.tsx
│   │   │   ├── Detail.tsx
│   │   │   └── RuleForm.tsx
│   │   ├── Files/
│   │   ├── FileTypes/
│   │   ├── Buckets/
│   │   ├── Events/
│   │   ├── Logs/
│   │   └── Settings/
│   ├── components/
│   │   ├── AgentStatusBadge.tsx
│   │   ├── DirectoryTree.tsx
│   │   └── BatchDownload.tsx
│   ├── services/
│   │   ├── api.ts
│   │   ├── agents.ts
│   │   ├── files.ts
│   │   └── events.ts
│   ├── store/
│   │   ├── auth.ts
│   │   └── global.ts
│   └── utils/
│       ├── download.ts
│       └── pathTemplate.ts
├── index.html
├── vite.config.ts
└── package.json
```

---

# 第八章 Client SDK 设计

## 8.1 SDK 功能范围

| 功能模块       | 说明                 | 对应 API                          |
|------------|--------------------|---------------------------------|
| **认证**     | 用户名密码登录，Token 自动刷新 | POST /auth/login, /auth/refresh |
| **文件查询**   | 按文件类型、时间范围等条件检索    | GET /files                      |
| **文件下载**   | 获取预签名 URL，支持直接写入本地 | GET /files/{id}/download-url    |
| **批量下载**   | 批量获取预签名 URL        | POST /files/batch-download-urls |
| **文件类型查询** | 列出所有文件类型           | GET /file-types                 |
| **采集器查询**  | 查询采集器列表和状态（只读）     | GET /agents                     |
| **上传日志查询** | 查询上传历史记录           | GET /upload-logs                |

**设计原则：**

- 强类型；Token 透明管理；分页透明（迭代器接口）；错误语义化；可配置重试。

## 8.2 Python SDK

```python
from fileagent import FileAgentClient, FileQuery
from datetime import datetime, timedelta

client = FileAgentClient(
    base_url="https://control.example.com",
    username="api-user",
    password="secret",
    timeout=30,
    max_retries=3,
    verify_ssl=True,
)

# 迭代器查询（推荐）
query = FileQuery(
    file_type_name="var_hourly",
    agent_ids=["uuid-agent-a"],
    start_time=datetime.now() - timedelta(days=7),
    end_time=datetime.now(),
    path_prefix="/var/2025/04/",
    page_size=100,
)
for file_entry in client.files.iter(query):
    print(file_entry.file_name, file_entry.size_bytes)

# 单文件下载
client.files.download(file_id="file-uuid", dest_path="/local/data/file.bin")

# 流式下载
with client.files.stream("file-uuid") as stream:
    with open("/local/output.bin", "wb") as f:
        for chunk in stream.iter_content(chunk_size=8192):
            f.write(chunk)
```

**异常类型：** `AuthenticationError` / `PermissionError` / `NotFoundError` / `RateLimitError` / `ServerError` / `NetworkError`

### Python SDK 项目结构

```
fileagent-python/
├── fileagent/
│   ├── __init__.py
│   ├── client.py
│   ├── auth.py
│   ├── http.py
│   ├── resources/
│   │   ├── files.py
│   │   ├── file_types.py
│   │   ├── agents.py
│   │   └── upload_logs.py
│   ├── models/
│   │   ├── file_entry.py
│   │   ├── file_type.py
│   │   ├── agent.py
│   │   └── pagination.py
│   └── exceptions.py
├── tests/
├── pyproject.toml
└── README.md
```

## 8.3 Java SDK

```java
FileAgentConfig config = FileAgentConfig.builder()
    .baseUrl("https://control.example.com")
    .username("api-user")
    .password("secret")
    .timeoutSeconds(30)
    .maxRetries(3)
    .build();
FileAgentClient client = new FileAgentClient(config);

// 迭代器查询
FileQuery query = FileQuery.builder()
    .fileTypeName("var_hourly")
    .startTime(Instant.now().minus(7, ChronoUnit.DAYS))
    .endTime(Instant.now())
    .pageSize(100)
    .build();
client.files().iterate(query).forEachRemaining(file -> {
    System.out.println(file.getFileName());
});

// 下载
client.files().download("file-uuid", Path.of("/local/data/file.bin"));
```

**Java SDK 依赖：** OkHttp3 + Jackson + Lombok，最低 Java 11。

## 8.4 Token 管理实现细节

```python
class TokenManager:
    def get_access_token(self) -> str:
        with self._lock:
            if self._is_expiring_soon():  # 剩余 < 5 分钟时主动刷新
                self._refresh()
            return self._access_token

    def _refresh(self):
        if self._refresh_token:
            try:
                resp = self._http.post("/api/auth/refresh", ...)
                self._update_tokens(resp); return
            except AuthenticationError:
                pass
        # 重新登录
        resp = self._http.post("/api/auth/login", ...)
        self._update_tokens(resp)
```

## 8.5 分页迭代器实现

**REST API 分页参数规范（cursor-based）：**

```
GET /api/v1/files?page_size=50&cursor=<opaque_string>

响应：
{
  "items": [...],
  "total": 1234,
  "next_cursor": "eyJpZCI6InV1aWQxMjMifQ==",
  "has_more": true
}
```

---

# 第九章 监控与运维

## 9.1 监控组件栈

| 组件               | 版本   | 职责              |
|------------------|------|-----------------|
| **Prometheus**   | v2.x | 指标采集与存储，15天本地保留 |
| **Grafana**      | v10+ | 指标可视化 Dashboard |
| **Loki**         | v3.x | 日志聚合存储          |
| **Promtail**     | v3.x | 日志采集 Agent      |
| **Alertmanager** | v0.x | 告警路由与去重         |

## 9.2 各组件 Metrics 指标清单

### Control Plane 自定义指标

```
fileagent_agents_online_total{org_id}
fileagent_agent_connections_total{event="connect|disconnect"}
fileagent_grpc_request_duration_seconds{method}
fileagent_http_requests_total{method, path, status_code}
fileagent_http_request_duration_seconds{method, path}
fileagent_file_entries_created_total{org_id, file_type}
fileagent_event_deliveries_total{rule_id, status}
fileagent_webhook_retry_queue_depth
fileagent_sts_issued_total
```

### Agent 上报指标

```
fileagent_agent_queue_depth{agent_id, agent_name}
fileagent_agent_upload_bps{agent_id}
fileagent_agent_disk_free_bytes{agent_id, mount_path}
fileagent_agent_uploaded_files_total{agent_id, status}
fileagent_agent_info{agent_id, version, os, arch}
```

## 9.3 核心告警规则

```yaml
groups:
- name: fileagent
  rules:
  - alert: ManyAgentsOffline
    expr: fileagent_agents_online_total < (count(fileagent_agent_info) * 0.7)
    for: 5m
    labels: { severity: warning }
  - alert: AgentQueueBacklog
    expr: fileagent_agent_queue_depth > 1000
    for: 10m
    labels: { severity: warning }
  - alert: MinioLowDiskSpace
    expr: minio_cluster_capacity_usable_free_bytes / minio_cluster_capacity_usable_total_bytes < 0.2
    for: 0m
    labels: { severity: critical }
  - alert: HighAPIErrorRate
    expr: rate(fileagent_http_requests_total{status_code=~"5.."}[5m]) / rate(fileagent_http_requests_total[5m]) > 0.05
    for: 2m
    labels: { severity: critical }
```

## 9.4 日志采集方案

所有组件统一输出结构化 JSON 日志，由 Promtail 采集后发送至 Loki。

## 9.5 Grafana Dashboard 规划

| Dashboard | 核心面板                           |
|-----------|--------------------------------|
| **系统总览**  | 在线采集器数、今日上传量、存储用量、API QPS、错误率  |
| **采集器详情** | 队列深度趋势、上传速率、磁盘剩余、历史在线状态        |
| **存储层**   | MinIO 各节点磁盘、入站/出站流量、S3 API QPS |
| **事件系统**  | Webhook 成功/失败率、重试队列深度          |
| **数据库**   | PostgreSQL 连接数、QPS、复制延迟        |

---

# 第十章 部署方案

## 10.1 All-in-One 单机部署（开发/小规模）

- OS：Ubuntu 22.04 LTS / Rocky Linux 9
- 建议配置：8C 16G RAM，500GB SSD
- MinIO 使用单节点模式（SNSD）

### 单二进制分发（含 Web UI）

Control Plane 支持把编译后的 Web UI（`webui/dist`）**嵌入自身二进制**，运维只需分发一个
`controlplane` 二进制即可同时提供 REST API 与 Web 管理界面，无需额外部署静态站点或前置代理转发。
数据库迁移同样**嵌入二进制**（`//go:embed`，见 §10.6 / D-023），启动时自动应用——分发时无需随行
`migrations/` 目录，真正做到"一个二进制 + 一份配置"。

- 构建：`make bundle`（编译 webui → 拷入 CP 嵌入目录 → `go build -tags webui`）。默认 `make build`
  仍产出**纯 API** 二进制（不含前端，`/` 返回 404）。见 [D-022]、[D-023]。
- 落地产物：`controlplane/Dockerfile`（多阶段，运行镜像不含 migrations/）、
  `deploy/docker-compose.prod.yml`（全栈 all-in-one）、`deploy/caddy/Caddyfile`；主机部署见
  `deploy/systemd/*.service`。完整步骤见 `docs/ops/deployment.md`。
- 路由：SPA 由 CP 的 HTTP 服务在**同源**下提供——API 全在 `/api/*`、`/internal/*`、`/healthz`，
  其余路径服务前端静态文件，未命中的客户端路由回退 `index.html`（BrowserRouter）。
- 同源提供 → **无需 CORS**；webui 的 API base 为相对路径 `/`（`webui/src/services/api.ts`）。
- 开发期仍分离：webui 走 `pnpm dev` + vite proxy（`/api → :8080`，见 `webui/vite.config.ts`），
  不使用嵌入产物。

## 10.2 生产最小化部署（节点规划）

| 节点组       | 数量 | 配置               | 部署服务                                 |
|-----------|----|------------------|--------------------------------------|
| **应用节点**  | 3台 | 4C 8G            | Control Plane + Redis + NATS + Caddy |
| **数据库节点** | 2台 | 4C 8G + 500G SSD | PostgreSQL 主从 + Patroni              |
| **存储节点**  | 4台 | 4C 8G + 数据盘      | MinIO MNMD                           |
| **监控节点**  | 1台 | 4C 8G            | Prometheus + Grafana + Loki          |

## 10.3 systemd Service 文件

> **权威文件**（可直接使用）：`deploy/systemd/controlplane.service` 与
> `deploy/systemd/fileagent-agent.service`；安装步骤见 `docs/ops/deployment.md` 路径 B。
> CP 单元 `ExecStart=/opt/fileagent/controlplane`（单二进制，内嵌 Web UI + 迁移，无需
> `WorkingDirectory` 指向 migrations），`StateDirectory=fileagent` 承载 bootstrap 凭据文件；
> agent 单元 `StateDirectory` 承载 SQLite 队列/指纹/token，日志由 journald 捕获。

## 10.4 Caddy 网关配置

### 公网部署

```
control.example.com {
  reverse_proxy /api/* localhost:8080
  root * /opt/fileagent/webui/dist
  file_server
  try_files {path} /index.html
}

grpc.example.com {
  reverse_proxy h2c://localhost:9090
}

minio.example.com {
  reverse_proxy minio1.internal:9000
}
```

### 内网部署

```
{
  acme_ca https://step-ca.internal:9000/acme/acme/directory
  acme_ca_root /etc/caddy/step-ca-root.crt
}

control.internal {
  reverse_proxy /api/* localhost:8080
  root * /opt/fileagent/webui/dist
  file_server
  try_files {path} /index.html
}
```

### MinIO endpoint 角色（internal / public 拆分，D-024）

Control Plane 对 MinIO 用**两个**端点，配置项独立，网关无关：

| 角色 | 配置 | 用途 | 可达要求 |
|------|------|------|----------|
| **internal** | `MINIO_ENDPOINT` / `MINIO_USE_SSL` | CP 自身调用：STS `AssumeRole`、建桶 admin | 仅需 CP 可达（内网服务名，如 `minio:9000`） |
| **public** | `MINIO_PUBLIC_ENDPOINT` / `MINIO_PUBLIC_USE_SSL` | 写入 STS payload 交给 agent；presign 下载 URL 的签名 host | 须对**浏览器 / agent** 可达（宿主 LAN IP 或网关地址） |

`MINIO_PUBLIC_*` 缺省回落到 `MINIO_*`（单端点部署向后兼容）。生产把 MinIO 置于 TLS 网关之后
（上面的 `minio.example.com` 反代即一例，但**任何**终结代理均可），并将 `MINIO_PUBLIC_ENDPOINT`
指向网关地址；CP 仍走 internal 端点，流量留在内网、不 hairpin。presign 为本地签名（不发网络），
故 public 端点只影响 URL 的签名 host，不引入额外网络跳。

## 10.5 内网 TLS 方案（step-ca）

```bash
# 初始化 CA
step ca init \
  --name "FileAgent Internal CA" \
  --dns step-ca.internal \
  --address :9000 \
  --provisioner acme

# 导出根证书
step ca root > /etc/fileagent/ca-root.crt
```

## 10.6 数据库初始化与迁移方案

迁移文件（`controlplane/migrations/*.sql`）经 `//go:embed` **嵌入 CP 二进制**
（`controlplane/migrations/embed.go`），Control Plane 启动时用 golang-migrate 的 `iofs` source
**自动应用**（幂等：已最新则 no-op），见 [D-023]。因此：

- 生产部署**无需** `migrate` CLI，也无需随二进制分发 `migrations/` 目录或设置 `MIGRATIONS_PATH`
  （该配置项已移除）。
- 升级只需发布新二进制并重启——新迁移在启动时应用。迁移文件**只追加不改**（契约约束）。

```
# 迁移文件命名规范（golang-migrate）
controlplane/migrations/
├── 000001_init_schema.up.sql
├── 000001_init_schema.down.sql
└── ...
```

> 开发期如需手动操作，仍可用 golang-migrate CLI：
> `migrate -database "$DATABASE_URL" -path controlplane/migrations up`。

## 10.7 版本升级策略

**Control Plane：** 无状态，可逐台滚动重启，升级前先执行数据库迁移。

**Agent：** 通过 gRPC 指令下发新版本下载地址，Agent 自动下载、校验 SHA-256、替换自身、通过 systemd 重启。

---

# 附录 A 名词表

| 术语                 | 定义                                       |
|--------------------|------------------------------------------|
| **Control Plane**  | 控制平面，系统的中心服务器                            |
| **Data Plane**     | 数据平面，Agent → MinIO 直传路径                  |
| **Edge Agent**     | 边缘采集器，Go 单二进制程序                          |
| **STS**            | Security Token Service，临时安全凭据服务          |
| **Fingerprint**    | 设备唯一标识符，Agent 首次启动时生成                    |
| **FileType**       | 文件逻辑分类，通过路径 glob 规则自动匹配                  |
| **FileTypeRule**   | 文件类型匹配规则，定义存储路径的 glob 模式                 |
| **Watch 模式**       | 持续监控目录的采集模式                              |
| **Scheduled 模式**   | 按 cron 表达式触发的定时采集模式                      |
| **append_mode**    | 追加写入文件处理策略：overwrite / close_wait / tail |
| **MNMD**           | Multi-Node Multi-Drive，MinIO 分布式部署模式     |
| **Presigned URL**  | 预签名 URL，带时限的对象访问链接                       |
| **NATS JetStream** | NATS 消息系统的持久化消息流功能                       |
| **org_id**         | 组织 ID，多租户预留字段                            |

---

# 附录 B 接口速查

## B.1 gRPC 接口汇总

| RPC 方法             | 类型                   | 说明          |
|--------------------|----------------------|-------------|
| Register           | Unary                | Agent 注册申请  |
| PollApproval       | Unary                | 轮询审批结果      |
| Connect            | Bidirectional Stream | 主控制通道       |
| RefreshCredentials | Unary                | 主动续期 STS 凭据 |

## B.2 REST API 汇总

| Method | Path                                | 说明                |
|--------|-------------------------------------|-------------------|
| POST   | /api/v1/auth/login                  | 登录                |
| POST   | /api/v1/auth/refresh                | 刷新 Token          |
| POST   | /api/v1/auth/logout                 | 登出                |
| GET    | /api/v1/auth/me                     | 当前用户信息            |
| GET    | /api/v1/users                       | 用户列表              |
| POST   | /api/v1/users                       | 创建用户              |
| PUT    | /api/v1/users/{id}                  | 更新用户              |
| DELETE | /api/v1/users/{id}                  | 禁用用户              |
| GET    | /api/v1/agents                      | 采集器列表             |
| GET    | /api/v1/agents/{id}                 | 采集器详情             |
| POST   | /api/v1/agents/{id}/approve         | 审批通过              |
| POST   | /api/v1/agents/{id}/revoke          | 吊销采集器             |
| POST   | /api/v1/agents/{id}/list-dir        | 下发列目录指令           |
| GET    | /api/v1/agents/{id}/rules           | 采集规则列表            |
| POST   | /api/v1/agents/{id}/rules           | 创建采集规则            |
| PUT    | /api/v1/agents/{id}/rules/{rid}     | 更新采集规则            |
| DELETE | /api/v1/agents/{id}/rules/{rid}     | 删除采集规则            |
| GET    | /api/v1/files                       | 文件条目查询            |
| GET    | /api/v1/files/{id}                  | 文件详情              |
| GET    | /api/v1/files/{id}/download-url     | 生成预签名下载 URL       |
| POST   | /api/v1/files/batch-download-urls   | 批量生成预签名 URL       |
| GET    | /api/v1/file-types                  | 文件类型列表            |
| POST   | /api/v1/file-types                  | 创建文件类型            |
| PUT    | /api/v1/file-types/{id}             | 更新文件类型            |
| DELETE | /api/v1/file-types/{id}             | 删除文件类型            |
| GET    | /api/v1/buckets                     | Bucket 列表         |
| POST   | /api/v1/buckets                     | 创建 Bucket         |
| GET    | /api/v1/event-rules                 | 事件规则列表            |
| POST   | /api/v1/event-rules                 | 创建事件规则            |
| PUT    | /api/v1/event-rules/{id}            | 更新事件规则            |
| DELETE | /api/v1/event-rules/{id}            | 删除事件规则            |
| GET    | /api/v1/event-rules/{id}/deliveries | 事件投递历史            |
| GET    | /api/v1/upload-logs                 | 上传日志查询            |
| GET    | /api/v1/stats/dashboard             | 仪表盘聚合统计           |
| POST   | /internal/minio-event               | MinIO 事件回调（仅内部访问） |

---

# 附录 C 配置项速查

## C.1 Control Plane 关键配置

```env
DATABASE_URL=postgres://user:pass@host:5432/dbname?sslmode=require
DATABASE_MAX_CONNS=25
REDIS_URL=redis://localhost:6379/0
NATS_URL=nats://localhost:4222
MINIO_ENDPOINT=minio.internal:9000
MINIO_ACCESS_KEY=<ak>
MINIO_SECRET_KEY=<sk>
MINIO_USE_SSL=true
INTERNAL_WEBHOOK_SECRET=<共享密钥>
JWT_SECRET=<256位随机字符串>
JWT_ACCESS_TOKEN_TTL=2h
JWT_REFRESH_TOKEN_TTL=720h
AGENT_TOKEN_TTL=720h
STS_CREDENTIAL_TTL=3600
STS_REFRESH_THRESHOLD=600
API_RATE_LIMIT_PER_MINUTE=600
GRPC_PORT=9090
HTTP_PORT=8080
METRICS_LISTEN=:9200
AGENT_HEARTBEAT_INTERVAL=30s
AGENT_OFFLINE_THRESHOLD=90s
LOG_LEVEL=info
LOG_FORMAT=json
```

## C.2 Agent 关键配置

```toml
[server]
endpoint    = "grpc.example.com:443"
tls_ca_cert = ""
dial_timeout = "10s"

[agent]
data_dir           = "/var/lib/fileagent"
log_dir            = "/var/log/fileagent"
heartbeat_interval = "30s"
reconnect_delay    = "5s"
reconnect_max_delay = "5m"

[upload]
concurrency         = 3
part_size_mb        = 64
queue_max_size      = 10000
retry_max           = 10
retry_initial_delay = "1m"
retry_max_delay     = "60m"

[metrics]
enabled = true
listen  = ":9100"

[log]
level       = "info"
max_size_mb = 100
max_backups = 7
compress    = true
```

---

# 附录 D 已知限制与后续迭代计划

## D.1 第一版已知限制

| 限制项                  | 说明与临时方案                        |
|----------------------|--------------------------------|
| **多租户**              | 第一版仅单组织，org_id 已预留             |
| **LDAP/OIDC**        | 仅内置账号，OIDC 接口返回 501            |
| **Agent mTLS**       | 仅 Bearer Token，无双向证书认证         |
| **tail 增量上传**        | 第一版建议使用 overwrite 或 close_wait |
| **批量下载 Safari**      | StreamSaver.js 不支持 Safari      |
| **文件删除**             | 不支持通过管理后台删除 MinIO 对象           |
| **Control Plane HA** | 第一版单实例                         |

## D.2 后续迭代计划

| 优先级 | 功能                  | 说明                       |
|-----|---------------------|--------------------------|
| P1  | Control Plane HA    | HAProxy L4 + 多实例         |
| P1  | append_mode tail 实现 | 大文件增量上传                  |
| P1  | Agent 远程升级          | gRPC 指令推送新版本             |
| P2  | 多租户支持               | 启用 org_id 隔离             |
| P2  | OIDC/SSO 对接         | Keycloak 或企业 AD          |
| P2  | Agent mTLS          | 双向证书认证                   |
| P3  | 文件管理功能              | 对象删除、移动、重命名              |
| P3  | Go SDK              | 补充 Go 语言 Client SDK      |
| P3  | Electron 下载工具       | 桌面端文件浏览器                 |
| P4  | 数据归档                | MinIO Lifecycle 规则 UI 配置 |
