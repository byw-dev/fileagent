package middleware_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func init() {
	gin.SetMode(gin.TestMode)
}

func setupErrorRouter() *gin.Engine {
	r := gin.New()
	r.Use(middleware.RequestID())
	return r
}

func TestRequestID_Generated(t *testing.T) {
	r := setupErrorRouter()
	r.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.NotEmpty(t, w.Header().Get("X-Request-ID"))
}

func TestRequestID_Provided(t *testing.T) {
	r := setupErrorRouter()
	r.GET("/test", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/test", nil)
	req.Header.Set("X-Request-ID", "my-req-id")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, "my-req-id", w.Header().Get("X-Request-ID"))
}

func TestAbortWithError_StandardShape(t *testing.T) {
	r := gin.New()
	r.Use(middleware.RequestID())
	r.GET("/err", func(c *gin.Context) {
		middleware.AbortWithError(c, http.StatusBadRequest, "BAD_REQUEST", "bad request test", nil)
	})

	req := httptest.NewRequest(http.MethodGet, "/err", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusBadRequest, w.Code)
	var body middleware.ErrorResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "BAD_REQUEST", body.Error.Code)
	assert.Equal(t, "bad request test", body.Error.Message)
	assert.NotEmpty(t, body.RequestID)
}

func TestAbortWithError_WithDetail(t *testing.T) {
	r := gin.New()
	r.Use(middleware.RequestID())
	r.GET("/err", func(c *gin.Context) {
		middleware.AbortWithError(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "validation failed",
			map[string]string{"field": "email", "reason": "invalid"})
	})

	req := httptest.NewRequest(http.MethodGet, "/err", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	errObj := body["error"].(map[string]interface{})
	assert.NotNil(t, errObj["detail"])
}

func TestNotImplemented_Returns501(t *testing.T) {
	r := gin.New()
	r.Use(middleware.RequestID())
	r.GET("/ni", middleware.NotImplemented)

	req := httptest.NewRequest(http.MethodGet, "/ni", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestNewErrorBody_Shape(t *testing.T) {
	body := middleware.NewErrorBody("ERR_CODE", "some message", nil)
	assert.Equal(t, "ERR_CODE", body["code"])
	assert.Equal(t, "some message", body["message"])
	assert.Nil(t, body["detail"])
}

func TestNewErrorBody_WithDetail(t *testing.T) {
	detail := []string{"a", "b"}
	body := middleware.NewErrorBody("ERR_CODE", "msg", detail)
	assert.Equal(t, detail, body["detail"])
}
