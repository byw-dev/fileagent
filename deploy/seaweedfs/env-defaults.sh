# env-defaults.sh — single source of truth for SeaweedFS script defaults.
#
# Sourced by deploy/scripts/render-seaweedfs-configs.sh,
# deploy/scripts/init-seaweedfs.sh, and the FA_STORAGE=seaweedfs branch of
# deploy/scripts/smoke.sh, so the Control Plane credentials CP launches with
# are guaranteed to be the same values the renderer wrote into s3.json and the
# init script self-checks against (rework P2-2: they previously drifted when
# SW_* overrides were set).
#
# Values set ONLY when the variable is unset or empty, so exporting any SW_*
# before invoking those scripts overrides everything consistently.
#
# ⚠️ Every default below is a DEVELOPMENT placeholder, not a secret. Real
#    deployments must override all of them via environment.
: "${SW_ADMIN_ACCESS_KEY:=swAdmin00000000000}"   # storage admin identity key id
: "${SW_ADMIN_SECRET_KEY:=swAdminSecret0000000}" # storage admin identity secret
: "${SW_CP_ACCESS_KEY:=cpAdminIAM000000000}"     # Control Plane identity key id
: "${SW_CP_SECRET_KEY:=cpAdminSecret00000000}"   # Control Plane identity secret
# STS token signing key: MUST be valid standard base64 decoding to >= 16 bytes
# (live-verified on 4.48: shorter -> "signing key must be at least 16 bytes"
# and the S3 STS service refuses to load; non-base64 -> "illegal base64 data").
# Default decodes to 29 bytes. Generate real ones with: openssl rand -base64 32
: "${SW_STS_SIGNING_KEY:=ZmlsZUFnZW50LWRldi1zdHMtc2lnbmluZy1rZXk=}"
: "${SW_AGENT_ROLE_ARN:=arn:aws:iam::000000000000:role/agent-role}"
