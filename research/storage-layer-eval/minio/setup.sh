#!/usr/bin/env bash
# Configure the baseline MinIO the way init-minio.sh does (CP IAM user + named
# policy, webhook target with persistent queue, bucket event subscription).
set -euo pipefail
here=$(cd "$(dirname "$0")" && pwd)
export MC_CONFIG_DIR="$here/.mc"
A=semin
mc alias set $A http://localhost:19100 minioadmin minioadmin --api S3v4 >/dev/null
mc mb --ignore-existing $A/probe-grant $A/probe-other >/dev/null
mc admin user add $A cpadmin cpadmin-secret >/dev/null
cat > "$here/cp-policy.json" <<'P'
{"Version":"2012-10-17","Statement":[
 {"Effect":"Allow","Action":["s3:CreateBucket","s3:ListBucketMultipartUploads"],"Resource":["arn:aws:s3:::*"]},
 {"Effect":"Allow","Action":["s3:GetObject","s3:PutObject","s3:AbortMultipartUpload","s3:ListMultipartUploadParts"],"Resource":["arn:aws:s3:::*/*"]}]}
P
mc admin policy create $A fileagent-cp "$here/cp-policy.json" >/dev/null
mc admin policy attach $A fileagent-cp --user cpadmin >/dev/null 2>&1 || true
python3 -m http.server 18990 --bind 0.0.0.0 >/dev/null 2>&1 & dummy=$!
trap 'kill $dummy 2>/dev/null || true' EXIT
sleep 1
mc admin config set $A notify_webhook:probe endpoint=http://host.docker.internal:18990/ auth_token=probe-token queue_dir=/queue queue_limit=10000 >/dev/null
mc admin service restart $A --json >/dev/null
until mc ready $A --quiet 2>/dev/null; do sleep 1; done
mc event add $A/probe-grant arn:minio:sqs::probe:webhook --event put,delete --ignore-existing >/dev/null
mc event ls $A/probe-grant
kill $dummy
echo SETUP OK
