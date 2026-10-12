#!/usr/bin/env python3
"""Analyze watch logs from `eventsource watch` (B2 experiments).

usage: analyze.py load <watch.jsonl> [writes.jsonl]
  - heartbeat claim check: an event received AFTER a heartbeat must have ts_ns > that heartbeat's ts_ns
    (a heartbeat on the local stream claims "everything at or below me was already sent")
  - ts inversion: lateness = (max ts_ns received so far) - ts_ns, for events arriving below the running max
  - completeness: every successful write key appears as an object event (create/update)
"""
import json, sys

def pct(xs, p):
    if not xs: return 0
    xs = sorted(xs); return xs[min(len(xs)-1, int(len(xs)*p))]

def load(watch, writes=None):
    recs = [json.loads(l) for l in open(watch)]
    ev = [r for r in recs if r['kind'] == 'event']
    hb = [r for r in recs if r['kind'] == 'heartbeat']
    claim = 0; violations = []; runmax = 0; late = []
    for r in recs:
        if r['kind'] == 'heartbeat':
            claim = max(claim, r['ts_ns'])
        elif r['kind'] == 'event':
            if r['ts_ns'] <= claim:
                violations.append((r['ts_ns'], claim))
            if r['ts_ns'] < runmax:
                late.append(runmax - r['ts_ns'])
            runmax = max(runmax, r['ts_ns'])
    out = {
        'records': len(recs), 'events': len(ev), 'heartbeats': len(hb),
        'claim_violations': len(violations),
        'max_violation_ms': max((c - t for t, c in violations), default=0) / 1e6,
        'inverted_events': len(late),
        'inverted_pct': round(100 * len(late) / max(1, len(ev)), 3),
        'lateness_ms_p50': pct(late, .5) / 1e6, 'lateness_ms_p99': pct(late, .99) / 1e6,
        'lateness_ms_max': max(late, default=0) / 1e6,
    }
    # heartbeat freshness: how far behind receive time is the claim
    if hb:
        lag = [(h['recv_ns'] - h['ts_ns']) / 1e6 for h in hb]
        out['heartbeat_claim_lag_ms_max'] = round(max(lag), 1)
        gaps = [(b['recv_ns'] - a['recv_ns']) / 1e9 for a, b in zip(hb, hb[1:])]
        out['heartbeat_gap_s_max'] = round(max(gaps, default=0), 1)
    if writes:
        ok = {json.loads(l)['key'] for l in open(writes) if json.loads(l)['ok']}
        seen = set()
        for r in ev:
            name = r.get('new') or ''
            if name and not r.get('new_is_dir'):
                d = r['dir'].split('/', 3)
                if len(d) >= 4:
                    seen.add(d[3] + '/' + name)
        out['writes_ok'] = len(ok); out['writes_missing_events'] = len(ok - seen)
    return out

if __name__ == '__main__':
    if sys.argv[1] == 'load':
        print(json.dumps(load(*sys.argv[2:]), ensure_ascii=False))
