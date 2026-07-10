package handler_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
)

type mockPendingDB struct {
	rows       []*db.ListPendingTagValuesRow
	listErr    error
	getRow     *db.PendingTagValue
	getErr     error
	createErr  error
	deleteRows int64
	deleteErr  error

	createCalled bool
	deleteCalled bool
}

func (m *mockPendingDB) ListPendingTagValues(_ context.Context, _ uuid.UUID) ([]*db.ListPendingTagValuesRow, error) {
	return m.rows, m.listErr
}
func (m *mockPendingDB) GetPendingTagValue(_ context.Context, _ uuid.UUID, _ uuid.UUID) (*db.PendingTagValue, error) {
	return m.getRow, m.getErr
}
func (m *mockPendingDB) CreateTagValueIfAbsent(_ context.Context, _ uuid.UUID, _ string) (int64, error) {
	m.createCalled = true
	if m.createErr != nil {
		return 0, m.createErr
	}
	return 1, nil
}
func (m *mockPendingDB) DeletePendingTagValue(_ context.Context, _ uuid.UUID, _ uuid.UUID) (int64, error) {
	m.deleteCalled = true
	return m.deleteRows, m.deleteErr
}

func testPendingRouter(h *handler.PendingTagValuesHandler, role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, role, testTagOrgID.String(), uuid.New().String())
		c.Next()
	})
	superAdmin := middleware.RequireRole("super_admin")
	g := r.Group("/api/v1/pending-tag-values")
	g.GET("", h.List)
	g.POST("/:id/approve", superAdmin, h.Approve)
	g.POST("/:id/reject", superAdmin, h.Reject)
	return r
}

func pendingRow() *db.ListPendingTagValuesRow {
	return &db.ListPendingTagValuesRow{
		ID: uuid.New(), TagKeyID: uuid.New(), Key: "site",
		ExtractedValue: "tokyo", Source: "path_var", HitCount: 3,
		SuggestedValue: sql.NullString{String: "Tokyo", Valid: true},
		FirstSeenAt:    time.Now(),
	}
}

func TestPending_List_Success(t *testing.T) {
	mockDB := &mockPendingDB{rows: []*db.ListPendingTagValuesRow{pendingRow()}}
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "org_viewer").ServeHTTP(w, req(http.MethodGet, "/api/v1/pending-tag-values", ""))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestPending_List_NilDB_501(t *testing.T) {
	h := handler.NewPendingTagValuesHandler(nil, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, req(http.MethodGet, "/api/v1/pending-tag-values", ""))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestPending_Approve_Success(t *testing.T) {
	id := uuid.New()
	mockDB := &mockPendingDB{
		getRow:     &db.PendingTagValue{ID: id, OrgID: testTagOrgID, TagKeyID: uuid.New(), ExtractedValue: "tokyo"},
		deleteRows: 1,
	}
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/pending-tag-values/"+id.String()+"/approve", ""))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, mockDB.createCalled) // value promoted into vocabulary
	assert.True(t, mockDB.deleteCalled) // removed from queue
}

func TestPending_Approve_NotFound(t *testing.T) {
	mockDB := &mockPendingDB{getErr: sql.ErrNoRows}
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/pending-tag-values/"+uuid.New().String()+"/approve", ""))
	assert.Equal(t, http.StatusNotFound, w.Code)
	assert.False(t, mockDB.createCalled)
}

func TestPending_Approve_Forbidden_NonSuperAdmin(t *testing.T) {
	h := handler.NewPendingTagValuesHandler(&mockPendingDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "org_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/pending-tag-values/"+uuid.New().String()+"/approve", ""))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestPending_Approve_InvalidID(t *testing.T) {
	h := handler.NewPendingTagValuesHandler(&mockPendingDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/pending-tag-values/not-a-uuid/approve", ""))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestPending_Reject_Success(t *testing.T) {
	mockDB := &mockPendingDB{deleteRows: 1}
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/pending-tag-values/"+uuid.New().String()+"/reject", ""))
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestPending_Reject_NotFound(t *testing.T) {
	mockDB := &mockPendingDB{deleteRows: 0}
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/pending-tag-values/"+uuid.New().String()+"/reject", ""))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestPending_Reject_Forbidden_NonSuperAdmin(t *testing.T) {
	h := handler.NewPendingTagValuesHandler(&mockPendingDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "org_viewer").ServeHTTP(w, req(http.MethodPost, "/api/v1/pending-tag-values/"+uuid.New().String()+"/reject", ""))
	assert.Equal(t, http.StatusForbidden, w.Code)
}
