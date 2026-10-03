"""Recompute the SeaweedFS metadata-subscription results (report §3) offline.

Inputs are the published logs under results/subscribe/<exp>/: events.jsonl.gz
(what the subscriber persisted) and writes.jsonl.gz (each acknowledged PUT with
its completion time). No storage service is needed.

Usage: python3 subscribe_summary.py [results/subscribe]
"""
import collections
import gzip
import json
import os
import sys

R = sys.argv[1] if len(sys.argv) > 1 else "results/subscribe"

EXPERIMENTS = [
    ("e1", "e1/", "订阅方 kill -9 后续订（400 个，20ms 间隔）"),
    ("e2", "e2/", "filer 优雅重启（订阅方在线，600 个）"),
    ("e2b", "e2b/", "filer kill -9（订阅方在线，600 个）"),
    ("e2c", "e2c/", "订阅方离线 + filer kill -9（写完立即硬杀，200 个）"),
    ("e6", "e6/", "docker compose restart（10s 宽限被强杀，100 个）"),
    ("e7", "e7/", "4 路并发写入后 filer 停机 120s 宽限、事后回放，回退窗口 + 按身份去重（100 个；延迟为回放时刻，无意义）"),
    ("e4", "e4/", "1 万突发（c=32，4KB）"),
]


def load(path):
    if not os.path.exists(path):
        return []
    with gzip.open(path, "rt") as f:
        return [json.loads(line) for line in f if line.strip()]


def pct(values, p):
    if not values:
        return None
    values = sorted(values)
    return values[min(len(values) - 1, int(p * len(values)))]


def summarize(exp, prefix):
    events = [e for e in load(f"{R}/{exp}/events.jsonl.gz") if e["key"].startswith(prefix)]
    writes = [w for w in load(f"{R}/{exp}/writes.jsonl.gz") if w.get("ok")]
    first = {}
    for e in events:
        if e["type"] != "delete" and e["key"] not in first:
            first[e["key"]] = e
    ids = collections.Counter((e["ts_ns"], e["type"], e["key"]) for e in events)
    dups = sum(n - 1 for n in ids.values())
    inversions = sum(1 for a, b in zip(events, events[1:]) if b["ts_ns"] < a["ts_ns"])
    lags = [(first[w["key"]]["recv_ns"] - w["done_ns"]) / 1e6 for w in writes if w["key"] in first]
    missing = sum(1 for w in writes if w["key"] not in first)
    return {
        "acked_writes": len(writes),
        "delivered": len(writes) - missing,
        "missing": missing,
        "duplicates": dups,
        "ts_inversions": inversions,
        "lag_p50_ms": None if not lags else round(pct(lags, 0.5), 1),
        "lag_p99_ms": None if not lags else round(pct(lags, 0.99), 1),
    }


print("| 实验 | 说明 | 已确认写入 | 收到事件 | 缺失 | 重复 | 时间戳倒退 | 写入确认→收到 p50 / p99 (ms) |")
print("|---|---|---|---|---|---|---|---|")
for exp, prefix, desc in EXPERIMENTS:
    s = summarize(exp, prefix)
    lag = "—" if s["lag_p50_ms"] is None else f"{s['lag_p50_ms']} / {s['lag_p99_ms']}"
    print(f"| {exp} | {desc} | {s['acked_writes']} | {s['delivered']} | {s['missing']} | "
          f"{s['duplicates']} | {s['ts_inversions']} | {lag} |")

# e3b: concurrent put/delete race. The final-state comparison against a live
# ListObjects was done at run time; the truth listing was not saved, so only the
# event-side facts are recomputable here.
ev = load(f"{R}/e3b/events.jsonl.gz")
state = {}
for e in ev:
    if e["type"] == "delete":
        state.pop(e["key"], None)
    else:
        state[e["key"]] = e["size"]
types = collections.Counter(e["type"] for e in ev)
print(f"\ne3b（8 路并发对 50 个 key 做 4000 次 put/delete）：事件 {len(ev)} 条 {dict(types)}；"
      f"按事件顺序重放的终态对象数 {len(state)}。"
      f"（与存储真值的逐对象比对为现场执行，真值列表未留存。）")

# purge: resubscribe from a cursor older than the purge.
ev = load(f"{R}/purge/events.jsonl.gz")
a = sum(1 for e in ev if e["key"].startswith("purgeA/"))
b = sum(1 for e in ev if e["key"].startswith("purgeB/"))
print(f"purge（fs.log.purge -daysAgo 0 后，从清理前的游标续订）：清理前写入的 purgeA 收到 {a}/100，"
      f"清理后写入的 purgeB 收到 {b}/100；订阅日志无任何报错（见 purge/sub.log）。")
