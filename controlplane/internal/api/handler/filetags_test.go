package handler_test

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type mockFileTagsDB struct {
	entry       *db.FileEntry
	entryErr    error
	tagKeys     map[string]*db.TagKey
	oldValues   map[string]string
	valueExists map[string]bool
	similar     string

	setCalls     []string
	setValues    map[string]string
	deleteCalls  []string
	auditCalls   []db.CreateTagAuditParams
	pendingCalls []db.UpsertPendingTagValueManualParams
}

func (m *mockFileTagsDB) GetFileEntryByID(_ context.Context, _ uuid.UUID) (*db.FileEntry, error) {
	return m.entry, m.entryErr
}
func (m *mockFileTagsDB) GetTagKey(_ context.Context, _ uuid.UUID, key string) (*db.TagKey, error) {
	if k, ok := m.tagKeys[key]; ok {
		return k, nil
	}
	return nil, sql.ErrNoRows
}
func (m *mockFileTagsDB) GetFileTagValue(_ context.Context, _ uuid.UUID, key string) (string, error) {
	if v, ok := m.oldValues[key]; ok {
		return v, nil
	}
	return "", sql.ErrNoRows
}
func (m *mockFileTagsDB) SetFileTag(_ context.Context, _ uuid.UUID, key, value string, _ string) error {
	m.setCalls = append(m.setCalls, key)
	if m.setValues == nil {
		m.setValues = map[string]string{}
	}
	m.setValues[key] = value
	return nil
}
func (m *mockFileTagsDB) DeleteFileTag(_ context.Context, _ uuid.UUID, key string) (int64, error) {
	m.deleteCalls = append(m.deleteCalls, key)
	if _, ok := m.oldValues[key]; ok {
		return 1, nil
	}
	return 0, nil
}
func (m *mockFileTagsDB) TagValueExists(_ context.Context, _ uuid.UUID, value string) (bool, error) {
	return m.valueExists[value], nil
}
func (m *mockFileTagsDB) FindSimilarTagValue(_ context.Context, _ uuid.UUID, _ string) (string, error) {
	if m.similar == "" {
		return "", sql.ErrNoRows
	}
	return m.similar, nil
}
func (m *mockFileTagsDB) UpsertPendingTagValueManual(_ context.Context, arg db.UpsertPendingTagValueManualParams) error {
	m.pendingCalls = append(m.pendingCalls, arg)
	return nil
}
func (m *mockFileTagsDB) CreateTagAudit(_ context.Context, arg db.CreateTagAuditParams) error {
	m.auditCalls = append(m.auditCalls, arg)
	return nil
}
func (m *mockFileTagsDB) ListFileTagsByFileIDs(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]map[string]string, error) {
	return nil, nil
}

func fileInOrg() *db.FileEntry {
	return &db.FileEntry{ID: uuid.New(), OrgID: testTagOrgID}
}

func controlledKey() *db.TagKey {
	return &db.TagKey{ID: uuid.New(), Key: "site", ValueControlled: true, AllowPathVar: true}
}

func testFileTagsRouter(h *handler.FileTagsHandler, role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, role, testTagOrgID.String(), uuid.New().String())
		c.Next()
	})
	superAdmin := middleware.RequireRole("super_admin")
	r.PUT("/api/v1/files/:id/tags", superAdmin, h.SetTags)
	return r
}

func putTags(id, body string) *http.Request {
	return req(http.MethodPut, "/api/v1/files/"+id+"/tags", body)
}

func TestFileTags_Set_Success(t *testing.T) {
	entry := fileInOrg()
	mockDB := &mockFileTagsDB{
		entry:       entry,
		tagKeys:     map[string]*db.TagKey{"site": controlledKey()},
		valueExists: map[string]bool{"tokyo": true}, // registered → no queue
	}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{"site":"tokyo"}}`))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"site"}, mockDB.setCalls)
	require.Len(t, mockDB.auditCalls, 1)
	assert.Equal(t, "set", mockDB.auditCalls[0].Action)
	assert.Empty(t, mockDB.pendingCalls)
}

func TestFileTags_Set_UnregisteredValue_Queued(t *testing.T) {
	entry := fileInOrg()
	mockDB := &mockFileTagsDB{
		entry:       entry,
		tagKeys:     map[string]*db.TagKey{"site": controlledKey()},
		valueExists: map[string]bool{}, // "tokyo" not registered
		similar:     "Tokyo",
	}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{"site":"tokyo"}}`))
	assert.Equal(t, http.StatusOK, w.Code)
	require.Len(t, mockDB.pendingCalls, 1)
	assert.Equal(t, "tokyo", mockDB.pendingCalls[0].ExtractedValue)
	assert.Equal(t, "Tokyo", mockDB.pendingCalls[0].SuggestedValue.String)
	assert.Equal(t, "manual", mockDB.pendingCalls[0].Source)
}

func TestFileTags_Set_TrimsValue(t *testing.T) {
	// A padded value is trimmed before persisting/matching, matching the
	// controlled-vocabulary path (CreateValue trims): the registered "tokyo"
	// must be recognized and not queued as unregistered.
	entry := fileInOrg()
	mockDB := &mockFileTagsDB{
		entry:       entry,
		tagKeys:     map[string]*db.TagKey{"site": controlledKey()},
		valueExists: map[string]bool{"tokyo": true},
	}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{"site":"  tokyo  "}}`))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "tokyo", mockDB.setValues["site"])
	assert.Empty(t, mockDB.pendingCalls) // recognized as registered → not queued
}

func TestFileTags_Set_ValueTooLong_400(t *testing.T) {
	entry := fileInOrg()
	mockDB := &mockFileTagsDB{entry: entry, tagKeys: map[string]*db.TagKey{"site": controlledKey()}}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	long := strings.Repeat("x", 129) // 129 runes > maxManualTagValueLen (128)
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{"site":"`+long+`"}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, mockDB.setCalls) // rejected up front, nothing applied
}

func TestFileTags_Clear_Success(t *testing.T) {
	entry := fileInOrg()
	mockDB := &mockFileTagsDB{
		entry:     entry,
		oldValues: map[string]string{"site": "tokyo"}, // exists → clear + audit
	}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{"site":null}}`))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, []string{"site"}, mockDB.deleteCalls)
	require.Len(t, mockDB.auditCalls, 1)
	assert.Equal(t, "clear", mockDB.auditCalls[0].Action)
	assert.Equal(t, "tokyo", mockDB.auditCalls[0].OldValue.String)
}

func TestFileTags_Response_EmptyTagsIsObject(t *testing.T) {
	// ListFileTagsByFileIDs omits files without tags, so tagsByFile[id] is nil.
	// The response must still carry "tags":{} (a stable object), never null.
	entry := fileInOrg()
	mockDB := &mockFileTagsDB{entry: entry, oldValues: map[string]string{"site": "tokyo"}}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{"site":null}}`))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Contains(t, w.Body.String(), `"tags":{}`)
	assert.NotContains(t, w.Body.String(), `"tags":null`)
}

func TestFileTags_Clear_Absent_NoAudit(t *testing.T) {
	entry := fileInOrg()
	mockDB := &mockFileTagsDB{entry: entry, oldValues: map[string]string{}}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{"site":null}}`))
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Empty(t, mockDB.auditCalls) // nothing to clear → no audit
}

func TestFileTags_Forbidden_NonSuperAdmin(t *testing.T) {
	h := handler.NewFileTagsHandler(&mockFileTagsDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "org_admin").ServeHTTP(w, putTags(uuid.New().String(), `{"tags":{"site":"tokyo"}}`))
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestFileTags_FileNotFound(t *testing.T) {
	mockDB := &mockFileTagsDB{entryErr: sql.ErrNoRows}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(uuid.New().String(), `{"tags":{"site":"tokyo"}}`))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestFileTags_CrossOrg_404(t *testing.T) {
	entry := &db.FileEntry{ID: uuid.New(), OrgID: uuid.New()} // different org
	mockDB := &mockFileTagsDB{entry: entry}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{"site":"tokyo"}}`))
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestFileTags_UnregisteredKey_400(t *testing.T) {
	entry := fileInOrg()
	mockDB := &mockFileTagsDB{entry: entry, tagKeys: map[string]*db.TagKey{}} // "site" not registered
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{"site":"tokyo"}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Empty(t, mockDB.setCalls) // nothing applied when validation fails
}

func TestFileTags_InvalidKeyFormat_400(t *testing.T) {
	entry := fileInOrg()
	mockDB := &mockFileTagsDB{entry: entry}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{"Site!":"tokyo"}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestFileTags_EmptyValue_400(t *testing.T) {
	entry := fileInOrg()
	mockDB := &mockFileTagsDB{entry: entry, tagKeys: map[string]*db.TagKey{"site": controlledKey()}}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{"site":"  "}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestFileTags_EmptyTags_400(t *testing.T) {
	entry := fileInOrg()
	mockDB := &mockFileTagsDB{entry: entry}
	h := handler.NewFileTagsHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(entry.ID.String(), `{"tags":{}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestFileTags_InvalidID_400(t *testing.T) {
	h := handler.NewFileTagsHandler(&mockFileTagsDB{}, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags("not-a-uuid", `{"tags":{"site":"tokyo"}}`))
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestFileTags_NilDB_501(t *testing.T) {
	h := handler.NewFileTagsHandler(nil, newTestLogger())
	w := httptest.NewRecorder()
	testFileTagsRouter(h, "super_admin").ServeHTTP(w, putTags(uuid.New().String(), `{"tags":{"site":"tokyo"}}`))
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}
