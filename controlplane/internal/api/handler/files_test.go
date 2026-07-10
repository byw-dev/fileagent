package handler_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── mock FileTypesDB ──────────────────────────────────────────────────────────

type mockFileTypesDB struct {
	types     []*db.FileType
	listErr   error
	getType   *db.FileType
	getErr    error
	createErr error
	updateErr error
	deleteErr error
}

func (m *mockFileTypesDB) ListFileTypes(_ context.Context, _ uuid.UUID) ([]*db.FileType, error) {
	return m.types, m.listErr
}
func (m *mockFileTypesDB) GetFileTypeByID(_ context.Context, _ uuid.UUID) (*db.FileType, error) {
	return m.getType, m.getErr
}
func (m *mockFileTypesDB) CreateFileType(_ context.Context, orgID uuid.UUID, name string, description sql.NullString, createdBy uuid.NullUUID) (*db.FileType, error) {
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &db.FileType{
		ID:          uuid.New(),
		OrgID:       orgID,
		Name:        name,
		Description: description,
		CreatedAt:   time.Now(),
	}, nil
}
func (m *mockFileTypesDB) UpdateFileType(_ context.Context, id uuid.UUID, name string, description sql.NullString) (*db.FileType, error) {
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	return &db.FileType{ID: id, OrgID: uuid.New(), Name: name, Description: description, CreatedAt: time.Now()}, nil
}
func (m *mockFileTypesDB) DeleteFileType(_ context.Context, _ uuid.UUID) error { return m.deleteErr }

// ── mock FilesDB ──────────────────────────────────────────────────────────────

type mockFilesDB struct {
	entries    []*db.FileEntry
	listErr    error
	countTotal int64
	countErr   error
	entry      *db.FileEntry
	getErr     error
	bucket     *db.Bucket
	bucketErr  error
	tagsByFile map[uuid.UUID]map[string]string
	tagsErr    error
	lastList   db.ListFileEntriesParams
}

func (m *mockFilesDB) ListFileEntries(_ context.Context, arg db.ListFileEntriesParams) ([]*db.FileEntry, error) {
	m.lastList = arg
	return m.entries, m.listErr
}
func (m *mockFilesDB) CountFileEntries(_ context.Context, _ db.CountFileEntriesFilter) (int64, error) {
	return m.countTotal, m.countErr
}
func (m *mockFilesDB) GetFileEntryByID(_ context.Context, _ uuid.UUID) (*db.FileEntry, error) {
	return m.entry, m.getErr
}
func (m *mockFilesDB) GetBucketByID(_ context.Context, _ uuid.UUID) (*db.Bucket, error) {
	if m.bucketErr != nil {
		return nil, m.bucketErr
	}
	if m.bucket != nil {
		return m.bucket, nil
	}
	return &db.Bucket{ID: uuid.New(), Name: "data-sensor"}, nil
}
func (m *mockFilesDB) ListFileTagsByFileIDs(_ context.Context, _ []uuid.UUID) (map[uuid.UUID]map[string]string, error) {
	return m.tagsByFile, m.tagsErr
}

// ── mock MinIOPresigner ───────────────────────────────────────────────────────

type mockPresigner struct {
	url string
	err error
}

func (m *mockPresigner) PresignedGetObject(_ context.Context, _, _ string, _ time.Duration) (string, error) {
	return m.url, m.err
}

// ── FileTypesHandler router ───────────────────────────────────────────────────

func testFileTypesRouter(h *handler.FileTypesHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	orgID := uuid.New().String()
	userID := uuid.New().String()
	r.Use(func(c *gin.Context) {
		injectClaims(c, "super_admin", orgID, userID)
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.GET("/file-types", h.List)
	v1.POST("/file-types", h.Create)
	v1.PUT("/file-types/:id", h.Update)
	v1.DELETE("/file-types/:id", h.Delete)
	return r
}

// ── FileTypesHandler tests ────────────────────────────────────────────────────

func TestFileTypesHandler_List_Success(t *testing.T) {
	mockDB := &mockFileTypesDB{types: []*db.FileType{
		{ID: uuid.New(), OrgID: uuid.New(), Name: "logs", CreatedAt: time.Now()},
	}}
	h := handler.NewFileTypesHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/file-types", nil)
	testFileTypesRouter(h).ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	data := body["items"].([]interface{})
	assert.Len(t, data, 1)
}

func TestFileTypesHandler_List_DBError(t *testing.T) {
	h := handler.NewFileTypesHandler(&mockFileTypesDB{listErr: assert.AnError}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/file-types", nil)
	testFileTypesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestFileTypesHandler_List_NilDB_Returns501(t *testing.T) {
	h := handler.NewFileTypesHandler(nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/file-types", nil)
	testFileTypesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestFileTypesHandler_Create_Success(t *testing.T) {
	h := handler.NewFileTypesHandler(&mockFileTypesDB{}, newTestLogger())
	body := `{"name":"images","description":"Image files"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/file-types", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testFileTypesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestFileTypesHandler_Create_MissingName(t *testing.T) {
	h := handler.NewFileTypesHandler(&mockFileTypesDB{}, newTestLogger())
	body := `{"description":"no name"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/file-types", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testFileTypesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestFileTypesHandler_Create_DBError(t *testing.T) {
	h := handler.NewFileTypesHandler(&mockFileTypesDB{createErr: assert.AnError}, newTestLogger())
	body := `{"name":"logs"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/file-types", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testFileTypesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestFileTypesHandler_Update_Success(t *testing.T) {
	h := handler.NewFileTypesHandler(&mockFileTypesDB{}, newTestLogger())
	body := `{"name":"renamed"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/file-types/"+uuid.New().String(), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testFileTypesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestFileTypesHandler_Update_InvalidID(t *testing.T) {
	h := handler.NewFileTypesHandler(&mockFileTypesDB{}, newTestLogger())
	body := `{"name":"renamed"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/file-types/bad-id", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testFileTypesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestFileTypesHandler_Update_NotFound(t *testing.T) {
	h := handler.NewFileTypesHandler(&mockFileTypesDB{updateErr: sql.ErrNoRows}, newTestLogger())
	body := `{"name":"renamed"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/file-types/"+uuid.New().String(), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testFileTypesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestFileTypesHandler_Delete_Success(t *testing.T) {
	h := handler.NewFileTypesHandler(&mockFileTypesDB{}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/v1/file-types/"+uuid.New().String(), nil)
	testFileTypesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestFileTypesHandler_Delete_InvalidID(t *testing.T) {
	h := handler.NewFileTypesHandler(&mockFileTypesDB{}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/v1/file-types/bad-id", nil)
	testFileTypesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestFileTypesHandler_Delete_DBError(t *testing.T) {
	h := handler.NewFileTypesHandler(&mockFileTypesDB{deleteErr: assert.AnError}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/v1/file-types/"+uuid.New().String(), nil)
	testFileTypesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ── FilesHandler tests ────────────────────────────────────────────────────────

// testFilesOrgID is the caller org injected by testFilesRouter; sample entries
// share it so org-ownership checks pass on the success paths.
var testFilesOrgID = uuid.New()

func testFilesRouter(h *handler.FilesHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(middleware.RequestID())
	r.Use(func(c *gin.Context) {
		injectClaims(c, "org_admin", testFilesOrgID.String(), uuid.New().String())
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.GET("/files", h.List)
	v1.GET("/files/:id", h.Get)
	v1.GET("/files/:id/download-url", h.DownloadURL)
	v1.POST("/files/batch-download-urls", h.BatchDownloadURLs)
	return r
}

func newSampleEntry() *db.FileEntry {
	return &db.FileEntry{
		ID:           uuid.New(),
		OrgID:        testFilesOrgID,
		AgentID:      uuid.NullUUID{UUID: uuid.New(), Valid: true},
		BucketID:     uuid.New(),
		StoragePath:  "uploads/file.txt",
		OriginalPath: sql.NullString{String: "/data/file.txt", Valid: true},
		FileName:     "file.txt",
		SizeBytes:    1024,
		Status:       db.FileStatusCompleted,
		CreatedAt:    time.Now(),
		UpdatedAt:    time.Now(),
	}
}

func TestFilesHandler_List_Success(t *testing.T) {
	mockDB := &mockFilesDB{entries: []*db.FileEntry{newSampleEntry()}, countTotal: 1}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, float64(1), body["total"])
	assert.Equal(t, false, body["has_more"])
	assert.Len(t, body["items"].([]interface{}), 1)
}

func TestFilesHandler_List_HasMore(t *testing.T) {
	// Simulate limit=1 and 2 entries returned (limit+1), so has_more=true.
	entries := []*db.FileEntry{newSampleEntry(), newSampleEntry()}
	mockDB := &mockFilesDB{entries: entries, countTotal: 5}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	// limit=1 → fetch 2, detect has_more
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files?limit=1", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, true, body["has_more"])
	assert.Equal(t, float64(5), body["total"])
	assert.NotEmpty(t, body["next_cursor"])
	assert.Len(t, body["items"].([]interface{}), 1)
}

func TestFilesHandler_List_CountDBError(t *testing.T) {
	mockDB := &mockFilesDB{entries: []*db.FileEntry{newSampleEntry()}, countErr: assert.AnError}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestFilesHandler_List_NilDB_Returns501(t *testing.T) {
	h := handler.NewFilesHandler(nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestFilesHandler_List_InvalidCursor(t *testing.T) {
	h := handler.NewFilesHandler(&mockFilesDB{}, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files?cursor=!!!invalid!!!", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestFilesHandler_List_UnknownQueryParam(t *testing.T) {
	// A mistyped filter must return 400 INVALID_QUERY_PARAM instead of a
	// silent 200 with the filter ignored (CC-5).
	mockDB := &mockFilesDB{entries: []*db.FileEntry{newSampleEntry()}, countTotal: 1}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files?file_type_name=foo", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "INVALID_QUERY_PARAM", body["error"].(map[string]interface{})["code"])
	assert.NotEmpty(t, body["request_id"])
}

func TestFilesHandler_List_KnownFiltersAccepted(t *testing.T) {
	// All documented filters must pass the unknown-param guard.
	mockDB := &mockFilesDB{entries: []*db.FileEntry{newSampleEntry()}, countTotal: 1}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	url := "/api/v1/files?agent_id=" + uuid.NewString() +
		"&bucket_id=" + uuid.NewString() +
		"&file_type_id=" + uuid.NewString() +
		"&status=completed&limit=10"
	req, _ := http.NewRequest(http.MethodGet, url, nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestFilesHandler_Get_Success(t *testing.T) {
	entry := newSampleEntry()
	h := handler.NewFilesHandler(&mockFilesDB{entry: entry}, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files/"+entry.ID.String(), nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestFilesHandler_Get_NotFound(t *testing.T) {
	h := handler.NewFilesHandler(&mockFilesDB{getErr: sql.ErrNoRows}, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files/"+uuid.New().String(), nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestFilesHandler_Get_CrossOrg_Returns404(t *testing.T) {
	entry := newSampleEntry()
	entry.OrgID = uuid.New() // belongs to a different org than the caller
	h := handler.NewFilesHandler(&mockFilesDB{entry: entry}, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files/"+entry.ID.String(), nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestFilesHandler_DownloadURL_CrossOrg_Returns404(t *testing.T) {
	entry := newSampleEntry()
	entry.OrgID = uuid.New()
	presigner := &mockPresigner{url: "https://minio/presigned"}
	h := handler.NewFilesHandler(&mockFilesDB{entry: entry}, presigner, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files/"+entry.ID.String()+"/download-url", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestFilesHandler_DownloadURL_Success(t *testing.T) {
	entry := newSampleEntry()
	presigner := &mockPresigner{url: "https://minio/presigned"}
	h := handler.NewFilesHandler(&mockFilesDB{entry: entry}, presigner, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files/"+entry.ID.String()+"/download-url", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "https://minio/presigned", body["url"])
}

func TestFilesHandler_DownloadURL_NilMinio_Returns501(t *testing.T) {
	entry := newSampleEntry()
	h := handler.NewFilesHandler(&mockFilesDB{entry: entry}, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files/"+entry.ID.String()+"/download-url", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestFilesHandler_BatchDownloadURLs_Success(t *testing.T) {
	entry := newSampleEntry()
	presigner := &mockPresigner{url: "https://minio/presigned"}
	h := handler.NewFilesHandler(&mockFilesDB{entry: entry}, presigner, newTestLogger())
	body := `{"ids":["` + entry.ID.String() + `"]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/files/batch-download-urls", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestFilesHandler_BatchDownloadURLs_InvalidID(t *testing.T) {
	presigner := &mockPresigner{url: "https://minio/presigned"}
	h := handler.NewFilesHandler(&mockFilesDB{}, presigner, newTestLogger())
	body := `{"ids":["not-a-uuid"]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/files/batch-download-urls", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testFilesRouter(h).ServeHTTP(w, req)
	// Returns 200 with per-item error.
	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	items := resp["data"].([]interface{})
	item := items[0].(map[string]interface{})
	assert.Equal(t, "invalid id", item["error"])
}

func TestFilesHandler_DownloadURL_BucketNotFound(t *testing.T) {
	entry := newSampleEntry()
	mockDB := &mockFilesDB{entry: entry, bucketErr: assert.AnError}
	presigner := &mockPresigner{url: "https://minio/presigned"}
	h := handler.NewFilesHandler(mockDB, presigner, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files/"+entry.ID.String()+"/download-url", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestFilesHandler_DownloadURL_UsesBucketName(t *testing.T) {
	entry := newSampleEntry()
	bucketName := "my-real-bucket"
	var capturedBucket string
	mockDB := &mockFilesDB{
		entry:  entry,
		bucket: &db.Bucket{ID: entry.BucketID, Name: bucketName},
	}
	capturePresigner := &capturingPresigner{url: "https://minio/presigned"}
	h := handler.NewFilesHandler(mockDB, capturePresigner, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files/"+entry.ID.String()+"/download-url", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	capturedBucket = capturePresigner.lastBucket
	assert.Equal(t, bucketName, capturedBucket)
	// expires_in should be 900 (15 minutes).
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, float64(900), body["expires_in"])
}

func TestFilesHandler_DownloadURL_PresignError(t *testing.T) {
	entry := newSampleEntry()
	mockDB := &mockFilesDB{entry: entry}
	h := handler.NewFilesHandler(mockDB, &mockPresigner{err: assert.AnError}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files/"+entry.ID.String()+"/download-url", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestFilesHandler_BatchDownloadURLs_BucketNotFound(t *testing.T) {
	entry := newSampleEntry()
	mockDB := &mockFilesDB{entry: entry, bucketErr: assert.AnError}
	presigner := &mockPresigner{url: "https://minio/presigned"}
	h := handler.NewFilesHandler(mockDB, presigner, newTestLogger())
	body := `{"ids":["` + entry.ID.String() + `"]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/files/batch-download-urls", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	items := resp["data"].([]interface{})
	assert.Equal(t, "bucket not found", items[0].(map[string]interface{})["error"])
}

// capturingPresigner records the bucket name used in the last presign call.
type capturingPresigner struct {
	url        string
	lastBucket string
}

func (m *capturingPresigner) PresignedGetObject(_ context.Context, bucket, _ string, _ time.Duration) (string, error) {
	m.lastBucket = bucket
	return m.url, nil
}

// ── MT-2: file tags ────────────────────────────────────────────────────────────

func TestFilesHandler_List_TagsInResponse(t *testing.T) {
	entry := newSampleEntry()
	mockDB := &mockFilesDB{
		entries:    []*db.FileEntry{entry},
		countTotal: 1,
		tagsByFile: map[uuid.UUID]map[string]string{
			entry.ID: {"vendor": "omron", "site": "tokyo"},
		},
	}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files", nil)
	testFilesRouter(h).ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	item := body["items"].([]interface{})[0].(map[string]interface{})
	tags := item["tags"].(map[string]interface{})
	assert.Equal(t, "omron", tags["vendor"])
	assert.Equal(t, "tokyo", tags["site"])
}

func TestFilesHandler_List_TagFilterParsed(t *testing.T) {
	mockDB := &mockFilesDB{entries: []*db.FileEntry{newSampleEntry()}, countTotal: 1}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files?tag=site:tokyo&tag=level:raw", nil)
	testFilesRouter(h).ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.Len(t, mockDB.lastList.Tags, 2)
	assert.Equal(t, db.FileTagFilter{Key: "site", Value: "tokyo"}, mockDB.lastList.Tags[0])
	assert.Equal(t, db.FileTagFilter{Key: "level", Value: "raw"}, mockDB.lastList.Tags[1])
}

func TestFilesHandler_List_MalformedTag_Returns400(t *testing.T) {
	mockDB := &mockFilesDB{entries: []*db.FileEntry{newSampleEntry()}, countTotal: 1}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files?tag=novalue", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestFilesHandler_List_DuplicateExactTag_Collapsed(t *testing.T) {
	mockDB := &mockFilesDB{entries: []*db.FileEntry{newSampleEntry()}, countTotal: 1}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	// Same key:value twice must collapse to a single predicate, not inflate N.
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files?tag=site:tokyo&tag=site:tokyo", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	require.Len(t, mockDB.lastList.Tags, 1)
	assert.Equal(t, db.FileTagFilter{Key: "site", Value: "tokyo"}, mockDB.lastList.Tags[0])
}

func TestFilesHandler_List_ConflictingTagKey_Returns400(t *testing.T) {
	mockDB := &mockFilesDB{entries: []*db.FileEntry{newSampleEntry()}, countTotal: 1}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	// Two values for one key can never both match (one value per file×key).
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files?tag=site:tokyo&tag=site:osaka", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestFilesHandler_List_TooManyTagFilters_Returns400(t *testing.T) {
	mockDB := &mockFilesDB{entries: []*db.FileEntry{newSampleEntry()}, countTotal: 1}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	// 21 distinct keys exceeds the max of 20.
	q := make([]string, 0, 21)
	for i := 0; i < 21; i++ {
		q = append(q, "tag=k"+strconv.Itoa(i)+":v")
	}
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files?"+strings.Join(q, "&"), nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestFilesHandler_List_TagsDBError(t *testing.T) {
	mockDB := &mockFilesDB{entries: []*db.FileEntry{newSampleEntry()}, countTotal: 1, tagsErr: assert.AnError}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files", nil)
	testFilesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestFilesHandler_Get_TagsInResponse(t *testing.T) {
	entry := newSampleEntry()
	mockDB := &mockFilesDB{
		entry:      entry,
		tagsByFile: map[uuid.UUID]map[string]string{entry.ID: {"vendor": "omron"}},
	}
	h := handler.NewFilesHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/files/"+entry.ID.String(), nil)
	testFilesRouter(h).ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	tags := body["tags"].(map[string]interface{})
	assert.Equal(t, "omron", tags["vendor"])
}
