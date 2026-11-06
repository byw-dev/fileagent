package startupcheck

import (
	"context"
	"fmt"
	"net/http"
	"time"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// ProbeS3 implements S3Prober on a minio-go client bound to the INTERNAL
// endpoint with the CP identity — the exact client construction the runtime
// uses (main.go's admin client), so the check exercises the effective
// configuration rather than a parallel one.
type ProbeS3 struct {
	Client *miniogo.Client
}

// ListMultipartUploads performs GET /{bucket}?uploads (ListBucketMultipartUploads).
//
// Choice of operation (live-verified on both backends, PoC 2026-11-10):
//   - MinIO init-minio.sh policy "fileagent-controlplane" grants
//     s3:ListBucketMultipartUploads on arn:aws:s3:::*;
//   - SeaweedFS deploy/seaweedfs/s3.json.tpl policy "fileagent-cp" grants
//     s3:ListBucketMultipartUploads on arn:aws:s3:::*;
//   - it is the only read-shaped action in the intersection that touches no
//     object and creates nothing, so a passing probe proves endpoint
//     reachability AND credential validity with zero side effects.
//
// A missing bucket (NoSuchBucket) still proves both — bucket state is server
// data, not identity — so only non-404 errors fail the probe. minio-go's
// channel API surfaces the first error via ObjectMultipartInfo.Err; an empty
// channel is success (no incomplete uploads, or the listing was simply empty).
func (p ProbeS3) ListMultipartUploads(ctx context.Context, bucket string) error {
	for info := range p.Client.ListIncompleteUploads(ctx, bucket, "", false) {
		if info.Err != nil {
			if resp := miniogo.ToErrorResponse(info.Err); resp.Code == "NoSuchBucket" || resp.StatusCode == 404 {
				// Bucket absent: identity authenticated, endpoint reachable.
				return nil
			}
			return info.Err
		}
	}
	return nil
}

// stsProbeTimeout bounds a single AssumeRole call. minio-go's STS flow takes
// no context, so the bound must come from the HTTP client (same approach and
// value as storage.STSManager's stsRequestTimeout).
const stsProbeTimeout = 10 * time.Second

// AssumeRoleSTS implements STSAssumer via minio-go's credentials chain — the
// same mechanism storage.STSManager.IssueCredentials uses, so whatever the
// agents will experience at credential issue time, this probe experiences at
// startup.
type AssumeRoleSTS struct {
	Config Config
}

// AssumeRole performs one STS AssumeRole with the CP identity and the
// configured MINIO_ROLE_ARN, then DISCARDS the returned credentials: nothing
// is cached, nothing is logged beyond success/failure.
//
// DurationSeconds: both backends accept 900s as the minimum (live-verified:
// lower values return InvalidParameterValue on MinIO and SeaweedFS alike),
// but minio-go silently rewrites any DurationSeconds <= 3600 to its own
// default of 3600 (pkg/credentials/assume_role.go: defaultDurationSeconds),
// so the shortest duration actually attainable through this library is 3600
// — the value used here deliberately rather than fighting the library.
//
// KNOWN BLIND SPOT (live-verified): MinIO's STS does not validate RoleArn —
// AssumeRole succeeds for an arbitrary ARN — so a wrong MINIO_ROLE_ARN is
// detectable on SeaweedFS (AccessDenied "not authorized to assume role") but
// NOT on MinIO. That limitation belongs to the backend, not this check; it is
// recorded in the IC-BUG-40 card.
func (a AssumeRoleSTS) AssumeRole(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return fmt.Errorf("startupcheck sts: %w", err)
	}
	scheme := "http"
	if a.Config.UseSSL {
		scheme = "https"
	}
	li := credentials.New(&credentials.STSAssumeRole{
		STSEndpoint: scheme + "://" + a.Config.Endpoint,
		Options: credentials.STSAssumeRoleOptions{
			AccessKey:       a.Config.AccessKey,
			SecretKey:       a.Config.SecretKey,
			RoleARN:         a.Config.RoleARN,
			RoleSessionName: "fileagent-startup-selfcheck",
			DurationSeconds: 3600,
		},
		Client: &http.Client{Timeout: stsProbeTimeout},
	})
	val, err := li.Get()
	if err != nil {
		return err
	}
	_ = val // credentials discarded by design: never cached, never logged
	return nil
}
