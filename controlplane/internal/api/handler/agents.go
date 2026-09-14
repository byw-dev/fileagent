package handler

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/agent"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/cache"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/dirstore"
	"github.com/byw-dev/fileagent/pkg/trollsift"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// MaxRulesPerAgent caps the collection rules a single agent may own
// (IC-2b review F3): the reconnect sync delivers the whole rule set as one
// snapshot message, and the count must stay far below the gRPC message limit.
const MaxRulesPerAgent = 1000

// revokeSendWait is the hard upper bound Revoke gives the send goroutine to
// confirm the RevokeCommand was written to the stream before the stream is cut
// regardless (IC-BUG-32). It must never grow: an agent that stops reading its
// stream pins stream.Send in flow control, and the only correct answer to that
// is cutting the stream when the bound expires.
const revokeSendWait = time.Second

// estimatedRuleBytes is the snapshot budget a request-built rule is assumed
// to consume: the serialized byte length of every free-form string field plus
// a generous fixed overhead (uuids, timestamps, booleans, map keys) so the
// creation-time estimate never undershoots the dispatcher's exact proto.Size
// pre-flight (IC-2b review R3).
func estimatedRuleBytes(req createRuleRequest) int {
	fixed := 512
	return fixed + len(req.Name) + len(req.BasePath) + len(req.PathPattern) +
		len(req.DestPathTemplate) + len(req.CronExpr) + len(req.Metadata)
}

// estimatedRuleBytesFromDB is estimatedRuleBytes for rules already persisted.
func estimatedRuleBytesFromDB(r *db.CollectionRule) int {
	fixed := 512
	cron := len(r.CronExpr.String)
	meta := len(r.Metadata)
	return fixed + len(r.Name) + len(r.BasePath) + len(r.PathPattern) +
		len(r.DestPathTemplate) + cron + meta
}

// AgentsDB is the minimal database interface needed by AgentsHandler.
type AgentsDB interface {
	ListAgents(ctx context.Context, orgID uuid.UUID) ([]*db.Agent, error)
	ListAgentsByStatus(ctx context.Context, orgID uuid.UUID, status db.AgentStatus) ([]*db.Agent, error)
	GetAgentByID(ctx context.Context, id uuid.UUID) (*db.Agent, error)
	UpdateAgentName(ctx context.Context, id uuid.UUID, name string) (*db.Agent, error)
	ListCollectionRulesByAgent(ctx context.Context, agentID uuid.UUID) ([]*db.CollectionRule, error)
	GetCollectionRuleByID(ctx context.Context, id uuid.UUID) (*db.CollectionRule, error)
	CreateCollectionRule(ctx context.Context, arg db.CreateCollectionRuleParams) (*db.CollectionRule, error)
	UpdateCollectionRuleStatus(ctx context.Context, iD uuid.UUID, status db.RuleStatus) (*db.CollectionRule, error)
	UpdateCollectionRule(ctx context.Context, arg db.UpdateCollectionRuleParams) (*db.CollectionRule, error)
	// DeleteCollectionRule removes the rule only when it belongs to the given
	// agent and org, and reports how many rows the delete actually removed.
	DeleteCollectionRule(ctx context.Context, id, agentID, orgID uuid.UUID) (int64, error)
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
	// Disconnect cuts the agent's stream. Revocation needs it because the
	// Revoke command it sends is cooperative and a compromised agent ignores it.
	Disconnect(agentID string) bool
	// SendSync enqueues a message and waits a bounded timeout for the send
	// goroutine to confirm it was written to the stream. Revoke uses it so the
	// cooperative command is not lost to the Disconnect that immediately
	// follows; the bound must stay hard because a non-reading agent pins
	// stream.Send forever.
	SendSync(agentID string, msg *agentv1.ServerMessage, timeout time.Duration) bool
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
	// Register records which agent requestID was issued to, so the gRPC side
	// can refuse a listing arriving from any other agent.
	Register(requestID, agentID string) <-chan dirstore.Result
	Cancel(requestID string)
}

// DryRunStore manages pending dry-run result channels.
type DryRunStore interface {
	// Register records which agent reqID was issued to, so the gRPC side can
	// refuse a result arriving from any other agent.
	Register(reqID, agentID string) <-chan *agentv1.DryRunResult
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
	// True when the agent's rule snapshot exceeded the size budget and the
	// last sync ran degraded: the agent keeps its previous rule view until
	// the rule set shrinks below the budget and the agent reconnects
	// (IC-2b review R3). Read from the cache; unaffected by is_online.
	RuleSyncDegraded bool `json:"rule_sync_degraded"`
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
	// Independent of is_online: the marker is (re)set by every sync and
	// cleared by the next successful one, so it reflects the last sync's
	// outcome even while the agent is between connections (IC-2b review R3).
	if n, err := h.cache.Exists(ctx, cache.AgentSyncDegradedKey(a.ID.String())); err == nil {
		r.RuleSyncDegraded = n > 0
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

// maxAgentNameLen caps an agent's custom display name.
const maxAgentNameLen = 64

// agentNameRe restricts the display name to letters (any script, incl. CJK),
// digits, spaces, and ._- . It excludes path separators and other special
// characters because the name is injected into upload path templates
// ({agent_name}); a "/" or control char there would corrupt object keys.
var agentNameRe = regexp.MustCompile(`^[\p{L}\p{N} ._-]+$`)

// renameAgentRequest is the body expected by PATCH /api/v1/agents/:id.
type renameAgentRequest struct {
	Name string `json:"name" binding:"required"`
}

// Rename handles PATCH /api/v1/agents/:id — sets an agent's custom display name
// (super_admin only). The new name takes effect in the agent's path templates
// on its next reconnect; previously uploaded objects keep their old-name paths.
func (h *AgentsHandler) Rename(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agent id", nil)
		return
	}

	var req renameAgentRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	name := strings.TrimSpace(req.Name)
	if name == "" {
		middleware.RespondError(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR", "name must not be empty", nil)
		return
	}
	if len([]rune(name)) > maxAgentNameLen {
		middleware.RespondError(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
			"name must be at most 64 characters", nil)
		return
	}
	if !agentNameRe.MatchString(name) {
		middleware.RespondError(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
			"name may contain only letters, digits, spaces, and . _ -", nil)
		return
	}

	agent, err := h.db.UpdateAgentName(c.Request.Context(), id, name)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "agent not found", nil)
			return
		}
		h.logger.Error("rename agent", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to rename agent", nil)
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
		// Ask the agent to clean up its local token, and give the send
		// goroutine a bounded moment to confirm the command was written to the
		// stream (IC-BUG-32): a command still queued behind an in-flight
		// stream.Send is lost to the Disconnect below. The bound is a hard
		// cap — an agent that never reads its stream is cut on timeout, never
		// waited on forever — and the wait is best-effort: the stream is cut
		// regardless of the outcome, because the command is cooperative and a
		// compromised agent ignores it anyway (IC-BUG-25).
		h.registry.SendSync(id.String(), &agentv1.ServerMessage{
			Payload: &agentv1.ServerMessage_Revoke{
				Revoke: &agentv1.RevokeCommand{Reason: "revoked_by_admin"},
			},
		}, revokeSendWait)
		// …then cut the stream regardless. The command above is cooperative and
		// a compromised agent will ignore it; without this it would keep
		// heartbeating (appearing online, unkickable) and keep reporting
		// uploads. See IC-BUG-25.
		if h.registry.Disconnect(id.String()) {
			h.logger.Info("revoke: agent stream cut", zap.String("agent_id", id.String()))
		}
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
	// the agent's response. The request is recorded as addressed to this agent,
	// so a listing arriving from any other agent is refused by the store.
	resultCh := h.dirStore.Register(requestID, agentID)
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
	resultCh := h.dryRunStore.Register(ruleID, agentID)
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
	// Metadata is the rule's declared metadata (file_type / static_tags /
	// path_tag_map, metadata 6c). Returned so the rule form can round-trip it on
	// edit — without it an update would overwrite the stored metadata with {}.
	Metadata  json.RawMessage `json:"metadata,omitempty"`
	CreatedAt string          `json:"created_at"`
	// Warnings carries non-fatal contract notices (IC-BUG-50 / D-034): a
	// dest_path_template using the deprecated {time} reserved word still
	// renders, but creation/update tells the admin to migrate to {submit_time}.
	Warnings []string `json:"warnings,omitempty"`
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
		Metadata:         r.Metadata,
		CreatedAt:        r.CreatedAt.UTC().Format(time.RFC3339),
	}
	if r.CronExpr.Valid {
		resp.CronExpr = r.CronExpr.String
	}
	return resp
}

// ruleTemplateWarnings returns the readable contract notices for BOTH rule
// fields that decide the object key (review C2): path_pattern and
// dest_path_template. This mirrors the agent's reservedTimeMisuse gate, which
// checks both — checking only dest_path_template here would let the admin
// create a rule the CP passes but the agent refuses at upload time.
//
// Deprecated {time}: a hint, not a rejection — existing rules created through
// the Web UI's old default template must not break. The wording is
// field-appropriate (review D2): path_pattern parses, dest_path_template only
// composes — one template for both fields got this wrong.
//
// Misuse (ValidateReservedTimeUse: bare or non-time-typed reserved word)
// matches the agent's hard refusal.
//
// Full syntax & timezone validity (review D1): the webui validator only
// checks kind/basic syntax and cannot be authoritative about IANA zones —
// measured cross-language gap ({tz=Nope/Bad}, trailing space, repeated |tz=
// all pass the UI but fail trollsift.New). The CP runs the same New() the
// agent runs (glob patterns are skipped: they are not trollsift) and reports
// the error in warnings at save time, so it surfaces here rather than at
// upload. Refusing the request outright would need a new decision — D-030 §8
// keeps templates shape-unconstrained.
func ruleTemplateWarnings(pathPattern, destPathTemplate string) []string {
	var warnings []string
	for _, f := range []struct{ field, value string }{
		{"path_pattern", pathPattern},
		{"dest_path_template", destPathTemplate},
	} {
		if trollsift.UsesDeprecatedTimeField(f.value) {
			warnings = append(warnings, deprecatedNotice(f.field))
		}
		if reason := trollsift.ValidateReservedTimeUse(f.value); reason != "" {
			warnings = append(warnings, f.field+": "+reason+
				" — the agent refuses to compose such uploads (task failure, no guessed key)")
		}
		if f.field == "path_pattern" && !trollsift.IsTrollsiftPattern(f.value) {
			continue
		}
		if _, err := trollsift.New(f.value); err != nil {
			warnings = append(warnings, f.field+" is not a valid trollsift pattern; the agent will not compose it: "+err.Error())
		}
	}
	return warnings
}

// deprecatedNotice wording per field (review D2): the parse-first exception
// lives on the path_pattern side, so the dest hint references path_pattern.
func deprecatedNotice(field string) string {
	if field == "path_pattern" {
		return "path_pattern uses the deprecated reserved word {time}; " +
			"it still parses as a time field (parse results always win), " +
			"but new rules should use {submit_time}, the declared name for the submit instant (see docs/design/contracts.md V-3)"
	}
	return "dest_path_template uses the deprecated reserved word {time}; " +
		"it still renders (the file's submit-for-upload instant, unless path_pattern parses a field with that name — parse results always win), " +
		"but new rules should use {submit_time}, the declared name for the submit instant (see docs/design/contracts.md V-3)"
}

// respondRule writes a rule response, attaching contract warnings for the
// deprecated {time} reserved word and reserved-word misuse in BOTH
// path_pattern and dest_path_template (review C2), plus a Warn log so the
// hint survives even for API clients that ignore the warnings field.
func (h *AgentsHandler) respondRule(c *gin.Context, code int, rule *db.CollectionRule) {
	resp := toRuleResponse(rule)
	if warns := ruleTemplateWarnings(rule.PathPattern, rule.DestPathTemplate); warns != nil {
		resp.Warnings = warns
		h.logger.Warn("rule template/pattern contract warnings (IC-BUG-50 / D-034); migrate to {submit_time}",
			zap.String("rule_id", rule.ID.String()),
			zap.String("path_pattern", rule.PathPattern),
			zap.String("dest_path_template", rule.DestPathTemplate),
			zap.Strings("warnings", warns))
	}
	c.JSON(code, resp)
}

// rejectTailAppendMode enforces the IC-BUG-46 fail-closed block on
// append_mode=tail. The refusal is NOT because "there are no consumers today"
// (that is a point-in-time snapshot, not a property of the mode): tail is a
// mode already offered by system-design.md §4.4.3, the rule schema and the
// proto — and with the current upload path an incremental tail upload
// REPLACES the whole object with just the appended bytes, silently destroying
// previously collected data. A mode that silently loses data must be stopped
// first; a visible failure beats a silently wrong result. The correct
// implementation (rolling chunks + server-side merge) is IC-15; until it
// lands, creation/update of tail rules is refused with a readable 422 shaped
// like the existing VALIDATION_ERROR responses. Returns true when the request
// was rejected (the caller must return immediately).
func rejectTailAppendMode(c *gin.Context, appendMode string) bool {
	if strings.EqualFold(strings.TrimSpace(appendMode), "tail") {
		middleware.RespondError(c, http.StatusUnprocessableEntity, "VALIDATION_ERROR",
			"append_mode=tail is disabled: an incremental tail upload currently replaces the whole object with only the appended bytes, silently losing previously collected data (IC-BUG-46); it stays refused until the correct implementation lands (IC-15: rolling chunks + server-side merge)", nil)
		return true
	}
	return false
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

	// Per-agent rule-count cap (IC-2b review F3): the reconnect sync delivers
	// the agent's whole rule set as ONE snapshot message, so an unbounded
	// count would eventually exceed the gRPC message limit and degrade that
	// agent into the oversized-snapshot keep-alive state. Capping at creation
	// makes that state structurally unreachable. 1000 rules is far above any
	// real deployment (~1 KB/rule worst case ≈ 1 MB, vs the 4 MiB gRPC limit
	// and the dispatcher's 3 MiB pre-flight check).
	rules, err := h.db.ListCollectionRulesByAgent(c.Request.Context(), agentID)
	if err != nil {
		h.logger.Error("create rule: count existing rules", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to check rule count", nil)
		return
	}
	if len(rules) >= MaxRulesPerAgent {
		h.logger.Warn("create rule: per-agent rule count limit reached",
			zap.String("agent_id", agentID.String()),
			zap.Int("limit", MaxRulesPerAgent))
		middleware.RespondError(c, http.StatusUnprocessableEntity,
			"RULE_COUNT_LIMIT",
			fmt.Sprintf("rule count limit: an agent may have at most %d collection rules (the reconnect sync delivers them as one snapshot message)", MaxRulesPerAgent),
			nil)
		return
	}

	// Byte-budget gate (IC-2b review R3): the count cap alone does not bound
	// the snapshot — a single rule with a 3 MiB TEXT field overshoots the
	// gRPC limit in ~200 such rules (measured). The quantity that actually
	// decides failure is serialized bytes, so creation estimates the
	// projected snapshot (existing rules + the new one) against the same
	// budget the dispatcher enforces (agent.MaxRulesSnapshotBytes). The
	// estimate is deliberately generous (fixed per-rule overhead); the
	// dispatcher's exact proto.Size pre-flight stays authoritative. Not
	// airtight by construction — concurrent creates and direct DB writes can
	// still race past it — that residual is the review's explicit card.
	projected := estimatedRuleBytes(req)
	for _, r := range rules {
		projected += estimatedRuleBytesFromDB(r)
	}
	if projected > agent.MaxRulesSnapshotBytes {
		h.logger.Warn("create rule: projected rule snapshot exceeds the size budget",
			zap.String("agent_id", agentID.String()),
			zap.Int("rules", len(rules)+1),
			zap.Int("projected_bytes", projected),
			zap.Int("limit", agent.MaxRulesSnapshotBytes))
		middleware.RespondError(c, http.StatusUnprocessableEntity,
			"RULE_SNAPSHOT_SIZE_LIMIT",
			fmt.Sprintf("rule snapshot size limit: this agent's rules would serialize to about %d bytes, above the %d-byte snapshot budget; reduce path/template sizes or move rules to other agents", projected, agent.MaxRulesSnapshotBytes),
			nil)
		return
	}

	appendMode := req.AppendMode
	if appendMode == "" {
		appendMode = "overwrite"
	}
	if rejectTailAppendMode(c, appendMode) {
		return
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
	h.respondRule(c, http.StatusCreated, rule)
}

// updateRuleRequest is the body expected by PUT /api/v1/agents/:id/rules/:rid.
// The endpoint serves two shapes:
//   - status-only toggle: {"status":"active"|"inactive"} — enable/disable.
//   - full-field edit (CC-9): when a "name" string is provided, every content
//     field below is applied. Status is derived from Enabled (defaulting to active).
//
// The two are distinguished by whether "name" is present as a JSON string: Name
// is a *string, so a provided string (including "") selects the full-update path
// and an empty value is rejected as a validation error rather than silently
// falling back to the status toggle. An absent "name" — or an explicit
// "name": null, which JSON-unmarshals to nil and is treated the same as absent —
// takes the status-only path. The enable/disable toggle never sends "name", so
// it stays backward compatible.
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
	// respondRule (not a bare c.JSON) so the status-only path — enabling a
	// rule whose template misuses a reserved word or uses the deprecated
	// {time} alias — also surfaces the contract warnings (review E1).
	h.respondRule(c, http.StatusOK, rule)
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
	if rejectTailAppendMode(c, appendMode) {
		return
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
	h.respondRule(c, http.StatusOK, rule)
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
	// The URL agent id stays a validated route parameter even though ownership
	// itself is decided by the DB: an unparsable id is a malformed request.
	if _, err := uuid.Parse(c.Param("id")); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid agent id", nil)
		return
	}
	rid, err := uuid.Parse(c.Param("rid"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid rule id", nil)
		return
	}

	// The rule row is read back before deleting, for two reasons (IC-BUG-26):
	// an unknown rule must 404 before anything happens, and the cancel command
	// below must go to the rule's real owner — both the URL agent id and the
	// rule id are client-supplied, so only the DB row knows who owns the rule.
	rule, err := h.db.GetCollectionRuleByID(c.Request.Context(), rid)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "rule not found", nil)
			return
		}
		h.logger.Error("get rule for delete", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete rule", nil)
		return
	}
	orgID := orgIDFromClaims(c)

	// The delete itself is scoped to the owner (id + agent_id + org_id); rows
	// == 0 means the rule is gone or not the caller's, and nothing — not even
	// a cancel — may be dispatched for a delete that did not happen.
	rows, err := h.db.DeleteCollectionRule(c.Request.Context(), rid, rule.AgentID, orgID)
	if err != nil {
		h.logger.Error("delete rule", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete rule", nil)
		return
	}
	if rows == 0 {
		middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "rule not found", nil)
		return
	}

	// Best-effort cancel dispatch, addressed to the owner read back from the
	// DB row — never to c.Param("id").
	if h.dispatcher != nil {
		if err := h.dispatcher.DispatchRuleCancel(c.Request.Context(), rid.String(), rule.AgentID.String()); err != nil {
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
