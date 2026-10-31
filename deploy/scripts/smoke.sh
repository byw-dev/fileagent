#!/usr/bin/env bash
# smoke.sh — FileAgent 端到端冒烟测试（目标 A 的十二环）
#
# 断言的是**业务不变量**，不是日志文案：最终判据是「下载回来的字节
# 与源文件的 sha256 完全一致」。改日志、改字段名、换实现都不会让它假绿，
# 而链路任何一环断掉都会让它红。
#
# 用法：
#   bash deploy/scripts/smoke.sh              # 本地：自建二进制，隔离环境，结束即清理
#   FA_STORAGE=seaweedfs bash deploy/scripts/smoke.sh
#                                            # 同上，但对象存储用 SeaweedFS 拓扑
#                                              （deploy/docker-compose.seaweedfs.yml +
#                                              init-seaweedfs.sh；存储事件环断言
#                                              filer 订阅 → S3 回读 → 索引，见
#                                              下方「存储事件回读」一节）
#   FA_BIN_DIR=bin bash deploy/scripts/smoke.sh   # CI：用上游 job 构建好的二进制
#   KEEP=1 bash deploy/scripts/smoke.sh        # 失败排查：保留环境不清理
#
# ⚠️ 环境隔离：本脚本默认用**独立的 compose 项目名、容器名前缀和高位端口**，
#    因此可以与开发者正在跑的 dev 环境（fileagent-dev-*，5432/6379/9000/…）
#    并存而互不影响。不要把这些默认值改回 dev 的端口。
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
cd "$REPO_ROOT"

# ── 隔离参数（全部可覆盖，默认值刻意避开 dev 环境）──────────────────────────
export COMPOSE_PROJECT_NAME="${COMPOSE_PROJECT_NAME:-fileagent-smoke}"
export FA_CONTAINER_PREFIX="${FA_CONTAINER_PREFIX:-fileagent-smoke}"
export FA_POSTGRES_PORT="${FA_POSTGRES_PORT:-15432}"
export FA_REDIS_PORT="${FA_REDIS_PORT:-16379}"
export FA_MINIO_PORT="${FA_MINIO_PORT:-19000}"
export FA_MINIO_CONSOLE_PORT="${FA_MINIO_CONSOLE_PORT:-19001}"
export FA_NATS_PORT="${FA_NATS_PORT:-14222}"
export FA_NATS_MONITOR_PORT="${FA_NATS_MONITOR_PORT:-18222}"

# 后端选择：minio（默认，行为与历史版本完全一致）或 seaweedfs（D-037 目标
# 后端）。seaweedfs 分支下 MinIO 不启动，对象存储由 SeaweedFS 拓扑提供
#（deploy/docker-compose.seaweedfs.yml），初始化走 init-seaweedfs.sh。
FA_STORAGE="${FA_STORAGE:-minio}"
case "$FA_STORAGE" in
  minio|seaweedfs) ;;
  *) echo "✗ FA_STORAGE 必须是 minio 或 seaweedfs，得到：$FA_STORAGE" >&2; exit 1 ;;
esac

# SeaweedFS 拓扑的宿主端口（默认放 3xxxx 段，避开 dev 与本脚本的 1xxxx 段）。
export FA_SEAWEEDFS_MASTER_PORT="${FA_SEAWEEDFS_MASTER_PORT:-39333}"
export FA_SEAWEEDFS_S3_PORT="${FA_SEAWEEDFS_S3_PORT:-38333}"
export FA_SEAWEEDFS_FILER_HTTP_PORT="${FA_SEAWEEDFS_FILER_HTTP_PORT:-38888}"
export FA_SEAWEEDFS_FILER_GRPC_PORT="${FA_SEAWEEDFS_FILER_GRPC_PORT:-38889}"

CP_HTTP_PORT="${CP_HTTP_PORT:-18080}"
CP_GRPC_PORT="${CP_GRPC_PORT:-19090}"

COMPOSE="docker compose -f deploy/docker-compose.dev.yml"
SW_COMPOSE="docker compose -f deploy/docker-compose.seaweedfs.yml"
BIN_DIR="${FA_BIN_DIR:-}"
WORK="${FA_WORK_DIR:-$(mktemp -d "${TMPDIR:-/tmp}/fileagent-smoke.XXXXXX")}"
mkdir -p "$WORK"
# mc 的配置是**全局**的（~/.mc/config.json）。init-minio.sh 会 `mc alias set myminio ...`，
# 不隔离就会把开发者指向 dev MinIO 的同名 alias 覆盖成 smoke 的高位端口，
# 且清理阶段无法还原——容器/端口/卷隔离得再干净，这一处仍会污染宿主。
export MC_CONFIG_DIR="$WORK/mc"
CP_PID=""
AGENT_PID=""

# D-036 护栏：FA_MINIO_IMAGE 是离线交付的逃生舱（docs/ops/deployment.md §0.1②）。
# compose 会自动加载 deploy/.env，所以除了 shell 环境还要查文件——开发机上一份
# 遗留的 deploy/.env 会让冒烟跑另一个镜像，而预检与抽 mc 仍按 digest 校验，
# 两者可能不一致。grep 不锚死行首：行首带空格的写法 compose 照样生效，不能漏检。
# 只告警不 fail：排障脚本，有意用本地镜像跑冒烟是合理需求。
if [ -n "${FA_MINIO_IMAGE:-}" ] || grep -qE '^[[:space:]]*FA_MINIO_IMAGE=' deploy/.env 2>/dev/null; then
  EFFECTIVE="${FA_MINIO_IMAGE:-$(grep -E '^[[:space:]]*FA_MINIO_IMAGE=' deploy/.env 2>/dev/null | head -1 | cut -d= -f2- | tr -d '"')}"
  echo "" >&2
  echo "  ⚠️⚠️ 检测到 FA_MINIO_IMAGE=${EFFECTIVE}" >&2
  echo "  ⚠️ 冒烟将使用该镜像，而不是 D-036 钉定的 digest；预检与 mc 抽取仍按 digest 校验（CI 里；本地跑本脚本没有这两步）。" >&2
  echo "  ⚠️ 若非有意为之，请清空它（检查 shell 环境 与 deploy/.env）。" >&2
fi

ADMIN_USER="admin"
ADMIN_PASS="SmokeAdmin@2026"
WEBHOOK_SECRET="smoke-webhook-secret"

# ── 清理 ───────────────────────────────────────────────────────────────────
cleanup() {
  local rc=$?
  if [ "${KEEP:-0}" = "1" ]; then
    echo ""
    echo "KEEP=1 — 保留环境供排查：项目 ${COMPOSE_PROJECT_NAME}，工作目录 ${WORK}"
    echo "  清理容器：docker compose -p ${COMPOSE_PROJECT_NAME} -f deploy/docker-compose.dev.yml down -v"
    if [ "$FA_STORAGE" = "seaweedfs" ]; then
      echo "  清理 SeaweedFS：docker compose -p ${COMPOSE_PROJECT_NAME} -f deploy/docker-compose.seaweedfs.yml down -v"
    fi
    # CP / agent 是宿主进程，KEEP=1 不杀它们；本地忘了收会占住端口，
    # 下一次运行只会表现为「CP /healthz 就绪 超时」，很难联想到是上一次的残留。
    echo "  清理进程：kill ${CP_PID} ${AGENT_PID}   # controlplane / agent"
    return $rc
  fi
  echo ""
  echo "── 清理 ──"
  [ -n "$CP_PID" ] && kill "$CP_PID" 2>/dev/null || true
  [ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null || true
  # -v 连卷一起删：下次必须是全新环境，否则「首次运行路径」就没被验过。
  # SeaweedFS 先停（filer→volume→master 的优雅停机由 compose 反向依赖保证），
  # 否则它还挂在共享网络上，基础 compose 的网络删除会被拖住。
  if [ "$FA_STORAGE" = "seaweedfs" ]; then
    $SW_COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
  fi
  $COMPOSE down -v --remove-orphans >/dev/null 2>&1 || true
  rm -rf "$WORK"
  return $rc
}
trap cleanup EXIT

step()  { echo ""; echo "── $* ──"; }
ok()    { echo "  ✓ $*"; }
fail()  { echo "  ✗ $*" >&2; exit 1; }

# wait_for <描述> <超时秒> <命令...>
# 轮询而不是 sleep：审批轮询等固定常数会让 sleep 写法间歇性红。
wait_for() {
  local desc="$1" timeout="$2"; shift 2
  local deadline=$(( $(date +%s) + timeout ))
  until "$@" >/dev/null 2>&1; do
    if [ "$(date +%s)" -ge "$deadline" ]; then
      fail "超时：${desc}（${timeout}s）"
    fi
    sleep 2
  done
  ok "$desc"
}

psql_q() { $COMPOSE exec -T postgres psql -U fileagent -d fileagent -tAc "$1"; }
api()    { curl -sS -H "Authorization: Bearer $TOKEN" "$@"; }

# 修 2：sha256 在 macOS 是 shasum，在 Linux/GHA runner 上通常是 sha256sum。
# 这条断言是整个冒烟的最终判据，不能因为平台差异而失灵。
sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then sha256sum "$1" | cut -d' ' -f1
  else shasum -a 256 "$1" | cut -d' ' -f1; fi
}

# wait_for 的谓词写成函数（而非 bash -c 字符串），否则子 shell 里没有上面这些函数
agent_is_pending() { psql_q "select 1 from agents where status='pending'" | grep -q 1; }
file_is_indexed()  { psql_q "select 1 from file_entries where file_name='smoke.csv' and status='completed'" | grep -q 1; }
probe_is_indexed() { psql_q "select 1 from file_entries where storage_path='probe/webhook-probe.txt' and source='minio_event'" | grep -q 1; }
# 存储事件回读环（seaweedfs 分支）：建与删两向都断言（D1 的两个产出方向）。
readback_is_indexed() { psql_q "select 1 from file_entries where storage_path='probe/readback-probe.txt' and source='storage_readback' and status='completed'" | grep -q 1; }
readback_is_deleted() { psql_q "select 1 from file_entries where storage_path='probe/readback-probe.txt' and source='storage_readback' and status='deleted'" | grep -q 1; }

# ── 前置检查：端口冲突要立刻说清楚 ─────────────────────────────────────────
# 否则症状是 60 秒后的「CP /healthz 就绪 超时」，看不出是端口被占
# （最常见来源：上一次 KEEP=1 运行遗留的 CP 进程）。
# 存储端口按后端选择：minio 查 FA_MINIO_PORT，seaweedfs 查四个拓扑端口。
if [ "$FA_STORAGE" = "seaweedfs" ]; then
  STORAGE_PORT_SPECS=(
    "${FA_SEAWEEDFS_S3_PORT}:SeaweedFS S3"
    "${FA_SEAWEEDFS_FILER_HTTP_PORT}:SeaweedFS filer HTTP"
    "${FA_SEAWEEDFS_FILER_GRPC_PORT}:SeaweedFS filer gRPC"
    "${FA_SEAWEEDFS_MASTER_PORT}:SeaweedFS master"
  )
else
  STORAGE_PORT_SPECS=("${FA_MINIO_PORT}:MinIO")
fi
if command -v lsof >/dev/null 2>&1; then
for port_spec in "${CP_HTTP_PORT}:Control Plane HTTP" "${CP_GRPC_PORT}:Control Plane gRPC" \
                 "${FA_POSTGRES_PORT}:PostgreSQL" "${FA_REDIS_PORT}:Redis" \
                 "${STORAGE_PORT_SPECS[@]}" "${FA_NATS_PORT}:NATS"; do
  port="${port_spec%%:*}"; what="${port_spec#*:}"
  if lsof -nP -iTCP:"$port" -sTCP:LISTEN >/dev/null 2>&1; then
    echo "  ✗ 端口 ${port}（${what}）已被占用：" >&2
    lsof -nP -iTCP:"$port" -sTCP:LISTEN 2>/dev/null | tail -n +2 | sed 's/^/      /' >&2
    fail "先释放该端口，或用环境变量指定别的端口（见脚本头部注释）"
  fi
done
fi  # 无 lsof 则跳过（不是所有环境都装了；CI 上端口本来就是干净的）

# ── ① 基础设施 ─────────────────────────────────────────────────────────────
step "① 拉起基础设施（独立项目 ${COMPOSE_PROJECT_NAME}）"
if [ "$FA_STORAGE" = "seaweedfs" ]; then
  # MinIO 不启动（过渡期两套后端并存，但一次冒烟只验一套）。
  $COMPOSE up -d --wait postgres redis nats
  # 先渲染 s3/iam 配置再起拓扑：docker 对不存在的 bind mount 会静默建出空目录。
  bash deploy/scripts/render-seaweedfs-configs.sh > "$WORK/render-seaweedfs.log" 2>&1 \
    || { cat "$WORK/render-seaweedfs.log" >&2; fail "render-seaweedfs-configs.sh 失败"; }
  $SW_COMPOSE up -d --wait
  ok "PostgreSQL / Redis / NATS + SeaweedFS（master/volume/filer+S3）全部 healthy"
else
  $COMPOSE up -d --wait
  ok "PostgreSQL / Redis / MinIO / NATS 全部 healthy"
fi

# ── 二进制 ─────────────────────────────────────────────────────────────────
step "构建/获取二进制"
if [ -n "$BIN_DIR" ] && [ -x "$BIN_DIR/controlplane" ] && [ -x "$BIN_DIR/agent" ]; then
  ok "复用已构建产物：$BIN_DIR"
else
  BIN_DIR="bin"
  # -tags webui：目标 A 含 Web UI，纯 API 构建下 GET / 会 404
  (cd controlplane && go build -tags webui -o "../$BIN_DIR/controlplane" ./cmd/server)
  (cd agent && go build -o "../$BIN_DIR/agent" ./cmd/agent)
  ok "本地构建完成（含 webui tag）"
fi

# ── ① CP 启动 ──────────────────────────────────────────────────────────────
step "① 启动 Control Plane"
# 对象存储端点按后端切换：env 变量名不变（MINIO_*，改名不在本刀，spec 第 4 条）。
if [ "$FA_STORAGE" = "seaweedfs" ]; then
  CP_S3_ENDPOINT="127.0.0.1:${FA_SEAWEEDFS_S3_PORT}"
  # CP 凭据与 role ARN 必须与 renderer / init 同一来源（返工 P2-2）：默认值只在
  # deploy/seaweedfs/env-defaults.sh 定义一处；覆盖只需 export SW_CP_ACCESS_KEY /
  # SW_CP_SECRET_KEY / SW_AGENT_ROLE_ARN，三者（渲染/自检/CP）自动一致。
  # 注意 role ARN 账号段是 12 个零（MinIO 默认是空段）。
  # shellcheck source=/dev/null
  source deploy/seaweedfs/env-defaults.sh
  CP_ACCESS_KEY="$SW_CP_ACCESS_KEY"
  CP_SECRET_KEY="$SW_CP_SECRET_KEY"
  export MINIO_ROLE_ARN="$SW_AGENT_ROLE_ARN"
else
  CP_S3_ENDPOINT="127.0.0.1:${FA_MINIO_PORT}"
  CP_ACCESS_KEY="cpAdminIAM000000000"
  CP_SECRET_KEY="cpAdminSecret00000000"
fi
# 存储事件源按后端切换（B3-PR3b D6）：seaweedfs 分支起 filer 订阅适配器
# （明文 filer gRPC 必须显式 INSECURE=true，CP 才肯启动；dev/隔离环境专用）。
# minio 分支显式走默认值 minio_webhook（现状行为；显式写出以避免空数组在
# bash 3.2 + set -u 下的展开问题）。
if [ "$FA_STORAGE" = "seaweedfs" ]; then
  STORAGE_EVENT_ENV=(
    STORAGE_EVENT_SOURCE=seaweedfs_filer
    SEAWEEDFS_FILER_GRPC_ADDR="127.0.0.1:${FA_SEAWEEDFS_FILER_GRPC_PORT}"
    SEAWEEDFS_FILER_INSECURE=true
  )
else
  STORAGE_EVENT_ENV=(STORAGE_EVENT_SOURCE=minio_webhook)
fi
DATABASE_URL="postgres://fileagent:fileagent@127.0.0.1:${FA_POSTGRES_PORT}/fileagent?sslmode=disable" \
REDIS_URL="redis://127.0.0.1:${FA_REDIS_PORT}/0" \
NATS_URL="nats://127.0.0.1:${FA_NATS_PORT}" \
MINIO_ENDPOINT="$CP_S3_ENDPOINT" \
MINIO_PUBLIC_ENDPOINT="$CP_S3_ENDPOINT" \
MINIO_USE_SSL=false \
MINIO_ACCESS_KEY="$CP_ACCESS_KEY" \
MINIO_SECRET_KEY="$CP_SECRET_KEY" \
JWT_SECRET=smoke-jwt-secret-not-for-production-use \
INTERNAL_WEBHOOK_SECRET="$WEBHOOK_SECRET" \
HTTP_PORT="$CP_HTTP_PORT" \
GRPC_PORT="$CP_GRPC_PORT" \
BOOTSTRAP_ADMIN_USERNAME="$ADMIN_USER" \
BOOTSTRAP_ADMIN_PASSWORD="$ADMIN_PASS" \
BOOTSTRAP_ADMIN_FORCE_RESET=true \
BOOTSTRAP_ADMIN_CREDENTIALS_FILE="$WORK/bootstrap.txt" \
  env "${STORAGE_EVENT_ENV[@]}" \
  "$BIN_DIR/controlplane" > "$WORK/cp.log" 2>&1 &
CP_PID=$!

wait_for "CP /healthz 就绪" 60 \
  curl -sf -o /dev/null "http://127.0.0.1:${CP_HTTP_PORT}/healthz"
grep -q "migrations applied successfully\|schema already up to date" "$WORK/cp.log" \
  || fail "迁移未在启动时自动应用（D-023 声称内嵌自动迁移）"
ok "内嵌迁移已自动应用"

# ── 存储初始化 ─────────────────────────────────────────────────────────────
# ⚠️ 硬顺序：必须在 CP 起来之后跑（minio 分支）。MinIO 在 `mc admin config set`
#    时会**真的去拨** webhook 端点，CP 没起来就直接失败。deployment.md 路线 A
#    的 A.2→A.3 顺序是对的，dev 文档以前没写。
#    seaweedfs 分支的存储事件源已随 PR3b 接线（CP 启动即订阅 filer），保持
#    同一顺序只是让两条分支的步骤排列一致。
if [ "$FA_STORAGE" = "seaweedfs" ]; then
  step "初始化 SeaweedFS（buckets / 生命周期 / 三项自检）"
  # B7：init-seaweedfs.sh 自管私有 AWS CLI 配置（mktemp + trap 清理，不碰
  # ~/.aws），smoke 不再传 MC_CONFIG_DIR。
  SW_S3_ENDPOINT="http://localhost:${FA_SEAWEEDFS_S3_PORT}" \
    bash deploy/scripts/init-seaweedfs.sh > "$WORK/init-seaweedfs.log" 2>&1 \
    || { tail -n 5 "$WORK/init-seaweedfs.log" >&2; fail "init-seaweedfs.sh 失败（完整日志：$WORK/init-seaweedfs.log）"; }
  ok "buckets + 生命周期就绪，CP 身份自检（AssumeRole / 建桶 / 预签名 GET）通过"
else
  step "初始化 MinIO（buckets / IAM / webhook）"
  MINIO_ENDPOINT="http://localhost:${FA_MINIO_PORT}" \
  WEBHOOK_ENDPOINT="http://host.docker.internal:${CP_HTTP_PORT}/internal/minio-event" \
  WEBHOOK_AUTH_TOKEN="$WEBHOOK_SECRET" \
    bash deploy/scripts/init-minio.sh > "$WORK/init-minio.log" 2>&1 \
    || { sed -n '$p;/ERROR/p' "$WORK/init-minio.log" >&2; fail "init-minio.sh 失败（完整日志：$WORK/init-minio.log）"; }
  ok "buckets + IAM + webhook 就绪，三项自检通过"
fi

# ── ② 登录 + Web UI ────────────────────────────────────────────────────────
step "② 管理员登录 / Web UI"
TOKEN="$(curl -sS -X POST "http://127.0.0.1:${CP_HTTP_PORT}/api/auth/login" \
  -H 'Content-Type: application/json' \
  -d "{\"username\":\"$ADMIN_USER\",\"password\":\"$ADMIN_PASS\"}" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["access_token"])')"
[ -n "$TOKEN" ] || fail "登录未返回 access_token"
ok "REST 登录成功"

# A 含 Web UI：纯 API 构建（无 webui tag）这里会是 404
UI_CODE="$(curl -s -o "$WORK/index.html" -w '%{http_code}' "http://127.0.0.1:${CP_HTTP_PORT}/")"
[ "$UI_CODE" = "200" ] || fail "GET / 返回 ${UI_CODE}，Web UI 未嵌入（需 -tags webui / make bundle）"
grep -qi '<div id="root"\|<title>' "$WORK/index.html" || fail "GET / 返回 200 但不像 SPA 首页"
# 只验首页会放过「壳在、资产全 404」的 bundle。取 index.html 引用的第一个 JS chunk
# 真下一遍：嵌入不完整 / dist 拷贝漏文件时这里才会红。
UI_ASSET="$(grep -oE '/assets/[^"]+\.js' "$WORK/index.html" | head -1)"
[ -n "$UI_ASSET" ] || fail "index.html 未引用任何 /assets/*.js，Web UI 产物不完整"
ASSET_CODE="$(curl -s -o "$WORK/asset.js" -w '%{http_code}' "http://127.0.0.1:${CP_HTTP_PORT}${UI_ASSET}")"
[ "$ASSET_CODE" = "200" ] || fail "SPA 资产 ${UI_ASSET} 返回 ${ASSET_CODE}，Web UI 无法运行"
[ "$(wc -c < "$WORK/asset.js")" -gt 1000 ] || fail "SPA 资产 ${UI_ASSET} 过小，疑似错误页而非 JS"
ok "Web UI 已随二进制提供（首页 + 资产 ${UI_ASSET} 均可取）"

# ── ③ Agent 注册 → 审批 → RUNNING ─────────────────────────────────────────
step "③ Agent 注册 → 审批 → gRPC 连接"
mkdir -p "$WORK/agent/data" "$WORK/watch"
cat > "$WORK/agent/config.toml" <<CFG
[server]
endpoint = "127.0.0.1:${CP_GRPC_PORT}"
tls_ca_cert = ""
tls_insecure = true

[agent]
fingerprint_file = "$WORK/agent/data/fingerprint"
token_file = "$WORK/agent/data/token.enc"
data_dir = "$WORK/agent/data"

[upload]
concurrency = 3
part_size_mb = 64
queue_max_size = 10000
retry_max = 10
min_timeout_seconds = 300
assumed_upload_bytes_per_second = 1048576

[metrics]
enabled = false
port = 19100

[log]
level = "info"
output = "stdout"
max_size_mb = 100
max_backups = 7
CFG

"$BIN_DIR/agent" --config "$WORK/agent/config.toml" > "$WORK/agent.log" 2>&1 &
AGENT_PID=$!

wait_for "agent 注册为 pending" 60 agent_is_pending
AGENT_ID="$(psql_q "select id from agents order by created_at desc limit 1")"
[ -n "$AGENT_ID" ] || fail "agents 表没有注册记录"

api -X POST -H 'Content-Type: application/json' \
  "http://127.0.0.1:${CP_HTTP_PORT}/api/v1/agents/${AGENT_ID}/approve" -d '{}' >/dev/null
ok "审批已提交（agent 侧轮询间隔硬编码 30s，见 registration.go:155）"

# 这一跳是全流程最慢的，必须轮询而不是 sleep
wait_for "agent 进入 RUNNING（gRPC 流建立）" 120 \
  grep -q "agent: running" "$WORK/agent.log"

# ── ⑤⑥ bucket + 采集规则 ──────────────────────────────────────────────────
step "⑤⑥ bucket 与采集规则"
BUCKET_ID="$(api "http://127.0.0.1:${CP_HTTP_PORT}/api/v1/buckets" \
  | python3 -c 'import json,sys; print([b["id"] for b in json.load(sys.stdin)["items"] if b["name"]=="data-sensor"][0])')"
[ -n "$BUCKET_ID" ] || fail "data-sensor bucket 未出现在 API 列表里"
ok "bucket data-sensor 可用"

RULE_HTTP="$(api -o "$WORK/rule.json" -w '%{http_code}' \
  -X POST -H 'Content-Type: application/json' \
  "http://127.0.0.1:${CP_HTTP_PORT}/api/v1/agents/${AGENT_ID}/rules" -d "{
    \"bucket_id\": \"$BUCKET_ID\",
    \"name\": \"smoke-rule\",
    \"mode\": \"watch\",
    \"base_path\": \"$WORK/watch\",
    \"path_pattern\": \"{name}.csv\",
    \"dest_path_template\": \"smoke/{submit_time:yyyy/MM/dd}/{filename}\",
    \"recursive\": true,
    \"enabled\": true
  }")"
[ "$RULE_HTTP" = "201" ] || fail "建规则返回 ${RULE_HTTP}（期望 201）：$(cat "$WORK/rule.json")"
ok "采集规则已创建"

# ── ④ STS 凭据（规则存在后才会下发）────────────────────────────────────────
wait_for "④ STS 凭据已下发给 agent" 60 \
  grep -q "STS credentials updated" "$WORK/agent.log"

# ── ⑦⑧⑨⑩ 落文件 → 采集 → 直传 → 索引 ────────────────────────────────────
step "⑦⑧⑨⑩ 落文件 → 采集 → 直传 MinIO → 索引"
# 原子落盘：先写到监听目录之外，再 mv 进去。
# 这样只产生一个 CREATE 事件，断言「上传恰好 1 次」才是确定性的；
# 同时规则用的是**默认** append_mode（overwrite——自 D-035 起走普适防抖，
# 原子 mv 只产生一个 CREATE 事件、防抖窗口过后恰好一次交付），所以这条
# 护栏守的就是默认路径本身。
mkdir -p "$WORK/stage"
printf 'ts,sensor,value\n2026-01-01T00:00:00Z,s1,42.5\n' > "$WORK/stage/smoke.csv"
mv "$WORK/stage/smoke.csv" "$WORK/watch/smoke.csv"
SRC_SHA="$(sha256_of "$WORK/watch/smoke.csv")"

wait_for "file_entries 出现索引行" 120 file_is_indexed

# 逐字段单独查询，不做位置解析：字段为 NULL 时 psql -tA 会输出空串，
# 一次性 read 会整体串位，把「source 错了」误报成「sha256 错了」（变异 M2 实测踩到）。
fe() { psql_q "select coalesce(${1}::text,'<null>') from file_entries where file_name='smoke.csv'"; }

# source 放在最前面断言：agent 不上报时 webhook 兜底路径**仍会建出行**，
# 所以「有没有行」挡不住 IC-2a 回归，「行是谁写的」才挡得住。
# seaweedfs 分支（PR #135 返工 1 修正）：filer 订阅开启后，S3 回读也会为同一
# 对象建行/刷新行（D-037：事件只是提示，回读才是事实），且回读行的
# observed_at（HEAD 前的 DB 时钟）**可能晚于** agent 行的 now()——尤其在高负载
# 下——此时行的 source 合法地变为 'storage_readback'（upsert 的 COALESCE 保留
# agent 写入的 sha256/agent_id/rule_id）。因此 seaweedfs 分支改用「agent 专属
# 字段」断言 agent 主路径（upload_logs 只有 agent 写；agent_id/rule_id 只有
# agent 上报填），source 允许两者；minio 分支维持严格 source='agent'。
if [ "$FA_STORAGE" = "seaweedfs" ]; then
  agent_row_is_source_of_truth() {
    [ "$(psql_q "select count(*) from file_entries where file_name='smoke.csv' and agent_id is not null and rule_id is not null")" -ge 1 ]
  }
  wait_for "file_entries 出现 agent 上报写入的索引行（agent_id/rule_id 已填）" 120 agent_row_is_source_of_truth
  DB_SOURCE="$(fe source)"
  case "$DB_SOURCE" in
    agent|storage_readback) ;;
    *) fail "source='${DB_SOURCE}'，期望 'agent' 或 'storage_readback'（seaweedfs：回读行可能后到并以更晚时钟覆盖）" ;;
  esac
fi
if [ "$FA_STORAGE" != "seaweedfs" ]; then
  DB_SOURCE="$(fe source)"
  [ "$DB_SOURCE" = "agent" ] || fail "source='${DB_SOURCE}'，期望 'agent' —— 索引行不是 agent 上报写的（IC-2a 主路径回归，退化成了 webhook 兜底）"
fi
DB_SHA="$(fe sha256)"
[ "$DB_SHA" = "$SRC_SHA" ] || fail "索引的 sha256 与源文件不符：${DB_SHA} != ${SRC_SHA}"
[ "$(fe agent_id)" != "<null>" ] || fail "file_entries.agent_id 为空（归属信息丢失）"
[ "$(fe rule_id)"  != "<null>" ] || fail "file_entries.rule_id 为空（归属信息丢失）"
ok "索引行字段正确（source=${DB_SOURCE} / sha256 / agent_id / rule_id）"

# 写放大护栏（原子落盘）：规则用的是默认 append_mode（overwrite），文件是原子 mv 进来的，
# 所以恰好 1 条是确定的。默认模式一旦再次出现重复上传，这里就会红。
# ⚠️ 按文件名收窄：下面 bulk.csv 护栏的 upload_logs 也落在 smoke/ 前缀下，
#    用 'smoke/%' 一把数会把两个文件混在一起（AUD-9 护栏引入时踩过）。
UPLOADS="$(psql_q "select count(*) from upload_logs where storage_path like 'smoke/%smoke.csv'")"
[ "$UPLOADS" = "1" ] || fail "smoke.csv 的 upload_logs 有 $UPLOADS 条记录，期望 1 条（重复上传 / 写放大）"
ok "上传次数为 1（无写放大）"

# 写放大护栏（分块写，AUD-9 / D-035）：上一条护栏用的是原子 mv（只产生一个 CREATE 事件），
# 抓不到「边写边传」——审计实测 150MB 文件 cp 进监听目录产生 150 次完整上传（≈22GB），
# 其中 3 次是写到一半的内容。这里直接 cp 一个 16MB 文件进监听目录（一路 Write 事件），
# 断言：恰好上传 1 次，且索引里的 sha256 与源文件一致（防「上传了写到一半的内容」）。
# 防抖普适（D-035）后写入静默 500ms 才触发一次整文件上传，所以「恰好 1」是确定的。
step "写放大护栏（分块写：cp 产生持续 Write 事件）"
dd if=/dev/urandom of="$WORK/stage/bulk.csv" bs=1M count=16 2>/dev/null
cp "$WORK/stage/bulk.csv" "$WORK/watch/bulk.csv"
BULK_SHA="$(sha256_of "$WORK/stage/bulk.csv")"

bulk_is_indexed() { psql_q "select 1 from file_entries where file_name='bulk.csv' and status='completed'" | grep -q 1; }
wait_for "bulk.csv（16MB，cp 直写）进入索引" 120 bulk_is_indexed

# 进入索引只证明第一次上传完成；晚到的第二次上传仍可能在路上。再跨过
# 三个 1s 观察间隔，要求每轮终态计数都稳定为 1，才能排除延迟写放大。
for BULK_STABLE_ROUND in 1 2 3; do
  sleep 1
  BULK_UPLOADS="$(psql_q "select count(*) from upload_logs where storage_path like 'smoke/%bulk.csv'")"
  case "$BULK_UPLOADS" in
    1) ;;
    0) fail "bulk.csv 已进索引但 upload_logs 仍为 0（终态确认第 ${BULK_STABLE_ROUND}/3 轮）" ;;
    *) fail "bulk.csv 多传了：upload_logs 已有 $BULK_UPLOADS 条，期望终态恰好 1 条（写放大回归，AUD-9）" ;;
  esac
done
BULK_DB_SHA="$(psql_q "select coalesce(sha256::text,'<null>') from file_entries where file_name='bulk.csv' limit 1")"
[ "$BULK_DB_SHA" = "$BULK_SHA" ] || fail "bulk.csv 索引的 sha256 与源文件不符：$BULK_DB_SHA != $BULK_SHA —— 上传了写到一半的内容（截断上传回归）"
ok "16MB 分块写终态连续 3 轮均为 1 次上传，且 sha256 与源文件一致（无写放大、无截断上传）"

# ── 存储事件回读（配置契约护栏）────────────────────────────────────────────
# 上面所有断言都要求 source='agent'，也就是**主路径**。这意味着存储事件源
# 整条链路就算完全坏掉，上面也全是绿的——所以这里绕开 agent 直接往 bucket 里
# 放一个探针对象。两条分支各验各的事件链路，机制不同、断言同等严格：
#   - minio：MinIO 通知 → CP webhook → 索引（source='minio_event'）；
#   - seaweedfs：filer 元数据订阅 → S3 HEAD 回读 → 索引（source='storage_readback'），
#     再删掉探针对象，断言软删除也经回读落账（status='deleted'）——这是
#     D1「事件只是提示、按回读写账」两向（建/删）的唯一探针。
if [ "$FA_STORAGE" = "seaweedfs" ]; then
  step "存储事件回读（绕过 agent：filer 订阅 → S3 回读 → 索引）"
  # AWS 探针用 ADMIN 身份（CP 身份按 iam.json.tpl 刻意没有 s3:DeleteObject；
  # 探针要删对象，借 admin 的全权限，与被测的 CP 链路无关）。B7：mc → aws；
  # 私有 AWS 配置落在 ${WORK}（mktemp 目录，cleanup 随 WORK 一并删除），
  # 绝不触碰开发者的 ~/.aws。
  SW_AWS_DIR="$(mktemp -d "$WORK/aws.XXXXXX")" \
    || fail "创建私有 AWS 凭据目录失败（mktemp -d $WORK/aws.XXXXXX）"
  # H1（评审 r2）：凭据目录每次 mktemp -d 新建——不是既有路径、不可能被换成
  # 指向 ~/.aws 的符号链接（旧实现 mkdir -p "$WORK/aws" 会跟随链接，chmod/rm/
  # 重定向全部命中真实配置）。mktemp 目录本身即 0700；KEEP=1 时 WORK 整体
  # 保留，aws.XXXXXX 诊断目录随之保留（约定不变）。
  ( umask 077; cat > "$SW_AWS_DIR/config" <<'SWAWSCFG'
[profile swadmin]
region = us-east-1
s3 =
  addressing_style = path
# retry 显式写死（F6）：standard + max_attempts=3。理由：探针只有 put/delete
# 两次短调用，且发生在 init 全部自检通过之后（网关已健康、无重启窗口）；
# 失败应快速红在冒烟里。显式写值而不依赖 CLI 默认——swaws 的白名单已清掉
# 调用者环境里的 AWS_RETRY_MODE/AWS_MAX_ATTEMPTS，这里的值就是权威。
retry_mode = standard
max_attempts = 3
SWAWSCFG
  )
  ( umask 077; cat > "$SW_AWS_DIR/credentials" <<SWAWSCRED
[swadmin]
aws_access_key_id = $SW_ADMIN_ACCESS_KEY
aws_secret_access_key = $SW_ADMIN_SECRET_KEY
SWAWSCRED
  )
  swaws() { # <aws s3api args...> — admin 身份 + 显式 endpoint 的私有 aws 调用（恒走 s3api）
    # F2（评审 r1）白名单：清掉调用者环境里的**全部** AWS_*（凭据/region 的
    # 优先级高于 profile 文件；AWS_MAX_ATTEMPTS/AWS_RETRY_MODE/AWS_CA_BUNDLE/
    # AWS_ENDPOINT_URL* 等同样不得漏入），再显式设置本探针需要的。
    local _v
    for _v in $(compgen -e); do
      case "$_v" in AWS_*|AWS) unset "$_v" ;; esac
    done
    AWS_PROFILE=swadmin \
    AWS_CONFIG_FILE="$SW_AWS_DIR/config" \
    AWS_SHARED_CREDENTIALS_FILE="$SW_AWS_DIR/credentials" \
    AWS_PAGER="" AWS_EC2_METADATA_DISABLED=true \
    aws --endpoint-url "http://localhost:${FA_SEAWEEDFS_S3_PORT}" --no-cli-pager s3api "$@"
  }
  printf 'readback probe\n' > "$WORK/probe.txt"
  swaws put-object --bucket data-sensor --key probe/readback-probe.txt --body "$WORK/probe.txt" >/dev/null \
    || fail "aws put-object 失败（无法投放回读探针对象）"
  wait_for "filer 事件经回读进索引（source=storage_readback）" 90 readback_is_indexed
  # J1（评审 r3）：探针删除用 `--if-match '*'` 条件删除——对象不存在即失败
  # （SeaweedFS 实测返回 PreconditionFailed），与「删除前确认对象存在」同义且
  # 原子；旧实现只看 delete rc，对象已被删掉/丢失时会假成功。
  swaws delete-object --bucket data-sensor --key probe/readback-probe.txt --if-match '*' >/dev/null \
    || fail "aws delete-object（--if-match 条件删除）失败（探针对象不存在或删除请求出错）"
  wait_for "回读删除进索引（status=deleted）" 90 readback_is_deleted
else
  step "webhook 投递（绕过 agent 的对账兜底路径）"
  printf 'webhook probe\n' > "$WORK/probe.txt"
  mc cp --quiet "$WORK/probe.txt" "myminio/data-sensor/probe/webhook-probe.txt" >/dev/null \
    || fail "mc cp 失败（无法投放 webhook 探针对象）"
  wait_for "MinIO 事件经 webhook 进索引（source=minio_event）" 90 probe_is_indexed
fi

# ── ⑪ 列表查询 ─────────────────────────────────────────────────────────────
step "⑪ 文件列表 / 查询"
FILE_ID="$(api "http://127.0.0.1:${CP_HTTP_PORT}/api/v1/files?limit=10" \
  | python3 -c 'import json,sys; d=json.load(sys.stdin); print([f["id"] for f in d["items"] if f["filename"]=="smoke.csv"][0])')"
[ -n "$FILE_ID" ] || fail "文件未出现在 /api/v1/files 列表中"
ok "列表 API 能查到该文件"

# ── ⑫ 下载并逐字节校验 ─────────────────────────────────────────────────────
step "⑫ 预签名下载 + 内容校验"
DL_URL="$(api "http://127.0.0.1:${CP_HTTP_PORT}/api/v1/files/${FILE_ID}/download-url" \
  | python3 -c 'import json,sys; print(json.load(sys.stdin)["url"])')"
[ -n "$DL_URL" ] || fail "download-url 未返回 url"

DL_CODE="$(curl -s -o "$WORK/downloaded.csv" -w '%{http_code}' "$DL_URL")"
[ "$DL_CODE" = "200" ] || fail "预签名 GET 返回 ${DL_CODE}（期望 200）"

DL_SHA="$(sha256_of "$WORK/downloaded.csv")"
[ "$DL_SHA" = "$SRC_SHA" ] || fail "下载内容与源文件不一致：$DL_SHA != $SRC_SHA"
ok "下载内容与源文件逐字节一致（${SRC_SHA}）"

echo ""
echo "════════════════════════════════════════════════════════"
# 两条分支都只在**真的全过**时走到这里（任何一环失败都会被 fail 截停），
# 横幅可以如实打印同一句话；后端区别放在括号里备查。
if [ "$FA_STORAGE" = "seaweedfs" ]; then
  echo " SMOKE PASSED — 目标 A 的十二环全部通过（seaweedfs：存储事件环走 filer 订阅回读）"
else
  echo " SMOKE PASSED — 目标 A 的十二环全部通过"
fi
echo "════════════════════════════════════════════════════════"
