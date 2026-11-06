package uploader

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Review-r1 fix 2: the REAL minio-go client over httptest ───────────────────
//
// These tests drive the production Uploader through the real minio-go v7.0.91
// client stack (streaming SigV4 signer, chunked encoding, Content-Length
// handling) against a local httptest server. Mock stores cannot pin this
// behaviour: the growth scenario hinges on how the SDK's streaming signer
// reads the body relative to the declared length (live-probed in
// bin/b7-ic-bug-51/review-r1/minio_read_probe.go — without the LimitReader
// the SDK consumes MORE than the declared size and the request fails loudly).

// realSDKBucketServer is a minimal S3-compatible PUT endpoint: it accepts one
// object upload, records the exact bytes the wire carried, and answers with
// the body's MD5 as the ETag (both supported backends do this — live-verified
// in the PoC). The first-read hook lets a test mutate the source file after
// the client has started streaming.
type realSDKBucketServer struct {
	srv       *httptest.Server
	received  atomic.Int64
	mu        sync.Mutex
	body      []byte
	afterRead func()
	reads     atomic.Int32
}

func newRealSDKBucketServer(t *testing.T) *realSDKBucketServer {
	t.Helper()
	s := &realSDKBucketServer{}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPut {
			// minio-go probes GetBucketLocation before the first object request
			// (bucket-cache). Answer it so the client proceeds to the PUT.
			w.Header().Set("Content-Type", "application/xml")
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><LocationConstraint>us-east-1</LocationConstraint>`))
			return
		}
		buf := make([]byte, 64*1024)
		for {
			n, err := r.Body.Read(buf)
			if n > 0 {
				s.mu.Lock()
				s.body = append(s.body, buf[:n]...)
				s.mu.Unlock()
				s.received.Add(int64(n))
				s.reads.Add(1)
				if s.reads.Load() == 1 && s.afterRead != nil {
					s.afterRead()
				}
			}
			if err == io.EOF {
				break
			}
			if err != nil {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
		}
		// NOTE: no ETag is returned. Over plaintext HTTP the SDK signs the
		// request with aws-chunked STREAMING-AWS4-HMAC-SHA256-PAYLOAD, whose
		// // chunk framing/signature bytes are part of the raw body seen here;
		// a server-computed md5(s.body) is therefore NOT the md5 of the object
		// payload and cannot be compared with the client-side digest. A real
		// S3 backend decodes the framing; this test server counts the WIRE
		// bytes (>= payload) and omits the ETag, so the uploader's
		// opportunistic ETag cross-check is skipped (etagIsComparable("") is
		// false) — exactly the designed behavior for non-comparable ETags.
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// uploaderAgainstRealSDK wires a production Uploader (real minio.Core under
// the minioAdapter) at the test server's endpoint.
func uploaderAgainstRealSDK(t *testing.T, srv *realSDKBucketServer, q *queue.Queue) *Uploader {
	t.Helper()
	endpoint := strings.TrimPrefix(srv.srv.URL, "http://")
	u, err := New(Config{
		Endpoint:    endpoint,
		AccessKey:   "test-access",
		SecretKey:   "test-secret",
		UseSSL:      false,
		ThresholdMB: 64,
		PartSizeMB:  64,
	}, q, zap.NewNop())
	require.NoError(t, err)
	return u
}

// TestIC51_RealSDK_GrowsMidUpload_UploadsDeclaredPrefix: with the real
// minio-go client, a file that grows after the uploader's stat still uploads
// EXACTLY the declared prefix successfully, and the reported hash/size
// describe that prefix. (Without io.LimitReader the SDK over-reads past the
// declared Content-Length and the request fails loudly — verified by the
// review probe; the LimitReader turns that into the deterministic prefix
// upload asserted here.)
func TestIC51_RealSDK_GrowsMidUpload_UploadsDeclaredPrefix(t *testing.T) {
	const size = 256 * 1024
	dir := t.TempDir()
	path := filepath.Join(dir, "grow-real.dat")
	require.NoError(t, os.WriteFile(path, bytesRepeat("A", size), 0o644))

	srv := newRealSDKBucketServer(t)
	q := newTestQueue(t)
	task := newTask(t, path, "bucket", "ic51/grow-real.dat")
	require.NoError(t, q.Enqueue(task))

	srv.afterRead = func() {
		// The writer appends AFTER the uploader's stat and AFTER the client
		// started streaming: the first `size` bytes are already on the wire.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
		if err != nil {
			return
		}
		_, _ = f.Write(bytesRepeat("B", 64*1024))
		_ = f.Close()
	}

	u := uploaderAgainstRealSDK(t, srv, q)
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err, "with the LimitReader the growth must not break the real SDK upload")

	// The wire carries the payload PLUS the aws-chunked framing (chunk
	// headers/trailers), so the received count is strictly greater than the
	// payload — but it must be bounded by payload + framing of exactly the
	// declared size, never the grown size. The decisive assertion is the
	// reported hash: it must equal the sha256 of the declared 256 KiB prefix,
	// and must NOT equal the sha256 of the grown file.
	assert.Greater(t, srv.received.Load(), int64(size), "framing bytes are on the wire too")
	assert.LessOrEqual(t, srv.received.Load(), int64(size)+8*1024,
		"the wire carried the declared payload plus bounded aws-chunked framing — not the grown file")
	assert.Equal(t, sha256Hex(bytesRepeat("A", size)), result.SHA256,
		"reported sha256 = hash of the declared prefix (what the object will contain)")
	grown := append(append([]byte{}, bytesRepeat("A", size)...), bytesRepeat("B", 64*1024)...)
	assert.NotEqual(t, sha256Hex(grown), result.SHA256,
		"the reported hash must NOT describe the grown file")
	assert.Equal(t, int64(size), result.SizeBytes)
}

// TestIC51_RealSDK_TruncatedMidUpload_FailsLoudly: with the real client, a
// file truncated mid-stream cannot satisfy the declared Content-Length — the
// request fails (streaming signer / count guard; whichever fires first), and
// the upload is NEVER reported as success.
func TestIC51_RealSDK_TruncatedMidUpload_FailsLoudly(t *testing.T) {
	const size = 256 * 1024
	dir := t.TempDir()
	path := filepath.Join(dir, "trunc-real.dat")
	require.NoError(t, os.WriteFile(path, bytesRepeat("A", size), 0o644))

	srv := newRealSDKBucketServer(t)
	q := newTestQueue(t)
	task := newTask(t, path, "bucket", "ic51/trunc-real.dat")
	require.NoError(t, q.Enqueue(task))

	srv.afterRead = func() {
		_ = os.Truncate(path, 64*1024)
	}

	u := uploaderAgainstRealSDK(t, srv, q)
	result, err := u.UploadFile(context.Background(), task)
	require.Error(t, err, "a truncated source must never produce a successful upload")
	assert.Nil(t, result)
	assert.Less(t, srv.received.Load(), int64(size),
		"the wire carried less than the declared size")
}
