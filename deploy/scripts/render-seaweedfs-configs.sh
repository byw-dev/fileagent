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
# The DEFAULTS below are development placeholders shared with
# init-seaweedfs.sh and the seaweedfs branch of smoke.sh — they are NOT
# secrets. Real deployments MUST override every SW_* variable.
#
# Environment variables (all have dev defaults):
#   SW_ADMIN_ACCESS_KEY   storage admin identity key id    (default: swAdmin00000000000)
#   SW_ADMIN_SECRET_KEY   storage admin identity secret    (default: swAdminSecret0000000)
#   SW_CP_ACCESS_KEY      Control Plane identity key id    (default: cpAdminIAM000000000 —
#                         same default as init-minio.sh, so the MinIO-era CP env also works here)
#   SW_CP_SECRET_KEY      Control Plane identity secret    (default: cpAdminSecret00000000)
#   SW_STS_SIGNING_KEY    STS token signing key (HMAC secret; default is a
#                         base64 dev placeholder)          (default: ZmlsZUFnZW50LWRldi1zdHMtc2lnbmluZy1rZXk=)
#   SW_AGENT_ROLE_ARN     STS role ARN CP assumes          (default: arn:aws:iam::000000000000:role/agent-role)
#   SW_CONFIG_DIR         output directory                 (default: deploy/config/seaweedfs)
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
DEPLOY_DIR="${REPO_ROOT}/deploy"

SW_ADMIN_ACCESS_KEY="${SW_ADMIN_ACCESS_KEY:-swAdmin00000000000}"
SW_ADMIN_SECRET_KEY="${SW_ADMIN_SECRET_KEY:-swAdminSecret0000000}"
SW_CP_ACCESS_KEY="${SW_CP_ACCESS_KEY:-cpAdminIAM000000000}"
SW_CP_SECRET_KEY="${SW_CP_SECRET_KEY:-cpAdminSecret00000000}"
SW_STS_SIGNING_KEY="${SW_STS_SIGNING_KEY:-ZmlsZUFnZW50LWRldi1zdHMtc2lnbmluZy1rZXk=}"
SW_AGENT_ROLE_ARN="${SW_AGENT_ROLE_ARN:-arn:aws:iam::000000000000:role/agent-role}"
SW_CONFIG_DIR="${SW_CONFIG_DIR:-${DEPLOY_DIR}/config/seaweedfs}"

# ---------------------------------------------------------------------------
# Guards: refuse empty values early, by variable name, instead of letting an
# empty credential silently land in the rendered file (and every S3 request
# fail with an opaque signature error afterwards).
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
# Render: plain sed on __PLACEHOLDER__ tokens. The values are credentials, not
# arbitrary text, so they contain no sed metacharacters worth escaping wars —
# but enforce that convention by rejecting values with the delimiter in them.
# ---------------------------------------------------------------------------
render() { # <template> <output>
  local src="$1" dst="$2"
  for pair in \
    "SW_ADMIN_ACCESS_KEY:${SW_ADMIN_ACCESS_KEY}" \
    "SW_ADMIN_SECRET_KEY:${SW_ADMIN_SECRET_KEY}" \
    "SW_CP_ACCESS_KEY:${SW_CP_ACCESS_KEY}" \
    "SW_CP_SECRET_KEY:${SW_CP_SECRET_KEY}" \
    "SW_STS_SIGNING_KEY:${SW_STS_SIGNING_KEY}" \
    "SW_AGENT_ROLE_ARN:${SW_AGENT_ROLE_ARN}"; do
    local value="${pair#*:}"
    case "${value}" in
      *'|'*) echo "ERROR: credential values containing '|' are not supported (sed delimiter)." >&2; exit 1 ;;
    esac
  done
  sed \
    -e "s|__SW_ADMIN_ACCESS_KEY__|${SW_ADMIN_ACCESS_KEY}|g" \
    -e "s|__SW_ADMIN_SECRET_KEY__|${SW_ADMIN_SECRET_KEY}|g" \
    -e "s|__SW_CP_ACCESS_KEY__|${SW_CP_ACCESS_KEY}|g" \
    -e "s|__SW_CP_SECRET_KEY__|${SW_CP_SECRET_KEY}|g" \
    -e "s|__SW_STS_SIGNING_KEY__|${SW_STS_SIGNING_KEY}|g" \
    -e "s|__SW_AGENT_ROLE_ARN__|${SW_AGENT_ROLE_ARN}|g" \
    "${src}" > "${dst}"
}

mkdir -p "${SW_CONFIG_DIR}"
render "${DEPLOY_DIR}/seaweedfs/s3.json.tpl" "${SW_CONFIG_DIR}/s3.json"
render "${DEPLOY_DIR}/seaweedfs/iam.json.tpl" "${SW_CONFIG_DIR}/iam.json"
chmod 600 "${SW_CONFIG_DIR}/s3.json" "${SW_CONFIG_DIR}/iam.json"

# Validate JSON-ness when python3 is available (it is on dev machines and CI;
# a broken JSON would otherwise only surface as a cryptic weed startup error).
if command -v python3 >/dev/null 2>&1; then
  python3 -m json.tool "${SW_CONFIG_DIR}/s3.json" >/dev/null
  python3 -m json.tool "${SW_CONFIG_DIR}/iam.json" >/dev/null
fi

# Fail loudly if any placeholder survived (template/variable drift).
if grep -q "__SW_" "${SW_CONFIG_DIR}/s3.json" "${SW_CONFIG_DIR}/iam.json"; then
  echo "ERROR: unreplaced __SW_* placeholder left in rendered config — template and renderer are out of sync." >&2
  exit 1
fi

echo "Rendered SeaweedFS configs:"
echo "  ${SW_CONFIG_DIR}/s3.json  (identities: admin, fileagent-controlplane)"
echo "  ${SW_CONFIG_DIR}/iam.json (role: ${SW_AGENT_ROLE_ARN})"
echo "  CP identity access key: ${SW_CP_ACCESS_KEY}"
