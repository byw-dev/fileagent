"""Median-of-runs summary for tier-4 benchmark + cold-IO.

Usage: python3 aggregate.py [bench_dir]   (default: results/bench, the published evidence;
pass tmp/bench to summarise a fresh run).
"""
import json, os, statistics as st, collections, sys
D = sys.argv[1] if len(sys.argv) > 1 else 'results/bench'
runs = {1: f'{D}/results.jsonl', 2: f'{D}/run2.jsonl', 3: f'{D}/run3.jsonl'}
T = ['minio', 'seaweedfs-nowh', 'juicefs']
vals = collections.defaultdict(list)
for run, path in runs.items():
    if not os.path.exists(path):
        continue
    if True:
        for l in open(path):
            r = json.loads(l)
            if r['target'] not in T: continue
            tot = r.get('to') or r.get('total')
            for k, v in r.items():
                if isinstance(v, (int, float)) and not isinstance(v, bool):
                    vals[(r['target'], r['op'], tot, k)].append(v)
def m(t, op, tot, k):
    v = vals.get((t, op, tot, k))
    if not v: return None, None, 0
    med = st.median(v); spread = (max(v) - min(v)) / med * 100 if med else 0
    return med, spread, len(v)
metrics = [("写入 obj/s", "load", "obj_per_s", 0), ("写入 p99 ms", "load", "put_p99_ms", 1),
           ("全量列举 obj/s", "list_recursive", "obj_per_s", 0), ("叶目录 p50 ms", "list_leaf", "p50_ms", 1),
           ("叶目录 p99 ms", "list_leaf", "p99_ms", 1), ("StatObject ops/s", "stat_random", "ops_per_s", 0),
           ("StatObject p99 ms", "stat_random", "p99_ms", 1), ("filer 直扫 obj/s", "filer_grpc_walk", "obj_per_s", 0)]
for tot in [100000, 500000, 1000000]:
    print(f"\n### {tot:,} 对象（中位数 ±三轮极差%，n=轮数）")
    print("| 指标 | " + " | ".join(T) + " |"); print("|---|" + "---|" * len(T))
    for name, op, k, d in metrics:
        cells = []
        for t in T:
            med, sp, n = m(t, op, tot, k)
            cells.append("—" if med is None else f"{med:,.{d}f} ±{sp:.0f}% (n={n})")
        if any(c != "—" for c in cells): print(f"| {name} | " + " | ".join(cells) + " |")
io = collections.defaultdict(list)
for l in open(f'{D}/io-cold.jsonl'):
    r = json.loads(l)
    if r['target'] in T: io[(r['target'], r['step'])].append(r)
print("\n### 冷缓存读盘（每千对象读盘次数：中位数 [各轮]）")
print("| 操作 | " + " | ".join(T) + " |"); print("|---|" + "---|" * len(T))
for step, name in [("list", "全量列举 1M"), ("leaf", "叶目录列举"), ("filer", "filer 直扫")]:
    cells = []
    for t in T:
        v = [r['reads_per_1k_obj'] for r in io.get((t, step), [])]
        cells.append("—" if not v else f"{st.median(v):,.1f} {v}")
    print(f"| {name} | " + " | ".join(cells) + " |")

print("\n### 冷缓存读取总量（MB：中位数 [各轮]）")
print("| 操作 | " + " | ".join(T) + " |"); print("|---|" + "---|" * len(T))
for step, name in [("list", "全量列举 1M"), ("leaf", "叶目录列举"), ("filer", "filer 直扫")]:
    cells = []
    for t in T:
        v = [r['read_MB'] for r in io.get((t, step), [])]
        cells.append("—" if not v else f"{st.median(v):,.1f} {v}")
    print(f"| {name} | " + " | ".join(cells) + " |")
print("\n### 占用（100 万对象，各轮）：内存 / 数据 MB / 元数据 MB")
for t in T:
    fp = []
    for run, path in runs.items():
        if not os.path.exists(path): continue
        for l in open(path):
            r = json.loads(l)
            if r['target'] == t and r['op'] == 'footprint' and r['total'] == 1000000:
                fp.append(f"{r['mem']} / {r['data_kb'] // 1024} / {r['meta_kb'] // 1024}")
    print(f"- {t}: " + "; ".join(fp))
