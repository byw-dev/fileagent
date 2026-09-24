#!/usr/bin/env bash
# M1b: operations.md config tables  <->  code reality (both directions)
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"

echo "=== CP: documented in operations.md section 1 ==="
sed -n '/## 1. Control Plane 配置参考/,/## 2. Agent 配置参考/p' docs/ops/operations.md \
  | grep -oE '`[A-Z][A-Z_0-9]+`' | tr -d '`' | sort -u > /tmp/doc_cp.txt
cat /tmp/doc_cp.txt

echo; echo "=== CP: actually read by controlplane/internal/config ==="
grep -ohE '(envString|envInt|envBool|envDuration|os\.Getenv)\(\s*"[A-Z_0-9]+"' \
  controlplane/internal/config/*.go | grep -oE '"[A-Z_0-9]+"' | tr -d '"' | sort -u > /tmp/code_cp.txt
cat /tmp/code_cp.txt

echo; echo ">>> DOCUMENTED BUT NOT READ (in config pkg):"
comm -23 /tmp/doc_cp.txt /tmp/code_cp.txt
echo ">>> READ BUT NOT DOCUMENTED:"
comm -13 /tmp/doc_cp.txt /tmp/code_cp.txt

echo; echo "=== AGENT: documented AGENT_* in operations.md section 2 ==="
sed -n '/## 2. Agent 配置参考/,/^## 3/p' docs/ops/operations.md \
  | grep -oE '`AGENT_[A-Z_0-9]+`' | tr -d '`' | sort -u > /tmp/doc_ag.txt
wc -l < /tmp/doc_ag.txt
echo; echo "=== AGENT: env override mechanism in agent code ==="
grep -rn 'AGENT_' --include="*.go" agent/internal/config/ | head -30
