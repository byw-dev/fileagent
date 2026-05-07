package handler_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	_ "go.uber.org/zap" // indirect — newTestLogger defined in users_test.go
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupHandlerRouter(t *testing.T) (*httptest.Server, *gin.Engine) {
	t.Helper()
	r := gin.New()
	return httptest.NewServer(r), r
}

// ── AgentsHandler ─────────────────────────────────────────────────────────────

func TestAgentsHandler_AllReturn501(t *testing.T) {
	r := gin.New()
	h := handler.NewAgentsHandler(nil, nil, nil, nil, newTestLogger())
	r.GET("/agents", h.List)
	r.GET("/agents/:id", h.Get)
	r.POST("/agents/:id/approve", h.Approve)
	r.POST("/agents/:id/revoke", h.Revoke)
	r.POST("/agents/:id/list-dir", h.ListDir)
	r.GET("/agents/:id/rules", h.ListRules)
	r.POST("/agents/:id/rules", h.CreateRule)
	r.PUT("/agents/:id/rules/:rid", h.UpdateRule)
	r.DELETE("/agents/:id/rules/:rid", h.DeleteRule)
	r.GET("/agents/:id/upload-logs", h.ListUploadLogs)

	srv := httptest.NewServer(r)
	defer srv.Close()

	routes := []struct{ method, path string }{
		{"GET", "/agents"},
		{"GET", "/agents/1"},
		{"POST", "/agents/1/approve"},
		{"POST", "/agents/1/revoke"},
		{"POST", "/agents/1/list-dir"},
		{"GET", "/agents/1/rules"},
		{"POST", "/agents/1/rules"},
		{"PUT", "/agents/1/rules/2"},
		{"DELETE", "/agents/1/rules/2"},
		{"GET", "/agents/1/upload-logs"},
	}
	for _, rt := range routes {
		t.Run(rt.method+"_"+rt.path, func(t *testing.T) {
			req, _ := http.NewRequest(rt.method, srv.URL+rt.path, nil)
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		})
	}
}

// ── FilesHandler ──────────────────────────────────────────────────────────────

func TestFilesHandler_AllReturn501(t *testing.T) {
	r := gin.New()
	h := handler.NewFilesHandler(nil, nil, newTestLogger())
	r.GET("/files", h.List)
	r.GET("/files/:id", h.Get)
	r.GET("/files/:id/download-url", h.DownloadURL)
	r.POST("/files/download-urls", h.BatchDownloadURLs)

	srv := httptest.NewServer(r)
	defer srv.Close()

	routes := []struct{ method, path string }{
		{"GET", "/files"},
		{"GET", "/files/1"},
		{"GET", "/files/1/download-url"},
		{"POST", "/files/download-urls"},
	}
	for _, rt := range routes {
		t.Run(rt.method+"_"+rt.path, func(t *testing.T) {
			req, _ := http.NewRequest(rt.method, srv.URL+rt.path, nil)
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		})
	}
}

// ── FileTypesHandler ──────────────────────────────────────────────────────────

func TestFileTypesHandler_AllReturn501(t *testing.T) {
	r := gin.New()
	h := handler.NewFileTypesHandler(nil, newTestLogger())
	r.GET("/file-types", h.List)
	r.POST("/file-types", h.Create)
	r.PUT("/file-types/:id", h.Update)
	r.DELETE("/file-types/:id", h.Delete)

	srv := httptest.NewServer(r)
	defer srv.Close()

	routes := []struct{ method, path string }{
		{"GET", "/file-types"},
		{"POST", "/file-types"},
		{"PUT", "/file-types/1"},
		{"DELETE", "/file-types/1"},
	}
	for _, rt := range routes {
		t.Run(rt.method+"_"+rt.path, func(t *testing.T) {
			req, _ := http.NewRequest(rt.method, srv.URL+rt.path, nil)
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		})
	}
}

// ── BucketsHandler ────────────────────────────────────────────────────────────

func TestBucketsHandler_AllReturn501(t *testing.T) {
	r := gin.New()
	h := handler.NewBucketsHandler(nil, nil, newTestLogger())
	r.GET("/buckets", h.List)
	r.POST("/buckets", h.Create)

	srv := httptest.NewServer(r)
	defer srv.Close()

	routes := []struct{ method, path string }{
		{"GET", "/buckets"},
		{"POST", "/buckets"},
	}
	for _, rt := range routes {
		t.Run(rt.method+"_"+rt.path, func(t *testing.T) {
			req, _ := http.NewRequest(rt.method, srv.URL+rt.path, nil)
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		})
	}
}

// ── EventRulesHandler ─────────────────────────────────────────────────────────

func TestEventRulesHandler_AllReturn501(t *testing.T) {
	r := gin.New()
	h := handler.NewEventRulesHandler(nil, newTestLogger())
	r.GET("/event-rules", h.List)
	r.POST("/event-rules", h.Create)
	r.PUT("/event-rules/:id", h.Update)
	r.DELETE("/event-rules/:id", h.Delete)
	r.GET("/event-rules/:id/deliveries", h.ListDeliveries)

	srv := httptest.NewServer(r)
	defer srv.Close()

	routes := []struct{ method, path string }{
		{"GET", "/event-rules"},
		{"POST", "/event-rules"},
		{"PUT", "/event-rules/1"},
		{"DELETE", "/event-rules/1"},
		{"GET", "/event-rules/1/deliveries"},
	}
	for _, rt := range routes {
		t.Run(rt.method+"_"+rt.path, func(t *testing.T) {
			req, _ := http.NewRequest(rt.method, srv.URL+rt.path, nil)
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		})
	}
}

// ── UploadLogsHandler ─────────────────────────────────────────────────────────

func TestUploadLogsHandler_AllReturn501(t *testing.T) {
	r := gin.New()
	h := handler.NewUploadLogsHandler(nil, newTestLogger())
	r.GET("/upload-logs", h.List)
	r.GET("/upload-logs/:id", h.Get)

	srv := httptest.NewServer(r)
	defer srv.Close()

	routes := []struct{ method, path string }{
		{"GET", "/upload-logs"},
		{"GET", "/upload-logs/1"},
	}
	for _, rt := range routes {
		t.Run(rt.method+"_"+rt.path, func(t *testing.T) {
			req, _ := http.NewRequest(rt.method, srv.URL+rt.path, nil)
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		})
	}
}

// ── UsersHandler ──────────────────────────────────────────────────────────────

func TestUsersHandler_AllReturn501(t *testing.T) {
	r := gin.New()
	h := handler.NewUsersHandler(nil, newTestLogger()) // nil db → 501 for all methods
	r.GET("/users", h.List)
	r.POST("/users", h.Create)
	r.PUT("/users/:id", h.Update)
	r.DELETE("/users/:id", h.Delete)
	r.PUT("/users/:id/password", h.UpdatePassword)

	srv := httptest.NewServer(r)
	defer srv.Close()

	routes := []struct{ method, path string }{
		{"GET", "/users"},
		{"POST", "/users"},
		{"PUT", "/users/1"},
		{"DELETE", "/users/1"},
		{"PUT", "/users/1/password"},
	}
	for _, rt := range routes {
		t.Run(rt.method+"_"+rt.path, func(t *testing.T) {
			req, _ := http.NewRequest(rt.method, srv.URL+rt.path, nil)
			resp, err := srv.Client().Do(req)
			require.NoError(t, err)
			defer resp.Body.Close()
			assert.Equal(t, http.StatusNotImplemented, resp.StatusCode)
		})
	}
}
