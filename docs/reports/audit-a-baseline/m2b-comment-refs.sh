#!/usr/bin/env bash
#
# ⚠️ 定位：**人工用的候选生成器，不是判定器。仅供参考，不得接入 CI 做机械判断。**
#   本脚本**以 0 退出**并输出大量已知假阳性（错误码常量被当成环境变量、相对路径与
#   示例文件被报成 missing、`MIGRATIONS_PATH` 被报成「文档写了代码没读」——而
#   operations.md 恰恰是在说它已被移除）。输出**必须由人筛**。
#   要做成 CI 闸，先给每条规则加白名单/基线和非零失败语义（见报告 §4）。
#
# M2b: file paths + Go identifiers named INSIDE code/migration comments must exist.
# Charter §7: comments are high-precision objects — no "future plan" excuse.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
git ls-files > /tmp/aud_files.txt

echo "### A. file paths named in Go/SQL comments ###"
grep -rnE '(^|[[:space:]])(//|--|#).*[A-Za-z0-9_/-]+\.(go|sql|sh|ts|tsx|toml|proto|yml|yaml|md)\b' \
   --include="*.go" --include="*.sql" controlplane agent pkg api 2>/dev/null \
 | grep -oE '[A-Za-z0-9_][A-Za-z0-9_./-]*\.(go|sql|sh|ts|tsx|toml|proto|yml|yaml|md)\b' \
 | sort -u > /tmp/aud_c_paths.txt
wc -l < /tmp/aud_c_paths.txt
while read -r p; do
  [ -z "$p" ] && continue
  [ -e "$p" ] && continue
  grep -qxF "$p" /tmp/aud_files.txt && continue
  grep -qE "(^|/)$(printf '%s' "$p" | sed 's/[.[\*^$]/\\&/g')$" /tmp/aud_files.txt && continue
  echo "UNRESOLVED: $p"
done < /tmp/aud_c_paths.txt
