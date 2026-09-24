#!/usr/bin/env bash
#
# ⚠️ 定位：**人工用的候选生成器，不是判定器。仅供参考，不得接入 CI 做机械判断。**
#   本脚本**以 0 退出**并输出大量已知假阳性（错误码常量被当成环境变量、相对路径与
#   示例文件被报成 missing、`MIGRATIONS_PATH` 被报成「文档写了代码没读」——而
#   operations.md 恰恰是在说它已被移除）。输出**必须由人筛**。
#   要做成 CI 闸，先给每条规则加白名单/基线和非零失败语义（见报告 §4）。
#
# M1: every env-var-looking name CLAIMED in docs/comments/deploy must be READ by code.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

# --- (a) env vars the code actually reads ---
{
  # Go: os.Getenv("X"), envString("X",...), envInt, envBool, envDuration, LookupEnv
  grep -rhoE '(os\.Getenv|os\.LookupEnv|envString|envInt|envBool|envDuration|envInt64)\(\s*"[A-Z_0-9]+"' \
    --include="*.go" controlplane agent pkg 2>/dev/null | grep -oE '"[A-Z_0-9]+"' | tr -d '"'
  # Vite/webui
  grep -rhoE 'import\.meta\.env\.[A-Z_0-9]+' --include="*.ts" --include="*.tsx" webui/src 2>/dev/null | sed 's/.*env\.//'
  # shell scripts: ${X:-...} / ${X}
  grep -rhoE '\$\{[A-Z_][A-Z_0-9]*(:-|\}|:?)' deploy/scripts 2>/dev/null | grep -oE '[A-Z_][A-Z_0-9]*'
} | sort -u > /tmp/aud_read.txt

# --- (b) env vars CLAIMED somewhere ---
# docs (excluding system-design.md per charter §11), Go comments, compose, env samples
{
  grep -rhoE '\b[A-Z][A-Z_0-9]{4,}\b' \
    --include="*.md" docs/ops docs/design/contracts.md docs/design/consistency-and-ingest.md README.md CLAUDE.md 2>/dev/null
  grep -rhoE '^\s*//.*\b[A-Z][A-Z_0-9]{4,}\b' --include="*.go" controlplane agent pkg 2>/dev/null | grep -oE '\b[A-Z][A-Z_0-9]{4,}\b'
  grep -rhoE '\b[A-Z][A-Z_0-9]{4,}\b' deploy/docker-compose.dev.yml deploy/docker-compose.prod.yml deploy/docker-compose.test.yml deploy/systemd/*.service 2>/dev/null
} | sort -u > /tmp/aud_claimed.txt

# --- (c) claimed but never read; filter obvious non-env noise ---
comm -23 /tmp/aud_claimed.txt /tmp/aud_read.txt \
 | grep -E '_' \
 | grep -vE '^(CLAUDE|DECISIONS|TASK_LIST|MEMORY|SCHEMA|TIMESTAMPTZ|POSTGRES_[A-Z]+|MINIO_ROOT_[A-Z]+|MINIO_NOTIFY_.*|REDIS_[A-Z]*PASSWORD|NATS_[A-Z]*|IC_BUG.*|[A-Z]+_SQL)$'
