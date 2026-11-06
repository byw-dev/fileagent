package startupcheck

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// probe_test.go covers the PRODUCTION adapters (probe.go + newInternalClient)
// against a local httptest server. No real storage is touched: the server
// answers canned S3/STS responses so both the happy path and the error
// mapping (XML body → miniogo.ErrorResponse / credentials.ErrorResponse) run
// through the real minio-go code.

// s3Server answers GET /{bucket}?uploads like both backends do.
func s3Server(t *testing.T, status int, body string) *http.Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	srv.Addr = ln.Addr().String()
	return srv
}

// stsServer answers POST / with the STS AssumeRole contract.
func stsServer(t *testing.T, status int, body string) *http.Server {
	return s3Server(t, status, body)
}

const listOK = `<?xml version="1.0" encoding="UTF-8"?>
<ListMultipartUploadsResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Bucket>data-sensor</Bucket><IsTruncated>false</IsTruncated></ListMultipartUploadsResult>`

const sigMismatchXML = `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>SignatureDoesNotMatch</Code><Message>The request signature we calculated does not match the signature you provided.</Message><Resource>/data-sensor</Resource></Error>`

// Real backends always include <RequestId> in the S3 error XML (MinIO and
// SeaweedFS live-verified); the probe's anti-forgery guard requires it
// (minio-go synthesizes a RequestID-less NoSuchBucket for unparseable 404s).
const noSuchBucketXML = `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist.</Message><Resource>/data-sensor</Resource><RequestId>18E5EXAMPLE</RequestId></Error>`

// A NoSuchBucket WITHOUT RequestId is indistinguishable from minio-go's
// synthesized fallback for non-S3 404 bodies — must NOT be trusted.
const noSuchBucketNoRequestIDXML = `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist.</Message></Error>`

const stsOK = `<?xml version="1.0" encoding="UTF-8"?>
<AssumeRoleResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><AssumeRoleResult><Credentials><AccessKeyId>ASIAEXAMPLE</AccessKeyId><SecretAccessKey>tempsecret</SecretAccessKey><SessionToken>tok</SessionToken><Expiration>2030-01-01T00:00:00Z</Expiration></Credentials></AssumeRoleResult></AssumeRoleResponse>`

const stsDeniedXML = `<?xml version="1.0" encoding="UTF-8"?>
<ErrorResponse xmlns="https://sts.amazonaws.com/doc/2011-06-15/"><Error><Type>Sender</Type><Code>AccessDenied</Code><Message>user fileagent-controlplane is not authorized to assume role arn:aws:iam::000000000000:role/nope</Message></Error></ErrorResponse>`

func probeCfg(endpoint string) Config {
	return Config{
		Endpoint:    endpoint,
		AccessKey:   "TESTACCESSKEYID",
		SecretKey:   "test-secret",
		UseSSL:      false,
		RoleARN:     "arn:aws:iam:::role/agent-role",
		ProbeBucket: "data-sensor",
	}
}

// ListMultipartUploads: happy path through real minio-go client code.
func TestProbeS3_Success(t *testing.T) {
	srv := s3Server(t, 200, listOK)
	client, err := newInternalClient(probeCfg(srv.Addr))
	require.NoError(t, err)
	require.NoError(t, ProbeS3{Client: client}.ListMultipartUploads(context.Background(), "data-sensor"))
}

// ListMultipartUploads: wrong secret surfaces as miniogo.ErrorResponse with
// the backend's code (live-verified shape on MinIO + SeaweedFS).
func TestProbeS3_SignatureDoesNotMatch(t *testing.T) {
	srv := s3Server(t, 403, sigMismatchXML)
	client, err := newInternalClient(probeCfg(srv.Addr))
	require.NoError(t, err)
	perr := ProbeS3{Client: client}.ListMultipartUploads(context.Background(), "data-sensor")
	require.Error(t, perr)
	resp := miniogo.ToErrorResponse(perr)
	assert.Equal(t, "SignatureDoesNotMatch", resp.Code)
	assert.Equal(t, 403, resp.StatusCode)
	assert.Equal(t, ClassAuth, diagnose(perr).Class)
}

// ListMultipartUploads (rework 4 finding 1): a missing bucket FAILS the probe
// — the bucket must physically exist; NoSuchBucket retries within the window.
func TestProbeS3_NoSuchBucketFails(t *testing.T) {
	srv := s3Server(t, 404, noSuchBucketXML)
	client, err := newInternalClient(probeCfg(srv.Addr))
	require.NoError(t, err)
	perr := ProbeS3{Client: client}.ListMultipartUploads(context.Background(), "data-sensor")
	require.Error(t, perr, "NoSuchBucket must fail the probe (bucket must exist)")
	d := diagnose(perr)
	assert.Equal(t, "NoSuchBucket", d.S3Code)
	assert.Equal(t, ClassConfig, d.Class)
}

// ListMultipartUploads: transport failure (server gone) is a network-class
// error, not an S3 ErrorResponse.
func TestProbeS3_Unreachable(t *testing.T) {
	// Bind then close: a port that refuses connections.
	srv := s3Server(t, 200, listOK)
	addr := srv.Addr
	_ = srv.Close()
	time.Sleep(10 * time.Millisecond)
	client, err := newInternalClient(probeCfg(addr))
	require.NoError(t, err)
	perr := ProbeS3{Client: client}.ListMultipartUploads(context.Background(), "data-sensor")
	require.Error(t, perr)
	var uerr *url.Error
	require.ErrorAs(t, perr, &uerr)
	assert.Equal(t, ClassNetwork, diagnose(perr).Class)
}

// AssumeRole: happy path — credentials returned but DISCARDED (function
// returns nil and exposes nothing).
func TestAssumeRoleSTS_Success(t *testing.T) {
	srv := stsServer(t, 200, stsOK)
	cfg := probeCfg(srv.Addr)
	require.NoError(t, AssumeRoleSTS{Config: cfg}.AssumeRole(context.Background()))
}

// AssumeRole: role-not-authorized (SeaweedFS shape) parses into
// credentials.ErrorResponse and classifies auth/config.
func TestAssumeRoleSTS_RoleDenied(t *testing.T) {
	srv := stsServer(t, 403, stsDeniedXML)
	cfg := probeCfg(srv.Addr)
	err := AssumeRoleSTS{Config: cfg}.AssumeRole(context.Background())
	require.Error(t, err)
	var errResp credentials.ErrorResponse
	require.ErrorAs(t, err, &errResp)
	assert.Equal(t, "AccessDenied", errResp.STSError.Code)
	assert.Equal(t, ClassConfig, diagnose(err).Class)
}

// AssumeRole: unreachable STS endpoint is network-class.
func TestAssumeRoleSTS_Unreachable(t *testing.T) {
	srv := stsServer(t, 200, stsOK)
	addr := srv.Addr
	_ = srv.Close()
	time.Sleep(10 * time.Millisecond)
	cfg := probeCfg(addr)
	err := AssumeRoleSTS{Config: cfg}.AssumeRole(context.Background())
	require.Error(t, err)
	assert.Equal(t, ClassNetwork, diagnose(err).Class)
}

// AssumeRole honours a pre-cancelled context without dialing.
func TestAssumeRoleSTS_ContextCancelled(t *testing.T) {
	srv := stsServer(t, 200, stsOK)
	cfg := probeCfg(srv.Addr)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := AssumeRoleSTS{Config: cfg}.AssumeRole(ctx)
	require.Error(t, err)
	assert.ErrorIs(t, errors.Unwrap(err), context.Canceled)
}

// newInternalClient rejects an unparseable endpoint (validated earlier in
// config, but the constructor must stay honest on its own).
func TestNewInternalClient_BadEndpoint(t *testing.T) {
	_, err := newInternalClient(probeCfg("http://bad endpoint space:9000"))
	require.Error(t, err)
}

// newInternalClient: https flag threads through (Secure set on the client
// options — observable via a successful construction only here; the flag is
// exercised live in the backend demos).
func TestNewInternalClient_Valid(t *testing.T) {
	client, err := newInternalClient(probeCfg("127.0.0.1:9000"))
	require.NoError(t, err)
	require.NotNil(t, client)
	_ = fmt.Sprint(client)
}

// ─── Rework 4 finding 1: every 404 fails; the bucket must exist ──────────────

// A 404 whose S3 error code is AccessDenied (a rejecting proxy shape) must
// FAIL the probe.
func TestProbeS3_AccessDenied404Fails(t *testing.T) {
	srv := s3Server(t, 404, `<Error><Code>AccessDenied</Code><Message>proxy rejected this credential</Message></Error>`)
	client, err := newInternalClient(probeCfg(srv.Addr))
	require.NoError(t, err)
	perr := ProbeS3{Client: client}.ListMultipartUploads(context.Background(), "data-sensor")
	require.Error(t, perr, "404 + AccessDenied must fail the probe")
	resp := miniogo.ToErrorResponse(perr)
	assert.Equal(t, "AccessDenied", resp.Code)
	assert.Equal(t, ClassConfig, diagnose(perr).Class)
}

// A gateway's HTML 404 (not S3 XML) must FAIL the probe.
func TestProbeS3_GatewayHTML404Fails(t *testing.T) {
	srv := s3Server(t, 404, `<html><head><title>404 Not Found</title></head><body>nginx</body></html>`)
	client, err := newInternalClient(probeCfg(srv.Addr))
	require.NoError(t, err)
	perr := ProbeS3{Client: client}.ListMultipartUploads(context.Background(), "data-sensor")
	require.Error(t, perr, "non-S3 HTML 404 must fail the probe")
}

// Review r2 finding 1's exploit, asserted the FIXED way: minio-go synthesizes
// NoSuchBucket for unparseable 404s and back-fills RequestID from the
// x-amz-request-id header — a gateway can therefore forge the exact shape the
// old "NoSuchBucket + RequestID" guard trusted. Under "the bucket must
// exist", the forged absence FAILS like any other error.
func TestProbeS3_Gateway404WithRequestHeaderFails(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("x-amz-request-id", "proxy-request-123")
		w.Header().Set("Content-Type", "text/html")
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte("<html>404 gateway route missing</html>"))
	})}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	client, err := newInternalClient(probeCfg(ln.Addr().String()))
	require.NoError(t, err)
	perr := ProbeS3{Client: client}.ListMultipartUploads(context.Background(), "data-sensor")
	require.Error(t, perr, "gateway HTML 404 with x-amz-request-id must fail the probe (review r2 exploit)")
}
