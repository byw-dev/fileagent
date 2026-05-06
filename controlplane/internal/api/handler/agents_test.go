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

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/sqlc-dev/pqtype"
)

// ── mocks ─────────────────────────────────────────────────────────────────────

type mockAgentsDB struct {
	agents     []*db.Agent
	listErr    error
	agent      *db.Agent
	getErr     error
	rules      []*db.CollectionRule
	rulesErr   error
	rule       *db.CollectionRule
	ruleGetErr error
	createErr  error
	updateErr  error
	deleteErr  error
	logs       []*db.UploadLog
	logsErr    error
}

func (m *mockAgentsDB) ListAgents(_ context.Context, _ uuid.UUID) ([]*db.Agent, error) {
	return m.agents, m.listErr
}
func (m *mockAgentsDB) GetAgentByID(_ context.Context, _ uuid.UUID) (*db.Agent, error) {
	return m.agent, m.getErr
}
func (m *mockAgentsDB) ListCollectionRulesByAgent(_ context.Context, _ uuid.UUID) ([]*db.CollectionRule, error) {
	return m.rules, m.rulesErr
}
func (m *mockAgentsDB) GetCollectionRuleByID(_ context.Context, _ uuid.UUID) (*db.CollectionRule, error) {
	return m.rule, m.ruleGetErr
}
func (m *mockAgentsDB) CreateCollectionRule(_ context.Context, arg db.CreateCollectionRuleParams) (*db.CollectionRule, error) {
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &db.CollectionRule{
		ID:                 uuid.New(),
		OrgID:              arg.OrgID,
		AgentID:            arg.AgentID,
		BucketID:           arg.BucketID,
		Name:               arg.Name,
		Mode:               arg.Mode,
		Status:             db.RuleStatusActive,
		SourcePathTemplate: arg.SourcePathTemplate,
		FileGlob:           arg.FileGlob,
		UploadPathTemplate: arg.UploadPathTemplate,
		Metadata:           arg.Metadata,
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}, nil
}
func (m *mockAgentsDB) UpdateCollectionRuleStatus(_ context.Context, id uuid.UUID, status db.RuleStatus) (*db.CollectionRule, error) {
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	return &db.CollectionRule{
		ID:        id,
		AgentID:   uuid.New(),
		Status:    status,
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}, nil
}
func (m *mockAgentsDB) DeleteCollectionRule(_ context.Context, _ uuid.UUID) error { return m.deleteErr }
func (m *mockAgentsDB) ListUploadLogs(_ context.Context, _ db.ListUploadLogsParams) ([]*db.UploadLog, error) {
	return m.logs, m.logsErr
}

type mockAgentMgr struct {
	token      string
	approveErr error
	revokeErr  error
}

func (m *mockAgentMgr) ApproveAgent(_ context.Context, _ uuid.UUID, _ uuid.UUID) (string, error) {
	return m.token, m.approveErr
}
func (m *mockAgentMgr) RevokeAgent(_ context.Context, _ uuid.UUID, _ uuid.UUID) error {
	return m.revokeErr
}

type mockDispatcher struct {
	dispatchErr error
	cancelErr   error
}

func (m *mockDispatcher) DispatchRule(_ context.Context, _ *db.CollectionRule) error {
	return m.dispatchErr
}
func (m *mockDispatcher) DispatchRuleCancel(_ context.Context, _, _ string) error {
	return m.cancelErr
}

type mockAgentRegistry struct {
	online  bool
	sendOK  bool
}

func (m *mockAgentRegistry) Send(_ string, _ *agentv1.ServerMessage) bool { return m.sendOK }
func (m *mockAgentRegistry) IsOnline(_ string) bool                       { return m.online }

// ── helpers ───────────────────────────────────────────────────────────────────

func newSampleAgent() *db.Agent {
	return &db.Agent{
		ID:          uuid.New(),
		OrgID:       uuid.New(),
		Name:        "edge-01",
		Status:      db.AgentStatusApproved,
		IpAddress:   pqtype.Inet{},
		OsInfo:      json.RawMessage(`{}`),
		Metadata:    json.RawMessage(`{}`),
	}
}

func testAgentsRouter(h *handler.AgentsHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, "super_admin", uuid.New().String(), uuid.New().String())
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.GET("/agents", h.List)
	v1.GET("/agents/:id", h.Get)
	v1.POST("/agents/:id/approve", h.Approve)
	v1.POST("/agents/:id/revoke", h.Revoke)
	v1.POST("/agents/:id/list-dir", h.ListDir)
	v1.GET("/agents/:id/rules", h.ListRules)
	v1.POST("/agents/:id/rules", h.CreateRule)
	v1.PUT("/agents/:id/rules/:rid", h.UpdateRule)
	v1.DELETE("/agents/:id/rules/:rid", h.DeleteRule)
	v1.GET("/agents/:id/upload-logs", h.ListUploadLogs)
	return r
}

// ── List ──────────────────────────────────────────────────────────────────────

func TestAgentsHandler_List_Success(t *testing.T) {
	mockDB := &mockAgentsDB{agents: []*db.Agent{newSampleAgent()}}
	h := handler.NewAgentsHandler(mockDB, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Len(t, body["data"].([]interface{}), 1)
}

func TestAgentsHandler_List_NilDB_Returns501(t *testing.T) {
	h := handler.NewAgentsHandler(nil, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestAgentsHandler_Get_Success(t *testing.T) {
	a := newSampleAgent()
	mockDB := &mockAgentsDB{agent: a}
	h := handler.NewAgentsHandler(mockDB, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/"+a.ID.String(), nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAgentsHandler_Get_NotFound(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{getErr: sql.ErrNoRows}, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/"+uuid.New().String(), nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── Approve / Revoke ──────────────────────────────────────────────────────────

func TestAgentsHandler_Approve_Success(t *testing.T) {
	mgr := &mockAgentMgr{token: "agent-token-abc"}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, mgr, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/approve", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "agent-token-abc", body["token"])
}

func TestAgentsHandler_Approve_NilMgr_Returns501(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/approve", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestAgentsHandler_Approve_Error(t *testing.T) {
	mgr := &mockAgentMgr{approveErr: assert.AnError}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, mgr, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/approve", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestAgentsHandler_Revoke_Success(t *testing.T) {
	mgr := &mockAgentMgr{}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, mgr, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/revoke", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ── ListDir ───────────────────────────────────────────────────────────────────

func TestAgentsHandler_ListDir_AgentOnline_Returns202(t *testing.T) {
	registry := &mockAgentRegistry{online: true, sendOK: true}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, registry, newTestLogger())
	body := `{"path":"/data","recursive":true}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/list-dir", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusAccepted, w.Code)
}

func TestAgentsHandler_ListDir_AgentOffline_Returns409(t *testing.T) {
	registry := &mockAgentRegistry{online: false}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, registry, newTestLogger())
	body := `{"path":"/data"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/list-dir", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusConflict, w.Code)
}

func TestAgentsHandler_ListDir_NilRegistry_Returns501(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	body := `{"path":"/data"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/list-dir", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

// ── ListRules ─────────────────────────────────────────────────────────────────

func TestAgentsHandler_ListRules_Success(t *testing.T) {
	rule := &db.CollectionRule{ID: uuid.New(), AgentID: uuid.New(), Status: db.RuleStatusActive, Metadata: json.RawMessage(`{}`), CreatedAt: time.Now(), UpdatedAt: time.Now()}
	h := handler.NewAgentsHandler(&mockAgentsDB{rules: []*db.CollectionRule{rule}}, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/"+uuid.New().String()+"/rules", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ── CreateRule ────────────────────────────────────────────────────────────────

func TestAgentsHandler_CreateRule_Success(t *testing.T) {
	dispatcher := &mockDispatcher{}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, dispatcher, nil, newTestLogger())
	body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","source_path_template":"/data","file_glob":"*.log","upload_path_template":"logs/"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestAgentsHandler_CreateRule_InvalidBucketID(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	body := `{"bucket_id":"bad","name":"rule1","mode":"watch","source_path_template":"/data","file_glob":"*.log","upload_path_template":"logs/"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── UpdateRule ────────────────────────────────────────────────────────────────

func TestAgentsHandler_UpdateRule_Activate(t *testing.T) {
	dispatcher := &mockDispatcher{}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, dispatcher, nil, newTestLogger())
	body := `{"status":"active"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/agents/"+uuid.New().String()+"/rules/"+uuid.New().String(), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAgentsHandler_UpdateRule_InvalidStatus(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	body := `{"status":"unknown"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/agents/"+uuid.New().String()+"/rules/"+uuid.New().String(), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAgentsHandler_UpdateRule_NotFound(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{updateErr: sql.ErrNoRows}, nil, nil, nil, newTestLogger())
	body := `{"status":"inactive"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/agents/"+uuid.New().String()+"/rules/"+uuid.New().String(), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── DeleteRule ────────────────────────────────────────────────────────────────

func TestAgentsHandler_DeleteRule_Success(t *testing.T) {
	dispatcher := &mockDispatcher{}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, dispatcher, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/v1/agents/"+uuid.New().String()+"/rules/"+uuid.New().String(), nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

// ── ListUploadLogs ────────────────────────────────────────────────────────────

func TestAgentsHandler_ListUploadLogs_Success(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{logs: []*db.UploadLog{newSampleLog()}}, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/"+uuid.New().String()+"/upload-logs", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestAgentsHandler_ListUploadLogs_InvalidID(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/bad-id/upload-logs", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── Missing error path tests ──────────────────────────────────────────────────

func TestAgentsHandler_Get_InvalidID(t *testing.T) {
h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/not-a-uuid", nil)
testAgentsRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAgentsHandler_Get_DBError(t *testing.T) {
h := handler.NewAgentsHandler(&mockAgentsDB{getErr: assert.AnError}, nil, nil, nil, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/"+uuid.New().String(), nil)
testAgentsRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestAgentsHandler_Revoke_InvalidID(t *testing.T) {
h := handler.NewAgentsHandler(nil, &mockAgentMgr{}, nil, nil, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/not-a-uuid/revoke", nil)
testAgentsRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAgentsHandler_Revoke_Error(t *testing.T) {
h := handler.NewAgentsHandler(nil, &mockAgentMgr{revokeErr: assert.AnError}, nil, nil, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/revoke", nil)
testAgentsRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestAgentsHandler_ListRules_InvalidAgentID(t *testing.T) {
h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/bad-id/rules", nil)
testAgentsRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAgentsHandler_ListRules_DBError(t *testing.T) {
h := handler.NewAgentsHandler(&mockAgentsDB{rulesErr: assert.AnError}, nil, nil, nil, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/"+uuid.New().String()+"/rules", nil)
testAgentsRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestAgentsHandler_DeleteRule_InvalidID(t *testing.T) {
h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodDelete, "/api/v1/agents/bad-id/rules/"+uuid.New().String(), nil)
testAgentsRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAgentsHandler_DeleteRule_DBError(t *testing.T) {
h := handler.NewAgentsHandler(&mockAgentsDB{deleteErr: assert.AnError}, nil, nil, nil, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodDelete, "/api/v1/agents/"+uuid.New().String()+"/rules/"+uuid.New().String(), nil)
testAgentsRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestAgentsHandler_ListUploadLogs_DBError(t *testing.T) {
h := handler.NewAgentsHandler(&mockAgentsDB{logsErr: assert.AnError}, nil, nil, nil, newTestLogger())
w := httptest.NewRecorder()
req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/"+uuid.New().String()+"/upload-logs", nil)
testAgentsRouter(h).ServeHTTP(w, req)
assert.Equal(t, http.StatusInternalServerError, w.Code)
}
