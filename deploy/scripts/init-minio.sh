#!/usr/bin/env bash
# init-minio.sh — FileAgent MinIO initialisation script
#
# This script is IDEMPOTENT: safe to run multiple times.
# It uses `mc` (MinIO Client) to:
#   1. Create the required buckets (data-sensor, tmp-uploads)
#   2. Set a 7-day lifecycle expiry on tmp-uploads
#   3. Create the controlplane-admin service account
#   4. Configure the webhook event notification target
#   5. Subscribe data-sensor bucket to the webhook target
#
# Environment variables (all have defaults suitable for local dev):
#   MINIO_ENDPOINT          MinIO API address           (default: http://localhost:9000)
#   MINIO_ROOT_USER         MinIO root / admin user     (default: minioadmin)
#   MINIO_ROOT_PASSWORD     MinIO root / admin password (default: minioadmin)
#   MINIO_ALIAS             mc alias name               (default: myminio)
#   CP_ADMIN_ACCESS_KEY     controlplane-admin key id   (default: cpAdmin00000000000000)
#   CP_ADMIN_SECRET_KEY     controlplane-admin secret   (default: cpAdminSecret00000000)
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

CP_ADMIN_ACCESS_KEY="${CP_ADMIN_ACCESS_KEY:-cpAdmin00000000000000}"
CP_ADMIN_SECRET_KEY="${CP_ADMIN_SECRET_KEY:-cpAdminSecret00000000}"

WEBHOOK_ENDPOINT="${WEBHOOK_ENDPOINT:-http://controlplane:8080/internal/minio-event}"
WEBHOOK_AUTH_TOKEN="${WEBHOOK_AUTH_TOKEN:-changeme}"
WEBHOOK_TARGET_NAME="${WEBHOOK_TARGET_NAME:-primary}"

BUCKET_DATA="data-sensor"
BUCKET_TMP="tmp-uploads"
LIFECYCLE_DAYS=7

# ---------------------------------------------------------------------------
# Helper: check mc is available
# ---------------------------------------------------------------------------
if ! command -v mc &>/dev/null; then
  echo "ERROR: 'mc' (MinIO Client) is not installed or not in PATH." >&2
  echo "       Install it from https://min.io/docs/minio/linux/reference/minio-mc.html" >&2
  exit 1
fi

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

# Build the lifecycle XML in a temp file so we can pipe it to mc
LIFECYCLE_XML=$(mktemp /tmp/lifecycle-XXXXXX.xml)
trap 'rm -f "${LIFECYCLE_XML}"' EXIT

cat >"${LIFECYCLE_XML}" <<EOF
<LifecycleConfiguration>
  <Rule>
    <ID>auto-expire-tmp-uploads</ID>
    <Status>Enabled</Status>
    <Filter>
      <Prefix></Prefix>
    </Filter>
    <Expiration>
      <Days>${LIFECYCLE_DAYS}</Days>
    </Expiration>
  </Rule>
</LifecycleConfiguration>
EOF

# mc ilm import reads the XML from stdin
mc ilm import "${MINIO_ALIAS}/${BUCKET_TMP}" <"${LIFECYCLE_XML}"

# ---------------------------------------------------------------------------
# 4. Create the controlplane-admin service account (idempotent)
#    mc admin user svcacct add exits 0 even if the key already exists;
#    the --access-key flag pins the access key so repeated runs are safe.
# ---------------------------------------------------------------------------
echo "==> Creating service account '${CP_ADMIN_ACCESS_KEY}'"
SVCACCT_OUT=$(mc admin user svcacct add \
  --access-key "${CP_ADMIN_ACCESS_KEY}" \
  --secret-key "${CP_ADMIN_SECRET_KEY}" \
  "${MINIO_ALIAS}" "${MINIO_ROOT_USER}" 2>&1) || {
  if echo "${SVCACCT_OUT}" | grep -qi "already exists"; then
    echo "    Service account already exists — skipping."
  else
    echo "ERROR: Failed to create service account:" >&2
    echo "${SVCACCT_OUT}" >&2
    exit 1
  fi
}

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
mc admin service restart "${MINIO_ALIAS}" --quiet

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
#    mc event add is idempotent: adding an identical rule a second time is a no-op.
# ---------------------------------------------------------------------------
echo "==> Subscribing '${BUCKET_DATA}' to webhook '${WEBHOOK_TARGET_NAME}'"
EVENT_OUT=$(mc event add "${MINIO_ALIAS}/${BUCKET_DATA}" \
  "arn:minio:sqs::${WEBHOOK_TARGET_NAME}:webhook" \
  --event "s3:ObjectCreated:*,s3:ObjectRemoved:*" 2>&1) || {
  if echo "${EVENT_OUT}" | grep -qi "already exists"; then
    echo "    Event notification already configured — skipping."
  else
    echo "ERROR: Failed to add event notification:" >&2
    echo "${EVENT_OUT}" >&2
    exit 1
  fi
}

# ---------------------------------------------------------------------------
# Done
# ---------------------------------------------------------------------------
echo ""
echo "MinIO initialisation complete."
echo "  Buckets   : ${BUCKET_DATA}, ${BUCKET_TMP}"
echo "  Lifecycle : ${BUCKET_TMP} objects expire after ${LIFECYCLE_DAYS} days"
echo "  Svc acct  : ${CP_ADMIN_ACCESS_KEY}"
echo "  Webhook   : ${WEBHOOK_TARGET_NAME} -> ${WEBHOOK_ENDPOINT}"
