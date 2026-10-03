# 存储层替代调研：MinIO / SeaweedFS / JuiceFS 评估矩阵与建议

> 2026-10-03 · 本机实测（macOS + OrbStack，SSD）· 对应 [`active.md`](../tasks/active.md)「下一步」第 4 条
> 产出是**矩阵 + 建议**；换不换由产品拍板。D-030「不换存储层」的前提（MinIO 稳定可得）已失效（D-036）。
> 测试工具与原始结果在 [`research/storage-layer-eval/`](../../research/storage-layer-eval/)
> （能力探针、元数据订阅器、压测与冷缓存脚本、Python 生态测试、各候选 compose、`results/`）。
> **证据分三类**：①**仓内可复算**——数字出自 `results/` 的原始数据，或由 `aggregate.py`、`subscribe_summary.py`、
> `hdd_model.py` 从中算出；②**模型**——机械盘外推，输入为外部基线（minio-inventory），见 Tier 4；
> ③**现场记录**——测试时在终端上观察到、原始输出未留存的数字，正文逐项标注「现场记录」，工具仍可重跑。

**怎么读**：§1 矩阵（含 Tier 5 Python 生态）→ §4 建议。§2/§3 是选 SeaweedFS 时的改造面与硬约束，§6 是本轮未实测的其他候选。

## 0. 被测版本（对象存储与辅助镜像均按 digest 钉定）

| 候选 | 镜像 | 形态 |
|---|---|---|
| MinIO（对照组 / 默认选项） | `ghcr.io/byw-dev/minio@sha256:a66e1fd7…`（RELEASE.2025-04-22，D-036 自持） | 单进程 |
| SeaweedFS 4.48 CE | `chrislusf/seaweedfs@sha256:4e61d15f…` | `weed server`：master+volume+filer+s3 单进程 |
| SeaweedFS 4.48 Enterprise | `chrislusf/seaweedfs-enterprise@sha256:9eb82b98…` | 同上（仅功能对照，已出局） |
| JuiceFS CE 1.4.1 | `juicedata/mount@sha256:ab99388a…` | `juicefs gateway` + 独立 PostgreSQL 15（另测 Redis 7 变体） |
| 辅助：JuiceFS 元数据库 | `postgres@sha256:09e4f20b…`（15-alpine）、`redis@sha256:286bd426…`（7-alpine） | — |
| 辅助：冷缓存测量 | `alpine@sha256:294b683c…`（读 `/proc/diskstats`、清页缓存） | — |

完整 digest 见 `research/storage-layer-eval/` 下各 compose 与 `io-cold.sh`。

客户端统一用项目同版本 **minio-go v7.0.91**，客户端选项照搬 agent。

## 1. 判据矩阵

图例：✅ 原生满足 · 🟡 需适配 / 有条件 · ❌ 不满足 · — 不适用

### Tier 0 —— 硬闸门

| # | 判据 | MinIO | SeaweedFS CE | JuiceFS |
|---|---|---|---|---|
| G1 | STS AssumeRole + 内联 session policy（11 项含负向；放宽 policy 的负向对照均翻红） | ✅ | ✅（需 `iam.json` 声明 role + trust policy） | ✅（MinIO 原生 STS） |
| G1.12 | 凭据到期后被拒、到期前可用（注：minio-go 会把 <3600s 的请求钳成 3600s，本项用手工签名绕过） | ✅ 900s 凭据：+840s 可用、+930s 被拒、重签即恢复 | ✅ 900s 凭据：+840s 可用、+930s 被拒、重签即恢复 | ✅ 900s 凭据：+840s 可用、+930s 被拒、重签即恢复 |
| G1.13 | 存储服务重启（优雅 / `kill -9`）后，已签发的 STS 凭据继续有效 | ✅ | ✅ | ✅ |
| G2 | Multipart：64MB 分片、换客户端续传、Abort、错 ETag 拒绝、服务重启后续传 | ✅ | ✅ | ✅ |
| G3 | 预签名 GET（签名绑定 Host，D-024 行为一致） | ✅ | ✅ | ✅ |
| G4 | 对象事件：S3 Records 格式 | ✅ | ❌ 无桶通知；filer 事件需适配（约 50 行翻译） | ✅ 与 MinIO 一字不差 |
| G4.5 | 事件接收端宕机后补投 | ✅ | 🟡 webhook 内存队列会丢；**元数据订阅可回放**（见 §3） | ✅ |
| G4.7 | **存储服务被 `kill -9` 后事件不丢**（实测：接收端离线时写 200 → 硬杀 → 200/200；写完 300 立即硬杀，杀前仅发出 18/0 → 重启后 300/300） | ✅ `queue_dir` 先落盘再确认 | ❌ 订阅方同时离线时丢最近 ≤60s（§3） | ✅ 同 MinIO |
| G5 | 脚本化 IAM / policy | ✅ `mc` | ✅ 声明式 `s3.json`/`iam.json` | 🟡 新版 `mc` 与 2021 admin API 不全兼容（绑定 policy 走旧接口） |
| G5 | Lifecycle：「清理未完成分片」规则（**现场记录**；规则文件见 `lifecycle/`） | ❌ 拒绝或静默剥离 | 🟡 能存；执行需额外 admin+worker | ❌ lifecycle 完全未实现 |

### Tier 1 —— S3 语义细节

| 判据 | MinIO | SeaweedFS | JuiceFS |
|---|---|---|---|
| `a` 与 `a/b` 共存（两种顺序） | ✅ | ✅ | ❌ **POSIX 冲突，且返回 HTTP 500**（agent 会当可重试） |
| 特殊字符 / 中文 / emoji 键、元数据、覆盖写、分页、delimiter、404、幂等删除 | ✅ | ✅ | ✅ |
| 校验和与签名模式（5 种） | ✅ 5/5 | 🟡 4/5：minio-go 大小未知（-1）流式上传 BadDigest——我们的代码不走，**Python 生态路径上未复现**（Tier 5 A7） | 🟡 3/5：trailing checksum 不支持——**agent 不开** |

### Tier 2 —— 供给与合规

| 判据 | MinIO | SeaweedFS CE | JuiceFS |
|---|---|---|---|
| 许可证 | ❌ AGPL，义务在我方（D-036） | ✅ Apache-2.0 | ✅ Apache-2.0（网关核心 `juicedata/minio` fork 自 AGPL 之前，仍维护） |
| 公开供给 / 补丁 | ❌ 已断供，无升级路径 | ✅ Docker Hub，活跃（2026-09-28 发版） | ✅ 活跃 |
| 其他 | — | 企业版：生产必付费 + EULA 禁分发 → **出局**；功能实测与 CE 完全一致 | 默认上报使用统计，须 `--no-usage-report` |

### Tier 3 —— 单机运维形态

| 判据 | MinIO | SeaweedFS | JuiceFS |
|---|---|---|---|
| 组件 | 1 进程 | 1 进程（`weed server`）+ 生命周期需 worker | 网关 + **外部元数据库** + 数据后端 |
| 重启耗时（**现场记录**） | 秒级 | 优雅关闭约 25s（**须 `stop_grace_period` ≥ 60s**，否则丢事件，见 §3 e6 / e7） | 0.3s（网关无状态） |
| 内存（1M 对象，三轮） | 471–475MB | 0.75–1.03GB | 223–259MB + PG |
| 已知运维坑 | `queue_dir` 必须持久卷（IC-BUG-9） | **filer webhook 目标不可达时内存无界增长**（灌 20 万：RSS 5.07GB 对 0.67GB，≈22KB/事件，写入 5,315 对 8,728 obj/s，`results/ab-mem.log`；首轮 1M 压测内存 10.4GiB，`results/bench/results.jsonl` 的 `seaweedfs` 行；峰值 15.5GB 为现场记录）→ 不得启用 webhook | 元数据库就是整个文件系统的真相源：丢了 = 数据不可达；**网关在元数据库不可用时直接 FATAL 退出、不重试**（**现场记录**：与 PG 同时重启即退出；`io-cold.sh` 因此先重启 PG 再启网关）→ 须配自动重启 + 启动顺序 |

### Tier 4 —— 性能（仿生产负载形态：按站点/日期分层的叶目录、每叶目录 200 个、2KB 对象）

**三轮、每轮全新环境，取中位数**（± 为三轮极差占中位数的百分比）。SSD 上的结果视为单机单盘的上限。

**热缓存，100 万对象**：

| 指标 | MinIO | SeaweedFS | JuiceFS+PG |
|---|---|---|---|
| 写入（obj/s，c=32） | 2,895 ±15% | **8,577 ±13%** | 325 ±2% |
| 写入 p99 | 31.7ms | **8.7ms** | 520ms |
| 全量递归列举（obj/s，含 LastModified） | 63,951 ±2% | 62,851 ±2% | 19,336 ±1% |
| filer gRPC 直扫（obj/s） | — | **415,565 ±2%** | — |
| 叶目录列举 p50 | 4.6ms | **3.7ms** | 16.6ms |
| StatObject（ops/s，c=16） | **17,409 ±9%** | 15,065 ±7% | 5,989 ±12% |
| 内存（三轮） | 471–475MB | 0.75–1.03GB | 223–259MB + PG |
| 数据盘 / 元数据 | 3.9GB / — | **2.5GB / 140MB** | 3.9GB / 850MB（PG） |

- 10 万、50 万、100 万三个规模点的列举速率持平：**在 SSD 加热缓存的条件下看不出差异**。
- 尾延迟（p99）三轮之间波动很大，例如 MinIO 叶目录 p99 的极差达 ±75%，只作参考，不据此下结论。
- JuiceFS+Redis 变体只测了 4KB × 1 万对象的写入（约 1,550 obj/s）和内存占用（每文件约 600B，常驻内存）——**现场记录**，原始输出未留存（compose 在 `juicefs-redis/`）。
  按此推算，1 亿文件约需 60GB 内存；Redis 用 AOF everysec 时，崩溃最多丢 1 秒的元数据。不推荐。

**冷缓存读盘次数**（先重启、再清页缓存，与硬件无关，决定机械盘上的表现），每 1000 个对象的中位数，方括号里是各轮数值：

以下均由 `python3 aggregate.py` 从 `results/bench/io-cold.jsonl` 算出。

| 冷启动后的操作 | MinIO | SeaweedFS | JuiceFS+PG |
|---|---|---|---|
| 全量列举 1M | **1,057** [1056, 1057, 1058] | **6.2** [6.6, 6.2, 5.6] | 27.9 [23.5, 27.9, 28.1] |
| 全量列举 1M 的读取总量（MB，中位数） | 4,910 | **197** | 1,120 |
| 叶目录列举 | 1,086 | 40 | 295 |
| filer 直扫 | — | 3.7 [3.8, 3.6, 3.7] | — |

**机械盘外推（模型估算，`python3 hdd_model.py`，输出存于 `results/hdd-model.md`）**：
minio-inventory 在单机单盘机械盘的 MinIO 上实测：约 170 万对象全量扫描耗时 5h31m，聚合约 86 obj/s。
用 MinIO 的冷缓存读盘次数把这段时间换算成该盘的
有效随机读速率（≈90.5 次/秒），再按各家的每对象读盘次数换算：

| 全量列举 | MinIO | SeaweedFS | JuiceFS+PG |
|---|---|---|---|
| 170 万（标定点） | 5.5 小时 | 1.9 分钟 | 8.7 分钟 |
| 1 亿 | 13.5 天 | 1.9 小时 | 8.6 小时 |
| 1 亿、封存分片每 30 天轮转复核 | 10.8 小时/天 | 0.06 小时/天 | 0.29 小时/天 |

⚠️ **MinIO 这一列是标定，不是验证**：速率本身就是用 5h31m 反推的，所以 MinIO 的 5.5 小时、13.5 天与
minio-inventory 的数字一致是必然的，不构成独立证据。模型的信息量全在**三家每对象读盘次数之比**（约 170 : 1 : 4.5）。
另外它把大块连续读也按随机读计价，对 SeaweedFS / JuiceFS 偏保守；SeaweedFS 的元数据很小（1 亿对象约 14GB），大部分能驻留内存。

**测试过程说明**：
- 第一轮 SeaweedFS 被 webhook 内存问题污染（见 Tier 3），已用不带 webhook 的配置重跑；旧数据保留作对照。
- 后台任务在第 3 轮中途撞上 2 小时上限被强停，JuiceFS 第 3 轮因此整轮重跑。
- JuiceFS 第 3 轮的冷缓存测量补测过一次，原因见 Tier 3「网关启动依赖」。

### Tier 5 —— Python 数据生态兼容（产品判据：ETL 不绕过 CP，但必须容易接入）

产品决定（2026-10-03）：维持「写入方不得绕过 CP」，**前提是 ETL 和 Python 生态容易接入**。
加工产物为 nc / csv / parquet，需能被 xarray / pyarrow / pandas / DuckDB 方便读写。

**测法**：uv 虚拟环境，Python 3.12 + xarray 2026.9 / h5netcdf 1.8 / pandas 3.0 / pyarrow 25 / duckdb 1.5 / s3fs 2026.9 /
boto3 1.43（均为当前版本、默认配置）。造数据：`raw/` 下 3 个 NetCDF（10.9MB/个）+ 100 万行观测 CSV。
**所有读写都用 CP 式的前缀受限 STS 凭据**：session policy 只给 `raw/` 只读、`runs/r1/` 读写，与真实的 ETL run 一致。

| # | 项目 | MinIO | SeaweedFS |
|---|---|---|---|
| A1–A6 | 前缀受限 STS：区内读写可用；越界写、写只读区、列桶根目录均被拒 | ✅ | ✅ |
| A7 | boto3 上传**不可回绕的 40MB 流**（aws-chunked + 尾部校验和，新版 SDK 默认行为） | ✅ | ✅（minio-go 探针里的 BadDigest **未在 Python 路径上复现**） |
| X1 / X1b | xarray 懒加载 NetCDF 切片（默认块；强制 1MB 块，共 18 次 Range GET），值与真值一致 | ✅ | ✅ |
| X2 / X3 | `open_dataset('s3://…')`；`open_mfdataset` 通配多文件 + 区域均值 | ✅ | ✅ |
| X5 / X6 | 写 NetCDF：本地临时文件 + 上传（NetCDF4）；内存字节 + 上传（NetCDF3） | ✅ | ✅ |
| P1–P3 | pandas 读写 CSV / Parquet（经 s3fs），与原表逐值相等 | ✅ | ✅ |
| P4–P6 | pyarrow 原生 S3FileSystem：600 万行（47.8MB，分片上传）；按天 hive 分区写 30 个文件 + 过滤扫描；读 CSV | ✅ | ✅ |
| D1–D6 | DuckDB httpfs：SQL 聚合 CSV、分区 Parquet 通配 + 分区裁剪、`COPY … TO` Parquet / CSV / `PARTITION_BY`；越界写被拒 | ✅ | ✅ |
| K1–K4 | **`credential_process` 桥接**：s3fs、xarray、pyarrow、DuckDB 四家只给 profile、代码里不写密钥，全部可用 | ✅ | ✅ |
| 合计 | | **31/31** | **31/31** |

**ETL 接入要点（与存储无关，SDK 和文档要写明）**：

- **NetCDF 不能直接写进 S3 流**：HDF5 写入需要随机定位，所以用「先写本地，再上传」（X4 实测必然失败，X5 为正确路径）。SDK 应代劳这一步。
- **pyarrow `write_dataset` 默认会对整个桶做 HeadBucket**，这需要桶级的 `s3:ListBucket`，前缀受限凭据会被拒。
  在 AWS 上也是如此。要传 `create_dir=False`（P5a 固定了这条约束）。
- **DuckDB 读 `credential_process` 要用 `CHAIN 'process'`**（不是 `config`）。
- **按 run 划前缀的 policy 与 D-030 第八条（「policy 写整桶」）冲突**：ETL 接入需要前缀级的 session policy。
  本轮实测两家都能正确执行前缀条件（含 `s3:prefix` 条件的 ListBucket），但子决策需要改写。
- **桥接很薄**：一个返回 STS JSON 的命令（`credential_process`）+ 一个 profile，四个库全部通吃；数据路径保持原生 `s3://`。

**未覆盖**：大文件（>50MB）NetCDF 的 Range 读性能；Zarr（大量小对象）；写入时带 `x-amz-meta-*` 能否进入事件；Windows 客户端。

## 2. 对 FileAgent 的改造面（对照代码现实，非设计文档）

现实基线：入站事件 = MinIO webhook → `IndexUpload`（IC-4a 已加固）；D-031（JetStream）、L2 对账（IC-13）、注册端点（IC-9）均**未实现**；agent `UploadResult` 是主写入者。

| 改造项 | SeaweedFS | JuiceFS |
|---|---|---|
| 事件入库 | **重写**：CP 订阅 filer 元数据日志（gRPC），游标 = ts_ns + 回退窗口 + 按身份去重 | 零改动 |
| `init-minio.sh` | 换成声明式配置文件 | 保留 `mc`，绕开 2–3 个不兼容的管理接口 |
| Lifecycle / 分片清理 | worker 或 CP 定时任务 | CP 自理（IC-3 已有孤儿分片清理） |
| 路径模板 | 无约束 | **须保证不产生 `a` 与 `a/b` 共存**，并把 500 定为不可重试 |
| 部署 | `stop_grace_period ≥ 60s`；filer gRPC 端口内网 + mTLS；元数据日志按天清理 + 游标越界自检 | 另起并运维一个元数据库（PG 实例与业务库分开） |

## 3. SeaweedFS 元数据订阅（替代入站事件链路）的实测

除标注「现场记录」的项外，数字均由 `python3 subscribe_summary.py` 从 `results/subscribe/` 的事件日志与写入日志
离线算出（输出存于 `results/subscribe/summary.md`）。

| 实验 | 结果 |
|---|---|
| e1 订阅方 `kill -9` 后续订 | ✅ 400/400，无重复 |
| e2 / e2b filer 优雅重启 / `kill -9`（订阅方在线） | ✅ 600/600 ×2，无重复 |
| e3b 并发 put/delete 因果：重放终态 = 真实状态 | ✅ 3,543 条事件重放出 33 个对象；与存储真值逐对象一致为**现场记录**（真值列表未留存） |
| e4 1 万突发 | ✅ 10,000/10,000；写入确认→收到 p99 155.6ms |
| e7 并发写入：回退窗口 + 按身份去重 | ✅ 100/100（严格「≤ 游标即丢」时丢 2 条，为现场记录） |
| e2c 订阅方离线 + filer 硬崩溃 | ❌ 0/200：最近 ≤60s 事件丢失（`LogFlushInterval` 为常量） |
| e6 `docker compose restart`（10s 宽限） | ❌ 0/100：缓冲事件全丢；放宽到 120s 后可正常刷盘（e7） |

**日志清理（实测）**：`fs.log.purge -daysAgo N` 以**整天目录**为粒度、按目录 mtime 删除，`-daysAgo 0` 连当天一起删光；
**游标落在已清理范围时订阅静默跳过，不报错**（清理前写入的 100 条在续订中为 0，清理后的 100 条完整；`results/subscribe/purge/`）。
→ CP 只能清理「已消费完的整天」，并必须自检「游标早于最早可用日志」，否则静默丢事件。

三条硬约束：① ts_ns 并发下约 0.1ms 乱序 → 不能用严格游标；② 硬崩溃 ≤60s 窗口，且**当前无 L2 兜底**——agent 写入有 `UploadResult` 覆盖，**SDK/ETL 写入会永久丢索引**；③ filer gRPC 只有 mTLS、全权限、日志默认永久保留，清理粒度为天且越界静默。
加分项：日志永久保留意味着「订阅方长时间停机、超过事件保留期」这类事故可以从游标完整回放。

## 4. 建议

**结论：以 SeaweedFS CE 为替换目标；JuiceFS 不推荐；MinIO 只作过渡（沿用 D-036 自持镜像）。**

**理由**

1. **供给与合规**：MinIO 是死路（AGPL 分发义务在我方，其充分性以 D-036 为准、待法务确认；且无补丁来源）。两个候选都是 Apache-2.0、公开可得，都能解决。
2. **产品形态放大了列举成本的分量**：FileAgent 很可能部署为单机单盘 all-in-one，规模数亿对象，以 KB 级小文件为主。
   这正是 MinIO 的结构性死穴：每列举一个对象就要读一次盘，按模型外推 1 亿对象全扫约 13.5 天。
   **但列举不在用户读路径上**——现有用户读路径只走 CP 返回的精确文件清单，不列举存储（`contracts.md` V-5）；将来在 CP 授权前缀内如何确定成员，是 V-5 的待定项。
   列举成本决定的是**对账与灾难恢复的代价**：按 minio-inventory 的封存分片做法每 30 天轮转复核一遍，
   1 亿对象在 MinIO 上约占机械盘每天 10.8 小时，SeaweedFS 只要几分钟；从存储全量重建索引分别约 13.5 天和 1.9 小时（均为模型估算）。
   minio-inventory 和 FileAgent 的 L2 那一整套「分片 + 封存 + 轮转预算」都是为了绕开它。
   SeaweedFS 的冷缓存读盘次数约是它的 1/170，写入最快，数据占用最小（小文件打包进大卷，正是本项目这类小文件负载）。
   所以离开 MinIO 的**首要理由仍是第 1 条**（断供 + AGPL）；列举成本是选 SeaweedFS 的重要加分项，而非唯一决定因素。
3. **JuiceFS 出局**：
   - 它的强项（事件链路零改动、STS 原生）抵不过三点：POSIX key 冲突，而且报 500；
     搭 PG 时写入只有约 325 个/秒；还要额外运维一套数亿行的元数据库。
   - 搭 Redis 换来的吞吐，代价是约 60GB 常驻内存，外加元数据丢失窗口。

**SeaweedFS 的前置条件（按顺序，缺一不可）**

1. **入站事件改为订阅 filer 元数据日志**：
   - 游标用「时间戳 + 回退窗口 + 按身份去重」；
   - 过滤掉 `.uploads/` 和目录事件；
   - 放在一层「存储事件源」接口后面。
   这一步取代现有的 webhook 入库，也取代 D-031 的入站部分。
2. **补上 60 秒崩溃窗口**：agent 写入已经由 `UploadResult` 兜住。SDK/ETL 等非 agent 写入方上线之前，
   必须先有注册端点（IC-9）或 L2 对账（IC-13）。在 SeaweedFS 上，filer 直接扫描每秒约 40 万对象，
   L2 很可能可以比 minio-inventory 那一套简单得多。
3. **部署加固**：
   - `stop_grace_period` 不少于 60 秒；
   - **禁止启用 filer webhook**；
   - filer gRPC 只开在内网，并配 mTLS；
   - 定期执行 `fs.log.purge`，只清理已消费完的整天，CP 启动时自检「游标是否早于最早可用日志」，越界就报警并触发对账；
   - 生命周期由 worker 执行，或者由 CP 定时清理。
4. **验收**：重写初始化脚本，让 `deploy/scripts/smoke.sh` 在 SeaweedFS 上十二环全绿。
   在这之前，本报告只证明它「值得换」，还没证明「能换」。
5. **决策记录**：新立一条决策，取代 D-030 第一条和 D-031 的入站部分；D-036 改定为过渡方案。

**设计原则：不做通用存储抽象，只抽象事件源**

- **数据面直接复用 S3。** 三家用同一份客户端代码（minio-go v7.0.91，选项照搬 agent）跑同一套探针，
  STS、分片续传、预签名、列举全部通过。现有的窄接口（CP 的 `STSManagerClient`、`MinIOPresigner`、
  `MinioBucketMaker`，agent 的 `ObjectStore`）已经覆盖了这部分，换后端不需要新抽象。
- **运行时代码里唯一要加的接口是「存储事件源」**：把 S3 Records webhook 和 filer 元数据订阅
  统一成「对象新建 / 删除」事件。账号、policy、事件订阅、生命周期这些差异，放在部署脚本里解决，不进 CP。
- **任何时候只正式支持一家后端。** 接口是为了可测试、将来能迁移，不是为了同时支持多家；
  后者真正的成本是每家各一套测试矩阵、部署文档和排障经验。MinIO 的适配器只在过渡期保留。
- **抽象只能统一接口，统一不了语义保证**（事件持久性、key 语义、生命周期）。CP 应按最弱的保证设计：
  事件只是低延迟提示，对账才是事实来源（D-030 第二条）。做到这一点，换后端只影响性能，不影响正确性。
- **JuiceFS「可接多种存储」帮不上忙**：它的可插拔在最底层，瓶颈却在它自己加的元数据层（每次写入都是多次元数据库事务）。
  把它架在 SeaweedFS 或 MinIO 上，等于叠两套元数据系统。它的强项是 POSIX 挂载，我们没有这个需求；
  真有的话，SeaweedFS 自带 FUSE 挂载。

**ETL / Python 生态接入（产品前提，实测已验证可行，与选型无关）**：MinIO 与 SeaweedFS 均 31/31（Tier 5）。
落地需要：① CP 按 run 发放**前缀级** STS 凭据——须改写 D-030 第八条「policy 写整桶」；
② 一个 `credential_process` 命令 + SDK 的 run 封装（申请 grant → 原生 `s3://` 读写 → 注册 → 结算；注册时以哪种方式确定文件集合——SDK 记录写入清单还是列举 run 前缀——属于 `contracts.md` V-5 的待定项，等真实 ETL 接入时定）；
③ 文档写明 NetCDF「先本地后上传」、pyarrow `create_dir=False`、DuckDB `CHAIN 'process'`。

**时机**：换存储不挡当前 MVP，自持 MinIO 目前可用。但换存储最便宜的时间点是第一批生产数据落地之前，
那时还没有存量要迁移。建议**现在拍板方向**，先做一个 spike（前置条件 1 的原型加第 4 条的 smoke 全绿），
再决定排期。

**附：与选型无关、但本轮暴露的 FileAgent 缺口**：三家（包括 MinIO）对过期 STS 凭据都返回 `InvalidAccessKeyId`，
而 agent 的兜底刷新只认 `AccessDenied`（`agent/cmd/agent/main.go:617`）。主路径是按时间提前 10 分钟刷新
（`credential.go:233`），正常情况下不受影响；但机器休眠或 CP 长时间不可达之后，凭据会真正过期，这条兜底路径不会触发。
另外，minio-go 会把有效期请求小于 3600 秒的值钳成 3600 秒，CP 想缩短凭据有效期时，需要绕开 minio-go 自己签名。

## 5. 本机未能验证

- HDD 上的绝对性能（只有冷缓存读盘次数 + 模型外推）
- 多节点、副本、纠删码、磁盘故障
- SeaweedFS 生命周期规则的实际执行（需 admin+worker 长时间运行）
- filer gRPC 的 mTLS 配置
- CP 侧数亿行容量（另立「容量基线」，未拍板）

## 6. 其他候选（文献筛查，未实测）

本轮只实测了产品点名的 SeaweedFS 与 JuiceFS。其余候选按两道筛子筛查：STS `AssumeRole` + session policy，
以及**元数据是否独立于数据存储**（决定海量小文件下的列举成本，见 Tier 4）。**均未跑探针。**

| 候选 | 现状（2026-10） | STS | 事件 | 判断 |
|---|---|---|---|---|
| **`pgsty/silo`**（原 `pgsty/minio`，MinIO 社区分支） | AGPL-3.0；每月发版（最新 RELEASE.2026-09-16）；Docker Hub 公开 | 同 MinIO | 同 MinIO | **不考察**：只解决「拿不到补丁」；过渡期 D-036 自持镜像已够用，且它与 MinIO 共享同一结构性缺陷（见下行） |
| **RustFS** | 1.0 GA（2026-09-16）；Apache-2.0；活跃 | 宣称支持，未证实 | 未查 | **排除**：其架构文档写明每个对象在纠删组每块盘的独立子目录里各存一份 `xl.meta`（另有可选的 MinIO 磁盘格式兼容层），与 MinIO 同为「文件系统承载命名空间」——列举同样逐对象读盘，海量小文件场景下继承同一缺陷 |
| **Ceph RGW** | 成熟；Apache/LGPL | ✅ 完整（AssumeRole、session policy、STS Lite） | ✅ 支持 HTTP/Kafka/AMQP，有持久化 topic | 功能最全，桶索引独立存储、列举不必逐对象读盘；但 mon/mgr/osd 一套组件，**与「单机 all-in-one」定位冲突**，运维体量最大 |
| **Garage** | 活跃；AGPL-3.0 | ❌ 无 | ❌ 无桶通知 | 第一道筛子即出局（且无 bucket policy） |
| Zenko CloudServer、versitygw 等 | — | 未查 | 未查 | 未评估 |

**旁证：AWS 自己也不让用户靠列举。** S3 Inventory（定期全量清单）之后，AWS 又推出 S3 Metadata：
自动维护「日志表（近实时变更）+ 实时清单表（全量当前状态，约一小时内刷新）」两张 Iceberg 表。
这与 FileAgent 的「事件流 / 元数据订阅 + CP 镜像 + 对账」结构一一对应，且连 AWS 也只承诺全量清单小时级刷新。

来源：[pgsty/silo releases](https://github.com/pgsty/silo/releases)、[RustFS](https://github.com/rustfs/rustfs)、
[Ceph RGW STS](https://docs.ceph.com/en/quincy/radosgw/STS/)、[Garage S3 兼容性](https://garagehq.deuxfleurs.fr/documentation/reference-manual/s3-compatibility/)、
[RustFS：MinIO 文件格式兼容](https://github.com/rustfs/rustfs/blob/main/docs/architecture/minio-file-format-compat.md)、
[Amazon S3 Metadata](https://aws.amazon.com/s3/features/metadata/)、[S3 Metadata 支持全部对象（AWS News Blog）](https://aws.amazon.com/blogs/aws/amazon-s3-metadata-now-supports-metadata-for-all-your-s3-objects/)。
