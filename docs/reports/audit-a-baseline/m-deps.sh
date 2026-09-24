#!/usr/bin/env bash
cd /Users/tsingye/workspace/fileagent
AUD=/private/tmp/claude-501/-Users-tsingye-workspace-fileagent/d9962603-aa79-4389-8c0b-081a99722502/scratchpad/aud
for dep in postgres redis nats minio; do
  echo "############ DEP DOWN: $dep ############"
  docker stop fileagent-dev-$dep >/dev/null 2>&1
  sleep 2
  ( set -a; . deploy/config/controlplane.env; set +a; export MINIO_PUBLIC_ENDPOINT=127.0.0.1:9000
    ./bin/controlplane-bundle > $AUD/cp-no-$dep.log 2>&1 &
    CPPID=$!
    sleep 12
    if kill -0 $CPPID 2>/dev/null; then
      echo "RESULT: CP still RUNNING after 12s (did NOT fail fast)"
      echo "  healthz: $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/healthz)"
      kill $CPPID 2>/dev/null
    else
      echo "RESULT: CP EXITED (fail-fast)"
    fi
  )
  echo "--- last log lines ---"
  grep -vE '^\[GIN-debug\]' $AUD/cp-no-$dep.log | tail -3
  docker start fileagent-dev-$dep >/dev/null 2>&1
  sleep 6
  echo
done
