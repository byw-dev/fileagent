package handler_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

type mockRetagJobsDB struct {
	job *db.RetagJob
	err error
}

func (m *mockRetagJobsDB) GetRetagJob(_ context.Context, _ uuid.UUID, _ uuid.UUID) (*db.RetagJob, error) {
	return m.job, m.err
}

func testRetagJobsRouter(h *handler.RetagJobsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, "org_viewer", testTagOrgID.String(), uuid.New().String())
		c.Next()
	})
	r.GET("/api/v1/retag-jobs/:id", h.Get)
	return r
}

func getRetagJob(id string) *http.Request {
	return req(http.MethodGet, "/api/v1/retag-jobs/"+id, "")
}

func TestRetagJobs_Get_Success(t *testing.T) {
	id := uuid.New()
	mockDB := &mockRetagJobsDB{job: &db.RetagJob{
		ID: id, OrgID: testTagOrgID, Kind: "merge", Status: "done",
		Attempts: 1, AffectedCount: 4, CreatedAt: time.Now(),
		FinishedAt: sql.NullTime{Time: time.Now(), Valid: true},
	}}
	h := handler.NewRetagJobsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testRetagJobsRouter(h).ServeHTTP(w, getRetagJob(id.String()))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"status":"done"`)
	assert.Contains(t, w.Body.String(), `"affected_count":4`)
}

func TestRetagJobs_Get_NotFound(t *testing.T) {
	mockDB := &mockRetagJobsDB{err: sql.ErrNoRows}
	h := handler.NewRetagJobsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testRetagJobsRouter(h).ServeHTTP(w, getRetagJob(uuid.New().String()))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestRetagJobs_Get_InvalidID(t *testing.T) {
	h := handler.NewRetagJobsHandler(&mockRetagJobsDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testRetagJobsRouter(h).ServeHTTP(w, getRetagJob("not-a-uuid"))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestRetagJobs_Get_NilDB_501(t *testing.T) {
	h := handler.NewRetagJobsHandler(nil, newTestLogger())
	w := httptest.NewRecorder()
	testRetagJobsRouter(h).ServeHTTP(w, getRetagJob(uuid.New().String()))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}
