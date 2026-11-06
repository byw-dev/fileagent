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
// auth / config / network — retries with backoff inside one configurable
// window (MinIOStartupCheckTimeout, default 120s): compose stacks may race
// CP vs storage startup, and on a fresh deployment the init script
// legitimately creates the CP identity and probe bucket only after the CP is
// up. When the window expires still failing, the check reports the
// whitelisted diagnostics only (item, class, endpoint, access key id, codes,
// status, net_kind, fix hint — NEVER the secret and NEVER raw error text,
// see diagnostics.go) and shuts the
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

// The FailureClass enum, its values and the diagnostic extraction
// (diagnose) live in diagnostics.go — see that file for the whitelist
// logging contract (rework 4 finding 2).

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
	// (MINIO_STARTUP_CHECK_BUCKET, default data-sensor). The bucket must
	// PHYSICALLY exist: a NoSuchBucket answer is a FAILED attempt (class
	// config) retried within the window — bootstrap only writes DB records,
	// the physical bucket is created by the init scripts, so on a fresh
	// deployment the check keeps retrying until init completes (rework 4
	// finding 1: no answer taken from the wire can prove the identity when
	// the bucket is absent, so absence is simply not a pass).
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
// Exported so main.go can build the exit log line from a *Failure. The hint
// is OUR text (safe by construction), keyed off the diagnostic: a
// NoSuchBucket S3 answer gets the bucket-specific runbook, everything else
// keeps the credential/role hints.
func FixHint(roleARN, probeBucket, item string, diag Diagnostic) string {
	if item == itemS3 {
		if diag.S3Code == "NoSuchBucket" {
			return fmt.Sprintf("probe bucket %q does not exist: run the init script (init-minio.sh / init-seaweedfs.sh, both create data-sensor), or point MINIO_STARTUP_CHECK_BUCKET at a bucket that physically exists", probeBucket)
		}
		return "check MINIO_ENDPOINT / MINIO_ACCESS_KEY / MINIO_SECRET_KEY against the storage side CP identity (init-minio.sh CP_ADMIN_* or deploy/seaweedfs s3.json SW_CP_*); on a fresh deployment run the init script first"
	}
	return fmt.Sprintf("check MINIO_ROLE_ARN (%s) against the role configured on the storage side (init-minio.sh or deploy/seaweedfs iam.json) and that the CP identity may assume it; note MinIO's STS does not validate RoleArn, so a wrong ARN passes this check on MinIO and fails only on SeaweedFS", roleARN)
}

// Run performs the self-check with retry. It returns:
//
//   - nil once BOTH probes succeed;
//   - a *Failure when the retry window (cfg.Timeout) expires still failing —
//     the ONLY terminal-failure return: main.go treats exactly this shape as
//     "refuse to run" (rework 3 finding 1: a plain SIGTERM during a failing
//     check used to surface as *Failure and turn a clean shutdown into a
//     non-zero exit);
//   - a wrapped context error when the PARENT ctx is cancelled (shutdown
//     outranks the check; this is NOT a startup failure);
//   - a plain error for invalid config (no logger, non-positive window,
//     unparseable endpoint).
//
// The window is enforced with a derived context (WithTimeout) so both the
// sleeps AND the probes are bounded by it (rework 3 finding 4: the previous
// wall-clock deadline only ran between attempts, letting a 1ms window sleep
// out a 10s backoff and letting in-flight STS calls run 10s past expiry).
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

	// The window is a derived context: cancellation of the parent (SIGTERM)
	// is distinguishable from expiry (window exhausted) by checking ctx.Err()
	// before wctx.Err().
	wctx, cancelWindow := context.WithTimeout(ctx, cfg.Timeout)
	defer cancelWindow()

	var last *Failure
	// lastSubstantive is the most recent attempt whose failure was NOT just
	// the window deadline cutting the probe mid-flight. When the window
	// expires ON a cut attempt, the terminal report must still name the
	// SUBSTANTIVE cause (e.g. s3_code=NoSuchBucket → the bucket runbook)
	// rather than a bare timeout — the operator reads the terminal line,
	// not the retry warns (spec, rework 4 finding 1).
	var lastSubstantive *Failure
	delay := backoff
	for attempt := 1; ; attempt++ {
		// Parent cancelled first (SIGTERM): clean shutdown, not a failure.
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("startupcheck: aborted by context cancellation: %w", err)
		}
		last = attemptOnce(wctx, cfg, s3p, sts)
		if last != nil && !isDeadlineCut(last.Err) {
			lastSubstantive = last
		}

		if last == nil {
			// Rework 4 finding 3: a success observed AFTER the window (or a
			// SIGTERM) expired must not count — the check promised to answer
			// within the window. Check the parent ctx first (SIGTERM stays a
			// clean shutdown, never a failure), then the window deadline.
			if err := ctx.Err(); err != nil {
				return fmt.Errorf("startupcheck: aborted by context cancellation: %w", err)
			}
			if wctx.Err() != nil {
				// The last probe happened to succeed but the window was
				// already gone when it returned. Class choice documented:
				// class=network + net_kind=timeout — the failure IS that the
				// window timed out while the probe was in flight (a distinct
				// "late" class was rejected to keep the enum exactly the four
				// whitelisted values); the config hint tells the operator to
				// raise the window or fix the slow storage.
				late := &Failure{
					Item:   itemS3,
					Err:    wctx.Err(),
					Diag:   Diagnostic{Class: ClassNetwork, NetKind: "timeout"},
					Window: cfg.Timeout, Attempts: attempt,
				}
				cfg.Logger.Error("storage startup self-check FAILED: window expired while the last probe was in flight",
					late.LogFields(cfg.Endpoint, cfg.AccessKey,
						"the probe bucket / STS answered, but only after MINIO_STARTUP_CHECK_TIMEOUT had expired: raise the window or investigate why storage answers that slowly")...)
				return late
			}
			cfg.Logger.Info("storage startup self-check passed",
				zap.String("endpoint", cfg.Endpoint),
				zap.String("access_key_id", cfg.AccessKey),
				zap.String("role_arn", cfg.RoleARN),
				zap.Int("attempts", attempt),
			)
			return nil
		}

		if err := ctx.Err(); err != nil {
			// SIGTERM raced a failing attempt: still a clean shutdown.
			return fmt.Errorf("startupcheck: aborted by context cancellation: %w", err)
		}
		if wctx.Err() != nil {
			// Window expired (or was cancelled together with the parent —
			// the parent case is handled above): terminal failure.
			return terminalFailure(cfg, last, lastSubstantive, attempt)
		}
		retryFields := append(whitelistFields(last, cfg.Endpoint, cfg.AccessKey,
			FixHint(cfg.RoleARN, cfg.ProbeBucket, last.Item, last.Diag)),
			zap.Int("attempt", attempt),
			zap.Duration("window_remaining", windowRemaining(wctx)),
			zap.String("hint", "if this is a fresh deployment where init-minio.sh / init-seaweedfs.sh has not run yet, this is expected and the check will pass once it does"),
		)
		cfg.Logger.Warn("storage startup self-check attempt failed; retrying", retryFields...)
		// Sleep at most the remaining window (finding 4): never past expiry.
		wait := delay
		if rem := windowRemaining(wctx); rem < wait {
			wait = rem
		}
		if wait > 0 {
			select {
			case <-ctx.Done():
				return fmt.Errorf("startupcheck: aborted by context cancellation: %w", ctx.Err())
			case <-wctx.Done():
				if err := ctx.Err(); err != nil {
					return fmt.Errorf("startupcheck: aborted by context cancellation: %w", err)
				}
				return terminalFailure(cfg, last, lastSubstantive, attempt)
			case <-time.After(wait):
			}
		}
		if delay < maxBackoff {
			delay *= 2
			if delay > maxBackoff {
				delay = maxBackoff
			}
		}
	}
}

// isDeadlineCut reports whether err is the WINDOW deadline expiring mid-probe
// (as opposed to a real answer from the wire / transport).
func isDeadlineCut(err error) bool {
	return errors.Is(err, context.DeadlineExceeded)
}

// terminalFailure fills in the window metadata, logs the terminal Error
// with the whitelisted diagnostics (no error text), and returns the *Failure.
// When the final attempt was merely the deadline cutting an in-flight probe,
// the SUBSTANTIVE last diagnostic is reported instead (attempts still counts
// every attempt) — see lastSubstantive in Run.
func terminalFailure(cfg Config, last, lastSubstantive *Failure, attempt int) *Failure {
	reported := last
	if isDeadlineCut(last.Err) && lastSubstantive != nil {
		reported = lastSubstantive
	}
	reported.Window = cfg.Timeout
	reported.Attempts = attempt
	cfg.Logger.Error("storage startup self-check FAILED: window expired, shutting down",
		whitelistFields(reported, cfg.Endpoint, cfg.AccessKey,
			FixHint(cfg.RoleARN, cfg.ProbeBucket, reported.Item, reported.Diag))...,
	)
	return reported
}

// whitelistFields builds the whitelisted log fields for a Failure. Single
// source shared by the retry Warn and the terminal Error so the two can
// never drift (see diagnostics.go for the field contract).
func whitelistFields(f *Failure, endpoint, accessKeyID, fixHint string) []zap.Field {
	return f.LogFields(endpoint, accessKeyID, fixHint)
}

// windowRemaining reports the time left before the window context expires
// (0 once expired).
func windowRemaining(wctx context.Context) time.Duration {
	deadline, ok := wctx.Deadline()
	if !ok {
		return 0
	}
	if rem := time.Until(deadline); rem > 0 {
		return rem
	}
	return 0
}

// attemptOnce runs both probes once and converts any failure into a
// *Failure; nil means both passed. Every Failure carries the whitelisted
// Diagnostic (diagnostics.go): the raw Err stays for programmatic use, but
// NO log output ever derives from error text (rework 4 finding 2).
func attemptOnce(ctx context.Context, cfg Config, s3p S3Prober, sts STSAssumer) *Failure {
	if err := s3p.ListMultipartUploads(ctx, cfg.ProbeBucket); err != nil {
		return &Failure{Item: itemS3, Err: err, Diag: diagnose(err)}
	}
	if err := sts.AssumeRole(ctx); err != nil {
		return &Failure{Item: itemSTS, Err: err, Diag: diagnose(err)}
	}
	return nil
}

// Failure describes the terminal state of the check after the window
// expired. (Context cancellation is NOT a Failure — Run returns a plain
// context error for that, rework 3 finding 1.)
type Failure struct {
	// Item is which probe failed last (itemS3 / itemSTS).
	Item string
	// Err is the raw last error. NEVER log it or any of its text — the
	// whitelisted Diag is the only sanctioned log representation.
	Err error
	// Diag is the whitelisted, log-safe diagnostic (diagnostics.go).
	Diag Diagnostic
	// Window is the configured total retry window.
	Window time.Duration
	// Attempts is how many attempts were made.
	Attempts int
}

// Error implements error using ONLY the whitelisted diagnostic parts — no
// error text (the string may be printed by wrappers/main; same rules as logs).
func (f *Failure) Error() string {
	parts := []string{
		fmt.Sprintf("item=%s", f.Item),
		fmt.Sprintf("class=%s", f.Diag.Class),
	}
	if f.Diag.S3Code != "" {
		parts = append(parts, "s3_code="+f.Diag.S3Code)
	}
	if f.Diag.STSCode != "" {
		parts = append(parts, "sts_code="+f.Diag.STSCode)
	}
	if f.Diag.HTTPStatus != 0 {
		parts = append(parts, fmt.Sprintf("http_status=%d", f.Diag.HTTPStatus))
	}
	if f.Diag.NetKind != "" {
		parts = append(parts, "net_kind="+f.Diag.NetKind)
	}
	return fmt.Sprintf("storage startup self-check failed after %d attempts over %s: %s",
		f.Attempts, f.Window, strings.Join(parts, " "))
}

// LogFields returns the structured fields for the terminal diagnostics log.
// The whitelist (rework 4 finding 2): item / class / endpoint /
// access_key_id / s3_code / sts_code / http_status / net_kind / attempts /
// window / fix_hint — endpoint, access_key_id and the hint come from OUR
// config, everything else from the Diagnostic. No free error text.
func (f *Failure) LogFields(endpoint, accessKeyID, fixHint string) []zap.Field {
	fields := []zap.Field{
		zap.String("item", f.Item),
		zap.String("class", f.Diag.Class.String()),
		zap.String("endpoint", endpoint),
		zap.String("access_key_id", accessKeyID),
	}
	if f.Diag.S3Code != "" {
		fields = append(fields, zap.String("s3_code", f.Diag.S3Code))
	}
	if f.Diag.STSCode != "" {
		fields = append(fields, zap.String("sts_code", f.Diag.STSCode))
	}
	if f.Diag.HTTPStatus != 0 {
		fields = append(fields, zap.Int("http_status", f.Diag.HTTPStatus))
	}
	if f.Diag.NetKind != "" {
		fields = append(fields, zap.String("net_kind", f.Diag.NetKind))
	}
	fields = append(fields,
		zap.Int("attempts", f.Attempts),
		zap.Duration("window", f.Window),
		zap.String("fix_hint", fixHint),
	)
	return fields
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
