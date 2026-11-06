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

const noSuchBucketXML = `<?xml version="1.0" encoding="UTF-8"?>
<Error><Code>NoSuchBucket</Code><Message>The specified bucket does not exist.</Message><Resource>/data-sensor</Resource></Error>`

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
	assert.Equal(t, ClassAuthConfig, ClassifyS3(perr))
}

// ListMultipartUploads: a missing bucket still proves identity + reachability
// (bucket state is server data, not a credential fact) — must return nil.
func TestProbeS3_NoSuchBucketStillPasses(t *testing.T) {
	srv := s3Server(t, 404, noSuchBucketXML)
	client, err := newInternalClient(probeCfg(srv.Addr))
	require.NoError(t, err)
	require.NoError(t, ProbeS3{Client: client}.ListMultipartUploads(context.Background(), "data-sensor"))
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
	assert.Equal(t, ClassNetwork, ClassifyS3(perr))
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
	assert.Equal(t, ClassAuthConfig, ClassifySTS(err))
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
	assert.Equal(t, ClassNetwork, ClassifySTS(err))
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
