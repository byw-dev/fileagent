package handler_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"github.com/stretchr/testify/assert"
)

// ── mock TagKeysDB ──────────────────────────────────────────────────────────────

type mockTagKeysDB struct {
	keys          []*db.TagKey
	getKey        *db.TagKey
	getErr        error
	createErr     error
	updateErr     error
	deleteRows    int64
	deleteErr     error
	values        []*db.TagValue
	createVal     *db.TagValue
	createValErr  error
	deleteValRows int64
	deleteValErr  error

	lastCreate db.CreateTagKeyParams
	lastUpdate db.UpdateTagKeyParams
}

func (m *mockTagKeysDB) ListTagKeys(_ context.Context, _ uuid.UUID) ([]*db.TagKey, error) {
	return m.keys, nil
}
func (m *mockTagKeysDB) GetTagKey(_ context.Context, _ uuid.UUID, _ string) (*db.TagKey, error) {
	return m.getKey, m.getErr
}
func (m *mockTagKeysDB) CreateTagKey(_ context.Context, arg db.CreateTagKeyParams) (*db.TagKey, error) {
	m.lastCreate = arg
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &db.TagKey{ID: uuid.New(), Key: arg.Key, Label: arg.Label,
		ValueControlled: arg.ValueControlled, RequiredAtCollection: arg.RequiredAtCollection,
		AllowPathVar: arg.AllowPathVar, CreatedAt: time.Now()}, nil
}
func (m *mockTagKeysDB) UpdateTagKey(_ context.Context, arg db.UpdateTagKeyParams) (*db.TagKey, error) {
	m.lastUpdate = arg
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	return &db.TagKey{ID: uuid.New(), Key: arg.Key, Label: arg.Label,
		ValueControlled: arg.ValueControlled, RequiredAtCollection: arg.RequiredAtCollection,
		AllowPathVar: arg.AllowPathVar, CreatedAt: time.Now()}, nil
}
func (m *mockTagKeysDB) DeleteTagKey(_ context.Context, _ uuid.UUID, _ string) (int64, error) {
	return m.deleteRows, m.deleteErr
}
func (m *mockTagKeysDB) ListTagValues(_ context.Context, _ uuid.UUID) ([]*db.TagValue, error) {
	return m.values, nil
}
func (m *mockTagKeysDB) CreateTagValue(_ context.Context, tagKeyID uuid.UUID, value string) (*db.TagValue, error) {
	if m.createValErr != nil {
		return nil, m.createValErr
	}
	if m.createVal != nil {
		return m.createVal, nil
	}
	return &db.TagValue{ID: uuid.New(), TagKeyID: tagKeyID, Value: value, CreatedAt: time.Now()}, nil
}
func (m *mockTagKeysDB) DeleteTagValue(_ context.Context, _ uuid.UUID, _ uuid.UUID) (int64, error) {
	return m.deleteValRows, m.deleteValErr
}

// ── test router ─────────────────────────────────────────────────────────────────

var testTagOrgID = uuid.New()

func testTagKeysRouter(h *handler.TagKeysHandler, role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, role, testTagOrgID.String(), uuid.New().String())
		c.Next()
	})
	superAdmin := middleware.RequireRole("super_admin")
	g := r.Group("/api/v1/tag-keys")
	g.GET("", h.List)
	g.POST("", superAdmin, h.Create)
	g.PATCH("/:key", superAdmin, h.Update)
	g.DELETE("/:key", superAdmin, h.Delete)
	g.GET("/:key/values", h.ListValues)
	g.POST("/:key/values", superAdmin, h.CreateValue)
	g.DELETE("/:key/values/:vid", superAdmin, h.DeleteValue)
	return r
}

func req(method, path, body string) *http.Request {
	var r *http.Request
	if body != "" {
		r, _ = http.NewRequest(method, path, strings.NewReader(body))
		r.Header.Set("Content-Type", "application/json")
	} else {
		r, _ = http.NewRequest(method, path, nil)
	}
	return r
}

func uniqueViolation() error { return &pq.Error{Code: "23505"} }

// ── tests ─────────────────────────────────────────────────────────────────────

func TestTagKeys_List_Success(t *testing.T) {
	mockDB := &mockTagKeysDB{keys: []*db.TagKey{{ID: uuid.New(), Key: "site", Label: "Site", CreatedAt: time.Now()}}}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "org_viewer").ServeHTTP(w, req(http.MethodGet, "/api/v1/tag-keys", ""))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestTagKeys_Create_Success_AppliesDefaults(t *testing.T) {
	mockDB := &mockTagKeysDB{}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	// value_controlled/allow_path_var omitted → schema defaults (true).
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/tag-keys", `{"key":"site","label":"Site"}`))
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.True(t, mockDB.lastCreate.ValueControlled)
	assert.True(t, mockDB.lastCreate.AllowPathVar)
	assert.False(t, mockDB.lastCreate.RequiredAtCollection)
}

func TestTagKeys_Create_Forbidden_NonSuperAdmin(t *testing.T) {
	h := handler.NewTagKeysHandler(&mockTagKeysDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "org_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/tag-keys", `{"key":"site","label":"Site"}`))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestTagKeys_Create_InvalidKey(t *testing.T) {
	h := handler.NewTagKeysHandler(&mockTagKeysDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/tag-keys", `{"key":"Site!","label":"Site"}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTagKeys_Create_Duplicate_Conflict(t *testing.T) {
	h := handler.NewTagKeysHandler(&mockTagKeysDB{createErr: uniqueViolation()}, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/tag-keys", `{"key":"site","label":"Site"}`))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestTagKeys_Update_PartialKeepsFlags(t *testing.T) {
	existing := &db.TagKey{ID: uuid.New(), Key: "site", Label: "Old", ValueControlled: true, AllowPathVar: false}
	mockDB := &mockTagKeysDB{getKey: existing}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	// Only label provided; flags must retain existing values.
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPatch, "/api/v1/tag-keys/site", `{"label":"New"}`))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "New", mockDB.lastUpdate.Label)
	assert.True(t, mockDB.lastUpdate.ValueControlled)
	assert.False(t, mockDB.lastUpdate.AllowPathVar)
}

func TestTagKeys_Update_NotFound(t *testing.T) {
	mockDB := &mockTagKeysDB{getErr: sql.ErrNoRows}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPatch, "/api/v1/tag-keys/nope", `{"label":"x"}`))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTagKeys_Delete_Success(t *testing.T) {
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site"}, deleteRows: 1}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodDelete, "/api/v1/tag-keys/site", ""))
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestTagKeys_Delete_SystemReserved_Conflict(t *testing.T) {
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "level", SystemReserved: true}}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodDelete, "/api/v1/tag-keys/level", ""))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestTagKeys_CreateValue_Success(t *testing.T) {
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site"}}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/tag-keys/site/values", `{"value":"tokyo"}`))
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestTagKeys_CreateValue_Duplicate_Conflict(t *testing.T) {
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site"}, createValErr: uniqueViolation()}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/tag-keys/site/values", `{"value":"tokyo"}`))
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestTagKeys_DeleteValue_NotFound(t *testing.T) {
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site"}, deleteValRows: 0}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodDelete, "/api/v1/tag-keys/site/values/"+uuid.New().String(), ""))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTagKeys_List_NilDB_Returns501(t *testing.T) {
	h := handler.NewTagKeysHandler(nil, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodGet, "/api/v1/tag-keys", ""))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestTagKeys_Create_DBError(t *testing.T) {
	h := handler.NewTagKeysHandler(&mockTagKeysDB{createErr: assert.AnError}, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/tag-keys", `{"key":"site","label":"Site"}`))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestTagKeys_ListValues_KeyNotFound(t *testing.T) {
	h := handler.NewTagKeysHandler(&mockTagKeysDB{getErr: sql.ErrNoRows}, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "org_viewer").ServeHTTP(w, req(http.MethodGet, "/api/v1/tag-keys/site/values", ""))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTagKeys_ListValues_Success(t *testing.T) {
	mockDB := &mockTagKeysDB{
		getKey: &db.TagKey{ID: uuid.New(), Key: "site"},
		values: []*db.TagValue{{ID: uuid.New(), Value: "tokyo"}},
	}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "org_viewer").ServeHTTP(w, req(http.MethodGet, "/api/v1/tag-keys/site/values", ""))
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestTagKeys_DeleteValue_Success(t *testing.T) {
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site"}, deleteValRows: 1}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodDelete, "/api/v1/tag-keys/site/values/"+uuid.New().String(), ""))
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestTagKeys_Update_DBError(t *testing.T) {
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site"}, updateErr: assert.AnError}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPatch, "/api/v1/tag-keys/site", `{"label":"x"}`))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestTagKeys_Delete_DBError(t *testing.T) {
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site"}, deleteErr: assert.AnError}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodDelete, "/api/v1/tag-keys/site", ""))
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestTagKeys_CreateValue_EmptyValue(t *testing.T) {
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site"}}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/tag-keys/site/values", `{"value":"  "}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTagKeys_DeleteValue_InvalidID(t *testing.T) {
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site"}}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodDelete, "/api/v1/tag-keys/site/values/not-a-uuid", ""))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTagKeys_Update_EmptyLabel(t *testing.T) {
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site", Label: "Old"}}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPatch, "/api/v1/tag-keys/site", `{"label":""}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTagKeys_Create_WhitespaceLabel_Rejected(t *testing.T) {
	h := handler.NewTagKeysHandler(&mockTagKeysDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/tag-keys", `{"key":"site","label":"   "}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTagKeys_Update_InvalidKeyParam_400(t *testing.T) {
	// Invalid :key must 400 up front, before any DB lookup.
	mockDB := &mockTagKeysDB{getErr: sql.ErrNoRows}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPatch, "/api/v1/tag-keys/Bad!", `{"label":"x"}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTagKeys_Delete_InvalidKeyParam_400(t *testing.T) {
	h := handler.NewTagKeysHandler(&mockTagKeysDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodDelete, "/api/v1/tag-keys/Bad!", ""))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTagKeys_Values_InvalidKeyParam_400(t *testing.T) {
	h := handler.NewTagKeysHandler(&mockTagKeysDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPost, "/api/v1/tag-keys/Bad!/values", `{"value":"x"}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestTagKeys_Update_RaceDeleted_404(t *testing.T) {
	// Key exists at check time but UpdateTagKey finds no row (deleted in between).
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site", Label: "Old"}, updateErr: sql.ErrNoRows}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodPatch, "/api/v1/tag-keys/site", `{"label":"New"}`))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestTagKeys_Delete_RaceZeroRows_404(t *testing.T) {
	// Key exists at check time but DeleteTagKey affects 0 rows (deleted in between).
	mockDB := &mockTagKeysDB{getKey: &db.TagKey{ID: uuid.New(), Key: "site"}, deleteRows: 0}
	h := handler.NewTagKeysHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testTagKeysRouter(h, "super_admin").ServeHTTP(w, req(http.MethodDelete, "/api/v1/tag-keys/site", ""))
	assert.Equal(t, http.StatusNotFound, w.Code)
}
