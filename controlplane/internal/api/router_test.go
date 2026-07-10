package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func newTestRouter(t *testing.T) *httptest.Server {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	r := NewRouter(RouterConfig{
		JWTSecret: "test-secret",
		Logger:    logger,
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestHealthz(t *testing.T) {
	srv := newTestRouter(t)

	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
}

func TestAuthRoutes_Return501(t *testing.T) {
	srv := newTestRouter(t)
	client := srv.Client()

	routes := []struct {
		method string
		path   string
	}{
		{"POST", "/api/auth/login"},
		{"POST", "/api/auth/refresh"},
		{"POST", "/api/auth/logout"},
		{"GET", "/api/auth/me"},
	}

	for _, rt := range routes {
		t.Run(rt.method+"_"+rt.path, func(t *testing.T) {
			req, _ := http.NewRequest(rt.method, srv.URL+rt.path, nil)
			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		})
	}
}

func TestProtectedRoutes_NoToken_Returns401(t *testing.T) {
	srv := newTestRouter(t)
	client := srv.Client()

	// Authenticated routes without a token should return 401.
	protectedRoutes := []struct {
		method string
		path   string
	}{
		{"GET", "/api/v1/agents"},
		{"GET", "/api/v1/files"},
		{"GET", "/api/v1/file-types"},
		{"GET", "/api/v1/tag-keys"},
		{"GET", "/api/v1/tag-keys/site/values"},
		{"GET", "/api/v1/pending-tag-values"},
		{"GET", "/api/v1/buckets"},
		{"GET", "/api/v1/event-rules"},
		{"GET", "/api/v1/upload-logs"},
		{"GET", "/api/v1/users"},
		{"GET", "/api/v1/stats/dashboard"},
	}

	for _, rt := range protectedRoutes {
		t.Run(rt.method+"_"+rt.path, func(t *testing.T) {
			req, _ := http.NewRequest(rt.method, srv.URL+rt.path, nil)
			resp, err := client.Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusUnauthorized, resp.StatusCode)
		})
	}
}

func TestRequestIDInjected(t *testing.T) {
	srv := newTestRouter(t)

	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()

	// The response must always carry an X-Request-ID header.
	assert.NotEmpty(t, resp.Header.Get("X-Request-ID"))
}

func TestRequestID_ClientProvided_EchoedBack(t *testing.T) {
	srv := newTestRouter(t)

	req, _ := http.NewRequest("GET", srv.URL+"/healthz", nil)
	req.Header.Set("X-Request-ID", "client-123")

	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, "client-123", resp.Header.Get("X-Request-ID"))
}
