// Package handler contains the Gin HTTP handlers for the Control Plane REST API.
// Phase 1 provides 501 Not Implemented skeletons for all routes.
// Business logic is filled in during Phase 2.
package handler

import (
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/gin-gonic/gin"
)

// AuthHandler groups the authentication-related handlers.
type AuthHandler struct{}

// NewAuthHandler returns a new AuthHandler.
func NewAuthHandler() *AuthHandler { return &AuthHandler{} }

// Login handles POST /api/auth/login.
// Phase 1: 501 placeholder. Implemented in Phase 2 (T2-A8).
func (h *AuthHandler) Login(c *gin.Context) {
	middleware.NotImplemented(c)
}

// Refresh handles POST /api/auth/refresh.
// Phase 1: 501 placeholder. Implemented in Phase 2 (T2-A8).
func (h *AuthHandler) Refresh(c *gin.Context) {
	middleware.NotImplemented(c)
}

// Logout handles POST /api/auth/logout.
// Phase 1: 501 placeholder. Implemented in Phase 2 (T2-A8).
func (h *AuthHandler) Logout(c *gin.Context) {
	middleware.NotImplemented(c)
}

// Me handles GET /api/auth/me.
// Phase 1: 501 placeholder. Implemented in Phase 2 (T2-A8).
func (h *AuthHandler) Me(c *gin.Context) {
	middleware.NotImplemented(c)
}

// OIDCCallback handles GET /api/auth/oidc/callback.
// Reserved for OIDC SSO integration (system-design.md §5.3.3).
// Phase 1: 501 placeholder.
func (h *AuthHandler) OIDCCallback(c *gin.Context) {
	middleware.NotImplemented(c)
}
