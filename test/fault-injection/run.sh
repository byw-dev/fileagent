#!/usr/bin/env bash
# run.sh — filer 订阅链路故障注入测试（4 场景，隔离环境，仓库根 `test/fault-injection/`）。
#
# 验证目标：CP「订阅 SeaweedFS filer 事件 → S3 回读 → 入账」链路在故障下的最终一致。
# 脚本自起隔离环境（SeaweedFS 拓扑 + PG/Redis/NATS + CP，CP 经一个可杀 TCP 代理
# 订阅 filer），跑 4 个场景，断言 S3 存活集合与 PG 账本一致（等集合比较）：
#   场景 1  断流重连（杀代理），含「已入账后删除/覆盖」两组；
#   场景 2  filer 优雅重启（持续写入期间 docker compose restart）；
#   场景 3  CP 停机期间写入（重启后游标从持久化行恢复）；
#   场景 4  docker kill -s KILL 硬杀 filer（只记录、不要求通过）。
#
# 通过判据：FAILURES=0 且退出码 0（场景 4 只记录）。用法详见同目录 README.md，
# 常用开关：FI_ONLY=<n> 只跑某场景；FI_HANG_DEMO=1 有界调用自证（预期失败）。
#
# 隔离边界：compose 项目/容器名（fileagent-fi）与端口均与 smoke/dev 隔离；
# 被测二进制构建到 bin/tmp/fi/ 专有路径（不碰共享的 bin/controlplane）。
# 但 SeaweedFS 配置渲染写共享的 deploy/config/seaweedfs/{s3,iam}.json（compose
# 挂载路径写死，见 README）——不要与 smoke（FA_STORAGE=seaweedfs）或 dev 的
# SeaweedFS 在同一工作树里并发运行；端口固定，同一台机器同时只能跑一份 FI。
#
# 设计要点（评审轮次标签 F3/F6/F7/N4/N5/R3-x/R4-x 来自 PR #135 的评审轮次）：
#   F3  every group has an independent GENERATION PLAN (key|size|etag from the
#       actual upload); asserts verify against the plan (existence, status,
#       source, size, etag) — a vanished row or an empty list is a FAILURE,
#       never a pass; set-compare carries an expected-live-count guard.
#   F6  every external call (psql_q, s3_keys, aws, curl) is bounded via run_to
#       and fail-closed on error; FI_HANG_DEMO=1 injects one hanging query to
#       prove the bound (script exits within the deadline counting a failure).
#   F7  after every filer restart the script waits for the S3 gateway to
#       actually answer before continuing; put/delete retry and every failure
#       is recorded (no silent shrinkage of the expected set); delete stderr
#       is logged, not discarded.
set -uo pipefail

REPO="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO" || exit 1
OUT="bin/tmp/fi"
mkdir -p "$OUT"

# ── isolation ────────────────────────────────────────────────────────────────
export COMPOSE_PROJECT_NAME="fileagent-fi"
export FA_CONTAINER_PREFIX="fileagent-fi"
export FA_POSTGRES_PORT=25132 FA_REDIS_PORT=26179 FA_NATS_PORT=27122 FA_NATS_MONITOR_PORT=28122
export FA_SEAWEEDFS_MASTER_PORT=29334 FA_SEAWEEDFS_S3_PORT=28334
export FA_SEAWEEDFS_FILER_HTTP_PORT=28898 FA_SEAWEEDFS_FILER_GRPC_PORT=28899
CP_HTTP_PORT=28081; CP_GRPC_PORT=29091
PROXY_PORT=28990
export TMPDIR="$REPO/bin/tmp"

COMPOSE="docker compose -f deploy/docker-compose.dev.yml"
SWCOMPOSE="docker compose -f deploy/docker-compose.seaweedfs.yml"
FILER_CONTAINER="fileagent-fi-seaweed-filer"
FAILURES=0

# shellcheck source=/dev/null
source deploy/seaweedfs/env-defaults.sh

step()  { echo ""; echo "══ $* ══"; }
note()  { echo "  · $*"; }
ok()    { echo "  ✓ $*"; }
bad()   { echo "  ✗ $*"; FAILURES=$((FAILURES+1)); }
fatal() { echo "  ☠ FATAL: $*"; exit 1; }

# ── AWS CLI v2（B7：mc → aws cli v2，MinIO 生态不再依赖）────────────────────
# 私有配置隔离：config/credentials/aws-s3 封装全部落在本次运行 mktemp -d 新建的
# 私有目录里，绝不触碰开发者的 ~/.aws。两个身份：
#   swcp    — CP 身份（put 走它，与 agent/CP 运行时同源）
#   swadmin — 管理员身份（delete / list / 建桶）
# 重试取舍：数据操作 standard 重试 + max_attempts=8（与 mc/minio-go 的内置
# 瞬时错误容错等价；理由写在生成的 config 注释里）。**最多 8 次**，但每次
# 调用被 run_to 强制在 30s 截断（8 次退避不保证在 30s 内走完）；外层 FI 级
# 的 3 次重试不变——单个 key 最坏 ≈ 3×30s + 2×1s sleep ≈ 93s，再由各场景的
# 绝对预算硬截止。max_attempts=8 不可被环境覆盖：调用者 AWS_* 已按 F2 白名单
# 全部清除，FI_AWS_MAX_ATTEMPTS 也在清理之列（H2）——只有 wait_s3_ready 那
# 一次调用经 env 显式传入 1，追求秒退（网关未就绪时默认重试会把每次探测拖长）。
AWS_S3="http://localhost:${FA_SEAWEEDFS_S3_PORT}"
# F2（评审 r1）白名单：调用者环境里的 AWS_* 一律不得漏入——凭据/region 的
# 优先级高于 profile 文件，AWS_MAX_ATTEMPTS/AWS_RETRY_MODE 会覆盖 config 里
# 的重试配置（实测 AWS_MAX_ATTEMPTS=1 会把数据操作的 max_attempts=8 打回
# 1 次、场景 2 的问题复发），AWS_CA_BUNDLE/AWS_ENDPOINT_URL* 同理。先全部
# unset，再显式 export 本脚本需要的；封装脚本（aws-s3）内部再做同样清理，
# 保证 run_to / writer 子进程路径同样生效。H2（评审 r2）：FI_AWS_MAX_ATTEMPTS
# 同样会被封装映射成 AWS_MAX_ATTEMPTS，一并从调用者环境清除——它只允许经
# wait_s3_ready 那一次调用以 `env FI_AWS_MAX_ATTEMPTS=1` 显式传入。
for _aws_v in $(compgen -e); do
  case "$_aws_v" in AWS_*|AWS|FI_AWS_MAX_ATTEMPTS) unset "$_aws_v" ;; esac
done
# H1（评审 r2）：历史固定路径残留只清理、不再写入——bin/tmp/fi/aws（跨运行
# 保留的旧凭据目录）与 bin/tmp/fi/aws-s3（旧封装位置）。若被换成指向别处的
# 符号链接，只删链接本身（rm -f，不加 -r、不跟随）；普通目录/文件（上一轮
# FI 自建）直接删除。
if [ -L "$REPO/$OUT/aws" ]; then
  rm -f "$REPO/$OUT/aws"
elif [ -d "$REPO/$OUT/aws" ]; then
  rm -rf "$REPO/$OUT/aws"
fi
rm -f "$REPO/$OUT/aws-s3"
# H1：凭据目录每次运行 mktemp -d 新建——mktemp 建出的目录不会是既有链接、
# 权限即 0700，config/credentials/aws-s3 封装都写在里面，cleanup 整目录删除。
# 本脚本 set -uo pipefail 没有 -e：以下每一步显式 fail-closed。
AWS_DIR="$(mktemp -d "$REPO/$OUT/aws.XXXXXX")" \
  || fatal "创建私有 AWS 凭据目录失败（mktemp -d $REPO/$OUT/aws.XXXXXX）"
# AWS 块先于主 cleanup trap 定义；这个临时 trap 覆盖「凭据初始化自身 fatal」
# 的窗口，随后主 `trap cleanup EXIT` 会将其替换（cleanup 同样删除 AWS_DIR）。
trap 'rm -rf "${AWS_DIR:-}" 2>/dev/null' EXIT
chmod 700 "$AWS_DIR" || fatal "校正 AWS 凭据目录权限失败（$AWS_DIR）"
export AWS_CONFIG_FILE="$AWS_DIR/config" AWS_SHARED_CREDENTIALS_FILE="$AWS_DIR/credentials"
export AWS_PAGER="" AWS_EC2_METADATA_DISABLED=true
( umask 077; cat > "${AWS_CONFIG_FILE}" <<'AWSCFG'
[profile swcp]
region = us-east-1
s3 =
  addressing_style = path
retry_mode = standard
# max_attempts=8（standard 退避累计 ~30s）——与 mc/minio-go 的内置重试等价：
# 场景 2 的写入器本来就会横跨 filer 重启的 ~17s 停机窗口，mc 时代短暂连接
# 拒绝由 minio-go 内部重试吸收（FI 级 3 次重试从不触发）；aws standard 默认
# 3 次尝试（~4s）盖不住 17s，实测 fi2/p007 永久失败。最多 8 次，每次调用被
# run_to 强制在 30s 截断（不保证 8 次内走完）；外层 FI 级的 3 次重试不变，
# 单个 key 最坏 ≈ 3×30s + 2×1s sleep ≈ 93s，再由各场景绝对预算硬截止。
max_attempts = 8
[profile swadmin]
region = us-east-1
s3 =
  addressing_style = path
retry_mode = standard
max_attempts = 8
AWSCFG
) || fatal "写入 AWS config 失败（$AWS_CONFIG_FILE）"
( umask 077; cat > "${AWS_SHARED_CREDENTIALS_FILE}" <<AWSCRED
[swcp]
aws_access_key_id = ${SW_CP_ACCESS_KEY}
aws_secret_access_key = ${SW_CP_SECRET_KEY}
[swadmin]
aws_access_key_id = ${SW_ADMIN_ACCESS_KEY}
aws_secret_access_key = ${SW_ADMIN_SECRET_KEY}
AWSCRED
) || fatal "写入 AWS credentials 失败（$AWS_SHARED_CREDENTIALS_FILE）"
# aws-s3 — aws S3 薄封装（身份用第 1 个参数选 profile）。写成文件是因为
# run_to 经 in_new_group 的 exec 启动子进程，shell 函数无法穿过去（mc 时代
# 等价物是裸 mc + alias；aws 时代身份经 AWS_PROFILE 选择）。H1：放在本次
# mktemp 私有目录里（无既有文件、cleanup 随目录一起消失）。
# F2：封装自带白名单——先清掉继承环境里的全部 AWS_*，再显式设置本封装需要的
# （config/credentials 路径在生成时烘焙进来，endpoint 同）。
AWSBIN="$AWS_DIR/aws-s3"
cat > "$AWSBIN" <<AWSBIN
#!/usr/bin/env bash
# B7 生成物（bin/tmp/fi/ 下，不入库）：FI 的 aws S3 API 封装（恒走 s3api；
# 身份用第 1 个参数选 profile；endpoint 显式给出）。
# F2 白名单：调用者环境的全部 AWS_* 一律不得漏入。
for _v in \$(compgen -e); do case "\$_v" in AWS_*|AWS) unset "\$_v" ;; esac; done
export AWS_CONFIG_FILE='${AWS_CONFIG_FILE}' AWS_SHARED_CREDENTIALS_FILE='${AWS_SHARED_CREDENTIALS_FILE}'
export AWS_PAGER='' AWS_EC2_METADATA_DISABLED=true
# 就绪探测经 FI_AWS_MAX_ATTEMPTS 传入，在清理**之后**映射成 AWS_MAX_ATTEMPTS。
case "\${FI_AWS_MAX_ATTEMPTS:-}" in ''|*[!0-9]*) ;; *) AWS_MAX_ATTEMPTS="\${FI_AWS_MAX_ATTEMPTS}"; export AWS_MAX_ATTEMPTS ;; esac
exec env AWS_PROFILE="\$1" aws --endpoint-url "${AWS_S3}" --no-cli-pager s3api "\${@:2}"
AWSBIN
chmod +x "$AWSBIN" || fatal "chmod +x aws-s3 封装失败（$AWSBIN）"

# ts_ns — 可移植的纳秒级 Unix 时间戳（纯数字）。date 的 %N 在 GNU date 与新版
# macOS（Darwin 24 实测）都可用；但部分旧版 macOS/BSD date 会输出字面 N 或
# 静默丢弃纳秒位，故先验证 date 输出为纯数字：date 可用用 date；否则 python3
# 可用则退回 python3（仅标准库，time.time_ns()）；都不行用秒级 date +%s 兜底
#（仍为纯数字）。消费方（writes*.jsonl / ovr-times.txt 的第二列）只做展示性
# 对照，不解析格式。TS_ENGINE 导出后，子脚本（writer-s2.sh）用同一引擎，
# 两端时间戳同源。
# ⚠ 不能写成 `[ ... ] || cmd && X`：&& 的优先级会让 date 分支被 python3 覆盖。
TS_ENGINE=""
case "$(date +%s%N 2>/dev/null)" in
  ''|*[!0-9]*) ;;
  *) TS_ENGINE="date" ;;
esac
if [ -z "$TS_ENGINE" ] && command -v python3 >/dev/null 2>&1; then
  TS_ENGINE="python3"
fi
export TS_ENGINE
ts_ns() {
  case "${TS_ENGINE:-}" in
    date)    date +%s%N ;;
    python3) python3 -c 'import time; print(time.time_ns())' ;;
    *)       date +%s ;;   # 兜底：秒级整数（仍为数字，不做格式断言）
  esac
}

ts_hms() { # <ts：19 位纳秒或 10 位秒> — 人类可读时刻（纳秒取前 10 位换秒）
  local s=$1
  [ "${#s}" -gt 10 ] && s=${s:0:10}
  [ "$s" -gt 0 ] 2>/dev/null && date -r "$s" '+%H:%M:%S' 2>/dev/null || echo '-'
}

# s2_span_classify <writes-file> <restart-start> <restart-end> <ready> — G1 横跨
# 判据的时间戳分类（H3，评审 r2）。三个边界与写入时间戳**同宽度**时用 bash
# `[[ ]]` 定长字符串比较（同宽度下字符串序=数值序；19 位纳秒绝不进 awk 浮点）；
# 边界为 10 位秒（ts_ns 秒级兜底）时降级：**同秒写入无法排序，不计入任何一侧**
# （S2_TS_DEGRADED=1，调用方打印降级说明），其余按严格比较分类。
# 结果（全局）：S2_PRE_KEY/S2_PRE_TS（重启前最后一次成功写入，取最大）、
# S2_POST_KEY/S2_POST_TS（恢复后第一次成功写入，取最小）、S2_WIN_N（restart
# 起止之间——观测值，不参与判定）、S2_TS_DEGRADED（1=秒级兜底降级）。
s2_span_classify() {
  local file=$1 rs=$2 re=$3 rd=$4 k ts
  S2_PRE_KEY='-'; S2_PRE_TS=''; S2_POST_KEY='-'; S2_POST_TS=''; S2_WIN_N=0
  S2_TS_DEGRADED=0
  if [ "${#rs}" -ne 19 ] || [ "${#re}" -ne 19 ] || [ "${#rd}" -ne 19 ]; then
    S2_TS_DEGRADED=1
  fi
  while read -r k ts <&5; do
    [ -z "${k:-}" ] && continue
    case "${ts:-}" in ''|*[!0-9]*) continue ;; esac
    if [[ "$ts" < "$rs" ]]; then
      if [ -z "$S2_PRE_TS" ] || [[ "$ts" > "$S2_PRE_TS" ]]; then S2_PRE_TS=$ts; S2_PRE_KEY=$k; fi
    fi
    if [[ "$ts" > "$rd" ]]; then
      if [ -z "$S2_POST_TS" ] || [[ "$ts" < "$S2_POST_TS" ]]; then S2_POST_TS=$ts; S2_POST_KEY=$k; fi
    fi
    if [[ ( "$ts" > "$rs" || "$ts" = "$rs" ) && ( "$ts" < "$re" || "$ts" = "$re" ) ]]; then
      S2_WIN_N=$((S2_WIN_N+1))
    fi
  done 5< "$file"
}

# sc_deadline_set <seconds> — 设置当前场景的绝对截止（SCEN_END）。
# SCEN_END ≤0 / 未设 = 无场景期限（预检与 infra 阶段不裁剪）。
# ⚠️ 必须定义在 run_to **之前**：run_to 的预算裁剪在 infra 阶段就会调用它。
sc_deadline_set() { SCEN_END=$(( $(date +%s) + $1 )); }
sc_remaining() {
  local now=$(date +%s)
  if [ "${SCEN_END:-0}" -le 0 ]; then echo 100000; return; fi
  [ "$SCEN_END" -le "$now" ] && { echo 0; return; }
  echo $(( SCEN_END - now ))
}

# sleep_capped <seconds> — sleep，但不超过场景剩余预算；预算耗尽不睡并返回 1
#（R3-5：轮询循环的 sleep 一律裁剪到绝对 deadline）。
sleep_capped() {
  local want=$1 rem
  rem=$(sc_remaining)
  [ "$rem" -le 0 ] && return 1
  [ "$rem" -lt "$want" ] && want=$rem
  sleep "$want"
  return 0
}

# in_new_group <cmd...> — 在独立进程组里运行命令（macOS 无 setsid 二进制，用
# perl setpgrp）。⚠️ 函数体必须 **exec perl**：后台调用 `in_new_group … &` 会
# 先 fork 一个 bash 子壳，不 exec 的话 $! 是这个等待中的子壳（且其 pgid 还是
# 主脚本的组），真正执行命令的进程比 $! 晚一两号、自成一组 —— 超时/清理按
# $! 杀组会完全脱靶（实测复现过：监听 pid = $!+2）。exec 让子壳被 perl 顶替，
# setpgrp 后组 id == $! == 命令 pid，kill -KILL -PID 才能整组命中（R3-5）。
in_new_group() { exec perl -e 'setpgrp(0,0); exec @ARGV or die "in_new_group: exec @ARGV: $!\n"' "$@"; }

# run_to <seconds> <cmd...> — bounded command; child output captured and
# re-emitted; stdin detached (while-read safety).
# R3-5：实际超时 = min(单次预算, 场景剩余)；剩余 ≤0 拒绝启动（rc=124，调用方
# 计失败）；命令放进独立进程组，超时杀整组并 wait 确认退出（孙进程不留活口）。
# 裁剪/拒绝事件记入 $OUT/run_to-clipped.log（不写 stdout/stderr——psql_q 以
# 2>&1 捕获输出，混入会污染查询结果）。
RUN_TO_CLIPPED="$OUT/run_to-clipped.log"; : > "$RUN_TO_CLIPPED"
run_to() {
  local limit=$1; shift
  local rem; rem=$(sc_remaining)
  if [ "$rem" -lt "$limit" ]; then
    echo "$(date '+%H:%M:%S') clipped: ${limit}s -> ${rem}s :: $*" >> "$RUN_TO_CLIPPED"
    limit=$rem
  fi
  if [ "$limit" -le 0 ]; then
    echo "$(date '+%H:%M:%S') REFUSED (场景剩余 ${rem}s ≤ 0): $*" >> "$RUN_TO_CLIPPED"
    return 124
  fi
  local tmpf
  tmpf=$(mktemp "$OUT/.run_to.XXXXXX") || { echo "run_to: mktemp 失败" >> "$RUN_TO_CLIPPED"; return 124; }
  in_new_group "$@" > "$tmpf" 2>&1 < /dev/null &
  local pid=$! start=$(date +%s) spent rc left step
  while kill -0 "$pid" 2>/dev/null; do
    spent=$(( $(date +%s) - start ))
    if [ "$spent" -ge "$limit" ]; then
      kill -KILL "-$pid" 2>/dev/null   # 整组（组 id == pid，见 in_new_group）
      kill -KILL "$pid" 2>/dev/null
      wait "$pid" 2>/dev/null          # 确认退出（reap）
      cat "$tmpf"; rm -f "$tmpf"
      return 124
    fi
    left=$(( limit - spent )); step=1
    [ "$left" -lt "$step" ] && step=$left
    [ "$step" -le 0 ] && step=0.2
    sleep "$step"
  done
  rc=0
  wait "$pid" || rc=$?
  cat "$tmpf"; rm -f "$tmpf"
  return "$rc"
}

# psql_q_soft <sql> — bounded (15s)，失败**返回 rc** 而不是 FATAL：断言函数内
# 的查询失败必须变成 bad + return 1（N4），而不是把整跑作废或当成空结果。
_PSQL_HUNG=0
psql_q_soft() {
  local out rc
  out=$(run_to 15 $COMPOSE exec -T postgres psql -U fileagent -d fileagent -tAc "$1" 2>&1)
  rc=$?
  if [ "$rc" != "0" ]; then
    echo "QUERY FAILED (rc=$rc): $1 — output: ${out:0:200}" >&2
    return 1
  fi
  printf '%s' "$out"
}

# psql_q <sql> — fail-closed 版本：任何查询失败立即终止脚本（查询错误绝不能
# 伪装成空结果，review F3）。断言/轮询内部请用 psql_q_soft。
psql_q() {
  local out
  if [ "${FI_HANG_DEMO:-0}" = "1" ] && [ "$_PSQL_HUNG" = "0" ]; then
    _PSQL_HUNG=1
    run_to 15 $COMPOSE exec -T postgres psql -U fileagent -d fileagent -tAc "select pg_sleep(300)" >/dev/null 2>&1
    rc=$?
    bad "F6 红证明：挂起查询被 15s 上限终止（rc=${rc}，期望 124）——脚本按失败退出"
    exit 1
  fi
  out=$(psql_q_soft "$1") || { echo "FATAL: psql_q failed: $1" >&2; exit 1; }
  printf '%s' "$out"
}

# should_run <n> — scenario gate (FI_ONLY=4 runs only scenario 4; default all).
should_run() { case ",${FI_ONLY:-1,2,3,4}," in *",$1,"*) return 0 ;; *) return 1 ;; esac; }

# ── 遗留进程防线（返工 2）：端口预检 + 启动 pid 核实 + 按 pid 清理并确认 ──────
# 上一轮的泄漏（旧 CP 应答 healthz、新 CP 绑定失败被无视）让断流根本没有发生。
# 从这版起：所有本脚本要监听的端口必须空闲，否则 FATAL 并打印占用者 pid；
# 启动后核实「监听该端口的 pid == 本次启动的 pid」；清理按 pid 杀并确认退出。
assert_port_free() { # <port> <what>
  local port=$1 what=$2 occupier cmd
  occupier=$(lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | tail -n +2 | awk '{print $2}' | sort -u | head -1)
  if [ -n "$occupier" ]; then
    cmd=$(ps -p "$occupier" -o command= 2>/dev/null | head -1)
    echo "FATAL: 端口 ${port}（${what}）已被占用 —— 占用者 pid=${occupier} cmd=${cmd}" >&2
    echo "FATAL: 存在遗留进程时本脚本拒绝运行（fail-closed）；请先清理再跑。" >&2
    exit 1
  fi
}

verify_listening_pid() { # <port> <expected-pid> <what>
  local port=$1 want=$2 what=$3 got
  got=$(lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | tail -n +2 | awk '{print $2}' | sort -u | head -1)
  if [ "$got" != "$want" ]; then
    echo "FATAL: ${what} 监听 pid=${got:-<none>} ≠ 本次启动 pid=${want} —— 端口被其它进程应答（遗留进程）" >&2
    exit 1
  fi
}

# stop_and_confirm <pid> — 杀（TERM→KILL）并确认**整个进程组**退出；仍有成员
# 返回 1（R4-3）。确认对象是组 `-$pid` 而非组头：组头先退、子孙忽略 TERM 时
# 组仍存在，必须继续等待并对整组升级 KILL；wait 只负责回收直属子进程。
# 只作用于本次记录的 pid 的进程组（没有任何按路径/模式的兜底杀）。
stop_and_confirm() {
  local pid=$1 try
  kill -TERM "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || return 0
  for try in 1 2 3 4 5 6; do
    # kill -0 -PGID：组内任一成员存活即成功 → 组还在就继续等
    if ! kill -0 "-$pid" 2>/dev/null; then
      wait "$pid" 2>/dev/null   # 回收直属子进程
      return 0
    fi
    sleep 1
  done
  kill -KILL "-$pid" 2>/dev/null   # 整组 KILL（含忽略 TERM 的子孙）
  kill -KILL "$pid" 2>/dev/null
  sleep 1
  if kill -0 "-$pid" 2>/dev/null; then
    echo "  ✗ 进程组 ${pid} 杀后仍有成员存活" >&2
    return 1
  fi
  wait "$pid" 2>/dev/null   # 回收直属子进程（孤儿成员由 init 回收，已死）
  return 0
}

# port_occupied <port> — 非致命查询：打印监听 pid（空 = 空闲）。
port_occupied() {
  lsof -nP -iTCP:"$1" -sTCP:LISTEN 2>/dev/null | tail -n +2 | awk '{print $2}' | sort -u | head -1
}

# PRECHECK_OK：全部端口预检通过之后才置 1。cleanup 据此判断本次是否**拥有**任何
# 资源 —— 拒绝路径（预检失败）绝不执行 down / pkill / 端口清理（R3-6）。
# CLEANUP_FAILED：清理中的任何失败置 1，并计入最终退出码（R3-6）。
PRECHECK_OK=0
CLEANUP_FAILED=0

cleanup() {
  local rc=$?   # ⚠️ 必须是第一条语句（R4-1）：保存进入 EXIT trap 时的主流程
  # 退出码；任何 echo/命令都会把它清零。最终码 = 主流程非零或清理失败任一。
  echo ""; echo "══ cleanup ══"
  SCEN_END=0   # 清理阶段的 down 不受场景预算约束（sc_remaining 视为无限）
  # H1：本次运行的私有 AWS 目录（config/credentials/aws-s3 封装）整目录删除；
  # 历史固定路径 bin/tmp/fi/aws 已在启动时清理且不再写入。
  if [ -n "${AWS_DIR:-}" ]; then
    rm -rf "${AWS_DIR}"
  fi
  if [ "${PRECHECK_OK:-0}" != 1 ]; then
    echo "  · 预检未全部通过（拒绝路径）：本次未创建任何基础设施，不执行任何 down/pkill（R3-6）"
    return $rc
  fi
  local pid spec left
  for pid in "${CP_PID:-}" "${PROXY_PID:-}" "${WRITER_PID:-}"; do
    [ -n "$pid" ] || continue
    if ! stop_and_confirm "$pid"; then
      echo "  ✗ 本次记录的 pid ${pid} 未能确认退出"
      CLEANUP_FAILED=1
    fi
  done
  if ! run_to 180 $SWCOMPOSE down -v --remove-orphans >"$OUT/cleanup-sw.log" 2>&1; then
    echo "  ✗ seaweedfs compose down 失败/超时（see $OUT/cleanup-sw.log）"
    CLEANUP_FAILED=1
  fi
  if ! run_to 180 $COMPOSE down -v --remove-orphans >"$OUT/cleanup-base.log" 2>&1; then
    echo "  ✗ base compose down 失败/超时（see $OUT/cleanup-base.log）"
    CLEANUP_FAILED=1
  fi
  # 只核验**本脚本自己的端口**是否全部释放（所有权校验，不触碰别人）
  for spec in "${FA_POSTGRES_PORT}" "${FA_REDIS_PORT}" "${FA_NATS_PORT}" "${FA_NATS_MONITOR_PORT}" \
              "${FA_SEAWEEDFS_MASTER_PORT}" "${FA_SEAWEEDFS_S3_PORT}" "${FA_SEAWEEDFS_FILER_HTTP_PORT}" \
              "${FA_SEAWEEDFS_FILER_GRPC_PORT}" "${CP_HTTP_PORT}" "${CP_GRPC_PORT}" "${PROXY_PORT}"; do
    left=$(port_occupied "$spec")
    if [ -n "$left" ]; then
      echo "  ✗ 清理后自有端口 ${spec} 仍被 pid=${left} 监听"
      CLEANUP_FAILED=1
    fi
  done
  if [ "${CLEANUP_FAILED:-0}" = 1 ]; then
    echo "  ✗ cleanup 未完全成功（计入最终结果，R3-6）"
    [ "$rc" -eq 0 ] && rc=1   # 清理失败也要非零（主流程为 0 时）
  else
    echo "  ✓ cleanup 完成：记录的 pid 全部退出、两个 compose down 成功、自有端口全部释放"
  fi
  # EXIT trap 里 return 不会改变退出码 —— 显式 exit 传递「主流程退出码
  # （trap 进入时保存）或清理失败」二者取非零（R4-1：cleanup 成功不得掩盖
  # 主流程的失败码）。
  exit $rc
}
trap cleanup EXIT


# ── 前置工具检查（fail-closed；PRECHECK_OK 置位之前，fatal 不触发任何清理）──
# python3 是硬前置：起基础设施时 render-seaweedfs-configs.sh 缺 python3 直接
# exit 1（渲染 SeaweedFS JSON 凭据配置）；docker/aws/go 同样缺一不可。
req_tool() { # <cmd> <用途说明>
  command -v "$1" >/dev/null 2>&1 && return 0
  echo "FATAL: 缺少前置工具 $1（$2）—— 请先安装再跑本脚本。" >&2
  exit 1
}
req_tool docker "容器运行时（compose v2 起隔离环境）"
req_tool aws    "AWS CLI v2（S3 写入/删除/列举；macOS: brew install awscli，其余见 https://docs.aws.amazon.com/cli/latest/userguide/getting-started-install.html）"
req_tool go     "构建 controlplane 与 faultproxy（当前工作树）"
req_tool python3 "渲染 SeaweedFS JSON 配置（render-seaweedfs-configs.sh 硬依赖；亦是 ts_ns 时间戳的兜底引擎）"
case "$(aws --version 2>&1)" in
  aws-cli/2.*) ;;
  *)
    echo "FATAL: 需要 AWS CLI v2（实测基线 2.39.2），当前：$(aws --version 2>&1)" >&2
    echo "       v1 的 profile 配置/校验和行为与本次改造验证过的路径不同，不支持。" >&2
    exit 1
    ;;
esac

# ── 遗留进程预检（fail-closed）───────────────────────────────────────────────
for spec in "${FA_POSTGRES_PORT}:PostgreSQL" "${FA_REDIS_PORT}:Redis" "${FA_NATS_PORT}:NATS" \
            "${FA_NATS_MONITOR_PORT}:NATS-monitor" "${FA_SEAWEEDFS_MASTER_PORT}:SeaweedFS-master" \
            "${FA_SEAWEEDFS_S3_PORT}:SeaweedFS-S3" "${FA_SEAWEEDFS_FILER_HTTP_PORT}:SeaweedFS-filer-http" \
            "${FA_SEAWEEDFS_FILER_GRPC_PORT}:SeaweedFS-filer-grpc" "${CP_HTTP_PORT}:CP-http" \
            "${CP_GRPC_PORT}:CP-grpc" "${PROXY_PORT}:faultproxy"; do
  assert_port_free "${spec%%:*}" "${spec#*:}"
done
PRECHECK_OK=1
ok "全部端口预检通过（无遗留进程占用）——cleanup 所有权自此生效"

# ── 构建被测二进制（从当前工作树，不依赖预置产物）────────────────────────────
# CP 用仓库统一构建方式（与 Makefile build-controlplane 同一命令）；faultproxy
# 是独立 Go 模块（根 go.work 不含它），用 GOWORK=off 单独构建。产物放 FI 专有
# 路径 bin/tmp/fi/（不覆盖共享的 bin/controlplane，避免与 smoke/dev 互踩）。
#（⚠ -C 切目录后 -o 的相对路径基准也随切，故这里用 $REPO 绝对路径）。
step "构建 controlplane 与 faultproxy（当前工作树 → bin/tmp/fi/ 专有路径）"
run_to 600 go build -o "$REPO/$OUT/controlplane" ./controlplane/cmd/server \
  >"$OUT/build-cp.log" 2>&1 || fatal "go build controlplane（see $OUT/build-cp.log）"
run_to 120 env GOWORK=off go build -C "$REPO/test/fault-injection/faultproxy" \
  -o "$REPO/$OUT/faultproxy" . >"$OUT/build-proxy.log" 2>&1 \
  || fatal "go build faultproxy（see $OUT/build-proxy.log）"
ok "$OUT/controlplane 与 $OUT/faultproxy 就绪（FI 专有路径，不碰 bin/ 共享产物）"

# ── infra ────────────────────────────────────────────────────────────────────
step "起基础设施（SeaweedFS 拓扑 + PG/Redis/NATS，项目 ${COMPOSE_PROJECT_NAME}）"
run_to 300 $COMPOSE up -d --wait postgres redis nats >"$OUT/compose-base.log" 2>&1 || fatal "base compose up"
run_to 120 bash deploy/scripts/render-seaweedfs-configs.sh >"$OUT/render.log" 2>&1 || fatal "render (see $OUT/render.log)"
run_to 600 $SWCOMPOSE up -d --wait >>"$OUT/compose-sw.log" 2>&1 || fatal "seaweedfs compose up"
ok "基础设施 healthy"

# B7：mc alias ×2 → 私有 AWS profile（凭据在 $OUT/aws/credentials，见文件头）；
# mc mb --ignore-existing → head-bucket 探测 + 缺失时 create-bucket（aws 无
# --ignore-existing；「已存在算成功」语义由此保持：head 命中即跳过，重复
# create 会以 BucketAlreadyOwnedByYou 报错——PoC 实测，故必须先探）。
run_to 30 "$AWSBIN" swcp head-bucket --bucket data-sensor >/dev/null 2>&1 \
  || run_to 30 "$AWSBIN" swcp create-bucket --bucket data-sensor >/dev/null \
  || fatal "create bucket data-sensor"
ok "AWS CLI 配置与桶就绪"

if [ "${FI_HANG_DEMO:-0}" = "1" ]; then
  note "F6 红证明：人为挂起一次查询（pg_sleep 300，run_to 15s）——脚本必须数秒内按失败退出"
  psql_q "select 1"
fi

# ── CP（经代理订阅 filer）────────────────────────────────────────────────────
start_proxy() {
  in_new_group "$REPO/$OUT/faultproxy" "127.0.0.1:${PROXY_PORT}" "127.0.0.1:${FA_SEAWEEDFS_FILER_GRPC_PORT}" >>"$OUT/proxy.log" 2>&1 &
  PROXY_PID=$!
  sleep 0.5
  kill -0 "$PROXY_PID" 2>/dev/null || fatal "faultproxy 启动即退出"
  verify_listening_pid "$PROXY_PORT" "$PROXY_PID" "faultproxy"
}
start_cp() {
  in_new_group env \
  DATABASE_URL="postgres://fileagent:fileagent@127.0.0.1:${FA_POSTGRES_PORT}/fileagent?sslmode=disable" \
  REDIS_URL="redis://127.0.0.1:${FA_REDIS_PORT}/0" \
  NATS_URL="nats://127.0.0.1:${FA_NATS_PORT}" \
  MINIO_ENDPOINT="127.0.0.1:${FA_SEAWEEDFS_S3_PORT}" \
  MINIO_PUBLIC_ENDPOINT="127.0.0.1:${FA_SEAWEEDFS_S3_PORT}" \
  MINIO_USE_SSL=false \
  MINIO_ACCESS_KEY="$SW_CP_ACCESS_KEY" \
  MINIO_SECRET_KEY="$SW_CP_SECRET_KEY" \
  MINIO_ROLE_ARN="$SW_AGENT_ROLE_ARN" \
  JWT_SECRET=fi-jwt-secret-not-for-production-use \
  HTTP_PORT="$CP_HTTP_PORT" GRPC_PORT="$CP_GRPC_PORT" \
  BOOTSTRAP_ADMIN_USERNAME=admin BOOTSTRAP_ADMIN_PASSWORD='FiAdmin@2026' \
  STORAGE_EVENT_SOURCE=seaweedfs_filer \
  SEAWEEDFS_FILER_GRPC_ADDR="127.0.0.1:${PROXY_PORT}" \
  SEAWEEDFS_FILER_INSECURE=true \
  SEAWEEDFS_RESUME_MARGIN=60s \
    "$REPO/$OUT/controlplane" >>"$OUT/cp.log" 2>&1 &
  CP_PID=$!
  local i
  for i in $(seq 1 60); do
    run_to 5 curl -sf --connect-timeout 2 --max-time 4 -o /dev/null "http://127.0.0.1:${CP_HTTP_PORT}/healthz" && break
    kill -0 "$CP_PID" 2>/dev/null || fatal "CP 进程在启动期间退出（端口被占或配置错误）"
    sleep_capped 1 || true
  done
  run_to 5 curl -sf --connect-timeout 2 --max-time 4 -o /dev/null "http://127.0.0.1:${CP_HTTP_PORT}/healthz" || fatal "CP 未就绪（300s）"
  # F-遗留：healthz 被 200 应答还不够——HTTP 与 gRPC 两个端口的监听者都必须是
  # 本次启动的 pid（R3-6：gRPC 端口单独核实）
  verify_listening_pid "$CP_HTTP_PORT" "$CP_PID" "CP-http"
  local g got=""
  for g in $(seq 1 10); do
    got=$(port_occupied "$CP_GRPC_PORT")
    [ "$got" = "$CP_PID" ] && break
    kill -0 "$CP_PID" 2>/dev/null || fatal "CP 进程在 gRPC 监听建立前退出"
    sleep 1
  done
  verify_listening_pid "$CP_GRPC_PORT" "$CP_PID" "CP-gRPC"
}

step "启动 TCP 代理 + CP（订阅走代理 127.0.0.1:${PROXY_PORT} → filer :${FA_SEAWEEDFS_FILER_GRPC_PORT}）"
start_proxy
start_cp
ok "CP 就绪（pid ${CP_PID}），代理 pid ${PROXY_PID}"

# ── S3 写入/删除/统计（重试 + 记录；全部带超时）──────────────────────────────
FAILED_WRITES="$OUT/failed-writes.txt"; : > "$FAILED_WRITES"
RM_ERRORS="$OUT/rm-errors.log"; : > "$RM_ERRORS"

# md5_of <content> — SeaweedFS S3 的单部分对象 ETag＝内容 MD5（hex 小写），
# 所以期望 etag 可以完全本地计算（mc stat/ls 在 SeaweedFS 上不返回 ETag，实测）。
md5_of() {
  if command -v md5sum >/dev/null 2>&1; then
    printf '%s' "$1" | md5sum | cut -d' ' -f1
  else
    printf '%s' "$1" | md5 -q
  fi
}

# put_object <key> <content> — 重试 3 次；失败记入 FAILED_WRITES 并返回 1
# B7：mc cp --quiet → s3api put-object --body（CP 身份 swcp）。用底层 API 而
# 非高层 `s3 cp`：输出稳定（stdout JSON），且少一层本地扫描。CLI 默认附带
# 的 CRC 校验和 SeaweedFS 接受（PoC 实测），无需任何环境变量覆盖。
put_object() {
  local key=$1 content=$2 try
  printf '%s' "$content" > "$OUT/.putobj"
  for try in 1 2 3; do
    if run_to 30 "$AWSBIN" swcp put-object --bucket data-sensor --key "$key" --body "$OUT/.putobj" >/dev/null; then
      return 0
    fi
    sleep 1
  done
  echo "$key" >> "$FAILED_WRITES"
  echo "FATAL: aws put-object failed after 3 retries: $key" >&2
  return 1
}

delete_object() { # <key> — 重试 3 次；stderr 记日志；仍失败则致命
  # B7：mc rm → s3api delete-object。语义差异（实测）：mc rm 对不存在的对象
  # 报错（rc=1），delete-object 按 S3 语义幂等（rc=0）。本脚本只删「已确认
  # 写入成功」的 key，重试判据基于退出码——aws 语义更宽松，不会引入假失败。
  local try
  for try in 1 2 3; do
    if run_to 30 "$AWSBIN" swadmin delete-object --bucket data-sensor --key "$1" >>"$RM_ERRORS" 2>&1; then
      return 0
    fi
    sleep 1
  done
  echo "FATAL: aws delete-object failed after 3 retries: $1 (see $RM_ERRORS tail below)" >&2
  tail -3 "$RM_ERRORS" >&2
  exit 1
}

# wait_s3_ready <label> — F7：filer 重启后等 S3 网关真正应答。
# R3-5：总预算 = min(120s, 场景剩余)；每次探测和末次 sleep 都从预算中扣除实际
# 耗时，超预算立即 FATAL（不再有末次固定 sleep 2 的越界）。
wait_s3_ready() {
  local label=$1 i=0 start=$(date +%s) elapsed probe_rc budget=120 rem left
  rem=$(sc_remaining)
  if [ "$rem" -lt "$budget" ]; then
    [ "$rem" -le 0 ] && fatal "wait_s3_ready：场景剩余预算 ${rem}s ≤ 0（${label}）"
    budget=$rem
  fi
  while :; do
    elapsed=$(( $(date +%s) - start ))
    [ "$elapsed" -ge "$budget" ] && break
    probe_rc=0
    # 就绪探测专用 1 次尝试：网关未就绪时秒退（0.3s），不被 CLI 默认重试拖长。
    # 经 FI_AWS_MAX_ATTEMPTS 传入——封装脚本先清环境再映射成 AWS_MAX_ATTEMPTS
    # （F2：调用者的 AWS_MAX_ATTEMPTS 不得漏入，探测的 1 只由本脚本自己设）。
    run_to $(( budget - elapsed )) env FI_AWS_MAX_ATTEMPTS=1 "$AWSBIN" swadmin \
      list-objects-v2 --bucket data-sensor --max-items 1 >/dev/null 2>&1 || probe_rc=$?
    if [ "$probe_rc" = "0" ]; then
      ok "S3 网关就绪（${label}，第 $((i+1)) 次探测，实际耗时 $(($(date +%s)-start))s）"
      return 0
    fi
    i=$((i+1))
    # rc=124 表示这次探测自己耗尽了剩余预算 → 循环条件会退出
    left=$(( budget - ( $(date +%s) - start ) ))
    [ "$left" -le 0 ] && break
    [ "$left" -gt 2 ] && left=2
    sleep "$left"
  done
  fatal "S3 网关 ${budget}s 未就绪（${label}，实际耗时 $(($(date +%s)-start))s）"
}

# ── 计划与断言（F3：独立生成计划；缺行/空列表/两边空/命令失败 = 失败）─────────
# 计划文件行格式：<key>|<expected_size>|<expected_etag>(可空 → 断言时用 S3 实测)
PLAN_S1_NEW="$OUT/plan-s1-new.txt"     # 场景1 新建存活
PLAN_S1_DEL="$OUT/plan-s1-del.txt"     # 场景1 已入账后删除
PLAN_S1_OVR="$OUT/plan-s1-ovr.txt"     # 场景1 已入账后覆盖（记录旧值）
PLAN_S2="$OUT/plan-s2.txt"
PLAN_S3_NEW="$OUT/plan-s3-new.txt"
PLAN_S3_DEL="$OUT/plan-s3-del.txt"
PLAN_S3_OVR="$OUT/plan-s3-ovr.txt"

# s3_keys / ledger_keys — 失败**返回 1**（不再 FATAL）：调用方（断言/轮询）必须
# 显式检查 rc 并自行决定（N4：listing 失败绝不能当作空结果）。错误打印到 stderr。
# B7：mc ls --recursive → list-objects-v2 --prefix。解析差异（实测）：
#   · mc 的最后一列是相对 prefix 的 key（旧解析用 sed 补回前缀）；
#   · aws 的 Key 是完整 key，无需补前缀；
#   · text 输出把同页多个 key 用 TAB 连接（分页时按页换行），tr 还原成行；
#   · 前缀下无对象时 JMESPath 投影在 text 模式输出字面 "None"（CLI 已知行为，
#     FI 的 key 都是 fi*/pNNN，不会撞名），按空清单过滤；
#   · >1000 个对象时 CLI 自动携带 continuation token 分页。
s3_keys() {
  local out rc
  out=$(run_to 60 "$AWSBIN" swadmin list-objects-v2 --bucket data-sensor \
          --prefix "$1" --query 'Contents[].Key' --output text 2>&1)
  rc=$?
  if [ "$rc" != "0" ]; then
    echo "QUERY FAILED: aws list-objects-v2 (rc=$rc) for prefix $1: ${out:0:200}" >&2
    return 1
  fi
  printf '%s\n' "$out" | tr '\t' '\n' | awk '$0 != "" && $0 != "None"' | sort
  # F3（评审 r1）：过滤用 awk 而不是 grep——空前缀时 CLI 输出字面 "None"（rc=0），
  # grep -vx 对空/全过滤输入返回 1，pipefail 会把「成功、零对象」误判成查询
  # 失败（场景 4 纯观测与异常态会触发）；awk 对空输入 rc=0。run_to 非零时的
  # 显式失败分支保持不变（真正的 API/连接错误仍走上面的 return 1）。
}

ledger_keys() {
  local out
  out=$(psql_q_soft "select storage_path from file_entries where storage_path like '${1}%' and status='$2' order by storage_path") || return 1
  printf '%s\n' "$out" | sort | sed '/^$/d'
}

# assert_converged <prefix> <actual-deleted-file> <expected-live-set-file> <expected-del-set-file>
# R4-2：期望写/删/存活 key 集合在场景开始前按固定规则生成、**不随执行结果改写**；
# 本断言做**等集合**比较：
#   · S3 实际存活集合 == 账本 completed 集合 == 期望存活集合（sort -u 后 diff）；
#   · 实际删除操作清单 == 期望删除集合（sort -u 后 diff），且两边都**无重复 key**；
#   · 期望删除集合逐 key 查账：deleted 或合法缺席，无活行，合计 == 集合大小；
#   · 期望删除集合文件为空 = 失败（空删除计划必须失败，N5 语义保留）。
# 实际查询计数写入全局 AC_LIVE / AC_DELETED / AC_ABSENT（摘要行用，保留真实计数）。
# R3-3：两个 listing 与所有查询/管道赋值显式检查 rc，失败立即 bad 并 return 1。
AC_LIVE=0; AC_DELETED=0; AC_ABSENT=0
assert_converged() {
  local prefix=$1 deleted_file=$2 live_set=$3 del_set=$4
  AC_LIVE=0; AC_DELETED=0; AC_ABSENT=0
  local s3n ledn dchk=0 deld=0 dmissing=0 dabsent=0 dexp k st
  if ! s3_keys "$prefix" > "$OUT/s3.actual" 2>>"$OUT/query-errors.log"; then
    bad "S3 listing 失败（${prefix}）—— 查询失败不能当作空结果（N4）"; return 1
  fi
  if ! ledger_keys "$prefix" completed > "$OUT/ledger.completed" 2>>"$OUT/query-errors.log"; then
    bad "账本 listing 失败（${prefix}）—— 查询失败不能当作空结果（N4）"; return 1
  fi
  s3n=$(wc -l < "$OUT/s3.actual" | tr -d ' ') || { bad "S3 清单计数失败（N4）"; return 1; }
  ledn=$(wc -l < "$OUT/ledger.completed" | tr -d ' ') || { bad "账本清单计数失败（N4）"; return 1; }
  AC_LIVE=$s3n
  if [ ! -s "$live_set" ]; then
    bad "期望存活集合文件不可读/为空（${live_set}）—— 集合校验无法进行（R4-2）"; return 1
  fi
  if [ ! -f "$del_set" ] || ! grep -q . "$del_set" 2>/dev/null; then
    bad "期望删除集合为空/不可读（${del_set}）—— 空删除清单必须失败（N5/R4-2）"; return 1
  fi
  # 重复 key 检查（期望删除集合与实际操作清单各自去重前后行数必须一致）
  if [ "$(grep -c . "$del_set")" != "$(sort -u "$del_set" | grep -c .)" ]; then
    bad "期望删除集合含重复 key（R4-2）："; sort "$del_set" | uniq -d | head -5 | sed 's/^/    dup: /'; return 1
  fi
  if [ "$(grep -c . "$deleted_file" 2>/dev/null)" != "$(sort -u "$deleted_file" 2>/dev/null | grep -c .)" ]; then
    bad "实际删除操作清单含重复 key（R4-2）："; sort "$deleted_file" 2>/dev/null | uniq -d | head -5 | sed 's/^/    dup: /'; return 1
  fi
  # 等集合比较 ①：S3 实际存活 == 期望存活集合
  if ! diff -u <(sort -u "$live_set") <(sort -u "$OUT/s3.actual") > "$OUT/liveset.txt"; then
    bad "S3 实际存活集合 ≠ 期望存活集合（R4-2，多/漏对象都是失败）："
    head -10 "$OUT/liveset.txt"
    return 1
  fi
  # 等集合比较 ②：账本 completed == 期望存活集合
  if ! diff -u <(sort -u "$live_set") <(sort -u "$OUT/ledger.completed") > "$OUT/ledset.txt"; then
    bad "账本 completed 集合 ≠ 期望存活集合（R4-2）："
    head -10 "$OUT/ledset.txt"
    return 1
  fi
  ok "存活等集合一致：S3 == 账本 completed == 期望存活集合（$s3n 个）"
  # 等集合比较 ③：实际删除操作清单 == 期望删除集合
  if ! diff -u <(sort -u "$del_set") <(sort -u "$deleted_file") > "$OUT/delset.txt"; then
    bad "实际删除操作集合 ≠ 期望删除集合（R4-2，陌生 key/漏删都是失败）："
    head -10 "$OUT/delset.txt"
    return 1
  fi
  # 期望删除集合（权威）逐 key 查账
  dexp=$(grep -c . "$del_set")
  while read -r k <&3; do
    [ -z "$k" ] && continue
    dchk=$((dchk+1))
    if ! st=$(psql_q_soft "select status from file_entries where storage_path='$k'" 2>>"$OUT/query-errors.log"); then
      bad "status 查询失败（${k}）—— 查询失败不能当作合法缺席（N4）"; return 1
    fi
    case "$st" in
      deleted) deld=$((deld+1)) ;;
      "") dabsent=$((dabsent+1)) ;;
      *) dmissing=$((dmissing+1)); echo "    活行: $k (status=$st)" ;;
    esac
  done 3< "$del_set"
  if [ "$dchk" != "$dexp" ]; then
    bad "删除集合断言只检查了 $dchk/$dexp 个（循环故障，F3）"; return 1
  fi
  AC_DELETED=$deld; AC_ABSENT=$dabsent
  if [ "$dmissing" -ne 0 ]; then
    bad "有 $dmissing 个被删对象仍是活行（账本声称存在、S3 已无 —— 真失败）"; return 1
  fi
  if [ "$((deld + dabsent))" != "$dexp" ]; then
    bad "deleted($deld) + 合法缺席($dabsent) ≠ 期望删除集合大小 ${dexp}（N5/R4-2）"; return 1
  fi
  ok "被删对象落账正确：status='deleted' $deld 个 + 合法缺席行 $dabsent 个（集合 $dexp 个全检，无活行，等集合一致）"
  return 0
}

wait_converged() { # <prefix> <deleted-file> <expected-live-set-file> <expected-del-set-file> <timeout-s>
  local prefix=$1 dfile=$2 liveset=$3 delset=$4 timeout=$5 rem
  rem=$(sc_remaining)
  [ "$rem" -lt "$timeout" ] && timeout=$rem
  local deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if converged_q "$prefix" "$dfile" "$liveset" "$delset"; then
      assert_converged "$prefix" "$dfile" "$liveset" "$delset"; return $?
    fi
    sleep_capped 3 || true
  done
  bad "超时未收敛（预算 ${timeout}s）"
  assert_converged "$prefix" "$dfile" "$liveset" "$delset"
  return $?
}

# converged_q — non-counting poll (transients must not inflate FAILURES).
# 轮询也按集合（R4-2）：S3/账本集合与期望存活集合一致、删除操作清单与期望删除
# 集合一致、期望删除 key 状态合法 —— 全部满足才算收敛。
converged_q() { # <prefix> <deleted-file> <expected-live-set-file> <expected-del-set-file>
  local prefix=$1 dfile=$2 liveset=$3 delset=$4 k st
  s3_keys "$prefix" > "$OUT/.q.s3" 2>>"$OUT/query-errors.log" || return 1
  ledger_keys "$prefix" completed > "$OUT/.q.led" 2>>"$OUT/query-errors.log" || return 1
  diff -q <(sort -u "$liveset" 2>/dev/null) <(sort -u "$OUT/.q.s3") >/dev/null 2>&1 || return 1
  diff -q <(sort -u "$liveset" 2>/dev/null) <(sort -u "$OUT/.q.led") >/dev/null 2>&1 || return 1
  diff -q <(sort -u "$delset" 2>/dev/null) <(sort -u "$dfile" 2>/dev/null) >/dev/null 2>&1 || return 1
  while read -r k <&3; do
    [ -z "$k" ] && continue
    if ! st=$(psql_q_soft "select status from file_entries where storage_path='$k'" 2>>"$OUT/query-errors.log"); then
      return 1 # 查询失败 → 未收敛（fail-closed，N4）
    fi
    case "$st" in deleted|"") ;; *) return 1 ;; esac
  done 3< "$delset"
  return 0
}

wait_indexed() { # <prefix> <expect-count> <timeout-s>
  local prefix=$1 expect=$2 timeout=$3 rem n
  rem=$(sc_remaining)
  [ "$rem" -lt "$timeout" ] && timeout=$rem
  local deadline=$(( $(date +%s) + timeout ))
  while [ "$(date +%s)" -lt "$deadline" ]; do
    if ! n=$(psql_q_soft "select count(*) from file_entries where storage_path like '${prefix}%' and status='completed'" 2>>"$OUT/query-errors.log"); then
      bad "wait_indexed 查询失败（${prefix}）—— 查询失败不能当作 0（N4）"; return 1
    fi
    [ "$n" = "$expect" ] && return 0
    sleep_capped 2 || true
  done
  n=$(psql_q_soft "select count(*) from file_entries where storage_path like '${prefix}%' and status='completed'" 2>>"$OUT/query-errors.log" || echo "query-failed")
  bad "wait_indexed 超时：${prefix} 期望 ${expect}，实际 ${n}"
  return 1
}

# preindexed_group <prefix> <n> <plan-file> — 断流前写入 n 个并确认入账；
# 计划记录 key|size|etag（etag 取自 S3 实测，作为账本断言的基准）
preindexed_group() {
  local prefix=$1 n=$2 plan=$3 timeout=$4 i key content size meta etag
  : > "$plan"
  for ((i=1; i<=n; i++)); do
    key=$(printf "%sp%03d" "$prefix" "$i")
    content="pre-content-$key-$(printf 'x%.0s' $(seq 1 100))"
    put_object "$key" "$content" || fatal "pre-indexed put failed: $key"
    size=${#content}
    etag=$(md5_of "$content") || fatal "md5_of 失败：${key}（计划完整性受损）"
    echo "$key|$size|$etag" >> "$plan"
  done
  wait_indexed "$prefix" "$n" "$timeout" || fatal "pre-indexed group ${prefix} 未全部入账"
}

# dump_ovr_diagnostics <plan-file> <ovr-times-file> — per-key evidence for the
# overwrite group (ledger row / S3 HEAD / CP read-back log), exported BEFORE
# cleanup (resume-style requirement).
dump_ovr_diagnostics() {
  local plan=$1 times=$2
  echo "══ s1ovr per-key diagnostics (ledger / S3 HEAD / CP read-back log) ══"
  while IFS='|' read -r k old_size old_etag <&3; do
    echo "== $k"
    echo "  db_old: size=$old_size etag=$old_etag"
    echo "  db_now: $(psql_q_soft "select 'status='||status||' source='||source||' size='||size_bytes||' etag='||coalesce(etag,'')||' observed_at='||observed_at||' updated_at='||updated_at from file_entries where storage_path='$k'" || echo query-failed)"
    echo "  s3_head: $(s3_object_meta "$k")"
    echo "  cp_log_applied_lines: $(grep -c "\"key\":\"$k\"" "$OUT/cp.log" 2>/dev/null || echo 0)"
    echo "  put_done_at_ns: $(grep "^$k " "$times" 2>/dev/null | awk '{print $2}')"
  done 3< "$plan"
}

gap_marks_total()  { psql_q "select count(*) from storage_event_marks where reason='gap'"; }
gap_marks_window() { psql_q "select from_ts_ns, to_ts_ns from storage_event_marks where reason='gap' order by created_at"; }

rm -f "$OUT/writes.jsonl" "$OUT/deletes.jsonl" "$OUT/pre-indexed.txt"
: > "$OUT/result.txt"

# ══ 场景 1：断流重连 ══════════════════════════════════════════════════════════
if should_run 1; then
sc_deadline_set 1500
step "场景 1：断流重连（含协调者补充的「已入账后删除/覆盖」两组）"
note "断流前写入：已入账后删除组 15 个（s1del/）…"
preindexed_group "s1del/" 15 "$PLAN_S1_DEL" 180
note "断流前写入：已入账后覆盖组 15 个（s1ovr/）…"
preindexed_group "s1ovr/" 15 "$PLAN_S1_OVR" 180
ok "两组各 15 个已确认入账（status=completed，计划含实测 etag）"

if ! GAP_BEFORE=$(gap_marks_total); then
  bad "杀代理前读取 gap 标记总数失败 —— 必需读取失败（R4-5）"
  fatal "gap 标记总数读取失败（场景 1 前置）"
fi
note "杀代理前 gap 标记总数：$GAP_BEFORE"
kill -9 "$PROXY_PID" 2>/dev/null; wait "$PROXY_PID" 2>/dev/null
T_OUTAGE_START=$(date +%s)
note "代理已杀（CP↔filer gRPC 断流）；断流窗口起点 $(date '+%H:%M:%S')——窗口由「写完才恢复代理」界定（非固定时长），写入变慢只拉长窗口、不改变被测对象（B7 aws put 变慢的影响登记，见 report-impl）"
note "断流窗口内操作："
# N5：预先固定写 / 删 / 存活三个 key 集合（不随实际写入成功与否收缩）。
S1_N=200; S1_DELN=20; LIVE1_EXPECT=$(( S1_N - S1_DELN ))
S1_KEYS="$OUT/s1-keys.txt"
for i in $(seq -w 1 "$S1_N"); do echo "fi1/p$i"; done > "$S1_KEYS"
DEL1="$OUT/del1.txt"
awk 'NR%10==1' "$S1_KEYS" > "$DEL1"                                    # p001,p011,…,p191 = 20
comm -23 <(sort -u "$S1_KEYS") <(sort -u "$DEL1") > "$OUT/s1-live-keys.txt"   # 固定存活集合 180
note "固定集合：写 $(wc -l < "$S1_KEYS" | tr -d ' ') / 删 $(wc -l < "$DEL1" | tr -d ' ') / 存活 $(wc -l < "$OUT/s1-live-keys.txt" | tr -d ' ')（期望存活 ${LIVE1_EXPECT}）"
note "新建 $S1_N 个（fi1/）"
: > "$PLAN_S1_NEW"
while read -r key <&3; do
  i=${key#fi1/p}
  content="fi1-payload-$i-$(printf 'y%.0s' $(seq 1 50))"
  if put_object "$key" "$content"; then
    echo "$key|${#content}|" >> "$PLAN_S1_NEW"
    echo "$key $(ts_ns)" >> "$OUT/writes.jsonl"
  fi
done 3< "$S1_KEYS"
note "删除 $S1_DELN 个新建对象（fi1/，固定集合 DEL1，含 p001——新建即删的合法缺席样本）"
DEL1_ACTUAL="$OUT/del1-actual.txt"; : > "$DEL1_ACTUAL"
while read -r k <&3; do
  if grep -qxF "$k" "$FAILED_WRITES"; then
    note "跳过删除 ${k}（写入已失败；视为对期望删除计划的偏离，集合断言会失败）"
  else
    delete_object "$k"
    echo "$k" >> "$DEL1_ACTUAL"
  fi
done 3< "$DEL1"
note "删除 15 个「已入账」对象（s1del/）——恢复后必须 status='deleted'"
while read -r k <&3; do delete_object "${k%%|*}"; done 3< "$PLAN_S1_DEL"
note "覆盖 15 个「已入账」对象（s1ovr/，内容 ~111B → ~230B）——恢复后必须 size/etag 匹配新内容"
OVR_TIMES="$OUT/ovr-times.txt"; : > "$OVR_TIMES"
PLAN_S1_OVR_NEW="$OUT/plan-s1-ovr-new.txt"; : > "$PLAN_S1_OVR_NEW"
while read -r kline <&3; do
  k="${kline%%|*}"
  new_content="OVERWRITTEN-$k-$(printf 'z%.0s' $(seq 1 200))"
  if put_object "$k" "$new_content"; then
    echo "$k|${#new_content}|$(md5_of "$new_content")" >> "$PLAN_S1_OVR_NEW"
    echo "$k $(ts_ns)" >> "$OVR_TIMES"
  fi
done 3< "$PLAN_S1_OVR"
sleep_capped 5 || true
OUTAGE_WINDOW=$(( $(date +%s) - T_OUTAGE_START ))
note "断流窗口内写/删/覆盖段结束于 $(date '+%H:%M:%S')（窗口实际 ${OUTAGE_WINDOW}s，全部操作已落在窗口内——恢复代理是最后一步）"
note "恢复代理"
start_proxy

# N5/R4-2：期望集合是固定计划文件（s1-live-keys.txt / DEL1，执行前生成、不改写）；
# 实际存活/删除计数由 assert_converged 查询后写入 AC_LIVE 等
LIVE1_EXPECT=$(wc -l < "$OUT/s1-live-keys.txt" | tr -d ' ')
DEL1_N=$(grep -c . "$DEL1")
if wait_converged "fi1/" "$DEL1_ACTUAL" "$OUT/s1-live-keys.txt" "$DEL1" 300; then SC1_SET=OK; else SC1_SET=FAIL; fi

# 已入账后删除组：全部 status='deleted'（计划数 = 检查数）
SC1_DEL=OK; n_del=0; chk_del=0
while IFS='|' read -r k exp_size exp_etag <&3; do
  chk_del=$((chk_del+1))
  if ! st=$(psql_q "select status from file_entries where storage_path='$k'"); then
    bad "已入账后删除的 $k status 查询失败"; SC1_DEL=FAIL; continue
  fi
  if [ "$st" = "deleted" ]; then n_del=$((n_del+1)); else
    bad "已入账后删除的 $k status=${st}（期望 deleted）"; SC1_DEL=FAIL; fi
done 3< "$PLAN_S1_DEL"
[ "$chk_del" = 15 ] || { bad "已入账后删除组断言只检查了 $chk_del/15 个"; SC1_DEL=FAIL; }
ok "已入账后删除组：$n_del/$chk_del 个 status='deleted'（Removed 回读路径实测）"
[ "$SC1_DEL" = OK ] || SC1_DEL=FAIL

# 已入账后覆盖组：行必须存在、completed、source=storage_readback、size/etag ==
# 新上传内容（etag 与 S3 实测一致）；缺行 = 失败（F3 问题 A）
SC1_OVR=OK; n_ovr=0; chk_ovr=0
while IFS='|' read -r k new_size new_etag <&3; do
  chk_ovr=$((chk_ovr+1))
  if ! row=$(psql_q_soft "select status||'|'||source||'|'||size_bytes||'|'||coalesce(etag,'') from file_entries where storage_path='$k'" 2>>"$OUT/query-errors.log"); then
    bad "已入账后覆盖的 $k 行查询失败 —— 查询失败不能当作行消失（N4/R4-5）"; SC1_OVR=FAIL; continue
  fi
  if [ -z "$row" ]; then
    bad "已入账后覆盖的 $k 行消失（缺席不是通过）"; SC1_OVR=FAIL; continue
  fi
  st=${row%%|*}; rest=${row#*|}; src=${rest%%|*}; rest2=${rest#*|}; led_size=${rest2%%|*}; led_etag=${rest2##*|}
  if [ "$st" = "completed" ] && [ "$src" = "storage_readback" ] && [ "$led_size" = "$new_size" ] && [ "$led_etag" = "$new_etag" ]; then
    n_ovr=$((n_ovr+1))
  else
    bad "已入账后覆盖的 $k 不匹配（st=$st src=$src size=$led_size want=$new_size etag=$led_etag want=${new_etag}）"; SC1_OVR=FAIL
  fi
done 3< "$PLAN_S1_OVR_NEW"
[ "$chk_ovr" = 15 ] || { bad "覆盖组断言只检查了 $chk_ovr/15 个（循环故障，F3）"; SC1_OVR=FAIL; }
ok "已入账后覆盖组：$n_ovr/$chk_ovr 个 completed+storage_readback+size/etag 与新内容一致（缺行即失败）"
[ "$SC1_OVR" = OK ] || SC1_OVR=FAIL

if ! GAP_AFTER="$(gap_marks_total)"; then
  bad "恢复后读取 gap 标记总数失败 —— 必需读取失败（R4-5）"
  fatal "gap 标记总数读取失败（场景 1 断言）"
fi
[ -z "$GAP_AFTER" ] && GAP_AFTER=0
if [ "$GAP_AFTER" -gt "$GAP_BEFORE" ]; then
  ok "storage_event_marks 新增 gap 标记（$GAP_BEFORE -> ${GAP_AFTER}）"; SC1_GAP=OK
else
  bad "断流重连后没有新增 gap 标记（$GAP_BEFORE -> ${GAP_AFTER}）"; SC1_GAP=FAIL
fi
FAILED1=$(wc -l < "$FAILED_WRITES" | tr -d ' ')
SC1_DEL_TOTAL=$(( ${AC_DELETED:-0} + ${AC_ABSENT:-0} ))
echo "SCENARIO1 set=$SC1_SET predel=$SC1_DEL predovr=$SC1_OVR gap=$SC1_GAP | 新建存活 实际=${AC_LIVE:-0}/期望=${LIVE1_EXPECT} | 新建即删合法缺席 deleted 实际=${AC_DELETED:-0}+缺席 实际=${AC_ABSENT:-0}=合计实际 ${SC1_DEL_TOTAL}/期望=${DEL1_N} | 已入账后删除 实际=$n_del/期望=15 | 已入账后覆盖 实际=$n_ovr/期望=15 | 写失败 实际=$FAILED1/期望=0" | tee -a "$OUT/result.txt"
[ "$FAILED1" = 0 ] || { bad "有 $FAILED1 个写入最终失败（见 ${FAILED_WRITES}）"; }
mv -f "$OUT/writes.jsonl" "$OUT/writes-s1.jsonl" 2>/dev/null || true
fi

# ══ 场景 2：filer 优雅重启 ════════════════════════════════════════════════════
if should_run 2; then
sc_deadline_set 900
step "场景 2：filer 优雅重启（持续写入期间 docker compose restart seaweed-filer）"
# N5：独立固定计划——先于写入生成全部 120 个 key|size；成功/失败清单分开；
# 断言 plan=120、delete=10、live=110、failed(场景增量)=0。计划不随成功数缩小。
SC2_FAIL0=$(wc -l < "$FAILED_WRITES" | tr -d ' ')
PLAN_S2="$OUT/plan-s2.txt"; : > "$PLAN_S2"
# 内容固定："fi2-payload-pNNN-"(17B) + 50 个 q(50B) = 67B（p001..p120 等长）
for i in $(seq -w 1 120); do
  echo "fi2/p${i}|67" >> "$PLAN_S2"
done
PLAN2_N=$(wc -l < "$PLAN_S2" | tr -d ' ')
DEL2_EXPECT=10; LIVE2_EXPECT=$(( PLAN2_N - DEL2_EXPECT ))
[ "$PLAN2_N" = 120 ] || fatal "场景 2 计划生成错误（$PLAN2_N ≠ 120）"
# N5：固定删除集合 = 计划前 10 个 key（预先生成，不依赖写入结果）
DEL2="$OUT/del2.txt"
head -10 "$PLAN_S2" | awk -F'|' '{print $1}' > "$DEL2"
comm -23 <(cut -d'|' -f1 "$PLAN_S2" | sort -u) <(sort -u "$DEL2") > "$OUT/s2-live-keys.txt"
: > "$OUT/writes2.jsonl"
# R3-5：写入器放独立进程组（生成的脚本自带单次 put 上限 + 重试），join 有界——
# 超出场景预算则杀整组并计失败，绝不无限等待。
cat > "$OUT/writer-s2.sh" <<'WEOF'
#!/usr/bin/env bash
# 场景 2 持续写入器（由 run.sh 以 in_new_group 启动 → 独立进程组，可整组清理）
# 时间戳引擎由父脚本导出的 TS_ENGINE 提供（ts_ns 的副本，两端同源）。
set -uo pipefail
: "${OUT:?}" "${FAILED_WRITES:?}" "${PLAN_S2:?}"
ts_ns() {
  case "${TS_ENGINE:-}" in
    date)    date +%s%N ;;
    python3) python3 -c 'import time; print(time.time_ns())' ;;
    *)       date +%s ;;
  esac
}


put_bounded() { # <key> <content-file> — 单次 put 带 30s 上限（目标桶与主脚本 put_object 一致；B7: mc cp → aws s3api put-object，CP 身份）
  "$AWSBIN" swcp put-object --bucket data-sensor --key "$1" --body "$2" >/dev/null 2>&1 < /dev/null &
  local pid=$! waited=0
  while kill -0 "$pid" 2>/dev/null; do
    if [ "$waited" -ge 30 ]; then
      kill -KILL "-$pid" 2>/dev/null; kill -KILL "$pid" 2>/dev/null
      wait "$pid" 2>/dev/null; return 124
    fi
    sleep 1; waited=$((waited+1))
  done
  wait "$pid" 2>/dev/null
}
while IFS='|' read -r k exp_size <&3; do
  key="$k"
  content="fi2-payload-${key#fi2/}-$(printf 'q%.0s' $(seq 1 50))"
  printf '%s' "$content" > "$OUT/.putobj.w2"
  done_ok=1
  for try in 1 2 3; do
    if put_bounded "$key" "$OUT/.putobj.w2"; then done_ok=0; break; fi
    sleep 1
  done
  if [ "$done_ok" = 0 ]; then
    got=${#content}
    if [ "$got" != "$exp_size" ]; then
      echo "$key 计划 size=$exp_size 实际=${got}（内容生成不一致）" >> "$OUT/plan-mismatch.txt"
    fi
    echo "$key $(ts_ns)" >> "$OUT/writes2.jsonl"
  else
    echo "$key" >> "$FAILED_WRITES"
  fi
done 3< "$PLAN_S2"
WEOF
T_WRITER_START=$(date +%s)
in_new_group env OUT="$OUT" FAILED_WRITES="$FAILED_WRITES" PLAN_S2="$PLAN_S2" AWSBIN="$AWSBIN" bash "$OUT/writer-s2.sh" &
WRITER_PID=$!
sleep_capped 6 || true
note "重启 seaweed-filer（compose stop_grace_period 保障优雅停机）"
T_RESTART_START=$(date +%s)
T_RESTART_START_NS=$(ts_ns)   # H3：纳秒边界（ts_ns 秒级兜底时为 10 位秒，见 s2_span_classify）
run_to 180 $SWCOMPOSE restart seaweed-filer >>"$OUT/compose-sw.log" 2>&1 || bad "restart 命令失败/超时"
T_RESTART_END=$(date +%s)
T_RESTART_END_NS=$(ts_ns)
echo "  · filer 重启耗时 $(( T_RESTART_END - T_RESTART_START ))s"
# F7：重启后先等 S3 网关真正应答，再做任何删除/断言
wait_s3_ready "场景2 filer 重启后"
T_S2_READY_NS=$(ts_ns)   # G1：网关确认恢复服务的时刻（横跨判据后沿，比 T_RESTART_END 更严）
# R3-5：writer join 有界（场景剩余预算）；超时杀整组并计失败
while kill -0 "$WRITER_PID" 2>/dev/null; do
  if [ "$(sc_remaining)" -le 0 ]; then
    bad "场景2 writer 未在场景预算内结束 —— 杀 writer 进程组"
    kill -KILL "-$WRITER_PID" 2>/dev/null; kill -KILL "$WRITER_PID" 2>/dev/null
    break
  fi
  sleep_capped 2 || true
done
wait "$WRITER_PID" 2>/dev/null; WRITER_PID=""
# G1（返工 2）有效性守卫：判据 = **写入横跨整个重启**，而非「重启窗口内成功
# 写入数 ≥ 1」。filer 停机期间本来就写不进去——写入卡在重试里，恢复之后才
# 成功——所以「窗口内成功数」是偶然值（返工 1 实测 p006 恰好落在重启开始的
# 第一秒才碰巧通过，换一次运行很可能为 0，会把测到了东西的场景误判无效）。
# 新判据（两条任一不满足 → bad + SC2_VALID=FAIL，fail-closed，期望计数不变）：
#   (a) 存在成功写入的时刻 < 重启开始时刻（重启前 writer 确实在写）；
#   (b) 存在成功写入的时刻 > T_S2_READY（wait_s3_ready 成功返回、网关确认
#       恢复服务之后 writer 仍在写；比 T_RESTART_END 更严）。
# H3（评审 r2）：边界与写入时间戳都用 ts_ns 的纳秒、**严格比较**——秒级截断
# 会把「恢复后同一秒的成功写入」漏掉（假阴性）、把「重启开始后同一秒的写入」
# 算成重启前（假阳性）；ts_ns 秒级兜底时明确降级（同秒写入无法排序、不计入
# 任何一侧，见 s2_span_classify 与下方日志）。重启前最后一次成功写入 → 恢复
# 后第一次成功写入的间隔，本身就是「写入在重试中跨过停机」的证据。重启窗口
# 内的成功写入数仅作观测打印、不参与判定。writes2.jsonl 只记成功写入，
# 天然排除失败尝试。
s2_span_classify "$OUT/writes2.jsonl" "$T_RESTART_START_NS" "$T_RESTART_END_NS" "$T_S2_READY_NS"
if [ -n "$S2_PRE_TS" ] && [ -n "$S2_POST_TS" ]; then
  S2_SPAN_GAP=$(( ${S2_POST_TS:0:10} - ${S2_PRE_TS:0:10} ))
else
  S2_SPAN_GAP='-'
fi
note "G1 横跨判据（$( [ "$S2_TS_DEGRADED" = 1 ] && echo '秒级兜底降级：同秒写入无法排序、不计入任何一侧' || echo '纳秒精度、严格比较')）：writer 于重启前 $(( ${T_RESTART_START:0:10} - ${T_WRITER_START:0:10} ))s 启动；重启前最后一次成功写入 ${S2_PRE_KEY} @$(ts_hms "$S2_PRE_TS")、网关恢复（T_S2_READY=$(ts_hms "$T_S2_READY_NS")，restart 窗口 [$T_RESTART_START → $T_RESTART_END]）后第一次成功写入 ${S2_POST_KEY} @$(ts_hms "$S2_POST_TS")，两者间隔 ${S2_SPAN_GAP}s；重启窗口内成功写入 ${S2_WIN_N} 个（仅观测，不参与判定）"
SC2_VALID=OK
if [ -z "$S2_PRE_TS" ]; then
  bad "场景2 有效性守卫（G1a）：重启之前没有任何成功写入——writer 在重启前就写完了（或写入全失败），「持续写入期间重启」未发生，本次场景无效（fail-closed）"
  SC2_VALID=FAIL
fi
if [ -z "$S2_POST_TS" ]; then
  bad "场景2 有效性守卫（G1b）：网关恢复（T_S2_READY）之后没有任何成功写入——写入没有横跨重启，本次场景无效（fail-closed）"
  SC2_VALID=FAIL
fi
DEL2_ACTUAL="$OUT/del2-actual.txt"; : > "$DEL2_ACTUAL"
while read -r k <&3; do
  if grep -qxF "$k" "$FAILED_WRITES"; then
    note "跳过删除 ${k}（写入已失败；视为对期望删除计划的偏离，集合断言会失败）"
  else
    delete_object "$k"
    echo "$k" >> "$DEL2_ACTUAL"
  fi
done 3< "$DEL2"
FAILED2=$(( $(wc -l < "$FAILED_WRITES" | tr -d ' ') - SC2_FAIL0 ))
# R4-2/R3-4：期望集合 = 固定计划文件；wait 失败必须保留为 FAIL
LIVE2_EXPECT=$(wc -l < "$OUT/s2-live-keys.txt" | tr -d ' ')
DEL2_N=$(grep -c . "$DEL2")
DEL2_ACTUAL_N=$(grep -c . "$DEL2_ACTUAL")
if wait_converged "fi2/" "$DEL2_ACTUAL" "$OUT/s2-live-keys.txt" "$DEL2" 300; then SC2=OK; else SC2=FAIL; fi
[ "${SC2_VALID:-OK}" = OK ] || { SC2=FAIL; bad "场景2 因 G1 有效性守卫失败而判 FAIL"; }
LIVE2_ACTUAL=${AC_LIVE:-0}
[ "$PLAN2_N" = 120 ] || { bad "场景2 计划 $PLAN2_N ≠ 120"; SC2=FAIL; }
[ "$DEL2_N" = "$DEL2_EXPECT" ] || { bad "场景2 期望删除集合 ≠ $DEL2_EXPECT 个"; SC2=FAIL; }
[ "$DEL2_ACTUAL_N" = "$DEL2_EXPECT" ] || { bad "场景2 实际删除操作 ≠ $DEL2_EXPECT 个"; SC2=FAIL; }
[ "$LIVE2_ACTUAL" = "$LIVE2_EXPECT" ] || { bad "场景2 存活 $LIVE2_ACTUAL ≠ 期望 $LIVE2_EXPECT"; SC2=FAIL; }
[ "$FAILED2" = 0 ] || { bad "场景2 增量写失败 $FAILED2 个（计入 FAILURES）"; SC2=FAIL; }
SC2_DEL_TOTAL=$(( ${AC_DELETED:-0} + ${AC_ABSENT:-0} ))
echo "SCENARIO2=$SC2 | 新建存活 实际=${LIVE2_ACTUAL}/期望=${LIVE2_EXPECT} | 写入计划 实际=${PLAN2_N}/期望=120 | 删除 实际=${DEL2_ACTUAL_N}/期望=${DEL2_EXPECT} | deleted 实际=${AC_DELETED:-0}+缺席 实际=${AC_ABSENT:-0}=合计实际 ${SC2_DEL_TOTAL}/期望=${DEL2_N} | 写失败 实际=${FAILED2}/期望=0" | tee -a "$OUT/result.txt"
fi

# ══ 场景 3：CP 停机期间写入 ═══════════════════════════════════════════════════
if should_run 3; then
sc_deadline_set 900
step "场景 3：CP 停机期间写入（含「已入账后删除/覆盖」两组）"
note "停机前写入：已入账后删除组 15 个（s3del/）+ 覆盖组 15 个（s3ovr/），确认入账"
preindexed_group "s3del/" 15 "$PLAN_S3_DEL" 180
preindexed_group "s3ovr/" 15 "$PLAN_S3_OVR" 180
CURSOR_BEFORE_STOP=$(psql_q "select declared_ts_ns from storage_event_cursors") || {
  bad "停 CP 前读取持久游标失败 —— 必需读取失败，终止当前场景（R4-5）"
  echo "  ☠ FATAL: 持久游标读取失败（R4-5）" >&2
  exit 1   # ≙ fatal：EXIT trap 仍会完整清理；查询失败的子壳退出必须在这里升级
}
note "停 CP 前的持久游标：$CURSOR_BEFORE_STOP"
stop_and_confirm "$CP_PID" || bad "停 CP 后进程仍存活"; CP_PID=""
T_CPDOWN_START=$(date +%s)
note "CP 已停（停机窗口起点 $(date '+%H:%M:%S')——窗口由「写完才重启 CP」界定，非固定时长，写入变慢不改变被测对象：游标恢复自持久化行）；写入 120 个新对象（fi3/）+ 删除 12 个新建 + 删 15 个已入账 + 覆盖 15 个已入账"
# N5：预先固定写 / 删 / 存活三个 key 集合（不随实际写入成功与否收缩）。
S3_N=120; S3_DELN=12; LIVE3_EXPECT=$(( S3_N - S3_DELN ))
S3_KEYS="$OUT/s3-keys-fi3.txt"
for i in $(seq -w 1 "$S3_N"); do echo "fi3/p$i"; done > "$S3_KEYS"
DEL3="$OUT/del3.txt"
awk 'NR%10==1' "$S3_KEYS" > "$DEL3"                                    # p001,p011,…,p111 = 12
comm -23 <(sort -u "$S3_KEYS") <(sort -u "$DEL3") > "$OUT/s3-live-keys.txt"   # 固定存活集合 108
note "固定集合：写 $(wc -l < "$S3_KEYS" | tr -d ' ') / 删 $(wc -l < "$DEL3" | tr -d ' ') / 存活 $(wc -l < "$OUT/s3-live-keys.txt" | tr -d ' ')（期望存活 ${LIVE3_EXPECT}）"
: > "$PLAN_S3_NEW"
while read -r key <&3; do
  i=${key#fi3/p}
  content="fi3-payload-$i-$(printf 'w%.0s' $(seq 1 50))"
  if put_object "$key" "$content"; then
    echo "$key|${#content}|" >> "$PLAN_S3_NEW"
    echo "$key $(ts_ns)" >> "$OUT/writes.jsonl"
  fi
done 3< "$S3_KEYS"
note "删除 $S3_DELN 个新建对象（fi3/，固定集合 DEL3，含 p001——新建即删的合法缺席样本）"
DEL3_ACTUAL="$OUT/del3-actual.txt"; : > "$DEL3_ACTUAL"
while read -r k <&3; do
  if grep -qxF "$k" "$FAILED_WRITES"; then
    note "跳过删除 ${k}（写入已失败；视为对期望删除计划的偏离，集合断言会失败）"
  else
    delete_object "$k"
    echo "$k" >> "$DEL3_ACTUAL"
  fi
done 3< "$DEL3"
while read -r k <&3; do delete_object "${k%%|*}"; done 3< "$PLAN_S3_DEL"
PLAN_S3_OVR_NEW="$OUT/plan-s3-ovr-new.txt"; : > "$PLAN_S3_OVR_NEW"
while read -r kline <&3; do
  k="${kline%%|*}"
  new_content="OVERWRITTEN-$k-$(printf 'v%.0s' $(seq 1 210))"
  if put_object "$k" "$new_content"; then
    echo "$k|${#new_content}|$(md5_of "$new_content")" >> "$PLAN_S3_OVR_NEW"
  else
    bad "put(overwrite) $k 失败"
  fi
done 3< "$PLAN_S3_OVR"
note "重启 CP（同一 DB：游标来自持久化行；停机窗口实际 $(( $(date +%s) - T_CPDOWN_START ))s，全部写入已落在窗口内——重启 CP 是最后一步）"
start_cp
# N5/R4-2：期望集合 = 固定计划文件（s3-live-keys.txt / DEL3）；实际计数由查询写入 AC_*
LIVE3_EXPECT=$(wc -l < "$OUT/s3-live-keys.txt" | tr -d ' ')
DEL3_N=$(grep -c . "$DEL3")
if wait_converged "fi3/" "$DEL3_ACTUAL" "$OUT/s3-live-keys.txt" "$DEL3" 300; then SC3_SET=OK; else SC3_SET=FAIL; fi
SC3_DEL=OK; n3del=0; chk3del=0
while IFS='|' read -r k exp_size exp_etag <&3; do
  chk3del=$((chk3del+1))
  if ! st=$(psql_q "select status from file_entries where storage_path='$k'"); then
    bad "场景3 已入账后删除的 $k status 查询失败"; SC3_DEL=FAIL; continue
  fi
  if [ "$st" = "deleted" ]; then n3del=$((n3del+1)); else bad "场景3 已入账后删除的 $k status=$st"; SC3_DEL=FAIL; fi
done 3< "$PLAN_S3_DEL"
[ "$chk3del" = 15 ] || { bad "场景3 已入账后删除组断言只检查了 $chk3del/15 个"; SC3_DEL=FAIL; }
ok "场景3 已入账后删除组：$n3del/$chk3del 个 status='deleted'"
SC3_OVR=OK; n3ovr=0; chk3ovr=0
while IFS='|' read -r k new_size new_etag <&3; do
  chk3ovr=$((chk3ovr+1))
  if ! row=$(psql_q_soft "select status||'|'||source||'|'||size_bytes||'|'||coalesce(etag,'') from file_entries where storage_path='$k'" 2>>"$OUT/query-errors.log"); then
    bad "场景3 已入账后覆盖的 $k 行查询失败 —— 查询失败不能当作行消失（N4/R4-5）"; SC3_OVR=FAIL; continue
  fi
  if [ -z "$row" ]; then bad "场景3 已入账后覆盖的 $k 行消失"; SC3_OVR=FAIL; continue; fi
  st=${row%%|*}; rest=${row#*|}; src=${rest%%|*}; rest2=${rest#*|}; led_size=${rest2%%|*}; led_etag=${rest2##*|}
  if [ "$st" = "completed" ] && [ "$src" = "storage_readback" ] && [ "$led_size" = "$new_size" ] && [ "$led_etag" = "$new_etag" ]; then
    n3ovr=$((n3ovr+1))
  else
    bad "场景3 已入账后覆盖的 $k 不匹配（st=$st src=$src size=$led_size want=$new_size etag=$led_etag want=${new_etag}）"; SC3_OVR=FAIL
  fi
done 3< "$PLAN_S3_OVR_NEW"
[ "$chk3ovr" = 15 ] || { bad "场景3 覆盖组断言只检查了 $chk3ovr/15 个"; SC3_OVR=FAIL; }
ok "场景3 已入账后覆盖组：$n3ovr/$chk3ovr 个 completed+storage_readback+size/etag 匹配"
SC3_DEL_TOTAL=$(( ${AC_DELETED:-0} + ${AC_ABSENT:-0} ))
echo "SCENARIO3 set=$SC3_SET predel=$SC3_DEL predovr=$SC3_OVR | 新建存活 实际=${AC_LIVE:-0}/期望=${LIVE3_EXPECT} | 新建即删合法缺席 deleted 实际=${AC_DELETED:-0}+缺席 实际=${AC_ABSENT:-0}=合计实际 ${SC3_DEL_TOTAL}/期望=${DEL3_N} | 已入账后删除 实际=$n3del/期望=15 | 已入账后覆盖 实际=$n3ovr/期望=15" | tee -a "$OUT/result.txt"
fi

# ══ 场景 4：docker kill 硬杀 filer（记录，不要求通过）═════════════════════════
if should_run 4; then
sc_deadline_set 900
step "场景 4：硬杀 filer（docker kill -s KILL 后显式 start + 等 healthy；只记录）"
rm -f "$OUT/writes.jsonl" "$OUT/writes-s4-ok.jsonl"
T_S4W_START=$(date +%s)
note "连续写入 80 个对象（制造硬杀前的写入流，观测其相对落盘窗口的分布）——filer 元数据日志的落盘窗口可达约 60s（docs/reports/storage-layer-eval.md:183）；换用 AWS CLI 后单次写入变慢，窗口内的积压量取决于单次写入耗时，**不再能从「80 个」这个数量推出**（最早的写入可能已落盘）。下面打印本次实测值；场景 4 仍然只记录、不设通过线"
for i in $(seq -w 1 80); do
  # G2（返工 2）：writes.jsonl 原样保留全部写入尝试（含失败的，SCENARIO4_LOST
  # 的丢失对象写入时刻诊断依赖它）；「成功写入」观测另记 writes-s4-ok.jsonl
  # （仅 put_object 成功时追加），F5 的两个观测值只从它计算，失败尝试不再
  # 被算进「成功写入」。
  if put_object "fi4/p$i" "fi4-payload-$i-$(printf 'u%.0s' $(seq 1 60))"; then
    echo "fi4/p$i $(ts_ns)" >> "$OUT/writes-s4-ok.jsonl"
  else
    bad "put fi4/p$i"
  fi
  echo "fi4/p$i $(ts_ns)" >> "$OUT/writes.jsonl"
done
T_S4_KILL=$(date +%s)
# F5（评审 r1）观测值：最后一次成功 put 到 kill 的间隔、kill 前 60s 内的
# 成功写入数——G2 起只从 writes-s4-ok.jsonl（仅成功写入）计算，失败尝试
# 不再计入。时间戳秒级兜底时（length≤10）直接用，纳秒取前 10 位换算成秒。
S4_LAST_PUT_S=$(awk '{ts=$2; if (length(ts)>10) ts=substr(ts,1,10); if (ts+0>m) m=ts+0} END{print m+0}' "$OUT/writes-s4-ok.jsonl")
S4_KILL_GAP=$(( T_S4_KILL - S4_LAST_PUT_S ))
S4_WIN60=$(awk -v lo="$(( T_S4_KILL - 60 ))" '{ts=$2; if (length(ts)>10) ts=substr(ts,1,10); if (ts+0>=lo) n++} END{print n+0}' "$OUT/writes-s4-ok.jsonl")
S4_OK_N=$(grep -c . "$OUT/writes-s4-ok.jsonl" 2>/dev/null)
S4_OK_N=${S4_OK_N:-0}
if [ "${S4_LAST_PUT_S:-0}" -le 0 ]; then
  note "F5 实测：80 次写入尝试全部失败（无成功写入，观测值无意义；场景 4 只记录）"
else
  note "F5 实测：80 次写入尝试成功 ${S4_OK_N} 个，写入段耗时 $(( T_S4_KILL - T_S4W_START ))s（起 $(date -r "$T_S4W_START" '+%H:%M:%S' 2>/dev/null || echo "$T_S4W_START") → 止 $(date '+%H:%M:%S')）；最后一次成功 put 距 kill ${S4_KILL_GAP}s；kill 前 60s 内成功写入 ${S4_WIN60}/${S4_OK_N} 个（其余更早、可能已落盘）——硬杀未落盘量以 SCENARIO4_LOST 的账本对账为准"
fi
cp "$OUT/writes.jsonl" "$OUT/writes-s4.jsonl"
note "docker kill -s KILL $FILER_CONTAINER"
run_to 30 docker kill --signal=KILL "$FILER_CONTAINER" >/dev/null || bad "docker kill 失败"
note "docker kill 不触发 restart 策略（手动停止语义）→ 显式 docker start"
run_to 60 docker start "$FILER_CONTAINER" >/dev/null || bad "docker start 失败"
note "等 filer healthy（上限 300s，且受场景预算约束；每次轮询打印状态）"
fhealthy=0
for i in $(seq 1 60); do
  [ "$(sc_remaining)" -le 0 ] && { bad "场景4 预算耗尽，healthy 轮询提前结束"; break; }
  st=$(run_to 15 docker inspect --format '{{.State.Health.Status}}' "$FILER_CONTAINER" 2>/dev/null < /dev/null || echo inspect-timeout)
  echo "    t=$((i*5))s health=$st"
  [ "$st" = "healthy" ] && { fhealthy=1; break; }
  sleep_capped 5 || true
done
if [ "$fhealthy" = 1 ]; then
  ok "filer 已恢复 healthy"
else
  bad "filer 300s 未恢复 healthy；filer 最近日志："
  run_to 15 docker logs --tail 10 "$FILER_CONTAINER" 2>&1 | sed 's/^/    /'
fi
wait_s3_ready "场景4 filer 硬杀后"
note "再写 20 个（重启后事件正常）"
for i in $(seq -w 81 100); do
  put_object "fi4/p$i" "fi4-payload-$i-$(printf 't%.0s' $(seq 1 60))" || bad "put fi4/p$i"
  echo "fi4/p$i $(ts_ns)" >> "$OUT/writes.jsonl"
done
cp "$OUT/writes.jsonl" "$OUT/writes-s4-full.jsonl"
note "等流稳定（最多 120s，只观察不设通过线）"
sleep_capped 30 || true
if ! s3_keys "fi4/" > "$OUT/s3.fi4"; then bad "场景4 s3 listing 失败（记录为全量丢失口径不可信）"; fi
if ! ledger_keys "fi4/" completed > "$OUT/ledger.fi4"; then bad "场景4 账本 listing 失败（N4）"; fi
comm -23 "$OUT/s3.fi4" "$OUT/ledger.fi4" > "$OUT/fi4.lost" || true
LOST=$(wc -l < "$OUT/fi4.lost" | tr -d ' ')
echo "  · 硬杀后丢失（S3 有、账本无）对象数：$LOST"
head -5 "$OUT/fi4.lost" | sed 's/^/    lost: /'
if [ "$LOST" -gt 0 ]; then
  note "gap 窗口（filer 时钟 ns，最近 5 条）："
  gap_marks_window | tail -5 | sed 's/^/    /'
  note "丢失对象的写入时刻（宿主时钟，仅示意——两个时钟域，不做硬断言）："
  while read -r k <&3; do
    grep "^$k " "$OUT/writes-s4.jsonl" 2>/dev/null | head -1 | sed 's/^/    /'
  done 3< "$OUT/fi4.lost"
else
  note "硬杀后 0 丢失：CP 订阅在线时事件已实时投递并 Apply（D-037 的未落盘丢失类只打击断连期间的写入方）"
fi
echo "SCENARIO4_LOST=$LOST (recorded only)" | tee -a "$OUT/result.txt"
fi

echo ""
echo "══ 结果汇总 ══"
cat "$OUT/result.txt"
if [ -s "$RUN_TO_CLIPPED" ]; then
  echo "── run_to 预算裁剪/拒绝记录（R3-5）──"
  cat "$RUN_TO_CLIPPED"
fi
echo "FAILURES=$FAILURES"
exit $FAILURES
