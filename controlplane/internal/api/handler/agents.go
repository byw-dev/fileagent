package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/dirstore"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// AgentsDB is the minimal database interface needed by AgentsHandler.
type AgentsDB interface {
	ListAgents(ctx context.Context, orgID uuid.UUID) ([]*db.Agent, error)
	ListAgentsByStatus(ctx context.Context, orgID uuid.UUID, status db.AgentStatus) ([]*db.Agent, error)
	GetAgentByID(ctx context.Context, id uuid.UUID) (*db.Agent, error)
	ListCollectionRulesByAgent(ctx context.Context, agentID uuid.UUID) ([]*db.CollectionRule, error)
	GetCollectionRuleByID(ctx context.Context, id uuid.UUID) (*db.CollectionRule, error)
	CreateCollectionRule(ctx context.Context, arg db.CreateCollectionRuleParams) (*db.CollectionRule, error)
	UpdateCollectionRuleStatus(ctx context.Context, iD uuid.UUID, status db.RuleStatus) (*db.CollectionRule, error)
	UpdateCollectionRule(ctx context.Context, arg db.UpdateCollectionRuleParams) (*db.CollectionRule, error)
	DeleteCollectionRule(ctx context.Context, id uuid.UUID) error
	ListUploadLogs(ctx context.Context, arg db.ListUploadLogsParams) ([]*db.UploadLog, error)
	CountUploadLogs(ctx context.Context, f db.CountUploadLogsFilter) (int64, error)
}

// AgentManager manages agent approval/revocation lifecycle.
type AgentManager interface {
	ApproveAgent(ctx context.Context, agentID uuid.UUID, approvedByUserID uuid.UUID) (string, error)
	RevokeAgent(ctx context.Context, agentID uuid.UUID, revokedByUserID uuid.UUID) error
}

// RuleDispatcher sends collection rules to agents.
type RuleDispatcher interface {
	DispatchRule(ctx context.Context, rule *db.CollectionRule) error
	DispatchRuleCancel(ctx context.Context, ruleID, agentID string) error
}

// AgentRegistryClient queries online status and sends ServerMessages.
type AgentRegistryClient interface {
	Send(agentID string, msg *agentv1.ServerMessage) bool
	IsOnline(agentID string) bool
}

// AgentCacheClient is the cache interface used by AgentsHandler.
type AgentCacheClient interface {
	Exists(ctx context.Context, keys ...string) (int64, error)
	Get(ctx context.Context, key string) (string, error)
}

// agentStatsSnapshot mirrors the heartbeat telemetry cached by the gRPC server
// under cache.AgentStatsKey (see grpcserver.agentStats).
type agentStatsSnapshot struct {
	QueueDepth    int32   `json:"queue_depth"`
	UptimeSeconds int64   `json:"uptime_seconds"`
	UploadBps     float32 `json:"upload_bps"`
	Version       string  `json:"version"`
}

// DirListingStore manages pending directory listing result channels.
type DirListingStore interface {
	Register(requestID string) <-chan dirstore.Result
	Cancel(requestID string)
}

// DryRunStore manages pending dry-run result channels.
type DryRunStore interface {
	Register(reqID string) <-chan *agentv1.DryRunResult
	Cancel(reqID string)
}

// AgentsHandler groups the Agent management handlers.
type AgentsHandler struct {
	db          AgentsDB
	agentMgr    AgentManager
	dispatcher  RuleDispatcher
	registry    AgentRegistryClient
	cache       AgentCacheClient
	dirStore    DirListingStore
	dryRunStore DryRunStore
	logger      *zap.Logger
}

// NewAgentsHandler returns a new AgentsHandler. Nil arguments cause affected
// methods to return 501 until all dependencies are wired.
func NewAgentsHandler(agentsDB AgentsDB, agentMgr AgentManager, dispatcher RuleDispatcher, registry AgentRegistryClient, logger *zap.Logger) *AgentsHandler {
	return &AgentsHandler{
		db:         agentsDB,
		agentMgr:   agentMgr,
		dispatcher: dispatcher,
		registry:   registry,
		logger:     logger,
	}
}

// WithCache injects the cache client for real-time online status queries.
func (h *AgentsHandler) WithCache(c AgentCacheClient) *AgentsHandler {
	h.cache = c
	return h
}

// WithDirStore injects the directory-listing result store used to convert the
// async gRPC response into a synchronous REST reply.
func (h *AgentsHandler) WithDirStore(store DirListingStore) *AgentsHandler {
	h.dirStore = store
	return h
}

// WithDryRunStore injects the dry-run result store.
func (h *AgentsHandler) WithDryRunStore(store DryRunStore) *AgentsHandler {
	h.dryRunStore = store
	return h
}

// agentResponse is the outbound JSON shape for an agent.
type agentResponse struct {
	ID           string `json:"id"`
	OrgID        string `json:"org_id"`
	Name         string `json:"name"`
	Status       string `json:"status"`
	IsOnline     bool   `json:"is_online"`
	IpAddress    string `json:"ip_address,omitempty"`
	Hostname     string `json:"hostname,omitempty"`
	OsType       string `json:"os_type,omitempty"`
	OsVersion    string `json:"os_version,omitempty"`
	AgentVersion string `json:"agent_version,omitempty"`
	LastSeenAt   string `json:"last_seen_at,omitempty"`
	CreatedAt    string `json:"created_at,omitempty"`
	// Live telemetry from the latest heartbeat (present only while online).
	QueueDepth    *int32 `json:"queue_depth,omitempty"`
	UptimeSeconds *int64 `json:"uptime_seconds,omitempty"`
}

// mapFrontendStatusToDB converts a frontend AgentStatus (uppercase, using RUNNING
// for the online state) to the DB AgentStatus (lowercase).
func mapFrontendStatusToDB(frontendStatus string) db.AgentStatus {
	s := strings.ToLower(frontendStatus)
	if s == "running" {
		s = "online"
	}
	return db.AgentStatus(s)
}

// mapDBStatusToFrontend converts a DB AgentStatus (lowercase) to the frontend
// representation (uppercase, with "online" mapped to "RUNNING").
func mapDBStatusToFrontend(s db.AgentStatus) string {
	upper := strings.ToUpper(string(s))
	if upper == "ONLINE" {
		return "RUNNING"
	}
	return upper
}

// toAgentResponse converts a DB Agent to the outbound JSON shape. The caller
// should use toAgentResponseWithCache when a real-time is_online value is needed.
func toAgentResponse(a *db.Agent) agentResponse {
	r := agentResponse{
		ID:        a.ID.String(),
		OrgID:     a.OrgID.String(),
		Name:      a.Name,
		Status:    mapDBStatusToFrontend(a.Status),
		CreatedAt: a.CreatedAt.UTC().Format(time.RFC3339),
	}
	if a.IpAddress.Valid {
		r.IpAddress = a.IpAddress.IPNet.IP.String()
	}
	if len(a.OsInfo) > 0 {
		var osInfo map[string]interface{}
		if err := json.Unmarshal(a.OsInfo, &osInfo); err != nil {
			// Log at debug level; os_info fields will remain empty for this agent.
			zap.L().Debug("failed to unmarshal agent os_info",
				zap.String("agent_id", a.ID.String()),
				zap.Error(err),
			)
		} else {
			if v, ok := osInfo["hostname"].(string); ok {
				r.Hostname = v
			}
			if v, ok := osInfo["os_type"].(string); ok {
				r.OsType = v
			}
			if v, ok := osInfo["os_version"].(string); ok {
				r.OsVersion = v
			}
			if v, ok := osInfo["agent_version"].(string); ok {
				r.AgentVersion = v
			}
		}
	}
	if a.LastSeenAt.Valid {
		r.LastSeenAt = a.LastSeenAt.Time.UTC().Format(time.RFC3339)
	}
	return r
}

// toAgentResponseWithOnline enriches an agentResponse with real-time values from
// the cache: the is_online flag (Redis TTL key) and, when present, the latest
// heartbeat telemetry snapshot (queue depth, uptime).
func (h *AgentsHandler) toAgentResponseWithOnline(ctx context.Context, a *db.Agent) agentResponse {
	r := toAgentResponse(a)
	if h.cache == nil {
		return r
	}
	if n, err := h.cache.Exists(ctx, cache.AgentOnlineKey(a.ID.String())); err == nil {
		r.IsOnline = n > 0
	}
	// Only surface live telemetry for agents we consider online, so the fields
	// never contradict is_online under partial cache desync (e.g. the stats key
	// outliving the online key).
	if r.IsOnline {
		if raw, err := h.cache.Get(ctx, cache.AgentStatsKey(a.ID.String())); err == nil && raw != "" {
			var stats agentStatsSnapshot
			if err := json.Unmarshal([]byte(raw), &stats); err == nil {
				qd, up := stats.QueueDepth, stats.UptimeSeconds
				r.QueueDepth = &qd
				r.UptimeSeconds = &up
				if stats.Version != "" {
					r.AgentVersion = stats.Version
				}
			}
		}
	}
	return r
}

// List handles GET /api/v1/agents.
func (h *AgentsHandler) List(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)

	var agents []*db.Agent
	var err error

	if statusParam := c.Query("status"); statusParam != "" {
		agents, err = h.db.ListAgentsByStatus(c.Request.Context(), orgID, mapFrontendStatusToDB(statusParam))
	} else {
		agents, err = h.db.ListAgents(c.Request.Context(), orgID)
	}
	if err != nil {
		h.logger.Error("list agents", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list agents", nil)
		return
	}

	// agents is a slow-growth table (see DECISIONS.md D-007): return full list;
	// the frontend handles local pagination.
	resp := make([]agentResponse, 0, len(agents))
	for _, a := range agents {
		resp = append(resp, h.toAgentResponseWithOnline(c.Request.Context(), a))
	}
	c.JSON(http.StatusOK, gin.H{"items": resp, "total": len(resp)})
}

// Get handles GET /api/v1/agents/:id.
func (h *AgentsHandler) Get(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agent id", nil)
		return
	}
	agent, err := h.db.GetAgentByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "agent not found", nil)
			return
		}
		h.logger.Error("get agent", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to get agent", nil)
		return
	}
	c.JSON(http.StatusOK, h.toAgentResponseWithOnline(c.Request.Context(), agent))
}

// Approve handles POST /api/v1/agents/:id/approve.
func (h *AgentsHandler) Approve(c *gin.Context) {
	if h.agentMgr == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agent id", nil)
		return
	}
	callerID := callerIDFromClaims(c)
	token, err := h.agentMgr.ApproveAgent(c.Request.Context(), id, callerID)
	if err != nil {
		h.logger.Error("approve agent", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to approve agent", nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"token": token, "message": "agent approved"})
}

// Revoke handles POST /api/v1/agents/:id/revoke.
func (h *AgentsHandler) Revoke(c *gin.Context) {
	if h.agentMgr == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agent id", nil)
		return
	}
	callerID := callerIDFromClaims(c)
	if err := h.agentMgr.RevokeAgent(c.Request.Context(), id, callerID); err != nil {
		h.logger.Error("revoke agent", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to revoke agent", nil)
		return
	}
	if h.registry != nil && h.registry.IsOnline(id.String()) {
		h.registry.Send(id.String(), &agentv1.ServerMessage{
			Payload: &agentv1.ServerMessage_Revoke{
				Revoke: &agentv1.RevokeCommand{Reason: "revoked_by_admin"},
			},
		})
	}
	c.JSON(http.StatusOK, gin.H{"message": "agent revoked"})
}

// listDirRequest is the body expected by POST /api/v1/agents/:id/list-dir.
type listDirRequest struct {
	Path      string `json:"path"      binding:"required"`
	Recursive bool   `json:"recursive"`
	MaxDepth  int32  `json:"max_depth"`
}

// listDirResponse is the successful response body for POST /api/v1/agents/:id/list-dir.
type listDirResponse struct {
	Path    string              `json:"path"`
	Entries []dirstore.DirEntry `json:"entries"`
}

// ListDir handles POST /api/v1/agents/:id/list-dir.
// It sends a ListDirectoryCommand to the online agent and waits up to 30 s for
// the agent to send the result back over the gRPC stream before returning 200
// with the directory listing. Returns 409 when the agent is offline, 502 when
// the agent reports an error, and 504 on timeout.
func (h *AgentsHandler) ListDir(c *gin.Context) {
	if h.registry == nil {
		middleware.NotImplemented(c)
		return
	}
	agentID := c.Param("id")
	if _, err := uuid.Parse(agentID); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agent id", nil)
		return
	}

	var req listDirRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	inMemory := h.registry.IsOnline(agentID)
	if !inMemory {
		// Fallback: check Redis TTL key; the agent may have reconnected recently
		// but CP memory registry is empty (e.g. after a CP restart).
		inRedis := false
		if h.cache != nil {
			if n, err := h.cache.Exists(c.Request.Context(), cache.AgentOnlineKey(agentID)); err == nil && n > 0 {
				inRedis = true
			}
		}
		if !inRedis {
			middleware.RespondError(c, http.StatusConflict, "AGENT_OFFLINE", "agent is not online", nil)
			return
		}
	}

	requestID := uuid.New().String()

	// If no dirStore is wired (e.g. during migration / tests without it) fall
	// back to the original fire-and-forget 202 behaviour.
	if h.dirStore == nil {
		msg := &agentv1.ServerMessage{
			Payload: &agentv1.ServerMessage_ListDirectory{
				ListDirectory: &agentv1.ListDirectoryCommand{
					RequestId: requestID,
					Path:      req.Path,
					Recursive: req.Recursive,
					MaxDepth:  req.MaxDepth,
				},
			},
		}
		if !h.registry.Send(agentID, msg) {
			middleware.RespondError(c, http.StatusConflict, "AGENT_OFFLINE", "agent disconnected during send", nil)
			return
		}
		c.JSON(http.StatusAccepted, gin.H{"request_id": requestID, "message": "list directory command sent"})
		return
	}

	// Register the result channel BEFORE sending the command so we cannot miss
	// the agent's response.
	resultCh := h.dirStore.Register(requestID)
	defer h.dirStore.Cancel(requestID)

	msg := &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_ListDirectory{
			ListDirectory: &agentv1.ListDirectoryCommand{
				RequestId: requestID,
				Path:      req.Path,
				Recursive: req.Recursive,
				MaxDepth:  req.MaxDepth,
			},
		},
	}
	if !h.registry.Send(agentID, msg) {
		middleware.RespondError(c, http.StatusConflict, "AGENT_OFFLINE", "agent disconnected during send", nil)
		return
	}

	// Wait for the agent's DirectoryListing response with a 30 s deadline.
	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	select {
	case result := <-resultCh:
		if result.Error != "" {
			h.logger.Warn("list-dir: agent reported error",
				zap.String("agent_id", agentID),
				zap.String("request_id", requestID),
				zap.String("error", result.Error),
			)
			middleware.RespondError(c, http.StatusBadGateway, "AGENT_ERROR", result.Error, nil)
			return
		}
		entries := result.Entries
		if entries == nil {
			entries = []dirstore.DirEntry{}
		}
		c.JSON(http.StatusOK, listDirResponse{Path: req.Path, Entries: entries})
	case <-ctx.Done():
		h.logger.Warn("list-dir: timeout waiting for agent response",
			zap.String("agent_id", agentID),
			zap.String("request_id", requestID),
		)
		middleware.RespondError(c, http.StatusGatewayTimeout, "TIMEOUT", "agent did not respond in time", nil)
	}
}

// testRuleRequest is the body for POST /api/v1/agents/:id/test-rule.
type testRuleRequest struct {
	BasePath         string `json:"base_path"          binding:"required"`
	PathPattern      string `json:"path_pattern"       binding:"required"`
	DestPathTemplate string `json:"dest_path_template" binding:"required"`
	Recursive        bool   `json:"recursive"`
	DryRunLimit      int    `json:"dry_run_limit"`
}

// testRuleFileResult is a single file entry in the TestRule response.
type testRuleFileResult struct {
	LocalPath    string            `json:"local_path"`
	UploadPath   string            `json:"upload_path,omitempty"`
	ParsedFields map[string]string `json:"parsed_fields"`
	ComposeError string            `json:"compose_error,omitempty"`
}

// TestRule handles POST /api/v1/agents/:id/test-rule.
// It sends a dry-run PushRuleCommand to the online agent and waits up to 30 s
// for the agent to walk its filesystem and return matching file results.
// Returns 409 when the agent is offline, 504 on timeout, 422 on pattern error.
func (h *AgentsHandler) TestRule(c *gin.Context) {
	if h.registry == nil || h.dryRunStore == nil {
		middleware.NotImplemented(c)
		return
	}
	agentID := c.Param("id")
	if _, err := uuid.Parse(agentID); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agent id", nil)
		return
	}

	var req testRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}
	if req.DryRunLimit <= 0 {
		req.DryRunLimit = 10
	}
	if req.DryRunLimit > 50 {
		req.DryRunLimit = 50
	}

	// Check agent online status (memory registry first, then Redis fallback).
	inMemory := h.registry.IsOnline(agentID)
	if !inMemory {
		inRedis := false
		if h.cache != nil {
			if n, err := h.cache.Exists(c.Request.Context(), cache.AgentOnlineKey(agentID)); err == nil && n > 0 {
				inRedis = true
			}
		}
		if !inRedis {
			middleware.RespondError(c, http.StatusConflict, "AGENT_OFFLINE", "agent is not online", nil)
			return
		}
	}

	// Use a temporary rule_id so the gRPC round-trip can be correlated.
	ruleID := uuid.New().String()
	resultCh := h.dryRunStore.Register(ruleID)
	defer h.dryRunStore.Cancel(ruleID)

	msg := &agentv1.ServerMessage{
		Payload: &agentv1.ServerMessage_PushRule{
			PushRule: &agentv1.PushRuleCommand{
				Rule: &agentv1.CollectionRule{
					RuleId:           ruleID,
					BasePath:         req.BasePath,
					PathPattern:      req.PathPattern,
					DestPathTemplate: req.DestPathTemplate,
					Recursive:        req.Recursive,
					DryRun:           true,
				},
			},
		},
	}
	if !h.registry.Send(agentID, msg) {
		middleware.RespondError(c, http.StatusConflict, "AGENT_OFFLINE", "agent disconnected during send", nil)
		return
	}

	ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
	defer cancel()

	select {
	case result := <-resultCh:
		if result.GetError() != "" {
			middleware.RespondError(c, http.StatusUnprocessableEntity, "INVALID_PATTERN", result.GetError(), nil)
			return
		}
		files := make([]testRuleFileResult, 0, len(result.GetFiles()))
		for i, f := range result.GetFiles() {
			if i >= req.DryRunLimit {
				break
			}
			pf := f.GetParsedFields()
			if pf == nil {
				pf = map[string]string{}
			}
			files = append(files, testRuleFileResult{
				LocalPath:    f.GetLocalPath(),
				UploadPath:   f.GetUploadPath(),
				ParsedFields: pf,
				ComposeError: f.GetComposeError(),
			})
		}
		c.JSON(http.StatusOK, gin.H{"files": files})
	case <-ctx.Done():
		h.logger.Warn("test-rule: timeout waiting for agent response",
			zap.String("agent_id", agentID),
			zap.String("rule_id", ruleID),
		)
		middleware.RespondError(c, http.StatusGatewayTimeout, "TIMEOUT", "agent did not respond within 30s", nil)
	}
}

// collectionRuleResponse is the outbound JSON shape for a collection rule.
type collectionRuleResponse struct {
	ID               string `json:"id"`
	AgentID          string `json:"agent_id"`
	BucketID         string `json:"bucket_id"`
	Name             string `json:"name"`
	Mode             string `json:"mode"`
	Enabled          bool   `json:"enabled"`
	RunOnceOnStart   bool   `json:"run_once_on_start"`
	BasePath         string `json:"base_path"`
	PathPattern      string `json:"path_pattern"`
	DestPathTemplate string `json:"dest_path_template"`
	Recursive        bool   `json:"recursive"`
	AppendMode       string `json:"append_mode"`
	CronExpr         string `json:"cron_expr,omitempty"`
	CreatedAt        string `json:"created_at"`
}

func toRuleResponse(r *db.CollectionRule) collectionRuleResponse {
	resp := collectionRuleResponse{
		ID:               r.ID.String(),
		AgentID:          r.AgentID.String(),
		BucketID:         r.BucketID.String(),
		Name:             r.Name,
		Mode:             string(r.Mode),
		Enabled:          r.Status == db.RuleStatusActive,
		RunOnceOnStart:   r.RunOnceOnStart,
		BasePath:         r.BasePath,
		PathPattern:      r.PathPattern,
		DestPathTemplate: r.DestPathTemplate,
		Recursive:        r.Recursive,
		AppendMode:       r.AppendMode,
		CreatedAt:        r.CreatedAt.UTC().Format(time.RFC3339),
	}
	if r.CronExpr.Valid {
		resp.CronExpr = r.CronExpr.String
	}
	return resp
}

// ListRules handles GET /api/v1/agents/:id/rules.
func (h *AgentsHandler) ListRules(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	agentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agent id", nil)
		return
	}
	rules, err := h.db.ListCollectionRulesByAgent(c.Request.Context(), agentID)
	if err != nil {
		h.logger.Error("list rules", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list rules", nil)
		return
	}
	resp := make([]collectionRuleResponse, 0, len(rules))
	for _, r := range rules {
		resp = append(resp, toRuleResponse(r))
	}
	// collection_rules is a slow-growth table (see DECISIONS.md D-007): return full list.
	c.JSON(http.StatusOK, gin.H{"items": resp, "total": len(resp)})
}

// createRuleRequest is the body expected by POST /api/v1/agents/:id/rules.
// Field names match the response shape (collectionRuleResponse) so that the
// same JSON key set is used for both reads and writes.
type createRuleRequest struct {
	BucketID         string          `json:"bucket_id"          binding:"required"`
	Name             string          `json:"name"               binding:"required"`
	Mode             string          `json:"mode"               binding:"required"`
	BasePath         string          `json:"base_path"          binding:"required"`
	PathPattern      string          `json:"path_pattern"       binding:"required"`
	DestPathTemplate string          `json:"dest_path_template" binding:"required"`
	Recursive        bool            `json:"recursive"`
	CronExpr         string          `json:"cron_expr"`
	RunOnceOnStart   bool            `json:"run_once_on_start"`
	AppendMode       string          `json:"append_mode"`
	Enabled          *bool           `json:"enabled"`
	Metadata         json.RawMessage `json:"metadata"`
}

// CreateRule handles POST /api/v1/agents/:id/rules.
func (h *AgentsHandler) CreateRule(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	agentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agent id", nil)
		return
	}
	orgID := orgIDFromClaims(c)

	var req createRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	bucketID, err := uuid.Parse(req.BucketID)
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_BUCKET_ID", "invalid bucket_id", nil)
		return
	}

	appendMode := req.AppendMode
	if appendMode == "" {
		appendMode = "overwrite"
	}
	mode := strings.ToLower(req.Mode)
	status := db.RuleStatusActive
	if req.Enabled != nil && !*req.Enabled {
		status = db.RuleStatusInactive
	}
	metadata := req.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}

	params := db.CreateCollectionRuleParams{
		OrgID:            orgID,
		AgentID:          agentID,
		BucketID:         bucketID,
		Name:             req.Name,
		Mode:             db.UploadMode(mode),
		BasePath:         req.BasePath,
		PathPattern:      req.PathPattern,
		DestPathTemplate: req.DestPathTemplate,
		Recursive:        req.Recursive,
		Status:           status,
		RunOnceOnStart:   req.RunOnceOnStart,
		AppendMode:       appendMode,
		Metadata:         metadata,
	}
	if req.CronExpr != "" {
		params.CronExpr = sql.NullString{String: req.CronExpr, Valid: true}
	}

	rule, err := h.db.CreateCollectionRule(c.Request.Context(), params)
	if err != nil {
		h.logger.Error("create rule", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create rule", nil)
		return
	}

	// Best-effort dispatch; log error but don't fail the HTTP request.
	if h.dispatcher != nil {
		if err := h.dispatcher.DispatchRule(c.Request.Context(), rule); err != nil {
			h.logger.Warn("dispatch rule after create", zap.Error(err))
		}
	}
	c.JSON(http.StatusCreated, toRuleResponse(rule))
}

// updateRuleRequest is the body expected by PUT /api/v1/agents/:id/rules/:rid.
// The endpoint serves two shapes:
//   - status-only toggle: {"status":"active"|"inactive"} — enable/disable.
//   - full-field edit (CC-9): when Name is non-empty, every content field below
//     is applied. Status is derived from Enabled (defaulting to active).
//
// The two are distinguished by the presence of the Name key (a *string, so an
// explicit empty "name" still selects the full-update path and is rejected as a
// validation error rather than silently falling back to the status toggle),
// which the toggle never sends — keeping the enable/disable path backward
// compatible.
type updateRuleRequest struct {
	Status string `json:"status"`

	Name             *string         `json:"name"`
	BucketID         string          `json:"bucket_id"`
	Mode             string          `json:"mode"`
	BasePath         string          `json:"base_path"`
	PathPattern      string          `json:"path_pattern"`
	DestPathTemplate string          `json:"dest_path_template"`
	Recursive        bool            `json:"recursive"`
	CronExpr         string          `json:"cron_expr"`
	RunOnceOnStart   bool            `json:"run_once_on_start"`
	AppendMode       string          `json:"append_mode"`
	Enabled          *bool           `json:"enabled"`
	Metadata         json.RawMessage `json:"metadata"`
}

// UpdateRule handles PUT /api/v1/agents/:id/rules/:rid. It either changes only
// the rule status or applies a full-field edit (see updateRuleRequest). Either
// way, an active result is re-dispatched (the Agent hot-reloads the watcher) and
// an inactive result is cancelled.
func (h *AgentsHandler) UpdateRule(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	rid, err := uuid.Parse(c.Param("rid"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid rule id", nil)
		return
	}

	var req updateRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	if req.Name != nil {
		h.updateRuleFull(c, rid, req)
		return
	}
	h.updateRuleStatus(c, rid, req.Status)
}

// updateRuleStatus applies an enable/disable toggle.
func (h *AgentsHandler) updateRuleStatus(c *gin.Context, rid uuid.UUID, statusStr string) {
	status := db.RuleStatus(statusStr)
	switch status {
	case db.RuleStatusActive, db.RuleStatusInactive:
	default:
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_STATUS", "status must be 'active' or 'inactive'", nil)
		return
	}

	rule, err := h.db.UpdateCollectionRuleStatus(c.Request.Context(), rid, status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "rule not found", nil)
			return
		}
		h.logger.Error("update rule status", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update rule", nil)
		return
	}

	h.redispatchRule(c.Request.Context(), rule, status)
	c.JSON(http.StatusOK, toRuleResponse(rule))
}

// updateRuleFull applies a full-field edit of a collection rule. The update is
// scoped to the path agent and the caller's org so a guessed rule UUID cannot
// modify another agent's or org's rule (a mismatch yields NOT_FOUND).
func (h *AgentsHandler) updateRuleFull(c *gin.Context, rid uuid.UUID, req updateRuleRequest) {
	agentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agent id", nil)
		return
	}
	orgID := orgIDFromClaims(c)

	name := ""
	if req.Name != nil {
		name = *req.Name
	}
	// All content fields are required for a full update. Use a fixed order so the
	// reported field is deterministic (a map would randomise it).
	required := []struct{ field, value string }{
		{"name", name},
		{"bucket_id", req.BucketID},
		{"mode", req.Mode},
		{"base_path", req.BasePath},
		{"path_pattern", req.PathPattern},
		{"dest_path_template", req.DestPathTemplate},
	}
	for _, r := range required {
		if r.value == "" {
			middleware.RespondError(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
				r.field+" is required for a full rule update", nil)
			return
		}
	}

	bucketID, err := uuid.Parse(req.BucketID)
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_BUCKET_ID", "invalid bucket_id", nil)
		return
	}

	mode := strings.ToLower(req.Mode)
	switch db.UploadMode(mode) {
	case db.UploadModeWatch, db.UploadModeScheduled:
	default:
		middleware.RespondError(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
			"mode must be 'watch' or 'scheduled'", nil)
		return
	}

	status := db.RuleStatusActive
	if req.Enabled != nil && !*req.Enabled {
		status = db.RuleStatusInactive
	}
	appendMode := req.AppendMode
	if appendMode == "" {
		appendMode = "overwrite"
	}
	metadata := req.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}

	params := db.UpdateCollectionRuleParams{
		ID:               rid,
		AgentID:          agentID,
		OrgID:            orgID,
		BucketID:         bucketID,
		Name:             name,
		Mode:             db.UploadMode(mode),
		BasePath:         req.BasePath,
		PathPattern:      req.PathPattern,
		DestPathTemplate: req.DestPathTemplate,
		Recursive:        req.Recursive,
		Status:           status,
		RunOnceOnStart:   req.RunOnceOnStart,
		AppendMode:       appendMode,
		Metadata:         metadata,
	}
	if req.CronExpr != "" {
		params.CronExpr = sql.NullString{String: req.CronExpr, Valid: true}
	}

	rule, err := h.db.UpdateCollectionRule(c.Request.Context(), params)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "rule not found", nil)
			return
		}
		h.logger.Error("update rule", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update rule", nil)
		return
	}

	// Re-dispatch so an active rule's content change hot-reloads on the Agent
	// (offline agents pick it up via SyncRulesOnConnect on reconnect).
	h.redispatchRule(c.Request.Context(), rule, status)
	c.JSON(http.StatusOK, toRuleResponse(rule))
}

// redispatchRule pushes the rule to its Agent when active, or cancels it when
// inactive. Dispatch failures are logged, not surfaced, since the DB write has
// already succeeded and the Agent reconciles on reconnect.
func (h *AgentsHandler) redispatchRule(ctx context.Context, rule *db.CollectionRule, status db.RuleStatus) {
	if h.dispatcher == nil {
		return
	}
	switch status {
	case db.RuleStatusActive:
		if err := h.dispatcher.DispatchRule(ctx, rule); err != nil {
			h.logger.Warn("re-dispatch rule", zap.Error(err))
		}
	case db.RuleStatusInactive:
		if err := h.dispatcher.DispatchRuleCancel(ctx, rule.ID.String(), rule.AgentID.String()); err != nil {
			h.logger.Warn("cancel rule dispatch", zap.Error(err))
		}
	}
}

// DeleteRule handles DELETE /api/v1/agents/:id/rules/:rid.
func (h *AgentsHandler) DeleteRule(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	agentID := c.Param("id")
	rid, err := uuid.Parse(c.Param("rid"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid rule id", nil)
		return
	}

	if err := h.db.DeleteCollectionRule(c.Request.Context(), rid); err != nil {
		h.logger.Error("delete rule", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete rule", nil)
		return
	}

	// Best-effort cancel dispatch.
	if h.dispatcher != nil {
		if err := h.dispatcher.DispatchRuleCancel(c.Request.Context(), rid.String(), agentID); err != nil {
			h.logger.Warn("cancel rule after delete", zap.Error(err))
		}
	}
	c.Status(http.StatusNoContent)
}

// ListUploadLogs handles GET /api/v1/agents/:id/upload-logs.
func (h *AgentsHandler) ListUploadLogs(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	agentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agent id", nil)
		return
	}

	orgID := orgIDFromClaims(c)
	limit := parseLimitParam(c)
	cursorCreatedAt, cursorID, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_CURSOR", "invalid cursor", nil)
		return
	}

	agentNullUUID := uuid.NullUUID{UUID: agentID, Valid: true}
	params := db.ListUploadLogsParams{
		OrgID:           orgID,
		AgentID:         agentNullUUID,
		CursorCreatedAt: cursorCreatedAt,
		CursorID:        cursorID,
		Limit:           limit + 1, // fetch one extra to detect has_more
	}
	logs, err := h.db.ListUploadLogs(c.Request.Context(), params)
	if err != nil {
		h.logger.Error("list upload logs for agent", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list upload logs", nil)
		return
	}

	hasMore := len(logs) > int(limit)
	if hasMore {
		logs = logs[:limit]
	}

	total, err := h.db.CountUploadLogs(c.Request.Context(), db.CountUploadLogsFilter{
		OrgID:   orgID,
		AgentID: agentNullUUID,
	})
	if err != nil {
		h.logger.Error("count upload logs for agent", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to count upload logs", nil)
		return
	}

	resp := make([]uploadLogResponse, 0, len(logs))
	for _, l := range logs {
		resp = append(resp, toUploadLogResponse(l))
	}
	var nextCursor string
	if hasMore {
		last := logs[len(logs)-1]
		nextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	c.JSON(http.StatusOK, gin.H{
		"items":       resp,
		"total":       total,
		"has_more":    hasMore,
		"next_cursor": nextCursor,
	})
}
