#!/usr/bin/env bash
# Build the Go tools into ./bin and create the Python venv in ./pyenv.
# GOWORK=off: the repo-root go.work would otherwise claim these nested modules.
# TMPDIR points into this (git-ignored) directory per the repo's temp-file rule.
set -euo pipefail
cd "$(dirname "$0")"
export TMPDIR="$PWD/tmp" GOWORK=off
mkdir -p "$TMPDIR" bin
(cd probe && go build -o ../bin/probe .)
(cd sub && go build -o ../bin/sub .)
if command -v uv >/dev/null 2>&1; then
  [ -d pyenv ] || uv venv --python 3.12 pyenv
  uv pip install --python pyenv/bin/python -r eco/requirements.txt
else
  echo "uv not found: skip Python venv (only needed for eco/eco_test.py)" >&2
fi
echo "built: bin/probe bin/sub$( [ -d pyenv ] && echo ' + pyenv/' )"
