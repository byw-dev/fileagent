#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"
mkdir -p tmp/crash   # outputs go to tmp/; copy to results/ only to publish evidence
ready(){ until curl -s -o /dev/null -w '%{http_code}' --max-time 2 "http://$1/probe-grant" | grep -qE '^(200|403|404)$'; do sleep 1; done; sleep 3; }
count(){ python3 -c "
import json,sys
ks={json.loads(l)['key'] for l in open('$1') if l.strip() and json.loads(l)['event'].startswith('s3:ObjectCreated') and json.loads(l)['key'].startswith('$2')}
print(len(ks))" 2>/dev/null || echo 0; }
waitfor(){ local f=$1 p=$2 n=$3 lim=$4 t0=$(date +%s); while [ $(count $f $p) -lt $n ] && [ $(( $(date +%s)-t0 )) -lt $lim ]; do sleep 2; done; echo "$(count $f $p)/$n after $(( $(date +%s)-t0 ))s"; }
for spec in minio:19100:minio:minio juicefs:19400:juicefs:jfs; do IFS=: read t port dir svc <<< "$spec"; ep=localhost:$port; c=$(docker compose -f $dir/compose.yml ps -q $svc); ts=$(date +%s)
  f=tmp/crash/$t-b1.jsonl; : > $f; p=crash1-$ts/
  ./bin/sub write -s3 $ep -bucket probe-grant -prefix $p -n 200 -c 8 -out /dev/null | sed "s/^/  $t B1 /"; sleep 3
  docker kill -s KILL $c >/dev/null; sleep 1; docker start $c >/dev/null; ready $ep
  ./bin/sub sink -out $f 2>/dev/null & s=$!; echo "  $t B1 (sink down during writes, then storage SIGKILL): $(waitfor $f $p 200 150)"; kill $s; wait $s 2>/dev/null
  f=tmp/crash/$t-b2.jsonl; : > $f; p=crash2-$ts/
  ./bin/sub sink -out $f 2>/dev/null & s=$!; sleep 1
  ./bin/sub write -s3 $ep -bucket probe-grant -prefix $p -n 300 -c 16 -out /dev/null | sed "s/^/  $t B2 /"
  docker kill -s KILL $c >/dev/null; echo "  $t B2 delivered before kill: $(count $f $p)/300"; sleep 1; docker start $c >/dev/null; ready $ep
  echo "  $t B2 (sink up, storage SIGKILL right after writes): $(waitfor $f $p 300 150)"; kill $s; wait $s 2>/dev/null
done
