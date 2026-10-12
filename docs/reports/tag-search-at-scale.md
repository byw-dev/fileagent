# 海量文件的标签检索：业界做法与 FileAgent 方案建议

> **状态**：调研草稿（2026-10-12），**未立决策**；其中三项已由产品拍板（§5 第 1、2 条），待与基线结论一并写入决策。结论中的体积与代价数字是估算，须由「容量基线」实测验证后再定。
> **规模口径**：FileAgent 目标环境（D-030「千万/年、3–5 年上亿」，产品口径「数亿」），**不是**任何试验环境的数字。
> **关联**：D-025（6c 受控标签）、IC-6（拆表 + 分区）、IC-7（去 `COUNT(*)`）、`docs/design/metadata-model.md`（Phase 2 数据集）。

## 0. 结论先行

1. **检索面是「类型化属性 + 受控标签」，不是路径。** 路径只支持精确与前缀浏览；`storage_path` 的 pg_trgm 索引
   **没有任何查询在用**（代码已核实），删除。
2. **时间、大小、桶、类型等是类型化列，不是标签。** 它们要范围查询、要排序、要做分区键——Danbooru 的 metatag、
   STAC 的 `datetime` 都是这么处理的。
3. **标签按「字典编码的整数数组 + GIN」存在文件行上**，取代「每文件每标签一行」的 EAV 作为检索结构。
   AND / OR / NOT 在一个索引里做位图运算，不需要 `GROUP BY ... HAVING COUNT(*) = N`。
4. **「过滤 + 排序 + 取一页」靠时间分区分块扫描、凑满即停**（pgSTAC 的做法），不靠让规划器猜。
5. **默认不给精确总数。** 单标签计数由增量维护的计数器 O(1) 给出；组合条件只给估算或「超过 N」。
6. **一套小 DSL，编译成同一棵 AST**：文件列表、批量打标圈选、Phase 2 数据集（命名谓词）、将来的订阅都复用它。

## 1. 业界做法

### 1.1 Danbooru（图站，PostgreSQL，数百万帖 / 数千万标签关系）

源码核实（`app/models/post.rb`、`app/logical/post_query_builder.rb`、`db/structure.sql`）：

| 方面 | 做法 |
|---|---|
| 存储 | 标签**反范式化**为帖子行上的 `tag_string`（空格分隔）；GIN 表达式索引 `string_to_array(tag_string, ' ')`，并把该列统计目标调到 3000 |
| 查询 | 数组包含 `@>` / 重叠 `&&`；排序默认按**单调递增的整数 id**，翻页按 id |
| 计数 | `tags.post_count` 在保存帖子时**增量维护**；单标签搜索的总数直接读它；其他情况先估算、再查缓存，**1 秒超时就返回「未知」** |
| 规划器 | 承认 PG 分不清「大搜索 / 小搜索」：结果 ≤ 阈值时**强制位图扫描**（`SET LOCAL enable_seqscan/enable_indexscan = off`）；规模未知时先按大搜索跑、1 秒超时再按小搜索重跑。源码注释直呼 "ugly hack" |
| 护栏 | 每用户标签条数上限、通配符最多展开 100 个标签、按用户的语句超时 |
| 治理 | 标签分类（≈ 命名空间）、别名（同义合并）、蕴含（A ⇒ B，**保存时物化**） |
| DSL | 空格 = AND，`-` = NOT，`~` = OR；`rating:s`、`score:>100`、`date:..`、`order:score` 等 **metatag 映射到类型化列** |

**可借鉴**：反范式数组 + GIN、增量计数器、蕴含写入时物化、metatag 映射类型化列、硬护栏。
**要避开**：靠运行时猜规模、改规划器开关——数据分布一变就失效。

### 1.2 pgSTAC / STAC API（遥感目录，PostgreSQL，生产环境数亿条）

| 方面 | 做法 |
|---|---|
| 分区 | 按 collection，再按年或月；建议**单分区不超过几十万行**；须在灌数据前设定 |
| 索引 | 默认只建 id / datetime / collection / geometry；其余属性经 `queryables` 表**显式声明**才建索引，并警告「索引越多写入越慢」 |
| 检索 | 针对「按时间排序、每次返回 < 1 万条」优化：**按分区分块扫描，凑满所需条数立即返回** |
| 计数 | context（总数）**默认关闭**，文档强调在大库上「非常昂贵」；`auto` 模式用统计信息估算 |
| 查询语言 | OGC CQL2（文本与 JSON 两种形式），可查询属性通过 `/queryables` 公布 |

**可借鉴**：时间分区 + 分块凑满即停、属性须登记才可查（与 D-025「key 严格受控」同构）、总数默认关闭、CQL2-JSON 作为机器接口的参考形态。
FileAgent 的数据多为气象 / 遥感类文件，STAC 是同领域的事实标准，值得在 SDK 层保持兼容的可能。

### 1.3 其他

- **Hydrus Network**（个人图库，SQLite，百万级文件）：`namespace:value` 标签、同义（siblings）与父子（parents）关系——与 D-025 的受控词表思路一致。
- **RUM 索引**（Postgres Pro 扩展）：GIN 的变体，可在倒排项旁附带时间戳，实现「数组检索 + 按时间排序 + LIMIT」在一个索引里完成。
  效果直接，但**不是 PostgreSQL 自带的扩展**，单机一体化部署要多打包、多维护一个 C 扩展 → 作为备选，先不用。
- **Elasticsearch / OpenSearch**：图站与目录类系统规模再大时的常见归宿。对单机一体化部署是又一个有状态组件，还要解决与 PG 的一致性 → 不推荐，除非出现全文检索需求。

## 2. FileAgent 现状（代码核实）

- 文件列表 API 只接受 `agent_id / bucket_id / file_type_id / status / tag=key:value`（可重复，**只有 AND**），`handler/files.go`。
- 标签过滤 SQL（`db/read_queries.go` `tagFilterClause`）：
  `id IN (SELECT file_entry_id FROM file_tags WHERE (key,value) IN (...) GROUP BY file_entry_id HAVING COUNT(*) = N)`
  → **先物化全部命中的文件再排序取一页**，代价与命中数成正比；总数同理。
- `file_tags` 是 EAV：主键 `(file_entry_id, key)` + 索引 `(key, value)`，行数 = 文件数 × 每文件标签数。
- `file_entries` 有 7 个索引，其中 `idx_file_entries_bucket` 与 `UNIQUE (bucket_id, storage_path)` 列完全相同（重复）、
  `idx_file_entries_path_trgm` 无人使用。主键为 UUIDv4。
- 已有可复用的基础：受控词表 `tag_keys` / `tag_values`、待确认队列、`tag_audit`、规则声明 `static_tags` + `path_tag_map`
  （trollsift 反解路径）、回溯打标 worker（MT-5）。

## 3. 方案建议

### 3.1 三类属性

| 类别 | 例子 | 存法 | DSL 写法 |
|---|---|---|---|
| **核心类型化属性** | 数据时间、入库时间、大小、桶、文件类型、采集端、状态 | `file_entries` 的列，B 树索引，**可作分区键与排序键** | `time:2026-01..2026-03`、`size:>10MB`、`bucket:radar`、`sort:-time` |
| **受控标签** | 站点、产品、要素、加工级别、厂家、型号 | 字典编码后存为文件行上的 `int4[]`，GIN 索引 | `site:Z9010`、`level:L2` |
| **未受控值** | 路径反解出的、尚未核准的值 | 照 D-025：随文件保存、进待确认队列，**不进检索** | — |

**「数据时间」要成为一等列**（现在只有 `file_mtime` / `uploaded_at` / `created_at`）。气象类数据几乎所有查询都带时间条件，它是最主要的过滤、排序与分区维度。

现有机制（代码核实，2026-10-12）：
- 路径模板语法是 trollsift 的 `{变量:LDML}`，**没有 `${now:...}`**。时间变量有两种来源：
  - 保留字 `{submit_time:yyyy/MM/dd}`：文件**被提交上传的时刻**，可加时区 `{submit_time:yyyy/MM/dd|tz=Asia/Shanghai}`；同一文件重复采集时取值稳定（D-034，`pkg/trollsift/uploadfields.go`）。
  - `path_pattern` 里用户自己命名的时间字段，例如 `{obs_time:yyyyMMddHHmm}`：从源文件路径解析出来，也就是真正的数据时间。
- CP 入库时已经在用 `dest_path_template` 反解对象键（`controlplane/internal/indexer/indexer.go:439` `applyPathVarTags`）；trollsift 的 `Value` 带 `IsTime` / `Time`，所以反解出的时间字段是带类型的时间，不是字符串。
- 陷阱：`path_pattern` 若声明了 `{time:...}`，别名镜像规则会让 `{submit_time}` 取到**解析出的数据日期**，而不是提交时刻（`InjectSubmitTime`）。所以 `submit_time` 不能当作「入库时间」的可靠来源。

**约定（用户 2026-10-12 定缺省）：**
1. 规则在 `metadata` 里声明数据时间变量（形如 `data_time_var: obs_time`，与 `path_tag_map` 同处），只接受时间类型的字段。
2. 规则没声明、或这个文件解析不出来时，**缺省取入库时间**（CP 的 `created_at`：CP 时钟可信，agent 时钟不一定）。
3. 另存一列 `data_time_source`（`declared` / `fallback`）：按时间检索时，缺省值和真实的数据时间混在同一分区，用户要能分辨，也能筛掉。

**精度问题（已定，见本段末）**：CP 反解的是对象键，数据时间的精度只到 `dest_path_template` 里写进去的程度——源文件名里有 `yyyyMMddHHmm`，而目标路径只写了 `{obs_time:yyyy/MM/dd}` 加 `{filename}`，CP 只能拿到「日」。两条路：
- (a) 要求目标模板写到所需精度：零契约改动，但把检索精度绑在了目录布局上；
- (b) agent 在 `UploadResult` 里上报解析出的数据时间（proto 只增字段，`buf breaking` 允许）：精度完整，解析只发生在一处。
- **定 (b)（用户 2026-10-12）**。不要让 CP 再用 `path_pattern` 反解 `local_path`——那样同一个解析逻辑就有两处（agent 的 `base_path` 相对路径计算也要复制），迟早漂移。

### 3.2 标签的检索结构

```sql
-- 每个受控取值一个整数 id（tag_values 已有，补一列整数代理键即可）
ALTER TABLE file_entries ADD COLUMN tag_ids int4[] NOT NULL DEFAULT '{}';
CREATE INDEX ... ON file_entries USING gin (tag_ids gin__int_ops);   -- contrib 扩展 intarray

-- AND / OR / NOT 一次位图运算：
WHERE tag_ids @@ '12 & (31 | 32) & !77'::query_int
```

- **为什么不用字符串数组**：整数更紧凑，GIN 的倒排列表按整数压缩；`intarray` 的 `query_int` 本身就是布尔表达式，DSL 可以直接编译过去。
- **为什么不再用 EAV 做检索**：每个标签一行 + 两个 B 树索引，数亿文件 × 若干标签会到数十亿行；而且组合查询必须 `GROUP BY` 全部命中。
- **标签的来源与审计**（`source`、谁在何时改的）不放进检索结构：
  **保留完整的 `file_tags` 明细（用户 2026-10-12 拍板）**：它是来源与审计的权威，`tag_ids` 是它的派生检索结构，可以从明细整体重建。
  两者在同一事务里写；明细上只留主键与审计需要的索引，`idx_file_tags_kv` 不再承担检索，基线确认后去掉——这样保留明细的写入代价只剩一份堆写入和一个主键索引。
- **别名与蕴含在写入时物化**（Danbooru 的做法）：`tokyo → Tokyo` 合并、`site:Z9010 ⇒ region:north` 这类关系在打标时展开进 `tag_ids`，
  检索就只有简单的集合运算。代价：改词表关系时要回溯重写（MT-5 回溯 worker 已有）。

### 3.3 过滤 + 排序 + 分页

这是海量检索的核心难题（Danbooru 用规划器开关硬掰，pgSTAC 用分区分块）。建议走 pgSTAC 的路：

1. `file_entries` **按数据时间分区**（月或周，粒度由基线定），这也是 IC-6 的方向。
2. 默认排序 = 数据时间倒序 + id；翻页游标 = `(data_time, id)`，**保持 cursor 分页契约**（V-2）。
3. 查询**从最新的分区往旧扫**：每个分区内用 GIN 位图 + 时间条件，凑满一页立即返回。
   - 命中多的条件（如 `level:L2`）：第一个分区就凑满，成本 ≈ 一页；
   - 命中少的条件（如某个冷门站点）：要扫多个分区，但每个分区都走索引，且用户几乎总会带时间范围。
4. **硬护栏**：每个查询有语句超时；条件数、OR 分支数、通配展开数有上限；**纯否定查询**（只有 `-x`、没有任何正向条件或时间范围）拒绝执行。
5. 主键改 **UUIDv7**（与数据时间、入库时间同向递增），写入只碰索引右侧的热页（见容量基线）。

### 3.4 计数

- **单个标签值的文件数**：在 `tag_values` 上增量维护计数（Danbooru 的 `post_count`），O(1)；同时用于筛选面板与自动补全。
- **组合条件的总数**：默认不算。需要时给估算（规划器估计或按分区抽样），或者给「超过 10,000」。
- 这需要**修改列表接口的 `total` 契约（D-007）**，即 IC-7 的待决事项——此处给出方向：总数可选、可估算、带「是否精确」标记。

### 3.5 DSL

面向人的文本语法，借 Danbooru 的手感，但用 `key:value` 命名空间（与 D-025 的受控 key 对齐）：

```
site:Z9010 product:CR (level:L1 | level:L2) -qc:bad time:2026-01-01..2026-03-31 sort:-time
```

| 元素 | 语法 |
|---|---|
| 与 / 或 / 非 | 空格；`\|`（配括号）；前缀 `-` |
| 标签 | `key:value`；key 必须已登记，value 必须已核准（未核准值不进检索，D-025） |
| 类型化属性 | `time:` `ingested:` `size:` `bucket:` `type:` `agent:` `status:`，支持 `>` `<` `..` 范围 |
| 前缀通配 | `site:Z90*`，展开为已登记取值的 OR，**展开上限**（如 100） |
| 排序 | `sort:-time`（默认）、`sort:size` 等，仅限有索引的列 |

- **文本只是外壳**：解析为 AST，API 同时接受 JSON 形式的 AST（供 SDK，形态参考 CQL2-JSON，必要时可做双向转换）。
- **同一棵 AST 复用到**：文件列表筛选、批量打标圈选（现已用同款筛选谓词）、D-025 Phase 2 的「数据集 = 命名谓词」、将来的订阅。
- 查询编译成 SQL 时只用参数绑定，不拼接字符串（CLAUDE.md 规范）。

### 3.6 路径

只保留精确查找与**前缀浏览**（按桶 + 路径前缀列目录）。`UNIQUE (bucket_id, storage_path)` 的 B 树要能服务前缀查询，
需 `COLLATE "C"` 或 `text_pattern_ops`——随 IC-6 一起定。删除 trgm 索引与重复索引。

## 4. 量级估算（待基线验证）

每文件 8 个标签、5 亿文件，粗估：

| 结构 | 估算 |
|---|---|
| EAV `file_tags` | 40 亿行；单行约 60–80 字节 + 两个 B 树索引 → **数百 GB** |
| `int4[]` 列 | 每行约 24 字节数组头 + 32 字节 → 约 30 GB（随行存放） |
| GIN（`gin__int_ops`） | 40 亿个「标签–行」对，倒排列表压缩后每对约 1–3 字节 → **约 4–12 GB** |

数量级差异足够大，但 GIN 的写入代价（待处理列表、合并）与「数组更新 = 整行新版本」的影响必须实测。

## 5. 待定问题

1. ~~`file_tags` 明细是否保留~~ → **保留**（用户 2026-10-12），见 §3.2。
2. ~~数据时间的来源约定~~ → **规则声明、缺省取入库时间**（用户 2026-10-12），见 §3.1；精度走 (b) agent 上报（用户 2026-10-12）。
3. **分区粒度**：按月还是按周，要看基线里单分区行数与查询延迟（pgSTAC 建议单分区几十万行，在 FileAgent 规模下意味着很多分区）。
4. **SDK 是否直接采用 CQL2-JSON**：与 STAC 生态互通的价值，对比自定义 AST 的简单。
5. **蕴含关系的范围**：只做「值 ⇒ 值」，还是允许「值 ⇒ 类型化属性」。

## 6. 下一步

1. **容量基线**（只测不改）加入以下对照：EAV 对比 `int4[]` + GIN；单 / 多标签 × 高 / 低选择性；分区分块扫描对比全表；UUIDv4 对比 v7；有 / 无 trgm 与重复索引。
   只测与硬件无关的量：索引字节 / 行、WAL 字节 / 行、每次查询读页数。
2. 基线出数后，合并为一条决策：检索模型（本报告）+ IC-6 分区 + IC-7 计数契约 + UUIDv7。

## 资料来源

- Danbooru 源码：[post_query_builder.rb](https://github.com/danbooru/danbooru/blob/master/app/logical/post_query_builder.rb)、
  [post_query.rb](https://github.com/danbooru/danbooru/blob/master/app/logical/post_query.rb)、
  [post.rb](https://github.com/danbooru/danbooru/blob/master/app/models/post.rb)、
  [structure.sql](https://github.com/danbooru/danbooru/blob/master/db/structure.sql)；
  [DeepWiki: Danbooru Search and Querying](https://deepwiki.com/danbooru/danbooru/4.2-search-and-querying)
- [PgSTAC 文档](https://stac-utils.github.io/pgstac/pgstac/)、[pgstac 仓库](https://github.com/stac-utils/pgstac)、
  [pystac-client CQL2 教程](https://pystac-client.readthedocs.io/en/latest/tutorials/cql2-filter.html)
- [RUM 扩展](https://github.com/postgrespro/rum)
- [Crunchy Data: Tags and Postgres Arrays](https://www.crunchydata.com/blog/tags-aand-postgres-arrays-a-purrfect-combination)
- [Hydrus Network（DeepWiki）](https://deepwiki.com/hydrusnetwork/hydrus)
