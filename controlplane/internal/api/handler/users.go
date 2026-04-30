package handler

import (
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/gin-gonic/gin"
)

// UsersHandler groups the user management handlers.
type UsersHandler struct{}

// NewUsersHandler returns a new UsersHandler.
func NewUsersHandler() *UsersHandler { return &UsersHandler{} }

// List handles GET /api/v1/users.
func (h *UsersHandler) List(c *gin.Context) { middleware.NotImplemented(c) }

// Create handles POST /api/v1/users.
func (h *UsersHandler) Create(c *gin.Context) { middleware.NotImplemented(c) }

// Update handles PUT /api/v1/users/:id.
func (h *UsersHandler) Update(c *gin.Context) { middleware.NotImplemented(c) }

// Delete handles DELETE /api/v1/users/:id.
func (h *UsersHandler) Delete(c *gin.Context) { middleware.NotImplemented(c) }

// UpdatePassword handles PUT /api/v1/users/:id/password.
func (h *UsersHandler) UpdatePassword(c *gin.Context) { middleware.NotImplemented(c) }
