package handler

import (
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/gin-gonic/gin"
)

// AgentsHandler groups the Agent management handlers.
type AgentsHandler struct{}

// NewAgentsHandler returns a new AgentsHandler.
func NewAgentsHandler() *AgentsHandler { return &AgentsHandler{} }

// List handles GET /api/v1/agents.
func (h *AgentsHandler) List(c *gin.Context) { middleware.NotImplemented(c) }

// Get handles GET /api/v1/agents/:id.
func (h *AgentsHandler) Get(c *gin.Context) { middleware.NotImplemented(c) }

// Approve handles POST /api/v1/agents/:id/approve.
func (h *AgentsHandler) Approve(c *gin.Context) { middleware.NotImplemented(c) }

// Revoke handles POST /api/v1/agents/:id/revoke.
func (h *AgentsHandler) Revoke(c *gin.Context) { middleware.NotImplemented(c) }

// ListDir handles POST /api/v1/agents/:id/list-dir.
func (h *AgentsHandler) ListDir(c *gin.Context) { middleware.NotImplemented(c) }

// ListRules handles GET /api/v1/agents/:id/rules.
func (h *AgentsHandler) ListRules(c *gin.Context) { middleware.NotImplemented(c) }

// CreateRule handles POST /api/v1/agents/:id/rules.
func (h *AgentsHandler) CreateRule(c *gin.Context) { middleware.NotImplemented(c) }

// UpdateRule handles PUT /api/v1/agents/:id/rules/:rid.
func (h *AgentsHandler) UpdateRule(c *gin.Context) { middleware.NotImplemented(c) }

// DeleteRule handles DELETE /api/v1/agents/:id/rules/:rid.
func (h *AgentsHandler) DeleteRule(c *gin.Context) { middleware.NotImplemented(c) }

// ListUploadLogs handles GET /api/v1/agents/:id/upload-logs.
func (h *AgentsHandler) ListUploadLogs(c *gin.Context) { middleware.NotImplemented(c) }
