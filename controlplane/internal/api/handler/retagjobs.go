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

// RetagJobsDB is the minimal database interface needed by RetagJobsHandler. It is
// satisfied by *db.Queries.
type RetagJobsDB interface {
	GetRetagJob(ctx context.Context, id uuid.UUID, orgID uuid.UUID) (*db.RetagJob, error)
}

// RetagJobsHandler exposes read-only status of retro-tagging jobs (metadata 6c,
// Phase 1 — MT-5). Jobs are enqueued by actions such as pending-value merge and
// executed asynchronously by worker.RetagWorker; this lets a client poll for
// completion via GET-by-id. Reads are open to any authenticated user, each
// scoped to the caller's org.
type RetagJobsHandler struct {
	db     RetagJobsDB
	logger *zap.Logger
}

// NewRetagJobsHandler returns a new handler. A nil db causes handlers to return
// 501 until dependencies are wired.
func NewRetagJobsHandler(retagDB RetagJobsDB, logger *zap.Logger) *RetagJobsHandler {
	return &RetagJobsHandler{db: retagDB, logger: logger}
}

// retagJobResponse is the outbound JSON shape for a retag job's status.
//
// The raw last_error (wrapped driver/SQL errors) is deliberately omitted: status
// already tells a client whether the job failed, and the detailed cause is kept
// to server logs rather than exposed to every authenticated user in the org.
type retagJobResponse struct {
	ID            string `json:"id"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
	Attempts      int32  `json:"attempts"`
	AffectedCount int32  `json:"affected_count"`
	CreatedAt     string `json:"created_at"`
	FinishedAt    string `json:"finished_at,omitempty"`
}

func toRetagJobResponse(j *db.RetagJob) retagJobResponse {
	r := retagJobResponse{
		ID:            j.ID.String(),
		Kind:          j.Kind,
		Status:        j.Status,
		Attempts:      j.Attempts,
		AffectedCount: j.AffectedCount,
		CreatedAt:     j.CreatedAt.UTC().Format(time.RFC3339),
	}
	if j.FinishedAt.Valid {
		r.FinishedAt = j.FinishedAt.Time.UTC().Format(time.RFC3339)
	}
	return r
}

// Get handles GET /api/v1/retag-jobs/:id. It returns the job's status scoped to
// the caller's org (a cross-org id yields 404, not existence leakage).
func (h *RetagJobsHandler) Get(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid retag job id", nil)
		return
	}
	job, err := h.db.GetRetagJob(c.Request.Context(), id, orgIDFromClaims(c))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "retag job not found", nil)
			return
		}
		h.logger.Error("get retag job", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load retag job", nil)
		return
	}
	c.JSON(http.StatusOK, toRetagJobResponse(job))
}
