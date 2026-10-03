#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"
c=se-seaweed-nowh-seaweed-1
WS(){ docker exec -i $c sh -c "echo '$1' | weed shell -master=localhost:9333" 2>&1 | grep -vE '^\s*$|^>|master'; }
d=subexp/purge; rm -rf $d; mkdir -p $d
python3 -c "import time;print(time.time_ns()-5*10**9)" > $d/c0.txt
./bin/sub write -s3 localhost:19200 -prefix purgeA/ -n 100 -out /dev/null; sleep 70
echo "== log files before purge:"; WS "fs.ls /topics/.system/log"; WS "fs.ls /topics/.system/log/$(date -u +%Y-%m-%d)" | wc -l
echo "== purge -daysAgo 0:"; WS "fs.log.purge -daysAgo 0 -v"
echo "== after purge:"; WS "fs.ls /topics/.system/log"
./bin/sub write -s3 localhost:19200 -prefix purgeB/ -n 100 -out /dev/null; sleep 70
echo "== after B + flush:"; WS "fs.ls /topics/.system/log"; WS "fs.ls /topics/.system/log/$(date -u +%Y-%m-%d)"
docker stop -t 120 $c >/dev/null; docker start $c >/dev/null
until curl -s -o /dev/null -w '%{http_code}' --max-time 2 localhost:19200/probe-grant | grep -qE '^(200|403|404)$'; do sleep 1; done; sleep 4
( cd $d && ../../bin/sub sub -since $(cat c0.txt) -cursor cur.txt -out ev.jsonl 2>sub.log & p=$!; sleep 15; kill -TERM $p; wait $p 2>/dev/null )
echo "== resubscribe from pre-purge cursor:"; grep -v DROPPED $d/sub.log
for g in purgeA purgeB; do echo "$g events: $(grep -c "\"key\":\"$g/" $d/ev.jsonl)"; done
