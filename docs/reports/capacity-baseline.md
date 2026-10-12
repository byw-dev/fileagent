# File 索引容量基线（只测量，不作决策）

> 2026-10-12 · PostgreSQL 15 · 合成数据 · 结果与复现工具：[`research/capacity-baseline/`](../../research/capacity-baseline/)
> 目的：为 B5 的标签存储、IC-6 分区、IC-7 计数契约与 UUIDv7 讨论提供硬件无关的字节、WAL 和缓冲页数据。本报告不作方案决策。

## 0. 结论先行

- 在本次 1M / 5M 数据点上，EAV（文件行 + 8 个 `file_tags` 行）总关系尺寸约为 **1,290 / 1,276 B/文件**；`int4[]` + GIN 约为 **283 / 260 B/文件**。两点线性拟合到 1 亿、5 亿行分别约为 **127 / 636 GB** 与 **25 / 127 GB**。这些是关系尺寸外推，不含数据库级额外对象和备份空间。
- 受默认 `full_page_writes=on`、`max_wal_size=1GB` 影响，5M 批量写期间观测到 71 个 requested checkpoints 与约 3.87M 个 full-page images；EAV 与数组每文件 WAL 分别约 **6,640 B**、**1,120 B**。WAL 数值明显依赖 checkpoint/FPI，不能当成固定写放大系数外推。
- 对 5 种过滤，两个存储模型的完整匹配集、计数和最新 50 个 ID 均通过 SQL 断言。查询缓冲数显示，EAV 的高命中与组合条件成本随命中集显著增长；数组 GIN 在本数据分布上的缓冲数更低。
- 时间分区父表查询与按最新分区逐个扫描的缓冲数不同；本机结果中，分区逐块扫描凑满 50 行后停止，常见路径只访问 1–2 个分区。这里测量的是 `created_at` 代理时间与固定 36 个月分区，不代表产品数据时间列或分区粒度已定。
- UUIDv4 与 UUIDv7 在**随机到达顺序**下的索引尺寸、WAL 和页碎片接近。此实验没有测量 UUIDv7 按时间递增到达的最佳情形。

## 1. 被测数据与环境

生成器、SQL 与一条命令复现见 [`README.md`](../../research/capacity-baseline/README.md)、[`run.sh`](../../research/capacity-baseline/run.sh)、[`benchmark.sql`](../../research/capacity-baseline/benchmark.sql) 和 [`index-set.sql`](../../research/capacity-baseline/index-set.sql)。固定 seed 为 `20261012`；每个 scale 使用相同规则重新生成数据。1M 与 5M 主结果分别为 [`scale-1000000.log`](../../research/capacity-baseline/results/scale-1000000.log) 与 [`scale-5000000.log`](../../research/capacity-baseline/results/scale-5000000.log)；UUID 列类型的索引组原始输出为 [`index-set-1000000.log`](../../research/capacity-baseline/results/index-set-1000000.log)、[`index-set-5000000.log`](../../research/capacity-baseline/results/index-set-5000000.log)。

每文件 8 个标签、8 个 key：site（1,000 值，均匀）、product（20 值，P01 占 40%，其余 19 值分摊余量）、level（4）、qc（3）、vendor（10）、model（50）、region（20）、instrument（100）。因此 high 为 `product=P01`（40%），low 为 `site=S0001`（0.1%）；另测 AND、OR 与 AND-NOT。时间戳确定性地覆盖 2023-01 至 2025-12，路径形状为 `agent/yyyy/mm/dd/file`。两模型共享文件 ID、路径、时间与标签映射。

容器使用 `postgres:15-alpine`，版本输出为 PostgreSQL 15.19。设置均为镜像默认值：`shared_buffers=128MB`、`work_mem=4MB`、`maintenance_work_mem=64MB`、`effective_cache_size=4GB`、`full_page_writes=on`、`wal_compression=off`、`checkpoint_timeout=5min`、`max_wal_size=1GB`。每组 bulk 前执行 `CHECKPOINT`；结果同时记录 checkpoint 计数、`pg_stat_wal.wal_fpi` 与 LSN 差。5M bulk 段 71 次 requested checkpoint 包含显式预检查点，另有约 70 次由 WAL 阈值触发；timed checkpoint 为 0。`EXPLAIN` 中的 execution time 只属于本机，仅供参考；正文只采用字节、WAL 与 shared hit + read blocks。

## 2. 标签存储尺寸

下表为 `pg_relation_size` 的 heap 与各索引字节数除以名义文件数；EAV 总计包含 `fe_eav` 与 `ft_eav`，数组总计包含 `fe_arr`。路径唯一索引和文件 ID 主键分别列出。原始字节可在对应 raw log 的 `relation_sizes`、`index_sizes` 行查到。

| Scale | 模型 | Heap B/文件 | `fe` PK | `file_tags` PK | UNIQUE(path) | KV | GIN | 总 B/文件 | 原始结果 |
|---:|---|---:|---:|---:|---:|---:|---:|---:|---|
| 1M | EAV：`fe_eav` + `ft_eav` | 691.28 | 42.14 | 434.50 | 68.20 | 54.03 | — | **1,290.39** | [1M log](../../research/capacity-baseline/results/scale-1000000.log) |
| 1M | `int4[]` + GIN | 141.25 | 42.14 | — | 68.20 | — | 30.87 | **282.55** | [1M log](../../research/capacity-baseline/results/scale-1000000.log) |
| 5M | EAV：`fe_eav` + `ft_eav` | 691.27 | 40.46 | 435.35 | 56.82 | 52.17 | — | **1,276.25** | [5M log](../../research/capacity-baseline/results/scale-5000000.log) |
| 5M | `int4[]` + GIN | 141.24 | 40.46 | — | 56.82 | — | 21.44 | **260.00** | [5M log](../../research/capacity-baseline/results/scale-5000000.log) |

说明：EAV 的 Heap 列是两张表 heap 相加；两张表的主键索引分开列出。`int4[]` 的 heap 包含数组值本身。每个命名 index 的原始总字节均按上列来源文件中的索引名记录。总量采用 `pg_total_relation_size`，包含 TOAST 关系；所以它可能比表中 heap 与命名索引的可见小计多几个页。两组每文件尺寸差异反映 B-tree/GIN 页填充与索引规模变化，不能把 1M 点当成唯一单行常数。

### Bulk 与单行 WAL

| Scale | EAV 文件行 WAL B/文件 | EAV 8 个标签行 WAL B/文件 | EAV 合计 B/文件 | `int4[]` 行 WAL B/文件 | Bulk FPI / requested checkpoint | 原始结果 |
|---:|---:|---:|---:|---:|---|---|
| 1M | 324.09 | 3,151.01 | **3,475.10** | **758.45** | 162,820 / 8 | [1M log](../../research/capacity-baseline/results/scale-1000000.log) |
| 5M | 520.31 | 6,119.24 | **6,639.55** | **1,119.62** | 3,868,838 / 71 | [5M log](../../research/capacity-baseline/results/scale-5000000.log) |

bulk WAL 按各 `INSERT` 前后 `pg_current_wal_lsn()` 差计算；EAV 标签行的每文件数为 8 × 每标签 WAL。默认 checkpoint/WAL 阈值使较大 scale 有更多 FPI，故跨 scale 的 WAL/文件上升不是单纯行宽差。

在数据已加载后，每个模型各执行 100 次单文件 INSERT（EAV 每文件再插 8 条标签）：

| Scale | EAV 单文件 WAL B/文件 | 数组单文件 WAL B/文件 | 原始结果 |
|---:|---:|---:|---|
| 1M | 15,154.96 | 6,598.24 | [1M log](../../research/capacity-baseline/results/scale-1000000.log) |
| 5M | 15,609.68 | 6,942.00 | [5M log](../../research/capacity-baseline/results/scale-5000000.log) |

## 3. 查询结果等价性与缓冲页

EAV 使用当前 `tagFilterClause` 形状：`id IN (SELECT file_entry_id FROM file_tags WHERE ... GROUP BY file_entry_id HAVING COUNT(*) = N)`；OR 用 `HAVING COUNT(*) >= 1`，AND-NOT 在正向分组后加 `NOT EXISTS`。数组使用 `@>`、`&&` 与负向谓词。SQL 对完整 ID 集做双向 `EXCEPT`，对最新页也做双向 `EXCEPT`，并比较 COUNT；任一不等会 `RAISE EXCEPTION`。

| Predicate | 命中行数 1M / 5M | EAV 最新 50 缓冲 blocks 1M / 5M | 数组最新 50 blocks 1M / 5M | EAV COUNT blocks 1M / 5M | 数组 COUNT blocks 1M / 5M | 等价断言 |
|---|---:|---:|---:|---:|---:|---|
| high：product=P01 | 400,000 / 2,000,000 | 1,674,392 / 8,371,961 | 17,372 / 86,337 | 1,274,393 / 7,299,669 | 17,242 / 86,207 | 两 scale 均通过 |
| low：site=S0001 | 1,000 / 5,000 | 5,003 / 25,006 | 1,005 / 5,008 | 4,004 / 22,347 | 1,005 / 5,008 | 两 scale 均通过 |
| AND：product=P01 且 level=L1 | 100,000 / 500,000 | 474,591 / 2,372,947 | 17,425 / 86,975 | 374,592 / 2,104,911 | 17,393 / 86,943 | 两 scale 均通过 |
| OR：product=P01 或 qc=Q1 | 600,000 / 3,000,000 | 234,309 / 424,815 | 17,372 / 86,337 | 234,277 / 424,815 | 17,242 / 86,207 | 两 scale 均通过 |
| AND-NOT：product=P01 且非 qc=Q1 | 266,667 / 1,333,334 | 3,274,392 / 18,371,961 | 17,339 / 86,337 | 2,874,393 / 17,299,669 | 17,307 / 86,207 | 两 scale 均通过 |

blocks = `EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON)` 最外层节点的 shared hit + shared read；不把父/子节点重复累计。最新页固定 `ORDER BY created_at DESC, id DESC LIMIT 50`。COUNT 为精确 `COUNT(*)`。每个 scale 的日志含 5 条 `identical=true` 断言和各查询计划 JSON。

## 4. 时间分区查询

按 `created_at` 建立 36 个按月 range 分区；无时间范围与固定 `2025-07` 至 `2025-12` 范围都测。对照是同一生成数据的未分区 `fe_arr`。Chunked 路径按最新分区降序逐个 `EXPLAIN ANALYZE`，到累计 50 行即停止。表格是 shared hit + read blocks；chunked 同时列出停止分区数。

| Scale | Predicate | 时间范围 | 未分区 | 分区父表单查询 | Chunked | 分区数 |
|---:|---|---|---:|---:|---:|---:|
| 1M | high | 无 | 17,372 | 13,479 | 388 | 1 |
| 1M | low | 无 | 1,005 | 2,891 | 94 | 2 |
| 1M | high | 2025-07..12 | 17,339 | 2,344 | 388 | 1 |
| 1M | low | 2025-07..12 | 1,005 | 352 | 94 | 2 |
| 5M | high | 无 | 86,337 | 66,817 | 1,890 | 1 |
| 5M | low | 无 | 5,008 | 11,668 | 251 | 1 |
| 5M | high | 2025-07..12 | 86,337 | 11,262 | 1,890 | 1 |
| 5M | low | 2025-07..12 | 5,008 | 1,797 | 251 | 1 |

高命中谓词在最新分区即取得一页。低命中谓词在 1M 需要 2 个分区，在 5M 最新分区已有至少 50 个命中。chunked 与父表都按同一顺序和过滤条件执行；每个计划的原始 buffers 位于对应 scale 日志末段，chunked 汇总在 `chunked_scan` NOTICE 行。

## 5. UUID 主键与 `file_entries` 索引集合

UUIDv4/v7 都在 `CHECKPOINT` 后按相同确定性伪随机顺序插入；索引碎片使用 `pgstattuple` 的 `leaf_fragmentation`，不是对文件系统碎片的测量。

| Scale | 主键 | PK 索引 bytes | Bulk WAL B/row | FPI | 平均叶密度 | 叶碎片 % | 原始结果 |
|---:|---|---:|---:|---:|---:|---:|---|
| 1M | UUIDv4 | 39,567,360 | 170.069 | 350 | 71.88% | 50.01 | [1M log](../../research/capacity-baseline/results/scale-1000000.log) |
| 1M | UUIDv7 | 39,264,256 | 169.935 | 0 | 72.41% | 49.99 | [1M log](../../research/capacity-baseline/results/scale-1000000.log) |
| 5M | UUIDv4 | 203,366,400 | 188.704 | 16,654 | 69.90% | 49.76 | [5M log](../../research/capacity-baseline/results/scale-5000000.log) |
| 5M | UUIDv7 | 205,750,272 | 189.165 | 16,266 | 69.10% | 49.79 | [5M log](../../research/capacity-baseline/results/scale-5000000.log) |

当前 7 索引形状为 PK、UNIQUE(bucket,path)、org/type、agent、重复 bucket、status partial、path trgm；缩减组只去掉重复 bucket 与 path trgm。

| Scale | 形状 | 索引 bytes | WAL B/row | FPI | 原始结果 |
|---:|---|---:|---:|---:|---|
| 1M | 当前 7 索引 | 400,039,936 | 2,046.30 | 89,994 | [UUID index log](../../research/capacity-baseline/results/index-set-1000000.log) |
| 1M | 去掉两项 | 262,488,064 | 810.73 | 27,482 | [UUID index log](../../research/capacity-baseline/results/index-set-1000000.log) |
| 5M | 当前 7 索引 | 1,918,468,096 | 16,283.86 | 12,725,695 | [UUID index log](../../research/capacity-baseline/results/index-set-5000000.log) |
| 5M | 去掉两项 | 1,236,303,872 | 7,637.51 | 6,063,278 | [UUID index log](../../research/capacity-baseline/results/index-set-5000000.log) |

各索引原始尺寸（bytes；缩减组没有的两项记为 —）：

| Index | 1M 当前 | 1M 缩减 | 5M 当前 | 5M 缩减 |
|---|---:|---:|---:|---:|
| PK(id) | 42,139,648 | 42,139,648 | 202,293,248 | 202,285,056 |
| UNIQUE(bucket_id, storage_path) | 85,729,280 | 85,729,280 | 440,745,984 | 440,745,984 |
| org/type/uploaded_at | 80,044,032 | 80,044,032 | 363,708,416 | 363,700,224 |
| agent/uploaded_at | 54,231,040 | 54,231,040 | 227,950,592 | 227,950,592 |
| duplicate bucket index | 85,729,280 | — | 440,745,984 | — |
| partial status | 344,064 | 344,064 | 1,622,016 | 1,622,016 |
| path trgm GIN | 51,798,016 | — | 241,369,088 | — |

UUID 类型索引组的 heap 另为 160,022,528 B（1M）与 814,735,360 B（5M）。`index-set.sql` 的 bucket/org/type/agent 列类型与迁移一致，索引名尺寸见两份 `index-set` raw logs；`benchmark.sql` 另留有 int4 surrogate 的快速 profile，但它不进入本节数字。5M WAL 中大量 FPI 与 requested checkpoints 同时出现；这些 WAL 值只适用于记录的默认 checkpoint 条件。

## 6. 规模外推（模型，不是测量）

只用 1M 与 5M 两个尺寸点拟合 `bytes = 截距 + 每文件斜率 × 文件数`，并假设每文件 8 个标签、字段宽度/索引集合不变。十进制 GB。结果是 §2 全部关系的估算：

| 结构 | 拟合 B/文件 | 1 亿文件 | 5 亿文件 |
|---|---:|---:|---:|
| EAV：`fe_eav` + `ft_eav` | 1,272.72 | 127.29 GB | 636.38 GB |
| `int4[]` + GIN | 254.37 | 25.46 GB | 127.21 GB |

对索引集合与 UUID 主键索引采用相同线性拟合，作为尺寸量级：

| 索引 | 1 亿文件 | 5 亿文件 |
|---|---:|---:|
| 当前 7 索引集合 | 37.98 GB | 189.82 GB |
| 去掉重复 bucket 与 trgm | 24.36 GB | 121.75 GB |
| UUIDv4 PK B-tree | 4.09 GB | 20.47 GB |
| UUIDv7 PK B-tree | 4.16 GB | 20.81 GB |

不对 WAL 或 buffers 作规模外推：本次 1M→5M 已显示 checkpoint/FPI 数量、缓存状态与查询计划会显著改变这些数值，线性套用会把本机默认配置误当成固定规律。

## 7. 限制 / 本测量不能说明什么

- 这不是完整生产 `file_entries` 表：标签实验保留了主键、路径唯一索引及被测标签结构；没有复制所有业务列、FK、TOAST、审计/词表表、并发事务或在线保留策略。索引集合实验单独按迁移里的 7 索引列建表。
- 当前 schema 没有设计稿中的数据时间列，分区实验用 `created_at` 代理；没有比较周/月粒度、分区维护成本或分布漂移。
- UUIDv7 与 UUIDv4 都随机到达，未测试按时间升序写入 UUIDv7 的右侧热页行为。索引 `leaf_fragmentation` 是该随机插入次序的结果。
- 只测 1M 和 5M 两点；1 亿 / 5 亿是明确假设下的线性尺寸外推，不是 PostgreSQL 实测。WAL 与 buffers 没有外推。
- 数据值分布由确定性生成器控制；除 product 热值外，多数 key 均匀分布，不等于真实租户的长尾与相关性。查询计划不代表所有参数选择性或并发负载。
- 所有墙钟时间仅属于本机实验环境；正文不以延迟作主要结论。以上结果提供 B5 的量化输入，不决定标签、分区、计数契约或主键方案。
