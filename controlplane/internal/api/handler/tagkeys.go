package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/lib/pq"
	"go.uber.org/zap"
)

// TagKeysDB is the minimal database interface needed by TagKeysHandler. It is
// satisfied by *db.Queries.
type TagKeysDB interface {
	ListTagKeys(ctx context.Context, orgID uuid.UUID) ([]*db.TagKey, error)
	GetTagKey(ctx context.Context, orgID uuid.UUID, key string) (*db.TagKey, error)
	CreateTagKey(ctx context.Context, arg db.CreateTagKeyParams) (*db.TagKey, error)
	UpdateTagKey(ctx context.Context, arg db.UpdateTagKeyParams) (*db.TagKey, error)
	DeleteTagKey(ctx context.Context, orgID uuid.UUID, key string) (int64, error)
	ListTagValues(ctx context.Context, tagKeyID uuid.UUID) ([]*db.TagValue, error)
	CreateTagValue(ctx context.Context, tagKeyID uuid.UUID, value string) (*db.TagValue, error)
	DeleteTagValue(ctx context.Context, id uuid.UUID, tagKeyID uuid.UUID) (int64, error)
}

// TagKeysHandler groups the tag-key vocabulary and tag-value management handlers
// (metadata model 6c, Phase 1). Reads are open to any authenticated user; writes
// are gated to super_admin at the router.
type TagKeysHandler struct {
	db     TagKeysDB
	logger *zap.Logger
}

// NewTagKeysHandler returns a new TagKeysHandler. A nil db causes handlers to
// return 501 until dependencies are wired.
func NewTagKeysHandler(tagKeysDB TagKeysDB, logger *zap.Logger) *TagKeysHandler {
	return &TagKeysHandler{db: tagKeysDB, logger: logger}
}

// tagKeyResponse is the outbound JSON shape for a tag key.
type tagKeyResponse struct {
	ID                   string `json:"id"`
	Key                  string `json:"key"`
	Label                string `json:"label"`
	ValueControlled      bool   `json:"value_controlled"`
	RequiredAtCollection bool   `json:"required_at_collection"`
	AllowPathVar         bool   `json:"allow_path_var"`
	SystemReserved       bool   `json:"system_reserved"`
	CreatedAt            string `json:"created_at"`
}

func toTagKeyResponse(k *db.TagKey) tagKeyResponse {
	return tagKeyResponse{
		ID:                   k.ID.String(),
		Key:                  k.Key,
		Label:                k.Label,
		ValueControlled:      k.ValueControlled,
		RequiredAtCollection: k.RequiredAtCollection,
		AllowPathVar:         k.AllowPathVar,
		SystemReserved:       k.SystemReserved,
		CreatedAt:            k.CreatedAt.UTC().Format(time.RFC3339),
	}
}

// tagValueResponse is the outbound JSON shape for a tag value.
type tagValueResponse struct {
	ID    string `json:"id"`
	Value string `json:"value"`
}

func toTagValueResponse(v *db.TagValue) tagValueResponse {
	return tagValueResponse{ID: v.ID.String(), Value: v.Value}
}

// isUniqueViolation reports whether err is a Postgres unique-constraint error.
func isUniqueViolation(err error) bool {
	var pqErr *pq.Error
	return errors.As(err, &pqErr) && pqErr.Code == "23505"
}

// List handles GET /api/v1/tag-keys.
func (h *TagKeysHandler) List(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)
	keys, err := h.db.ListTagKeys(c.Request.Context(), orgID)
	if err != nil {
		h.logger.Error("list tag keys", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list tag keys", nil)
		return
	}
	resp := make([]tagKeyResponse, 0, len(keys))
	for _, k := range keys {
		resp = append(resp, toTagKeyResponse(k))
	}
	c.JSON(http.StatusOK, gin.H{"items": resp, "total": len(resp)})
}

// createTagKeyRequest is the body for POST /api/v1/tag-keys.
type createTagKeyRequest struct {
	Key                  string `json:"key" binding:"required"`
	Label                string `json:"label" binding:"required"`
	ValueControlled      *bool  `json:"value_controlled"`
	RequiredAtCollection *bool  `json:"required_at_collection"`
	AllowPathVar         *bool  `json:"allow_path_var"`
}

// boolOr returns *p when set, else def — so an omitted flag takes its schema
// default rather than Go's false zero value.
func boolOr(p *bool, def bool) bool {
	if p == nil {
		return def
	}
	return *p
}

// Create handles POST /api/v1/tag-keys.
func (h *TagKeysHandler) Create(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	var req createTagKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}
	key := strings.TrimSpace(req.Key)
	if !isValidTagKey(key) {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "key must be 1-64 chars of [a-z0-9_]", nil)
		return
	}
	label := strings.TrimSpace(req.Label)
	if label == "" {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "label must not be empty", nil)
		return
	}
	k, err := h.db.CreateTagKey(c.Request.Context(), db.CreateTagKeyParams{
		OrgID:                orgIDFromClaims(c),
		Key:                  key,
		Label:                label,
		ValueControlled:      boolOr(req.ValueControlled, true),
		RequiredAtCollection: boolOr(req.RequiredAtCollection, false),
		AllowPathVar:         boolOr(req.AllowPathVar, true),
	})
	if err != nil {
		if isUniqueViolation(err) {
			middleware.RespondError(c, http.StatusConflict, "ALREADY_EXISTS", "tag key already exists", nil)
			return
		}
		h.logger.Error("create tag key", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create tag key", nil)
		return
	}
	c.JSON(http.StatusCreated, toTagKeyResponse(k))
}

// updateTagKeyRequest is the body for PATCH /api/v1/tag-keys/:key. Fields left
// nil keep their current value.
type updateTagKeyRequest struct {
	Label                *string `json:"label"`
	ValueControlled      *bool   `json:"value_controlled"`
	RequiredAtCollection *bool   `json:"required_at_collection"`
	AllowPathVar         *bool   `json:"allow_path_var"`
}

// Update handles PATCH /api/v1/tag-keys/:key. The key itself is immutable (it is
// the identity referenced by file_tags); only its label/flags change.
func (h *TagKeysHandler) Update(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)
	key, ok := h.validKeyParam(c)
	if !ok {
		return
	}

	existing, err := h.db.GetTagKey(c.Request.Context(), orgID, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "tag key not found", nil)
			return
		}
		h.logger.Error("get tag key", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load tag key", nil)
		return
	}

	var req updateTagKeyRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	label := existing.Label
	if req.Label != nil {
		trimmed := strings.TrimSpace(*req.Label)
		if trimmed == "" {
			middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "label must not be empty", nil)
			return
		}
		label = trimmed
	}

	k, err := h.db.UpdateTagKey(c.Request.Context(), db.UpdateTagKeyParams{
		OrgID:                orgID,
		Key:                  key,
		Label:                label,
		ValueControlled:      boolOr(req.ValueControlled, existing.ValueControlled),
		RequiredAtCollection: boolOr(req.RequiredAtCollection, existing.RequiredAtCollection),
		AllowPathVar:         boolOr(req.AllowPathVar, existing.AllowPathVar),
	})
	if err != nil {
		// The row may have been deleted between the GetTagKey check and here.
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "tag key not found", nil)
			return
		}
		h.logger.Error("update tag key", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update tag key", nil)
		return
	}
	c.JSON(http.StatusOK, toTagKeyResponse(k))
}

// Delete handles DELETE /api/v1/tag-keys/:key. System-reserved keys cannot be
// deleted. Deleting a key cascades to its tag_values and file_tags rows (per
// schema), so it is a super_admin-only, deliberate action.
func (h *TagKeysHandler) Delete(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)
	key, ok := h.validKeyParam(c)
	if !ok {
		return
	}

	existing, err := h.db.GetTagKey(c.Request.Context(), orgID, key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "tag key not found", nil)
			return
		}
		h.logger.Error("get tag key for delete", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load tag key", nil)
		return
	}
	if existing.SystemReserved {
		middleware.RespondError(c, http.StatusConflict, "RESERVED", "system-reserved tag key cannot be deleted", nil)
		return
	}

	rows, err := h.db.DeleteTagKey(c.Request.Context(), orgID, key)
	if err != nil {
		h.logger.Error("delete tag key", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete tag key", nil)
		return
	}
	if rows == 0 {
		// Raced with another delete between the GetTagKey check and here.
		middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "tag key not found", nil)
		return
	}
	c.Status(http.StatusNoContent)
}

// validKeyParam validates the :key path param against the tag-key naming rule,
// writing a 400 and returning ok=false on violation. Validating up front gives a
// clear 400 (rather than a misleading 404) and avoids a pointless DB lookup.
func (h *TagKeysHandler) validKeyParam(c *gin.Context) (string, bool) {
	key := c.Param("key")
	if !isValidTagKey(key) {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "key must be 1-64 chars of [a-z0-9_]", nil)
		return "", false
	}
	return key, true
}

// resolveKey validates the :key path param, then loads the tag key by (org, key),
// writing the appropriate error and returning ok=false when it is invalid or
// missing.
func (h *TagKeysHandler) resolveKey(c *gin.Context) (*db.TagKey, bool) {
	key, ok := h.validKeyParam(c)
	if !ok {
		return nil, false
	}
	k, err := h.db.GetTagKey(c.Request.Context(), orgIDFromClaims(c), key)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "tag key not found", nil)
			return nil, false
		}
		h.logger.Error("resolve tag key", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load tag key", nil)
		return nil, false
	}
	return k, true
}

// ListValues handles GET /api/v1/tag-keys/:key/values.
func (h *TagKeysHandler) ListValues(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	key, ok := h.resolveKey(c)
	if !ok {
		return
	}
	values, err := h.db.ListTagValues(c.Request.Context(), key.ID)
	if err != nil {
		h.logger.Error("list tag values", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list tag values", nil)
		return
	}
	resp := make([]tagValueResponse, 0, len(values))
	for _, v := range values {
		resp = append(resp, toTagValueResponse(v))
	}
	c.JSON(http.StatusOK, gin.H{"items": resp, "total": len(resp)})
}

// createTagValueRequest is the body for POST /api/v1/tag-keys/:key/values.
type createTagValueRequest struct {
	Value string `json:"value" binding:"required"`
}

// CreateValue handles POST /api/v1/tag-keys/:key/values.
func (h *TagKeysHandler) CreateValue(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	key, ok := h.resolveKey(c)
	if !ok {
		return
	}
	var req createTagValueRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}
	value := strings.TrimSpace(req.Value)
	if value == "" {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", "value must not be empty", nil)
		return
	}
	v, err := h.db.CreateTagValue(c.Request.Context(), key.ID, value)
	if err != nil {
		if isUniqueViolation(err) {
			middleware.RespondError(c, http.StatusConflict, "ALREADY_EXISTS", "value already exists", nil)
			return
		}
		h.logger.Error("create tag value", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create tag value", nil)
		return
	}
	c.JSON(http.StatusCreated, toTagValueResponse(v))
}

// DeleteValue handles DELETE /api/v1/tag-keys/:key/values/:vid. Removing a value
// does not touch already-tagged files (per design); it only stops the value from
// being offered for future collection/selection.
func (h *TagKeysHandler) DeleteValue(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	key, ok := h.resolveKey(c)
	if !ok {
		return
	}
	vid, err := uuid.Parse(c.Param("vid"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid value id", nil)
		return
	}
	rows, err := h.db.DeleteTagValue(c.Request.Context(), vid, key.ID)
	if err != nil {
		h.logger.Error("delete tag value", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete tag value", nil)
		return
	}
	if rows == 0 {
		middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "value not found", nil)
		return
	}
	c.Status(http.StatusNoContent)
}

// isValidTagKey reports whether a key is 1-64 chars of [a-z0-9_], matching the
// controlled-vocabulary convention (lowercase, machine-friendly identifiers).
func isValidTagKey(key string) bool {
	if key == "" || len(key) > 64 {
		return false
	}
	for _, r := range key {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '_') {
			return false
		}
	}
	return true
}
