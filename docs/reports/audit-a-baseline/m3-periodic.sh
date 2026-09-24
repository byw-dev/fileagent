#!/usr/bin/env bash
# M3: "periodic / 定期 / 每 N / 启动时 / 后台" claims must have a real caller/scheduler.
set -uo pipefail
cd "$(git rev-parse --show-toplevel)"
echo "=== (a) claims inside migrations (high-precision objects per charter §7) ==="
grep -rniE 'periodic|定期|每 ?[0-9]|cleanup|清理|后台|background|启动时|sweep' controlplane/migrations/*.sql || echo "(none)"
echo
echo "=== (b) every goroutine-launched loop in cmd/server ==="
grep -nE '^\s*go [a-zA-Z]' controlplane/cmd/server/main.go
echo
echo "=== (c) exported Run* loops defined in worker/ + handler/, and whether cmd/server calls them ==="
for f in $(grep -rlE 'func .*Run[A-Za-z]*\(ctx' controlplane/internal/worker controlplane/internal/api/handler 2>/dev/null); do
  grep -oE 'func (\([^)]*\) )?Run[A-Za-z]*\(' "$f" | sed "s|^|$f: |"
done
echo
echo "--- called from main.go? ---"
for fn in $(grep -rhoE 'func (\([^)]*\) )?Run[A-Za-z]*\(' controlplane/internal/worker controlplane/internal/api/handler 2>/dev/null | grep -oE 'Run[A-Za-z]*' | sort -u); do
  n=$(grep -c "\.$fn(\|[^a-zA-Z]$fn(" controlplane/cmd/server/main.go)
  printf '%-30s callsites_in_main=%s\n' "$fn" "$n"
done
