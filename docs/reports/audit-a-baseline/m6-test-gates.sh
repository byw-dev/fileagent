#!/usr/bin/env bash
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
