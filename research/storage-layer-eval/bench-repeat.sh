#!/usr/bin/env bash
# Two more fresh runs per target (run 1 = tmp/bench/results.jsonl) + cold-IO at 1M.
set -uo pipefail
cd "$(dirname "$0")"
mkdir -p tmp/bench   # outputs go to tmp/; copy to results/ only to publish evidence
for run in 2 3; do
  for spec in minio:19100 seaweedfs-nowh:19200 juicefs:19400; do IFS=: read t port <<< "$spec"
    OUT=$PWD/tmp/bench/run$run.jsonl ./run-bench.sh $t "100000 500000 1000000" > tmp/bench/$t-run$run.log 2>&1
    extra=""; [ $t = seaweedfs-nowh ] && extra="-filer localhost:19288"
    for s in list leaf; do ./io-cold.sh $t localhost:$port $s >> tmp/bench/io-cold-run$run.log 2>&1; done
    [ $t = seaweedfs-nowh ] && ./io-cold.sh $t localhost:$port filer $extra >> tmp/bench/io-cold-run$run.log 2>&1
    echo "run$run $t done $(date -u +%T)"
  done
done
