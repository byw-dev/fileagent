package startupcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"sync"
	"testing"
	"time"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// ─── fakes ───────────────────────────────────────────────────────────────────

// fakeS3 is a scriptable S3Prober: it replays results (error or nil) per call
// and counts calls so tests can prove retry semantics with call counts.
type fakeS3 struct {
	mu     sync.Mutex
	calls  int
	script []error // indexed by call-1; last entry repeats when exhausted
}

func (f *fakeS3) ListMultipartUploads(ctx context.Context, bucket string) error {
	f.mu.Lock()
	f.calls++
	i := f.calls - 1
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	err := f.script[i]
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return nil
}

func (f *fakeS3) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// fakeSTS mirrors fakeS3 for the STS probe.
type fakeSTS struct {
	mu     sync.Mutex
	calls  int
	script []error
}

func (f *fakeSTS) AssumeRole(ctx context.Context) error {
	f.mu.Lock()
	f.calls++
	i := f.calls - 1
	if i >= len(f.script) {
		i = len(f.script) - 1
	}
	err := f.script[i]
	f.mu.Unlock()
	return err
}

func (f *fakeSTS) callCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

// testConfig builds a minimal valid Config wired to the fakes and a fast
// backoff so window semantics are testable in milliseconds.
func testConfig(t *testing.T, s3 S3Prober, sts STSAssumer, window time.Duration) Config {
	t.Helper()
	return Config{
		Endpoint:     "s3.test:9000",
		AccessKey:    "TESTACCESSKEYID",
		SecretKey:    "test-secret",
		RoleARN:      "arn:aws:iam:::role/agent-role",
		ProbeBucket:  "data-sensor",
		Timeout:      window,
		Logger:       zaptestLogger(t),
		RetryBackoff: 5 * time.Millisecond,
		S3:           s3,
		STS:          sts,
	}
}

func zaptestLogger(t *testing.T) *zap.Logger {
	t.Helper()
	core, _ := observer.New(zap.WarnLevel)
	return zap.New(core)
}

// observedLogger returns a logger plus its observer for log-content asserts.
func observedLogger(t *testing.T) (*zap.Logger, *observer.ObservedLogs) {
	t.Helper()
	core, obs := observer.New(zap.DebugLevel)
	return zap.New(core), obs
}

// errSigMismatch is a canned S3 ErrorResponse like the backends return for a
// wrong secret (live-verified: MinIO + SeaweedFS, HTTP 403).
func errSigMismatch() error {
	return miniogo.ErrorResponse{
		Code:       "SignatureDoesNotMatch",
		Message:    "The request signature we calculated does not match the signature you provided.",
		StatusCode: 403,
	}
}

// errInvalidAK is the unknown-access-key shape (403 InvalidAccessKeyId).
func errInvalidAK() error {
	return miniogo.ErrorResponse{
		Code:       "InvalidAccessKeyId",
		Message:    "The Access Key Id you provided does not exist in our records.",
		StatusCode: 403,
	}
}

// errSTSAccessDenied is the STS ErrorResponse shape minio-go returns
// (credentials.ErrorResponse, not miniogo.ErrorResponse). MinIO wraps a
// signature failure in AccessDenied; SeaweedFS answers role failures with it.
func errSTSAccessDenied(msg string) error {
	return credentials.ErrorResponse{}
}

// makeURLErr fabricates the transport failure shape (dial refused), matching
// what a black-holed or absent endpoint surfaces as.
func makeURLErr() error {
	return &url.Error{Op: "Get", URL: "http://s3.test:9000/data-sensor", Err: errors.New("dial tcp 10.0.0.9:9000: connect: connection refused")}
}

// ─── classification ──────────────────────────────────────────────────────────

// diagnose() replaces the old ClassifyS3/ClassifySTS pair (rework 4): one
// function, whitelisted fields. Table-driven over the shapes that matter.
func TestDiagnose_S3Shapes(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		class    FailureClass
		s3Code   string
		httpStat int
	}{
		{"wrong secret", errSigMismatch(), ClassAuth, "SignatureDoesNotMatch", 403},
		{"unknown access key", errInvalidAK(), ClassAuth, "InvalidAccessKeyId", 403},
		{"missing probe bucket", miniogo.ErrorResponse{Code: "NoSuchBucket", StatusCode: 404, RequestID: "r1"}, ClassConfig, "NoSuchBucket", 404},
		{"access denied", miniogo.ErrorResponse{Code: "AccessDenied", StatusCode: 403}, ClassConfig, "AccessDenied", 403},
		{"unknown 4xx", miniogo.ErrorResponse{Code: "NoSuchLifecycleConfiguration", StatusCode: 404}, ClassConfig, "NoSuchLifecycleConfiguration", 404},
		{"500 InternalError", miniogo.ErrorResponse{Code: "InternalError", StatusCode: 500}, ClassNetwork, "InternalError", 500},
		{"503 SlowDown", miniogo.ErrorResponse{Code: "SlowDown", StatusCode: 503}, ClassNetwork, "SlowDown", 503},
		{"malicious non-standard code", miniogo.ErrorResponse{Code: "AccessDenied Authorization: Bearer tok", StatusCode: 403}, ClassConfig, "<non-standard>", 403},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := diagnose(tc.err)
			assert.Equal(t, tc.class, d.Class)
			assert.Equal(t, tc.s3Code, d.S3Code)
			assert.Equal(t, tc.httpStat, d.HTTPStatus)
		})
	}
}

func TestDiagnose_STSShapes(t *testing.T) {
	roleDenied := credentials.ErrorResponse{}
	roleDenied.STSError.Code = "AccessDenied"
	roleDenied.STSError.Message = "user fileagent-controlplane is not authorized to assume role arn:aws:iam::000000000000:role/nope"
	sigWrap := credentials.ErrorResponse{}
	sigWrap.STSError.Code = "AccessDenied"
	sigWrap.STSError.Message = "The request signature we calculated does not match."
	plainSig := credentials.ErrorResponse{}
	plainSig.STSError.Code = "SignatureDoesNotMatch"
	nonStandard := credentials.ErrorResponse{}
	nonStandard.STSError.Code = "AccessDenied Authorization: Bearer transient-session-token"

	cases := []struct {
		name    string
		err     error
		class   FailureClass
		stsCode string
	}{
		{"MinIO signature wrap (AccessDenied+message)", sigWrap, ClassAuth, "AccessDenied"},
		{"plain SignatureDoesNotMatch", plainSig, ClassAuth, "SignatureDoesNotMatch"},
		{"role not assumable (SeaweedFS)", roleDenied, ClassConfig, "AccessDenied"},
		{"malicious non-standard code", nonStandard, ClassConfig, "<non-standard>"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := diagnose(tc.err)
			assert.Equal(t, tc.class, d.Class)
			assert.Equal(t, tc.stsCode, d.STSCode)
		})
	}
}

func TestDiagnose_TransportShapes(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	timedOut := &net.OpError{Op: "dial", Net: "tcp", Err: timeoutErr{}}
	dnsErr := &net.DNSError{Err: "no such host", Name: "s3.test", IsNotFound: true}
	tlsErr := &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{}}
	wrappedRefused := &url.Error{Op: "Get", URL: "http://s3.test:9000/b", Err: refused}

	cases := []struct {
		name    string
		err     error
		class   FailureClass
		netKind string
	}{
		{"dial refused", refused, ClassNetwork, "dial_refused"},
		{"dial timeout", timedOut, ClassNetwork, "timeout"},
		{"dns", dnsErr, ClassNetwork, "dns"},
		{"tls", tlsErr, ClassNetwork, "tls"},
		{"url.Error wrapping dial refused", wrappedRefused, ClassNetwork, "dial_refused"},
		{"context deadline", context.DeadlineExceeded, ClassNetwork, "timeout"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := diagnose(tc.err)
			assert.Equal(t, tc.class, d.Class)
			assert.Equal(t, tc.netKind, d.NetKind)
		})
	}
}

// Unknown shapes collapse to class=unknown with NO other fields — the whole
// point of the whitelist (round 2 proved any text can leak).
func TestDiagnose_UnknownShapeSaysNothing(t *testing.T) {
	d := diagnose(errors.New("gateway error: Authorization: Bearer transient-session-token status=403"))
	assert.Equal(t, ClassUnknown, d.Class)
	assert.Empty(t, d.S3Code)
	assert.Empty(t, d.STSCode)
	assert.Zero(t, d.HTTPStatus)
	assert.Empty(t, d.NetKind)
}

// timeoutErr satisfies net.Error with Timeout()=true for the transport table.
type timeoutErr struct{}

func (timeoutErr) Error() string   { return "i/o timeout" }
func (timeoutErr) Timeout() bool   { return true }
func (timeoutErr) Temporary() bool { return true }

// ─── Run semantics ────────────────────────────────────────────────────────────

// Happy path: both probes pass on the first attempt, Run returns nil, exactly
// one call per probe.
func TestRun_AllPass(t *testing.T) {
	s3 := &fakeS3{script: []error{nil}}
	sts := &fakeSTS{script: []error{nil}}
	err := Run(context.Background(), testConfig(t, s3, sts, time.Second))
	require.NoError(t, err)
	assert.Equal(t, 1, s3.callCount())
	assert.Equal(t, 1, sts.callCount())
}

// S3 auth failure (SignatureDoesNotMatch): retried inside the window (call
// count > 1 proves it), then fails with a *Failure when the window expires.
// Per the product decision (2026-11-06, refined for IC-BUG-40) auth errors
// also retry — fresh deployments create the CP identity only after the CP is
// up — but the process still refuses to run once the window is gone.
func TestRun_S3AuthFailure_RetriesThenFailsAtWindowEnd(t *testing.T) {
	s3 := &fakeS3{script: []error{errSigMismatch()}}
	sts := &fakeSTS{script: []error{nil}}
	err := Run(context.Background(), testConfig(t, s3, sts, 30*time.Millisecond))
	require.Error(t, err)
	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, itemS3, failure.Item)
	assert.Equal(t, ClassAuth, failure.Diag.Class)
	assert.Greater(t, failure.Attempts, 1, "auth failures must retry inside the window (call count: %d)", s3.callCount())
	assert.Equal(t, failure.Attempts, s3.callCount())
	// The STS probe runs after S3 succeeds only — it must never be reached.
	assert.Equal(t, 0, sts.callCount())
	assert.Contains(t, failure.Error(), "s3-identity")
	assert.Contains(t, failure.Error(), "class=auth")
	assert.Contains(t, failure.Error(), "s3_code=SignatureDoesNotMatch")
}

// STS failure with correct S3 credentials: fails (retried) at window end.
func TestRun_STSFailure(t *testing.T) {
	role := credentials.ErrorResponse{}
	role.STSError.Code = "AccessDenied"
	role.STSError.Message = "user fileagent-controlplane is not authorized to assume role arn:aws:iam::000000000000:role/nope"
	s3 := &fakeS3{script: []error{nil}}
	sts := &fakeSTS{script: []error{role}}
	err := Run(context.Background(), testConfig(t, s3, sts, 30*time.Millisecond))
	require.Error(t, err)
	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, itemSTS, failure.Item)
	assert.Equal(t, ClassConfig, failure.Diag.Class)
	assert.Greater(t, failure.Attempts, 1)
	assert.Equal(t, failure.Attempts, s3.callCount(), "S3 probe succeeds every round")
}

// Network error first, success later: retries carry the check through.
func TestRun_NetworkError_RetriedThenSucceeds(t *testing.T) {
	s3 := &fakeS3{script: []error{makeURLErr(), makeURLErr(), nil}}
	sts := &fakeSTS{script: []error{nil}}
	err := Run(context.Background(), testConfig(t, s3, sts, 5*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 3, s3.callCount())
	assert.Equal(t, 1, sts.callCount())
}

// Network error for the whole window: fails with class network at window end.
func TestRun_NetworkError_WindowExhausted(t *testing.T) {
	s3 := &fakeS3{script: []error{makeURLErr()}}
	sts := &fakeSTS{script: []error{nil}}
	err := Run(context.Background(), testConfig(t, s3, sts, 40*time.Millisecond))
	require.Error(t, err)
	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, ClassNetwork, failure.Diag.Class)
	assert.GreaterOrEqual(t, failure.Attempts, 2)
	assert.Equal(t, 0, sts.callCount())
}

// Context cancelled mid-retry: Run returns promptly (the retry sleep must be
// interruptible) with the CONTEXT error — NOT a *Failure (rework 3 finding
// 1: cancellation is a clean shutdown, never a terminal startup failure, or
// a SIGTERM during a failing check would exit non-zero).
func TestRun_ContextCancelled(t *testing.T) {
	s3 := &fakeS3{script: []error{makeURLErr()}}
	sts := &fakeSTS{script: []error{nil}}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := testConfig(t, s3, sts, time.Minute)
	cfg.RetryBackoff = 20 * time.Millisecond

	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	start := time.Now()
	err := Run(ctx, cfg)
	elapsed := time.Since(start)
	require.Error(t, err)
	assert.Less(t, elapsed, 5*time.Second, "Run must return promptly after cancellation")
	assert.ErrorIs(t, err, context.Canceled)
	var failure *Failure
	assert.False(t, errors.As(err, &failure), "cancellation must NOT surface as *Failure (would turn SIGTERM into a non-zero exit)")
}

// Context already cancelled before the first attempt: no probe is dialed.
func TestRun_ContextCancelledBeforeStart(t *testing.T) {
	s3 := &fakeS3{script: []error{nil}}
	sts := &fakeSTS{script: []error{nil}}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Run(ctx, testConfig(t, s3, sts, time.Second))
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	assert.Equal(t, 0, s3.callCount())
	assert.Equal(t, 0, sts.callCount())
}

// Success after an AUTH failure inside the window (fresh deployment where
// init creates the identity late): the check must recover, not fail fast.
func TestRun_AuthFailureRecoversWithinWindow(t *testing.T) {
	s3 := &fakeS3{script: []error{errInvalidAK(), errSigMismatch(), nil}}
	sts := &fakeSTS{script: []error{nil}}
	err := Run(context.Background(), testConfig(t, s3, sts, 5*time.Second))
	require.NoError(t, err)
	assert.Equal(t, 3, s3.callCount())
}

// The secret must never appear in any log line; the access key id must.
func TestRun_LogsNeverContainSecret(t *testing.T) {
	const secret = "SUPER-SECRET-VALUE-42"
	s3 := &fakeS3{script: []error{errSigMismatch()}}
	sts := &fakeSTS{script: []error{nil}}
	logger, obs := observedLogger(t)
	cfg := testConfig(t, s3, sts, 20*time.Millisecond)
	cfg.SecretKey = secret
	cfg.Logger = logger

	_ = Run(context.Background(), cfg)
	fieldVal := func(fs []zap.Field, key string) (string, bool) {
		for _, f := range fs {
			if f.Key == key {
				if f.Interface != nil {
					return fmt.Sprint(f.Interface), true
				}
				return f.String, true
			}
		}
		return "", false
	}
	for _, entry := range obs.All() {
		assert.NotContains(t, entry.Message, secret)
		for _, f := range entry.Context {
			if v, ok := fieldVal([]zap.Field{f}, f.Key); ok {
				assert.NotContains(t, v, secret)
			}
		}
	}
	// Access key id IS logged (id only, by design).
	found := false
	for _, entry := range obs.All() {
		if _, ok := fieldVal(entry.Context, "access_key_id"); ok {
			found = true
		}
	}
	assert.True(t, found, "access key id must be logged")
	// The terminal Error names the item and a fix hint.
	term := obs.FilterMessage("storage startup self-check FAILED: window expired, shutting down")
	assert.Equal(t, 1, term.Len())
}

// Retry warns during the window, including the fresh-deployment hint.
func TestRun_RetryWarnCarriesHint(t *testing.T) {
	s3 := &fakeS3{script: []error{makeURLErr()}}
	sts := &fakeSTS{script: []error{nil}}
	logger, obs := observedLogger(t)
	cfg := testConfig(t, s3, sts, 20*time.Millisecond)
	cfg.Logger = logger

	_ = Run(context.Background(), cfg)
	warns := obs.FilterMessage("storage startup self-check attempt failed; retrying")
	assert.GreaterOrEqual(t, warns.Len(), 1)
	entry := warns.All()[0]
	var hint string
	for _, f := range entry.Context {
		if f.Key == "hint" {
			hint = f.String
		}
	}
	assert.Contains(t, hint, "fresh deployment")
}

// Guard the guard: an unsatisfiable config (no logger / non-positive window)
// is an immediate error, not a panic or silent pass.
func TestRun_InvalidConfig(t *testing.T) {
	err := Run(context.Background(), Config{Timeout: time.Second})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Logger")

	err = Run(context.Background(), Config{Logger: zaptestLogger(t), Timeout: 0})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Timeout")

	// Unparseable endpoint when no S3 override is injected: construction
	// failure surfaces as a startupcheck error, not a panic.
	err = Run(context.Background(), Config{
		Logger: zaptestLogger(t), Timeout: time.Second,
		Endpoint: "http://bad endpoint:9000",
		STS:      &fakeSTS{script: []error{nil}},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cannot construct S3 client")

	// Nil RetryBackoff falls back to the default without panic (exercised
	// with an immediate success so the test stays fast).
	cfg := testConfig(t, &fakeS3{script: []error{nil}}, &fakeSTS{script: []error{nil}}, time.Second)
	cfg.RetryBackoff = 0
	require.NoError(t, Run(context.Background(), cfg))
}

// STS probe cancelled mid-call (S3 succeeded): cancellation surfaces as the
// context error, NOT a *Failure (rework 3 finding 1).
func TestRun_STSCancelledMidProbe(t *testing.T) {
	s3 := &fakeS3{script: []error{nil}}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := testConfig(t, s3, &ctxCancelSTS{cancel: cancel}, time.Minute)
	cfg.RetryBackoff = time.Millisecond
	err := Run(ctx, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	var failure *Failure
	assert.False(t, errors.As(err, &failure))
}

// ctxCancelSTS returns context.Canceled from the probe (shape the real
// client surfaces when shutdown outranks the check).
type ctxCancelSTS struct {
	cancel context.CancelFunc
}

func (c *ctxCancelSTS) AssumeRole(ctx context.Context) error {
	err := context.Canceled
	c.cancel()
	return err
}

// Failure.LogFields carries exactly the IC-BUG-40 required diagnostics.
func TestFailure_LogFields(t *testing.T) {
	f := &Failure{Item: itemS3, Err: errSigMismatch(), Diag: diagnose(errSigMismatch()), Window: 15 * time.Second, Attempts: 4}
	fields := f.LogFields("minio:9000", "AKID123", "fix-me")
	keys := map[string]bool{}
	for _, fd := range fields {
		keys[fd.Key] = true
	}
	for _, k := range []string{"item", "class", "endpoint", "access_key_id", "s3_code", "http_status", "attempts", "window", "fix_hint"} {
		assert.True(t, keys[k], "missing log field %s", k)
	}
	// The free-text "error" field is GONE by design (whitelist, rework 4).
	assert.NotContains(t, keys, "error")
	assert.Contains(t, f.Error(), "s3-identity")
	assert.Contains(t, f.Error(), "15s")
}

// FixHint differentiates the items and the NoSuchBucket case (rework 4
// finding 1: a missing probe bucket gets the init-script runbook).
func TestFixHint(t *testing.T) {
	const role = "arn:aws:iam:::role/agent-role"
	s3Hint := FixHint(role, "data-sensor", itemS3, Diagnostic{})
	assert.Contains(t, s3Hint, "MINIO_SECRET_KEY")
	stsHint := FixHint(role, "data-sensor", itemSTS, Diagnostic{})
	assert.Contains(t, stsHint, "MINIO_ROLE_ARN")
	assert.Contains(t, stsHint, "MinIO's STS does not validate RoleArn")
	bucketHint := FixHint(role, "data-sensor", itemS3, Diagnostic{S3Code: "NoSuchBucket"})
	assert.Contains(t, bucketHint, "MINIO_STARTUP_CHECK_BUCKET")
	assert.Contains(t, bucketHint, "init")
}

// Atomic sanity check that FailureClass strings are stable (logs are an
// operator interface; see the deployment doc added with this change).
func TestFailureClassString(t *testing.T) {
	assert.Equal(t, "auth", ClassAuth.String())
	assert.Equal(t, "config", ClassConfig.String())
	assert.Equal(t, "network", ClassNetwork.String())
	assert.Equal(t, "unknown", ClassUnknown.String())
	assert.Equal(t, "FailureClass(9)", FailureClass(9).String())
}

// Context cancellation mid-probe (not between retries): Run surfaces the
// context error promptly — and never a *Failure.
func TestRun_ContextCancelledDuringProbe(t *testing.T) {
	s3 := &ctxCancelS3{}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := testConfig(t, s3, &fakeSTS{script: []error{nil}}, time.Minute)
	cfg.RetryBackoff = time.Millisecond
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()
	err := Run(ctx, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	var failure *Failure
	assert.False(t, errors.As(err, &failure))
}

// ctxCancelS3 blocks inside the probe until the context is cancelled,
// surfacing context.Canceled like the real client does.
type ctxCancelS3 struct{}

func (c *ctxCancelS3) ListMultipartUploads(ctx context.Context, bucket string) error {
	<-ctx.Done()
	return ctx.Err()
}

// unused helper silences the linter on errSTSAccessDenied when a test build
// excludes it (kept for the shape documentation it carries).
var _ = errSTSAccessDenied

// ─── Rework 3: review findings ───────────────────────────────────────────────

// Finding 1: a SIGTERM after several FAILING attempts must surface as the
// context error, never as *Failure (which main wires into the non-zero exit).
// Mirrors the review probe TestReviewCancelledWithPriorFailure — under the
// FIXED contract this must hold.
func TestRun_CancelledAfterPriorFailuresIsNotAFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s3 := &reviewS3{fn: func() error {
		cancel() // SIGTERM lands right as an attempt fails
		return errors.New("dial unavailable")
	}}
	cfg := testConfig(t, s3, &fakeSTS{script: []error{nil}}, time.Minute)
	got := Run(ctx, cfg)
	require.Error(t, got)
	assert.ErrorIs(t, got, context.Canceled)
	var failure *Failure
	assert.False(t, errors.As(got, &failure), "cancelled Run must not return *Failure — main would exit non-zero on a clean SIGTERM")
}

type reviewS3 struct{ fn func() error }

func (r *reviewS3) ListMultipartUploads(_ context.Context, _ string) error { return r.fn() }

// Finding 4: the retry window must bound the SLEEPS too — a 1ms window with
// a 100ms backoff must return in far less than the backoff (the old code
// slept the full delay regardless of remaining window; review probe measured
// 101ms). Upper bound generous enough to stay non-flaky.
func TestRun_WindowBoundsTheSleep(t *testing.T) {
	s3 := &reviewS3{fn: func() error { return errors.New("storage down") }}
	cfg := testConfig(t, s3, &fakeSTS{script: []error{nil}}, time.Millisecond)
	cfg.RetryBackoff = 100 * time.Millisecond
	started := time.Now()
	_ = Run(context.Background(), cfg)
	elapsed := time.Since(started)
	assert.Less(t, elapsed, 60*time.Millisecond, "1ms window with 100ms backoff took %s — sleep is not bounded by the window", elapsed)
}

// Finding 4 (STS side): the STS HTTP timeout shrinks to the remaining window
// (exercised through the exported adapter against a hanging server: with a
// tiny window the call must return before the full stsProbeTimeout).
func TestAssumeRoleSTS_TimeoutBoundedByWindowCtx(t *testing.T) {
	// A server that never answers; the deadline-bound STS call must give up
	// when the window (50ms) expires, not after the 10s probe timeout.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	started := time.Now()
	err = AssumeRoleSTS{Config: probeCfg(ln.Addr().String())}.AssumeRole(ctx)
	elapsed := time.Since(started)
	require.Error(t, err)
	assert.Less(t, elapsed, 5*time.Second, "STS call must be bounded by the window deadline (took %s)", elapsed)
}

// Finding 2 (rework 4): the whitelist contract. EVERY field the check ever
// logs must come from the allow-list in diagnostics.go; the five malicious
// scenarios from review round 2 (untrusted S3 code, untrusted STS code,
// Authorization headers, unknown query params) plus the configured secret
// itself must appear in NO log line or field.
func TestRun_LogsAreWhitelistOnly(t *testing.T) {
	const secret = "configured-secret"
	const bearer = "transient-session-token"

	malicious := fmt.Sprintf(`gateway error: Authorization: Bearer %s Get https://user:%s@storage.test/x?X-Amz-Signature=%s&session_token=%s: status 403`,
		bearer, secret, secret, bearer)
	s3 := &reviewS3{fn: func() error { return errors.New(malicious) }}
	logger, obs := observedLogger(t)
	cfg := testConfig(t, s3, &fakeSTS{script: []error{nil}}, 20*time.Millisecond)
	cfg.Logger = logger
	cfg.SecretKey = secret
	cfg.RetryBackoff = time.Millisecond

	err := Run(context.Background(), cfg)
	var failure *Failure
	require.ErrorAs(t, err, &failure)

	allowed := map[string]bool{
		"item": true, "class": true, "endpoint": true, "access_key_id": true,
		"s3_code": true, "sts_code": true, "http_status": true, "net_kind": true,
		"attempts": true, "window": true, "fix_hint": true, "attempt": true,
		"window_remaining": true, "hint": true, "role_arn": true,
	}
	for _, entry := range obs.All() {
		assert.NotContains(t, entry.Message, secret)
		assert.NotContains(t, entry.Message, bearer)
		for _, f := range entry.Context {
			assert.True(t, allowed[f.Key], "log field %q is outside the whitelist", f.Key)
			v := f.String
			if f.Interface != nil {
				v = fmt.Sprint(f.Interface)
			}
			assert.NotContains(t, v, secret, "field %q leaks the configured secret", f.Key)
			assert.NotContains(t, v, bearer, "field %q leaks a third-party token", f.Key)
		}
	}
	// The returned error's string form obeys the same rules.
	assert.NotContains(t, err.Error(), secret)
	assert.NotContains(t, err.Error(), bearer)
}

// Port of review r2's TestReviewSanitizerUntrustedCodeAndHeader: five
// server-controlled shapes must yield diagnostics that carry none of the
// smuggled values — asserted on the DIAGNOSTIC (the only thing that reaches
// logs).
func TestDiagnose_NeverCarriesUntrustedValues(t *testing.T) {
	const tok = "transient-session-token"
	const b64 = "base64-session-secret"
	cases := []struct {
		name string
		err  error
	}{
		{"s3 code with bearer", miniogo.ErrorResponse{Code: "AccessDenied Authorization: Bearer " + tok, StatusCode: 403}},
		{"sts code with bearer", func() error {
			sts := credentials.ErrorResponse{}
			sts.STSError.Code = "AccessDenied Authorization: Bearer " + tok
			return sts
		}()},
		{"authorization bearer", errors.New("gateway error: Authorization: Bearer " + tok + " status=403")},
		{"authorization basic", errors.New("gateway error: Authorization: Basic " + b64 + " status=403")},
		{"unknown query param", errors.New("Get https://store.test/bucket?session_token=" + tok + "&uploads=: 404")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := diagnose(tc.err)
			for _, v := range []string{d.S3Code, d.STSCode, d.NetKind, d.Class.String()} {
				assert.NotContains(t, v, tok)
				assert.NotContains(t, v, b64)
			}
			// Code fields are either empty, whitelist-patterned, or the
			// literal marker — never raw wire text.
			for _, code := range []string{d.S3Code, d.STSCode} {
				if code != "" && code != "<non-standard>" {
					assert.Regexp(t, `^[A-Za-z][A-Za-z0-9.]{0,63}$`, code)
				}
			}
		})
	}
}

// ─── Rework 4 finding 3: a success observed after window expiry ─────────────

// The review r2 probe TestReviewLateProbeSuccessMustNotPassExpiredWindow,
// asserted the FIXED way: the probe "succeeds" only after the 1ms window has
// expired — that success must NOT count; Run returns a *Failure with a
// timeout-classed diagnostic.
func TestRun_LateProbeSuccessAfterWindowExpiryFails(t *testing.T) {
	s3 := &reviewS3{fn: func() error {
		time.Sleep(20 * time.Millisecond) // window (1ms) expires mid-probe
		return nil                        // "success" — but too late
	}}
	cfg := testConfig(t, s3, &fakeSTS{script: []error{nil}}, time.Millisecond)
	cfg.RetryBackoff = time.Millisecond
	err := Run(context.Background(), cfg)
	require.Error(t, err)
	var f *Failure
	require.ErrorAs(t, err, &f, "window expired before the probe returned; Run must not report success")
	assert.Equal(t, ClassNetwork, f.Diag.Class)
	assert.Equal(t, "timeout", f.Diag.NetKind)
	assert.Equal(t, 1, f.Attempts)
}

// Same boundary, SIGTERM flavor: parent cancelled while the (late) probe was
// in flight → cancellation wins, clean shutdown (never a Failure).
func TestRun_LateProbeSuccessAfterParentCancelIsClean(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	s3 := &reviewS3{fn: func() error {
		cancel() // SIGTERM lands mid-probe
		time.Sleep(20 * time.Millisecond)
		return nil
	}}
	cfg := testConfig(t, s3, &fakeSTS{script: []error{nil}}, time.Minute)
	cfg.RetryBackoff = time.Millisecond
	err := Run(ctx, cfg)
	require.Error(t, err)
	assert.ErrorIs(t, err, context.Canceled)
	var f *Failure
	assert.False(t, errors.As(err, &f))
}
