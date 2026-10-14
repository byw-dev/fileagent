# File 索引容量基线（只测量，不作决策）

> 2026-10-13 · PostgreSQL 15 · 合成数据 · 结果与复现工具：[`research/capacity-baseline/`](../../research/capacity-baseline/)
> 目的：为 B5 的标签存储、IC-6 分区、IC-7 计数契约与 UUIDv7 讨论提供字节、WAL 与缓冲页的测量数据（测量平台见 §7，不代表生产）。本报告不作方案决策。

## 0. 结论先行

- 在本次 1M / 5M 数据点上，EAV（文件行 + 8 个 `file_tags` 行）总关系尺寸约为 **1,290 / 1,276 B/文件**；`int4[]` + GIN 约为 **283 / 260 B/文件**。两点线性拟合到 1 亿、5 亿行分别约为 **127 / 636 GB** 与 **25 / 127 GB**。这些是关系尺寸外推，不含数据库级额外对象和备份空间。
- 受默认 `full_page_writes=on`、`max_wal_size=1GB` 影响，5M 批量写期间观测到 71 个 requested checkpoints 与约 3.87M 个 full-page images。EAV 与数组的批量写入**在同一段里依次执行、中间没有各自的检查点**（顺序：EAV 文件行 → 数组 → EAV 标签行），整页写入无法分摊到哪个模型，所以 6,641 vs 1,120 B/文件**只是一次顺序运行的描述性观测，不能当作两个模型的对比证据**。WAL 数值明显依赖 checkpoint/FPI，也不能当成固定写放大系数外推。
- 对 5 种过滤，两个存储模型的完整匹配集、计数和最新 50 个 ID 均通过 SQL 断言。查询缓冲数显示，EAV 的高命中与组合条件成本随命中集显著增长；数组 GIN 在本数据分布上的缓冲数更低。
- 公平加入 `(created_at DESC,id DESC)` 后，高命中最新 50 行在 1M / 5M 只访问 **128 / 125 blocks**；5M 低命中、无时间范围时有序扫描需走 **50,657 blocks**，加时间范围后优化器改选 GIN bitmap + sort（**5,008 blocks**）。分区逐块扫描常见路径访问 1–2 个分区。
- 单 PK 表中，5M 时间严格递增 UUIDv7 的 PK 为 **157.7 MB、90.04% 叶密度、0% 叶碎片**，随机到达 v7 为 **205.4 MB、69.18%、49.66%**；bulk WAL 为 **155.72 vs 189.86 B/row**。当前 7 索引形状中，5M 有序与随机 v7 的总 bulk WAL 为 12,693.81 vs 12,708.25 B/row，**但这组对比无法下结论**：同样是随机写入的 v4 与随机 v7 之间就差 27.7%（FPI 差 31%），噪声远大于要测的效应；且打乱到达顺序时其余 6 个二级索引的写入顺序也随之改变（如 agent 索引 228 MB vs 265 MB）。见 §5。

## 1. 被测数据与环境

生成器、SQL 与一条命令复现见 [`README.md`](../../research/capacity-baseline/README.md)、[`run.sh`](../../research/capacity-baseline/run.sh)、[`benchmark.sql`](../../research/capacity-baseline/benchmark.sql) 和 [`index-set.sql`](../../research/capacity-baseline/index-set.sql)。固定 seed 为 `20261012`；每个 scale 使用相同规则重新生成数据。1M 与 5M 主结果分别为 [`scale-1000000.log`](../../research/capacity-baseline/results/scale-1000000.log) 与 [`scale-5000000.log`](../../research/capacity-baseline/results/scale-5000000.log)；UUID 列类型的当前 7 索引、三种到达顺序原始输出为 [`index-set-1000000.log`](../../research/capacity-baseline/results/index-set-1000000.log)、[`index-set-5000000.log`](../../research/capacity-baseline/results/index-set-5000000.log)。

每文件 8 个标签、8 个 key：site（1,000 值，均匀）、product（20 值，P01 占 40%，其余 19 值分摊余量）、level（4）、qc（3）、vendor（10）、model（50）、region（20）、instrument（100）。因此 high 为 `product=P01`（40%），low 为 `site=S0001`（0.1%）；另测 AND、OR 与 AND-NOT。时间戳确定性地覆盖 2023-01 至 2025-12，路径形状为 `agent/yyyy/mm/dd/file`。两模型共享文件 ID、路径、时间与标签映射。

容器使用 `postgres:15-alpine`，版本输出为 PostgreSQL 15.19。设置均为镜像默认值：`shared_buffers=128MB`、`work_mem=4MB`、`maintenance_work_mem=64MB`、`effective_cache_size=4GB`、`full_page_writes=on`、`wal_compression=off`、`checkpoint_timeout=5min`、`max_wal_size=1GB`。UUID 各组 bulk 与各组 100 次单行写入前都执行 `CHECKPOINT`；**EAV 与数组的 bulk 只在整段开头做一次**（见 §2）。结果同时记录 checkpoint 计数、`pg_stat_wal.wal_fpi` 与 LSN 差。5M bulk 段 71 次 requested checkpoint 包含显式预检查点，另有约 70 次由 WAL 阈值触发；timed checkpoint 为 0。`EXPLAIN` 中的 execution time 只属于本机，仅供参考；正文只采用字节、WAL 与 shared hit + read blocks。

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
| 1M | 324.09 | 3,150.41 | **3,474.49** | **758.21** | 162,748 / 8 | [1M log](../../research/capacity-baseline/results/scale-1000000.log) |
| 5M | 522.03 | 6,118.71 | **6,640.75** | **1,119.83** | 3,870,973 / 71 | [5M log](../../research/capacity-baseline/results/scale-5000000.log) |

bulk WAL 按各 `INSERT` 前后 `pg_current_wal_lsn()` 差计算；EAV 标签行的每文件数为 8 × 每标签 WAL。默认 checkpoint/WAL 阈值使较大 scale 有更多 FPI，故跨 scale 的 WAL/文件上升不是单纯行宽差。
⚠️ 三段 INSERT 依次执行（EAV 文件行 → 数组 → EAV 标签行），只在最前面有一次 `CHECKPOINT`，FPI 与 checkpoint 计数也只对整段记录一次；各段的 WAL 取决于它在序列中的位置和前面留下的检查点历史。**本表是描述性观测，不是两个模型的受控对比。** 受控对比需要每个模型完整装载前各自检查点、各自记录 FPI、交替顺序并重复。

在数据已加载后，每个模型各执行 100 次单文件 INSERT（EAV 每文件再插 8 条标签）：

| Scale | EAV 单文件 WAL B/文件 | 数组单文件 WAL B/文件 | 原始结果 |
|---:|---:|---:|---|
| 1M | 15,154.96 | 6,921.68 | [1M log](../../research/capacity-baseline/results/scale-1000000.log) |
| 5M | 15,609.68 | 7,265.44 | [5M log](../../research/capacity-baseline/results/scale-5000000.log) |

⚠️ 这 100 次单文件写入没有单独记录 FPI，其 WAL 中有多少是整页镜像无从拆分；与 §5 的单行写入同理（见 §5 末尾注），**不要把这两列当作「每文件写放大」引用**。

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

按 `created_at` 建立 36 个按月 range 分区；无时间范围与固定 `2025-07` 至 `2025-12` 范围都测。公平对照同时包含未分区 GIN-only 表，以及另加 `(created_at DESC,id DESC)` B-tree 的同一表。Chunked 路径按最新分区降序逐个 `EXPLAIN ANALYZE`，到累计 50 行停止。每格为最外层 shared hit + read blocks 与实际计划形状；`Bitmap` 均含 GIN bitmap heap/index scan 后排序。

| Scale | Predicate | 时间范围 | 未分区、无时间索引 | 未分区、有时间索引 | 分区父表 | Chunked（分区数） |
|---:|---|---|---|---|---|---|
| 1M | high | 无 | 17,372 · 并行 Seq + sort | **128 · 有序 index scan + filter** | 13,479 · Append Seq + sort | 388 · Bitmap + sort（1） |
| 1M | low | 无 | 1,005 · Bitmap + sort | 1,005 · Bitmap + sort | 2,891 · Append Bitmap + sort | **94 · Bitmap + sort（2）** |
| 1M | high | 2025-07..12 | 17,339 · Bitmap + sort | **128 · 有序 index scan + filter** | 2,344 · Append Seq + sort | 388 · Bitmap + sort（1） |
| 1M | low | 2025-07..12 | 1,005 · Bitmap + sort | 1,005 · Bitmap + sort | 352 · Append Bitmap + sort | **94 · Bitmap + sort（2）** |
| 5M | high | 无 | 86,337 · 并行 Seq + sort | **125 · 有序 index scan + filter** | 66,817 · Append Seq + sort | 1,890 · Gather Merge + sort（1） |
| 5M | low | 无 | **5,008 · Bitmap + sort** | 50,657 · 有序 index scan + filter | 11,668 · Append Bitmap + sort | **251 · Bitmap + sort（1）** |
| 5M | high | 2025-07..12 | 86,337 · 并行 Seq + sort | **125 · 有序 index scan + filter** | 11,262 · Append Seq + sort | 1,890 · Gather Merge + sort（1） |
| 5M | low | 2025-07..12 | 5,008 · Bitmap + sort | 5,008 · Bitmap + sort | 1,797 · Append Bitmap + sort | **251 · Bitmap + sort（1）** |

5M low、无时间范围这一格显示有序扫描的取舍：优化器沿时间索引检查约 50,657 个 blocks 才找到 50 个稀疏匹配，超过 GIN bitmap + sort 的 5,008 blocks；加时间范围后优化器改回后者。所有 `QUERY_CASE` 完整 JSON 计划与 chunked 的 `plan_shapes`/分区数均在对应 [1M](../../research/capacity-baseline/results/scale-1000000.log) 和 [5M](../../research/capacity-baseline/results/scale-5000000.log) raw log。

## 5. UUID 主键与 `file_entries` 索引集合

UUIDv4 与递增 v7 使用相同的 `g=1..N` 到达序列；v7 的 48-bit 毫秒时间部分随每次插入严格递增。随机 v7 使用完全相同的 ID 集合、仅按确定性 hash 打乱到达顺序。每次 bulk 与每组 100 次逐行 INSERT 前都显式 `CHECKPOINT`。碎片是 `pgstattuple.leaf_fragmentation`，不是文件系统碎片。

| Scale | 主键/到达顺序 | PK bytes | Bulk WAL B/row | Bulk FPI | 叶密度 | 叶碎片 | 单行 WAL B/row（100 次） | 单行 FPI | 原始结果 |
|---:|---|---:|---:|---:|---:|---:|---:|---:|---|
| 1M | UUIDv4（值随机） | 39,624,704 | 170.094 | 135 | 71.77% | 49.81% | 6,187.92 | 0 | [1M log](../../research/capacity-baseline/results/scale-1000000.log) |
| 1M | UUIDv7（时间递增） | 31,563,776 | 156.141 | 3 | 90.03% | 0% | 219.28 | 0 | [1M log](../../research/capacity-baseline/results/scale-1000000.log) |
| 1M | UUIDv7（随机到达） | 39,256,064 | 169.928 | 0 | 72.43% | 50.02% | 6,585.84 | 0 | [1M log](../../research/capacity-baseline/results/scale-1000000.log) |
| 5M | UUIDv4（值随机） | 202,432,512 | 188.821 | 16,894 | 70.21% | 49.84% | 6,573.04 | 0 | [5M log](../../research/capacity-baseline/results/scale-5000000.log) |
| 5M | UUIDv7（时间递增） | 157,720,576 | 155.719 | 4 | 90.04% | 0% | 310.96 | 0 | [5M log](../../research/capacity-baseline/results/scale-5000000.log) |
| 5M | UUIDv7（随机到达） | 205,447,168 | 189.864 | 16,574 | 69.18% | 49.66% | 6,236.56 | 0 | [5M log](../../research/capacity-baseline/results/scale-5000000.log) |

当前 7 索引形状为 PK、UNIQUE(bucket,path)、org/type、agent、重复 bucket、status partial、path trgm；三组除 PK 值/到达顺序外的列完全相同。

| Scale | 主键/到达顺序 | 全部索引 bytes | PK bytes | Bulk WAL B/row | Bulk FPI | 叶密度 | 叶碎片 | 单行 WAL B/row | 单行 FPI | 原始结果 |
|---:|---|---:|---:|---:|---:|---:|---:|---:|---:|---|
| 1M | UUIDv4（值随机） | 397,524,992 | 39,624,704 | 2,064.09 | 89,654 | 71.77% | 49.81% | 7,590.24 | 0 | [1M index log](../../research/capacity-baseline/results/index-set-1000000.log) |
| 1M | UUIDv7（时间递增） | 389,464,064 | 31,563,776 | 2,013.73 | 87,844 | 90.03% | 0% | 1,657.04 | 0 | [1M index log](../../research/capacity-baseline/results/index-set-1000000.log) |
| 1M | UUIDv7（随机到达） | 411,844,608 | 39,256,064 | 2,055.44 | 93,569 | 72.43% | 50.02% | 16,107.36 | 0 | [1M index log](../../research/capacity-baseline/results/index-set-1000000.log) |
| 5M | UUIDv4（值随机） | 1,918,590,976 | 202,432,512 | 16,227.42 | 12,663,910 | 70.21% | 49.84% | 15,791.04 | 0 | [5M index log](../../research/capacity-baseline/results/index-set-5000000.log) |
| 5M | UUIDv7（时间递增） | 1,873,879,040 | 157,720,576 | 12,693.81 | 9,684,943 | 90.04% | 0% | 9,986.64 | 0 | [5M index log](../../research/capacity-baseline/results/index-set-5000000.log) |
| 5M | UUIDv7（随机到达） | 1,994,407,936 | 205,447,168 | 12,708.25 | 9,668,151 | 69.18% | 49.66% | 11,653.84 | 0 | [5M index log](../../research/capacity-baseline/results/index-set-5000000.log) |

单 PK 表中，有序 v7 相对随机 v7 在 5M 少 23.2% PK bytes、18.0% bulk WAL，并把叶碎片从 49.66% 降到 0%。7 索引形状中 PK 的物理差异仍在（大小、叶密度、碎片），但**整表 bulk WAL 的对比无法下结论**：5M 有序 vs 随机 v7 为 12,693.81 vs 12,708.25 B/row，而同为随机写入的 v4 却是 16,227.42 B/row（与随机 v7 差 27.7%，FPI 12,663,910 vs 9,668,151，差 31%）——对照组之间的噪声远大于要测的差异；另外 `ORDER BY md5(g::text)` 打乱的是整行，其余 6 个二级索引的写入顺序也一起变了（5M agent 索引有序 227,950,592 B vs 随机 265,240,576 B，trgm 241,360,896 B vs 261,283,840 B），所以本次结果既不能说明「其余索引淹没了 PK 收益」，也不能说明整表 WAL 对 v7 到达顺序不敏感。100 次 post-load 单行写入则为 9,986.64 vs 11,653.84 B/row。5M bulk 的绝对 WAL/FPI 仍受默认 `max_wal_size=1GB` 触发 checkpoint 影响。

> ⚠️ **单行写入的 FPI 列不可信，WAL 列只作参考（2026-10-14 复读脚本时发现）**。两张表的「单行 FPI」全部为 0，与同行的 WAL 自相矛盾：
> 每组前刚执行过 `CHECKPOINT`（`index-set.sql:42–56`），检查点后每个页面的首次修改必然写一份整页镜像；随机 v4 主键每次插入落在不同叶页上，FPI 不可能为 0。
> 而约 6.5 KB/行的单 PK 随机写入 WAL，恰好接近「每行一份去掉空洞的 8 KB 页」。
> **推断原因（未实测验证）**：PostgreSQL 15 的后端 WAL 统计先在本进程累积，最快约 1 秒才刷新到 `pg_stat_wal`；100 次插入的 `DO` 块不到 1 秒就结束，紧接着在同一会话读取，读到的是刷新前的值。
> bulk 段耗时长，受影响小，但末尾不足 1 秒的部分可能同样漏计。
> **复测时的改法**：读取前调用 `pg_stat_force_next_flush()`（PG 15+），或用 `pg_waldump --stats` 直接统计 LSN 区间内的记录与 FPI。

## 6. 规模外推（模型，不是测量）

只用 1M 与 5M 两个尺寸点拟合 `bytes = 截距 + 每文件斜率 × 文件数`，并假设每文件 8 个标签、字段宽度/索引集合不变。十进制 GB。结果是 §2 全部关系的估算：

| 结构 | 拟合 B/文件 | 1 亿文件 | 5 亿文件 |
|---|---:|---:|---:|
| EAV：`fe_eav` + `ft_eav` | 1,272.72 | 127.29 GB | 636.38 GB |
| `int4[]` + GIN | 254.37 | 25.46 GB | 127.21 GB |

对索引集合与 UUID 主键索引采用相同线性拟合，作为尺寸量级：

| 索引 | 1 亿文件 | 5 亿文件 |
|---|---:|---:|
| 当前 7 索引集合（UUIDv4） | 38.04 GB | 190.15 GB |
| UUIDv4 PK B-tree | 4.07 GB | 20.35 GB |
| UUIDv7 时间递增 PK B-tree | 3.15 GB | 15.77 GB |
| UUIDv7 随机到达 PK B-tree | 4.15 GB | 20.77 GB |

不对 WAL 或 buffers 作规模外推：本次 1M→5M 已显示 checkpoint/FPI 数量、缓存状态与查询计划会显著改变这些数值，线性套用会把本机默认配置误当成固定规律。

## 7. 限制 / 本测量不能说明什么

- 这不是完整生产 `file_entries` 表：标签实验保留了主键、路径唯一索引及被测标签结构；没有复制所有业务列、FK、TOAST、审计/词表表、并发事务或在线保留策略。索引集合实验单独按迁移里的 7 索引列建表。
- 当前 schema 没有设计稿中的数据时间列，分区与有序索引实验用 `created_at` 代理；只测 `(created_at DESC,id DESC)` 一种未分区时间索引，没有比较周/月粒度、分区维护成本或分布漂移。
- 时间递增 UUIDv7 每行推进 1 ms，是单写入者、严格有序的右侧热页基线；没有覆盖同毫秒多 ID、并发 mint、批量乱序到达、跨节点时钟偏差或长事务交错。随机 v7 是同一 ID 集合的打乱控制。
- 7 索引 bulk 在同一实例顺序执行；尽管每组前有显式 `CHECKPOINT`，默认 `max_wal_size` 仍在组内触发大量 checkpoint，对照组（v4 vs 随机 v7）之间差 27.7%，**整表 WAL 对比不可下结论**。打乱到达顺序同时改变了二级索引的写入顺序，未能隔离 PK 顺序的单独效应；受控设计需保持二级键到达顺序不变、单次运行内不触发检查点（或调大 `max_wal_size`），并交替顺序重复。
- **EAV 与数组的 bulk WAL 不是受控对比**（见 §2）。
- **单行写入的 FPI 计数大概率漏计**（全部记为 0，与 WAL 量矛盾；推断为 `pg_stat_wal` 未刷新，未实测验证，见 §5 末尾注）。§2 与 §5 的单行 WAL 列只作参考，不能用来论证入库路径的写放大。
- **测试平台不代表生产**：ARM（aarch64）笔记本上的 Docker 容器（PostgreSQL 15.19，镜像默认配置），非专用机器（同时运行其他程序），共享宿主 SSD；本实验没有任何 x86 对照运行。
  - **可以直接沿用的只有逻辑结论**：两种标签模型在同一确定性数据上的查询结果等价（完整集合、计数、最新一页）。
  - **大小与读页数是方向性参考**：在同一 PostgreSQL 版本与构建、同样的 schema、数据和设置下，可以用来比较方案之间的量级差异；但它们是本平台的测量值，**不保证是 x86 或生产上的数值**，执行计划的选择也可能随版本、构建、统计信息、缓存状态和真实数据分布变化，本实验不能证明 x86 上计划相同。
  - **WAL/FPI、检查点影响、延迟与吞吐**须在与生产同架构、专用、磁盘可消耗的正式测试环境上复测。
- 只测 1M 和 5M 两点；1 亿 / 5 亿是明确假设下的线性尺寸外推，不是 PostgreSQL 实测。WAL 与 buffers 没有外推。
- 数据值分布由确定性生成器控制；除 product 热值外，多数 key 均匀分布，不等于真实租户的长尾与相关性。高/低两个选择性与顺序执行的缓存状态不代表所有参数、相关性或并发负载。
- 所有墙钟时间仅属于本机实验环境；正文不以延迟作主要结论。以上结果提供 B5 的量化输入，不决定标签、分区、计数契约或主键方案。
