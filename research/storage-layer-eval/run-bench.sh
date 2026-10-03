#!/usr/bin/env bash
# Tier-4 benchmark driver: fresh env per target, load in steps, measure at each point.
# usage: run-bench.sh <minio|seaweedfs|juicefs> "<points>"   e.g. "100000 500000 1000000"
set -uo pipefail
here=$(cd "$(dirname "$0")" && pwd); cd "$here"
export TMPDIR=$here/tmp
t=$1; points=$2; out=${OUT:-$here/tmp/bench/results.jsonl}; mkdir -p "$(dirname "$out")"
case $t in
  minio)     ep=localhost:19100; svc=minio;   extra="" ;;
  seaweedfs|seaweedfs-nowh) ep=localhost:19200; svc=seaweed; extra="-filer localhost:19288" ;;
  juicefs)   ep=localhost:19400; svc=jfs;     extra="" ;;
esac
dc="docker compose -f $t/compose.yml"
$dc down -v >/dev/null 2>&1; $dc up -d >/dev/null 2>&1
until curl -s -o /dev/null -w '%{http_code}' "http://$ep/bench" 2>/dev/null | grep -qE '^(200|403|404)$'; do sleep 1; done; sleep 3
footprint() {
  local c; c=$($dc ps -q $svc)
  local mem; mem=$(docker stats --no-stream --format '{{.MemUsage}}' "$c" | cut -d/ -f1 | tr -d ' ')
  local disk
  case $t in
    minio)     disk=$(docker exec "$c" du -sk /data | cut -f1) ;;
    seaweedfs|seaweedfs-nowh) disk=$(docker exec "$c" du -sk /data | cut -f1); meta=$(docker exec "$c" du -sk /data/filerldb2 | cut -f1) ;;
    juicefs)   disk=$(docker exec "$c" du -sk /jfsdata | cut -f1)
               meta=$($dc exec -T meta psql -U jfs -d jfs -tAc "select pg_database_size('jfs')/1024") ;;
  esac
  printf '{"target":"%s","op":"footprint","total":%s,"mem":"%s","data_kb":%s,"meta_kb":%s}\n' "$t" "$1" "$mem" "$disk" "${meta:-0}" | tee -a "$out"
}
prev=0
for p in $points; do
  ./bin/sub load -s3 $ep -target $t -start $prev -end $p -c 32 -out "$out"
  sleep 5
  footprint $p
  ./bin/sub measure -s3 $ep -target $t -total $p -out "$out" $extra
  prev=$p
done
echo "DONE $t"
