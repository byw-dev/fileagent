package handler_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
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

// ── mock BucketsDB ────────────────────────────────────────────────────────────

type mockBucketsDB struct {
	buckets   []*db.Bucket
	listErr   error
	createErr error
}

func (m *mockBucketsDB) ListBuckets(_ context.Context, _ uuid.UUID) ([]*db.Bucket, error) {
	return m.buckets, m.listErr
}
func (m *mockBucketsDB) CreateBucket(_ context.Context, arg db.CreateBucketParams) (*db.Bucket, error) {
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &db.Bucket{
		ID:          uuid.New(),
		OrgID:       arg.OrgID,
		Name:        arg.Name,
		Description: arg.Description,
		CreatedAt:   time.Now(),
	}, nil
}

// ── mock EventRulesDB ─────────────────────────────────────────────────────────

type mockEventRulesDB struct {
	rules       []*db.EventRule
	listErr     error
	createErr   error
	updateErr   error
	deleteErr   error
	deliveries  []*db.EventDelivery
	deliveryErr error
}

func (m *mockEventRulesDB) ListEventRules(_ context.Context, _ uuid.UUID) ([]*db.EventRule, error) {
	return m.rules, m.listErr
}
func (m *mockEventRulesDB) GetEventRuleByID(_ context.Context, _ uuid.UUID) (*db.EventRule, error) {
	return nil, nil
}
func (m *mockEventRulesDB) CreateEventRule(_ context.Context, arg db.CreateEventRuleParams) (*db.EventRule, error) {
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &db.EventRule{
		ID:           uuid.New(),
		OrgID:        arg.OrgID,
		Name:         arg.Name,
		EventType:    arg.EventType,
		Filter:       arg.Filter,
		ActionType:   arg.ActionType,
		ActionConfig: arg.ActionConfig,
		Enabled:      arg.Enabled,
		CreatedAt:    time.Now(),
	}, nil
}
func (m *mockEventRulesDB) UpdateEventRule(_ context.Context, arg db.UpdateEventRuleParams) (*db.EventRule, error) {
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	return &db.EventRule{
		ID:           arg.ID,
		Name:         arg.Name,
		EventType:    arg.EventType,
		ActionType:   arg.ActionType,
		ActionConfig: arg.ActionConfig,
		Enabled:      arg.Enabled,
		CreatedAt:    time.Now(),
	}, nil
}
func (m *mockEventRulesDB) DeleteEventRule(_ context.Context, _ uuid.UUID) error { return m.deleteErr }
func (m *mockEventRulesDB) ListDeliveriesByRule(_ context.Context, _ db.ListDeliveriesByRuleParams) ([]*db.EventDelivery, error) {
	return m.deliveries, m.deliveryErr
}

// ── mock UploadLogsDB ─────────────────────────────────────────────────────────

type mockUploadLogsDB struct {
	logs    []*db.UploadLog
	listErr error
	log     *db.UploadLog
	getErr  error
}

func (m *mockUploadLogsDB) ListUploadLogs(_ context.Context, _ db.ListUploadLogsParams) ([]*db.UploadLog, error) {
	return m.logs, m.listErr
}
func (m *mockUploadLogsDB) GetUploadLogByID(_ context.Context, _ uuid.UUID) (*db.UploadLog, error) {
	return m.log, m.getErr
}

// ── routers ───────────────────────────────────────────────────────────────────

func testBucketsRouter(h *handler.BucketsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, "super_admin", uuid.New().String(), uuid.New().String())
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.GET("/buckets", h.List)
	v1.POST("/buckets", h.Create)
	return r
}

func testEventRulesRouter(h *handler.EventRulesHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, "org_admin", uuid.New().String(), uuid.New().String())
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.GET("/event-rules", h.List)
	v1.POST("/event-rules", h.Create)
	v1.PUT("/event-rules/:id", h.Update)
	v1.DELETE("/event-rules/:id", h.Delete)
	v1.GET("/event-rules/:id/deliveries", h.ListDeliveries)
	return r
}

func testUploadLogsRouter(h *handler.UploadLogsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, "org_viewer", uuid.New().String(), uuid.New().String())
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.GET("/upload-logs", h.List)
	v1.GET("/upload-logs/:id", h.Get)
	return r
}

// ── BucketsHandler tests ──────────────────────────────────────────────────────

func TestBucketsHandler_List_Success(t *testing.T) {
	mockDB := &mockBucketsDB{buckets: []*db.Bucket{
		{ID: uuid.New(), OrgID: uuid.New(), Name: "data-sensor", CreatedAt: time.Now()},
	}}
	h := handler.NewBucketsHandler(mockDB, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/buckets", nil)
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body["data"].([]interface{}), 1)
}

func TestBucketsHandler_List_NilDB_Returns501(t *testing.T) {
	h := handler.NewBucketsHandler(nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/buckets", nil)
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestBucketsHandler_Create_Success(t *testing.T) {
	h := handler.NewBucketsHandler(&mockBucketsDB{}, nil, newTestLogger())
	body := `{"name":"new-bucket","description":"test bucket"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/buckets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestBucketsHandler_Create_MissingName(t *testing.T) {
	h := handler.NewBucketsHandler(&mockBucketsDB{}, nil, newTestLogger())
	body := `{"description":"no name"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/buckets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestBucketsHandler_Create_CallsMakeBucket(t *testing.T) {
	makeBucketCalled := false
	minio := &mockMinioBucketMaker{makeFn: func(name string) error {
		makeBucketCalled = true
		assert.Equal(t, "new-bucket", name)
		return nil
	}}
	h := handler.NewBucketsHandler(&mockBucketsDB{}, minio, newTestLogger())
	body := `{"name":"new-bucket"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/buckets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)
	assert.True(t, makeBucketCalled, "MakeBucket should have been called")
}

func TestBucketsHandler_Create_MinioError_StillReturns201(t *testing.T) {
	// Even when MinIO fails, the DB record is created and 201 returned.
	minio := &mockMinioBucketMaker{makeFn: func(_ string) error { return assert.AnError }}
	h := handler.NewBucketsHandler(&mockBucketsDB{}, minio, newTestLogger())
	body := `{"name":"new-bucket"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/buckets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestBucketsHandler_Create_DBError(t *testing.T) {
	h := handler.NewBucketsHandler(&mockBucketsDB{createErr: assert.AnError}, nil, newTestLogger())
	body := `{"name":"new-bucket"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/buckets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// mockMinioBucketMaker is a test double for handler.MinioBucketMaker.
type mockMinioBucketMaker struct {
	makeFn func(name string) error
}

func (m *mockMinioBucketMaker) MakeBucket(_ context.Context, name string) error {
	if m.makeFn != nil {
		return m.makeFn(name)
	}
	return nil
}

// ── EventRulesHandler tests ───────────────────────────────────────────────────

func validEventRuleBody() string {
	return `{
		"name":"on-upload",
		"event_type":"file_uploaded",
		"action_type":"webhook",
		"action_config":{"url":"https://example.com/hook"},
		"enabled":true
	}`
}

func TestEventRulesHandler_List_Success(t *testing.T) {
	rule := &db.EventRule{
		ID:           uuid.New(),
		OrgID:        uuid.New(),
		Name:         "test",
		EventType:    db.EventTypeFileUploaded,
		Filter:       json.RawMessage(`{}`),
		ActionType:   db.ActionTypeWebhook,
		ActionConfig: json.RawMessage(`{}`),
		Enabled:      true,
		CreatedAt:    time.Now(),
	}
	h := handler.NewEventRulesHandler(&mockEventRulesDB{rules: []*db.EventRule{rule}}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/event-rules", nil)
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestEventRulesHandler_List_NilDB_Returns501(t *testing.T) {
	h := handler.NewEventRulesHandler(nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/event-rules", nil)
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestEventRulesHandler_Create_Success(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/event-rules", bytes.NewBufferString(validEventRuleBody()))
	req.Header.Set("Content-Type", "application/json")
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestEventRulesHandler_Create_MissingFields(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	body := `{"name":"only-name"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/event-rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestEventRulesHandler_Update_Success(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/event-rules/"+uuid.New().String(), bytes.NewBufferString(validEventRuleBody()))
	req.Header.Set("Content-Type", "application/json")
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestEventRulesHandler_Update_InvalidID(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/event-rules/bad-id", bytes.NewBufferString(validEventRuleBody()))
	req.Header.Set("Content-Type", "application/json")
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestEventRulesHandler_Update_NotFound(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{updateErr: sql.ErrNoRows}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/event-rules/"+uuid.New().String(), bytes.NewBufferString(validEventRuleBody()))
	req.Header.Set("Content-Type", "application/json")
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestEventRulesHandler_Delete_Success(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/v1/event-rules/"+uuid.New().String(), nil)
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestEventRulesHandler_ListDeliveries_Success(t *testing.T) {
	delivery := &db.EventDelivery{
		ID:          uuid.New(),
		EventRuleID: uuid.New(),
		EventType:   db.EventTypeFileUploaded,
		Payload:     json.RawMessage(`{}`),
		Status:      "delivered",
		CreatedAt:   time.Now(),
	}
	h := handler.NewEventRulesHandler(&mockEventRulesDB{deliveries: []*db.EventDelivery{delivery}}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/event-rules/"+delivery.EventRuleID.String()+"/deliveries", nil)
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestEventRulesHandler_ListDeliveries_InvalidID(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/event-rules/bad-id/deliveries", nil)
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── UploadLogsHandler tests ───────────────────────────────────────────────────

func newSampleLog() *db.UploadLog {
	return &db.UploadLog{
		ID:          uuid.New(),
		OrgID:       uuid.New(),
		AgentID:     uuid.New(),
		StoragePath: "uploads/file.txt",
		SizeBytes:   1024,
		Status:      "completed",
		StartedAt:   time.Now(),
		CreatedAt:   time.Now(),
	}
}

func TestUploadLogsHandler_List_Success(t *testing.T) {
	h := handler.NewUploadLogsHandler(&mockUploadLogsDB{logs: []*db.UploadLog{newSampleLog()}}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/upload-logs", nil)
	testUploadLogsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body["data"].([]interface{}), 1)
}

func TestUploadLogsHandler_List_NilDB_Returns501(t *testing.T) {
	h := handler.NewUploadLogsHandler(nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/upload-logs", nil)
	testUploadLogsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestUploadLogsHandler_Get_Success(t *testing.T) {
	l := newSampleLog()
	h := handler.NewUploadLogsHandler(&mockUploadLogsDB{log: l}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/upload-logs/"+l.ID.String(), nil)
	testUploadLogsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUploadLogsHandler_Get_NotFound(t *testing.T) {
	h := handler.NewUploadLogsHandler(&mockUploadLogsDB{getErr: sql.ErrNoRows}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/upload-logs/"+uuid.New().String(), nil)
	testUploadLogsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestUploadLogsHandler_Get_InvalidID(t *testing.T) {
	h := handler.NewUploadLogsHandler(&mockUploadLogsDB{}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/upload-logs/bad-id", nil)
	testUploadLogsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── EventRulesHandler missing error path tests ────────────────────────────────

func TestEventRulesHandler_Delete_InvalidID(t *testing.T) {
h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodDelete, "/api/v1/event-rules/not-a-uuid", nil)
testEventRulesRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestEventRulesHandler_Delete_DBError(t *testing.T) {
h := handler.NewEventRulesHandler(&mockEventRulesDB{deleteErr: assert.AnError}, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodDelete, "/api/v1/event-rules/"+uuid.New().String(), nil)
testEventRulesRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ── MinioEventHandler tests ───────────────────────────────────────────────────

// mockIndexerClient is a test double for handler.IndexerClient.
type mockIndexerClient struct {
err     error
called  bool
lastBucket string
lastKey    string
}

func (m *mockIndexerClient) IndexUpload(_ context.Context, bucketName, objectKey string, _ int64, _ string) error {
m.called = true
m.lastBucket = bucketName
m.lastKey = objectKey
return m.err
}

func testMinioEventRouter(h *handler.MinioEventHandler) *gin.Engine {
gin.SetMode(gin.TestMode)
r := gin.New()
r.POST("/internal/minio-event", h.Handle)
return r
}

func TestMinioEventHandler_Handle_Success_WithRecords(t *testing.T) {
ix := &mockIndexerClient{}
h := handler.NewMinioEventHandler(ix, newTestLogger())
body := `{
"EventName": "s3:ObjectCreated:Put",
"Key": "data-sensor/file.csv",
"Records": [{
"eventName": "s3:ObjectCreated:Put",
"s3": {
"bucket": {"name": "data-sensor"},
"object": {"key": "uploads/file.csv", "size": 1024, "eTag": "abc123"}
}
}]
}`
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(body))
req.Header.Set("Content-Type", "application/json")
testMinioEventRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusOK, w.Code)
assert.True(t, ix.called, "IndexUpload should have been called")
assert.Equal(t, "data-sensor", ix.lastBucket)
assert.Equal(t, "uploads/file.csv", ix.lastKey)
}

func TestMinioEventHandler_Handle_EmptyRecords(t *testing.T) {
ix := &mockIndexerClient{}
h := handler.NewMinioEventHandler(ix, newTestLogger())
body := `{"EventName":"s3:ObjectCreated:Put","Key":"","Records":[]}`
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(body))
req.Header.Set("Content-Type", "application/json")
testMinioEventRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusOK, w.Code)
assert.False(t, ix.called, "IndexUpload should not be called for empty records")
}

func TestMinioEventHandler_Handle_InvalidJSON(t *testing.T) {
ix := &mockIndexerClient{}
h := handler.NewMinioEventHandler(ix, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString("not-json"))
req.Header.Set("Content-Type", "application/json")
testMinioEventRouter(h).ServeHTTP(w, req)
// Non-fatal: still returns 200 OK.
assert.Equal(t, http.StatusOK, w.Code)
assert.False(t, ix.called)
}

func TestMinioEventHandler_Handle_NilIndexer(t *testing.T) {
h := handler.NewMinioEventHandler(nil, newTestLogger())
body := `{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"b"},"object":{"key":"k","size":1}}}]}`
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(body))
req.Header.Set("Content-Type", "application/json")
testMinioEventRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusOK, w.Code)
}

func TestMinioEventHandler_Handle_IndexerError_StillReturns200(t *testing.T) {
ix := &mockIndexerClient{err: assert.AnError}
h := handler.NewMinioEventHandler(ix, newTestLogger())
body := `{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"b"},"object":{"key":"k","size":1}}}]}`
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(body))
req.Header.Set("Content-Type", "application/json")
testMinioEventRouter(h).ServeHTTP(w, req)
// Indexer error is non-fatal; still 200.
assert.Equal(t, http.StatusOK, w.Code)
assert.True(t, ix.called)
}
