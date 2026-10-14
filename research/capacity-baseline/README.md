# File index capacity baseline

Run from the repository root with Docker available:

```sh
bash research/capacity-baseline/run.sh
```

> **Cost warning — read before running.** A full run (1M + 5M, all variants) takes roughly 1.5–2.5 hours and, with
> PostgreSQL's default `max_wal_size=1GB`, writes on the order of **800 GB** to disk — an order-of-magnitude estimate, not a measured counter (checkpoint-driven
> full-page writes dominate; the 5M seven-index variants alone generate about 63–81 GB of WAL each). The database peaks around 20 GB.
> Do not re-run casually on a laptop SSD; for a quick sanity check, drop `5000000` from the two `for scale in …` lists in `run.sh`.

The script uses only `postgres:15-alpine`, container `fileagent-b4-pg`, host port
15432, and named volume `fileagent-b4-pg-data`. It refuses to reuse an existing
container/volume, captures SQL/CSV/JSON outputs under `research/capacity-baseline/results/`,
and removes both resources on exit. It sets `TMPDIR="$PWD/bin"` and does not set
`GOCACHE`. Seed: 20261012; row ids and values are deterministic. The data uses
three years of timestamps and path-shaped strings, eight tags per row across
eight keys, a 40% hot product value with the remaining mass spread across 19
values, a four-value level key, 3 QC values, and cardinalities 10/50/20/100 for
vendor/model/region/instrument plus 1,000 site values. The product head is
intentionally skewed; other keys are uniform deterministic controls.

Measurements are generated in SQL so row data and tag assignments are identical
between storage models. Result identity checks compare ordered UUID page sets and
full matching UUID sets for every predicate. Raw results include server version,
relevant settings, relation/index sizes, WAL LSN deltas, and `EXPLAIN (ANALYZE,
BUFFERS)` output. All wall-clock values are labeled local-only.

The current seven-index `file_entries` shape is run separately by `index-set.sql`
using UUID-typed bucket/org/type/agent columns. It repeats UUIDv4, chronological
UUIDv7, and random-arrival UUIDv7 with identical non-PK values and reports the
same bulk and post-load WAL, FPI, PK size, leaf-density, and fragmentation
metrics. The compact int4-key proxy in `benchmark.sql` remains auxiliary.

The PK comparison builds UUIDv4 with explicit version/variant bits from a seeded
MD5 stream and builds UUIDv7 in SQL from a 2023 epoch-millisecond prefix plus
seeded random bits. UUIDv4 and the primary UUIDv7 run arrive in the same ascending
generator sequence, so UUIDv7's timestamp component increases by one millisecond
per row; a third v7
table uses deterministic random arrival as a control. Every bulk and every set of
100 one-row post-load inserts begins after an explicit `CHECKPOINT`.

The time-query matrix records the GIN-only unpartitioned table, the same table
after adding `(created_at DESC, id DESC)`, the partition parent, and newest-first
chunked partition scans. Raw plans are preceded by `QUERY_CASE` labels.

The script is intentionally a measurement harness, not a production migration.
