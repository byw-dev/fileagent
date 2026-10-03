"""HDD extrapolation model for full listings (report Tier 4). A MODEL, not a measurement.

Calibration (external, not in this repo): minio-inventory measured a full scan of
a production bucket on a single-disk HDD MinIO — ~1.70M objects, 5h31m,
~86 objects/s aggregate.

Method: MinIO's measured cold-cache disk reads per listed object (results/bench/
io-cold.jsonl) turn that wall time into an effective random-read rate for that
disk; every target is then scaled by its own reads-per-object. The MinIO rows are
therefore CALIBRATION, not independent validation — the information is in the
ratio between implementations. Large sequential reads are treated like random
ones, so SeaweedFS / JuiceFS are pessimistic.

Usage: python3 hdd_model.py [results/bench]
"""
import json
import statistics
import sys

D = sys.argv[1] if len(sys.argv) > 1 else "results/bench"
BASE_OBJECTS, BASE_SECONDS = 1_700_000, 5 * 3600 + 31 * 60

reads = {}
for line in open(f"{D}/io-cold.jsonl"):
    r = json.loads(line)
    if r["step"] == "list":
        reads.setdefault(r["target"], []).append(r["reads_per_1k_obj"] / 1000)
per_obj = {t: statistics.median(v) for t, v in reads.items()}
rate = BASE_OBJECTS * per_obj["minio"] / BASE_SECONDS  # effective random reads / s


def fmt(seconds):
    if seconds >= 86400:
        return f"{seconds / 86400:.1f} 天"
    if seconds >= 3600:
        return f"{seconds / 3600:.1f} 小时"
    return f"{seconds / 60:.1f} 分钟"


print(f"标定：{BASE_OBJECTS:,} 对象 / {BASE_SECONDS}s × MinIO {per_obj['minio']:.4f} 次读盘/对象"
      f" → 有效随机读 ≈ {rate:.1f} 次/秒\n")
print("| 全量列举 | " + " | ".join(per_obj) + " |")
print("|---|" + "---|" * len(per_obj))
for n, label in [(BASE_OBJECTS, "170 万（标定点）"), (100_000_000, "1 亿")]:
    print(f"| {label} | " + " | ".join(fmt(n * per_obj[t] / rate) for t in per_obj) + " |")
daily = 100_000_000 / 30
print(f"| 1 亿对象、封存分片每 30 天轮转复核（每天 {daily / 1e6:.2f}M 次列举） | "
      + " | ".join(f"{daily * per_obj[t] / rate / 3600:.2f} 小时/天" for t in per_obj) + " |")
