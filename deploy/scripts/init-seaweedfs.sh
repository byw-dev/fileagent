#!/usr/bin/env bash
# init-seaweedfs.sh — FileAgent SeaweedFS initialisation script (D-037 backend).
#
# Declares-and-verifies; it does NOT provision IAM imperatively (that is the
# whole point of replacing the init-minio.sh `mc admin` flow): identities,
# policies and the STS role/trust policy come from s3.json / iam.json rendered
# by render-seaweedfs-configs.sh and loaded by the filer at startup. If those
# files are missing this script renders them itself — but it cannot restart
# the filer, so changing credentials still requires re-render + container
# restart (documented in deploy/docker-compose.seaweedfs.yml).
#
# This script is IDEMPOTENT: safe to run multiple times. It uses `mc` and curl:
#   1. Create the required buckets (data-sensor, tmp-uploads)
#   2. Set a 7-day lifecycle expiry on tmp-uploads and a 7-day
#      abort-incomplete-multipart rule on data-sensor (same backstops as
#      init-minio.sh; verified supported by SeaweedFS 4.48 — stored and read
#      back. Execution of stored rules needs an additional lifecycle worker,
#      see the summary at the end)
#   3. Self-check the Control Plane identity against everything the CP does at
#      runtime: STS AssumeRole (with a session policy), CreateBucket, and a
#      presigned GET round-trip
#
# Deliberately NOT done here: webhook / event subscription configuration.
# D-037 rejected SeaweedFS notification.toml; the event source is the CP's
# in-process filer metadata subscription, wired since B3-PR3b (CP env
# STORAGE_EVENT_SOURCE=seaweedfs_filer + SEAWEEDFS_FILER_GRPC_ADDR; nothing to
# configure on the storage side).
#
# Requirements: `mc` AND `curl` (>= 7.75, for --aws-sigv4) on PATH.
#
# Environment variables (all have defaults suitable for local dev; they must
# match what render-seaweedfs-configs.sh used, or the self-checks will fail —
# which is exactly what they are for):
#   SW_S3_ENDPOINT      S3 API address          (default: http://localhost:38333)
#   SW_ADMIN_ACCESS_KEY admin identity key id   (default: swAdmin00000000000)
#   SW_ADMIN_SECRET_KEY admin identity secret   (default: swAdminSecret0000000)
#   SW_CP_ACCESS_KEY    CP identity key id      (default: cpAdminIAM000000000)
#   SW_CP_SECRET_KEY    CP identity secret      (default: cpAdminSecret00000000)
#   SW_AGENT_ROLE_ARN   STS role CP assumes     (default: arn:aws:iam::000000000000:role/agent-role)
#   SW_BUCKET_DATA      data bucket             (default: data-sensor)
#   SW_BUCKET_TMP       staging bucket          (default: tmp-uploads)
#   SW_LIFECYCLE_DAYS   lifecycle days          (default: 7)
#   SW_ALIAS            mc alias prefix         (default: swinit)
#   MC_CONFIG_DIR       mc config isolation (smoke.sh sets this; otherwise a
#                       private temp dir is used so the dev machine's ~/.mc is
#                       never touched)

set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# Credential/role defaults live in exactly one place, shared with the renderer
# and smoke.sh (rework P2-2: smoke previously hardcoded them and drifted from
# SW_* overrides used by render/init).
# shellcheck source=/dev/null
source "${REPO_ROOT}/deploy/seaweedfs/env-defaults.sh"
SW_S3_ENDPOINT="${SW_S3_ENDPOINT:-http://localhost:38333}"
SW_BUCKET_DATA="${SW_BUCKET_DATA:-data-sensor}"
SW_BUCKET_TMP="${SW_BUCKET_TMP:-tmp-uploads}"
SW_LIFECYCLE_DAYS="${SW_LIFECYCLE_DAYS:-7}"
SW_ALIAS="${SW_ALIAS:-swinit}"

# ---------------------------------------------------------------------------
# Tool checks
# ---------------------------------------------------------------------------
if ! command -v mc &>/dev/null; then
  echo "ERROR: 'mc' (MinIO Client) is not installed or not in PATH." >&2
  echo "       Install it from https://min.io/docs/minio/linux/reference/minio-mc.html" >&2
  exit 1
fi
if ! command -v curl &>/dev/null; then
  echo "ERROR: 'curl' is required for the STS and presigned-GET self-checks." >&2
  exit 1
fi
case "$(curl --help all 2>/dev/null || true)" in
  *--aws-sigv4*) ;;
  *)
    echo "ERROR: curl does not support --aws-sigv4; upgrade curl before initialising SeaweedFS." >&2
    exit 1
    ;;
esac

# ---------------------------------------------------------------------------
# mc config isolation (mc's config is GLOBAL at ~/.mc/config.json; never
# touch the developer's aliases). smoke.sh exports MC_CONFIG_DIR already.
# bash 3.2 (macOS) safe array expansion is used throughout.
#
# Cleanup ordering (rework P2-3): probe object/bucket are removed FIRST —
# those mc calls need the admin alias to still exist — and only then are the
# aliases and the temporary mc config dir torn down. Every cleanup step that
# can leak state warns loudly instead of failing silently: a leftover probe
# object is never cleaned up by the (currently non-executing) lifecycle rules.
# ---------------------------------------------------------------------------
OWN_MC_DIR=""
LC_DIR=""
CHECK_OBJECT=""
CHECK_BUCKET=""
ADMIN_ALIAS_SET=0
CP_ALIAS_SET=0
if [ -z "${MC_CONFIG_DIR:-}" ]; then
  OWN_MC_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fileagent-swinit-mc.XXXXXX")"
  export MC_CONFIG_DIR="${OWN_MC_DIR}"
fi
cleanup() {
  if [ -n "${CHECK_OBJECT:-}" ]; then
    mc rm --force "${SW_ALIAS}/${CHECK_OBJECT}" >/dev/null 2>&1 \
      || echo "WARNING: failed to remove probe object '${SW_ALIAS}/${CHECK_OBJECT}' — remove it manually (lifecycle rules are NOT executed by SeaweedFS)." >&2
  fi
  if [ -n "${CHECK_BUCKET:-}" ]; then
    mc rb "${SW_ALIAS}/${CHECK_BUCKET}" >/dev/null 2>&1 \
      || echo "WARNING: failed to remove probe bucket '${SW_ALIAS}/${CHECK_BUCKET}' — remove it manually." >&2
  fi
  # `if` 形式（而非 `|| { [ ] && echo; }`）：flag=0 时后者整体返回 1，set -e
  # 会在 trap 里中途中断 cleanup（LC_DIR/OWN_MC_DIR 泄漏 + rc 漂移）。
  if [ "${ADMIN_ALIAS_SET}" = "1" ] && ! mc alias remove "${SW_ALIAS}" >/dev/null 2>&1; then
    echo "WARNING: failed to remove mc alias '${SW_ALIAS}' from MC_CONFIG_DIR '${MC_CONFIG_DIR}'." >&2
  fi
  if [ "${CP_ALIAS_SET}" = "1" ] && ! mc alias remove "${SW_ALIAS}-cp" >/dev/null 2>&1; then
    echo "WARNING: failed to remove mc alias '${SW_ALIAS}-cp' from MC_CONFIG_DIR '${MC_CONFIG_DIR}'." >&2
  fi
  [ -n "${LC_DIR}" ] && rm -rf "${LC_DIR}"
  [ -n "${OWN_MC_DIR}" ] && rm -rf "${OWN_MC_DIR}"
  # trap 的返回值会成为脚本的退出码（bash 在 set -e 下的行为）：
  # `[ -n "" ] && ...` 求值为假曾把成功跑完的脚本变成 rc=1。
  return 0
}
trap cleanup EXIT

# Force curl's 'localhost' onto IPv4, mirroring init-minio.sh: published
# container ports are commonly IPv4-only while 'localhost' also resolves to
# ::1. --connect-to rewrites only the transport, not the Host header, so the
# SigV4 signature is untouched.
CURL_CONNECT_ARGS=()
SW_AUTHORITY="${SW_S3_ENDPOINT#*://}"
SW_AUTHORITY="${SW_AUTHORITY%%/*}"
if [[ "${SW_AUTHORITY}" == localhost:* ]]; then
  CURL_CONNECT_ARGS=(--connect-to "${SW_AUTHORITY}:127.0.0.1:${SW_AUTHORITY##*:}")
fi

# assume_role <access key> <secret key> — same shape the Control Plane's
# STSManager issues (inline session policy, 3600s). STS is served on the S3
# port root. Raw response (XML) goes to stdout, curl's status to the caller.
STS_SESSION_POLICY='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:ListBucketMultipartUploads"],"Resource":["arn:aws:s3:::'"${SW_BUCKET_DATA}"'"]},{"Effect":"Allow","Action":["s3:PutObject","s3:AbortMultipartUpload","s3:ListMultipartUploadParts"],"Resource":["arn:aws:s3:::'"${SW_BUCKET_DATA}"'/*"]}]}'
assume_role() {
  curl --silent --show-error --fail-with-body \
    ${CURL_CONNECT_ARGS[@]+"${CURL_CONNECT_ARGS[@]}"} \
    --aws-sigv4 "aws:amz:us-east-1:sts" \
    --user "$1:$2" \
    --header "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "Action=AssumeRole" \
    --data-urlencode "Version=2011-06-15" \
    --data-urlencode "DurationSeconds=3600" \
    --data-urlencode "RoleArn=${SW_AGENT_ROLE_ARN}" \
    --data-urlencode "RoleSessionName=fileagent-init-check" \
    --data-urlencode "Policy=${STS_SESSION_POLICY}" \
    "${SW_S3_ENDPOINT}" 2>&1
}

# ---------------------------------------------------------------------------
# 0. Ensure rendered configs exist (docker would silently mount an empty
#    directory otherwise — the filer would run with no identities at all).
#    Re-rendering with the CURRENT env is harmless: it is idempotent.
# ---------------------------------------------------------------------------
if [ ! -f "${REPO_ROOT}/deploy/config/seaweedfs/s3.json" ] || \
   [ ! -f "${REPO_ROOT}/deploy/config/seaweedfs/iam.json" ]; then
  echo "==> Rendered configs missing; running render-seaweedfs-configs.sh first"
  SW_ADMIN_ACCESS_KEY="${SW_ADMIN_ACCESS_KEY}" \
  SW_ADMIN_SECRET_KEY="${SW_ADMIN_SECRET_KEY}" \
  SW_CP_ACCESS_KEY="${SW_CP_ACCESS_KEY}" \
  SW_CP_SECRET_KEY="${SW_CP_SECRET_KEY}" \
  SW_AGENT_ROLE_ARN="${SW_AGENT_ROLE_ARN}" \
    bash "${REPO_ROOT}/deploy/scripts/render-seaweedfs-configs.sh"
  echo "    NOTE: if the filer was already running with different credentials,"
  echo "    restart it (docker compose -f deploy/docker-compose.seaweedfs.yml"
  echo "    restart seaweed-filer) so it reloads the rendered files."
fi

# ---------------------------------------------------------------------------
# 1. Admin alias + buckets
# ---------------------------------------------------------------------------
echo "==> Configuring mc alias '${SW_ALIAS}' -> ${SW_S3_ENDPOINT}"
mc alias set "${SW_ALIAS}" "${SW_S3_ENDPOINT}" \
  "${SW_ADMIN_ACCESS_KEY}" "${SW_ADMIN_SECRET_KEY}" --api S3v4 --quiet
ADMIN_ALIAS_SET=1

echo "==> Creating bucket: ${SW_BUCKET_DATA}"
mc mb --ignore-existing "${SW_ALIAS}/${SW_BUCKET_DATA}"

echo "==> Creating bucket: ${SW_BUCKET_TMP}"
mc mb --ignore-existing "${SW_ALIAS}/${SW_BUCKET_TMP}"

# ---------------------------------------------------------------------------
# 2. Lifecycle rules (verified supported on SeaweedFS 4.48: both rules are
#    stored AND read back — unlike MinIO, which strips
#    AbortIncompleteMultipartUpload entirely). Execution of stored rules
#    requires an additional lifecycle worker (storage-layer-eval report §1
#    G5); storing them now means a future worker has the policy to enforce.
#    A server that rejects the import degrades to a loud warning, never a
#    silent pass (same contract as init-minio.sh).
# ---------------------------------------------------------------------------
LC_DIR="$(mktemp -d "${TMPDIR:-/tmp}/fileagent-swinit-lc.XXXXXX")"

echo "==> Setting ${SW_LIFECYCLE_DAYS}-day expiry on ${SW_BUCKET_TMP}"
cat >"${LC_DIR}/tmp-expire.json" <<EOF
{"Rules":[{"ID":"auto-expire-tmp-uploads","Status":"Enabled","Filter":{"Prefix":""},"Expiration":{"Days":${SW_LIFECYCLE_DAYS}}}]}
EOF
TMP_RULE_EFFECTIVE="no"
if mc ilm import "${SW_ALIAS}/${SW_BUCKET_TMP}" <"${LC_DIR}/tmp-expire.json" >/dev/null 2>&1; then
  if mc ilm export "${SW_ALIAS}/${SW_BUCKET_TMP}" 2>/dev/null | grep -q '"ID":"auto-expire-tmp-uploads"'; then
    TMP_RULE_EFFECTIVE="yes"
  fi
else
  echo "WARNING: lifecycle import on '${SW_BUCKET_TMP}' was rejected by the server." >&2
fi
[ "${TMP_RULE_EFFECTIVE}" = "yes" ] \
  && echo "    Verified: ${SW_BUCKET_TMP} expiry rule stored (${SW_LIFECYCLE_DAYS} days)" \
  || echo "WARNING: ${SW_BUCKET_TMP} expiry rule NOT verifiable on this server (see above)." >&2

echo "==> Setting ${SW_LIFECYCLE_DAYS}-day abort-incomplete-multipart rule on ${SW_BUCKET_DATA}"
cat >"${LC_DIR}/data-abort.json" <<EOF
{"Rules":[{"ID":"abort-incomplete-multipart-data-sensor","Status":"Enabled","Filter":{"Prefix":""},"AbortIncompleteMultipartUpload":{"DaysAfterInitiation":${SW_LIFECYCLE_DAYS}}}]}
EOF
ABORT_RULE_EFFECTIVE="no"
if mc ilm import "${SW_ALIAS}/${SW_BUCKET_DATA}" <"${LC_DIR}/data-abort.json" >/dev/null 2>&1; then
  if mc ilm export "${SW_ALIAS}/${SW_BUCKET_DATA}" 2>/dev/null | grep -q '"ID":"abort-incomplete-multipart-data-sensor"'; then
    ABORT_RULE_EFFECTIVE="yes"
  fi
else
  echo "WARNING: lifecycle import on '${SW_BUCKET_DATA}' was rejected by the server." >&2
fi
[ "${ABORT_RULE_EFFECTIVE}" = "yes" ] \
  && echo "    Verified: ${SW_BUCKET_DATA} abort-incomplete-multipart rule stored (${SW_LIFECYCLE_DAYS} days)" \
  || echo "WARNING: ${SW_BUCKET_DATA} abort rule NOT verifiable on this server (see above)." >&2

# ---------------------------------------------------------------------------
# 3. Self-checks with the exact Control Plane identity.
# ---------------------------------------------------------------------------

# 3a. AssumeRole (the credential path agents depend on).
echo "==> Self-checking CP identity AssumeRole (${SW_AGENT_ROLE_ARN})"
STS_OUT=""
if ! STS_OUT=$(assume_role "${SW_CP_ACCESS_KEY}" "${SW_CP_SECRET_KEY}"); then
  echo "ERROR: CP identity '${SW_CP_ACCESS_KEY}' failed AssumeRole: ${STS_OUT}" >&2
  exit 1
fi
if [[ "${STS_OUT}" != *"<AccessKeyId>"* ]] || \
   [[ "${STS_OUT}" != *"<SecretAccessKey>"* ]] || \
   [[ "${STS_OUT}" != *"<SessionToken>"* ]]; then
  echo "ERROR: AssumeRole response did not contain a complete temporary credential set." >&2
  exit 1
fi
STS_ACCESS_KEY="${STS_OUT#*<AccessKeyId>}"
STS_ACCESS_KEY="${STS_ACCESS_KEY%%</AccessKeyId>*}"
echo "    Self-check AssumeRole: OK (temporary access key prefix: ${STS_ACCESS_KEY:0:8}...)"

# The CP identity's own alias for 3b/3c.
mc alias set "${SW_ALIAS}-cp" "${SW_S3_ENDPOINT}" \
  "${SW_CP_ACCESS_KEY}" "${SW_CP_SECRET_KEY}" --api S3v4 --quiet
CP_ALIAS_SET=1

# 3b. CreateBucket (POST /api/v1/buckets at runtime).
echo "==> Self-checking CP identity CreateBucket"
CHECK_BUCKET="fileagent-init-check-$$-$(date +%s)"
MB_OUT=""
if ! MB_OUT=$(mc mb "${SW_ALIAS}-cp/${CHECK_BUCKET}" 2>&1); then
  echo "ERROR: CP identity '${SW_CP_ACCESS_KEY}' cannot create a bucket: ${MB_OUT}" >&2
  echo "       POST /api/v1/buckets would fail at runtime; check s3:CreateBucket in the fileagent-cp policy." >&2
  exit 1
fi
# The CP identity deliberately has no DeleteBucket; drop the empty probe as
# admin. Plain rb (no --force): the bucket IS empty, and with
# -s3.allowDeleteBucketNotEmpty=false a recursive delete must never be needed.
if ! mc rb "${SW_ALIAS}/${CHECK_BUCKET}" >/dev/null; then
  echo "ERROR: CreateBucket self-check succeeded, but admin cleanup failed for bucket '${CHECK_BUCKET}'." >&2
  exit 1
fi
CHECK_BUCKET=""
echo "    Self-check CreateBucket: OK (probe bucket created and removed)"

# 3c. Presigned GET (download URLs).
echo "==> Self-checking CP identity presigned GET"
CHECK_OBJECT="${SW_BUCKET_TMP}/.init-check/$(date +%s)-$$"
CHECK_BODY="fileagent-seaweedfs-init-check"
printf '%s' "${CHECK_BODY}" >"${LC_DIR}/probe-body.txt"
# `mc cp` (NOT `mc pipe`): piping from stdin triggers a Content-MD5 mismatch
# against SeaweedFS 4.48 (live-verified); a real file upload does not.
if ! mc cp --quiet "${LC_DIR}/probe-body.txt" "${SW_ALIAS}/${CHECK_OBJECT}" >/dev/null; then
  echo "ERROR: failed to upload the presign probe object as admin." >&2
  exit 1
fi

SHARE_OUT=""
if ! SHARE_OUT=$(mc share download --expire 5m --json \
  "${SW_ALIAS}-cp/${CHECK_OBJECT}" 2>&1); then
  echo "ERROR: CP identity '${SW_CP_ACCESS_KEY}' failed to sign a presigned GET: ${SHARE_OUT}" >&2
  exit 1
fi
PRESIGNED_URL=""
case "${SHARE_OUT}" in
  *'"share":"'*)
    PRESIGNED_URL="${SHARE_OUT#*\"share\":\"}"
    PRESIGNED_URL="${PRESIGNED_URL%%\"*}"
    ;;
esac
if [ -z "${PRESIGNED_URL}" ]; then
  echo "ERROR: mc returned no presigned URL: ${SHARE_OUT}" >&2
  exit 1
fi

HTTP_CODE=$(curl --silent --show-error ${CURL_CONNECT_ARGS[@]+"${CURL_CONNECT_ARGS[@]}"} \
  --output "${LC_DIR}/presigned-body.txt" --write-out "%{http_code}" "${PRESIGNED_URL}")
if [ "${HTTP_CODE}" != "200" ]; then
  echo "ERROR: presigned GET returned HTTP ${HTTP_CODE}; expected 200." >&2
  exit 1
fi
if [ "$(cat "${LC_DIR}/presigned-body.txt")" != "${CHECK_BODY}" ]; then
  echo "ERROR: presigned GET returned unexpected object content." >&2
  exit 1
fi
# CP identity has no DeleteObject (write-only by design); cleanup as admin.
if ! mc rm --force "${SW_ALIAS}/${CHECK_OBJECT}" >/dev/null; then
  echo "ERROR: presigned-GET self-check succeeded, but admin cleanup failed for '${CHECK_OBJECT}'." >&2
  exit 1
fi
CHECK_OBJECT=""
echo "    Self-check presigned GET: OK (HTTP 200, object content verified and removed)"

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
echo ""
echo "SeaweedFS initialisation complete."
echo "  Buckets   : ${SW_BUCKET_DATA}, ${SW_BUCKET_TMP}"
if [ "${TMP_RULE_EFFECTIVE}" = "yes" ]; then
  echo "  Lifecycle : ${SW_BUCKET_TMP} objects expire after ${SW_LIFECYCLE_DAYS} days (rule stored)"
else
  echo "  Lifecycle : ${SW_BUCKET_TMP} expiry rule NOT effective on this server (see warning above)"
fi
if [ "${ABORT_RULE_EFFECTIVE}" = "yes" ]; then
  echo "  Lifecycle : ${SW_BUCKET_DATA} incomplete multipart uploads abort after ${SW_LIFECYCLE_DAYS} days (rule stored)"
else
  echo "  Lifecycle : ${SW_BUCKET_DATA} abort-incomplete-multipart rule NOT effective on this server (see warning above)"
fi
echo "  ⚠️  SeaweedFS stores lifecycle rules but does NOT execute them by itself;"
echo "      a lifecycle worker is still an open gap (see PR description)."
echo "  IAM       : declarative s3.json / iam.json (CP identity '${SW_CP_ACCESS_KEY}', role ${SW_AGENT_ROLE_ARN})"
echo "  Events    : CP-side filer subscription (STORAGE_EVENT_SOURCE=seaweedfs_filer; wired in B3-PR3b, nothing to configure here)"
