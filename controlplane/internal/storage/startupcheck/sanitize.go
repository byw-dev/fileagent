package startupcheck

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
)

// sanitize.go — the log-safety boundary for IC-BUG-40 (rework 3 finding 2).
//
// Error text arriving from the wire is untrusted input: a request URL can
// embed userinfo (access key : secret), SigV4 query parameters
// (X-Amz-Signature / X-Amz-Credential / X-Amz-Security-Token), and a
// misbehaving gateway can echo submitted credentials back in an error body.
// NONE of that may reach the logs. SafeErrorSummary is the ONLY sanctioned
// way to render a check error for output: it reduces the error to its safe,
// structured essence and additionally scrubs the known credential shapes
// from whatever text does survive, plus the configured secret itself.

// sensitiveParamRE matches credential-bearing query/header parameters whose
// VALUE must never be logged (case-insensitive).
var sensitiveParamRE = regexp.MustCompile(`(?i)\b(X-Amz-Signature|X-Amz-Credential|X-Amz-Security-Token|Signature|SessionToken|SecretAccessKey|SecretKey|AccessKeySecret)=([^&\s"']+)`)

// userinfoRE matches a URL authority carrying credentials
// (scheme://user:password@host) — both parts are secrets.
var userinfoRE = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://)[^/\s:?@]*:[^/\s@]*@`)

// SafeErrorSummary reduces err to a log-safe one-line summary. secret is the
// configured CP secret key (also masked wherever it appears).
//
// The mapping is deliberately lossy in favor of safety:
//   - S3 ErrorResponse → "s3 error: code=<Code> status=<HTTP status>" (the
//     server's Message is NOT included: it is free-form wire text);
//   - STS ErrorResponse → "sts error: code=<Code>";
//   - *url.Error → op + scheme://host (userinfo/query stripped) + the LEAF
//     cause, which for net errors is a standard phrase
//     ("connection refused", "i/o timeout", "no such host");
//   - context errors → their canonical names;
//   - anything else → the error's type and scrubbed text (belt and
//     suspenders: the scrubber masks the enumerated credential shapes and
//     the configured secret).
func SafeErrorSummary(err error, secret string) string {
	if err == nil {
		return ""
	}
	if resp := miniogo.ToErrorResponse(err); resp.Code != "" || resp.StatusCode != 0 {
		return fmt.Sprintf("s3 error: code=%s status=%d", resp.Code, resp.StatusCode)
	}
	var stsResp credentials.ErrorResponse
	if errors.As(err, &stsResp) {
		return fmt.Sprintf("sts error: code=%s", stsResp.STSError.Code)
	}
	var uerr *url.Error
	if errors.As(err, &uerr) {
		target := uerr.URL
		if u, perr := url.Parse(uerr.URL); perr == nil {
			u.User = nil
			u.RawQuery = ""
			u.Fragment = ""
			u.Path = ""
			target = u.String() // scheme://host[:port] only
		}
		return scrub(fmt.Sprintf("network error: %s %s: %v", uerr.Op, target, leafCause(uerr.Err)), secret)
	}
	if errors.Is(err, context.Canceled) {
		return "context canceled"
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "context deadline exceeded"
	}
	return scrub(fmt.Sprintf("%T: %v", err, err), secret)
}

// leafCause unwraps to the deepest error in the chain; net-layer causes have
// standard, credential-free texts (dial/refused/timeout/DNS).
func leafCause(err error) error {
	for {
		u := errors.Unwrap(err)
		if u == nil {
			return err
		}
		err = u
	}
}

// scrub masks the known credential shapes and the configured secret from s.
// It is the second line of defense behind the structured extraction above.
func scrub(s, secret string) string {
	s = sensitiveParamRE.ReplaceAllString(s, "$1=***")
	s = userinfoRE.ReplaceAllString(s, "$1***@")
	if secret != "" {
		s = strings.ReplaceAll(s, secret, "***")
	}
	return s
}
