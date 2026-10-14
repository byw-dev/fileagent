# 海量文件的标签检索：业界做法与 FileAgent 方案建议

> **状态**：**B5 决策草案**（2026-10-12 起草，2026-10-14 按容量基线结果与产品拍板修订，2026-10-15 按评审修订），**未立决策**；产品已拍板的条目逐条标注日期。
> 体积与读页数取自容量基线实测（[`capacity-baseline.md`](capacity-baseline.md)，1M / 5M 两点）；1 亿以上为线性外推。
> **2026-10-14 修订要点**：§0.1 检索的使用场景（SDK 枚举 + 运维点查）；§1.4 轻量搜索引擎的评估结论（都不引入）；
> §3.1 数据时间的选取规则；§3.3 / §3.4 按使用场景重写；§3.7 只锁定以后改不动的部分；§4 换成实测数字。
> **2026-10-15 修订要点**：§3.3 身份表与检索表分离（`file_entries` 不分区，另建按数据时间分区的检索表）；§3.4 计数只作提示、不用于跳过分区；
> §3.3 续扫令牌取代二元游标；§3.1 明确时间字段个数校验与 `data_time` 持久化是 **B6 要新增的行为**（当前代码没有）；§3.7 拆成三栏。
> **规模口径**：FileAgent 目标环境（D-030「千万/年、3–5 年上亿」，产品口径「数亿」），**不是**任何试验环境的数字。
> **关联**：D-025（6c 受控标签）、IC-6（拆表 + 分区）、IC-7（去 `COUNT(*)`）、`docs/design/metadata-model.md`（Phase 2 数据集）。

## 0. 结论先行

1. **检索面是「类型化属性 + 受控标签」，不是路径。** 路径只支持精确与前缀浏览；`storage_path` 的 pg_trgm 索引
   **没有任何查询在用**（代码已核实），删除。
2. **时间、大小、桶、类型等是类型化列，不是标签。** 它们要范围查询、要排序、要做分区键——Danbooru 的 metatag、
   STAC 的 `datetime` 都是这么处理的。
3. **标签按「字典编码的整数数组 + GIN」存在检索表的行上**，取代「每文件每标签一行」的 EAV 作为检索结构。
   AND / OR / NOT 在一个索引里做位图运算，不需要 `GROUP BY ... HAVING COUNT(*) = N`。
4. **「过滤 + 排序 + 取一页」靠时间分区分块扫描、凑满即停**（pgSTAC 的做法），不靠让规划器猜。
   **按数据时间分区的是派生的检索表，`file_entries` 本身不分区**，继续承担全局唯一的身份与对象键（产品 2026-10-15，§3.3）。
5. **默认不给精确总数。** 「标签值 × 分区」计数只是近似统计，用来选扫法和展示，**不用来跳过分区**（产品 2026-10-15，§3.4）；组合条件只给估算或「超过 N」。
6. **一套小 DSL，编译成同一棵 AST**：文件列表、批量打标圈选、Phase 2 数据集（命名谓词）、将来的订阅都复用它。
7. **不引入任何外部搜索引擎**（Elasticsearch、Meilisearch、Typesense 等），也不做「任意组合条件下的分面计数」——按 §0.1 的使用场景，两者都用不上（§1.4）。
8. **SDK 查询必须带时间范围**（产品 2026-10-14），不允许不限时间的任意查询。

### 0.1 使用场景（产品 2026-10-14）

检索只服务两类使用方，优化目标按它们来定，**不按「网页翻页浏览」来定**：

| 使用方 | 典型问题 | 查询形态 | 成本特征 |
|---|---|---|---|
| **SDK**（用户、外部系统、将来的 ETL） | 「2026-01 到 03 月、`site:Z9010`、`product:CR` 的全部文件」 | **必须带时间范围**；标签条件；按续扫令牌分页（§3.3），大页（上千条） | 与命中数成正比——命中数就是要返回的结果量，无可省；要优化的是每行成本与续翻不重扫 |
| **Web UI**（运维） | 「某个 site 最新一笔到没到」「某时次的加工产物生成没有」 | 最新一条 / 某个时刻是否存在 | 点查，只碰一到两个分区 |

- SDK 返回文件清单（至少含 bucket、对象 key、大小、sha256），**用户自行用 STS 或 ak/sk 去 S3 下载**。
- **不支持**用户越过 CP 自行拼接 `s3://` URL 取文件；路径模板不是对外公开契约。
- 用户不会在网页上翻几十上百页列表，所以「最新一页的组合分面」「深分页浏览」都不是设计目标。
- ETL 是 SDK 的用户：经 SDK 取文件 → 下载 → 加工 → 上传并注册（IC-9 / IC-10），产物纳入 CP 管理、打标、关联血缘（IC-14）。
  **ETL 的接入方式、能否覆盖同一 key、SDK outbox 形态均未定；MVP 未到位，SDK 与 ETL 暂不实现，本文只做预留。**

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

### 1.4 轻量搜索引擎与 PG 扩展（2026-10-14 调研）

结论：**都不引入。** 独立引擎的设计目标是「搜索框边输边出、按相关度取前若干条」，与 §0.1 的「按时间范围全量枚举 + 运维点查」正相反；
且任何一个都是 PG 之外的第二份数据，需要同步与对账（IC track 至今没做完「存储 ↔ PG」这一对），批量改标签在倒排索引里同样是删了重建。

| 候选 | 情况（已核实的标注来源） | 与本系统的冲突 |
|---|---|---|
| Meilisearch | 社区版 MIT；分片、复制、S3 流式快照等企业功能受 BUSL / 商业许可约束 [^meili-lic]；单索引上限约 43 亿文档，建议索引不超过 2 TiB [^meili-lim] | 一次搜索默认最多只能翻到前 1000 条（`maxTotalHits`），无游标分页，**翻不完全部命中** [^meili-lim] |
| Typesense | GPL-3.0，索引全部常驻内存 | 10 亿条需数百 GB 内存，单机不现实 |
| Manticore | GPL，2025 年增强分面 [^manticore] | 深分页受 `max_matches` 限制；10 亿规模需分片 |
| Quickwit | 被 Datadog 收购后宣布改用 Apache 2.0 [^quickwit] | 面向日志，基本只追加，批量改标签几乎做不了 |
| ClickHouse / Doris / DuckDB | OLAP 引擎 | 改标签走 mutation，代价重；同样是第二份数据 |
| ParadeDB `pg_search` | PG 扩展（Tantivy），AGPL-3.0；分面强 [^parade-facet] | 长处是 BM25 相关度，用不上；聚合下推要求查询带它自己的搜索运算符、索引列建时定死，增减列须 `REINDEX` [^parade-lim]；AGPL 用于私有化交付需法务评估；Rust 大扩展打包负担重 |
| `pg_roaringbitmap` + `pgfaceting` | Apache 2.0 / BSD-3，PGDG 有安装包 [^pgfaceting] [^roaring]；1 亿行带过滤的分面从 3 分 42 秒降到 155 毫秒 [^pganalyze] | **唯一能补上 PG 短板（任意组合的分面计数）的方案**，但 §0.1 下不需要这项能力。另有限制：pgfaceting 只支持 32 位整数 id（需另加代理 id），项目很小（19 次提交）；镜像须从 alpine 换 Debian 系 |

**什么情况下重新评估**（出现任一条）：产品要求任意组合条件下的实时分面计数（→ 首选 `pg_roaringbitmap`，不是外部引擎）；出现全文 / 模糊检索需求；规模超出单机 PG。
本设计里有两处让将来接入的成本保持很低：检索结构是从 `file_tags` 明细**派生**、可整体重建的（§3.2）；查询先编译成 AST 再翻译（§3.5），换后端不改 API。

[^meili-lic]: [Meilisearch Enterprise Edition license](https://www.meilisearch.com/blog/enterprise-license)
[^meili-lim]: [Meilisearch known limitations](https://www.meilisearch.com/docs/learn/resources/known_limitations)
[^manticore]: [Manticore faceted search (25.12)](https://manticoresearch.com/blog/faceted-search-without-manual-filter-building.md)
[^quickwit]: [Quickwit joins Datadog](https://quickwit.io/blog/quickwit-joins-datadog)
[^parade-facet]: [ParadeDB faceting](https://www.paradedb.com/blog/faceting)
[^parade-lim]: [ParadeDB aggregate limitations](https://api.pgxn.org/src/pg_search/pg_search-0.23.0/docs/documentation/aggregates/limitations.mdx)
[^pgfaceting]: [pgfaceting](https://github.com/cybertec-postgresql/pgfaceting)
[^roaring]: [pg_roaringbitmap](https://pgxn.org/dist/pg_roaringbitmap)
[^pganalyze]: [pganalyze: roaring bitmaps & pgfaceting](https://pganalyze.com/blog/5mins-postgres-roaring-bitmaps-pgfaceting-query-performance)

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
| **核心类型化属性** | 数据时间、入库时间、大小、桶、文件类型、采集端、状态 | `file_entries` 的列；检索要用的那几列同时写入检索表（§3.3），在那里作分区键与排序键 | `time:2026-01..2026-03`、`size:>10MB`、`bucket:radar`、`sort:-time` |
| **受控标签** | 站点、产品、要素、加工级别、厂家、型号 | 字典编码后存为检索表行上的 `int4[]`，GIN 索引 | `site:Z9010`、`level:L2` |
| **未受控值** | 路径反解出的、尚未核准的值 | 照 D-025：随文件保存、进待确认队列，**不进检索** | — |

**「数据时间」要成为一等列**（现在只有 `file_mtime` / `uploaded_at` / `created_at`）。气象类数据几乎所有查询都带时间条件，它是最主要的过滤、排序与分区维度。

现有机制（代码核实，2026-10-12）：
- 路径模板语法是 trollsift 的 `{变量:LDML}`，**没有 `${now:...}`**。时间变量有两种来源：
  - 保留字 `{submit_time:yyyy/MM/dd}`：文件**被提交上传的时刻**，可加时区 `{submit_time:yyyy/MM/dd|tz=Asia/Shanghai}`；同一文件重复采集时取值稳定（D-034，`pkg/trollsift/uploadfields.go`）。
  - `path_pattern` 里用户自己命名的时间字段，例如 `{obs_time:yyyyMMddHHmm}`：从源文件路径解析出来，也就是真正的数据时间。
- CP 入库时已经在用 `dest_path_template` 反解对象键（`controlplane/internal/indexer/indexer.go:439` `applyPathVarTags`）；trollsift 的 `Value` 带 `IsTime` / `Time`，所以反解出的时间字段是带类型的时间，不是字符串。
- 陷阱：`path_pattern` 若声明了 `{time:...}`，别名镜像规则会让 `{submit_time}` 取到**解析出的数据日期**，而不是提交时刻（`InjectSubmitTime`）。所以 `submit_time` 不能当作「入库时间」的可靠来源。

**原则（产品 2026-10-14）：FileAgent 只负责采集，不负责理解文件。** 数据时间就是「这个文件对应的那个时间」——
原始路径里有时间戳就解析出来用（它通常也被合成进 dest_path），没有就用采集上传的时刻。两种来源记在**同一列**。
这个时间代表观测、起报还是时段，由使用数据的人自己理解；预报类、时段类数据都靠用户自适应，FileAgent 不区分。

**选取规则（产品 2026-10-14，取代 2026-10-12 的「缺省取 CP 入库时间」）：**

| `path_pattern` 中时间类型字段的个数 | 数据时间 | `data_time_source` |
|---|---|---|
| 0 | agent 的 `submit_time`（与 dest_path 合成用的是**同一个值**） | `fallback` |
| 1 | 自动选中该字段，无需声明 | `declared` |
| ≥ 2 | 规则必须在 `metadata` 里声明 `data_time_var`（如 `/{fire_dt:yyyyMMddHH}/XX_fc_{fc_dt:yyyyMMddHH}.nc` 声明 `fire_dt`） | `declared` |

1. **≥ 2 个时间字段而未声明时，沿用现有模板校验的处理模式**（产品 2026-10-14）：CP 保存规则时给警告、**不返回 422**（D-030 §8 不对模板形状设约束），
   agent 上传时拒绝、任务失败，**不猜**用哪个（「取第一个出现的」在 `XX_{fc_dt}_{fire_dt}.nc` 上会静默选错，而选错意味着数据落进错误分区）。
   Web UI 编辑规则时可把第一个时间字段**预选**为默认值，由人确认。
   ⚠️ **这是 B6 要新增的校验，不是现状**：现有的「CP 警告、agent 拒绝」（`contracts.md` V-3）只覆盖保留字误用与 trollsift 语法/时区；
   当前代码不统计时间字段个数，全仓也没有 `data_time_var`，两个普通时间字段的规则会被正常保存并上传。三端验收：
   CP `ruleTemplateWarnings` 新增该警告；agent 在 `reservedTimeMisuse` 同一位置拒绝；Web UI 规则编辑器给出同一提示并预选。
2. **选中的字段至少精确到日**；只到日按当天零点。日期与小时拆成两个字段（`{d:yyyyMMdd}/{h:HH}`）的合并，等真遇到再说。
3. **时区：默认按 UTC 解析，规则可声明时区**（复用 trollsift 已有的 `|tz=Asia/Shanghai` 写法）。库里统一存 UTC。
4. **为什么兜底取 agent 的 `submit_time` 而不是 CP 时钟**：「路径里的时间 = 表里的时间」在三种情况下都成立，用户最容易理解；
   取 CP 时钟会在零点附近与 dest_path 的日期目录不一致。agent 时钟可能出错，由 CP 兜底：与 CP 当前时间相差超过阈值（如一天）时照常入库，
   但打异常标记、告警，并落进默认分区，不让离谱时间（如 2099 年）造出远期分区。这与 `observed_at` 守卫不信任 agent 时间不冲突——守卫管写入先后，本列只用于检索与分区。
5. **没被选中的时间字段**（如上例的 `fc_dt`）只留在 dest_path 里，不进检索。将来真有按预报时效查询的需求，再加一个类型化列。
6. 另存 `data_time_source`（`declared` / `fallback`）：兜底值与真实数据时间混在同一分区，用户要能分辨、能筛掉。
7. **`data_time` 只算一次，随任务持久化**（B6 新增）：agent 在合成 dest_path 的**同一次解析**里同时得到对象键与选中的 UTC `data_time`、`data_time_source`，
   写进本地队列的上传任务（今天 `submitFile` 只把 `time.Now()` 传给路径合成，`UploadTask` / SQLite 只存 `storage_path` 等字段），
   `UploadResult` 原样上报。重试、进程重启都**不得重算**——否则跨零点重试会让对象键里的日期与表里的 `data_time` 分属不同分区。
8. **分区边界一律按 UTC**：月分区的上下界是 UTC 零点；用户按本地时间给的查询范围，先按规则声明的时区换成 UTC 再裁剪分区。

**与保留字规则的关系（产品 2026-10-14：D-034 维持现状）**：`path_pattern` 仍可把字段命名为 `submit_time` / `time` / `filename` / `ext` / `agent_name` / `agent_id`，
解析值优先、注入不覆盖，`time ↔ submit_time` 互为镜像（`contracts.md` V-3）。这与上表不冲突：镜像只在 pattern 解析出 `time` 或 `submit_time` 时发生，
而此时规则已经有时间字段、走的是「1 个 / ≥ 2 个」分支，不会落到 `submit_time` 兜底。

**精度问题（已定，见本段末）**：CP 反解的是对象键，数据时间的精度只到 `dest_path_template` 里写进去的程度——源文件名里有 `yyyyMMddHHmm`，而目标路径只写了 `{obs_time:yyyy/MM/dd}` 加 `{filename}`，CP 只能拿到「日」。两条路：
- (a) 要求目标模板写到所需精度：零契约改动，但把检索精度绑在了目录布局上；
- (b) agent 在 `UploadResult` 里上报解析出的数据时间（proto 只增字段，`buf breaking` 允许）：精度完整，解析只发生在一处。
- **定 (b)（用户 2026-10-12）**。不要让 CP 再用 `path_pattern` 反解 `local_path`——那样同一个解析逻辑就有两处（agent 的 `base_path` 相对路径计算也要复制），迟早漂移。

### 3.2 标签的检索结构

```sql
-- 每个受控取值一个整数 id（tag_values 已有，补一列整数代理键即可）
-- 列在检索表 file_search 上（§3.3），不在 file_entries 上
ALTER TABLE file_search ADD COLUMN tag_ids int4[] NOT NULL DEFAULT '{}';
CREATE INDEX ... ON file_search USING gin (tag_ids gin__int_ops);   -- contrib 扩展 intarray

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

### 3.3 过滤 + 排序 + 分页（2026-10-14 按 §0.1 重写，2026-10-15 修订）

走 pgSTAC 的路（分区 + 分块），但按两类使用场景分别优化：

**身份表与检索表分离（产品 2026-10-15）**

PostgreSQL 要求分区表的主键与唯一约束包含分区键（[文档](https://www.postgresql.org/docs/current/ddl-partitioning.html#DDL-PARTITIONING-DECLARATIVE-LIMITATIONS)）。
若直接把 `file_entries` 按 `data_time` 分区，`PRIMARY KEY (id)`、`UNIQUE (bucket_id, storage_path)` 都保不住，
`upload_logs` / `file_tags` / `tag_audit` 指向 `file_entries(id)` 的外键也随之失效。因此：
- **`file_entries` 不分区**，继续是文件身份的权威：全局唯一的 `id`、`UNIQUE (bucket_id, storage_path)`、全部外键与 IC-2a 的 `ON CONFLICT` 守卫都不变；新增 `data_time`、`data_time_source` 列。
- **另建检索表**（暂名 `file_search`），按 `data_time` 范围分区，主键 `(data_time, id)`；只放检索要用的列（`data_time`、`id`、桶、文件类型、采集端、`data_time_source`、`tag_ids` 等），GIN 建在这里。
- 检索表是**派生**的：与 `file_entries` / `file_tags` 在同一事务里写；改标签只改它的 `tag_ids`；`data_time` 被修正时在同一事务里删旧行、插新行（跨分区移动）；整张表可从 `file_entries` + `file_tags` 重建。
- 代价：每个文件多一行检索行（不含 `storage_path`，估算小于容量基线里数组模型的 260 B/文件，未实测），以及一次额外写入。
- 与 IC-6 原设计的差异：IC-6 原计划把 `file_entries` 按 `uploaded_at` 分区、身份移到窄表 `object_keys`，并为此列了三个前置阻塞（主键须含分区键、三张表的外键、`uploaded_at` 可空）。本方案让被分区的表不承担身份，这三个阻塞都不再出现；IC-6 的条目随 B5 立决策时改写。

**公共基础**
1. 检索表**按数据时间分区**，粒度先按月（每年约 2600 万对象 → 单月约 200 万行），等真实数据进来、单分区明显过大再改按周；
   分块扫描每次只直接查一个分区，分区多不会拖慢规划，限制粒度的是单分区内「位图 + 排序」的成本。分区边界按 UTC（§3.1）。
2. **异常时间与默认分区**：agent 时钟异常的兜底行（§3.1 第 4 条）落进默认分区。之后若要新建与默认分区里某些行重叠的月分区，PostgreSQL 会拒绝，
   所以分区维护任务在建新分区时，须在同一事务里把默认分区中落在该范围的行移出再建——检索表是派生的，删了重插不丢信息。
3. **分页用续扫令牌**，仍是 cursor 分页契约（V-2）。令牌对客户端不透明，内含：查询指纹（条件与固定下来的时间范围）、扫描方向、
   当前分区、该分区内已扫到的 `(data_time, id)`、是否已扫完。**不能只由最后一条结果推出**：请求可能扫完几个空分区就到了工作量上限，一条结果都没有，
   此时仍须给出令牌，下次从未扫的分区继续，而不是重扫已扫过的空分区。现行 V-2 实现只从最后一行生成游标（`handler/files.go`），新令牌是它的超集。
4. 维护「**标签值 × 分区**」的近似计数（§3.4），**只用来选扫法和排顺序，不用来跳过分区**。
5. 主键改 **UUIDv7**（与入库时间同向递增），写入只碰索引右侧的热页（容量基线 §5：5M 时 PK 小 22%、叶碎片 50% → 0%）。

**SDK 枚举（主路径）**
- **必须带时间范围**（产品 2026-10-14）→ 分区裁剪先把范围收到几个分区；分区内用 GIN 筛标签；按续扫令牌续翻，不重扫已扫过的部分。
- 页大小可到上千条；以后可加流式导出（如 NDJSON），省去逐页往返。
- 成本与命中数成正比，这就是输出量本身。容量基线 §3 表明：**现行 EAV 写法**（`GROUP BY … HAVING COUNT(*)`）在 5M 上取最新 50 条高命中要读 837 万页、AND-NOT 要读 1837 万页，
  数组 + GIN 同样条件约 8.6 万页（且这是未加时间索引的全表顺序扫描）——**现行写法撑不到大规模，必须换**。

**运维点查（Web UI）**
- 「某 site 最新一笔到没到」：从最新的分区往回逐个分区查，第一个命中的分区里取 `data_time` 最大的一条即停；受工作量上限约束，到上限仍未命中就如实回答「最近 N 个分区内没有」。
  正常情况下最新一笔就在最近一两个分区里。
- 「某时次的产物生成没有」：`data_time = T` 只落在一个分区，加标签条件即可。
- 在同一分区内，**命中多**的条件按时间索引顺序扫、边扫边过滤即可（容量基线 §4：约 125 页凑满 50 条，且不随规模增长）；
  **命中少**的条件按时间顺序扫会退化（5M 低命中要 50,657 页），应走 GIN 位图再排序（5,008 页）。
  **扫法由应用按计数器给出的命中率估计选择**，不交给规划器猜（Danbooru 的教训，§1.1）；计数偏了只会选到较慢的扫法，结果仍正确。
- 将来若要「所有站点最新到达时间一览」这类到报看板，再加一张「每个标签值最新一笔」的表，入库时顺手更新。现在不做。

**护栏**
- 每个请求的工作量设上限（扫描分区数或块数），到上限就带着已找到的结果和续扫令牌返回——「一页不满」甚至「零条但未结束」都是合法响应，SDK 须理解。
  在必须带时间范围的前提下这只是安全网。
- 上限靠**逐块执行、每块完成就记下进度**来实现，不靠 `statement_timeout`：语句超时会取消整条 SQL，已扫到的部分结果拿不回来。
- 验收用例至少覆盖：连续多个空分区；零结果但后面还有分区；恰好在分区边界到达上限；翻页期间有并发入库或改标签。
- 条件数、OR 分支数、通配展开数有上限；纯否定查询拒绝执行。
- 关于「几个常见标签求交、结果很少」：GIN 在同一索引上的多键 AND 是在压缩的倒排列表上求交，只为交集回表，
  成本主要是读倒排列表（每条约 1–2 字节，估算，未实测），不会失控。

### 3.4 计数

- **「标签值 × 分区」计数只作提示（产品 2026-10-15）**：用途限于选择分区内扫法（§3.3）、运维视图与自动补全里的**近似**计数。
  **不用来跳过分区，也不用来回答「有没有」**——它若作裁剪依据，一次漏记就会让查询静默漏文件，就得承担同事务更新、幂等、
  覆盖删除/改标签/时间修正、定期校验与重建的全套一致性成本；只作提示时，计数偏了最多选到较慢的扫法。
  因此维护可以宽松：入库与批量打标时顺带更新或异步批量更新均可，定期从检索表重算校正；展示时标明是近似值。
- **单标签在时间范围内的精确数**：不由计数器给出，与组合条件的总数一样走单独的计数调用。
- **组合条件的总数**：默认不算。SDK 需要「一共多少个文件、多大」时，提供单独的计数调用，并说明它的开销与枚举一次相当。
- **不做任意组合条件下的分面计数**（§0.1、§1.4）。
- 列表接口的 `total` 契约（D-007）需相应修改，即 IC-7 的待决事项：总数可选、可估算、带「是否精确」标记。

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

### 3.7 现在必须定的 vs 以后再做的

里程碑 B 的时间窗口（第一批生产数据落地之前）只用来锁定**以后改代价很高**的部分：

| 归属 | 项目 | 为什么这样排 |
|---|---|---|
| **B6 必做** | `file_entries` 加 `data_time`、`data_time_source`；按 `data_time`（UTC 月）分区的检索表及其 `tag_ids` + GIN；agent 解析并持久化 `data_time`、经 `UploadResult` 上报（proto 只增字段）；≥ 2 个时间字段的三端校验（§3.1） | 有了生产数据之后再改，要重写全部数据或锁表、双写 |
| **已拍板、另排期**（不占 B 的时间窗口） | 新文件改用 UUIDv7 生成（旧行保持 v4，无需迁移）；删除 trgm 与重复索引；文件列表 API 改走检索表（续扫令牌、工作量上限、DSL）；近似计数 | 随时可做，或可渐进上线；检索表在 B6 就开始写入，API 切换时无需回填 |
| **推后讨论**（产品 2026-10-14） | `file_entries` 自身的宽窄表拆分 | 检索已拆到独立的窄表，剩下的只是不分区的 `file_entries` 自身的列，以后再拆不涉及重写分区 |
| **只预留、不实现** | `file_entries.run_id` 等可空列（PG 加无默认值的可空列只改元数据）；IC-9 注册接口的 `data_time` 字段 | 等 SDK / ETL 落地时再加 |

为将来的 SDK / ETL 写入**预留**、但现在不实现（产品 2026-10-14）：
- IC-9 注册接口（`POST /api/v1/files/register`）的字段需增加 `data_time`（显式传入时 `data_time_source=declared`）。
  当前字段表（`consistency-ingest.md` IC-9）没有它，ETL 产物就只能退回入库时间，「某时次产物生成没有」会查不准。
- 血缘按 run 模型（IC-14，`metadata-model.md` P2.3）：`file_entries.run_id` 可空列，ETL 落地时再加。

## 4. 量级（容量基线实测 + 外推）

数字取自 [`capacity-baseline.md`](capacity-baseline.md)：合成数据、每文件 8 个标签、PostgreSQL 15 默认配置；1M / 5M 两点实测，1 亿 / 5 亿为线性外推。
本文只引用关系体积与 shared hit+read 读页数，它们只用于同一测试平台、版本、schema、数据与设置下的**方向性比较**，不保证在生产上是相同的绝对值或执行计划；WAL、延迟、吞吐不在此列。

| 结构 | 实测 B/文件（5M） | 外推 1 亿 | 外推 5 亿 |
|---|---:|---:|---:|
| EAV：文件行 + `file_tags` | 1,276 | 127 GB | 636 GB |
| `int4[]` + GIN（基线里放在文件行上；本方案放在检索表上，另加 `file_entries` 自身，未实测） | 260 | 25 GB | 127 GB |
| 当前 `file_entries` 7 个索引（UUIDv4） | — | 38 GB | 190 GB |

- 两种标签模型在 5 种过滤下的完整结果集、计数与最新一页**逐条一致**（容量基线 §3）。
- 保留 `file_tags` 明细作审计（§3.2）后，它不再承担检索，可去掉 `(key, value)` 索引，代价只剩一份堆与一个主键索引。
- 单行写入的 WAL 数据有测量缺陷（FPI 计数漏计），不能用来论证入库写放大（容量基线 §5 末尾注）。
  按每秒不到 1 个文件的入库速率，写放大不影响吞吐，只影响 WAL 体积与备份大小，**不构成决策依据**。

## 5. 待定问题

1. ~~`file_tags` 明细是否保留~~ → **保留**（产品 2026-10-12），见 §3.2。
2. ~~数据时间的来源约定~~ → **见 §3.1 选取规则**（产品 2026-10-14，取代 2026-10-12 的「缺省取入库时间」）；精度走 (b) agent 上报（产品 2026-10-12）。
3. ~~SDK 查询是否必须带时间范围~~ → **必须**（产品 2026-10-14）。
4. ~~是否引入外部搜索引擎 / 组合分面~~ → **不引入、不做**（2026-10-14），见 §1.4。
5. **分区粒度**：先按月，有真实数据后按单分区行数再定是否改周（§3.3）。
6. **宽窄表拆分**的具体列：后面再讨论（产品 2026-10-14）。检索表独立之后，这只涉及不分区的 `file_entries` 自身（§3.7）。
7. **SDK 是否直接采用 CQL2-JSON**：与 STAC 生态互通的价值，对比自定义 AST 的简单。SDK 实现时再定。
8. **蕴含关系的范围**：只做「值 ⇒ 值」，还是允许「值 ⇒ 类型化属性」。
9. **推后，不在 B5 范围**：ETL 的接入方式（取决于 ETL 的部署与调度平台）、ETL 能否覆盖同一 key（取决于 ETL 用户的需求；建议默认不允许，因为覆盖会让目录里的 sha256 与血缘失真）、SDK outbox 形态、Python SDK 的实现时机。
10. ~~分区后的主键与外键怎么办~~ → **身份表 `file_entries` 不分区，另建按数据时间分区的检索表**（产品 2026-10-15），见 §3.3。
11. ~~「标签值 × 分区」计数是否用于跳过分区~~ → **只作提示，不跳过分区**（产品 2026-10-15），见 §3.4。

## 6. 下一步

1. ~~容量基线~~ → 已完成（[`capacity-baseline.md`](capacity-baseline.md)）。产品判断：UUIDv7 与精简索引的方向已明确，**不再为补数字做正式环境复测**（2026-10-14）。
2. **B5 立决策**：把本文合并为一条决策（检索模型 + 身份表与检索表分离 + 数据时间一等列与选取规则 + 分区 + IC-7 计数契约 + UUIDv7）；
   UUIDv7 需同步修订 CLAUDE.md「数据库主键统一 UUID v4」；IC-6 的条目按 §3.3 改写。
3. **B6 落地**：只做 §3.7 的「B6 必做」一栏。

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
