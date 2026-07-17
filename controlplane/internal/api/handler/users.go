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
	"golang.org/x/crypto/bcrypt"
)

// UsersDB is the minimal database interface needed by UsersHandler.
type UsersDB interface {
	ListUsers(ctx context.Context, orgID uuid.UUID) ([]*db.User, error)
	GetUserByID(ctx context.Context, id uuid.UUID) (*db.User, error)
	CreateUser(ctx context.Context, arg db.CreateUserParams) (*db.User, error)
	UpdateUser(ctx context.Context, iD uuid.UUID, username string, email sql.NullString, role db.UserRole) (*db.User, error)
	UpdateUserActive(ctx context.Context, iD uuid.UUID, isActive bool) error
	DeleteUser(ctx context.Context, id uuid.UUID) error
	UpdateUserPassword(ctx context.Context, iD uuid.UUID, passwordHash string) error
}

// UsersHandler groups the user-management handlers.
type UsersHandler struct {
	db     UsersDB
	logger *zap.Logger
}

// NewUsersHandler returns a new UsersHandler. Passing nil for usersDB causes
// all methods to return 501 until dependencies are wired.
func NewUsersHandler(usersDB UsersDB, logger *zap.Logger) *UsersHandler {
	return &UsersHandler{db: usersDB, logger: logger}
}

// userResponse is the outbound JSON shape for a user (password_hash excluded).
type userResponse struct {
	ID        string `json:"id"`
	OrgID     string `json:"org_id"`
	Username  string `json:"username"`
	Email     string `json:"email,omitempty"`
	Role      string `json:"role"`
	IsActive  bool   `json:"is_active"`
	CreatedAt string `json:"created_at"`
}

func toUserResponse(u *db.User) userResponse {
	r := userResponse{
		ID:        u.ID.String(),
		OrgID:     u.OrgID.String(),
		Username:  u.Username,
		Role:      string(u.Role),
		IsActive:  u.IsActive,
		CreatedAt: u.CreatedAt.UTC().Format(time.RFC3339),
	}
	if u.Email.Valid {
		r.Email = u.Email.String
	}
	return r
}

// List handles GET /api/v1/users.
func (h *UsersHandler) List(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)
	users, err := h.db.ListUsers(c.Request.Context(), orgID)
	if err != nil {
		h.logger.Error("list users", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to list users", nil)
		return
	}
	resp := make([]userResponse, 0, len(users))
	for _, u := range users {
		resp = append(resp, toUserResponse(u))
	}
	c.JSON(http.StatusOK, gin.H{"items": resp, "total": len(resp)})
}

// createUserRequest is the body expected by POST /api/v1/users.
type createUserRequest struct {
	Username string `json:"username" binding:"required"`
	Email    string `json:"email"`
	Password string `json:"password" binding:"required,min=8"`
	Role     string `json:"role"     binding:"required"`
}

// Create handles POST /api/v1/users.
func (h *UsersHandler) Create(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	orgID := orgIDFromClaims(c)

	var req createUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	role := db.UserRole(req.Role)
	switch role {
	case db.UserRoleSuperAdmin, db.UserRoleOrgAdmin, db.UserRoleOrgViewer:
	default:
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ROLE", "invalid role: "+req.Role, nil)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		h.logger.Error("bcrypt hash", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to hash password", nil)
		return
	}

	params := db.CreateUserParams{
		OrgID:        orgID,
		Username:     req.Username,
		PasswordHash: string(hash),
		Role:         role,
	}
	if req.Email != "" {
		params.Email = sql.NullString{String: req.Email, Valid: true}
	}

	user, err := h.db.CreateUser(c.Request.Context(), params)
	if err != nil {
		h.logger.Error("create user", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to create user", nil)
		return
	}
	c.JSON(http.StatusCreated, toUserResponse(user))
}

// updateUserRequest is the body expected by PUT /api/v1/users/:id.
type updateUserRequest struct {
	Username string `json:"username" binding:"required"`
	Email    string `json:"email"`
	Role     string `json:"role"     binding:"required"`
}

// Update handles PUT /api/v1/users/:id.
func (h *UsersHandler) Update(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid user id", nil)
		return
	}

	var req updateUserRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	role := db.UserRole(req.Role)
	switch role {
	case db.UserRoleSuperAdmin, db.UserRoleOrgAdmin, db.UserRoleOrgViewer:
	default:
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ROLE", "invalid role: "+req.Role, nil)
		return
	}

	var email sql.NullString
	if req.Email != "" {
		email = sql.NullString{String: req.Email, Valid: true}
	}

	user, err := h.db.UpdateUser(c.Request.Context(), id, req.Username, email, role)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "user not found", nil)
			return
		}
		h.logger.Error("update user", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update user", nil)
		return
	}
	c.JSON(http.StatusOK, toUserResponse(user))
}

// Delete handles DELETE /api/v1/users/:id.
func (h *UsersHandler) Delete(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid user id", nil)
		return
	}

	if err := h.db.DeleteUser(c.Request.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "user not found", nil)
			return
		}
		h.logger.Error("delete user", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to delete user", nil)
		return
	}
	c.Status(http.StatusNoContent)
}

// setActiveRequest is the body expected by PUT /api/v1/users/:id/active. The
// pointer makes `binding:"required"` reject a missing field while still allowing
// the explicit value `false` (disable).
type setActiveRequest struct {
	IsActive *bool `json:"is_active" binding:"required"`
}

// SetActive handles PUT /api/v1/users/:id/active — soft-disable or re-enable a
// user instead of hard-deleting them (5b「禁用而非删除」). super_admin only (route
// group); a user cannot disable their own account.
func (h *UsersHandler) SetActive(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid user id", nil)
		return
	}

	var req setActiveRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	if !*req.IsActive && callerIDFromClaims(c) == id {
		middleware.RespondError(c, http.StatusBadRequest, "CANNOT_DISABLE_SELF", "cannot disable your own account", nil)
		return
	}

	if err := h.db.UpdateUserActive(c.Request.Context(), id, *req.IsActive); err != nil {
		h.logger.Error("set user active", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update user", nil)
		return
	}

	// UpdateUserActive is an :exec (no not-found signal); read back to detect a
	// missing user and return the refreshed row.
	user, err := h.db.GetUserByID(c.Request.Context(), id)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			middleware.RespondError(c, http.StatusNotFound, "NOT_FOUND", "user not found", nil)
			return
		}
		h.logger.Error("get user after set active", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to load user", nil)
		return
	}
	c.JSON(http.StatusOK, toUserResponse(user))
}

// updatePasswordRequest is the body expected by PUT /api/v1/users/:id/password.
type updatePasswordRequest struct {
	Password string `json:"password" binding:"required,min=8"`
}

// UpdatePassword handles PUT /api/v1/users/:id/password.
// Any authenticated user may change their own password; super_admin may change any password.
func (h *UsersHandler) UpdatePassword(c *gin.Context) {
	if h.db == nil {
		middleware.NotImplemented(c)
		return
	}
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_ID", "invalid user id", nil)
		return
	}

	var req updatePasswordRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		middleware.RespondError(c, http.StatusBadRequest, "INVALID_REQUEST", err.Error(), nil)
		return
	}

	claims := middleware.GetClaims(c)
	callerID := callerIDFromClaims(c)
	if claims != nil && db.UserRole(claims.Role) != db.UserRoleSuperAdmin && callerID != id {
		middleware.RespondError(c, http.StatusForbidden, "FORBIDDEN", "cannot change another user's password", nil)
		return
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(req.Password), bcrypt.DefaultCost)
	if err != nil {
		h.logger.Error("bcrypt hash", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to hash password", nil)
		return
	}

	if err := h.db.UpdateUserPassword(c.Request.Context(), id, string(hash)); err != nil {
		h.logger.Error("update password", zap.Error(err))
		middleware.RespondError(c, http.StatusInternalServerError, "INTERNAL_ERROR", "failed to update password", nil)
		return
	}
	c.JSON(http.StatusOK, gin.H{"message": "password updated"})
}
