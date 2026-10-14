#!/usr/bin/env bash
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
cd "$ROOT"
export TMPDIR="$PWD/bin"
mkdir -p "$TMPDIR" research/capacity-baseline/results
RESULTS="$PWD/research/capacity-baseline/results"
CONTAINER=fileagent-b4-pg
VOLUME=fileagent-b4-pg-data
if docker container inspect "$CONTAINER" >/dev/null 2>&1; then
  echo "Refusing to reuse existing container $CONTAINER" >&2; exit 2
fi
if docker volume inspect "$VOLUME" >/dev/null 2>&1; then
  echo "Refusing to reuse existing volume $VOLUME" >&2; exit 2
fi
cleanup() {
  docker rm -f "$CONTAINER" >/dev/null 2>&1 || true
  docker volume rm "$VOLUME" >/dev/null 2>&1 || true
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
docker volume create "$VOLUME" >/dev/null
docker run -d --name "$CONTAINER" -p 15432:5432 -v "$VOLUME:/var/lib/postgresql/data" \
  -e POSTGRES_PASSWORD=baseline -e POSTGRES_DB=baseline postgres:15-alpine >/dev/null
until docker exec "$CONTAINER" pg_isready -U postgres -d baseline >/dev/null 2>&1; do sleep 1; done
for scale in 1000000 5000000; do
  echo "Running scale=$scale"
  docker exec -i "$CONTAINER" psql -X -U postgres -d baseline -v scale="$scale" -f - \
    < research/capacity-baseline/benchmark.sql > "$RESULTS/scale-$scale.log" 2>&1
done
for scale in 1000000 5000000; do
  echo "Running UUID-typed file_entries index set at scale=$scale"
  docker exec -i "$CONTAINER" psql -X -U postgres -d baseline -v scale="$scale" -f - \
    < research/capacity-baseline/index-set.sql > "$RESULTS/index-set-$scale.log" 2>&1
done
echo "Results: research/capacity-baseline/results/"
