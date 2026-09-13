package handler_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/dirstore"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/sqlc-dev/pqtype"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	fullUpdateErr error
	renameErr     error
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
func (m *mockAgentsDB) UpdateAgentName(_ context.Context, id uuid.UUID, name string) (*db.Agent, error) {
	if m.renameErr != nil {
		return nil, m.renameErr
	}
	return &db.Agent{ID: id, Name: name, Status: db.AgentStatusApproved, OsInfo: json.RawMessage(`{}`), Metadata: json.RawMessage(`{}`)}, nil
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
		ID:               uuid.New(),
		OrgID:            arg.OrgID,
		AgentID:          arg.AgentID,
		BucketID:         arg.BucketID,
		Name:             arg.Name,
		Mode:             arg.Mode,
		Status:           arg.Status,
		BasePath:         arg.BasePath,
		PathPattern:      arg.PathPattern,
		DestPathTemplate: arg.DestPathTemplate,
		Recursive:        arg.Recursive,
		CronExpr:         arg.CronExpr,
		RunOnceOnStart:   arg.RunOnceOnStart,
		AppendMode:       arg.AppendMode,
		Metadata:         arg.Metadata,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
	}, nil
}
func (m *mockAgentsDB) UpdateCollectionRuleStatus(_ context.Context, id uuid.UUID, status db.RuleStatus) (*db.CollectionRule, error) {
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	// The real DB returns the FULL stored row. Cherry-picking fields here makes
	// the mock looser than production and silently hides response-shape bugs
	// (review round 6), so copy the whole stored rule and override only what
	// this statement actually changes.
	r := &db.CollectionRule{
		ID:        id,
		AgentID:   uuid.New(),
		Metadata:  json.RawMessage(`{}`),
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
	}
	if m.rule != nil {
		cp := *m.rule
		r = &cp
	}
	r.ID = id
	r.Status = status
	if len(r.Metadata) == 0 {
		r.Metadata = json.RawMessage(`{}`)
	}
	return r, nil
}
func (m *mockAgentsDB) UpdateCollectionRule(_ context.Context, arg db.UpdateCollectionRuleParams) (*db.CollectionRule, error) {
	if m.fullUpdateErr != nil {
		return nil, m.fullUpdateErr
	}
	return &db.CollectionRule{
		ID:               arg.ID,
		AgentID:          uuid.New(),
		BucketID:         arg.BucketID,
		Name:             arg.Name,
		Mode:             arg.Mode,
		Status:           arg.Status,
		BasePath:         arg.BasePath,
		PathPattern:      arg.PathPattern,
		DestPathTemplate: arg.DestPathTemplate,
		Recursive:        arg.Recursive,
		CronExpr:         arg.CronExpr,
		RunOnceOnStart:   arg.RunOnceOnStart,
		AppendMode:       arg.AppendMode,
		Metadata:         arg.Metadata,
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
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
	dispatchErr    error
	cancelErr      error
	dispatched     int
	cancelled      int
	lastDispatched *db.CollectionRule
}

func (m *mockDispatcher) DispatchRule(_ context.Context, rule *db.CollectionRule) error {
	m.dispatched++
	m.lastDispatched = rule
	return m.dispatchErr
}
func (m *mockDispatcher) DispatchRuleCancel(_ context.Context, _, _ string) error {
	m.cancelled++
	return m.cancelErr
}

type mockAgentRegistry struct {
	online          bool
	sendOK          bool
	sentTo          string
	lastSent        *agentv1.ServerMessage
	disconnectCalls int
}

func (m *mockAgentRegistry) Send(agentID string, msg *agentv1.ServerMessage) bool {
	m.sentTo = agentID
	m.lastSent = msg
	return m.sendOK
}
func (m *mockAgentRegistry) IsOnline(_ string) bool { return m.online }
func (m *mockAgentRegistry) Disconnect(_ string) bool {
	m.disconnectCalls++
	return m.online
}

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
		ID:        uuid.New(),
		OrgID:     uuid.New(),
		Name:      "edge-01",
		Status:    db.AgentStatusApproved,
		IpAddress: pqtype.Inet{},
		OsInfo:    json.RawMessage(`{}`),
		Metadata:  json.RawMessage(`{}`),
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
	v1.PATCH("/agents/:id", h.Rename)
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

// ── Rename ────────────────────────────────────────────────────────────────────

func patchAgentName(t *testing.T, h *handler.AgentsHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPatch, "/api/v1/agents/"+uuid.New().String(), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	return w
}

func TestAgentsHandler_Rename_Success(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	w := patchAgentName(t, h, `{"name":"生产传感器-01"}`)
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "生产传感器-01", body["name"])
}

func TestAgentsHandler_Rename_TrimsWhitespace(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	w := patchAgentName(t, h, `{"name":"  edge-1  "}`)
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "edge-1", body["name"])
}

func TestAgentsHandler_Rename_Empty_422(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	// Whitespace-only trims to empty → 422 (binding:required passes on non-empty string).
	w := patchAgentName(t, h, `{"name":"   "}`)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestAgentsHandler_Rename_TooLong_422(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	long := strings.Repeat("a", 65)
	w := patchAgentName(t, h, `{"name":"`+long+`"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestAgentsHandler_Rename_SpecialChars_422(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	// A "/" would corrupt {agent_name} path templates.
	w := patchAgentName(t, h, `{"name":"bad/name"}`)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestAgentsHandler_Rename_NotFound_404(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{renameErr: sql.ErrNoRows}, nil, nil, nil, newTestLogger())
	w := patchAgentName(t, h, `{"name":"whatever"}`)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

func TestAgentsHandler_Rename_NonAdmin_403(t *testing.T) {
	// The route is gated by RequireRole("super_admin"); a non-admin caller is
	// rejected before the handler runs.
	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, "operator", uuid.New().String(), uuid.New().String())
		c.Next()
	})
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	r.PATCH("/api/v1/agents/:id", middleware.RequireRole("super_admin"), h.Rename)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPatch, "/api/v1/agents/"+uuid.New().String(), bytes.NewBufferString(`{"name":"x"}`))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
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

	// The command above is cooperative — a compromised agent ignores it and
	// keeps heartbeating, so the stream has to be cut as well (IC-BUG-25).
	assert.Equal(t, 1, registry.disconnectCalls,
		"revocation must cut the stream, not only ask the agent to stand down")
}

// Revocation must still succeed when the agent is already gone; there is simply
// nothing to cut.
func TestAgentsHandler_Revoke_Offline_DoesNotDisconnect(t *testing.T) {
	registry := &mockAgentRegistry{online: false, sendOK: true}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, &mockAgentMgr{}, nil, registry, newTestLogger())

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost,
		"/api/v1/agents/"+uuid.New().String()+"/revoke", nil)
	testAgentsRouter(h).ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Zero(t, registry.disconnectCalls)
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
	// Metadata must round-trip in the response so the rule form can prefill it on
	// edit (otherwise an update overwrites the stored metadata with {}).
	meta := json.RawMessage(`{"file_type":"pressure","static_tags":{"vendor":"omron"}}`)
	rule := &db.CollectionRule{ID: uuid.New(), AgentID: uuid.New(), Status: db.RuleStatusActive, Metadata: meta, CreatedAt: time.Now(), UpdatedAt: time.Now()}
	h := handler.NewAgentsHandler(&mockAgentsDB{rules: []*db.CollectionRule{rule}}, nil, nil, nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/"+uuid.New().String()+"/rules", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
	// Assert the metadata structurally (not a raw substring) so the test is
	// resilient to key ordering / formatting.
	var body struct {
		Items []struct {
			Metadata struct {
				FileType   string            `json:"file_type"`
				StaticTags map[string]string `json:"static_tags"`
			} `json:"metadata"`
		} `json:"items"`
	}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	require.Len(t, body.Items, 1)
	assert.Equal(t, "pressure", body.Items[0].Metadata.FileType)
	assert.Equal(t, "omron", body.Items[0].Metadata.StaticTags["vendor"])
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

// IC-BUG-50 / D-034: creating a rule whose dest_path_template uses the
// deprecated {time} reserved word must still succeed (the agent accepts the
// alias), but the response must carry a readable deprecation hint pointing at
// {submit_time}.
func TestAgentsHandler_CreateRule_DeprecatedTimeTemplate_Warns(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, &mockDispatcher{}, nil, newTestLogger())
	body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"*.log","dest_path_template":"{time:yyyy/MM/dd}/{filename}"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	warnings, ok := resp["warnings"].([]interface{})
	require.True(t, ok, "deprecated {time} template must produce a warnings array")
	joined := fmt.Sprint(warnings)
	assert.Contains(t, joined, "deprecated")
	assert.Contains(t, joined, "submit_time")
}

// The new reserved word must not trigger the deprecation warning, nor must a
// field name that merely shares the {time prefix ({time_zone}).
func TestAgentsHandler_CreateRule_NonDeprecatedTemplates_NoWarning(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, &mockDispatcher{}, nil, newTestLogger())
	for _, tpl := range []string{"{submit_time:yyyy/MM/dd}/{filename}", "{time_zone}/{filename}"} {
		body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"*.log","dest_path_template":"` + tpl + `"}`
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		testAgentsRouter(h).ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code, tpl)
		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		assert.Nil(t, resp["warnings"], "template %s must not warn", tpl)
	}
}

// Review C2: the warnings must cover path_pattern too — the agent's gate
// refuses a pattern that declares a reserved word as a non-time field, so a
// rule the CP lets through would only fail at upload time. The warning names
// the field (parallel to the agent's refusePathField wording).
func TestAgentsHandler_CreateRule_PatternReservedTypedNonTime_Warns(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, &mockDispatcher{}, nil, newTestLogger())
	body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"of/{time:s}/{filename}","dest_path_template":"data/{filename}"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	warnings, ok := resp["warnings"].([]interface{})
	require.True(t, ok, "pattern misuse must produce a warnings array")
	joined := fmt.Sprint(warnings)
	assert.Contains(t, joined, "path_pattern")
	assert.Contains(t, joined, "LDML")
}

// The deprecated alias in path_pattern also warns (it renders fine, but the
// rule should migrate to the declared name).
func TestAgentsHandler_CreateRule_DeprecatedTimeInPattern_Warns(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, &mockDispatcher{}, nil, newTestLogger())
	body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"of/{time:yyyy}/{filename}","dest_path_template":"data/{time:yyyy}/{filename}"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	warnings, ok := resp["warnings"].([]interface{})
	require.True(t, ok, "deprecated {time} in path_pattern must warn")
	joined := fmt.Sprint(warnings)
	assert.Contains(t, joined, "path_pattern uses the deprecated reserved word")
}

// Review D1: timezone validity is Go-authoritative (time.LoadLocation inside
// trollsift.New) — the webui only checks kind/basic syntax and lets these
// through, so the CP must catch them at save time or the admin only finds out
// at upload. Measured New() errors: unknown zone, trailing space, repeated tz.
func TestAgentsHandler_CreateRule_InvalidTimezone_Warns(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, &mockDispatcher{}, nil, newTestLogger())
	for _, tpl := range []string{
		"{time:yyyy|tz=Nope/Bad}/{filename}",
		"{time:yyyy|tz=Asia/Shanghai }/{filename}",
		"{time:yyyy|tz=UTC|tz=UTC}/{filename}",
	} {
		body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"*.log","dest_path_template":"` + tpl + `"}`
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		testAgentsRouter(h).ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code, tpl)
		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		warnings, ok := resp["warnings"].([]interface{})
		require.True(t, ok, "invalid timezone in %s must produce a warnings array", tpl)
		assert.Contains(t, fmt.Sprint(warnings), "invalid timezone", tpl)
	}
	// A valid timezone must not warn (note: {time:...} would legitimately warn as
// the deprecated alias — use the declared {submit_time} name).
	body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"*.log","dest_path_template":"{submit_time:yyyy|tz=Asia/Shanghai}/{filename}"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Nil(t, resp["warnings"], "valid timezone must not warn")
}

// The same Go-authoritative check covers path_pattern (trollsift patterns
// only — glob patterns must not be fed to New).
func TestAgentsHandler_CreateRule_InvalidTimezoneInPattern_Warns(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, &mockDispatcher{}, nil, newTestLogger())
	body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"of/{time:yyyy|tz=Nope/Bad}/{filename}","dest_path_template":"data/{filename}"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	warnings, ok := resp["warnings"].([]interface{})
	require.True(t, ok, "invalid timezone in path_pattern must produce a warnings array")
	joined := fmt.Sprint(warnings)
	assert.Contains(t, joined, "path_pattern is not a valid trollsift pattern")
	assert.Contains(t, joined, "invalid timezone")
}

// Review D2: the two fields' deprecation hints must use field-appropriate
// verbs — dest_path_template does not parse, it only composes.
func TestAgentsHandler_CreateRule_DeprecatedTime_DestHintDoesNotSayParses(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, &mockDispatcher{}, nil, newTestLogger())
	body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"*.log","dest_path_template":"{time:yyyy/MM/dd}/{filename}"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusCreated, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	warnings, ok := resp["warnings"].([]interface{})
	require.True(t, ok)
	assert.NotContains(t, fmt.Sprint(warnings), "dest_path_template parses")
	assert.NotContains(t, fmt.Sprint(warnings), "dest_path_template still parses")
}

// Review E1: the status-only PUT (enable/disable toggle) must carry the same
// contract warnings — activating a rule whose template is deprecated or
// misuses a reserved word must not silently succeed with no signal.
func TestAgentsHandler_UpdateRule_StatusOnly_ReturnsTemplateWarnings(t *testing.T) {
	rule := &db.CollectionRule{
		ID:               uuid.New(),
		AgentID:          uuid.New(),
		Status:           db.RuleStatusInactive,
		DestPathTemplate: "{time:yyyy}/{filename}",
		CreatedAt:        time.Now(),
	}
	h := handler.NewAgentsHandler(&mockAgentsDB{rule: rule}, nil, &mockDispatcher{}, nil, newTestLogger())
	w := putRule(t, h, `{"status":"active"}`)
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	warnings, ok := resp["warnings"].([]interface{})
	require.True(t, ok, "status-only activation of a rule with deprecated {time} must warn")
	assert.Contains(t, fmt.Sprint(warnings), "dest_path_template uses the deprecated reserved word")
}

func TestAgentsHandler_UpdateRule_FullUpdate_DeprecatedTimeTemplate_Warns(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, &mockDispatcher{}, nil, newTestLogger())
	body := `{"name":"edited","bucket_id":"` + uuid.New().String() +
		`","mode":"watch","base_path":"/data","path_pattern":"*","dest_path_template":"{time:yyyy}/{filename}","enabled":true}`
	w := putRule(t, h, body)
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	warnings, ok := resp["warnings"].([]interface{})
	require.True(t, ok, "deprecated {time} template must produce a warnings array")
	assert.Contains(t, fmt.Sprint(warnings), "submit_time")
}

// Review P1-B: a bare reserved time field ({submit_time} or {time} without
// LDML) can never compose — every upload would fail. Creation still succeeds
// (D-030 §8: no template shape constraints), but the response must carry a
// readable warning pointing at the LDML form.
func TestAgentsHandler_CreateRule_BareReservedTimeTemplate_Warns(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, &mockDispatcher{}, nil, newTestLogger())
	for _, tpl := range []string{"{submit_time}/{filename}", "{time}/{filename}"} {
		body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"*.log","dest_path_template":"` + tpl + `"}`
		w := httptest.NewRecorder()
		req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
		req.Header.Set("Content-Type", "application/json")
		testAgentsRouter(h).ServeHTTP(w, req)
		require.Equal(t, http.StatusCreated, w.Code, tpl)
		var resp map[string]interface{}
		require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
		warnings, ok := resp["warnings"].([]interface{})
		require.True(t, ok, "bare reserved time template %s must produce a warnings array", tpl)
		assert.Contains(t, fmt.Sprint(warnings), "LDML", tpl)
	}
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

func putRule(t *testing.T, h *handler.AgentsHandler, body string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut,
		"/api/v1/agents/"+uuid.New().String()+"/rules/"+uuid.New().String(), bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	return w
}

func fullRuleBody(t *testing.T) string {
	t.Helper()
	return `{"name":"edited","bucket_id":"` + uuid.New().String() +
		`","mode":"scheduled","base_path":"/data2","path_pattern":"*.csv",` +
		`"dest_path_template":"out/{filename}","recursive":true,"cron_expr":"0 * * * *",` +
		`"run_once_on_start":true,"append_mode":"overwrite","enabled":true}`
}

func TestAgentsHandler_UpdateRule_FullUpdate_Success(t *testing.T) {
	dispatcher := &mockDispatcher{}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, dispatcher, nil, newTestLogger())
	w := putRule(t, h, fullRuleBody(t))
	require.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "edited", body["name"])
	assert.Equal(t, "scheduled", body["mode"])
	assert.Equal(t, "overwrite", body["append_mode"])
	// An active result must hot-reload via DispatchRule, not cancel.
	assert.Equal(t, 1, dispatcher.dispatched)
	assert.Equal(t, 0, dispatcher.cancelled)
}

func TestAgentsHandler_UpdateRule_FullUpdate_Disable(t *testing.T) {
	// enabled:false must map to inactive status and trigger a cancel dispatch.
	dispatcher := &mockDispatcher{}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, dispatcher, nil, newTestLogger())
	body := `{"name":"edited","bucket_id":"` + uuid.New().String() +
		`","mode":"watch","base_path":"/d","path_pattern":"*","dest_path_template":"x/","enabled":false}`
	w := putRule(t, h, body)
	require.Equal(t, http.StatusOK, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.Equal(t, false, resp["enabled"])
	// An inactive result must cancel the dispatch, not re-dispatch.
	assert.Equal(t, 1, dispatcher.cancelled)
	assert.Equal(t, 0, dispatcher.dispatched)
}

func TestAgentsHandler_UpdateRule_EmptyName_IsFullUpdateValidationError(t *testing.T) {
	// A present-but-empty "name" selects the full-update path and must be a 422
	// validation error, not a silent fallback to the status toggle.
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	w := putRule(t, h, `{"name":""}`)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, "VALIDATION_ERROR", body["error"].(map[string]interface{})["code"])
}

func TestAgentsHandler_UpdateRule_NullName_TakesStatusPath(t *testing.T) {
	// {"name": null} unmarshals to nil (same as absent) and takes the status-only
	// path — here with a valid status, so it succeeds as a toggle.
	dispatcher := &mockDispatcher{}
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, dispatcher, nil, newTestLogger())
	w := putRule(t, h, `{"name":null,"status":"inactive"}`)
	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, 1, dispatcher.cancelled)
}

func TestAgentsHandler_UpdateRule_FullUpdate_MissingField(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	// name present (→ full-update path) but base_path missing.
	body := `{"name":"edited","bucket_id":"` + uuid.New().String() +
		`","mode":"watch","path_pattern":"*","dest_path_template":"x/"}`
	w := putRule(t, h, body)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestAgentsHandler_UpdateRule_FullUpdate_InvalidMode(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	body := `{"name":"edited","bucket_id":"` + uuid.New().String() +
		`","mode":"bogus","base_path":"/d","path_pattern":"*","dest_path_template":"x/"}`
	w := putRule(t, h, body)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
}

func TestAgentsHandler_CreateRule_TailMode_422(t *testing.T) {
	// IC-BUG-46 fail-closed: append_mode=tail currently replaces the whole
	// object with the appended increment (silent data loss), so rule creation
	// must reject it with a readable 422 until IC-15 lands.
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	body := `{"bucket_id":"` + uuid.New().String() + `","name":"rule1","mode":"watch","base_path":"/data","path_pattern":"*.log","dest_path_template":"logs/","append_mode":"tail"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]interface{})
	assert.Equal(t, "VALIDATION_ERROR", errObj["code"])
	msg, _ := errObj["message"].(string)
	assert.Contains(t, msg, "tail", "the rejection must explain why tail is refused")
}

func TestAgentsHandler_UpdateRule_FullUpdate_InvalidBucketID(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	body := `{"name":"edited","bucket_id":"not-a-uuid","mode":"watch","base_path":"/d","path_pattern":"*","dest_path_template":"x/"}`
	w := putRule(t, h, body)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestAgentsHandler_UpdateRule_FullUpdate_TailMode_422(t *testing.T) {
	// IC-BUG-46 fail-closed: a full-field update switching the rule to
	// append_mode=tail must be rejected with the same readable 422 as creation.
	h := handler.NewAgentsHandler(&mockAgentsDB{}, nil, nil, nil, newTestLogger())
	body := `{"name":"edited","bucket_id":"` + uuid.New().String() +
		`","mode":"watch","base_path":"/d","path_pattern":"*","dest_path_template":"x/","append_mode":"tail"}`
	w := putRule(t, h, body)
	require.Equal(t, http.StatusUnprocessableEntity, w.Code)
	var resp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	errObj := resp["error"].(map[string]interface{})
	assert.Equal(t, "VALIDATION_ERROR", errObj["code"])
	msg, _ := errObj["message"].(string)
	assert.Contains(t, msg, "tail", "the rejection must explain why tail is refused")
}

func TestAgentsHandler_UpdateRule_FullUpdate_NotFound(t *testing.T) {
	h := handler.NewAgentsHandler(&mockAgentsDB{fullUpdateErr: sql.ErrNoRows}, nil, nil, nil, newTestLogger())
	w := putRule(t, h, fullRuleBody(t))
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
		ID:               uuid.New(),
		AgentID:          uuid.New(),
		BucketID:         uuid.New(),
		Status:           db.RuleStatusActive,
		Mode:             db.UploadModeWatch,
		BasePath:         "/data",
		PathPattern:      "*.log",
		DestPathTemplate: "logs/",
		RunOnceOnStart:   true,
		Metadata:         json.RawMessage(`{}`),
		CreatedAt:        time.Now(),
		UpdatedAt:        time.Now(),
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
	assert.Equal(t, true, item["enabled"])
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
	// existsKeys, when non-nil, is consulted per key first (a key absent from
	// it falls back to exists) so tests can distinguish cache keys.
	existsKeys map[string]int64
	// statsJSON, when non-empty, is returned by Get for any key (the stats blob).
	statsJSON string
	getErr    error
}

func (m *mockAgentCacheClient) Exists(_ context.Context, keys ...string) (int64, error) {
	if m.existsKeys != nil && len(keys) > 0 {
		return m.existsKeys[keys[0]], nil
	}
	return m.exists, nil
}

func (m *mockAgentCacheClient) Get(_ context.Context, _ string) (string, error) {
	return m.statsJSON, m.getErr
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

func TestAgentsHandler_List_EnrichesTelemetry_FromCache(t *testing.T) {
	a := newSampleAgent()
	mockDB := &mockAgentsDB{agents: []*db.Agent{a}}
	h := handler.NewAgentsHandler(mockDB, nil, nil, nil, newTestLogger())
	h.WithCache(&mockAgentCacheClient{
		exists:    1,
		statsJSON: `{"queue_depth":5,"uptime_seconds":900,"version":"0.1.0"}`,
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	item := body["items"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, float64(5), item["queue_depth"])
	assert.Equal(t, float64(900), item["uptime_seconds"])
	assert.Equal(t, "0.1.0", item["agent_version"])
}

// TestAgentsHandler_List_NoTelemetry_WhenNoStats verifies the live-telemetry
// fields are omitted (not misleading zeros) when the agent has no cached stats.
func TestAgentsHandler_List_NoTelemetry_WhenNoStats(t *testing.T) {
	a := newSampleAgent()
	mockDB := &mockAgentsDB{agents: []*db.Agent{a}}
	h := handler.NewAgentsHandler(mockDB, nil, nil, nil, newTestLogger())
	h.WithCache(&mockAgentCacheClient{exists: 0, statsJSON: ""})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	item := body["items"].([]interface{})[0].(map[string]interface{})
	_, hasQD := item["queue_depth"]
	_, hasUp := item["uptime_seconds"]
	assert.False(t, hasQD, "queue_depth should be omitted without stats")
	assert.False(t, hasUp, "uptime_seconds should be omitted without stats")
}

// TestAgentsHandler_List_NoTelemetry_WhenOffline verifies that a stale stats key
// is not surfaced when the agent is offline (online key absent) — telemetry is
// gated on is_online so the API never contradicts itself under cache desync.
func TestAgentsHandler_List_NoTelemetry_WhenOffline(t *testing.T) {
	a := newSampleAgent()
	mockDB := &mockAgentsDB{agents: []*db.Agent{a}}
	h := handler.NewAgentsHandler(mockDB, nil, nil, nil, newTestLogger())
	h.WithCache(&mockAgentCacheClient{
		exists:    0, // offline
		statsJSON: `{"queue_depth":5,"uptime_seconds":900,"version":"0.1.0"}`,
	})

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents", nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	item := body["items"].([]interface{})[0].(map[string]interface{})
	assert.Equal(t, false, item["is_online"])
	_, hasQD := item["queue_depth"]
	assert.False(t, hasQD, "offline agent must not surface stale telemetry")
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

// ── F3（IC-2b review）：单 agent 规则数硬上限（快照体积不可能超限的第一道闸）───

// 创建第 1001 条规则被拒绝（422），错误信息说明上限与理由——快照同步整条发送，
// 规则数不受控时快照会超过 gRPC 4MiB 上限并退化为降级状态。
func TestAgentsHandler_CreateRule_RuleCountCap(t *testing.T) {
	rules := make([]*db.CollectionRule, handler.MaxRulesPerAgent)
	for i := range rules {
		rules[i] = &db.CollectionRule{ID: uuid.New(), AgentID: uuid.New(), Status: db.RuleStatusActive}
	}
	dispatcher := &mockDispatcher{}
	h := handler.NewAgentsHandler(&mockAgentsDB{rules: rules}, nil, dispatcher, nil, newTestLogger())
	body := `{"bucket_id":"` + uuid.New().String() + `","name":"one-too-many","mode":"watch","base_path":"/data","path_pattern":"*.log","dest_path_template":"logs/"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code)
	assert.Contains(t, w.Body.String(), "rule count limit")
}

// ── R3（IC-2b review 三轮）：创建闸从条数改为序列化字节数 ──────────────────────

// 条数闸拦不住「单条含 3MiB TEXT 字段」的规则——201 条就越过 4MiB（codex 实测）。
// 真正对应失败条件的量是序列化后的字节：预估（含新规则）超预算即 422。
func TestAgentsHandler_CreateRule_SnapshotSizeLimit(t *testing.T) {
	dispatcher := &mockDispatcher{}
	h := handler.NewAgentsHandler(&mockAgentsDB{rules: nil}, nil, dispatcher, nil, newTestLogger())
	huge := strings.Repeat("x", 4<<20)
	body := `{"bucket_id":"` + uuid.New().String() + `","name":"huge","mode":"watch","base_path":"/data","path_pattern":"*.log","dest_path_template":"` + huge + `"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/agents/"+uuid.New().String()+"/rules", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testAgentsRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnprocessableEntity, w.Code, "a rule whose serialized size alone exceeds the snapshot budget must be rejected")
	assert.Contains(t, w.Body.String(), "snapshot size")
}

// 降级标记可被 API 读取：agentResponse.rule_sync_degraded 来自缓存键。
func TestAgentsHandler_Get_RuleSyncDegradedFlag(t *testing.T) {
	a := newSampleAgent()
	mockDB := &mockAgentsDB{agent: a}
	cacheMock := &mockAgentCacheClient{existsKeys: map[string]int64{
		cache.AgentSyncDegradedKey(a.ID.String()): 1,
	}}
	h := handler.NewAgentsHandler(mockDB, nil, nil, nil, newTestLogger()).WithCache(cacheMock)
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/agents/"+a.ID.String(), nil)
	testAgentsRouter(h).ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)
	var out map[string]any
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &out))
	assert.Equal(t, true, out["rule_sync_degraded"],
		"the degraded rule-sync state must be readable through the API, not only logged")
}
