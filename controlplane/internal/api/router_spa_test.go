package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"testing/fstest"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// newSPARouter builds a router wired with an in-memory Web UI filesystem so the
// SPA-serving behaviour can be exercised without a real webui/dist build.
func newSPARouter(t *testing.T) *httptest.Server {
	t.Helper()
	logger, _ := zap.NewDevelopment()
	webFS := fstest.MapFS{
		"index.html":    {Data: []byte("<!doctype html><title>fileagent</title>")},
		"assets/app.js": {Data: []byte("console.log('app')")},
		"favicon.svg":   {Data: []byte("<svg/>")},
	}
	r := NewRouter(RouterConfig{
		JWTSecret: "test-secret",
		Logger:    logger,
		WebUIFS:   webFS,
	})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestSPA_ServesIndexAtRoot(t *testing.T) {
	srv := newSPARouter(t)

	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "<title>fileagent</title>")
}

func TestSPA_ServesStaticAsset(t *testing.T) {
	srv := newSPARouter(t)

	resp, err := http.Get(srv.URL + "/assets/app.js")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "javascript")
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "console.log")
}

func TestSPA_ClientRouteFallsBackToIndex(t *testing.T) {
	srv := newSPARouter(t)

	// A deep client-side route (BrowserRouter) has no matching file → index.html.
	resp, err := http.Get(srv.URL + "/agents/123e4567-e89b-12d3-a456-426614174000")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "text/html")
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), "<title>fileagent</title>")
}

func TestSPA_UnknownAPIPathReturnsJSON404(t *testing.T) {
	srv := newSPARouter(t)

	resp, err := http.Get(srv.URL + "/api/v1/nonexistent")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
		RequestID string `json:"request_id"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&body))
	assert.Equal(t, "NOT_FOUND", body.Error.Code)
	assert.NotEmpty(t, body.RequestID, "API 404 must keep the standard error envelope")
}

func TestSPA_HealthzStillServed(t *testing.T) {
	srv := newSPARouter(t)

	resp, err := http.Get(srv.URL + "/healthz")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusOK, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.Contains(t, string(body), `"status":"ok"`)
}

func TestSPA_NonGETUnmatchedReturnsJSON404(t *testing.T) {
	srv := newSPARouter(t)

	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/some/random/path", nil)
	resp, err := srv.Client().Do(req)
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	assert.Contains(t, resp.Header.Get("Content-Type"), "application/json")
}

func TestSPA_DisabledWhenNoFS(t *testing.T) {
	// Pure-API build (WebUIFS nil): non-API routes fall through to Gin's default
	// 404 rather than serving an HTML shell.
	logger, _ := zap.NewDevelopment()
	r := NewRouter(RouterConfig{JWTSecret: "test-secret", Logger: logger})
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/")
	require.NoError(t, err)
	defer resp.Body.Close()

	assert.Equal(t, http.StatusNotFound, resp.StatusCode)
	body, _ := io.ReadAll(resp.Body)
	assert.NotContains(t, string(body), "<title>fileagent</title>")
}
