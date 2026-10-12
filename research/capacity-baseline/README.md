# File index capacity baseline

Run from the repository root with Docker available:

```sh
bash research/capacity-baseline/run.sh
```

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

The seven-versus-five `file_entries` index comparison is run separately by
`index-set.sql` using UUID-typed bucket/org/type/agent columns. The compact
int4-key proxy in `benchmark.sql` is auxiliary and is not used for the report's
index-set figures.

The PK comparison builds UUIDv4 with explicit version/variant bits from a seeded
MD5 stream and builds UUIDv7 in SQL from a 2023 epoch-millisecond prefix plus
seeded random bits. Both sets use the same deterministic `ORDER BY md5(g::text)` row
arrival order after `CHECKPOINT`; this measures randomized arrival, not the
chronological-arrival best case for UUIDv7.

The script is intentionally a measurement harness, not a production migration.
