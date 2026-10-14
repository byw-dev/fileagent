#!/usr/bin/env bash
# render-seaweedfs-configs.sh — render SeaweedFS S3/IAM configs from templates.
#
# SeaweedFS reads identities/policies from JSON files (-s3.config /
# -s3.iam.config) and does NOT expand environment variables inside them, so
# credentials cannot be injected natively (live-checked against 4.48; D-037
# only confirms WEED_* env vars for flags like the JWT signing keys). This
# script fills the placeholders in deploy/seaweedfs/{s3,iam}.json.tpl with the
# values below and writes the result to deploy/config/seaweedfs/ (gitignored).
#
# It is IDEMPOTENT and side-effect-free apart from (re)writing those two files:
# safe to run before every `docker compose up`.
#
# Requirements: python3 (REQUIRED, not optional). Replacement is JSON-aware —
# values are inserted as JSON string nodes by python's json module, so ANY
# characters in credentials (quotes, backslashes, &, unicode) survive
# byte-faithfully; the previous sed-based renderer corrupted them (rework
# P2-1). python3 is already a hard dependency of smoke.sh (token parsing) and
# of the CI runners that execute it, so this adds no new environment demand.
#
# Writes are ATOMIC per pair: both files are rendered to temporary files,
# validated (legal JSON + no unreplaced placeholder), and only then renamed
# into place — a failed render never overwrites the previous good configs.
#
# The DEFAULTS below are development placeholders shared with
# init-seaweedfs.sh and the seaweedfs branch of smoke.sh via
# deploy/seaweedfs/env-defaults.sh (single source of truth) — they are NOT
# secrets. Real deployments MUST override every SW_* variable.
#
# Environment variables (all have dev defaults):
#   SW_ADMIN_ACCESS_KEY   storage admin identity key id
#   SW_ADMIN_SECRET_KEY   storage admin identity secret
#   SW_CP_ACCESS_KEY      Control Plane identity key id
#   SW_CP_SECRET_KEY      Control Plane identity secret
#   SW_STS_SIGNING_KEY    STS token signing key — MUST be valid standard
#                         base64 decoding to >= 16 BYTES (live-verified 4.48
#                         requirement; the renderer enforces it up front with
#                         a clear error instead of letting the filer start
#                         with a broken STS service)
#   SW_AGENT_ROLE_ARN     STS role ARN CP assumes
#   SW_CONFIG_DIR         output directory                 (default: deploy/config/seaweedfs)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEPLOY_DIR="${REPO_ROOT}/deploy"

# Defaults live in exactly one place (env-defaults.sh), sourced here and by
# init-seaweedfs.sh / smoke.sh so overrides cannot drift between them.
# shellcheck source=/dev/null
source "${DEPLOY_DIR}/seaweedfs/env-defaults.sh"
SW_CONFIG_DIR="${SW_CONFIG_DIR:-${DEPLOY_DIR}/config/seaweedfs}"

if ! command -v python3 >/dev/null 2>&1; then
  echo "ERROR: python3 is required (JSON-aware credential rendering, rework P2-1)." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Guards: refuse empty values early, by variable name, instead of letting an
# empty credential silently land in the rendered file.
# ---------------------------------------------------------------------------
require_nonempty() {
  if [ -z "${!1}" ]; then
    echo "ERROR: ${1} must not be empty." >&2
    exit 1
  fi
}
require_nonempty SW_ADMIN_ACCESS_KEY
require_nonempty SW_ADMIN_SECRET_KEY
require_nonempty SW_CP_ACCESS_KEY
require_nonempty SW_CP_SECRET_KEY
require_nonempty SW_STS_SIGNING_KEY
require_nonempty SW_AGENT_ROLE_ARN

# ---------------------------------------------------------------------------
# STS signing key contract (live-verified on SeaweedFS 4.48, rework P2-4):
#   - must decode as STANDARD base64 (else the filer logs "illegal base64
#     data at input byte N" and refuses to load IAM);
#   - must decode to >= 16 BYTES — NOT >= 16 characters: a 20-char key
#     decoding to 14 bytes is rejected ("signing key must be at least 16
#     bytes") while the filer still reports HTTP-healthy. Reject it here,
#     where the message can tell the operator what to do.
# ---------------------------------------------------------------------------
python3_check_sts_key() {
  local py_error
  py_error="$(SW_STS_SIGNING_KEY="${SW_STS_SIGNING_KEY}" python3 - 2>&1 <<'PYEOF' || true
import base64, os, sys
key = os.environ["SW_STS_SIGNING_KEY"]
try:
    decoded = base64.b64decode(key, validate=True)
except Exception as exc:
    sys.exit(f"SW_STS_SIGNING_KEY is not valid standard base64: {exc}")
if len(decoded) < 16:
    sys.exit(
        f"SW_STS_SIGNING_KEY decodes to {len(decoded)} bytes; "
        f"SeaweedFS requires at least 16 DECODED bytes."
    )
PYEOF
)"
  if [ -n "${py_error}" ]; then
    echo "ERROR: invalid SW_STS_SIGNING_KEY: ${py_error}" >&2
    echo "       It must be STANDARD base64 (no raw multi-byte secrets) decoding to" >&2
    echo "       at least 16 bytes. Generate one with:  openssl rand -base64 32" >&2
    return 1
  fi
}
python3_check_sts_key

# ---------------------------------------------------------------------------
# JSON-aware render: replace any template string node that is EXACTLY a
# __SW_*__ placeholder with the matching environment value. json.dump performs
# all string escaping, so arbitrary characters in values are preserved
# byte-faithfully on read-back (rework P2-1 regression:
# SW_CP_SECRET_KEY='prod&se\cr"et').
# ---------------------------------------------------------------------------
render() { # <template> <output-temp-file>
  SW_ADMIN_ACCESS_KEY="${SW_ADMIN_ACCESS_KEY}" \
  SW_ADMIN_SECRET_KEY="${SW_ADMIN_SECRET_KEY}" \
  SW_CP_ACCESS_KEY="${SW_CP_ACCESS_KEY}" \
  SW_CP_SECRET_KEY="${SW_CP_SECRET_KEY}" \
  SW_STS_SIGNING_KEY="${SW_STS_SIGNING_KEY}" \
  SW_AGENT_ROLE_ARN="${SW_AGENT_ROLE_ARN}" \
  python3 - "$1" "$2" <<'PYEOF'
import json, os, sys

PLACEHOLDER_TO_VAR = {
    "__SW_ADMIN_ACCESS_KEY__": "SW_ADMIN_ACCESS_KEY",
    "__SW_ADMIN_SECRET_KEY__": "SW_ADMIN_SECRET_KEY",
    "__SW_CP_ACCESS_KEY__": "SW_CP_ACCESS_KEY",
    "__SW_CP_SECRET_KEY__": "SW_CP_SECRET_KEY",
    "__SW_STS_SIGNING_KEY__": "SW_STS_SIGNING_KEY",
    "__SW_AGENT_ROLE_ARN__": "SW_AGENT_ROLE_ARN",
}

def substitute(node):
    if isinstance(node, dict):
        return {k: substitute(v) for k, v in node.items()}
    if isinstance(node, list):
        return [substitute(v) for v in node]
    if isinstance(node, str):
        var = PLACEHOLDER_TO_VAR.get(node)
        return os.environ[var] if var is not None else node
    return node

src, dst = sys.argv[1], sys.argv[2]
with open(src, "r", encoding="utf-8") as f:
    doc = substitute(json.load(f))

# SeaweedFS resolves an AssumeRole RoleArn by its trailing role-name segment
# and looks that name up in iam.json's roles[].roleName (live-verified on
# 4.48: a roleArn suffix that does not match roleName fails with "user ...
# is not authorized to assume role ..."). The template keeps roleName fixed
# while roleArn is a placeholder, so derive roleName from the substituted
# ARN here — one source of truth, no drift.
for role in doc.get("roles", []):
    arn = role.get("roleArn", "")
    if arn:
        role["roleName"] = arn.rsplit("/", 1)[-1]

with open(dst, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2, ensure_ascii=False)
    f.write("\n")
PYEOF
}

mkdir -p "${SW_CONFIG_DIR}"
# Stage BOTH files as temporaries in the destination directory (same
# filesystem => rename(2) is atomic); only after both validate do we move them
# into place, so a failure never leaves a half-rendered pair behind.
TMP_S3="$(mktemp "${SW_CONFIG_DIR}/.s3.json.XXXXXX")"
TMP_IAM="$(mktemp "${SW_CONFIG_DIR}/.iam.json.XXXXXX")"
trap 'rm -f "${TMP_S3}" "${TMP_IAM}"' EXIT

render "${DEPLOY_DIR}/seaweedfs/s3.json.tpl" "${TMP_S3}"
render "${DEPLOY_DIR}/seaweedfs/iam.json.tpl" "${TMP_IAM}"

# Validate BEFORE replacing anything: legal JSON + no placeholder survived.
if grep -q "__SW_" "${TMP_S3}" "${TMP_IAM}"; then
  echo "ERROR: unreplaced __SW_* placeholder left in rendered config — template and renderer are out of sync." >&2
  exit 1
fi
python3 -m json.tool "${TMP_S3}" >/dev/null
python3 -m json.tool "${TMP_IAM}" >/dev/null

chmod 600 "${TMP_S3}" "${TMP_IAM}"
mv -f "${TMP_S3}" "${SW_CONFIG_DIR}/s3.json"
mv -f "${TMP_IAM}" "${SW_CONFIG_DIR}/iam.json"
trap - EXIT

echo "Rendered SeaweedFS configs:"
echo "  ${SW_CONFIG_DIR}/s3.json  (identities: admin, fileagent-controlplane)"
echo "  ${SW_CONFIG_DIR}/iam.json (role: ${SW_AGENT_ROLE_ARN})"
echo "  CP identity access key: ${SW_CP_ACCESS_KEY}"
