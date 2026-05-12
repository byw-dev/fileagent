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
	"github.com/byw-dev/fileagent/controlplane/internal/dirstore"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/sqlc-dev/pqtype"
)

// ── mocks ─────────────────────────────────────────────────────────────────────

type mockAgentsDB struct {
	agents        []*db.Agent
	listErr       error
	agent         *db.Agent
	getErr        error
	rules         []*db.CollectionRule
	rulesErr      error
	rule          *db.CollectionRule
	ruleGetErr    error
	createErr     error
	updateErr     error
	deleteErr     error
	logs          []*db.UploadLog
	logsErr       error
	logsCount     int64
	countLogsErr  error
}

func (m *mockAgentsDB) ListAgents(_ context.Context, _ uuid.UUID) ([]*db.Agent, error) {
	return m.agents, m.listErr
}
func (m *mockAgentsDB) ListAgentsByStatus(_ context.Context, _ uuid.UUID, _ db.AgentStatus) ([]*db.Agent, error) {
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
func (m *mockAgentsDB) CountUploadLogs(_ context.Context, _ db.CountUploadLogsFilter) (int64, error) {
	return m.logsCount, m.countLogsErr
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
	online   bool
	sendOK   bool
	sentTo   string
	lastSent *agentv1.ServerMessage
}

func (m *mockAgentRegistry) Send(agentID string, msg *agentv1.ServerMessage) bool {
	m.sentTo = agentID
	m.lastSent = msg
	return m.sendOK
}
func (m *mockAgentRegistry) IsOnline(_ string) bool { return m.online }

// mockDirStore implements handler.DirListingStore for tests.
// If result is non-nil, Register immediately sends it into the returned channel.
type mockDirStore struct {
	result *dirstore.Result
}

func (m *mockDirStore) Register(_ string) <-chan dirstore.Result {
	ch := make(chan dirstore.Result, 1)
	if m.result != nil {
		ch <- *m.result
	}
	return ch
}
func (m *mockDirStore) Cancel(_ string) {}

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
	assert.Len(t, body["items"].([]interface{}), 1)
	assert.Equal(t, float64(1), body["total"])
}

func TestAgentsHandler_List_ItemsEnvelopeShape(t *testing.T) {
	agent := newSampleAgent()
	agent.OsInfo = json.RawMessage(`{"hostname":"host1","os_type":"linux","os_version":"5.15","agent_version":"1.0.0"}`)
	agent.Status = db.AgentStatusOnline
	mockDB := &mockAgentsDB{agents: []*db.Agent{agent}}
	h := handler.NewAgentsHandler(mockDB, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	items := body["items"].([]interface{})
	require.Len(t, items, 1)
	item := items[0].(map[string]interface{})
	assert.Equal(t, "RUNNING", item["status"])
	assert.Equal(t, "host1", item["hostname"])
	assert.Equal(t, "linux", item["os_type"])
	assert.Equal(t, "1.0.0", item["agent_version"])
}

func TestAgentsHandler_List_WithStatusFilter(t *testing.T) {
	pending := newSampleAgent()
	pending.Status = db.AgentStatusPending
	mockDB := &mockAgentsDB{agents: []*db.Agent{pending}}
	h := handler.NewAgentsHandler(mockDB, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents?status=PENDING", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	items := body["items"].([]interface{})
	assert.Len(t, items, 1)
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

func TestAgentsHandler_Revoke_SendsRevokeCommandWhenOnline(t *testing.T) {
	mgr := &mockAgentMgr{}
	registry := &mockAgentRegistry{online: true, sendOK: true}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, mgr, nil, registry, newTestLogger())
	agentID := uuid.New().String()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/revoke", nil)
	testAgentsRouter(h).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, registry.lastSent)
	assert.Equal(t, agentID, registry.sentTo)
	revoke := registry.lastSent.GetRevoke()
	require.NotNil(t, revoke)
	assert.Equal(t, "revoked_by_admin", revoke.GetReason())
}

func TestAgentsHandler_Revoke_DoesNotSendCommandWhenOffline(t *testing.T) {
	mgr := &mockAgentMgr{}
	registry := &mockAgentRegistry{online: false, sendOK: true}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, mgr, nil, registry, newTestLogger())
	agentID := uuid.New().String()

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+agentID+"/revoke", nil)
	testAgentsRouter(h).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Nil(t, registry.lastSent)
}

// ── ListDir ───────────────────────────────────────────────────────────────────

// Without a DirStore wired the handler falls back to the legacy fire-and-forget
// 202 behaviour so that older deployments keep working without configuration.
func TestAgentsHandler_ListDir_NoDirStore_AgentOnline_Returns202(t *testing.T) {
	registry := &mockAgentRegistry{online: true, sendOK: true}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, registry, newTestLogger())
	body := `{"path":"/data","recursive":true}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/list-dir", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusAccepted, w.Code)
}

// With a DirStore wired and the agent responding immediately the handler should
// return 200 with the directory entries.
func TestAgentsHandler_ListDir_WithDirStore_AgentOnline_Returns200WithEntries(t *testing.T) {
	registry := &mockAgentRegistry{online: true, sendOK: true}
	sz := int64(1024)
	ts := "2025-01-01T00:00:00Z"
	store := &mockDirStore{result: &dirstore.Result{
		Entries: []dirstore.DirEntry{
			{Name: "file.csv", Path: "/data/file.csv", IsDir: false, Size: &sz, ModifiedAt: &ts},
		},
	}}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, registry, newTestLogger())
	h.WithDirStore(store)
	body := `{"path":"/data"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/list-dir", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	entries := resp["entries"].([]interface{})
	require.Len(t, entries, 1)
	entry := entries[0].(map[string]interface{})
	assert.Equal(t, "file.csv", entry["name"])
	assert.Equal(t, "/data/file.csv", entry["path"])
	assert.Equal(t, false, entry["is_dir"])
}

// When the agent reports an error the handler returns 502 with the agent's
// error message in the response body.
func TestAgentsHandler_ListDir_WithDirStore_AgentError_Returns502(t *testing.T) {
	registry := &mockAgentRegistry{online: true, sendOK: true}
	store := &mockDirStore{result: &dirstore.Result{Error: "permission denied"}}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, registry, newTestLogger())
	h.WithDirStore(store)
	body := `{"path":"/root"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/list-dir", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadGateway, w.Code)
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
	body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"*.log","dest_path_template":"logs/"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestAgentsHandler_CreateRule_InvalidBucketID(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	body := `{"bucket_id":"bad","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"*.log","dest_path_template":"logs/"}`
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
	h := handler.NewAgentsHandler(&mockAgentsDB{logs: []*db.UploadLog{newSampleLog()}, logsCount: 1}, nil, nil, nil, newTestLogger())
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

func TestAgentsHandler_DeleteRule_InvalidRuleID(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	// The handler only validates the rule ID (rid), not the agent ID.
	req, _ := http.NewRequest(http.MethodDelete, "/api/v1/agents/"+uuid.New().String()+"/rules/not-a-uuid", nil)
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

func TestAgentsHandler_ListRules_ItemsEnvelope(t *testing.T) {
	rule := &db.CollectionRule{
		ID:                 uuid.New(),
		AgentID:            uuid.New(),
		BucketID:           uuid.New(),
		Status:             db.RuleStatusActive,
		Mode:               db.UploadModeWatch,
		SourcePathTemplate: "/data",
		FileGlob:           "*.log",
		UploadPathTemplate: "logs/",
		RunOnceOnStart:     true,
		Metadata:           json.RawMessage(`{}`),
		CreatedAt:          time.Now(),
		UpdatedAt:          time.Now(),
	}
	h := handler.NewAgentsHandler(&mockAgentsDB{rules: []*db.CollectionRule{rule}}, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/"+uuid.New().String()+"/rules", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	items := body["items"].([]interface{})
	require.Len(t, items, 1)
	item := items[0].(map[string]interface{})
	assert.Equal(t, true, item["is_active"])
	assert.Equal(t, true, item["run_once_on_start"])
	assert.Equal(t, "/data", item["base_path"])
	assert.Equal(t, "*.log", item["path_pattern"])
	assert.Equal(t, "logs/", item["dest_path_template"])
	assert.Equal(t, rule.BucketID.String(), item["bucket_id"])
}

func TestAgentsHandler_ListUploadLogs_ItemsEnvelope(t *testing.T) {
	log := newSampleLog()
	h := handler.NewAgentsHandler(&mockAgentsDB{logs: []*db.UploadLog{log}, logsCount: 1}, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/"+uuid.New().String()+"/upload-logs", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	items := body["items"].([]interface{})
	assert.Len(t, items, 1)
	assert.Equal(t, float64(1), body["total"])
	assert.Equal(t, false, body["has_more"])
}

// ── is_online field ───────────────────────────────────────────────────────────

// mockAgentCacheClient satisfies handler.AgentCacheClient for tests.
type mockAgentCacheClient struct {
	exists int64
}

func (m *mockAgentCacheClient) Exists(_ context.Context, _ ...string) (int64, error) {
	return m.exists, nil
}

func TestAgentsHandler_List_IsOnline_True_WhenCacheHasKey(t *testing.T) {
	a := newSampleAgent()
	mockDB := &mockAgentsDB{agents: []*db.Agent{a}}
	h := handler.NewAgentsHandler(mockDB, nil, nil, nil, newTestLogger())
	h.WithCache(&mockAgentCacheClient{exists: 1})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	items := body["items"].([]interface{})
	item := items[0].(map[string]interface{})
	assert.Equal(t, true, item["is_online"])
}

func TestAgentsHandler_List_IsOnline_False_WhenCacheMisses(t *testing.T) {
	a := newSampleAgent()
	mockDB := &mockAgentsDB{agents: []*db.Agent{a}}
	h := handler.NewAgentsHandler(mockDB, nil, nil, nil, newTestLogger())
	h.WithCache(&mockAgentCacheClient{exists: 0})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	items := body["items"].([]interface{})
	item := items[0].(map[string]interface{})
	assert.Equal(t, false, item["is_online"])
}

func TestAgentsHandler_List_IsOnline_False_WhenNoCache(t *testing.T) {
	a := newSampleAgent()
	mockDB := &mockAgentsDB{agents: []*db.Agent{a}}
	h := handler.NewAgentsHandler(mockDB, nil, nil, nil, newTestLogger())
	// No WithCache call → cache is nil

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	items := body["items"].([]interface{})
	item := items[0].(map[string]interface{})
	assert.Equal(t, false, item["is_online"])
}

// ── ListDir Redis fallback ─────────────────────────────────────────────────────

// When the in-memory registry says offline but Redis reports the TTL key exists,
// the handler passes the online check and proceeds to send the command.
// With no dirStore wired it falls back to 202; with sendOK=false it gets 409.
func TestAgentsHandler_ListDir_AgentOnline_InRedisNotMemory_Returns409OnSendFail(t *testing.T) {
	// Registry says offline but Redis cache says online → should succeed.
	registry := &mockAgentRegistry{online: false}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, registry, newTestLogger())
	h.WithCache(&mockAgentCacheClient{exists: 1})

	body := `{"path":"/data"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/list-dir", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	// Registry.Send will be called but registry.sendOK is false → 409.
	// The important thing is we didn't get 409 for "offline" — we got past the guard.
	// With sendOK=false the registry.Send returns false and we get 409 from send failure.
	assert.Equal(t, http.StatusConflict, w.Code)
	// Read body to confirm it's the send-fail 409, not the "agent is not online" 409.
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errBody := resp["error"].(map[string]interface{})
	assert.Equal(t, "AGENT_OFFLINE", errBody["code"])
}

func TestAgentsHandler_ListDir_AgentOfflineNoCache_Returns409(t *testing.T) {
	registry := &mockAgentRegistry{online: false}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, registry, newTestLogger())
	h.WithCache(&mockAgentCacheClient{exists: 0})

	body := `{"path":"/data"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/list-dir", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusConflict, w.Code)
}
