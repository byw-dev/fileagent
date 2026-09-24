#!/usr/bin/env bash
# m-deps.sh — 逐个停掉依赖，观察 Control Plane 的启动行为（链路 ①）
#
# ⚠️ 初版硬编码了作者的绝对路径、私有 scratchpad 与 ./bin/controlplane-bundle，
#    且不校验前提。后果（PR #115 评审实测）：在一台没起 dev 环境的机器上运行，
#    四轮**全部**因为 PostgreSQL 不存在而报 "database migration failed"，
#    但循环标签仍分别写着 redis/nats/minio —— 产出一份**看起来像结论的假矩阵**。
#    现在改为：路径自推导 + 前置断言 + 失败即退出。
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO_ROOT"

CP_BIN="${CP_BIN:-bin/controlplane}"
OUT="${OUT:-$(mktemp -d "${TMPDIR:-/tmp}/aud-deps.XXXXXX")}"
PROJECT="${COMPOSE_PROJECT_NAME:-deploy}"
PREFIX="${FA_CONTAINER_PREFIX:-fileagent-dev}"

die() { echo "前提不满足：$*" >&2; exit 1; }

# ── 前置断言：任何一条不满足，结果都是假的 ────────────────────────────────
[ -x "$CP_BIN" ] || die "找不到可执行的 $CP_BIN（先 make build 或 make bundle；可用 CP_BIN= 覆盖）"
[ -f deploy/config/controlplane.env ] || die "缺 deploy/config/controlplane.env（从 controlplane/.env.example 复制）"
for dep in postgres redis nats minio; do
  docker ps --format '{{.Names}}' | grep -qx "${PREFIX}-${dep}" \
    || die "容器 ${PREFIX}-${dep} 未在运行。本脚本测的是「把一个健康环境的某一个依赖停掉」，
       四个依赖必须先全部就绪，否则每一轮量到的都是同一个缺失依赖的错误。"
done
echo "前提检查通过；输出目录：$OUT"

for dep in postgres redis nats minio; do
  echo "############ DEP DOWN: $dep ############"
  docker stop "${PREFIX}-${dep}" >/dev/null || die "无法停止 ${PREFIX}-${dep}"
  ( set -a; . deploy/config/controlplane.env; set +a
    export MINIO_PUBLIC_ENDPOINT="${MINIO_PUBLIC_ENDPOINT:-127.0.0.1:9000}"
    "$CP_BIN" > "$OUT/cp-no-$dep.log" 2>&1 &
    CPPID=$!
    sleep 12
    if kill -0 $CPPID 2>/dev/null; then
      echo "RESULT: CP 仍在运行（**没有** fail-fast）"
      echo "  healthz: $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/healthz)"
      kill $CPPID 2>/dev/null
    else
      echo "RESULT: CP 已退出（fail-fast）"
    fi
  )
  echo "--- 末行日志 ---"
  grep -vE '^\[GIN-debug\]' "$OUT/cp-no-$dep.log" | tail -2
  # 交叉校验：报出来的失败原因必须真的指向本轮停掉的依赖，否则是假矩阵
  case "$dep" in
    postgres) grep -qi '5432\|migration\|database' "$OUT/cp-no-$dep.log" || echo "⚠️ 日志未提及 PostgreSQL，结果存疑" ;;
    redis)    grep -qi '6379\|redis'     "$OUT/cp-no-$dep.log" || echo "⚠️ 日志未提及 Redis，结果存疑" ;;
    nats)     grep -qi '4222\|nats'      "$OUT/cp-no-$dep.log" || echo "⚠️ 日志未提及 NATS，结果存疑" ;;
    minio)    : ;;  # MinIO 不 fail-fast，日志里本就不会提它——这正是链路 ① 的发现
  esac
  docker start "${PREFIX}-${dep}" >/dev/null || die "无法恢复 ${PREFIX}-${dep}"
  sleep 6
  echo
done
echo "全部完成；日志在 $OUT"
