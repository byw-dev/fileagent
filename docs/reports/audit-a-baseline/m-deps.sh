#!/usr/bin/env bash
# m-deps.sh — 逐个停掉依赖，观察 Control Plane 的启动行为（链路 ①）
#
# ⚠️ 初版硬编码了作者的绝对路径、私有 scratchpad 与 ./bin/controlplane-bundle，
#    且不校验前提。后果（PR #115 评审实测）：在一台没起 dev 环境的机器上运行，
#    四轮**全部**因为 PostgreSQL 不存在而报 "database migration failed"，
#    但循环标签仍分别写着 redis/nats/minio —— 产出一份**看起来像结论的假矩阵**。
#    现在改为：路径自推导 + 前置断言 + **交叉校验硬失败** + 中断恢复 trap。
#    ⚠️ 二次修订（PR #115 复评）：初版的交叉校验只 echo 警告不退出，脚本头却写着
#    「失败即退出」——注释在撒谎。现在每一轮都断言「预期结果 + 日志确实提到本轮依赖」，
#    不符即 exit 非零。
set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../.." && pwd)"
cd "$REPO_ROOT"

CP_BIN="${CP_BIN:-bin/controlplane}"
OUT="${OUT:-$(mktemp -d "${TMPDIR:-/tmp}/aud-deps.XXXXXX")}"
PROJECT="${COMPOSE_PROJECT_NAME:-deploy}"
PREFIX="${FA_CONTAINER_PREFIX:-fileagent-dev}"

die() { echo "前提不满足：$*" >&2; exit 1; }

# ── 前置断言：任何一条不满足，结果都是假的 ────────────────────────────────
[ -x "$CP_BIN" ] || die "找不到可执行的 ${CP_BIN}（先 make build 或 make bundle；可用 CP_BIN= 覆盖）"
[ -f deploy/config/controlplane.env ] || die "缺 deploy/config/controlplane.env（从 controlplane/.env.example 复制）"
for dep in postgres redis nats minio; do
  docker ps --format '{{.Names}}' | grep -qx "${PREFIX}-${dep}" \
    || die "容器 ${PREFIX}-${dep} 未在运行。本脚本测的是「把一个健康环境的某一个依赖停掉」，
       四个依赖必须先全部就绪，否则每一轮量到的都是同一个缺失依赖的错误。"
done
# 端口占用会让 CP 因 address-in-use 退出，被误读成「该依赖导致 fail-fast」
if command -v lsof >/dev/null 2>&1; then
  lsof -nP -iTCP:8080 -sTCP:LISTEN >/dev/null 2>&1 && die "8080 已被占用（另一个 CP？）——会把 address-in-use 误判成依赖缺失"
fi

# 中断恢复：停掉的容器必须还回去，否则会留下一个半残的共享环境
STOPPED=""
CPPID=""
restore() {
  [ -n "$CPPID" ] && kill "$CPPID" 2>/dev/null
  for d in $STOPPED; do docker start "${PREFIX}-${d}" >/dev/null 2>&1 || true; done
}
trap restore EXIT INT TERM

echo "前提检查通过；输出目录：$OUT"

for dep in postgres redis nats minio; do
  echo "############ DEP DOWN: $dep ############"
  docker stop "${PREFIX}-${dep}" >/dev/null || die "无法停止 ${PREFIX}-${dep}"
  STOPPED="$STOPPED $dep"
  ( set -a; . deploy/config/controlplane.env; set +a
    export MINIO_PUBLIC_ENDPOINT="${MINIO_PUBLIC_ENDPOINT:-127.0.0.1:9000}"
    "$CP_BIN" > "$OUT/cp-no-$dep.log" 2>&1 &
    echo $! > "$OUT/cp.pid"
    sleep 12
    if kill -0 "$(cat "$OUT/cp.pid")" 2>/dev/null; then
      echo "alive" > "$OUT/verdict-$dep"
      echo "  healthz: $(curl -s -o /dev/null -w '%{http_code}' http://127.0.0.1:8080/healthz)"
      kill "$(cat "$OUT/cp.pid")" 2>/dev/null
    else
      echo "exited" > "$OUT/verdict-$dep"
    fi
  )
  VERDICT="$(cat "$OUT/verdict-$dep")"
  echo "RESULT: CP $([ "$VERDICT" = exited ] && echo '已退出（fail-fast）' || echo '仍在运行（**没有** fail-fast）')"
  echo "--- 末行日志 ---"
  grep -vE '^\[GIN-debug\]' "$OUT/cp-no-$dep.log" | tail -2

  # ⚠️ 交叉校验必须**硬失败**。初版只 echo 警告，于是即便四轮报的都是同一个
  #    PostgreSQL 错误，脚本仍打印「全部完成」——正是它当初产出假矩阵的原因。
  case "$dep" in
    postgres|redis|nats)
      [ "$VERDICT" = "exited" ] || die "预期 $dep 缺失会让 CP fail-fast，实际 CP 仍在运行——结果不可信"
      if   [ "$dep" = postgres ]; then pat='5432|migration|database'
      elif [ "$dep" = redis ];    then pat='6379|redis'
      else                             pat='4222|nats'
      fi
      grep -qiE "$pat" "$OUT/cp-no-$dep.log" \
        || die "CP 确实退出了，但日志没提到 ${dep}（匹配 /$pat/）——失败原因可能来自别的依赖，这正是假矩阵的特征"
      ;;
    minio)
      # MinIO 不 fail-fast 是链路 ① 的核心发现，所以这里断言的是「仍在运行」。
      # 若某天它变成 fail-fast，这条会红——那说明发现过期了，应当红。
      [ "$VERDICT" = "alive" ] || die "预期 MinIO 缺失时 CP 不 fail-fast，实际 CP 退出了——链路 ① 的结论已过期，请重新审计"
      ;;
  esac
  docker start "${PREFIX}-${dep}" >/dev/null || die "无法恢复 ${PREFIX}-${dep}"
  STOPPED="$(echo "$STOPPED" | sed "s/ $dep//")"
  sleep 6
  echo
done
echo "全部完成；日志在 $OUT"
