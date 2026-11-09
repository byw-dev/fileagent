package startupcheck

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strings"
	"syscall"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// diagnostics.go — the log-safety boundary for IC-BUG-40 (rework 4 finding 2).
//
// Round 2 of review proved that ANY free error text can leak: server-echoed
// credentials, Authorization headers, unknown query parameters — a blacklist
// scrubber only ever covers the shapes someone has already thought of. So the
// design inverted (coordinator decision, rework 4): logs carry ONLY a
// whitelist of structured fields, and NO text originating from the error ever
// enters a log line. The trade-off is deliberate and documented: we lose the
// raw error text (an operator cannot read the server's message from the CP
// log), but class + s3_code/sts_code + http_status + net_kind + endpoint +
// access_key_id is sufficient to locate the problem (wrong secret vs wrong
// role vs missing bucket vs unreachable endpoint), and for anything deeper
// the operator curls the endpoint directly with their own credentials. A
// debug-level raw-text channel was considered and rejected: proving it cannot
// leak is exactly what round 2 showed to be hard.

// Diagnostic is the whitelisted, log-safe description of one probe failure.
// Every field is either derived from OUR OWN classification of the error
// (class, net_kind) or a narrowly-validated scalar from the wire
// (s3_code/sts_code must be one of the knownS3Codes enum members, else they
// collapse to "<non-standard>"; http_status is an int).
type Diagnostic struct {
	// Class is our classification: auth / config / network / unknown.
	Class FailureClass
	// S3Code is the S3 error code, "" when absent, "<non-standard>" when the
	// server returned something outside the knownS3Codes fixed enum.
	S3Code string
	// STSCode is the STS error code, same rules as S3Code.
	STSCode string
	// HTTPStatus is the S3 response status code (0 = absent; minio-go's STS
	// path does not surface one).
	HTTPStatus int
	// NetKind is the transport failure kind: dial_refused / timeout / dns /
	// tls / other ("" when the failure is not transport-shaped). Derived from
	// errors.As on net error TYPES only — their text is never logged.
	NetKind string
}

// knownS3Codes is the FIXED ENUM of S3/STS error codes we are willing to
// print verbatim (rework 5 finding 2). A shape-based regex was not a value
// whitelist: any alphanumeric blob — including a server echoing a
// legitimate-SHAPED secret as the Code — would have passed it and reached
// the logs. Only these machine codes are printed; every other code collapses
// to "<non-standard>". The set covers everything the two supported backends
// actually return for the failure modes this check diagnoses (live-verified:
// SignatureDoesNotMatch / InvalidAccessKeyId / NoSuchBucket on S3;
// AccessDenied / SignatureDoesNotMatch / InvalidAccessKeyId on STS) plus the
// standard S3 machine codes the same family can produce. Adding a code here
// requires a live capture showing a supported backend returning it.
var knownS3Codes = map[string]struct{}{
	"AccessDenied":            {},
	"InvalidAccessKeyId":      {},
	"SignatureDoesNotMatch":   {},
	"NoSuchBucket":            {},
	"InvalidBucketName":       {},
	"ExpiredToken":            {},
	"InvalidClientTokenId":    {},
	"MalformedPolicyDocument": {},
	"InvalidParameterValue":   {},
	"InternalError":           {},
	"ServiceUnavailable":      {},
	"SlowDown":                {},
	"RequestTimeTooSkewed":    {},
	"NotImplemented":          {},
}

// FailureClass is our own classification of a probe failure. The four values
// are exactly the set the diagnostics whitelist allows.
type FailureClass int

const (
	// ClassAuth means the backend answered and rejected the identity:
	// SignatureDoesNotMatch, InvalidAccessKeyId, InvalidClientTokenId, or an
	// STS signature failure (MinIO wraps those as AccessDenied with a
	// signature-mismatch message — classified from the message, which is
	// never logged).
	ClassAuth FailureClass = iota
	// ClassConfig means the backend answered and the request itself is wrong
	// for this deployment: role not assumable (SeaweedFS), probe bucket
	// missing (NoSuchBucket — the init script has not run), or any other
	// non-5xx S3 answer we do not know by name.
	ClassConfig
	// ClassNetwork means no usable answer arrived: transport errors
	// (dial/timeout/dns/tls) or a 5xx (backend up but not ready).
	ClassNetwork
	// ClassUnknown means the error shape was none of the above (a plain
	// error with no S3/STS/net markers) — we cannot say more without
	// logging untrusted text, so we say nothing.
	ClassUnknown
)

// String implements fmt.Stringer for logs.
func (c FailureClass) String() string {
	switch c {
	case ClassAuth:
		return "auth"
	case ClassConfig:
		return "config"
	case ClassNetwork:
		return "network"
	case ClassUnknown:
		return "unknown"
	default:
		return fmt.Sprintf("FailureClass(%d)", int(c))
	}
}

// authS3Codes are S3 error codes that mean "the identity was rejected".
var authS3Codes = map[string]struct{}{
	"SignatureDoesNotMatch": {},
	"InvalidAccessKeyId":    {},
	"InvalidClientTokenId":  {},
	"ExpiredToken":          {},
	"Unauthorized":          {},
	"RequestTimeTooSkewed":  {},
}

// diagnose reduces an error from either probe to the whitelisted Diagnostic.
// It never inspects text for anything it might print, except the narrowly
// validated code fields; message text IS read for classification decisions
// (e.g. MinIO's signature-mismatch-in-AccessDenied wrapping) but is never
// included in the result.
func diagnose(err error) Diagnostic {
	if err == nil {
		return Diagnostic{}
	}
	// Context shapes first: the window/priority logic in Run depends on
	// distinguishing these; as diagnostics they are timeouts/cancellations.
	if errors.Is(err, context.DeadlineExceeded) {
		return Diagnostic{Class: ClassNetwork, NetKind: "timeout"}
	}
	if errors.Is(err, context.Canceled) {
		// Cancellation is never logged as a failure; keep a sane shape anyway.
		return Diagnostic{Class: ClassNetwork, NetKind: "other"}
	}
	// Our own sentinel for "200 but not this bucket's listing" (probe.go).
	var wrongProbe errWrongProbeResponse
	if errors.As(err, &wrongProbe) {
		// http_status=200 with no code is the wrong-route signature; a real
		// S3 error never carries 200 — FixHint keys off exactly that shape.
		return Diagnostic{Class: ClassConfig, HTTPStatus: 200}
	}
	// S3 ErrorResponse (parsed or synthesized by minio-go — we do not, and
	// cannot, tell which; the CODE itself is enum-checked before any output,
	// so a synthesized/gateway value cannot smuggle text).
	if resp := miniogo.ToErrorResponse(err); resp.Code != "" || resp.StatusCode != 0 {
		d := Diagnostic{HTTPStatus: resp.StatusCode, S3Code: safeCode(resp.Code)}
		switch {
		case resp.StatusCode >= 500:
			// Backend answered but is not ready: same handling as transport
			// (retry within the window), so it lands in network.
			d.Class = ClassNetwork
		default:
			if _, ok := authS3Codes[resp.Code]; ok {
				d.Class = ClassAuth
			} else {
				// AccessDenied, NoSuchBucket, InvalidParameterValue, unknown
				// 4xx: request/deployment-level rejections.
				d.Class = ClassConfig
			}
		}
		return d
	}
	// STS ErrorResponse (credentials.ErrorResponse — a different type; no
	// status code is carried).
	var stsResp credentials.ErrorResponse
	if errors.As(err, &stsResp) {
		d := Diagnostic{STSCode: safeCode(stsResp.STSError.Code)}
		switch {
		case isAuthSTSCode(stsResp.STSError.Code):
			d.Class = ClassAuth
		case containsAuthRejectionText(stsResp.STSError.Message):
			// MinIO wraps SigV4 failures as AccessDenied with a signature
			// message — classified from the message, never printed.
			d.Class = ClassAuth
		default:
			// Role-not-assumable (SeaweedFS "not authorized to assume
			// role") and every other STS answer: deployment-level.
			d.Class = ClassConfig
		}
		return d
	}
	// Transport shapes — classify by TYPE, log only the coarse net_kind.
	if d, ok := diagnoseTransport(err); ok {
		return d
	}
	// Unknown shape: no code, no status, no transport markers. Say nothing
	// more than "unknown" — printing any part of err.Error() is what round 2
	// proved unsafe.
	return Diagnostic{Class: ClassUnknown}
}

// safeCode returns code when it is one of the known machine codes, else the
// literal "<non-standard>" marker.
func safeCode(code string) string {
	if _, ok := knownS3Codes[code]; ok {
		return code
	}
	return "<non-standard>"
}

// isAuthSTSCode reports whether an STS error code is an identity rejection.
func isAuthSTSCode(code string) bool {
	switch code {
	case "SignatureDoesNotMatch", "InvalidAccessKeyId", "InvalidClientTokenId", "ExpiredToken", "Unauthorized":
		return true
	}
	return false
}

// containsAuthRejectionText checks classification markers in a server message.
// The message is INPUT to the decision only — it never appears in output.
func containsAuthRejectionText(msg string) bool {
	m := lower(msg)
	for _, marker := range []string{"signature", "access key", "credential"} {
		if contains(m, marker) {
			return true
		}
	}
	return false
}

// diagnoseTransport classifies transport-layer failures by TYPE into the
// coarse net_kind taxonomy. Refused is the overwhelmingly common dial
// failure; other dial errors (unreachable) land in "other" only when they
// are not timeouts, which keeps "dial_refused" honest without string
// matching on error text.
func diagnoseTransport(err error) (Diagnostic, bool) {
	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return Diagnostic{Class: ClassNetwork, NetKind: "timeout"}, true
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return Diagnostic{Class: ClassNetwork, NetKind: "dns"}, true
	}
	// TLS shapes: Go wraps these in tls.CertificateVerificationError (1.20+)
	// or x509 errors; matching by type, never by message.
	var tlsErr *tls.CertificateVerificationError
	if errors.As(err, &tlsErr) {
		return Diagnostic{Class: ClassNetwork, NetKind: "tls"}, true
	}
	var unknownCertErr x509.UnknownAuthorityError
	if errors.As(err, &unknownCertErr) {
		return Diagnostic{Class: ClassNetwork, NetKind: "tls"}, true
	}
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		// Rework 5 finding 4: "refused" is an errno fact, not an Op fact —
		// dial can also fail with EACCES/ENETUNREACH/etc. Only a genuine
		// ECONNREFUSED anywhere in the chain is labeled dial_refused; every
		// other transport failure lands in "other".
		if errors.Is(err, syscall.ECONNREFUSED) {
			return Diagnostic{Class: ClassNetwork, NetKind: "dial_refused"}, true
		}
		return Diagnostic{Class: ClassNetwork, NetKind: "other"}, true
	}
	// *url.Error that is not a net.OpError/DNSError inside (rare).
	var uerr *url.Error
	if errors.As(err, &uerr) {
		return Diagnostic{Class: ClassNetwork, NetKind: "other"}, true
	}
	return Diagnostic{}, false
}

// lower/contains are tiny string helpers (kept local and trivial).
func lower(s string) string { return strings.ToLower(s) }

func contains(hay, needle string) bool { return strings.Contains(hay, needle) }
