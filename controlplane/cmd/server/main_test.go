package main

// main_test.go — unit tests for the wiring-only adapters in main.go. The one
// with real failure modes is minioObjectReader: its 404→ErrObjectAbsent
// mapping is the border between "object gone" (a Removed fact) and "storage
// trouble" (a processing failure) — the exact line mutation ① of the PR3b
// acceptance attacks from the adapter side.

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"

	"github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs"
)

// statTestServer spins a minimal S3-flavored HTTP server answering HEAD
// requests per bucket/key with the given status + code. It also answers the
// GetBucketLocation probe minio-go issues before the first call per bucket
// (GET /<bucket>/?location), so StatObject can reach the HEAD at all.
func statTestServer(t *testing.T, status int, code string, headers map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/xml")
		if r.URL.Query().Has("location") {
			_, _ = w.Write([]byte(`<?xml version="1.0" encoding="UTF-8"?><LocationConstraint xmlns="http://s3.amazonaws.com/doc/2006-03-01"/>`))
			return
		}
		for k, v := range headers {
			w.Header().Set(k, v)
		}
		w.WriteHeader(status)
		if code != "" {
			_, _ = w.Write([]byte(`<?xml version="1.0"?><Error><Code>` + code + `</Code></Error>`))
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newStatReader(t *testing.T, srv *httptest.Server) *minioObjectReader {
	t.Helper()
	client, err := miniogo.New(srv.Listener.Addr().String(), &miniogo.Options{
		Creds:  credentials.NewStaticV4("ak", "sk", ""),
		Secure: false,
	})
	if err != nil {
		t.Fatalf("minio client: %v", err)
	}
	return &minioObjectReader{client: client}
}

// HEAD 200 → the object's truth, straight from S3 (D1: Size/ETag come from
// the HEAD result, never from the event hint).
func TestMinioObjectReader_Head200(t *testing.T) {
	srv := statTestServer(t, http.StatusOK, "", map[string]string{
		"ETag": `"abc123"`,
		// Go's server suppresses the (empty) HEAD body but keeps the header —
		// minio-go reads the object size from Content-Length.
		"Content-Length": "777",
		"Last-Modified":  "Wed, 21 Oct 2015 07:28:00 GMT",
	})
	got, err := newStatReader(t, srv).StatObject(context.Background(), "data-sensor", "probe/file.txt")
	if err != nil {
		t.Fatalf("StatObject = %v, want nil", err)
	}
	if got.Size != 777 || got.ETag != "abc123" {
		t.Errorf("ObjectInfo = %+v, want size 777 etag abc123", got)
	}
}

// HEAD 404 (NoSuchKey — and SeaweedFS S3's NoSuchBucket is 404 too) is the
// ONLY absence: mapped to ErrObjectAbsent.
func TestMinioObjectReader_Head404_IsErrObjectAbsent(t *testing.T) {
	for _, tc := range []struct {
		name string
		code string
	}{
		{"NoSuchKey", "NoSuchKey"},
		{"NoSuchBucket", "NoSuchBucket"},
		{"bare 404", ""},
	} {
		srv := statTestServer(t, http.StatusNotFound, tc.code, nil)
		_, err := newStatReader(t, srv).StatObject(context.Background(), "data-sensor", "probe/file.txt")
		if !errors.Is(err, seaweedfs.ErrObjectAbsent) {
			t.Errorf("%s: StatObject = %v, want ErrObjectAbsent", tc.name, err)
		}
	}
}

// Any other failure (5xx, 403, connection refused) is NOT absence — it must
// come back as a plain error so the adapter retries instead of writing a
// wrongful deletion.
func TestMinioObjectReader_OtherErrors_AreNotAbsence(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		code   string
	}{
		{"internal error", http.StatusInternalServerError, "InternalError"},
		{"slow down", http.StatusServiceUnavailable, "SlowDown"},
		{"forbidden", http.StatusForbidden, "AccessDenied"},
	} {
		srv := statTestServer(t, tc.status, tc.code, nil)
		_, err := newStatReader(t, srv).StatObject(context.Background(), "data-sensor", "probe/file.txt")
		if err == nil {
			t.Errorf("%s: StatObject = nil, want an error", tc.name)
			continue
		}
		if errors.Is(err, seaweedfs.ErrObjectAbsent) {
			t.Errorf("%s: error misclassified as absence — a %d must be a processing failure, not a Removed (mutation ① class)", tc.name, tc.status)
		}
	}
}

// A transport-level failure (server unreachable) is likewise not absence.
func TestMinioObjectReader_TransportError_NotAbsence(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	url := srv.URL
	srv.Close() // nothing listens anymore
	client, err := miniogo.New(stripScheme(t, url), &miniogo.Options{
		Creds: credentials.NewStaticV4("ak", "sk", ""),
	})
	if err != nil {
		t.Fatalf("minio client: %v", err)
	}
	_, err = (&minioObjectReader{client: client}).StatObject(context.Background(), "data-sensor", "probe/file.txt")
	if err == nil {
		t.Fatal("StatObject = nil, want a transport error")
	}
	if errors.Is(err, seaweedfs.ErrObjectAbsent) {
		t.Fatal("a connection failure must never read as object absent — CP would delete the row on a network blip")
	}
}

// stripScheme removes the scheme so minio-go can dial host:port.
func stripScheme(t *testing.T, url string) string {
	t.Helper()
	for _, prefix := range []string{"http://", "https://"} {
		if len(url) > len(prefix) && url[:len(prefix)] == prefix {
			return url[len(prefix):]
		}
	}
	return url
}
