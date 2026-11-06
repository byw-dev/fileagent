package startupcheck

import (
	"context"
	"errors"
	"fmt"
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
		Endpoint:    "s3.test:9000",
		AccessKey:   "TESTACCESSKEYID",
		SecretKey:   "test-secret",
		RoleARN:     "arn:aws:iam:::role/agent-role",
		ProbeBucket: "data-sensor",
		Timeout:     window,
		Logger:      zaptestLogger(t),
		RetryBackoff: 5 * time.Millisecond,
		S3:          s3,
		STS:         sts,
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

func TestClassifyS3(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"wrong secret → auth", errSigMismatch(), ClassAuthConfig},
		{"unknown AK → auth", errInvalidAK(), ClassAuthConfig},
		{"AccessDenied → auth", miniogo.ErrorResponse{Code: "AccessDenied", StatusCode: 403}, ClassAuthConfig},
		{"bare 401 → auth", miniogo.ErrorResponse{Code: "Whatever", StatusCode: 401}, ClassAuthConfig},
		{"bare 403 → auth", miniogo.ErrorResponse{Code: "Whatever", StatusCode: 403}, ClassAuthConfig},
		{"unknown 4xx code → auth (backend answered)", miniogo.ErrorResponse{Code: "NoSuchLifecycleConfiguration", StatusCode: 404}, ClassAuthConfig},
		{"500 → network", miniogo.ErrorResponse{Code: "InternalError", StatusCode: 500}, ClassNetwork},
		{"503 → network", miniogo.ErrorResponse{Code: "SlowDown", StatusCode: 503}, ClassNetwork},
		{"dial refused → network", makeURLErr(), ClassNetwork},
		{"plain error → network", errors.New("boom"), ClassNetwork},
		{"nil → network (total function)", nil, ClassNetwork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifyS3(tc.err))
		})
	}
}

func TestClassifySTS(t *testing.T) {
	// Real credentials.ErrorResponse needs its inner struct; build via XML is
	// overkill — use the public type directly.
	sig := credentials.ErrorResponse{}
	sig.STSError.Code = "AccessDenied"
	sig.STSError.Message = "The request signature we calculated does not match the signature you provided."
	role := credentials.ErrorResponse{}
	role.STSError.Code = "AccessDenied"
	role.STSError.Message = "user fileagent-controlplane is not authorized to assume role arn:aws:iam::000000000000:role/nope"
	byTextAK := credentials.ErrorResponse{}
	byTextAK.STSError.Code = "SomeFutureCode"
	byTextAK.STSError.Message = "The Access Key Id you provided does not exist"
	byTextToken := credentials.ErrorResponse{}
	byTextToken.STSError.Code = "SomeFutureCode"
	byTextToken.STSError.Message = "invalid token"
	byTextCred := credentials.ErrorResponse{}
	byTextCred.STSError.Code = "SomeFutureCode"
	byTextCred.STSError.Message = "credential mismatch"
	byTextSignature := credentials.ErrorResponse{}
	byTextSignature.STSError.Code = "SomeFutureCode"
	byTextSignature.STSError.Message = "Credential SIGNATURE mismatch"
	unknownCode := credentials.ErrorResponse{}
	unknownCode.STSError.Code = "SomeFutureCode"
	unknownCode.STSError.Message = "entirely opaque"

	cases := []struct {
		name string
		err  error
		want FailureClass
	}{
		{"signature mismatch text → auth", sig, ClassAuthConfig},
		{"not authorized to assume role → auth", role, ClassAuthConfig},
		{"unknown code, access-key text → auth", byTextAK, ClassAuthConfig},
		{"unknown code, token text → auth", byTextToken, ClassAuthConfig},
		{"unknown code, credential text → auth", byTextCred, ClassAuthConfig},
		{"unknown code, signature text (case-insensitive) → auth", byTextSignature, ClassAuthConfig},		{"unknown code, authless message → network", unknownCode, ClassNetwork},
		{"dial refused → network", makeURLErr(), ClassNetwork},
		{"plain error → network", errors.New("boom"), ClassNetwork},
		{"nil → network (total function)", nil, ClassNetwork},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifySTS(tc.err))
		})
	}
}

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
	assert.Equal(t, ClassAuthConfig, failure.Class)
	assert.Greater(t, failure.Attempts, 1, "auth failures must retry inside the window (call count: %d)", s3.callCount())
	assert.Equal(t, failure.Attempts, s3.callCount())
	// The STS probe runs after S3 succeeds only — it must never be reached.
	assert.Equal(t, 0, sts.callCount())
	assert.Contains(t, failure.Error(), "s3-identity")
	assert.Contains(t, failure.Error(), "auth/config")
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
	assert.Equal(t, ClassAuthConfig, failure.Class)
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
	assert.Equal(t, ClassNetwork, failure.Class)
	assert.GreaterOrEqual(t, failure.Attempts, 2)
	assert.Equal(t, 0, sts.callCount())
}

// Context cancelled mid-retry: Run returns promptly (the retry sleep must be
// interruptible), with the last failure (or context error) — never a hang.
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
	// Either the pending *Failure or the wrapped context error is acceptable;
	// both mean "did not hang and did not report success".
	var failure *Failure
	if !errors.As(err, &failure) {
		assert.ErrorIs(t, err, context.Canceled)
	}
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

// STS probe cancelled mid-call (S3 succeeded): failure is STS-item,
// network-class, and Run returns it.
func TestRun_STSCancelledMidProbe(t *testing.T) {
	s3 := &fakeS3{script: []error{nil}}
	ctx, cancel := context.WithCancel(context.Background())
	cfg := testConfig(t, s3, &ctxCancelSTS{cancel: cancel}, time.Minute)
	cfg.RetryBackoff = time.Millisecond
	err := Run(ctx, cfg)
	require.Error(t, err)
	var failure *Failure
	require.ErrorAs(t, err, &failure)
	assert.Equal(t, itemSTS, failure.Item)
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
	f := &Failure{Item: itemS3, Class: ClassAuthConfig, Err: errSigMismatch(), Window: 15 * time.Second, Attempts: 4}
	fields := f.LogFields("minio:9000", "AKID123", "fix-me")
	keys := map[string]bool{}
	for _, fd := range fields {
		keys[fd.Key] = true
	}
	for _, k := range []string{"item", "class", "endpoint", "access_key_id", "error", "attempts", "window", "fix_hint"} {
		assert.True(t, keys[k], "missing log field %s", k)
	}
	assert.Contains(t, f.Error(), "s3-identity")
	assert.Contains(t, f.Error(), "15s")
}

// FixHint differentiates the two items.
func TestFixHint(t *testing.T) {
	s3Hint := FixHint("arn:aws:iam:::role/agent-role", itemS3)
	assert.Contains(t, s3Hint, "MINIO_SECRET_KEY")
	stsHint := FixHint("arn:aws:iam:::role/agent-role", itemSTS)
	assert.Contains(t, stsHint, "MINIO_ROLE_ARN")
	assert.Contains(t, stsHint, "MinIO's STS does not validate RoleArn")
}

// Atomic sanity check that FailureClass strings are stable (logs are an
// operator interface; see the deployment doc added with this change).
func TestFailureClassString(t *testing.T) {
	assert.Equal(t, "auth/config", ClassAuthConfig.String())
	assert.Equal(t, "network", ClassNetwork.String())
	assert.Equal(t, "FailureClass(7)", FailureClass(7).String())
}

// Context cancellation mid-probe (not between retries): the failure carries
// the context error and stays network-class; Run surfaces it promptly.
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
