#!/usr/bin/env bash
set -uo pipefail
cd "$(dirname "$0")"
rss(){ docker exec $1 sh -c 'grep -E "VmRSS|VmHWM" /proc/1/status' | tr -s ' \n\t' ' '; }
for v in seaweedfs seaweedfs-nowh; do
  dc="docker compose -f $v/compose.yml"
  $dc down -v >/dev/null 2>&1; sleep 2
  $dc up -d 2>&1 | grep -iE "error" ; t0=$(date +%s)
  until curl -s -o /dev/null -w '%{http_code}' --max-time 2 localhost:19200/bench | grep -qE '^(200|403|404)$'; do sleep 1; [ $(( $(date +%s)-t0 )) -gt 60 ] && { echo "$v not ready"; exit 1; }; done; sleep 3
  c=$($dc ps -q seaweed); echo "== $v start: $(rss $c)"
  ./bin/sub load -s3 localhost:19200 -target $v -start 0 -end 200000 -c 32 | cut -c1-110
  echo "   after 200k: $(rss $c)"; sleep 30; echo "   +30s idle:  $(rss $c)"
  $dc down -v >/dev/null 2>&1; sleep 2
done
