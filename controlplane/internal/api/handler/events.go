package handler

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/event"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// ── BucketsHandler ────────────────────────────────��───────────────────────────

// BucketsDB is the minimal database interface needed by BucketsHandler.
type BucketsDB interface {
	ListBuckets(ctx context.Context, orgID uuid.UUID) ([]*db.Bucket, error)
	CreateBucket(ctx context.Context, arg db.CreateBucketParams) (*db.Bucket, error)
	DeleteBucket(ctx context.Context, id uuid.UUID) error
}

// MinioBucketMaker creates physical buckets in MinIO.
type MinioBucketMaker interface {
	MakeBucket(ctx context.Context, bucketName string) error
}

// BucketsHandler groups the Bucket management handlers.
type BucketsHandler struct {
	db     BucketsDB
	minio  MinioBucketMaker
	logger *zap.Logger
}

// NewBucketsHandler returns a new BucketsHandler.
// minio may be nil; when nil, bucket creation will succeed in DB only.
func NewBucketsHandler(bucketsDB BucketsDB, minio MinioBucketMaker, logger *zap.Logger) *BucketsHandler {
	return &BucketsHandler{db: bucketsDB, minio: minio, logger: logger}
}

// bucketResponse is the outbound JSON shape for a bucket.
type bucketResponse struct {
	ID          string `json:"id"`
	OrgID       string `json:"org_id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	CreatedAt   string `json:"created_at"`
}

func toBucketResponse(b *db.Bucket) bucketResponse {
	r := bucketResponse{
		ID:        b.ID.String(),
		OrgID:     b.OrgID.String(),
		Name:      b.Name,
		CreatedAt: b.CreatedAt.UTC().Format(time.RFC3339),
	}
	if b.Description.Valid {
		r.Description = b.Description.String
	}
	return r
}

// List handles GET /api/v1/buckets.
func (h *BucketsHandler) List(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)
	buckets, err := h.db.ListBuckets(c.Request.Context(), orgID)
	if err != nil {
		h.logger.Error("list buckets", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list buckets", nil)
		return
	}
	resp := make([]bucketResponse, 0, len(buckets))
	for _, b := range buckets {
		resp = append(resp, toBucketResponse(b))
	}
	c.JSON(http.StatusOK, gin.H{"items": resp, "total": len(resp)})
}

// createBucketRequest is the body expected by POST /api/v1/buckets.
type createBucketRequest struct {
	Name        string `json:"name"        binding:"required"`
	Description string `json:"description"`
}

// bucketNameRegexp matches valid S3-compatible bucket names:
// 3-63 chars, only lowercase letters/digits/hyphens, not starting or ending with a hyphen.
var bucketNameRegexp = regexp.MustCompile(`^[a-z0-9][a-z0-9\-]{1,61}[a-z0-9]$`)

// validateBucketName returns an error when name does not satisfy S3 naming rules.
func validateBucketName(name string) error {
	if len(name) < 3 || len(name) > 63 {
		return fmt.Errorf("bucket name must be 3-63 characters")
	}
	if !bucketNameRegexp.MatchString(name) {
		return fmt.Errorf("bucket name must contain only lowercase letters, numbers, and hyphens, and cannot start or end with a hyphen")
	}
	return nil
}

// Create handles POST /api/v1/buckets.
func (h *BucketsHandler) Create(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)

	var req createBucketRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	// Validate bucket name before writing to DB (D-001).
	if err := validateBucketName(req.Name); err != nil {
		middleware.RespondError(c, http.StatusUnprocessableEntity, "INVALID_BUCKET_NAME", err.Error(), nil)
		return
	}

	params := db.CreateBucketParams{
		OrgID: orgID,
		Name:  req.Name,
	}
	if req.Description != "" {
		params.Description = sql.NullString{String: req.Description, Valid: true}
	}

	bucket, err := h.db.CreateBucket(c.Request.Context(), params)
	if err != nil {
		h.logger.Error("create bucket", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create bucket", nil)
		return
	}

	// Create the physical bucket in MinIO. On failure, roll back the DB record
	// and return a 502 so the client knows the bucket was not actually created.
	if h.minio != nil {
		if mkErr := h.minio.MakeBucket(c.Request.Context(), bucket.Name); mkErr != nil {
			h.logger.Error("create minio bucket",
				zap.String("bucket", bucket.Name),
				zap.Error(mkErr),
			)
			if delErr := h.db.DeleteBucket(context.Background(), bucket.ID); delErr != nil {
				h.logger.Error("rollback: delete bucket record failed",
					zap.String("bucket_id", bucket.ID.String()),
					zap.Error(delErr),
				)
			}
			middleware.RespondError(c, http.StatusBadGateway, "MINIO_ERROR", "failed to create MinIO bucket: "+mkErr.Error(), nil)
			return
		}
	}

	c.JSON(http.StatusCreated, toBucketResponse(bucket))
}

// ── EventRulesHandler ─────────────────────────────��───────────────────────────

// EventRulesDB is the minimal database interface needed by EventRulesHandler.
type EventRulesDB interface {
	ListEventRules(ctx context.Context, orgID uuid.UUID) ([]*db.EventRule, error)
	GetEventRuleByID(ctx context.Context, id uuid.UUID) (*db.EventRule, error)
	CreateEventRule(ctx context.Context, arg db.CreateEventRuleParams) (*db.EventRule, error)
	UpdateEventRule(ctx context.Context, arg db.UpdateEventRuleParams) (*db.EventRule, error)
	DeleteEventRule(ctx context.Context, id uuid.UUID) error
	ListDeliveriesByRule(ctx context.Context, arg db.ListDeliveriesByRuleParams) ([]*db.EventDelivery, error)
	CountDeliveriesByRule(ctx context.Context, eventRuleID uuid.UUID) (int64, error)
}

// EventRulesHandler groups the event-rule management handlers.
type EventRulesHandler struct {
	db     EventRulesDB
	logger *zap.Logger
}

// NewEventRulesHandler returns a new EventRulesHandler.
func NewEventRulesHandler(eventsDB EventRulesDB, logger *zap.Logger) *EventRulesHandler {
	return &EventRulesHandler{db: eventsDB, logger: logger}
}

// eventRuleResponse is the outbound JSON shape for an event rule.
type eventRuleResponse struct {
	ID           string          `json:"id"`
	OrgID        string          `json:"org_id"`
	Name         string          `json:"name"`
	EventType    string          `json:"event_type"`
	Filter       json.RawMessage `json:"filter"`
	ActionType   string          `json:"action_type"`
	ActionConfig json.RawMessage `json:"action_config"`
	Enabled      bool            `json:"enabled"`
	CreatedAt    string          `json:"created_at"`
}

func toEventRuleResponse(r *db.EventRule) eventRuleResponse {
	return eventRuleResponse{
		ID:           r.ID.String(),
		OrgID:        r.OrgID.String(),
		Name:         r.Name,
		EventType:    string(r.EventType),
		Filter:       r.Filter,
		ActionType:   string(r.ActionType),
		ActionConfig: r.ActionConfig,
		Enabled:      r.Enabled,
		CreatedAt:    r.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// List handles GET /api/v1/event-rules.
func (h *EventRulesHandler) List(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)
	rules, err := h.db.ListEventRules(c.Request.Context(), orgID)
	if err != nil {
		h.logger.Error("list event rules", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list event rules", nil)
		return
	}
	resp := make([]eventRuleResponse, 0, len(rules))
	for _, r := range rules {
		resp = append(resp, toEventRuleResponse(r))
	}
	c.JSON(http.StatusOK, gin.H{"items": resp, "total": len(resp)})
}

// validateEventAction checks that action_type is supported end-to-end and that
// its action_config carries the field the action needs. kafka_publish exists in
// the DB enum but has no implementation (no Kafka in the deployment's fixed
// infra), so it is rejected here rather than silently accepted (CC-7). Returns
// an error code + message when invalid, or ok=true when the action is valid.
func validateEventAction(actionType string, actionConfig json.RawMessage) (code, message string, ok bool) {
	switch actionType {
	case string(db.ActionTypeWebhook):
		var cfg event.WebhookActionConfig
		if err := json.Unmarshal(actionConfig, &cfg); err != nil {
			return "INVALID_ACTION_CONFIG", "invalid webhook action_config: " + err.Error(), false
		}
		if cfg.URL == "" {
			return "INVALID_ACTION_CONFIG", "webhook action_config requires a non-empty \"url\"", false
		}
	case string(db.ActionTypeNatsPublish):
		var cfg event.NATSActionConfig
		if err := json.Unmarshal(actionConfig, &cfg); err != nil {
			return "INVALID_ACTION_CONFIG", "invalid nats_publish action_config: " + err.Error(), false
		}
		if cfg.Subject == "" {
			return "INVALID_ACTION_CONFIG", "nats_publish action_config requires a non-empty \"subject\"", false
		}
	default:
		return "INVALID_ACTION_TYPE",
			fmt.Sprintf("unsupported action_type %q; supported: webhook, nats_publish", actionType), false
	}
	return "", "", true
}

// createEventRuleRequest is the body expected by POST /api/v1/event-rules.
type createEventRuleRequest struct {
	Name         string          `json:"name"          binding:"required"`
	EventType    string          `json:"event_type"    binding:"required"`
	Filter       json.RawMessage `json:"filter"`
	ActionType   string          `json:"action_type"   binding:"required"`
	ActionConfig json.RawMessage `json:"action_config" binding:"required"`
	Enabled      bool            `json:"enabled"`
}

// Create handles POST /api/v1/event-rules.
func (h *EventRulesHandler) Create(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)
	callerID := callerIDFromClaims(c)

	var req createEventRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	if code, msg, ok := validateEventAction(req.ActionType, req.ActionConfig); !ok {
		middleware.RespondError(c, http.StatusBadRequest, code, msg, nil)
		return
	}

	filter := req.Filter
	if len(filter) == 0 {
		filter = json.RawMessage(`{}`)
	}

	var createdBy uuid.NullUUID
	if callerID != uuid.Nil {
		createdBy = uuid.NullUUID{UUID: callerID, Valid: true}
	}

	params := db.CreateEventRuleParams{
		OrgID:        orgID,
		Name:         req.Name,
		EventType:    db.EventType(req.EventType),
		Filter:       filter,
		ActionType:   db.ActionType(req.ActionType),
		ActionConfig: req.ActionConfig,
		Enabled:      req.Enabled,
		CreatedBy:    createdBy,
	}

	rule, err := h.db.CreateEventRule(c.Request.Context(), params)
	if err != nil {
		h.logger.Error("create event rule", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create event rule", nil)
		return
	}
	c.JSON(http.StatusCreated, toEventRuleResponse(rule))
}

// updateEventRuleRequest is the body expected by PUT /api/v1/event-rules/:id.
type updateEventRuleRequest struct {
	Name         string          `json:"name"          binding:"required"`
	EventType    string          `json:"event_type"    binding:"required"`
	Filter       json.RawMessage `json:"filter"`
	ActionType   string          `json:"action_type"   binding:"required"`
	ActionConfig json.RawMessage `json:"action_config" binding:"required"`
	Enabled      bool            `json:"enabled"`
}

// Update handles PUT /api/v1/event-rules/:id.
func (h *EventRulesHandler) Update(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid event rule id", nil)
		return
	}

	var req updateEventRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	if code, msg, ok := validateEventAction(req.ActionType, req.ActionConfig); !ok {
		middleware.RespondError(c, http.StatusBadRequest, code, msg, nil)
		return
	}

	filter := req.Filter
	if len(filter) == 0 {
		filter = json.RawMessage(`{}`)
	}

	params := db.UpdateEventRuleParams{
		ID:           id,
		Name:         req.Name,
		EventType:    db.EventType(req.EventType),
		Filter:       filter,
		ActionType:   db.ActionType(req.ActionType),
		ActionConfig: req.ActionConfig,
		Enabled:      req.Enabled,
	}

	rule, err := h.db.UpdateEventRule(c.Request.Context(), params)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "event rule not found", nil)
			return
		}
		h.logger.Error("update event rule", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update event rule", nil)
		return
	}
	c.JSON(http.StatusOK, toEventRuleResponse(rule))
}

// Delete handles DELETE /api/v1/event-rules/:id.
func (h *EventRulesHandler) Delete(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid event rule id", nil)
		return
	}
	if err := h.db.DeleteEventRule(c.Request.Context(), id); err != nil {
		h.logger.Error("delete event rule", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete event rule", nil)
		return
	}
	c.Status(http.StatusNoContent)
}

// eventDeliveryResponse is the outbound JSON shape for an event delivery.
type eventDeliveryResponse struct {
	ID           string `json:"id"`
	EventRuleID  string `json:"event_rule_id"`
	Status       string `json:"status"`
	AttemptCount int32  `json:"attempt_count"`
	// Response fields are populated for HTTP-style deliveries (webhook) and let
	// the UI expand a failed/dead row to show why it failed. Nullable in the DB;
	// omitted when absent (e.g. nats_publish has no HTTP code/body). Additive to
	// the contract — see DECISIONS.md D-026.
	ResponseCode int32  `json:"response_code,omitempty"`
	ResponseBody string `json:"response_body,omitempty"`
	NextRetryAt  string `json:"next_retry_at,omitempty"`
	DeliveredAt  string `json:"delivered_at,omitempty"`
	CreatedAt    string `json:"created_at"`
}

func toEventDeliveryResponse(d *db.EventDelivery) eventDeliveryResponse {
	r := eventDeliveryResponse{
		ID:           d.ID.String(),
		EventRuleID:  d.EventRuleID.String(),
		Status:       d.Status,
		AttemptCount: d.AttemptCount,
		CreatedAt:    d.CreatedAt.UTC().Format(time.RFC3339),
	}
	if d.ResponseCode.Valid {
		r.ResponseCode = d.ResponseCode.Int32
	}
	if d.ResponseBody.Valid {
		r.ResponseBody = d.ResponseBody.String
	}
	if d.NextRetryAt.Valid {
		r.NextRetryAt = d.NextRetryAt.Time.UTC().Format(time.RFC3339)
	}
	if d.DeliveredAt.Valid {
		r.DeliveredAt = d.DeliveredAt.Time.UTC().Format(time.RFC3339)
	}
	return r
}

// ListDeliveries handles GET /api/v1/event-rules/:id/deliveries.
func (h *EventRulesHandler) ListDeliveries(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid event rule id", nil)
		return
	}

	limit := parseLimitParam(c)
	cursorCreatedAt, cursorID, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_CURSOR", "invalid cursor", nil)
		return
	}

	params := db.ListDeliveriesByRuleParams{
		EventRuleID:     id,
		CursorCreatedAt: cursorCreatedAt,
		CursorID:        cursorID,
		Limit:           limit + 1, // fetch one extra to detect has_more
	}
	deliveries, err := h.db.ListDeliveriesByRule(c.Request.Context(), params)
	if err != nil {
		h.logger.Error("list deliveries", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list deliveries", nil)
		return
	}

	hasMore := len(deliveries) > int(limit)
	if hasMore {
		deliveries = deliveries[:limit]
	}

	total, err := h.db.CountDeliveriesByRule(c.Request.Context(), id)
	if err != nil {
		h.logger.Error("count deliveries", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to count deliveries", nil)
		return
	}

	resp := make([]eventDeliveryResponse, 0, len(deliveries))
	for _, d := range deliveries {
		resp = append(resp, toEventDeliveryResponse(d))
	}
	var nextCursor string
	if hasMore {
		last := deliveries[len(deliveries)-1]
		nextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	c.JSON(http.StatusOK, gin.H{
		"items":       resp,
		"total":       total,
		"has_more":    hasMore,
		"next_cursor": nextCursor,
	})
}

// ── UploadLogsHandler ────────────────────────────���────────────────────────────

// UploadLogsDB is the minimal database interface needed by UploadLogsHandler.
type UploadLogsDB interface {
	ListUploadLogs(ctx context.Context, arg db.ListUploadLogsParams) ([]*db.UploadLog, error)
	CountUploadLogs(ctx context.Context, f db.CountUploadLogsFilter) (int64, error)
	GetUploadLogByID(ctx context.Context, id uuid.UUID) (*db.UploadLog, error)
}

// UploadLogsHandler groups the upload-log query handlers.
type UploadLogsHandler struct {
	db     UploadLogsDB
	logger *zap.Logger
}

// NewUploadLogsHandler returns a new UploadLogsHandler.
func NewUploadLogsHandler(logsDB UploadLogsDB, logger *zap.Logger) *UploadLogsHandler {
	return &UploadLogsHandler{db: logsDB, logger: logger}
}

// uploadLogResponse is the outbound JSON shape for an upload log entry.
type uploadLogResponse struct {
	ID           string `json:"id"`
	AgentID      string `json:"agent_id"`
	FileID       string `json:"file_id,omitempty"`
	Filename     string `json:"filename"`
	StoragePath  string `json:"storage_path"`
	Status       string `json:"status"`
	Size         int64  `json:"size"`
	ErrorMessage string `json:"error_message,omitempty"`
	// Retry trail fields let a failed row expand to show why/when it failed
	// (retry count, transferred bytes, timing). All already in the UploadLog
	// model; additive to the contract — see DECISIONS.md D-027.
	RetryCount       int32 `json:"retry_count"`
	BytesTransferred int64 `json:"bytes_transferred"`
	// started_at is NOT NULL and always set → always present. finished_at is
	// nullable (null until the upload reaches a terminal state) → omitempty.
	StartedAt  string `json:"started_at"`
	FinishedAt string `json:"finished_at,omitempty"`
	UploadedAt string `json:"uploaded_at"`
}

func toUploadLogResponse(l *db.UploadLog) uploadLogResponse {
	r := uploadLogResponse{
		ID:               l.ID.String(),
		AgentID:          l.AgentID.String(),
		StoragePath:      l.StoragePath,
		Status:           strings.ToUpper(l.Status),
		Size:             l.SizeBytes,
		RetryCount:       l.RetryCount,
		BytesTransferred: l.BytesTransferred,
		StartedAt:        l.StartedAt.UTC().Format(time.RFC3339),
		UploadedAt:       l.CreatedAt.UTC().Format(time.RFC3339),
	}
	if l.StoragePath != "" {
		r.Filename = path.Base(l.StoragePath)
	}
	if l.FileEntryID.Valid {
		r.FileID = l.FileEntryID.UUID.String()
	}
	if l.ErrorMessage.Valid {
		r.ErrorMessage = l.ErrorMessage.String
	}
	if l.FinishedAt.Valid {
		r.FinishedAt = l.FinishedAt.Time.UTC().Format(time.RFC3339)
	}
	return r
}

// List handles GET /api/v1/upload-logs.
func (h *UploadLogsHandler) List(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	limit := parseLimitParam(c)
	cursorCreatedAt, cursorID, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_CURSOR", "invalid cursor", nil)
		return
	}

	orgID := orgIDFromClaims(c)
	filter := db.CountUploadLogsFilter{OrgID: orgID}
	params := db.ListUploadLogsParams{
		OrgID:           orgID,
		CursorCreatedAt: cursorCreatedAt,
		CursorID:        cursorID,
		Limit:           limit + 1, // fetch one extra to detect has_more
	}
	if v := c.Query("agent_id"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			nid := uuid.NullUUID{UUID: id, Valid: true}
			params.AgentID = nid
			filter.AgentID = nid
		}
	}
	// Status filter: clients send the display (upper-case) form; the column is
	// stored lower-case, so normalize before matching. Empty = no filter.
	if v := c.Query("status"); v != "" {
		ns := sql.NullString{String: strings.ToLower(v), Valid: true}
		params.Status = ns
		filter.Status = ns
	}

	logs, err := h.db.ListUploadLogs(c.Request.Context(), params)
	if err != nil {
		h.logger.Error("list upload logs", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list upload logs", nil)
		return
	}

	hasMore := len(logs) > int(limit)
	if hasMore {
		logs = logs[:limit]
	}

	total, err := h.db.CountUploadLogs(c.Request.Context(), filter)
	if err != nil {
		h.logger.Error("count upload logs", zap.Error(err))
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

// Get handles GET /api/v1/upload-logs/:id.
func (h *UploadLogsHandler) Get(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid upload log id", nil)
		return
	}
	log, err := h.db.GetUploadLogByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "upload log not found", nil)
			return
		}
		h.logger.Error("get upload log", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to get upload log", nil)
		return
	}
	c.JSON(http.StatusOK, toUploadLogResponse(log))
}

// ── MinioEventHandler ────────────────────────────────────────────────────────

// IndexerClient is the minimal interface needed by MinioEventHandler to reflect
// MinIO object events in the file index.
type IndexerClient interface {
	// IndexUpload records an ObjectCreated event in the file index.
	// A returned error is a PROCESSING failure: the handler counts it under
	// the event's identity and answers 5xx (under the cap) so MinIO redelivers,
	// or dead-letters the event past the cap — see webhook_policy.go. This is
	// the core IC-4a semantic; errors are NOT "logged only".
	IndexUpload(ctx context.Context, bucketName, objectKey string, sizeBytes int64, etag string, observedAt time.Time, eventSeq string) error
	// IndexDeletion records an ObjectRemoved event: it soft-deletes the matching
	// file entry and publishes events.file.deleted. Errors follow the same
	// counted failure path as IndexUpload.
	IndexDeletion(ctx context.Context, bucketName, objectKey string, observedAt time.Time, eventSeq string) error
}

// MinioEventHandler handles POST /internal/minio-event — the MinIO S3 event
// webhook endpoint. This serves as an alternate indexing path for file events
// that arrive directly from MinIO rather than through an agent.
//
// The endpoint writes to the file index from an external source, so it is
// authenticated with a shared secret (MinIO's notify_webhook auth_token, see
// system-design.md §6.1.2 / §6.5). The secret is compared in constant time.
type MinioEventHandler struct {
	logger  *zap.Logger
	indexer IndexerClient // optional; nil disables indexing
	secret  string        // shared webhook secret; empty disables the endpoint
	// fails is the persistent failure-counter backend (nil disables counting
	// and the poison pill; used only by tests that exercise parsing routes).
	fails WebhookFailStore
	// dead is where exhausted events are persisted (nil skips persistence;
	// production wiring always provides both).
	dead DeadLetterSink
	// failLimit is the poison-pill retry cap (WEBHOOK_FAIL_LIMIT). After this
	// many failed deliveries one event is dead-lettered and answered 200.
	failLimit int64
	// parseCap is the request-body parse cap (WEBHOOK_MAX_PARSE_BYTES,
	// B-2-3): bodies above it are oversized (dead letter + 5xx). Injected so
	// tests can exercise the boundary; production value comes from Config.
	parseCap int64
}

// NewMinioEventHandler returns a new MinioEventHandler.
//
// indexer may be nil, in which case upload events are only logged. secret is the
// shared webhook token; when empty the endpoint fails closed (rejects every
// request), because an endpoint that mutates the index from external input must
// be authenticated and without a secret it cannot be.
func NewMinioEventHandler(indexer IndexerClient, secret string, logger *zap.Logger) *MinioEventHandler {
	if secret == "" {
		logger.Warn("minio event webhook: INTERNAL_WEBHOOK_SECRET is not set; " +
			"the /internal/minio-event endpoint will reject all requests until it is configured")
	}
	return &MinioEventHandler{logger: logger, indexer: indexer, secret: secret, parseCap: DefaultWebhookParseCap()}
}

// NewMinioEventHandlerWithPolicy returns a MinioEventHandler wired with the
// IC-4a failure machinery: the persistent failure-counter store, the
// dead-letter sink and the poison-pill retry cap. Production wiring uses this
// constructor; NewMinioEventHandler (counters nil) remains for tests that
// exercise only parsing/auth routing.
func NewMinioEventHandlerWithPolicy(indexer IndexerClient, secret string, fails WebhookFailStore, dead DeadLetterSink, failLimit int64, logger *zap.Logger) *MinioEventHandler {
	return NewMinioEventHandlerWithPolicyParseCap(indexer, secret, fails, dead, failLimit, DefaultWebhookParseCap(), logger)
}

// DefaultWebhookParseCap returns the default parse cap (used by the
// convenience constructor).
func DefaultWebhookParseCap() int64 { return maxWebhookParseBytes }

// NewMinioEventHandlerWithPolicyParseCap is the full constructor (round-3
// B-2-3): the parse cap is injected so WEBHOOK_MAX_PARSE_BYTES is a real,
// testable knob.
func NewMinioEventHandlerWithPolicyParseCap(indexer IndexerClient, secret string, fails WebhookFailStore, dead DeadLetterSink, failLimit int64, parseCap int64, logger *zap.Logger) *MinioEventHandler {
	if failLimit < MinWebhookFailLimit {
		// S1: never silently replace the configured value. Below the floor the
		// cap is dangerously short (limit=1 tolerates ~3s), so this logs a
		// loud warning and honors the value anyway; the startup gate lives in
		// config.Validate (rejects below-floor WEBHOOK_FAIL_LIMIT), and tests
		// deliberately pass small limits to exercise the state machine.
		logger.Warn("minio event webhook: WEBHOOK_FAIL_LIMIT below the safety floor — a few-second blip can dead-letter an event; production configs below the floor are rejected at startup",
			zap.Int64("configured", failLimit),
			zap.Int64("floor", MinWebhookFailLimit),
		)
	}
	logger.Info("minio event webhook: failure policy configured",
		zap.Int64("fail_limit", failLimit),
		zap.Bool("counter_store", fails != nil),
		zap.Bool("dead_letter_sink", dead != nil),
	)
	if parseCap < MinWebhookParseCap {
		logger.Warn("minio event webhook: parse cap below the safety floor — near-zero caps turn every normal notification into a permanent 5xx; production configs below the floor are rejected at startup",
			zap.Int64("configured", parseCap),
			zap.Int64("floor", MinWebhookParseCap),
		)
	}
	logger.Info("minio event webhook: failure policy configured",
		zap.Int64("fail_limit", failLimit),
		zap.Int64("parse_cap", parseCap),
		zap.Bool("counter_store", fails != nil),
		zap.Bool("dead_letter_sink", dead != nil),
	)
	h := NewMinioEventHandler(indexer, secret, logger)
	h.fails = fails
	h.dead = dead
	h.failLimit = failLimit
	h.parseCap = parseCap
	return h
}

// authorized reports whether the request carries the correct shared secret.
// MinIO sends the configured auth_token in the Authorization header; depending
// on the MinIO version it may or may not be prefixed with a "Bearer" scheme, so
// both forms are accepted (the scheme is matched case-insensitively per RFC 7235
// and tolerant of arbitrary whitespace). The presented and expected secrets are
// SHA-256 hashed before a constant-time compare, so the comparison time is
// independent of the secret's length and content (a plain ConstantTimeCompare
// returns early on a length mismatch, leaking the expected length). When no
// secret is configured the endpoint fails closed.
func (h *MinioEventHandler) authorized(c *gin.Context) bool {
	if h.secret == "" {
		return false
	}
	presented := strings.TrimSpace(c.GetHeader("Authorization"))
	if fields := strings.Fields(presented); len(fields) == 2 && strings.EqualFold(fields[0], "bearer") {
		presented = fields[1]
	}
	want := sha256.Sum256([]byte(h.secret))
	got := sha256.Sum256([]byte(presented))
	return subtle.ConstantTimeCompare(want[:], got[:]) == 1
}

// minioS3Event is the top-level MinIO S3 event notification payload.
type minioS3Event struct {
	EventName string             `json:"EventName"`
	Key       string             `json:"Key"`
	Records   []minioEventRecord `json:"Records"`
}

type minioEventRecord struct {
	EventTime time.Time `json:"eventTime"`
	EventName string    `json:"eventName"`
	S3        minioS3   `json:"s3"`
}

type minioS3 struct {
	Bucket minioS3Bucket `json:"bucket"`
	Object minioS3Object `json:"object"`
}

type minioS3Bucket struct {
	Name string `json:"name"`
}

type minioS3Object struct {
	Sequencer string `json:"sequencer"`
	Key       string `json:"key"`
	Size      int64  `json:"size"`
	ETag      string `json:"eTag"`
}

// decodeObjectKey decodes the object key carried by an S3 event notification.
//
// MinIO URL-encodes s3.object.key when it builds the notification payload, so
// "a/b/中 文.csv" arrives as "a%2Fb%2F%E4%B8%AD+%E6%96%87.csv". The encoding is
// the *query* form (a space becomes "+", not "%20"), which is why this uses
// url.QueryUnescape rather than url.PathUnescape: PathUnescape leaves "+"
// untouched, so every key containing a space would keep a literal plus sign. A
// key that genuinely contains a plus arrives as "%2B", so the round trip is
// unambiguous.
//
// Indexing the raw encoded key breaks every consumer that treats storage_path as
// a real object key: presigned downloads 404, fileNameFromPath yields the whole
// encoded key as the file name, Classifier.Classify's filepath.Match/Base see no
// separator, IndexDeletion's exact-match soft delete never fires, and the
// agent-reported path (which is not encoded) lands under a different unique key,
// producing two rows for one object.
//
// This applies to keys delivered to a *configured notification target* — MinIO
// escapes those (ToEvent(escape=true)). The ListenBucketNotification streaming
// API is fed by the same event with escape=false, so a consumer built on that
// API must NOT call this function or it will rewrite literal "+" to a space.
//
// On failure the raw key is returned along with the error, so the caller can
// still index something instead of dropping the event; callers must log it. The
// failure path is effectively unreachable from MinIO (QueryEscape output is
// always valid escaping) and is symmetric — a create and a later delete of the
// same undecodable key fall back to the same string, so the soft delete still
// matches and no undeletable ghost row is created. Once IC-4 adds dead-letter
// handling, a decode failure should go there rather than into the index.
func decodeObjectKey(raw string) (string, error) {
	decoded, err := url.QueryUnescape(raw)
	if err != nil {
		return raw, err
	}
	return decoded, nil
}

// Handle handles POST /internal/minio-event.
//
// Response contract (IC-4a ① / IC-BUG-6) — one path for every failure kind:
//
//	a record indexes cleanly      → 200 (event consumed)
//	a record fails, under the cap → 5xx (MinIO retries; the counter persists)
//	a record fails, over the cap  → dead-letter row + 200 (queue head freed)
//
// Parse failures and decode failures flow through the SAME machinery (no
// 4xx/5xx split: MinIO retries 400 like 500, dev-measured). A payload that
// cannot be parsed at all is dead-lettered immediately — it can never succeed,
// so counting retries would only block the feed for the cap's duration before
// reaching the same verdict.
func (h *MinioEventHandler) Handle(c *gin.Context) {
	if !h.authorized(c) {
		h.logger.Warn("minio event: rejected unauthorized webhook call",
			zap.String("client_ip", c.ClientIP()))
		middleware.RespondError(c, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or missing webhook credentials", nil)
		return
	}

	var payload minioS3Event
	// Read the body with an explicit oversized check (B-NEW-2 → B-2, PR #110
	// round-3 review): reading cap+1 distinguishes oversized from malformed —
	// a silently truncated prefix is never mistaken for a complete payload.
	// The body is consumed to the end (tee-hashed by readBodyCapped) so the
	// dedup hash covers the FULL body, not just the prefix kept for capture.
	raw, oversized, fullHash, readErr := readBodyCapped(c.Request.Body, h.parseCap)
	if readErr != nil {
		h.logger.Error("minio event: failed to read request body; requesting redelivery",
			zap.Error(readErr))
		c.Status(http.StatusInternalServerError)
		return
	}
	if oversized {
		// Explicit terminal state for oversized payloads: NOT a silent 200.
		// 5xx keeps the event in MinIO's queue; the operator raises
		// WEBHOOK_MAX_PARSE_BYTES (the knob is wired through Config, with a
		// startup floor — see config.Validate) or handles the event manually.
		// The dead letter records the event with a 64KiB-captured prefix and
		// the FULL-body hash as its dedup key, so distinct oversized payloads
		// never overwrite each other's row.
		h.logger.Error("minio event: payload exceeds the parse cap; dead-lettering with truncated capture and answering 5xx — raise WEBHOOK_MAX_PARSE_BYTES if this is legitimate",
			zap.Int64("parse_cap", h.parseCap),
			zap.Int("captured_prefix_len", len(raw)))
		dl := DeadLetter{
			DedupKey:   "oversized:" + fullHash,
			EventName:  "oversized",
			FailCount:  1,
			LastError:  "payload exceeds WEBHOOK_MAX_PARSE_BYTES",
			RawPayload: captureRawPayload(raw),
			Truncated:  true,
		}
		if err := h.deadLetter(c.Request.Context(), dl); err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusInternalServerError)
		return
	}
	if err := json.Unmarshal(raw, &payload); err != nil {
		// The payload is not parseable, so no event identity can be recovered
		// from it — dead-letter it under its payload hash and free the queue.
		// Counting retries would be pointless: the failure is deterministic.
		// BUT the dead letter must be durably stored before the queue is freed
		// (B2): if the sink (same PostgreSQL as the indexer) cannot persist,
		// answer 5xx so MinIO redelivers and the next round retries the write.
		h.logger.Error("minio event: failed to parse payload; dead-lettering",
			zap.Error(err))
		dl := DeadLetter{
			DedupKey:   "unparseable:" + fullHash,
			EventName:  "unknown",
			FailCount:  1,
			LastError:  err.Error(),
			RawPayload: captureRawPayload(raw),
			Truncated:  len(raw) > maxDeadLetterPayloadBytes,
		}
		if err := h.deadLetter(c.Request.Context(), dl); err != nil {
			c.Status(http.StatusInternalServerError)
			return
		}
		c.Status(http.StatusOK)
		return
	}

	failed := false
	for _, rec := range payload.Records {
		bucket := rec.S3.Bucket.Name
		// The key is URL-encoded by MinIO; decode it before it reaches the index
		// or anything else, since storage_path must hold the real object key.
		key, err := decodeObjectKey(rec.S3.Object.Key)
		if err != nil {
			// IC-2c used to fall back to indexing the raw key here. Indexing it
			// would write a storage_path no real object matches. From MinIO this
			// path is unreachable (QueryEscape output is always valid escaping);
			// a decode failure means the payload was not produced by MinIO.
			// B3 (PR #110 review): decode failures flow through the SAME counted
			// state machine as indexing failures — under the cap 5xx (retry in
			// case of one-off transport corruption), past the cap a dead letter
			// with the raw key (not a decoded one) and 200. The counter identity
			// uses the RAW key + sequencer, which are stable across redeliveries.
			h.logger.Error("minio event: object key is not valid URL encoding",
				zap.String("bucket", bucket),
				zap.String("raw_key", rec.S3.Object.Key),
				zap.Error(err))
			if h.handleIndexFailure(c.Request.Context(), bucket, rec.S3.Object.Key, rec, err) {
				failed = true
			}
			continue
		}
		h.logger.Info("minio event received",
			zap.String("event", rec.EventName),
			zap.String("bucket", bucket),
			zap.String("key", key),
			zap.Int64("size", rec.S3.Object.Size),
		)
		if h.indexer == nil {
			continue
		}
		// Route by S3 event type: ObjectCreated:* indexes an upload, while
		// ObjectRemoved:* soft-deletes the entry and emits events.file.deleted.
		// Previously every event was indexed as an upload, so deletions were
		// mis-recorded and file_deleted event rules never fired.
		var indexErr error
		removed := strings.HasPrefix(rec.EventName, "s3:ObjectRemoved:")
		switch {
		case removed:
			indexErr = h.indexer.IndexDeletion(c.Request.Context(), bucket, key, rec.EventTime, rec.S3.Object.Sequencer)
		case strings.HasPrefix(rec.EventName, "s3:ObjectCreated:"):
			indexErr = h.indexer.IndexUpload(c.Request.Context(), bucket, key, rec.S3.Object.Size, rec.S3.Object.ETag, rec.EventTime, rec.S3.Object.Sequencer)
		default:
			h.logger.Debug("minio event: ignoring unhandled event type",
				zap.String("event", rec.EventName))
		}
		if indexErr != nil {
			// The single failure path: count under the event's identity, then
			// either 5xx (retry) or dead-letter + 200 (free the queue).
			if h.handleIndexFailure(c.Request.Context(), bucket, key, rec, indexErr) {
				failed = true
			}
			continue
		}
		h.clearFailCount(c.Request.Context(), bucket, key, rec)
	}
	if failed {
		// Any record needing a retry fails the whole request with 5xx; MinIO
		// redelivers the payload and the successful records re-run their
		// (idempotent) upserts.
		c.Status(http.StatusInternalServerError)
		return
	}
	c.Status(http.StatusOK)
}

// handleIndexFailure implements the one failure path (used by BOTH indexing
// errors and decode errors — B3). It returns true when the request must be
// answered 5xx (retry), false when the event was durably dead-lettered (the
// caller answers 200 and the queue moves on).
func (h *MinioEventHandler) handleIndexFailure(ctx context.Context, bucket, key string, rec minioEventRecord, failErr error) bool {
	removed := strings.HasPrefix(rec.EventName, "s3:ObjectRemoved:")
	identity := DeadLetterIdentity(bucket, key, rec.S3.Object.Sequencer)
	h.logger.Warn("minio event: processing failed",
		zap.String("event", rec.EventName),
		zap.String("bucket", bucket),
		zap.String("key", key),
		zap.String("dedup_key", identity),
		zap.Error(failErr))
	if h.fails == nil {
		// No counter store wired (tests / degraded wiring): retry, matching the
		// pre-IC-4a mechanism but with the correct status code.
		return true
	}
	count, err := h.fails.IncrFailCount(ctx, identity)
	if err != nil {
		// The whole counter stack failed (Redis AND the in-process fallback).
		// Retry is still the only non-lossy default; this path is effectively
		// unreachable with the fallback store wired (B1).
		h.logger.Error("minio event: failure counter unavailable; requesting retry",
			zap.String("dedup_key", identity), zap.Error(err))
		return true
	}
	if !ShouldDeadLetter(count, h.failLimit) {
		h.logger.Warn("minio event: requesting redelivery",
			zap.String("dedup_key", identity),
			zap.Int64("fail_count", count),
			zap.Int64("fail_limit", h.failLimit),
		)
		return true
	}
	dl := DeadLetter{
		DedupKey:   identity,
		EventName:  rec.EventName,
		Bucket:     bucket,
		Key:        key,
		SizeBytes:  rec.S3.Object.Size,
		ETag:       rec.S3.Object.ETag,
		ObservedAt: rec.EventTime,
		EventSeq:   rec.S3.Object.Sequencer,
		FailCount:  count,
		LastError:  failErr.Error(),
		Removed:    removed,
	}
	// B2: the verdict becomes durable ONLY when the dead letter is stored.
	// The sink shares PostgreSQL with the indexer, so a PG outage long enough
	// to exhaust the cap also fails this write — answering 200 here would
	// delete the event from MinIO's queue with no record anywhere (silent
	// loss). On failure: keep the counter, answer 5xx; the next redelivery
	// round lands here again and retries the persistence directly.
	if err := h.deadLetter(ctx, dl); err != nil {
		h.logger.Error("minio event: dead-letter persistence FAILED; answering 5xx so the event is not lost",
			zap.String("dedup_key", identity), zap.Error(err))
		return true
	}
	h.clearFailCount(ctx, bucket, key, rec)
	return false
}

// deadLetter persists one exhausted event and returns an error when the event
// is NOT yet durably recorded (B2). The verdict log fires only on success —
// "will NOT be retried" must never precede a durable write. The caller owns
// the response: success → 200 (free the queue), failure → 5xx (MinIO
// redelivers; the next round retries persistence; the counter is kept).
func (h *MinioEventHandler) deadLetter(ctx context.Context, dl DeadLetter) error {
	if h.dead == nil {
		return nil
	}
	if err := h.dead.DeadLetter(ctx, dl); err != nil {
		h.logger.Error("minio event: FAILED to persist dead letter; keeping the event in MinIO's queue (5xx) — the next redelivery retries this write",
			zap.String("dedup_key", dl.DedupKey), zap.Error(err))
		return err
	}
	logDeadLetter(h.logger, dl)
	return nil
}

// clearFailCount drops the persistent counter after success. Errors are only
// logged: a leftover counter makes a future genuine failure dead-letter
// early, which is the conservative direction (and self-corrects once the
// event is dead-lettered).
func (h *MinioEventHandler) clearFailCount(ctx context.Context, bucket, key string, rec minioEventRecord) {
	if h.fails == nil {
		return
	}
	identity := DeadLetterIdentity(bucket, key, rec.S3.Object.Sequencer)
	if err := h.fails.ClearFailCount(ctx, identity); err != nil {
		h.logger.Warn("minio event: could not clear failure counter",
			zap.String("dedup_key", identity), zap.Error(err))
	}
}
