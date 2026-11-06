// Package startupcheck implements the IC-BUG-40 storage startup self-check.
//
// The Control Plane used to construct its MinIO client with miniogo.New only —
// a purely local operation that sends no request — so wrong credentials, a
// deleted CP identity, or a rotated secret all booted "healthy", and the
// failure surfaced later and elsewhere (agent STS Access Denied, presigned
// download 500s, up to an hour of already-issued STS sessions away from the
// root cause). This package verifies, with the configuration that is actually
// in effect at runtime (MINIO_* env → config.Config), that:
//
//  1. the internal endpoint is reachable and the CP S3 identity authenticates,
//     using ListBucketMultipartUploads (minio-go ListIncompleteUploads) — the
//     one action present in the CP policy on BOTH backends (MinIO: init-minio.sh
//     policy "fileagent-controlplane"; SeaweedFS: deploy/seaweedfs/s3.json.tpl
//     policy "fileagent-cp"), and the only one in the intersection that reads
//     without touching objects or creating buckets;
//  2. one real STS AssumeRole with MINIO_ROLE_ARN succeeds. The issued
//     credentials are discarded immediately — never cached, never logged.
//
// Both backends (MinIO and SeaweedFS) go through the SAME checks: the CP only
// speaks the S3 API and STS, which both provide, so there is no per-backend
// branch here.
//
// Failure semantics (product decision 2026-11-06, refined by coordinator for
// IC-BUG-40): the check runs in a background goroutine AFTER the CP starts
// listening (init-minio.sh dials the CP webhook during initialization, so a
// blocking pre-listen check would deadlock a fresh deployment). ANY failure —
// auth/config included — retries with backoff inside one configurable window
// (MinIOStartupCheckTimeout, default 120s): compose stacks may race CP vs
// storage startup, and on a fresh MinIO deployment init-minio.sh legitimately
// creates the CP identity only after the CP is up. When the window expires
// still failing, the check reports every detail it has (which item, endpoint,
// access key ID — NEVER the secret, the raw error, a fix hint) and shuts the
// process down through the normal shutdown path (non-zero exit), not a bare
// os.Exit from the goroutine.
//
// Two live-verified backend facts encoded here (see the IC-BUG-40 card):
//   - MinIO's STS does NOT validate RoleArn (any ARN issues successfully), so
//     "wrong role ARN" is undetectable via STS on MinIO; SeaweedFS reports
//     AccessDenied. The blind spot is inherent to the backend, not this check.
//   - DurationSeconds: both backends allow 900s minimum, but minio-go silently
//     rewrites anything <= 3600 to its own default of 3600, so the probe uses
//     the library default (3600) — the shortest value actually attainable.
package startupcheck

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"
)

// S3Prober is the storage-side dependency: one authenticated S3 request with
// the CP identity. Implemented in production by the internal-endpoint minio-go
// client (see ProbeS3); abstract so unit tests never touch a real backend.
type S3Prober interface {
	// ListMultipartUploads performs GET /{bucket}?uploads — ListBucketMultipartUploads.
	ListMultipartUploads(ctx context.Context, bucket string) error
}

// STSAssumer is the STS-side dependency: one AssumeRole whose credentials are
// discarded. Implemented in production by AssumeRoleSTS (same mechanism as
// storage.STSManager.IssueCredentials); abstract for the same reason.
type STSAssumer interface {
	// AssumeRole performs one STS AssumeRole and discards the credentials.
	AssumeRole(ctx context.Context) error
}

// FailureClass classifies why a check attempt failed.
type FailureClass int

const (
	// ClassAuthConfig means the backend answered and rejected the identity or
	// the request shape: wrong secret (SignatureDoesNotMatch), unknown access
	// key (InvalidAccessKeyId), denied (AccessDenied), invalid role (STS).
	// A configuration error — the classic IC-BUG-40 case.
	ClassAuthConfig FailureClass = iota
	// ClassNetwork means no usable answer arrived: dial errors, timeouts, or
	// 5xx — the storage may simply not be up yet.
	ClassNetwork
)

// String implements fmt.Stringer for logs. An out-of-range value (only
// possible if a future edit adds a class and forgets the name) still prints
// a usable, greppable name rather than the empty string.
func (c FailureClass) String() string {
	switch c {
	case ClassAuthConfig:
		return "auth/config"
	case ClassNetwork:
		return "network"
	default:
		return fmt.Sprintf("FailureClass(%d)", int(c))
	}
}

// authErrorCodes are S3/STS error codes that mean "the backend answered and
// said no" rather than "could not reach/complete". Live-verified on both
// MinIO (RELEASE.2025-04-22) and SeaweedFS (4.48): wrong secret →
// SignatureDoesNotMatch (S3) / signature-mismatch text in STS ErrorResponse;
// unknown access key → InvalidAccessKeyId (S3) / AccessDenied wrapping the
// same message on MinIO STS; SeaweedFS wrong role → AccessDenied "not
// authorized to assume role".
var authErrorCodes = map[string]struct{}{
	"InvalidAccessKeyId":   {},
	"SignatureDoesNotMatch": {},
	"AccessDenied":          {},
	"InvalidClientTokenId":  {},
	"InvalidToken":          {},
	"ExpiredToken":          {},
	"Unauthorized":          {},
	"InvalidParameterValue": {},
	"MalformedPolicy":       {},
}

// ClassifyS3 maps an error from the S3 probe to a FailureClass.
//
// Any parsed S3 ErrorResponse with an auth/config code, or any HTTP 401/403,
// is ClassAuthConfig; 5xx statuses are ClassNetwork (backend reachable but
// not ready); everything else (url.Error wrapping net errors, context
// deadlines, undecodable bodies) is ClassNetwork — when in doubt we retry,
// because retrying is safe inside the bounded window while refusing early on
// a guess would break legitimate slow-starting deployments.
func ClassifyS3(err error) FailureClass {
	if err == nil {
		return ClassNetwork // not an error; callers never ask, but stay total
	}
	if resp := miniogo.ToErrorResponse(err); resp.Code != "" || resp.StatusCode != 0 {
		if resp.StatusCode >= 500 {
			return ClassNetwork
		}
		// Any non-5xx S3 answer means the backend is up and talking: it either
		// matched an auth/config code, or answered with a 4xx this version
		// does not know by name — still a request-level rejection (auth/config
		// family), never something retrying fixes.
		return ClassAuthConfig
	}
	return ClassNetwork
}

// ClassifySTS maps an error from the STS probe to a FailureClass.
//
// minio-go's STS path returns credentials.ErrorResponse (a DIFFERENT type
// from miniogo.ErrorResponse) or a bare *url.Error. The typed response has no
// status code, so classification rides on STSError.Code and — because MinIO
// wraps SigV4 failures as AccessDenied with a signature-mismatch message —
// on the message text. Auth-ish words in the message mean the backend
// answered; transport-shaped errors mean it did not.
func ClassifySTS(err error) FailureClass {
	if err == nil {
		return ClassNetwork
	}
	var errResp credentials.ErrorResponse
	if errors.As(err, &errResp) {
		if _, ok := authErrorCodes[errResp.STSError.Code]; ok {
			return ClassAuthConfig
		}
		msg := strings.ToLower(errResp.STSError.Message)
		for _, marker := range []string{"signature", "access key", "not authorized", "credential", "token"} {
			if strings.Contains(msg, marker) {
				return ClassAuthConfig
			}
		}
		return ClassNetwork
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return ClassNetwork
	}
	// Unknown shape: default to network (retryable within the window).
	return ClassNetwork
}

// Config carries the values the check needs; main.go fills them from
// config.Config so the check exercises the configuration actually in effect.
type Config struct {
	// Endpoint is the INTERNAL MinIO/SeaweedFS S3 endpoint (host:port).
	Endpoint string
	// AccessKey / SecretKey / UseSSL describe the CP S3 identity.
	AccessKey string
	SecretKey string
	UseSSL    bool
	// RoleARN is the STS role assumed once during the check.
	RoleARN string
	// ProbeBucket is the bucket the ListMultipartUploads probe targets
	// (data-sensor; bootstrap guarantees it exists). A NoSuchBucket answer is
	// a server-side state observation, not a credential failure — it still
	// counts as "endpoint reachable + identity valid" for this probe.
	ProbeBucket string
	// Timeout is the total retry window (MINIO_STARTUP_CHECK_TIMEOUT).
	Timeout time.Duration
	// Logger receives the structured check logs. Required.
	Logger *zap.Logger

	// RetryBackoff is the base delay between attempts (exponential, capped).
	// Overridable for tests; default 1s.
	RetryBackoff time.Duration

	// S3 and STS are the dependency seams. nil → production implementations
	// built from the fields above.
	S3  S3Prober
	STS STSAssumer
}

// maxBackoff caps the exponential retry delay so a long window does not end
// with multi-minute sleeps between attempts.
const maxBackoff = 10 * time.Second

// item names for logs.
const (
	itemS3  = "s3-identity (ListBucketMultipartUploads)"
	itemSTS = "sts-assume-role (RoleArn probe)"
)

// FixHint returns the repair hint for a failed item, for exit-time logs.
// Exported so main.go can build the Fatal log line from a *Failure.
func FixHint(roleARN, item string) string {
	switch item {
	case itemS3:
		return "check MINIO_ENDPOINT / MINIO_ACCESS_KEY / MINIO_SECRET_KEY against the storage side CP identity (init-minio.sh CP_ADMIN_* or deploy/seaweedfs s3.json SW_CP_*); on a fresh deployment run the init script first"
	default:
		return fmt.Sprintf("check MINIO_ROLE_ARN (%s) against the role configured on the storage side (init-minio.sh or deploy/seaweedfs iam.json) and that the CP identity may assume it; note MinIO's STS does not validate RoleArn, so a wrong ARN passes this check on MinIO and fails only on SeaweedFS", roleARN)
	}
}

// Run performs the self-check with retry. It returns nil once BOTH probes
// succeed, and a non-nil *Failure describing the last attempt when the
// configured window (cfg.Timeout) expires still failing. ctx cancellation
// aborts immediately with the context error (shutdown outranks the check).
//
// Run is synchronous; main.go wraps it in a goroutine. All attempts log at
// Warn with the classification, item, endpoint, access key ID (id only) and
// raw error — never the secret.
func Run(ctx context.Context, cfg Config) error {
	if cfg.Logger == nil {
		return errors.New("startupcheck: Logger is required")
	}
	if cfg.Timeout <= 0 {
		return errors.New("startupcheck: Timeout must be positive")
	}
	backoff := cfg.RetryBackoff
	if backoff <= 0 {
		backoff = time.Second
	}
	s3p, sts := cfg.S3, cfg.STS
	if s3p == nil {
		client, err := newInternalClient(cfg)
		if err != nil {
			return fmt.Errorf("startupcheck: cannot construct S3 client for endpoint %q: %w", cfg.Endpoint, err)
		}
		s3p = ProbeS3{Client: client}
	}
	if sts == nil {
		sts = AssumeRoleSTS{Config: cfg}
	}

	deadline := time.Now().Add(cfg.Timeout)
	var last *Failure
	delay := backoff
	for attempt := 1; ; attempt++ {
		if err := ctx.Err(); err != nil {
			if last != nil {
				// Cancelled mid-retry: prefer the pending failure (it explains
				// what was wrong) over a bare context error.
				return last
			}
			return fmt.Errorf("startupcheck: aborted by context cancellation: %w", err)
		}
		last = attemptOnce(ctx, cfg, s3p, sts)

		if last == nil {
			cfg.Logger.Info("storage startup self-check passed",
				zap.String("endpoint", cfg.Endpoint),
				zap.String("access_key_id", cfg.AccessKey),
				zap.String("role_arn", cfg.RoleARN),
				zap.Int("attempts", attempt),
			)
			return nil
		}

		if ctx.Err() != nil {
			return last
		}
		if time.Now().After(deadline) {
			last.Window = cfg.Timeout
			last.Attempts = attempt
			cfg.Logger.Error("storage startup self-check FAILED: window expired, shutting down",
				zap.String("item", last.Item),
				zap.String("class", last.Class.String()),
				zap.String("endpoint", cfg.Endpoint),
				zap.String("access_key_id", cfg.AccessKey),
				zap.Error(last.Err),
				zap.Int("attempts", attempt),
				zap.Duration("window", cfg.Timeout),
				zap.String("fix_hint", FixHint(cfg.RoleARN, last.Item)),
			)
			return last
		}
		cfg.Logger.Warn("storage startup self-check attempt failed; retrying",
			zap.String("item", last.Item),
			zap.String("class", last.Class.String()),
			zap.String("endpoint", cfg.Endpoint),
			zap.String("access_key_id", cfg.AccessKey),
			zap.Error(last.Err),
			zap.Int("attempt", attempt),
			zap.Duration("window_remaining", time.Until(deadline)),
			zap.String("hint", "if this is a fresh deployment where init-minio.sh / init-seaweedfs.sh has not run yet, this is expected and the check will pass once it does"),
		)
		select {
		case <-ctx.Done():
			return last
		case <-time.After(delay):
		}
		if delay < maxBackoff {
			delay *= 2
			if delay > maxBackoff {
				delay = maxBackoff
			}
		}
	}
}

// attemptOnce runs both probes once and converts any failure into a
// *Failure; nil means both passed.
func attemptOnce(ctx context.Context, cfg Config, s3p S3Prober, sts STSAssumer) *Failure {
	if err := s3p.ListMultipartUploads(ctx, cfg.ProbeBucket); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &Failure{Item: itemS3, Err: err, Class: ClassNetwork}
		}
		return &Failure{Item: itemS3, Err: err, Class: ClassifyS3(err)}
	}
	if err := sts.AssumeRole(ctx); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return &Failure{Item: itemSTS, Err: err, Class: ClassNetwork}
		}
		return &Failure{Item: itemSTS, Err: err, Class: ClassifySTS(err)}
	}
	return nil
}

// Failure describes the terminal state of the check after the window
// expired (or the context was cancelled mid-retry).
type Failure struct {
	// Item is which probe failed last (itemS3 / itemSTS).
	Item string
	// Class is the classification of the last error.
	Class FailureClass
	// Err is the raw last error (already included in logs; kept for callers).
	Err error
	// Window is the configured total retry window.
	Window time.Duration
	// Attempts is how many attempts were made.
	Attempts int
}

// Error implements error with the full IC-BUG-40 exit diagnostics: the item,
// endpoint, access key ID (NEVER the secret), the raw error, and a fix hint.
func (f *Failure) Error() string {
	return fmt.Sprintf("storage startup self-check failed after %d attempts over %s: item=%s class=%s err=%v",
		f.Attempts, f.Window, f.Item, f.Class, f.Err)
}

// LogFields returns the structured fields main.go logs at Fatal with.
func (f *Failure) LogFields(endpoint, accessKeyID, fixHint string) []zap.Field {
	return []zap.Field{
		zap.String("item", f.Item),
		zap.String("class", f.Class.String()),
		zap.String("endpoint", endpoint),
		zap.String("access_key_id", accessKeyID),
		zap.Error(f.Err),
		zap.Int("attempts", f.Attempts),
		zap.Duration("window", f.Window),
		zap.String("fix_hint", fixHint),
	}
}

// newInternalClient builds the minio-go client on the INTERNAL endpoint with
// the CP identity — the same construction main.go uses for the admin client,
// kept in one place so the check can never drift from the runtime wiring.
// A construction error is impossible for a validated endpoint (miniogo.New
// parses host:port); when it does happen the caller treats it as a terminal
// configuration failure, so the error is returned instead of a client.
//
// MINIO_PUBLIC_ENDPOINT is deliberately NOT probed: it is the client-facing
// address for agents/browsers and may be unreachable from the CP's network
// by design (D-024 split-endpoint deployments); probing it here would
// reject valid configurations.
func newInternalClient(cfg Config) (*miniogo.Client, error) {
	return miniogo.New(cfg.Endpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, ""),
		Secure: cfg.UseSSL,
	})
}
