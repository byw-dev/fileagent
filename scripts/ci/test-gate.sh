#!/usr/bin/env bash
export TMPDIR="$PWD/bin"
set -u
set -o pipefail

SCRIPT_DIR=$(cd "$(dirname "$0")" && pwd)
REPO_ROOT=$(cd "$SCRIPT_DIR/../.." && pwd)
cd "$REPO_ROOT" || exit 2

MODULE=${1:-controlplane}
if [ "$MODULE" != "controlplane" ]; then
	printf 'unsupported gate module: %s\n' "$MODULE" >&2
	exit 2
fi

CONF="$REPO_ROOT/ci/gate-controlplane.conf"
OUT_DIR="$REPO_ROOT/.ci-out/controlplane"
mkdir -p "$TMPDIR" "$OUT_DIR"
# This is a repository-owned policy file with shell-style assignments.
# shellcheck disable=SC1090
. "$CONF"

if [ -z "${TEST_DATABASE_URL:-}" ]; then
	export TEST_DATABASE_URL="$TEST_DATABASE_URL_DEFAULT"
fi

problem_count=0
add_problem() {
	problem_count=$((problem_count + 1))
	printf ' - %s\n' "$1" >> "$OUT_DIR/problems.log"
}
: > "$OUT_DIR/problems.log"

printf 'Preflight PostgreSQL reachability...\n'
if ! docker compose -f deploy/docker-compose.test.yml exec -T postgres pg_isready -U fileagent -d fileagent_test >/dev/null 2>&1; then
	printf 'PG not up — run docker compose -f deploy/docker-compose.test.yml up -d --wait postgres\n' >&2
	exit 1
fi

printf '\n== go vet ==\n'
if ! go -C controlplane vet ./...; then
	add_problem 'go vet ./... failed'
fi
if ! go -C controlplane vet -tags=integration ./...; then
	add_problem 'go vet -tags=integration ./... failed'
fi
if ! go -C pkg/trollsift vet ./...; then
	add_problem 'go vet pkg/trollsift failed'
fi

printf '\n== setup migration test: %s ==\n' "$SETUP_TEST"
if ! go -C controlplane test -count=1 -v -tags=integration -p 1 ./internal/db -run "^${SETUP_TEST}$"; then
	printf 'PostgreSQL migration setup failed; verify TEST_DATABASE_URL and the PostgreSQL service.\n' >&2
	exit 1
fi

TEST_LOG="$OUT_DIR/test.log"
COVER_PROFILE="$OUT_DIR/coverage.out"
printf '\n== full Control Plane integration suite ==\n'
test_status=0
go -C controlplane test -count=1 -v -tags=integration -p 1 -covermode=atomic -coverprofile="$COVER_PROFILE" ./... 2>&1 | tee "$TEST_LOG" || test_status=$?
if [ "$test_status" -ne 0 ]; then
	add_problem "full integration go test failed (exit $test_status)"
fi
# Cached results never touched the database: setup-go restores GOCACHE (test cache included) across CI runs.
if grep -Eq '^ok[[:space:]].*\(cached\)' "$TEST_LOG"; then
	add_problem 'some packages reported (cached) results; the gate must execute tests (-count=1)'
fi

printf '\n== required top-level PASS checks ==\n'
required_pass_count=0
required_pass_total=0
for test_name in $REQUIRED_PASS; do
	required_pass_total=$((required_pass_total + 1))
	if grep -Eq "^--- PASS: ${test_name} \\(" "$TEST_LOG"; then
		printf 'ok %s\n' "$test_name"
		required_pass_count=$((required_pass_count + 1))
	else
		printf 'FAIL %s\n' "$test_name"
		add_problem "required top-level test did not PASS: $test_name"
	fi
done
printf 'REQUIRED_PASS: %s/%s passed\n' "$required_pass_count" "$required_pass_total"

printf '\n== semantic marker checks ==\n'
if grep -Fq "$REQUIRED_LOG" "$TEST_LOG"; then
	printf 'ok marker present: %s\n' "$REQUIRED_LOG"
else
	printf 'FAIL marker missing: %s\n' "$REQUIRED_LOG"
	add_problem 'required mutation marker is missing'
fi
result_row_count=$(grep -Ec 'ingest_integration_test\.go:[0-9]+: [^|]+ \| (PASS|FAIL) \|' "$TEST_LOG" || true)
if [ "$result_row_count" -ge "$REQUIRED_LOG_COUNT" ]; then
	printf 'ok mutation result rows: %s (required >= %s)\n' "$result_row_count" "$REQUIRED_LOG_COUNT"
else
	printf 'FAIL mutation result rows: %s (required >= %s)\n' "$result_row_count" "$REQUIRED_LOG_COUNT"
	add_problem "mutation result rows were $result_row_count; required at least $REQUIRED_LOG_COUNT"
fi

printf '\n== top-level skip ledger ==\n'
skip_names=$(awk '/^--- SKIP: / { print $3 }' "$TEST_LOG" | sort -u)
allowed_skip_count=0
while IFS='|' read -r test_name reason restore_condition; do
	[ -n "$test_name" ] || continue
	allowed_skip_count=$((allowed_skip_count + 1))
	if printf '%s\n' "$skip_names" | grep -Fxq "$test_name"; then
		printf 'ok(declared) %s — %s Restore: %s (observed skip)\n' "$test_name" "$reason" "$restore_condition"
	else
		printf 'ok(declared) %s — %s Restore: %s (currently passing)\n' "$test_name" "$reason" "$restore_condition"
	fi
done <<EOF
$ALLOWED_SKIP
EOF
while IFS= read -r skipped_test; do
	[ -n "$skipped_test" ] || continue
	if ! printf '%s\n' "$ALLOWED_SKIP" | cut -d'|' -f1 | grep -Fxq "$skipped_test"; then
		printf 'FAIL undeclared top-level skip: %s\n' "$skipped_test"
		add_problem "undeclared top-level skip: $skipped_test"
	fi
done <<EOF
$skip_names
EOF
printf 'ALLOWED_SKIP: %s declared entries\n' "$allowed_skip_count"

printf '\n== pkg/trollsift tests ==\n'
if ! go -C pkg/trollsift test -count=1 ./...; then
	add_problem 'pkg/trollsift tests failed'
fi

printf '\n== informational coverage ==\n'
if [ -s "$COVER_PROFILE" ]; then
	go tool cover -func="$COVER_PROFILE" | tail -1
else
	printf 'coverage profile unavailable (full test run did not produce one)\n'
fi

if [ "$problem_count" -gt 0 ]; then
	printf '\nGate problems (%s):\n' "$problem_count" >&2
	cat "$OUT_DIR/problems.log" >&2
	exit 1
fi

printf '\nControl Plane integration gate passed.\n'
