#!/usr/bin/env bash
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
