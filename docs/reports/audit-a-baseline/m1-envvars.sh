#!/usr/bin/env bash
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
