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
	deleteErr error
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
func (m *mockBucketsDB) DeleteBucket(_ context.Context, _ uuid.UUID) error { return m.deleteErr }

// ── mock EventRulesDB ─────────────────────────────────────────────────────────

type mockEventRulesDB struct {
	rules            []*db.EventRule
	listErr          error
	createErr        error
	updateErr        error
	deleteErr        error
	deliveries       []*db.EventDelivery
	deliveryErr      error
	deliveriesCount  int64
	countDeliveryErr error
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
func (m *mockEventRulesDB) CountDeliveriesByRule(_ context.Context, _ uuid.UUID) (int64, error) {
	return m.deliveriesCount, m.countDeliveryErr
}

// ── mock UploadLogsDB ─────────────────────────────────────────────────────────

type mockUploadLogsDB struct {
	logs         []*db.UploadLog
	listErr      error
	logsCount    int64
	countLogsErr error
	log          *db.UploadLog
	getErr       error
	// Captured params from the last List/Count call for assertions.
	gotListParams  db.ListUploadLogsParams
	gotCountFilter db.CountUploadLogsFilter
}

func (m *mockUploadLogsDB) ListUploadLogs(_ context.Context, p db.ListUploadLogsParams) ([]*db.UploadLog, error) {
	m.gotListParams = p
	return m.logs, m.listErr
}
func (m *mockUploadLogsDB) CountUploadLogs(_ context.Context, f db.CountUploadLogsFilter) (int64, error) {
	m.gotCountFilter = f
	return m.logsCount, m.countLogsErr
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
	assert.Len(t, body["items"].([]interface{}), 1)
}

func TestBucketsHandler_List_NilDB_Returns501(t *testing.T) {
	h := handler.NewBucketsHandler(nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/buckets", nil)
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestBucketsHandler_List_EmptyDB_ReturnsEmptyDataArray(t *testing.T) {
	// DB has no records: expect 200 with {"data": []}, not an error.
	h := handler.NewBucketsHandler(&mockBucketsDB{buckets: []*db.Bucket{}}, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/buckets", nil)
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	data, ok := body["items"].([]interface{})
	require.True(t, ok, "items field should be an array")
	assert.Empty(t, data, "items array should be empty when DB has no bucket records")
}

func TestBucketsHandler_List_DBError_Returns500(t *testing.T) {
	h := handler.NewBucketsHandler(&mockBucketsDB{listErr: assert.AnError}, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/buckets", nil)
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
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

func TestBucketsHandler_Create_MinioError_Returns502AndRollsBack(t *testing.T) {
	// When MinIO fails the DB record should be rolled back and 502 returned.
	minio := &mockMinioBucketMaker{makeFn: func(_ string) error { return assert.AnError }}
	h := handler.NewBucketsHandler(&mockBucketsDB{}, minio, newTestLogger())
	body := `{"name":"new-bucket"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/buckets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadGateway, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errBody := resp["error"].(map[string]interface{})
	assert.Equal(t, "MINIO_ERROR", errBody["code"])
}

func TestBucketsHandler_Create_InvalidName_UnderscoreReturns422(t *testing.T) {
	h := handler.NewBucketsHandler(&mockBucketsDB{}, nil, newTestLogger())
	body := `{"name":"radar_data"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/buckets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errBody := resp["error"].(map[string]interface{})
	assert.Equal(t, "INVALID_BUCKET_NAME", errBody["code"])
}

func TestBucketsHandler_Create_InvalidName_UppercaseReturns422(t *testing.T) {
	h := handler.NewBucketsHandler(&mockBucketsDB{}, nil, newTestLogger())
	body := `{"name":"UPPER"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/buckets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestBucketsHandler_Create_InvalidName_LeadingHyphenReturns422(t *testing.T) {
	h := handler.NewBucketsHandler(&mockBucketsDB{}, nil, newTestLogger())
	body := `{"name":"-bad-bucket"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/buckets", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testBucketsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestBucketsHandler_Create_ValidName_OkBucket(t *testing.T) {
	h := handler.NewBucketsHandler(&mockBucketsDB{}, nil, newTestLogger())
	body := `{"name":"ok-bucket"}`
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

func postEventRule(t *testing.T, h *handler.EventRulesHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/event-rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testEventRulesRouter(h).ServeHTTP(w, req)
	return w
}

func errCode(t *testing.T, w *httptest.ResponseRecorder) string {
	t.Helper()
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	return body["error"].(map[string]interface{})["code"].(string)
}

func TestEventRulesHandler_Create_NatsPublish_Success(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	body := `{"name":"n","event_type":"file_uploaded","action_type":"nats_publish","action_config":{"subject":"events.custom.sink"},"enabled":true}`
	w := postEventRule(t, h, body)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestEventRulesHandler_Create_KafkaPublish_Rejected(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	body := `{"name":"k","event_type":"file_uploaded","action_type":"kafka_publish","action_config":{"brokers":"x"},"enabled":true}`
	w := postEventRule(t, h, body)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "INVALID_ACTION_TYPE", errCode(t, w))
}

func TestEventRulesHandler_Create_UnknownActionType_Rejected(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	body := `{"name":"e","event_type":"file_uploaded","action_type":"email","action_config":{},"enabled":true}`
	w := postEventRule(t, h, body)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "INVALID_ACTION_TYPE", errCode(t, w))
}

func TestEventRulesHandler_Create_WebhookMissingURL_Rejected(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	body := `{"name":"w","event_type":"file_uploaded","action_type":"webhook","action_config":{},"enabled":true}`
	w := postEventRule(t, h, body)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "INVALID_ACTION_CONFIG", errCode(t, w))
}

func TestEventRulesHandler_Create_NatsMissingSubject_Rejected(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	body := `{"name":"n","event_type":"file_uploaded","action_type":"nats_publish","action_config":{},"enabled":true}`
	w := postEventRule(t, h, body)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "INVALID_ACTION_CONFIG", errCode(t, w))
}

func TestEventRulesHandler_Update_KafkaPublish_Rejected(t *testing.T) {
	h := handler.NewEventRulesHandler(&mockEventRulesDB{}, newTestLogger())
	body := `{"name":"k","event_type":"file_uploaded","action_type":"kafka_publish","action_config":{},"enabled":true}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/event-rules/"+uuid.New().String(), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testEventRulesRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusBadRequest, w.Code)
	assert.Equal(t, "INVALID_ACTION_TYPE", errCode(t, w))
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

// A failed webhook delivery carries response_code/response_body/next_retry_at so
// the UI can expand the row and explain the failure (D-026, additive fields).
func TestEventRulesHandler_ListDeliveries_ExposesResponseFields(t *testing.T) {
	retryAt := time.Now().Add(time.Minute)
	delivery := &db.EventDelivery{
		ID:           uuid.New(),
		EventRuleID:  uuid.New(),
		EventType:    db.EventTypeFileUploaded,
		Payload:      json.RawMessage(`{}`),
		Status:       "failed",
		AttemptCount: 2,
		ResponseCode: sql.NullInt32{Int32: 500, Valid: true},
		ResponseBody: sql.NullString{String: "upstream error", Valid: true},
		NextRetryAt:  sql.NullTime{Time: retryAt, Valid: true},
		CreatedAt:    time.Now(),
	}
	h := handler.NewEventRulesHandler(&mockEventRulesDB{deliveries: []*db.EventDelivery{delivery}}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/event-rules/"+delivery.EventRuleID.String()+"/deliveries", nil)
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Items []map[string]interface{} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	item := body.Items[0]
	assert.Equal(t, float64(500), item["response_code"])
	assert.Equal(t, "upstream error", item["response_body"])
	assert.NotEmpty(t, item["next_retry_at"])
	// delivered_at is nil for a failed delivery → omitted.
	_, hasDelivered := item["delivered_at"]
	assert.False(t, hasDelivered)
}

// nats_publish / not-yet-attempted deliveries have no HTTP response fields; they
// must be omitted rather than serialized as zero values.
func TestEventRulesHandler_ListDeliveries_OmitsAbsentResponseFields(t *testing.T) {
	delivery := &db.EventDelivery{
		ID:          uuid.New(),
		EventRuleID: uuid.New(),
		EventType:   db.EventTypeFileUploaded,
		Payload:     json.RawMessage(`{}`),
		Status:      "pending",
		CreatedAt:   time.Now(),
	}
	h := handler.NewEventRulesHandler(&mockEventRulesDB{deliveries: []*db.EventDelivery{delivery}}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/event-rules/"+delivery.EventRuleID.String()+"/deliveries", nil)
	testEventRulesRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Items []map[string]interface{} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	item := body.Items[0]
	for _, k := range []string{"response_code", "response_body", "next_retry_at", "delivered_at"} {
		_, ok := item[k]
		assert.Falsef(t, ok, "expected %s to be omitted", k)
	}
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
	h := handler.NewUploadLogsHandler(&mockUploadLogsDB{logs: []*db.UploadLog{newSampleLog()}, logsCount: 1}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/upload-logs", nil)
	testUploadLogsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body["items"].([]interface{}), 1)
	assert.Equal(t, float64(1), body["total"])
	assert.Equal(t, false, body["has_more"])
}

// A failed upload log exposes the retry-trail fields (retry_count, transferred
// bytes, timing) so the UI can expand the row (D-027, additive fields).
func TestUploadLogsHandler_List_ExposesRetryTrail(t *testing.T) {
	started := time.Now()
	log := &db.UploadLog{
		ID:               uuid.New(),
		OrgID:            uuid.New(),
		AgentID:          uuid.New(),
		StoragePath:      "uploads/big.bin",
		SizeBytes:        2048,
		BytesTransferred: 512,
		Status:           "failed",
		ErrorMessage:     sql.NullString{String: "connection reset", Valid: true},
		RetryCount:       3,
		StartedAt:        started,
		FinishedAt:       sql.NullTime{Time: started.Add(time.Minute), Valid: true},
		CreatedAt:        started,
	}
	h := handler.NewUploadLogsHandler(&mockUploadLogsDB{logs: []*db.UploadLog{log}, logsCount: 1}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/upload-logs", nil)
	testUploadLogsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var body struct {
		Items []map[string]interface{} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	item := body.Items[0]
	assert.Equal(t, float64(3), item["retry_count"])
	assert.Equal(t, float64(512), item["bytes_transferred"])
	assert.Equal(t, "connection reset", item["error_message"])
	assert.NotEmpty(t, item["started_at"])
	assert.NotEmpty(t, item["finished_at"])
}

// The status query param (sent upper-case by the UI) is normalized to lower-case
// and threaded into both the list query and the count filter.
func TestUploadLogsHandler_List_StatusFilterNormalized(t *testing.T) {
	m := &mockUploadLogsDB{logs: []*db.UploadLog{newSampleLog()}, logsCount: 1}
	h := handler.NewUploadLogsHandler(m, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/upload-logs?status=FAILED", nil)
	testUploadLogsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, sql.NullString{String: "failed", Valid: true}, m.gotListParams.Status)
	assert.Equal(t, sql.NullString{String: "failed", Valid: true}, m.gotCountFilter.Status)
}

// No status param → no status filter on either query.
func TestUploadLogsHandler_List_NoStatusFilter(t *testing.T) {
	m := &mockUploadLogsDB{logs: []*db.UploadLog{newSampleLog()}, logsCount: 1}
	h := handler.NewUploadLogsHandler(m, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/upload-logs", nil)
	testUploadLogsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, m.gotListParams.Status.Valid)
	assert.False(t, m.gotCountFilter.Status.Valid)
}

func TestUploadLogsHandler_List_HasMore(t *testing.T) {
	// Simulate limit=1 with 2 entries (limit+1) returned by mock.
	logs := []*db.UploadLog{newSampleLog(), newSampleLog()}
	h := handler.NewUploadLogsHandler(&mockUploadLogsDB{logs: logs, logsCount: 10}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/upload-logs?limit=1", nil)
	testUploadLogsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, true, body["has_more"])
	assert.Equal(t, float64(10), body["total"])
	assert.Len(t, body["items"].([]interface{}), 1)
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
	err        error
	called     bool
	deleted    bool
	lastBucket string
	lastKey    string
	deletedKey string
}

func (m *mockIndexerClient) IndexUpload(_ context.Context, bucketName, objectKey string, _ int64, _ string) error {
	m.called = true
	m.lastBucket = bucketName
	m.lastKey = objectKey
	return m.err
}

func (m *mockIndexerClient) IndexDeletion(_ context.Context, bucketName, objectKey string) error {
	m.deleted = true
	m.lastBucket = bucketName
	m.deletedKey = objectKey
	return m.err
}

// testWebhookSecret is the shared secret used by the MinIO webhook tests.
const testWebhookSecret = "test-webhook-secret"

// testMinioEventRouter wires the handler and, for payload-focused tests, injects
// the valid webhook secret when the request carries no Authorization header.
// Auth-specific tests set their own header (or none) and assert the status.
func testMinioEventRouter(h *handler.MinioEventHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/internal/minio-event", func(c *gin.Context) {
		if c.GetHeader("Authorization") == "" {
			c.Request.Header.Set("Authorization", "Bearer "+testWebhookSecret)
		}
		h.Handle(c)
	})
	return r
}

func TestMinioEventHandler_Handle_Success_WithRecords(t *testing.T) {
	ix := &mockIndexerClient{}
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
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
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
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
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString("not-json"))
	req.Header.Set("Content-Type", "application/json")
	testMinioEventRouter(h).ServeHTTP(w, req)
	// Non-fatal: still returns 200 OK.
	assert.Equal(t, http.StatusOK, w.Code)
	assert.False(t, ix.called)
}

func TestMinioEventHandler_Handle_NilIndexer(t *testing.T) {
	h := handler.NewMinioEventHandler(nil, testWebhookSecret, newTestLogger())
	body := `{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"b"},"object":{"key":"k","size":1}}}]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testMinioEventRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestMinioEventHandler_Handle_IndexerError_StillReturns200(t *testing.T) {
	ix := &mockIndexerClient{err: assert.AnError}
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
	body := `{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"b"},"object":{"key":"k","size":1}}}]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testMinioEventRouter(h).ServeHTTP(w, req)
	// Indexer error is non-fatal; still 200.
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, ix.called)
}

// TestMinioEventHandler_RoutesRemovedToDeletion verifies an ObjectRemoved event
// is routed to IndexDeletion (soft-delete + file_deleted), not indexed as an
// upload. Previously every event was treated as an upload (CC-1).
func TestMinioEventHandler_RoutesRemovedToDeletion(t *testing.T) {
	ix := &mockIndexerClient{}
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
	body := `{"Records":[{"eventName":"s3:ObjectRemoved:Delete","s3":{"bucket":{"name":"data-sensor"},"object":{"key":"uploads/gone.csv"}}}]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testMinioEventRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, ix.deleted, "ObjectRemoved must route to IndexDeletion")
	assert.False(t, ix.called, "ObjectRemoved must not be indexed as an upload")
	assert.Equal(t, "uploads/gone.csv", ix.deletedKey)
}

func TestMinioEventHandler_RoutesCreatedToUpload(t *testing.T) {
	ix := &mockIndexerClient{}
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
	body := `{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"data-sensor"},"object":{"key":"uploads/new.csv","size":10}}}]}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testMinioEventRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, ix.called, "ObjectCreated must be indexed as an upload")
	assert.False(t, ix.deleted)
}

// ── Object key URL decoding (IC-2c / IC-BUG-19) ───────────────────────────────

// minioEventBody builds a one-record MinIO notification payload carrying the
// given raw (still URL-encoded) object key, exactly as MinIO would send it.
func minioEventBody(t *testing.T, eventName, bucket, rawKey string) string {
	t.Helper()
	rec := map[string]any{
		"eventName": eventName,
		"s3": map[string]any{
			"bucket": map[string]any{"name": bucket},
			"object": map[string]any{"key": rawKey, "size": 42, "eTag": "etag-1"},
		},
	}
	b, err := json.Marshal(map[string]any{"Records": []any{rec}})
	require.NoError(t, err)
	return string(b)
}

// postMinioEvent posts one notification through the authenticated test router.
func postMinioEvent(t *testing.T, ix *mockIndexerClient, body string) *httptest.ResponseRecorder {
	t.Helper()
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
	w := httptest.NewRecorder()
	req, err := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	testMinioEventRouter(h).ServeHTTP(w, req)
	return w
}

// TestMinioEventHandler_DecodesObjectKey pins IC-BUG-19: MinIO URL-encodes
// s3.object.key when it builds the event, so indexing it verbatim stored
// "a%2Fb%2Fc.csv" as the storage_path of an object whose real key is
// "a/b/c.csv". The encoding is the query form, so "+" means a space.
func TestMinioEventHandler_DecodesObjectKey(t *testing.T) {
	tests := []struct {
		name   string
		rawKey string
		want   string
	}{
		{"flat key needs no decoding", "file.csv", "file.csv"},
		{"hierarchical key", "a%2Fb%2Fc.csv", "a/b/c.csv"},
		{"space encoded as plus", "nested+dir%2Ffile.csv", "nested dir/file.csv"},
		{"space encoded as percent-20", "nested%20dir%2Ffile.csv", "nested dir/file.csv"},
		// The exact payload MinIO produced in dev for "ic19/nested dir/中 文.csv".
		{"non-ascii key", "ic19%2Fnested+dir%2F%E4%B8%AD+%E6%96%87.csv", "ic19/nested dir/中 文.csv"},
		// A literal plus in the key arrives percent-encoded, so decoding it as
		// query form does not corrupt it.
		{"literal plus in key", "a%2Fb%2Bc.csv", "a/b+c.csv"},
		{"template-shaped agent key", "Miru%2Ftokyo%2F2026%2F09%2F10%2Ftokyo_001.csv", "Miru/tokyo/2026/09/10/tokyo_001.csv"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ix := &mockIndexerClient{}
			w := postMinioEvent(t, ix, minioEventBody(t, "s3:ObjectCreated:Put", "data-sensor", tt.rawKey))
			assert.Equal(t, http.StatusOK, w.Code)
			require.True(t, ix.called, "ObjectCreated must reach IndexUpload")
			assert.Equal(t, tt.want, ix.lastKey)
			assert.Equal(t, "data-sensor", ix.lastBucket)
		})
	}
}

// TestMinioEventHandler_DecodesObjectKey_Deletion covers the deletion path,
// which reaches the index through a different call. Leaving it encoded would
// make IndexDeletion a silent no-op for every hierarchical key (it matches on
// storage_path), turning deleted objects into permanent ghost rows.
func TestMinioEventHandler_DecodesObjectKey_Deletion(t *testing.T) {
	ix := &mockIndexerClient{}
	w := postMinioEvent(t, ix, minioEventBody(t, "s3:ObjectRemoved:Delete", "data-sensor", "a%2Fb%2F%E4%B8%AD+%E6%96%87.csv"))
	assert.Equal(t, http.StatusOK, w.Code)
	require.True(t, ix.deleted, "ObjectRemoved must reach IndexDeletion")
	assert.Equal(t, "a/b/中 文.csv", ix.deletedKey)
}

// TestMinioEventHandler_InvalidEncoding_FallsBackToRawKey asserts the decode
// failure path: an unparseable escape must not drop the event, because losing a
// create leaves MinIO holding an object the index never learns about. The raw
// key is indexed instead (and logged as a warning).
func TestMinioEventHandler_InvalidEncoding_FallsBackToRawKey(t *testing.T) {
	ix := &mockIndexerClient{}
	w := postMinioEvent(t, ix, minioEventBody(t, "s3:ObjectCreated:Put", "data-sensor", "a%2Gb.csv"))
	assert.Equal(t, http.StatusOK, w.Code)
	require.True(t, ix.called, "an undecodable key must still be indexed, not dropped")
	assert.Equal(t, "a%2Gb.csv", ix.lastKey)
}

// ── Webhook authentication (G-3) ──────────────────────────────────────────────

const validMinioBody = `{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"b"},"object":{"key":"k","size":1}}}]}`

// plainMinioRouter wires Handle without any header injection, so auth behaviour
// can be asserted directly.
func plainMinioRouter(h *handler.MinioEventHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.POST("/internal/minio-event", h.Handle)
	return r
}

func TestMinioEventHandler_RejectsMissingAuthHeader(t *testing.T) {
	ix := &mockIndexerClient{}
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(validMinioBody))
	req.Header.Set("Content-Type", "application/json")
	plainMinioRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.False(t, ix.called, "unauthenticated event must not be indexed")
}

// TestMinioEventHandler_AcceptsCaseInsensitiveScheme verifies the scheme is
// matched per RFC 7235 (case-insensitive) and tolerates extra whitespace.
func TestMinioEventHandler_AcceptsCaseInsensitiveScheme(t *testing.T) {
	ix := &mockIndexerClient{}
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(validMinioBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "bearer   "+testWebhookSecret) // lowercase + extra spaces
	plainMinioRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, ix.called)
}

func TestMinioEventHandler_RejectsWrongSecret(t *testing.T) {
	ix := &mockIndexerClient{}
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(validMinioBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer wrong-secret")
	plainMinioRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.False(t, ix.called)
}

// TestMinioEventHandler_AcceptsRawToken verifies robustness against MinIO
// versions that send auth_token without a "Bearer " prefix.
func TestMinioEventHandler_AcceptsRawToken(t *testing.T) {
	ix := &mockIndexerClient{}
	h := handler.NewMinioEventHandler(ix, testWebhookSecret, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(validMinioBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", testWebhookSecret) // no "Bearer " prefix
	plainMinioRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	assert.True(t, ix.called)
}

// TestMinioEventHandler_NoSecretConfigured_FailsClosed verifies that with no
// configured secret the endpoint rejects every request rather than silently
// accepting unauthenticated input.
func TestMinioEventHandler_NoSecretConfigured_FailsClosed(t *testing.T) {
	ix := &mockIndexerClient{}
	h := handler.NewMinioEventHandler(ix, "", newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/internal/minio-event", bytes.NewBufferString(validMinioBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer anything")
	plainMinioRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
	assert.False(t, ix.called)
}
