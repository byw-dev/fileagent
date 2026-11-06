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
// Choice of operation (live-verified on both backends, PoC 2026-11-06):
//   - MinIO init-minio.sh policy "fileagent-controlplane" grants
//     s3:ListBucketMultipartUploads on arn:aws:s3:::*;
//   - SeaweedFS deploy/seaweedfs/s3.json.tpl policy "fileagent-cp" grants
//     s3:ListBucketMultipartUploads on arn:aws:s3:::*;
//   - it is the only read-shaped action in the intersection that touches no
//     object and creates nothing, so a passing probe proves endpoint
//     reachability AND credential validity with zero side effects.
//
// Rework 4 finding 1: the probe bucket must PHYSICALLY exist. Every error
// fails the attempt — including NoSuchBucket. Two rounds of trying to
// distinguish "genuine" NoSuchBucket answers (exact code, then RequestID
// provenance) were each broken by minio-go's error synthesis (it fabricates
// NoSuchBucket for unparseable 404s and back-fills RequestID from the
// x-amz-request-id header, which any gateway can set); NO wire-derived
// signal reliably proves "authenticated but missing bucket". So the check
// now requires the bucket to exist: the init scripts create data-sensor
// before smoke/production traffic, a missing bucket retries inside the
// window (fresh deployments pass once init completes), and a wrong
// endpoint/policy can no longer slip through as a fake absence.
//
// Rework 5 finding 1: the channel API (ListIncompleteUploads) only decodes a
// 200 body into a struct with NO root-element validation — a mis-routed
// gateway answering 200 with arbitrary XML (or an S3 <Error> document)
// decoded to zero values and PASSED. The probe now uses the Core API, which
// returns the parsed ListMultipartUploadsResult, and requires BOTH err==nil
// AND result.Bucket == the configured bucket: a listing that does not name
// the configured bucket is a class=config failure (see errWrongProbeResponse
// and its fix hint), not a pass.
func (p ProbeS3) ListMultipartUploads(ctx context.Context, bucket string) error {
	result, err := miniogo.Core{Client: p.Client}.ListMultipartUploads(ctx, bucket, "", "", "", "", 1)
	if err != nil {
		return err
	}
	if result.Bucket != bucket {
		return errWrongProbeResponse{got: result.Bucket}
	}
	return nil
}

// errWrongProbeResponse marks a 200 answer that is not the configured
// bucket's S3 listing (wrong route / non-S3 service / decodable-but-foreign
// XML). diagnose() maps it to class=config with http_status=200 — a real S3
// error never carries 200, so FixHint can key the endpoint-routing hint off
// that shape. The error's own text is never logged.
type errWrongProbeResponse struct{ got string }

func (e errWrongProbeResponse) Error() string {
	return "endpoint answered 200 but not with the configured bucket's S3 listing"
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
// The single HTTP call is bounded by BOTH stsProbeTimeout and the REMAINING
// window time read off ctx (rework 3 finding 4: minio-go's STS flow takes
// no context, so without this the call could run 10s past window expiry).
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
	timeout := stsProbeTimeout
	if deadline, ok := ctx.Deadline(); ok {
		if rem := time.Until(deadline); rem < timeout {
			if rem <= 0 {
				return fmt.Errorf("startupcheck sts: %w", context.DeadlineExceeded)
			}
			timeout = rem
		}
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
		Client: &http.Client{Timeout: timeout},
	})
	val, err := li.Get()
	if err != nil {
		return err
	}
	_ = val // credentials discarded by design: never cached, never logged
	return nil
}
