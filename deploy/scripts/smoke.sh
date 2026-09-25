#!/usr/bin/env bash
# smoke.sh — FileAgent 端到端冒烟测试（目标 A 的十二环）
#
# 断言的是**业务不变量**，不是日志文案：最终判据是「下载回来的字节
# 与源文件的 sha256 完全一致」。改日志、改字段名、换实现都不会让它假绿，
# 而链路任何一环断掉都会让它红。
#
# 用法：
#   bash deploy/scripts/smoke.sh              # 本地：自建二进制，隔离环境，结束即清理
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

CP_HTTP_PORT="${CP_HTTP_PORT:-18080}"
CP_GRPC_PORT="${CP_GRPC_PORT:-19090}"

COMPOSE="docker compose -f deploy/docker-compose.dev.yml"
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
# 两者可能不一致。只告警不 fail：排障脚本，有意用本地镜像跑冒烟是合理需求。
if [ -n "${FA_MINIO_IMAGE:-}" ] || grep -q '^FA_MINIO_IMAGE=' deploy/.env 2>/dev/null; then
  EFFECTIVE="${FA_MINIO_IMAGE:-$(grep '^FA_MINIO_IMAGE=' deploy/.env 2>/dev/null | head -1 | cut -d= -f2-)}"
  echo "" >&2
  echo "  ⚠️⚠️ 检测到 FA_MINIO_IMAGE=${EFFECTIVE}" >&2
  echo "  ⚠️ 冒烟将使用该镜像，而不是 D-036 钉定的 digest；预检与 mc 抽取仍按 digest 校验。" >&2
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
    # CP / agent 是宿主进程，KEEP=1 不杀它们；本地忘了收会占住端口，
    # 下一次运行只会表现为「CP /healthz 就绪 超时」，很难联想到是上一次的残留。
    echo "  清理进程：kill ${CP_PID} ${AGENT_PID}   # controlplane / agent"
    return $rc
  fi
  echo ""
  echo "── 清理 ──"
  [ -n "$CP_PID" ] && kill "$CP_PID" 2>/dev/null || true
  [ -n "$AGENT_PID" ] && kill "$AGENT_PID" 2>/dev/null || true
  # -v 连卷一起删：下次必须是全新环境，否则「首次运行路径」就没被验过
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

# ── 前置检查：端口冲突要立刻说清楚 ─────────────────────────────────────────
# 否则症状是 60 秒后的「CP /healthz 就绪 超时」，看不出是端口被占
# （最常见来源：上一次 KEEP=1 运行遗留的 CP 进程）。
if command -v lsof >/dev/null 2>&1; then
for port_spec in "${CP_HTTP_PORT}:Control Plane HTTP" "${CP_GRPC_PORT}:Control Plane gRPC" \
                 "${FA_POSTGRES_PORT}:PostgreSQL" "${FA_REDIS_PORT}:Redis" \
                 "${FA_MINIO_PORT}:MinIO" "${FA_NATS_PORT}:NATS"; do
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
$COMPOSE up -d --wait
ok "PostgreSQL / Redis / MinIO / NATS 全部 healthy"

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
DATABASE_URL="postgres://fileagent:fileagent@127.0.0.1:${FA_POSTGRES_PORT}/fileagent?sslmode=disable" \
REDIS_URL="redis://127.0.0.1:${FA_REDIS_PORT}/0" \
NATS_URL="nats://127.0.0.1:${FA_NATS_PORT}" \
MINIO_ENDPOINT="127.0.0.1:${FA_MINIO_PORT}" \
MINIO_PUBLIC_ENDPOINT="127.0.0.1:${FA_MINIO_PORT}" \
MINIO_USE_SSL=false \
MINIO_ACCESS_KEY=cpAdminIAM000000000 \
MINIO_SECRET_KEY=cpAdminSecret00000000 \
JWT_SECRET=smoke-jwt-secret-not-for-production-use \
INTERNAL_WEBHOOK_SECRET="$WEBHOOK_SECRET" \
HTTP_PORT="$CP_HTTP_PORT" \
GRPC_PORT="$CP_GRPC_PORT" \
BOOTSTRAP_ADMIN_USERNAME="$ADMIN_USER" \
BOOTSTRAP_ADMIN_PASSWORD="$ADMIN_PASS" \
BOOTSTRAP_ADMIN_FORCE_RESET=true \
BOOTSTRAP_ADMIN_CREDENTIALS_FILE="$WORK/bootstrap.txt" \
  "$BIN_DIR/controlplane" > "$WORK/cp.log" 2>&1 &
CP_PID=$!

wait_for "CP /healthz 就绪" 60 \
  curl -sf -o /dev/null "http://127.0.0.1:${CP_HTTP_PORT}/healthz"
grep -q "migrations applied successfully\|schema already up to date" "$WORK/cp.log" \
  || fail "迁移未在启动时自动应用（D-023 声称内嵌自动迁移）"
ok "内嵌迁移已自动应用"

# ── MinIO 初始化 ───────────────────────────────────────────────────────────
# ⚠️ 硬顺序：必须在 CP 起来之后跑。MinIO 在 `mc admin config set` 时会**真的去
#    拨** webhook 端点，CP 没起来就直接失败。deployment.md 路线 A 的 A.2→A.3
#    顺序是对的，dev 文档以前没写。
step "初始化 MinIO（buckets / IAM / webhook）"
MINIO_ENDPOINT="http://localhost:${FA_MINIO_PORT}" \
WEBHOOK_ENDPOINT="http://host.docker.internal:${CP_HTTP_PORT}/internal/minio-event" \
WEBHOOK_AUTH_TOKEN="$WEBHOOK_SECRET" \
  bash deploy/scripts/init-minio.sh > "$WORK/init-minio.log" 2>&1 \
  || { sed -n '$p;/ERROR/p' "$WORK/init-minio.log" >&2; fail "init-minio.sh 失败（完整日志：$WORK/init-minio.log）"; }
ok "buckets + IAM + webhook 就绪，三项自检通过"

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
# 同时规则用的是**默认** append_mode（overwrite，无防抖），所以这条护栏
# 守的是真正的默认路径，而不是 close_wait 自己。
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
DB_SOURCE="$(fe source)"
[ "$DB_SOURCE" = "agent" ] || fail "source='${DB_SOURCE}'，期望 'agent' —— 索引行不是 agent 上报写的（IC-2a 主路径回归，退化成了 webhook 兜底）"
DB_SHA="$(fe sha256)"
[ "$DB_SHA" = "$SRC_SHA" ] || fail "索引的 sha256 与源文件不符：${DB_SHA} != ${SRC_SHA}"
[ "$(fe agent_id)" != "<null>" ] || fail "file_entries.agent_id 为空（归属信息丢失）"
[ "$(fe rule_id)"  != "<null>" ] || fail "file_entries.rule_id 为空（归属信息丢失）"
ok "索引行字段正确（source=agent / sha256 / agent_id / rule_id）"

# 写放大护栏：规则用的是默认 append_mode（overwrite），文件是原子 mv 进来的，
# 所以恰好 1 条是确定的。默认模式一旦再次出现重复上传（审计实测一个 150MB
# 文件产生 150 次完整上传），这里就会红。
UPLOADS="$(psql_q "select count(*) from upload_logs where storage_path like 'smoke/%'")"
[ "$UPLOADS" = "1" ] || fail "upload_logs 有 $UPLOADS 条记录，期望 1 条（重复上传 / 写放大）"
ok "上传次数为 1（无写放大）"

# ── webhook 投递（配置契约护栏）────────────────────────────────────────────
# 上面所有断言都要求 source='agent'，也就是**主路径**。这意味着 webhook 整条
# 链路（MinIO 通知 → CP 鉴权 → 索引）就算完全坏掉，上面也全是绿的——
# INTERNAL_WEBHOOK_SECRET 与 init-minio.sh 的 WEBHOOK_AUTH_TOKEN 一旦漂移，
# CP 会 401 拒绝所有事件，而没有任何断言会红（评审实测：故意写错 token 仍全绿）。
# 所以这里绕开 agent 直接往 bucket 里放一个对象，断言它以 source='minio_event'
# 进索引——这是对账兜底能力的唯一探针。
step "webhook 投递（绕过 agent 的对账兜底路径）"
printf 'webhook probe\n' > "$WORK/probe.txt"
mc cp --quiet "$WORK/probe.txt" "myminio/data-sensor/probe/webhook-probe.txt" >/dev/null \
  || fail "mc cp 失败（无法投放 webhook 探针对象）"
wait_for "MinIO 事件经 webhook 进索引（source=minio_event）" 90 probe_is_indexed

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
echo " SMOKE PASSED — 目标 A 的十二环全部通过"
echo "════════════════════════════════════════════════════════"
