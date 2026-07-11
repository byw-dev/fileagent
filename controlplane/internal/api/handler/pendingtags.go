package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// PendingTagValuesDB is the minimal database interface needed by
// PendingTagValuesHandler. It is satisfied by *db.Queries.
type PendingTagValuesDB interface {
	ListPendingTagValues(ctx context.Context, orgID uuid.UUID) ([]*db.ListPendingTagValuesRow, error)
	GetPendingTagValue(ctx context.Context, id uuid.UUID, orgID uuid.UUID) (*db.PendingTagValue, error)
	CreateTagValueIfAbsent(ctx context.Context, tagKeyID uuid.UUID, value string, orgID uuid.UUID) (int64, error)
	DeletePendingTagValue(ctx context.Context, id uuid.UUID, orgID uuid.UUID) (int64, error)
}

// PendingTagValuesHandler serves the "pending tag values" review queue: values
// that path-variable extraction saw but that are not yet in a controlled key's
// vocabulary (metadata 6c, Phase 1). Listing is open to any authenticated user;
// the approve/reject actions are super_admin only (gated at the router).
type PendingTagValuesHandler struct {
	db     PendingTagValuesDB
	logger *zap.Logger
}

// NewPendingTagValuesHandler returns a new handler. A nil db causes handlers to
// return 501 until dependencies are wired.
func NewPendingTagValuesHandler(pendingDB PendingTagValuesDB, logger *zap.Logger) *PendingTagValuesHandler {
	return &PendingTagValuesHandler{db: pendingDB, logger: logger}
}

// pendingTagValueResponse is the outbound JSON shape for a queued value.
type pendingTagValueResponse struct {
	ID             string `json:"id"`
	Key            string `json:"key"`
	ExtractedValue string `json:"extracted_value"`
	Source         string `json:"source"`
	HitCount       int32  `json:"hit_count"`
	SuggestedValue string `json:"suggested_value,omitempty"`
	FirstSeenAt    string `json:"first_seen_at"`
}

func toPendingTagValueResponse(p *db.ListPendingTagValuesRow) pendingTagValueResponse {
	r := pendingTagValueResponse{
		ID:             p.ID.String(),
		Key:            p.Key,
		ExtractedValue: p.ExtractedValue,
		Source:         p.Source,
		HitCount:       p.HitCount,
		FirstSeenAt:    p.FirstSeenAt.UTC().Format(time.RFC3339),
	}
	if p.SuggestedValue.Valid {
		r.SuggestedValue = p.SuggestedValue.String
	}
	return r
}

// List handles GET /api/v1/pending-tag-values.
func (h *PendingTagValuesHandler) List(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	rows, err := h.db.ListPendingTagValues(c.Request.Context(), orgIDFromClaims(c))
	if err != nil {
		h.logger.Error("list pending tag values", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list pending values", nil)
		return
	}
	resp := make([]pendingTagValueResponse, 0, len(rows))
	for _, p := range rows {
		resp = append(resp, toPendingTagValueResponse(p))
	}
	c.JSON(http.StatusOK, gin.H{"items": resp, "total": len(resp)})
}

// resolvePending loads a pending value scoped to the caller's org, writing the
// appropriate error and returning ok=false when the id is invalid or missing.
func (h *PendingTagValuesHandler) resolvePending(c *gin.Context) (*db.PendingTagValue, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid pending tag value id", nil)
		return nil, false
	}
	p, err := h.db.GetPendingTagValue(c.Request.Context(), id, orgIDFromClaims(c))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "pending value not found", nil)
			return nil, false
		}
		h.logger.Error("get pending tag value", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load pending value", nil)
		return nil, false
	}
	return p, true
}

// Approve handles POST /api/v1/pending-tag-values/:id/approve. It promotes the
// extracted value into the key's controlled vocabulary and removes it from the
// queue. Files already carry the raw value, so no file_tags rewrite is needed;
// the promotion just makes the value a legitimate vocabulary/filter option.
//
// The two writes are sequential rather than transactional: if the delete fails
// after the value is added, the row simply stays queued and a retry is a no-op
// (the value insert is ON CONFLICT DO NOTHING), so the operation self-heals.
func (h *PendingTagValuesHandler) Approve(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	p, ok := h.resolvePending(c)
	if !ok {
		return
	}
	if _, err := h.db.CreateTagValueIfAbsent(c.Request.Context(), p.TagKeyID, p.ExtractedValue, p.OrgID); err != nil {
		h.logger.Error("approve: create tag value", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to approve value", nil)
		return
	}
	rows, err := h.db.DeletePendingTagValue(c.Request.Context(), p.ID, p.OrgID)
	if err != nil {
		h.logger.Error("approve: delete pending value", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to approve value", nil)
		return
	}
	if rows == 0 {
		// The row was resolved concurrently between resolvePending and here.
		middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "pending value not found", nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"key_id": p.TagKeyID.String(), "value": p.ExtractedValue})
}

// Reject handles POST /api/v1/pending-tag-values/:id/reject. It removes the value
// from the queue without adding it to the vocabulary. Already-tagged files keep
// the raw value (per design); it just will not become a filter/rule option.
func (h *PendingTagValuesHandler) Reject(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid pending tag value id", nil)
		return
	}
	rows, err := h.db.DeletePendingTagValue(c.Request.Context(), id, orgIDFromClaims(c))
	if err != nil {
		h.logger.Error("reject pending value", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to reject value", nil)
		return
	}
	if rows == 0 {
		middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "pending value not found", nil)
		return
	}
	c.Status(http.StatusNoContent)
}
