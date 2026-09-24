#!/usr/bin/env bash
#
# ⚠️ 定位：**人工用的候选生成器，不是判定器。仅供参考，不得接入 CI 做机械判断。**
#   本脚本**以 0 退出**并输出大量已知假阳性（错误码常量被当成环境变量、相对路径与
#   示例文件被报成 missing、`MIGRATIONS_PATH` 被报成「文档写了代码没读」——而
#   operations.md 恰恰是在说它已被移除）。输出**必须由人筛**。
#   要做成 CI 闸，先给每条规则加白名单/基线和非零失败语义（见报告 §4）。
#
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
