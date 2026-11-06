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
# Operational invariants of that model (rework 1):
#   * Rendering must ALWAYS be done by the SAME uid. Root must not take over
#     a directory another user rendered (the ownership gate below refuses
#     it on purpose): 0644 files under a directory some other uid can
#     re-permission would void the 0700 guarantee. Re-render as the owning
#     user, or move the directory aside and render fresh.
#   * Upgrading an existing deployment from the 0600 scheme: re-run this
#     renderer once after upgrading (as the same uid that owns the config
#     directory). init-seaweedfs.sh only invokes the renderer when the files
#     are MISSING, so stale 0600 files are not fixed by running init alone.
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
# --- Parent-directory trust gate (rework 1, review R1-1) --------------------
# With the files at 0644, the 0700 SW_CONFIG_DIR is the only thing keeping
# other host users away from the credentials — so nobody may be able to swap
# that directory out from under us after the chmod. On Linux, renaming a
# directory entry requires write permission on the PARENT directory, so the
# direct parent must be trusted BEFORE anything containing credentials is
# rendered: owned by us or root, and not group/other-writable unless the
# sticky bit is set (sticky makes renames safe: only the entry's owner, the
# directory owner, or root may rename/remove it — same rule that makes /tmp
# usable). This closes the swap attack master did not have (0600 files stayed
# unreadable even under a swapped directory); a fd/inode-pinning scheme would
# be stronger still but is deliberately out of scope (keep it simple, check
# the path once, render).
#
# Symlinked parents: checked as RESOLVED. We deliberately do NOT reject a
# parent that is a symlink — unlike SW_CONFIG_DIR itself (rejected above,
# because we chmod IT), we only need the real parent's owner/write bits.
# `stat -L` follows the link on BOTH GNU and BSD coreutils (plain stat does
# NOT follow on GNU and reports the link itself), so the check lands on the
# directory that actually mediates renames. The residual risk of the link
# being re-pointed between check and use is the same class as any path-based
# check and is not worsened by 0644 (master had it too); accepted for
# simplicity.
PARENT_DIR="$(dirname -- "${SW_CONFIG_DIR}")"
# mode: GNU `stat -c %a` prints up to 4 octal digits incl. sticky (1777);
# BSD `stat -f %p` prints the full st_mode (41777) — normalized below to 4
# octal digits by masking the file-type nibble.
P_UID="$(stat -Lc '%u' "${PARENT_DIR}" 2>/dev/null || stat -Lf '%u' "${PARENT_DIR}")"
P_MODE="$(stat -Lc '%a' "${PARENT_DIR}" 2>/dev/null || stat -Lf '%p' "${PARENT_DIR}")"
# Normalize the mode into OCTAL DIGITS (no prefix) so every later use can
# prefix 8# exactly once: GNU %a and BSD %p both yield octal digits only, but
# results of $((…)) are DECIMAL and must never be re-prefixed. Mask 07777
# drops BSD's file-type nibble (41777 → 01777); printf re-renders as octal.
_MODE_DEC="$(( 8#${P_MODE} & 07777 ))"
P_MODE="$(printf '%04o' "${_MODE_DEC}")"
if [ "${P_UID}" != "0" ] && [ "${P_UID}" != "$(id -u)" ]; then
  echo "ERROR: parent directory of SW_CONFIG_DIR is not trusted — refusing to render." >&2
  echo "       ${PARENT_DIR} is owned by uid=${P_UID} (mode ${P_MODE}); only the current" >&2
  echo "       user (uid=$(id -u)) or root may own it. With the published files at 0644," >&2
  echo "       a foreign owner of the parent could swap the 0700 directory aside and read" >&2
  echo "       the credentials. Fix: chown the parent to the rendering user (or root), or" >&2
  echo "       point SW_CONFIG_DIR somewhere under a directory you own." >&2
  exit 1
fi
# Group/other writable is acceptable ONLY with the sticky bit (mode & 01000):
# sticky restricts rename/unlink to the entry owner, directory owner or root —
# the same rule that makes a 1777 /tmp safe to use.
if [ "$(( 8#${P_MODE} & 0022 ))" -ne 0 ] && [ "$(( 8#${P_MODE} & 01000 ))" -eq 0 ]; then
  echo "ERROR: parent directory of SW_CONFIG_DIR is writable by group/other without the" >&2
  echo "       sticky bit — refusing to render. ${PARENT_DIR} (uid=${P_UID}, mode ${P_MODE})" >&2
  echo "       could let another user rename the 0700 config directory aside and read the" >&2
  echo "       0644 credentials inside. Fix: chmod go-w ${PARENT_DIR} (or chmod +t if it" >&2
  echo "       must stay shared), or point SW_CONFIG_DIR elsewhere." >&2
  exit 1
fi
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
  # Diagnostics only (rework 1, review R1-2): both branches redirect to stderr
  # and `|| true` keeps a failing diagnostic from aborting under set -e — the
  # fix suggestions and exit 1 below MUST stay reachable. BSD/macOS needs
  # `stat -f`; GNU coreutils needs `stat -c`.
  { stat -c '       current: owner uid=%u mode=%a' "${SW_CONFIG_DIR}" 2>/dev/null \
      || stat -f '       current: owner=%Su mode=%Sp' "${SW_CONFIG_DIR}" 2>/dev/null; } >&2 || true
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
