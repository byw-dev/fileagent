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
}

// AgentsHandler groups the Agent management handlers.
type AgentsHandler struct {
	db         AgentsDB
	agentMgr   AgentManager
	dispatcher RuleDispatcher
	registry   AgentRegistryClient
	cache      AgentCacheClient
	logger     *zap.Logger
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

// toAgentResponseWithOnline enriches an agentResponse with a real-time is_online
// value by querying the cache (Redis TTL key).
func (h *AgentsHandler) toAgentResponseWithOnline(ctx context.Context, a *db.Agent) agentResponse {
	r := toAgentResponse(a)
	if h.cache != nil {
		if n, err := h.cache.Exists(ctx, cache.AgentOnlineKey(a.ID.String())); err == nil {
			r.IsOnline = n > 0
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to list agents", nil),
		})
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid agent id", nil),
		})
		return
	}
	agent, err := h.db.GetAgentByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": middleware.NewErrorBody("NOT_FOUND", "agent not found", nil),
			})
			return
		}
		h.logger.Error("get agent", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to get agent", nil),
		})
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid agent id", nil),
		})
		return
	}
	callerID := callerIDFromClaims(c)
	token, err := h.agentMgr.ApproveAgent(c.Request.Context(), id, callerID)
	if err != nil {
		h.logger.Error("approve agent", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to approve agent", nil),
		})
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid agent id", nil),
		})
		return
	}
	callerID := callerIDFromClaims(c)
	if err := h.agentMgr.RevokeAgent(c.Request.Context(), id, callerID); err != nil {
		h.logger.Error("revoke agent", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to revoke agent", nil),
		})
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

// ListDir handles POST /api/v1/agents/:id/list-dir.
// Sends a ListDirectoryCommand to the online agent. Returns 202 when the
// command was delivered, 409 when the agent is offline.
func (h *AgentsHandler) ListDir(c *gin.Context) {
	if h.registry == nil {
		middleware.NotImplemented(c)
		return
	}
	agentID := c.Param("id")
	if _, err := uuid.Parse(agentID); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid agent id", nil),
		})
		return
	}

	var req listDirRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_REQUEST", err.Error(), nil),
		})
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
			c.JSON(http.StatusConflict, gin.H{
				"error": middleware.NewErrorBody("AGENT_OFFLINE", "agent is not online", nil),
			})
			return
		}
	}

	requestID := uuid.New().String()
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
		c.JSON(http.StatusConflict, gin.H{
			"error": middleware.NewErrorBody("AGENT_OFFLINE", "agent disconnected during send", nil),
		})
		return
	}
	c.JSON(http.StatusAccepted, gin.H{"request_id": requestID, "message": "list directory command sent"})
}

// collectionRuleResponse is the outbound JSON shape for a collection rule.
type collectionRuleResponse struct {
	ID                 string `json:"id"`
	AgentID            string `json:"agent_id"`
	DestBucketID       string `json:"dest_bucket_id"`
	Name               string `json:"name"`
	Mode               string `json:"mode"`
	IsActive           bool   `json:"is_active"`
	RunOnceOnStart     bool   `json:"run_once_on_start"`
	SourcePath         string `json:"source_path"`
	FilePattern        string `json:"file_pattern"`
	DestPathTemplate   string `json:"dest_path_template"`
	WatchRecursive     bool   `json:"watch_recursive"`
	CronExpr           string `json:"cron_expr,omitempty"`
	CreatedAt          string `json:"created_at"`
}

func toRuleResponse(r *db.CollectionRule) collectionRuleResponse {
	resp := collectionRuleResponse{
		ID:               r.ID.String(),
		AgentID:          r.AgentID.String(),
		DestBucketID:     r.BucketID.String(),
		Name:             r.Name,
		Mode:             string(r.Mode),
		IsActive:         r.Status == db.RuleStatusActive,
		RunOnceOnStart:   r.RunOnceOnStart,
		SourcePath:       r.SourcePathTemplate,
		FilePattern:      r.FileGlob,
		DestPathTemplate: r.UploadPathTemplate,
		WatchRecursive:   r.WatchRecursive,
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid agent id", nil),
		})
		return
	}
	rules, err := h.db.ListCollectionRulesByAgent(c.Request.Context(), agentID)
	if err != nil {
		h.logger.Error("list rules", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to list rules", nil),
		})
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
type createRuleRequest struct {
	BucketID           string          `json:"bucket_id"            binding:"required"`
	Name               string          `json:"name"                 binding:"required"`
	Mode               string          `json:"mode"                 binding:"required"`
	SourcePathTemplate string          `json:"source_path_template" binding:"required"`
	FileGlob           string          `json:"file_glob"            binding:"required"`
	UploadPathTemplate string          `json:"upload_path_template" binding:"required"`
	WatchRecursive     bool            `json:"watch_recursive"`
	WatchSubdirPattern string          `json:"watch_subdir_pattern"`
	CronExpr           string          `json:"cron_expr"`
	RunOnceOnStart     bool            `json:"run_once_on_start"`
	AppendMode         string          `json:"append_mode"`
	Metadata           json.RawMessage `json:"metadata"`
}

// CreateRule handles POST /api/v1/agents/:id/rules.
func (h *AgentsHandler) CreateRule(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	agentID, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid agent id", nil),
		})
		return
	}
	orgID := orgIDFromClaims(c)

	var req createRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_REQUEST", err.Error(), nil),
		})
		return
	}

	bucketID, err := uuid.Parse(req.BucketID)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_BUCKET_ID", "invalid bucket_id", nil),
		})
		return
	}

	appendMode := req.AppendMode
	if appendMode == "" {
		appendMode = "none"
	}
	metadata := req.Metadata
	if len(metadata) == 0 {
		metadata = json.RawMessage(`{}`)
	}

	params := db.CreateCollectionRuleParams{
		OrgID:              orgID,
		AgentID:            agentID,
		BucketID:           bucketID,
		Name:               req.Name,
		Mode:               db.UploadMode(req.Mode),
		SourcePathTemplate: req.SourcePathTemplate,
		FileGlob:           req.FileGlob,
		UploadPathTemplate: req.UploadPathTemplate,
		WatchRecursive:     req.WatchRecursive,
		RunOnceOnStart:     req.RunOnceOnStart,
		AppendMode:         appendMode,
		Metadata:           metadata,
	}
	if req.WatchSubdirPattern != "" {
		params.WatchSubdirPattern = sql.NullString{String: req.WatchSubdirPattern, Valid: true}
	}
	if req.CronExpr != "" {
		params.CronExpr = sql.NullString{String: req.CronExpr, Valid: true}
	}

	rule, err := h.db.CreateCollectionRule(c.Request.Context(), params)
	if err != nil {
		h.logger.Error("create rule", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to create rule", nil),
		})
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
type updateRuleRequest struct {
	Status string `json:"status" binding:"required"`
}

// UpdateRule handles PUT /api/v1/agents/:id/rules/:rid.
// Only the rule status (active/inactive) can be changed via REST.
// A rule becoming active triggers re-dispatch; inactive triggers cancel.
func (h *AgentsHandler) UpdateRule(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	rid, err := uuid.Parse(c.Param("rid"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid rule id", nil),
		})
		return
	}

	var req updateRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_REQUEST", err.Error(), nil),
		})
		return
	}

	status := db.RuleStatus(req.Status)
	switch status {
	case db.RuleStatusActive, db.RuleStatusInactive:
	default:
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_STATUS", "status must be 'active' or 'inactive'", nil),
		})
		return
	}

	rule, err := h.db.UpdateCollectionRuleStatus(c.Request.Context(), rid, status)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": middleware.NewErrorBody("NOT_FOUND", "rule not found", nil),
			})
			return
		}
		h.logger.Error("update rule status", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to update rule", nil),
		})
		return
	}

	if h.dispatcher != nil {
		switch status {
		case db.RuleStatusActive:
			if err := h.dispatcher.DispatchRule(c.Request.Context(), rule); err != nil {
				h.logger.Warn("re-dispatch rule on activate", zap.Error(err))
			}
		case db.RuleStatusInactive:
			if err := h.dispatcher.DispatchRuleCancel(c.Request.Context(), rid.String(), rule.AgentID.String()); err != nil {
				h.logger.Warn("cancel rule on deactivate", zap.Error(err))
			}
		}
	}
	c.JSON(http.StatusOK, toRuleResponse(rule))
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid rule id", nil),
		})
		return
	}

	if err := h.db.DeleteCollectionRule(c.Request.Context(), rid); err != nil {
		h.logger.Error("delete rule", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to delete rule", nil),
		})
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid agent id", nil),
		})
		return
	}

	orgID := orgIDFromClaims(c)
	limit := parseLimitParam(c)
	cursorCreatedAt, cursorID, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_CURSOR", "invalid cursor", nil),
		})
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to list upload logs", nil),
		})
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to count upload logs", nil),
		})
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
