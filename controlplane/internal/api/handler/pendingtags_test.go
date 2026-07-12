package handler_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/retag"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockPendingDB struct {
	rows       []*db.ListPendingTagValuesRow
	listErr    error
	getRow     *db.PendingTagValue
	getErr     error
	createErr  error
	deleteRows int64
	deleteErr  error
	valueOK    bool  // TagValueExists result
	valueErr   error // TagValueExists error
	enqueueErr error

	createCalled  bool
	deleteCalled  bool
	enqueuedKind  string
	enqueuedSpec  json.RawMessage
	enqueuedActor uuid.NullUUID
}

func (m *mockPendingDB) ListPendingTagValues(_ context.Context, _ uuid.UUID) ([]*db.ListPendingTagValuesRow, error) {
	return m.rows, m.listErr
}
func (m *mockPendingDB) GetPendingTagValue(_ context.Context, _ uuid.UUID, _ uuid.UUID) (*db.PendingTagValue, error) {
	return m.getRow, m.getErr
}
func (m *mockPendingDB) CreateTagValueIfAbsent(_ context.Context, _ uuid.UUID, _ string, _ uuid.UUID) (int64, error) {
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
func (m *mockPendingDB) TagValueExistsInOrg(_ context.Context, _ uuid.UUID, _ string, _ uuid.UUID) (bool, error) {
	return m.valueOK, m.valueErr
}
func (m *mockPendingDB) EnqueueRetagJob(_ context.Context, _ uuid.UUID, kind string, spec json.RawMessage, actor uuid.NullUUID) (*db.RetagJob, error) {
	if m.enqueueErr != nil {
		return nil, m.enqueueErr
	}
	m.enqueuedKind = kind
	m.enqueuedSpec = spec
	m.enqueuedActor = actor
	return &db.RetagJob{ID: uuid.New(), Kind: kind, Status: "pending"}, nil
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
	g.POST("/:id/merge", superAdmin, h.Merge)
	g.POST("/:id/reject", superAdmin, h.Reject)
	return r
}

// pendingValueRow returns a queued value whose canonical "Tokyo" is the suggested
// match, used by the merge tests.
func pendingValueRow(id uuid.UUID) *db.PendingTagValue {
	return &db.PendingTagValue{
		ID: id, OrgID: testTagOrgID, TagKeyID: uuid.New(),
		ExtractedValue: "tokyo",
		SuggestedValue: sql.NullString{String: "Tokyo", Valid: true},
	}
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

func TestPending_Approve_RaceDeleted_404(t *testing.T) {
	id := uuid.New()
	// Row exists at resolve time but delete affects 0 rows (concurrently resolved).
	mockDB := &mockPendingDB{
		getRow:     &db.PendingTagValue{ID: id, OrgID: testTagOrgID, TagKeyID: uuid.New(), ExtractedValue: "tokyo"},
		deleteRows: 0,
	}
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/pending-tag-values/"+id.String()+"/approve", ""))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestPending_Reject_InvalidID(t *testing.T) {
	h := handler.NewPendingTagValuesHandler(&mockPendingDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/pending-tag-values/not-a-uuid/reject", ""))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func mergeReq(id, body string) *http.Request {
	return req(http.MethodPost, "/api/v1/pending-tag-values/"+id+"/merge", body)
}

func TestPending_Merge_Success_ExplicitTarget(t *testing.T) {
	id := uuid.New()
	mockDB := &mockPendingDB{getRow: pendingValueRow(id), valueOK: true}
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, mergeReq(id.String(), `{"into":"Tokyo"}`))
	assert.Equal(t, http.StatusAccepted, w.Code) // enqueued, runs async
	assert.Equal(t, "merge", mockDB.enqueuedKind)
	// Spec folds the queued value into the canonical target.
	var spec retag.MergeSpec
	require.NoError(t, json.Unmarshal(mockDB.enqueuedSpec, &spec))
	assert.Equal(t, "tokyo", spec.FromValue)
	assert.Equal(t, "Tokyo", spec.ToValue)
}

func TestPending_Merge_DefaultsToSuggested(t *testing.T) {
	id := uuid.New()
	mockDB := &mockPendingDB{getRow: pendingValueRow(id), valueOK: true}
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	// Empty body → fall back to suggested_value ("Tokyo").
	testPendingRouter(h, "super_admin").ServeHTTP(w, mergeReq(id.String(), ``))
	assert.Equal(t, http.StatusAccepted, w.Code)
	var spec retag.MergeSpec
	require.NoError(t, json.Unmarshal(mockDB.enqueuedSpec, &spec))
	assert.Equal(t, "Tokyo", spec.ToValue)
}

func TestPending_Merge_NoTargetNoSuggestion_400(t *testing.T) {
	id := uuid.New()
	row := pendingValueRow(id)
	row.SuggestedValue = sql.NullString{} // no suggestion, and no body target
	mockDB := &mockPendingDB{getRow: row}
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, mergeReq(id.String(), ``))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, mockDB.enqueuedKind) // nothing enqueued
}

func TestPending_Merge_TargetEqualsValue_400(t *testing.T) {
	id := uuid.New()
	mockDB := &mockPendingDB{getRow: pendingValueRow(id), valueOK: true}
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, mergeReq(id.String(), `{"into":"tokyo"}`))
	assert.Equal(t, http.StatusBadRequest, w.Code) // use approve, not merge
	assert.Empty(t, mockDB.enqueuedKind)
}

func TestPending_Merge_UnregisteredTarget_400(t *testing.T) {
	id := uuid.New()
	mockDB := &mockPendingDB{getRow: pendingValueRow(id), valueOK: false} // target not in vocab
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, mergeReq(id.String(), `{"into":"Osaka"}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, mockDB.enqueuedKind)
}

func TestPending_Merge_Forbidden_NonSuperAdmin(t *testing.T) {
	h := handler.NewPendingTagValuesHandler(&mockPendingDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "org_admin").ServeHTTP(w, mergeReq(uuid.New().String(), `{"into":"Tokyo"}`))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestPending_Merge_NotFound(t *testing.T) {
	mockDB := &mockPendingDB{getErr: sql.ErrNoRows}
	h := handler.NewPendingTagValuesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, mergeReq(uuid.New().String(), `{"into":"Tokyo"}`))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestPending_Merge_NilDB_501(t *testing.T) {
	h := handler.NewPendingTagValuesHandler(nil, newTestLogger())
	w := httptest.NewRecorder()
	testPendingRouter(h, "super_admin").ServeHTTP(w, mergeReq(uuid.New().String(), `{"into":"Tokyo"}`))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}
