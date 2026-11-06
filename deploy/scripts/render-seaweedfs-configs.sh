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
# P2-1). smoke.sh and the CI runners already hard-depend on python3 (token
# parsing), so THOSE hosts see no new demand; a standalone deployment host
# running this renderer outside smoke must preinstall python3 — the standard
# library alone suffices, no pip packages (rework R2 nit).
#
# Publish semantics (rework R2 P2, narrowed to the truth): each file is
# replaced by an atomic same-directory rename, and a failure of the SECOND
# rename rolls the first file back to its previous bytes. A process killed
# BETWEEN the two renames can still leave a mixed generation (new s3.json +
# old iam.json) — re-running the renderer converges, it is idempotent.
#
# Permission model (B7, 2026-11-06): SW_CONFIG_DIR is forced to 0700 and the
# published s3.json / iam.json to 0644. The files must be world-READABLE on
# purpose: on Linux the filer bind-mounts them straight from this directory,
# and although the image starts as root, its entrypoint immediately drops to
# uid 1000 (seaweed, via su-exec). A bind mount preserves the HOST uid and
# mode bits, so the in-container seaweed user is literally "some other uid"
# from the file's point of view — a 0600 root/runner-owned file is unreadable
# to it, the S3 gate fails to load its config and the filer healthcheck goes
# red. That is exactly what killed every Linux CI run of the fault-injection
# suite after PR #139 (GitHub runner uid 1001 owned 0600 files; macOS Docker
# Desktop never enforces host perms, so local runs stayed green). 0644 keeps
# the file readable for ANY container uid without widening exposure on the
# host: the containing directory is 0700, so no OTHER host user can even
# traverse into it, and the Docker daemon resolves bind mounts as root
# regardless of file mode.
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

# roleName must mirror SeaweedFS 4.48's ExtractRoleNameFromArn
# (weed/iam/utils/arn_utils.go): the ARN must start with "arn:aws:iam::",
# the resource part must start with "role/", and the role name is the
# COMPLETE remainder after "role/" — it may itself contain "/" (a path-style
# ARN like ...:role/team/agent yields roleName "team/agent"). AssumeRole
# resolves the role by exactly this name (iam_manager.go ->
# roleStore.GetRole), so a derived name that disagrees with this rule fails
# with "user ... is not authorized to assume role ...".
IAM_ARN_PREFIX = "arn:aws:iam::"
ROLE_MARKER = "role/"

def extract_role_name(arn):
    # Returns the role name per 4.48 semantics, or None when the ARN is
    # invalid (wrong prefix, wrong resource type, or empty name).
    if not arn.startswith(IAM_ARN_PREFIX):
        return None
    remainder = arn[len(IAM_ARN_PREFIX):]
    resource = remainder
    colon = remainder.find(":")
    if colon != -1:
        resource = remainder[colon + 1:]
    if not resource.startswith(ROLE_MARKER):
        return None
    name = resource[len(ROLE_MARKER):]
    return name or None

for role in doc.get("roles", []):
    arn = role.get("roleArn", "")
    name = extract_role_name(arn)
    if name is None:
        sys.exit(
            f"SW_AGENT_ROLE_ARN is not a valid IAM role ARN: {arn!r}\n"
            f"Expected arn:aws:iam::[<account>:]role/<name> with a non-empty"
            f" <name> (it may contain '/' path segments)."
        )
    role["roleName"] = name

with open(dst, "w", encoding="utf-8") as f:
    json.dump(doc, f, indent=2, ensure_ascii=False)
    f.write("\n")
PYEOF
}

# The gate runs BEFORE mkdir -p: mkdir -p on a symlinked path would happily
# create/keep the LINK's target directory and we would never notice (mkdir
# -p succeeds silently when the link resolves to an existing dir).
if [ -L "${SW_CONFIG_DIR}" ]; then
  echo "ERROR: SW_CONFIG_DIR (${SW_CONFIG_DIR}) is a symbolic link — refusing to operate." >&2
  echo "       chmod would follow the link and re-permission a directory this script" >&2
  echo "       does not own. Point SW_CONFIG_DIR at a real directory (or remove the link)" >&2
  echo "       and re-run." >&2
  exit 1
fi
mkdir -p "${SW_CONFIG_DIR}"
# --- Directory permission gate (B7, 2026-11-06) -----------------------------
# SW_CONFIG_DIR must end up 0700 (see the permission-model note in the file
# header). Cases, all fail-closed:
#   * symlink (even one whose target we could fix) — refused, same rationale
#     as smoke.sh's ~/.aws guard: chmod on a symlink silently follows it and
#     would re-permission a directory this script does not own (e.g. a real
#     ~/.aws or an attacker-planted target); a symlinked config dir also
#     breaks the “same directory ⇒ atomic rename" publish guarantee above
#     when the link crosses a filesystem.
#   * existing dir owned by someone else — refused: we cannot chmod it and the
#     0700 guarantee this script promises would be a lie; report and exit.
#   * existing dir owned by us — corrected to 0700 (it may have been created
#     by an older renderer or a umask-happy process as 0755).
#   * newly created — mkdir already applied the umask, chmod pins it exactly.
if [ -d "${SW_CONFIG_DIR}" ] && [ "$(id -u)" != "$(stat -c '%u' "${SW_CONFIG_DIR}" 2>/dev/null || stat -f '%u' "${SW_CONFIG_DIR}")" ]; then
  echo "ERROR: SW_CONFIG_DIR (${SW_CONFIG_DIR}) is owned by another user — cannot enforce 0700." >&2
  stat -c '       current: owner uid=%u mode=%a' "${SW_CONFIG_DIR}" 2>/dev/null \
    || stat '       current: %Su %Sp' "${SW_CONFIG_DIR}" >&2
  echo "       Fix suggestions: chown the directory to the user running this script" >&2
  echo "       (sudo chown -R $(id -u) ${SW_CONFIG_DIR}), or move it aside and let the" >&2
  echo "       renderer create a fresh one (mv ${SW_CONFIG_DIR} ${SW_CONFIG_DIR}.old)." >&2
  exit 1
fi
if ! chmod 700 "${SW_CONFIG_DIR}"; then
  echo "ERROR: chmod 700 ${SW_CONFIG_DIR} failed — cannot guarantee the 0700 directory for" >&2
  echo "       the rendered credentials; refusing to publish." >&2
  exit 1
fi
# Stage BOTH files as temporaries in the destination directory (same
# filesystem => each rename(2) is atomic). Only after both validate are they
# published; if the SECOND publish fails, the first is rolled back from an
# in-place backup (see publish block below for the exact, narrowed
# semantics — this is per-file atomicity plus rollback, not a transaction).
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

# 0644, NOT 0600: the filer bind-mounts these files and its entrypoint drops
# to uid 1000 (seaweed) via su-exec while the host-side owner is whoever ran
# this script (root, a CI runner uid, ...). On Linux the bind mount keeps the
# host uid/mode, so 0600 files are unreadable in-container and the filer never
# comes healthy (see the permission-model note in the header). Still safe:
# the containing directory is 0700, so no other HOST user can traverse into it
# (the window before publish is covered too — the mktemp temporaries below
# live INSIDE that 0700 directory, and mktemp itself creates them 0600), and
# the Docker daemon resolves bind mounts as root regardless of file mode.
chmod 644 "${TMP_S3}" "${TMP_IAM}"

# --- Publish (rework R2 P2) -------------------------------------------------
# Back up the currently published s3.json so a failure of the SECOND rename
# can restore it; on a first run (no previous file) the rollback deletes the
# half-published one instead, returning the directory to its pre-run state.
# A process killed BETWEEN the two renames is NOT covered: that window can
# leave a mixed generation (new s3 + old iam). Re-running the renderer
# converges — it is idempotent and re-renders both files from the same env.
PUBLISHED_S3="${SW_CONFIG_DIR}/s3.json"
PUBLISHED_IAM="${SW_CONFIG_DIR}/iam.json"
HAD_OLD_S3=0
[ -f "${PUBLISHED_S3}" ] && HAD_OLD_S3=1
BK_S3="$(mktemp "${SW_CONFIG_DIR}/.s3.json.bak.XXXXXX")"
[ "${HAD_OLD_S3}" = "1" ] && cp -p "${PUBLISHED_S3}" "${BK_S3}"
trap 'rm -f "${TMP_S3}" "${TMP_IAM}" "${BK_S3}"' EXIT

mv -f "${TMP_S3}" "${PUBLISHED_S3}"
if ! mv -f "${TMP_IAM}" "${PUBLISHED_IAM}"; then
  echo "ERROR: publishing ${PUBLISHED_IAM} failed — rolling back ${PUBLISHED_S3} to its previous content." >&2
  ROLLED_BACK=0
  if [ "${HAD_OLD_S3}" = "1" ]; then
    if cat "${BK_S3}" > "${PUBLISHED_S3}" 2>/dev/null; then
      ROLLED_BACK=1
    fi
  else
    rm -f "${PUBLISHED_S3}" && ROLLED_BACK=1
  fi
  if [ "${ROLLED_BACK}" != "1" ]; then
    echo "ERROR: rollback of ${PUBLISHED_S3} ALSO failed — the published pair may now be a MIXED generation." >&2
    echo "       Re-run the renderer with the same SW_* environment before starting the filer." >&2
  fi
  exit 1
fi
trap - EXIT
rm -f "${BK_S3}"

echo "Rendered SeaweedFS configs:"
echo "  ${SW_CONFIG_DIR}/s3.json  (identities: admin, fileagent-controlplane)"
echo "  ${SW_CONFIG_DIR}/iam.json (role: ${SW_AGENT_ROLE_ARN})"
echo "  CP identity access key: ${SW_CP_ACCESS_KEY}"
