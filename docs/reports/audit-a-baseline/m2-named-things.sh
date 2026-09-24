#!/usr/bin/env bash
#
# ⚠️ 定位：**人工用的候选生成器，不是判定器。仅供参考，不得接入 CI 做机械判断。**
#   本脚本**以 0 退出**并输出大量已知假阳性（错误码常量被当成环境变量、相对路径与
#   示例文件被报成 missing、`MIGRATIONS_PATH` 被报成「文档写了代码没读」——而
#   operations.md 恰恰是在说它已被移除）。输出**必须由人筛**。
#   要做成 CI 闸，先给每条规则加白名单/基线和非零失败语义（见报告 §4）。
#
# M2: every path-like reference in docs/comments must resolve to a real file.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
git ls-files > /tmp/aud_files.txt

echo "=== (a) backticked path-like refs in docs (excl. system-design.md per charter §11) ==="
DOCS=$(git ls-files 'docs/**/*.md' '*.md' | grep -v 'docs/design/system-design.md' | grep -v 'docs/reports/')
grep -hoE '`[A-Za-z0-9_./-]+\.(go|sql|md|sh|yml|yaml|ts|tsx|toml|proto|json|env|service|ps1|txt)[a-zA-Z0-9_:.-]*`' $DOCS \
  | tr -d '`' | sed 's/:.*$//' | sort -u > /tmp/aud_claimed_paths.txt
wc -l < /tmp/aud_claimed_paths.txt

MISSING=0
while read -r p; do
  [ -z "$p" ] && continue
  # resolve: exact, or as suffix of a tracked path, or basename match
  if [ -e "$p" ]; then continue; fi
  if grep -qxF "$p" /tmp/aud_files.txt; then continue; fi
  if grep -qE "(^|/)$(printf '%s' "$p" | sed 's/[.[\*^$]/\\&/g')$" /tmp/aud_files.txt; then continue; fi
  echo "MISSING: $p"
  MISSING=$((MISSING+1))
done < /tmp/aud_claimed_paths.txt
echo "missing_count=$MISSING"
