#!/usr/bin/env bash
# Cold-cache disk-read cost per listing step: restart target (drop in-process caches),
# drop VM page cache, then count block-device reads during one measure step.
# usage: io-cold.sh <target> <endpoint> <step> [extra measure flags]
set -uo pipefail
cd "$(dirname "$0")"
t=$1; ep=$2; step=$3; shift 3
dev=vdb1
# alpine:latest as pulled for the published run (2026-10-03), pinned by digest.
ALPINE=alpine@sha256:294b683cb724975bec92580e1e685676bd4b50bda910ddb8c51d4cabeaec77e6
ds(){ docker run --rm --privileged $ALPINE awk -v d=$dev '$3==d{print $4, $6}' /proc/diskstats; }
if [ "$t" = juicefs ]; then
  # Restart the metadata DB first and wait for it: the gateway exits fatally
  # if PG is unavailable while it starts or closes its session.
  docker compose -f "$t/compose.yml" stop jfs >/dev/null 2>&1
  docker compose -f "$t/compose.yml" restart meta >/dev/null 2>&1
  until docker compose -f "$t/compose.yml" exec -T meta pg_isready -U jfs >/dev/null 2>&1; do sleep 1; done
  docker compose -f "$t/compose.yml" start jfs >/dev/null 2>&1
else
  docker compose -f "$t/compose.yml" restart >/dev/null 2>&1
fi
until curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://$ep/bench" | grep -qE '^(200|403|404)$'; do sleep 1; done; sleep 5
docker run --rm --privileged $ALPINE sh -c 'sync; echo 3 > /proc/sys/vm/drop_caches'
read r0 s0 < <(ds)
res=$(./bin/sub measure -s3 "$ep" -target "$t" -total 1000000 -only "$step" "$@" | tail -1)
read r1 s1 < <(ds)
python3 - "$t" "$step" "$res" $((r1-r0)) $((s1-s0)) <<'PY'
import sys,json
t,step,res,reads,sect=sys.argv[1],sys.argv[2],json.loads(sys.argv[3]),int(sys.argv[4]),int(sys.argv[5])
n=res.get('listed') or (res.get('leaves',0)*res.get('avg_objects',0)) or res.get('n',0)
out={"target":t,"step":step,"objects":n,"secs":round(res.get('secs',0) or 0,2),"disk_reads":reads,"read_MB":round(sect*512/2**20,1),
     "reads_per_1k_obj":round(reads/n*1000,1) if n else None,"KB_read_per_obj":round(sect*512/1024/n,2) if n else None}
print(json.dumps(out,ensure_ascii=False))
import os; os.makedirs("tmp/bench", exist_ok=True); open("tmp/bench/io-cold.jsonl",'a').write(json.dumps(out)+"\n")
PY
