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
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to list buckets", nil),
		})
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_REQUEST", err.Error(), nil),
		})
		return
	}

	// Validate bucket name before writing to DB (D-001).
	if err := validateBucketName(req.Name); err != nil {
		c.JSON(http.StatusUnprocessableEntity, gin.H{
			"error": middleware.NewErrorBody("INVALID_BUCKET_NAME", err.Error(), nil),
		})
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to create bucket", nil),
		})
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
			c.JSON(http.StatusBadGateway, gin.H{
				"error": middleware.NewErrorBody("MINIO_ERROR", "failed to create MinIO bucket: "+mkErr.Error(), nil),
			})
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to list event rules", nil),
		})
		return
	}
	resp := make([]eventRuleResponse, 0, len(rules))
	for _, r := range rules {
		resp = append(resp, toEventRuleResponse(r))
	}
	c.JSON(http.StatusOK, gin.H{"items": resp, "total": len(resp)})
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_REQUEST", err.Error(), nil),
		})
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to create event rule", nil),
		})
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid event rule id", nil),
		})
		return
	}

	var req updateEventRuleRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_REQUEST", err.Error(), nil),
		})
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
			c.JSON(http.StatusNotFound, gin.H{
				"error": middleware.NewErrorBody("NOT_FOUND", "event rule not found", nil),
			})
			return
		}
		h.logger.Error("update event rule", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to update event rule", nil),
		})
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid event rule id", nil),
		})
		return
	}
	if err := h.db.DeleteEventRule(c.Request.Context(), id); err != nil {
		h.logger.Error("delete event rule", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to delete event rule", nil),
		})
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
	CreatedAt    string `json:"created_at"`
}

func toEventDeliveryResponse(d *db.EventDelivery) eventDeliveryResponse {
	return eventDeliveryResponse{
		ID:           d.ID.String(),
		EventRuleID:  d.EventRuleID.String(),
		Status:       d.Status,
		AttemptCount: d.AttemptCount,
		CreatedAt:    d.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// ListDeliveries handles GET /api/v1/event-rules/:id/deliveries.
func (h *EventRulesHandler) ListDeliveries(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid event rule id", nil),
		})
		return
	}

	limit := parseLimitParam(c)
	cursorCreatedAt, cursorID, err := decodeCursor(c.Query("cursor"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_CURSOR", "invalid cursor", nil),
		})
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
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to list deliveries", nil),
		})
		return
	}

	hasMore := len(deliveries) > int(limit)
	if hasMore {
		deliveries = deliveries[:limit]
	}

	total, err := h.db.CountDeliveriesByRule(c.Request.Context(), id)
	if err != nil {
		h.logger.Error("count deliveries", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to count deliveries", nil),
		})
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
	UploadedAt   string `json:"uploaded_at"`
}

func toUploadLogResponse(l *db.UploadLog) uploadLogResponse {
	r := uploadLogResponse{
		ID:          l.ID.String(),
		AgentID:     l.AgentID.String(),
		StoragePath: l.StoragePath,
		Status:      strings.ToUpper(l.Status),
		Size:        l.SizeBytes,
		UploadedAt:  l.CreatedAt.UTC().Format(time.RFC3339),
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
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_CURSOR", "invalid cursor", nil),
		})
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

	logs, err := h.db.ListUploadLogs(c.Request.Context(), params)
	if err != nil {
		h.logger.Error("list upload logs", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to list upload logs", nil),
		})
		return
	}

	hasMore := len(logs) > int(limit)
	if hasMore {
		logs = logs[:limit]
	}

	total, err := h.db.CountUploadLogs(c.Request.Context(), filter)
	if err != nil {
		h.logger.Error("count upload logs", zap.Error(err))
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

// Get handles GET /api/v1/upload-logs/:id.
func (h *UploadLogsHandler) Get(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_ID", "invalid upload log id", nil),
		})
		return
	}
	log, err := h.db.GetUploadLogByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusNotFound, gin.H{
				"error": middleware.NewErrorBody("NOT_FOUND", "upload log not found", nil),
			})
			return
		}
		h.logger.Error("get upload log", zap.Error(err))
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "failed to get upload log", nil),
		})
		return
	}
	c.JSON(http.StatusOK, toUploadLogResponse(log))
}

// ── MinioEventHandler ────────────────────────────────────────────────────────

// IndexerClient is the minimal interface needed by MinioEventHandler to reflect
// MinIO object events in the file index.
type IndexerClient interface {
	// IndexUpload records an ObjectCreated event in the file index.
	// It is called in a best-effort, non-blocking fashion; errors are only
	// logged (warn level) and do not affect the HTTP response.
	IndexUpload(ctx context.Context, bucketName, objectKey string, sizeBytes int64, etag string) error
	// IndexDeletion records an ObjectRemoved event: it soft-deletes the matching
	// file entry and publishes events.file.deleted. Best-effort like IndexUpload.
	IndexDeletion(ctx context.Context, bucketName, objectKey string) error
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
	return &MinioEventHandler{logger: logger, indexer: indexer, secret: secret}
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
	EventName string  `json:"eventName"`
	S3        minioS3 `json:"s3"`
}

type minioS3 struct {
	Bucket minioS3Bucket `json:"bucket"`
	Object minioS3Object `json:"object"`
}

type minioS3Bucket struct {
	Name string `json:"name"`
}

type minioS3Object struct {
	Key  string `json:"key"`
	Size int64  `json:"size"`
	ETag string `json:"eTag"`
}

// Handle handles POST /internal/minio-event.
func (h *MinioEventHandler) Handle(c *gin.Context) {
	if !h.authorized(c) {
		h.logger.Warn("minio event: rejected unauthorized webhook call",
			zap.String("client_ip", c.ClientIP()))
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": middleware.NewErrorBody("UNAUTHORIZED", "invalid or missing webhook credentials", nil),
		})
		return
	}

	var payload minioS3Event
	if err := c.ShouldBindJSON(&payload); err != nil {
		// MinIO may send different payload shapes; accept any JSON and log.
		h.logger.Warn("minio event: failed to parse payload", zap.Error(err))
		c.Status(http.StatusOK)
		return
	}

	for _, rec := range payload.Records {
		h.logger.Info("minio event received",
			zap.String("event", rec.EventName),
			zap.String("bucket", rec.S3.Bucket.Name),
			zap.String("key", rec.S3.Object.Key),
			zap.Int64("size", rec.S3.Object.Size),
		)
		if h.indexer == nil {
			continue
		}
		// Route by S3 event type: ObjectCreated:* indexes an upload, while
		// ObjectRemoved:* soft-deletes the entry and emits events.file.deleted.
		// Previously every event was indexed as an upload, so deletions were
		// mis-recorded and file_deleted event rules never fired.
		bucket, key := rec.S3.Bucket.Name, rec.S3.Object.Key
		switch {
		case strings.HasPrefix(rec.EventName, "s3:ObjectRemoved:"):
			if err := h.indexer.IndexDeletion(c.Request.Context(), bucket, key); err != nil {
				h.logger.Warn("minio event: index deletion failed",
					zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
			}
		case strings.HasPrefix(rec.EventName, "s3:ObjectCreated:"):
			if err := h.indexer.IndexUpload(c.Request.Context(), bucket, key, rec.S3.Object.Size, rec.S3.Object.ETag); err != nil {
				h.logger.Warn("minio event: index upload failed",
					zap.String("bucket", bucket), zap.String("key", key), zap.Error(err))
			}
		default:
			h.logger.Debug("minio event: ignoring unhandled event type",
				zap.String("event", rec.EventName))
		}
	}
	c.Status(http.StatusOK)
}
