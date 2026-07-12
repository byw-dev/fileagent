package handler_test

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/retag"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockBatchTagDB struct {
	tagKeys     map[string]*db.TagKey
	valueExists map[string]bool
	similar     string

	pendingCalls []db.UpsertPendingTagValueManualParams
	enqueuedKind string
	enqueuedSpec json.RawMessage
	enqueueErr   error
}

func (m *mockBatchTagDB) GetTagKey(_ context.Context, _ uuid.UUID, key string) (*db.TagKey, error) {
	if k, ok := m.tagKeys[key]; ok {
		return k, nil
	}
	return nil, sql.ErrNoRows
}
func (m *mockBatchTagDB) TagValueExists(_ context.Context, _ uuid.UUID, value string) (bool, error) {
	return m.valueExists[value], nil
}
func (m *mockBatchTagDB) FindSimilarTagValue(_ context.Context, _ uuid.UUID, _ string) (string, error) {
	if m.similar == "" {
		return "", sql.ErrNoRows
	}
	return m.similar, nil
}
func (m *mockBatchTagDB) UpsertPendingTagValueManual(_ context.Context, arg db.UpsertPendingTagValueManualParams) error {
	m.pendingCalls = append(m.pendingCalls, arg)
	return nil
}
func (m *mockBatchTagDB) EnqueueRetagJob(_ context.Context, _ uuid.UUID, kind string, spec json.RawMessage, _ uuid.NullUUID) (*db.RetagJob, error) {
	if m.enqueueErr != nil {
		return nil, m.enqueueErr
	}
	m.enqueuedKind = kind
	m.enqueuedSpec = spec
	return &db.RetagJob{ID: uuid.New(), Kind: kind, Status: "pending"}, nil
}

func testBatchTagRouter(h *handler.BatchTagHandler, role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, role, testTagOrgID.String(), uuid.New().String())
		c.Next()
	})
	superAdmin := middleware.RequireRole("super_admin")
	r.POST("/api/v1/files/batch-tag", superAdmin, h.Submit)
	return r
}

func batchTag(body string) *http.Request {
	return req(http.MethodPost, "/api/v1/files/batch-tag", body)
}

func TestBatchTag_Success_Enqueued(t *testing.T) {
	mockDB := &mockBatchTagDB{
		tagKeys:     map[string]*db.TagKey{"vendor": {ID: uuid.New(), Key: "vendor", ValueControlled: true}},
		valueExists: map[string]bool{"omron": true}, // registered → no queue
	}
	h := handler.NewBatchTagHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	body := `{"filter":{"status":"completed","tags":["site:tokyo"]},"tags":{"vendor":"omron"}}`
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(body))
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Equal(t, "batch_tag", mockDB.enqueuedKind)
	assert.Empty(t, mockDB.pendingCalls)
	// Spec carries the filter + trimmed set value.
	var spec retag.BatchTagSpec
	require.NoError(t, json.Unmarshal(mockDB.enqueuedSpec, &spec))
	assert.Equal(t, "completed", spec.Filter.Status)
	require.Len(t, spec.Filter.Tags, 1)
	assert.Equal(t, "site", spec.Filter.Tags[0].Key)
	require.NotNil(t, spec.Tags["vendor"])
	assert.Equal(t, "omron", *spec.Tags["vendor"])
}

func TestBatchTag_TrimsValue_AndClear(t *testing.T) {
	mockDB := &mockBatchTagDB{
		tagKeys:     map[string]*db.TagKey{"vendor": {ID: uuid.New(), Key: "vendor", ValueControlled: true}},
		valueExists: map[string]bool{"omron": true},
	}
	h := handler.NewBatchTagHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{},"tags":{"vendor":"  omron  ","obsolete":null}}`))
	assert.Equal(t, http.StatusAccepted, w.Code)
	var spec retag.BatchTagSpec
	require.NoError(t, json.Unmarshal(mockDB.enqueuedSpec, &spec))
	assert.Equal(t, "omron", *spec.Tags["vendor"]) // trimmed
	require.Contains(t, spec.Tags, "obsolete")
	assert.Nil(t, spec.Tags["obsolete"]) // clear
}

func TestBatchTag_UnregisteredValue_Queued(t *testing.T) {
	mockDB := &mockBatchTagDB{
		tagKeys:     map[string]*db.TagKey{"vendor": {ID: uuid.New(), Key: "vendor", ValueControlled: true}},
		valueExists: map[string]bool{}, // "omron" not registered
		similar:     "Omron",
	}
	h := handler.NewBatchTagHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{},"tags":{"vendor":"omron"}}`))
	assert.Equal(t, http.StatusAccepted, w.Code) // still applied, just also queued
	require.Len(t, mockDB.pendingCalls, 1)
	assert.Equal(t, "omron", mockDB.pendingCalls[0].ExtractedValue)
	assert.Equal(t, "Omron", mockDB.pendingCalls[0].SuggestedValue.String)
	assert.Equal(t, "batch_tag", mockDB.enqueuedKind)
}

func TestBatchTag_EmptyTags_400(t *testing.T) {
	h := handler.NewBatchTagHandler(&mockBatchTagDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{},"tags":{}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBatchTag_UnknownKey_400(t *testing.T) {
	mockDB := &mockBatchTagDB{tagKeys: map[string]*db.TagKey{}} // "vendor" not registered
	h := handler.NewBatchTagHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{},"tags":{"vendor":"omron"}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, mockDB.enqueuedKind)
}

func TestBatchTag_InvalidKeyFormat_400(t *testing.T) {
	h := handler.NewBatchTagHandler(&mockBatchTagDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{},"tags":{"Vendor!":"omron"}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBatchTag_EmptyValue_400(t *testing.T) {
	mockDB := &mockBatchTagDB{tagKeys: map[string]*db.TagKey{"vendor": {ID: uuid.New(), Key: "vendor"}}}
	h := handler.NewBatchTagHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{},"tags":{"vendor":"   "}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBatchTag_BadFilterUUID_400(t *testing.T) {
	mockDB := &mockBatchTagDB{tagKeys: map[string]*db.TagKey{"vendor": {ID: uuid.New(), Key: "vendor"}}}
	h := handler.NewBatchTagHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{"agent_id":"nope"},"tags":{"vendor":"omron"}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, mockDB.enqueuedKind)
}

func TestBatchTag_BadFilterTagPredicate_400(t *testing.T) {
	mockDB := &mockBatchTagDB{tagKeys: map[string]*db.TagKey{"vendor": {ID: uuid.New(), Key: "vendor"}}}
	h := handler.NewBatchTagHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	// filter tag predicate missing ':' → 400
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{"tags":["site"]},"tags":{"vendor":"omron"}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBatchTag_InvalidStatus_400(t *testing.T) {
	// An unknown status is rejected up front rather than failing async post-202.
	mockDB := &mockBatchTagDB{tagKeys: map[string]*db.TagKey{"vendor": {ID: uuid.New(), Key: "vendor"}}}
	h := handler.NewBatchTagHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{"status":"bogus"},"tags":{"vendor":"omron"}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, mockDB.enqueuedKind)
}

func TestBatchTag_Forbidden_NonSuperAdmin(t *testing.T) {
	h := handler.NewBatchTagHandler(&mockBatchTagDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "org_admin").ServeHTTP(w, batchTag(`{"filter":{},"tags":{"vendor":"omron"}}`))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestBatchTag_UncontrolledKey_NotQueued(t *testing.T) {
	// A non-value-controlled key never queues a pending value, whatever the value.
	mockDB := &mockBatchTagDB{
		tagKeys: map[string]*db.TagKey{"note": {ID: uuid.New(), Key: "note", ValueControlled: false}},
	}
	h := handler.NewBatchTagHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{},"tags":{"note":"anything"}}`))
	assert.Equal(t, http.StatusAccepted, w.Code)
	assert.Empty(t, mockDB.pendingCalls)
}

func TestBatchTag_InvalidJSON_400(t *testing.T) {
	h := handler.NewBatchTagHandler(&mockBatchTagDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"tags":`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBatchTag_EnqueueError_500(t *testing.T) {
	mockDB := &mockBatchTagDB{
		tagKeys:     map[string]*db.TagKey{"vendor": {ID: uuid.New(), Key: "vendor", ValueControlled: true}},
		valueExists: map[string]bool{"omron": true},
		enqueueErr:  sql.ErrConnDone,
	}
	h := handler.NewBatchTagHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{},"tags":{"vendor":"omron"}}`))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestBatchTag_NilDB_501(t *testing.T) {
	h := handler.NewBatchTagHandler(nil, newTestLogger())
	w := httptest.NewRecorder()
	testBatchTagRouter(h, "super_admin").ServeHTTP(w, batchTag(`{"filter":{},"tags":{"vendor":"omron"}}`))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}
