#!/usr/bin/env bash
# init-minio.sh — FileAgent MinIO initialisation script
#
# This script is IDEMPOTENT: safe to run multiple times.
# It uses `mc` (MinIO Client) to:
#   1. Create the required buckets (data-sensor, tmp-uploads)
#   2. Set a 7-day lifecycle expiry on tmp-uploads
#   3. Create the controlplane-admin IAM user and least-privilege policy
#   4. Configure the webhook event notification target
#   5. Subscribe data-sensor bucket to the webhook target
#   6. Assert the IAM user can AssumeRole, create a bucket, and serve a
#      presigned GET — i.e. all three things the Control Plane does at runtime
#
# Requirements: `mc` AND `curl` (>= 7.75, for --aws-sigv4) must both be on PATH.
# No grep/sed/awk is used, so the script also runs inside the `minio/minio`
# image, which ships mc + curl + bash but no coreutils text tools. Note that the
# `minio/mc` image has no curl and therefore cannot run this script.
#
# Environment variables (all have defaults suitable for local dev):
#   MINIO_ENDPOINT          MinIO API address           (default: http://localhost:9000)
#   MINIO_ROOT_USER         MinIO root / admin user     (default: minioadmin)
#   MINIO_ROOT_PASSWORD     MinIO root / admin password (default: minioadmin)
#   MINIO_ALIAS             mc alias name               (default: myminio)
#   CP_ADMIN_ACCESS_KEY     controlplane-admin key id   (default: cpAdminIAM000000000)
#   CP_ADMIN_SECRET_KEY     controlplane-admin secret   (default: cpAdminSecret00000000)
#   CP_ADMIN_ROTATE         Set to 1 to allow resetting the secret of an
#                           already existing IAM user  (default: 0 — refuse)
#   MINIO_ROLE_ARN          Role used for STS check      (default: arn:aws:iam:::role/agent-role)
#   WEBHOOK_ENDPOINT        URL MinIO pushes events to  (default: http://controlplane:8080/internal/minio-event)
#   WEBHOOK_AUTH_TOKEN      Shared secret for webhook   (default: changeme)
#   WEBHOOK_TARGET_NAME     mc webhook config key       (default: primary)

set -euo pipefail

# ---------------------------------------------------------------------------
# Configuration
# ---------------------------------------------------------------------------
MINIO_ENDPOINT="${MINIO_ENDPOINT:-http://localhost:9000}"
MINIO_ROOT_USER="${MINIO_ROOT_USER:-minioadmin}"
MINIO_ROOT_PASSWORD="${MINIO_ROOT_PASSWORD:-minioadmin}"
MINIO_ALIAS="${MINIO_ALIAS:-myminio}"

CP_ADMIN_ACCESS_KEY="${CP_ADMIN_ACCESS_KEY:-cpAdminIAM000000000}"
CP_ADMIN_SECRET_KEY="${CP_ADMIN_SECRET_KEY:-cpAdminSecret00000000}"
CP_ADMIN_ROTATE="${CP_ADMIN_ROTATE:-0}"
MINIO_ROLE_ARN="${MINIO_ROLE_ARN:-arn:aws:iam:::role/agent-role}"

WEBHOOK_ENDPOINT="${WEBHOOK_ENDPOINT:-http://controlplane:8080/internal/minio-event}"
WEBHOOK_AUTH_TOKEN="${WEBHOOK_AUTH_TOKEN:-changeme}"
WEBHOOK_TARGET_NAME="${WEBHOOK_TARGET_NAME:-primary}"

BUCKET_DATA="data-sensor"
BUCKET_TMP="tmp-uploads"
LIFECYCLE_DAYS=7
CP_POLICY_NAME="fileagent-controlplane"

# Length guard. MinIO enforces only the *lower* bounds on IAM users (access key
# >= 3, secret >= 8) and accepts longer values; the 20 / 40 upper bounds are a
# service-account restriction. This script deliberately converges on the
# narrower service-account window so one credential value stays valid for either
# account form, and so an out-of-range value is reported here by variable name
# instead of surfacing as mc's opaque credential error after the script has
# already changed cluster state.
if [ "${#CP_ADMIN_ACCESS_KEY}" -lt 3 ] || [ "${#CP_ADMIN_ACCESS_KEY}" -gt 20 ]; then
  echo "ERROR: CP_ADMIN_ACCESS_KEY length must be between 3 and 20 characters; got ${#CP_ADMIN_ACCESS_KEY}." >&2
  exit 1
fi
if [ "${#CP_ADMIN_SECRET_KEY}" -lt 8 ] || [ "${#CP_ADMIN_SECRET_KEY}" -gt 40 ]; then
  echo "ERROR: CP_ADMIN_SECRET_KEY length must be between 8 and 40 characters; got ${#CP_ADMIN_SECRET_KEY}." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# Helper: check mc and curl are available
# ---------------------------------------------------------------------------
if ! command -v mc &>/dev/null; then
  echo "ERROR: 'mc' (MinIO Client) is not installed or not in PATH." >&2
  echo "       Install it from https://min.io/docs/minio/linux/reference/minio-mc.html" >&2
  exit 1
fi
if ! command -v curl &>/dev/null; then
  echo "ERROR: 'curl' is required for the STS and presigned-GET self-checks." >&2
  echo "       The 'minio/mc' image does not ship curl; use 'minio/minio' (mc + curl) instead." >&2
  exit 1
fi
# `case` rather than grep: the minio/minio image has curl but no grep.
case "$(curl --help all 2>/dev/null || true)" in
  *--aws-sigv4*) ;;
  *)
    echo "ERROR: curl does not support --aws-sigv4; upgrade curl before initialising MinIO." >&2
    exit 1
    ;;
esac

# ---------------------------------------------------------------------------
# Helper: force curl's 'localhost' connections onto IPv4
#
# On developer machines 'localhost' resolves to both ::1 and 127.0.0.1, while a
# published container port is commonly bound on IPv4 only. curl then burns its
# connect timeout on [::1] (or fails outright where the v6 socket is refused)
# even though MinIO is up. --connect-to rewrites only the transport target: the
# Host header, and therefore the SigV4 signature and the presigned URL, are
# untouched. A literal IP or a DNS name needs no remapping, so this applies to
# 'localhost' alone. Both self-check curl calls use it, so a remap that is ever
# needed is applied consistently.
#
# NB: bash 3.2 (macOS system bash) treats "${arr[@]}" of an *empty* array as an
# unbound variable under `set -u`. Every expansion below must therefore use the
# ${arr[@]+"${arr[@]}"} form, which expands to nothing when the array is empty.
# ---------------------------------------------------------------------------
CURL_CONNECT_ARGS=()
MINIO_AUTHORITY="${MINIO_ENDPOINT#*://}"
MINIO_AUTHORITY="${MINIO_AUTHORITY%%/*}"
if [[ "${MINIO_AUTHORITY}" == localhost:* ]]; then
  CURL_CONNECT_ARGS=(--connect-to "${MINIO_AUTHORITY}:127.0.0.1:${MINIO_AUTHORITY##*:}")
fi

# ---------------------------------------------------------------------------
# Helper: call STS AssumeRole with an explicit credential pair.
#
# Writes the raw response (success XML or MinIO's error XML) to stdout and
# returns curl's exit status, so callers can use it both as a probe ("do these
# credentials still authenticate?") and as the final self-check.
# ---------------------------------------------------------------------------
STS_SESSION_POLICY='{"Version":"2012-10-17","Statement":[{"Effect":"Allow","Action":["s3:ListBucketMultipartUploads"],"Resource":["arn:aws:s3:::data-sensor"]},{"Effect":"Allow","Action":["s3:PutObject","s3:AbortMultipartUpload","s3:ListMultipartUploadParts"],"Resource":["arn:aws:s3:::data-sensor/*"]}]}'
assume_role() {
  curl --silent --show-error --fail-with-body \
    ${CURL_CONNECT_ARGS[@]+"${CURL_CONNECT_ARGS[@]}"} \
    --aws-sigv4 "aws:amz:us-east-1:sts" \
    --user "$1:$2" \
    --header "Content-Type: application/x-www-form-urlencoded" \
    --data-urlencode "Action=AssumeRole" \
    --data-urlencode "Version=2011-06-15" \
    --data-urlencode "DurationSeconds=3600" \
    --data-urlencode "RoleArn=${MINIO_ROLE_ARN}" \
    --data-urlencode "RoleSessionName=fileagent-init-check" \
    --data-urlencode "Policy=${STS_SESSION_POLICY}" \
    "${MINIO_ENDPOINT}" 2>&1
}

# ---------------------------------------------------------------------------
# 1. Register (or refresh) the mc alias
# ---------------------------------------------------------------------------
echo "==> Configuring mc alias '${MINIO_ALIAS}' -> ${MINIO_ENDPOINT}"
mc alias set "${MINIO_ALIAS}" "${MINIO_ENDPOINT}" \
  "${MINIO_ROOT_USER}" "${MINIO_ROOT_PASSWORD}" --api S3v4 --quiet

# ---------------------------------------------------------------------------
# 2. Create buckets (idempotent — mc mb --ignore-existing)
# ---------------------------------------------------------------------------
echo "==> Creating bucket: ${BUCKET_DATA}"
mc mb --ignore-existing "${MINIO_ALIAS}/${BUCKET_DATA}"

echo "==> Creating bucket: ${BUCKET_TMP}"
mc mb --ignore-existing "${MINIO_ALIAS}/${BUCKET_TMP}"

# ---------------------------------------------------------------------------
# 3. Apply lifecycle rule: expire objects in tmp-uploads after 7 days
# ---------------------------------------------------------------------------
echo "==> Setting ${LIFECYCLE_DAYS}-day lifecycle on ${BUCKET_TMP}"

# Build the lifecycle JSON in a temp file so we can pipe it to mc.
LIFECYCLE_JSON=$(mktemp /tmp/fileagent-lifecycle-XXXXXX.json)
CP_POLICY_JSON=$(mktemp /tmp/fileagent-cp-policy-XXXXXX.json)
PRESIGNED_BODY=$(mktemp /tmp/fileagent-presigned-body-XXXXXX.txt)
CHECK_OBJECT=""
CHECK_BUCKET=""
CP_CHECK_ALIAS=""

cleanup() {
  if [ -n "${CHECK_OBJECT}" ]; then
    mc rm --force "${MINIO_ALIAS}/${CHECK_OBJECT}" >/dev/null 2>&1 || true
  fi
  if [ -n "${CHECK_BUCKET}" ]; then
    mc rb --force "${MINIO_ALIAS}/${CHECK_BUCKET}" >/dev/null 2>&1 || true
  fi
  if [ -n "${CP_CHECK_ALIAS}" ]; then
    mc alias remove "${CP_CHECK_ALIAS}" >/dev/null 2>&1 || true
  fi
  rm -f "${LIFECYCLE_JSON}" "${CP_POLICY_JSON}" "${PRESIGNED_BODY}"
}
trap cleanup EXIT

cat >"${LIFECYCLE_JSON}" <<EOF
{
  "Rules": [
    {
      "Expiration": {"Days": ${LIFECYCLE_DAYS}},
      "ID": "auto-expire-tmp-uploads",
      "Status": "Enabled"
    }
  ]
}
EOF

# Current mc releases accept lifecycle imports as JSON.
mc ilm import "${MINIO_ALIAS}/${BUCKET_TMP}" <"${LIFECYCLE_JSON}"

# ---------------------------------------------------------------------------
# 4. Create/update the controlplane-admin IAM user and policy (idempotent)
#
# The wildcard resources are intentional: POST /api/v1/buckets can create
# buckets at runtime, so a static bucket list would make the Control Plane fail
# against every newly created bucket. This is still substantially narrower than
# MinIO's readwrite/consoleAdmin policies: the Control Plane cannot delete or
# list objects/buckets, nor call admin APIs. It gets only CreateBucket,
# GetObject for presigned downloads, STS AssumeRole, and the write-only actions
# its STS sessions are allowed to delegate.
# ---------------------------------------------------------------------------
cat >"${CP_POLICY_JSON}" <<'EOF'
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:CreateBucket",
        "s3:ListBucketMultipartUploads"
      ],
      "Resource": ["arn:aws:s3:::*"]
    },
    {
      "Effect": "Allow",
      "Action": [
        "s3:GetObject",
        "s3:PutObject",
        "s3:AbortMultipartUpload",
        "s3:ListMultipartUploadParts"
      ],
      "Resource": ["arn:aws:s3:::*/*"]
    }
  ]
}
EOF

# `mc admin user add` resets the secret of an existing user unconditionally.
# Doing that on every re-run would silently revoke the credential a running
# Control Plane holds — and the self-checks below would still pass, because they
# use the *new* secret. So probe first: AssumeRole authenticates the credential
# pair (it succeeds even before a policy is attached), which tells us whether
# the values handed to this run are already the live ones.
CP_SECRET_ROTATED="no"
if mc admin user info "${MINIO_ALIAS}" "${CP_ADMIN_ACCESS_KEY}" >/dev/null 2>&1; then
  if assume_role "${CP_ADMIN_ACCESS_KEY}" "${CP_ADMIN_SECRET_KEY}" >/dev/null 2>&1; then
    echo "==> IAM user '${CP_ADMIN_ACCESS_KEY}' exists and the supplied secret already works; leaving it unchanged"
  elif [ "${CP_ADMIN_ROTATE}" = "1" ]; then
    echo "==> CP_ADMIN_ROTATE=1 — rotating the secret of existing IAM user '${CP_ADMIN_ACCESS_KEY}'"
    mc admin user add "${MINIO_ALIAS}" "${CP_ADMIN_ACCESS_KEY}" "${CP_ADMIN_SECRET_KEY}"
    CP_SECRET_ROTATED="yes"
  else
    echo "ERROR: IAM user '${CP_ADMIN_ACCESS_KEY}' already exists, but the supplied" >&2
    echo "       CP_ADMIN_SECRET_KEY does not authenticate against it." >&2
    echo "       Refusing to reset it: a running Control Plane may still be using the" >&2
    echo "       current secret, and resetting it here would only surface as Access" >&2
    echo "       Denied on the agents once their STS sessions expire." >&2
    echo "       - To leave the deployment as is, re-run with the secret the Control" >&2
    echo "         Plane uses (its MINIO_SECRET_KEY)." >&2
    echo "       - To rotate deliberately, re-run with CP_ADMIN_ROTATE=1 and afterwards" >&2
    echo "         update MINIO_ACCESS_KEY/MINIO_SECRET_KEY and restart the Control Plane." >&2
    exit 1
  fi
else
  echo "==> Creating IAM user '${CP_ADMIN_ACCESS_KEY}'"
  mc admin user add "${MINIO_ALIAS}" "${CP_ADMIN_ACCESS_KEY}" "${CP_ADMIN_SECRET_KEY}"
fi

if mc admin policy info "${MINIO_ALIAS}" "${CP_POLICY_NAME}" >/dev/null 2>&1; then
  echo "WARNING: IAM policy '${CP_POLICY_NAME}' already exists and will be replaced with the current definition." >&2
else
  echo "==> Creating IAM policy '${CP_POLICY_NAME}'"
fi
mc admin policy create "${MINIO_ALIAS}" "${CP_POLICY_NAME}" "${CP_POLICY_JSON}"

echo "==> Attaching IAM policy '${CP_POLICY_NAME}' to '${CP_ADMIN_ACCESS_KEY}'"
mc admin policy attach "${MINIO_ALIAS}" "${CP_POLICY_NAME}" --user "${CP_ADMIN_ACCESS_KEY}"

# ---------------------------------------------------------------------------
# 5. Configure webhook event notification target (idempotent)
#    mc admin config set is a full replace, so running it multiple times is safe.
# ---------------------------------------------------------------------------
echo "==> Configuring webhook notification target '${WEBHOOK_TARGET_NAME}'"
mc admin config set "${MINIO_ALIAS}" \
  "notify_webhook:${WEBHOOK_TARGET_NAME}" \
  "endpoint=${WEBHOOK_ENDPOINT}" \
  "auth_token=${WEBHOOK_AUTH_TOKEN}" \
  "queue_limit=10000" \
  "queue_dir=/tmp/minio-webhook-queue"

# Restart MinIO to apply the config change (required for webhook settings)
echo "==> Restarting MinIO service to apply config changes"
mc admin service restart "${MINIO_ALIAS}" --json

# Wait for MinIO to come back up
echo "==> Waiting for MinIO to be ready..."
RETRIES=30
until mc ready "${MINIO_ALIAS}" --quiet 2>/dev/null || [ "${RETRIES}" -le 0 ]; do
  sleep 2
  RETRIES=$((RETRIES - 1))
done
if [ "${RETRIES}" -le 0 ]; then
  echo "ERROR: MinIO did not become ready after restart." >&2
  exit 1
fi

# ---------------------------------------------------------------------------
# 6. Subscribe data-sensor bucket to the webhook target (idempotent)
#    --ignore-existing makes an identical subscription a no-op.
# ---------------------------------------------------------------------------
echo "==> Subscribing '${BUCKET_DATA}' to webhook '${WEBHOOK_TARGET_NAME}'"
mc event add "${MINIO_ALIAS}/${BUCKET_DATA}" \
  "arn:minio:sqs::${WEBHOOK_TARGET_NAME}:webhook" \
  --ignore-existing \
  --event "put,delete"

# ---------------------------------------------------------------------------
# 7. Self-check the exact Control Plane credentials produced above.
#    One probe per capability the Control Plane actually exercises at runtime:
#    STS AssumeRole (agent credentials), CreateBucket (POST /api/v1/buckets),
#    and presigned GET (download URLs).
# ---------------------------------------------------------------------------
echo "==> Self-checking IAM user AssumeRole"
STS_OUT=""
if ! STS_OUT=$(assume_role "${CP_ADMIN_ACCESS_KEY}" "${CP_ADMIN_SECRET_KEY}"); then
  echo "ERROR: IAM user '${CP_ADMIN_ACCESS_KEY}' failed AssumeRole: ${STS_OUT}" >&2
  exit 1
fi
if [[ "${STS_OUT}" != *"<AccessKeyId>"* ]] || \
   [[ "${STS_OUT}" != *"<SecretAccessKey>"* ]] || \
   [[ "${STS_OUT}" != *"<SessionToken>"* ]]; then
  echo "ERROR: AssumeRole response did not contain a complete temporary credential set." >&2
  exit 1
fi
# Parameter expansion rather than sed: the minio/minio image ships no sed.
STS_ACCESS_KEY="${STS_OUT#*<AccessKeyId>}"
STS_ACCESS_KEY="${STS_ACCESS_KEY%%</AccessKeyId>*}"
echo "    Self-check AssumeRole: OK (temporary access key prefix: ${STS_ACCESS_KEY:0:8}...)"

CP_CHECK_ALIAS="${MINIO_ALIAS}-cp-check-$$"
mc alias set "${CP_CHECK_ALIAS}" "${MINIO_ENDPOINT}" \
  "${CP_ADMIN_ACCESS_KEY}" "${CP_ADMIN_SECRET_KEY}" --api S3v4 --quiet

echo "==> Self-checking IAM user CreateBucket"
CHECK_BUCKET="fileagent-init-check-$$-$(date +%s)"
MB_OUT=""
if ! MB_OUT=$(mc mb "${CP_CHECK_ALIAS}/${CHECK_BUCKET}" 2>&1); then
  echo "ERROR: IAM user '${CP_ADMIN_ACCESS_KEY}' cannot create a bucket: ${MB_OUT}" >&2
  echo "       POST /api/v1/buckets would fail at runtime; check s3:CreateBucket in policy '${CP_POLICY_NAME}'." >&2
  exit 1
fi
# The IAM user deliberately has no DeleteBucket, so drop the probe as root.
if ! mc rb --force "${MINIO_ALIAS}/${CHECK_BUCKET}" >/dev/null; then
  echo "ERROR: CreateBucket self-check succeeded, but root cleanup failed for bucket '${CHECK_BUCKET}'." >&2
  exit 1
fi
CHECK_BUCKET=""
echo "    Self-check CreateBucket: OK (probe bucket created and removed)"

echo "==> Self-checking IAM user presigned GET"
CHECK_OBJECT="${BUCKET_TMP}/.init-check/$(date +%s)-$$"
CHECK_BODY="fileagent-minio-init-check"
printf '%s' "${CHECK_BODY}" | mc pipe "${MINIO_ALIAS}/${CHECK_OBJECT}" >/dev/null

SHARE_OUT=""
if ! SHARE_OUT=$(mc share download --expire 5m --json \
  "${CP_CHECK_ALIAS}/${CHECK_OBJECT}" 2>&1); then
  echo "ERROR: IAM user '${CP_ADMIN_ACCESS_KEY}' failed to sign a presigned GET: ${SHARE_OUT}" >&2
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
  --output "${PRESIGNED_BODY}" --write-out "%{http_code}" "${PRESIGNED_URL}")
if [ "${HTTP_CODE}" != "200" ]; then
  echo "ERROR: presigned GET returned HTTP ${HTTP_CODE}; expected 200." >&2
  exit 1
fi
if [ "$(<"${PRESIGNED_BODY}")" != "${CHECK_BODY}" ]; then
  echo "ERROR: presigned GET returned unexpected object content." >&2
  exit 1
fi

# The IAM user deliberately has no DeleteObject. Cleanup therefore uses the
# root alias, preserving the least-privilege policy while leaving no probe data.
if ! mc rm --force "${MINIO_ALIAS}/${CHECK_OBJECT}" >/dev/null; then
  echo "ERROR: presigned-GET self-check succeeded, but root cleanup failed for '${CHECK_OBJECT}'." >&2
  exit 1
fi
CHECK_OBJECT=""
if ! mc alias remove "${CP_CHECK_ALIAS}" >/dev/null; then
  echo "ERROR: failed to remove temporary mc alias '${CP_CHECK_ALIAS}'." >&2
  exit 1
fi
CP_CHECK_ALIAS=""
echo "    Self-check presigned GET: OK (HTTP 200, object content verified and removed)"

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
echo ""
echo "MinIO initialisation complete."
echo "  Buckets   : ${BUCKET_DATA}, ${BUCKET_TMP}"
echo "  Lifecycle : ${BUCKET_TMP} objects expire after ${LIFECYCLE_DAYS} days"
echo "  IAM user  : ${CP_ADMIN_ACCESS_KEY}"
echo "  IAM policy: ${CP_POLICY_NAME}"
echo "  Webhook   : ${WEBHOOK_TARGET_NAME} -> ${WEBHOOK_ENDPOINT}"
if [ "${CP_SECRET_ROTATED}" = "yes" ]; then
  echo ""
  echo "  ⚠️  The secret of IAM user '${CP_ADMIN_ACCESS_KEY}' WAS ROTATED by this run."
  echo "      Update the Control Plane's MINIO_ACCESS_KEY / MINIO_SECRET_KEY to match"
  echo "      and restart it; already-issued STS sessions keep working until they"
  echo "      expire (<= 1h), so agents will fail later, not immediately."
fi
