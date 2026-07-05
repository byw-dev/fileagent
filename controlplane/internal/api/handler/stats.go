package handler

import (
	"context"
	"net/http"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// StatsDB is the minimal database interface needed by StatsHandler.
type StatsDB interface {
	DashboardStats(ctx context.Context, orgID uuid.UUID, now time.Time) (*db.DashboardStats, error)
}

// StatsHandler serves aggregate figures for the dashboard.
type StatsHandler struct {
	db     StatsDB
	logger *zap.Logger
}

// NewStatsHandler returns a new StatsHandler.
func NewStatsHandler(statsDB StatsDB, logger *zap.Logger) *StatsHandler {
	return &StatsHandler{db: statsDB, logger: logger}
}

// Dashboard handles GET /api/v1/stats/dashboard. It returns real aggregate
// counts (agents, files, storage, today's uploads, 7-day upload trend) so the
// UI does not have to approximate them from a recent-logs sample.
func (h *StatsHandler) Dashboard(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)
	stats, err := h.db.DashboardStats(c.Request.Context(), orgID, time.Now().UTC())
	if err != nil {
		h.logger.Error("dashboard stats", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to compute dashboard stats", nil)
		return
	}
	c.JSON(http.StatusOK, stats)
}
