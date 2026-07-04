package handler_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockStatsDB struct {
	stats *db.DashboardStats
	err   error
}

func (m *mockStatsDB) DashboardStats(_ context.Context, _ uuid.UUID, _ time.Time) (*db.DashboardStats, error) {
	return m.stats, m.err
}

func testStatsRouter(h *handler.StatsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, "org_viewer", uuid.New().String(), uuid.New().String())
		c.Next()
	})
	r.GET("/api/v1/stats/dashboard", h.Dashboard)
	return r
}

func TestStatsHandler_Dashboard_ReturnsAggregates(t *testing.T) {
	mock := &mockStatsDB{stats: &db.DashboardStats{
		TotalAgents:  10,
		OnlineAgents: 7,
		TotalFiles:   1234,
		StorageBytes: 5_000_000,
		TodayUploads: 42,
		UploadTrend: []db.DayCount{
			{Date: "2026-06-28", Count: 3},
			{Date: "2026-06-29", Count: 5},
		},
	}}
	h := handler.NewStatsHandler(mock, newTestLogger())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/stats/dashboard", nil)
	testStatsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, float64(10), body["total_agents"])
	assert.Equal(t, float64(7), body["online_agents"])
	assert.Equal(t, float64(1234), body["total_files"])
	assert.Equal(t, float64(5_000_000), body["storage_bytes"])
	assert.Equal(t, float64(42), body["today_uploads"])
	assert.Len(t, body["upload_trend"], 2)
}

func TestStatsHandler_Dashboard_DBError(t *testing.T) {
	h := handler.NewStatsHandler(&mockStatsDB{err: errors.New("boom")}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/stats/dashboard", nil)
	testStatsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestStatsHandler_Dashboard_NilDB_Returns501(t *testing.T) {
	h := handler.NewStatsHandler(nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/stats/dashboard", nil)
	testStatsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}
