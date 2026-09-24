#!/usr/bin/env bash
#
# ⚠️ 定位：**人工用的候选生成器，不是判定器。仅供参考，不得接入 CI 做机械判断。**
#   本脚本**以 0 退出**并输出大量已知假阳性（错误码常量被当成环境变量、相对路径与
#   示例文件被报成 missing、`MIGRATIONS_PATH` 被报成「文档写了代码没读」——而
#   operations.md 恰恰是在说它已被移除）。输出**必须由人筛**。
#   要做成 CI 闸，先给每条规则加白名单/基线和非零失败语义（见报告 §4）。
#
# M6: build tags + t.Skip gates vs what CI actually provides.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
echo "### build tags in test files ###"
grep -rln '//go:build' --include="*_test.go" controlplane agent pkg | while read -r f; do
  printf '%-75s %s\n' "$f" "$(grep -m1 '//go:build' "$f")"
done
echo
echo "### t.Skip gates (env-conditional) ###"
grep -rnE 't\.Skip' --include="*_test.go" controlplane agent pkg | sed 's/^/  /'
echo
echo "### env vars those gates read ###"
grep -rhoE 'os\.Getenv\("[A-Z_0-9]+"\)' --include="*_test.go" controlplane agent pkg | sort -u
echo
echo "### does CI provide services / set those vars? ###"
for w in .github/workflows/*.yml; do
  echo "--- $w ---"
  grep -nE 'services:|image:|-tags|go test|FILEAGENT_[A-Z_]*|LIVE|INTEGRATION' "$w" | sed 's/^/  /'
done
