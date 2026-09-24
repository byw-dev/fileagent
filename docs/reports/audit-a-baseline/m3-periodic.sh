#!/usr/bin/env bash
#
# ⚠️ 定位：**人工用的候选生成器，不是判定器。仅供参考，不得接入 CI 做机械判断。**
#   本脚本**以 0 退出**并输出大量已知假阳性（错误码常量被当成环境变量、相对路径与
#   示例文件被报成 missing、`MIGRATIONS_PATH` 被报成「文档写了代码没读」——而
#   operations.md 恰恰是在说它已被移除）。输出**必须由人筛**。
#   要做成 CI 闸，先给每条规则加白名单/基线和非零失败语义（见报告 §4）。
#
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
