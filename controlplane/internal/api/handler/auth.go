// Package handler contains the Gin HTTP handlers for the Control Plane REST API.
// Phase 1 provides 501 Not Implemented skeletons for all routes.
// Business logic is filled in during Phase 2.
package handler

import (
	"context"
	"database/sql"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

// AuthDB is the minimal database interface needed by AuthHandler.
type AuthDB interface {
	GetUserByUsername(ctx context.Context, username string) (*db.User, error)
	UpdateUserLastLogin(ctx context.Context, id uuid.UUID) error
}

// AuthHandler groups the authentication-related handlers.
type AuthHandler struct {
	authSvc auth.Service
	authDB  AuthDB
}

// NewAuthHandler returns a new AuthHandler.
// When authSvc is nil all handlers return 501 (Phase 1 behaviour).
func NewAuthHandler(authSvc auth.Service, authDB AuthDB) *AuthHandler {
	return &AuthHandler{authSvc: authSvc, authDB: authDB}
}

// loginRequest is the body expected by POST /api/auth/login.
type loginRequest struct {
	Username string `json:"username" binding:"required"`
	Password string `json:"password" binding:"required"`
}

// userInfo is the user object embedded in auth responses.
type userInfo struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	OrgID    string `json:"org_id"`
}

// loginResponse is returned by POST /api/auth/login on success.
type loginResponse struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiresIn    int       `json:"expires_in"`
	TokenType    string    `json:"token_type"`
	User         *userInfo `json:"user,omitempty"`
}

// Login handles POST /api/auth/login.
func (h *AuthHandler) Login(c *gin.Context) {
	if h.authSvc == nil {
		middleware.NotImplemented(c)
		return
	}

	var req loginRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{
			"error": middleware.NewErrorBody("INVALID_REQUEST", err.Error(), nil),
		})
		return
	}

	user, err := h.authDB.GetUserByUsername(c.Request.Context(), req.Username)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			c.JSON(http.StatusUnauthorized, gin.H{
				"error": middleware.NewErrorBody("INVALID_CREDENTIALS", "Invalid username or password", nil),
			})
			return
		}
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("INTERNAL_ERROR", "Internal server error", nil),
		})
		return
	}

	if !user.IsActive {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": middleware.NewErrorBody("ACCOUNT_DISABLED", "Account is disabled", nil),
		})
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(req.Password)); err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": middleware.NewErrorBody("INVALID_CREDENTIALS", "Invalid username or password", nil),
		})
		return
	}

	const accessTTL = 2 * time.Hour
	const refreshTTL = 7 * 24 * time.Hour

	accessToken, err := h.authSvc.GenerateAccessToken(
		user.ID.String(),
		user.OrgID.String(), string(user.Role), user.Username,
		accessTTL,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("TOKEN_ERROR", "Failed to generate token", nil),
		})
		return
	}

	refreshToken, err := h.authSvc.GenerateRefreshToken(
		user.ID.String(),
		user.OrgID.String(), string(user.Role), user.Username,
		refreshTTL,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("TOKEN_ERROR", "Failed to generate refresh token", nil),
		})
		return
	}

	_ = h.authDB.UpdateUserLastLogin(c.Request.Context(), user.ID)

	c.JSON(http.StatusOK, loginResponse{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    int(accessTTL.Seconds()),
		TokenType:    "Bearer",
		User: &userInfo{
			ID:       user.ID.String(),
			Username: user.Username,
			Role:     string(user.Role),
			OrgID:    user.OrgID.String(),
		},
	})
}

// Refresh handles POST /api/auth/refresh.
func (h *AuthHandler) Refresh(c *gin.Context) {
	if h.authSvc == nil {
		middleware.NotImplemented(c)
		return
	}

	raw := c.GetHeader("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": middleware.NewErrorBody("MISSING_TOKEN", "Bearer token required", nil),
		})
		return
	}
	tokenStr := strings.TrimPrefix(raw, "Bearer ")

	claims, err := h.authSvc.ValidateToken(tokenStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": middleware.NewErrorBody("INVALID_TOKEN", "Token invalid or expired", nil),
		})
		return
	}

	if claims.TokenType != "refresh" {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": middleware.NewErrorBody("WRONG_TOKEN_TYPE", "Refresh token required", nil),
		})
		return
	}

	revoked, _ := h.authSvc.IsRevoked(c.Request.Context(), claims.ID)
	if revoked {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": middleware.NewErrorBody("TOKEN_REVOKED", "Token has been revoked", nil),
		})
		return
	}

	// Revoke old refresh token.
	_ = h.authSvc.RevokeToken(c.Request.Context(), tokenStr)

	const accessTTL = 2 * time.Hour
	accessToken, err := h.authSvc.GenerateAccessToken(
		claims.Subject,
		claims.OrgID, claims.Role, claims.Username,
		accessTTL,
	)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{
			"error": middleware.NewErrorBody("TOKEN_ERROR", "Failed to generate token", nil),
		})
		return
	}

	c.JSON(http.StatusOK, loginResponse{
		AccessToken: accessToken,
		ExpiresIn:   int(accessTTL.Seconds()),
		TokenType:   "Bearer",
	})
}

// Logout handles POST /api/auth/logout.
func (h *AuthHandler) Logout(c *gin.Context) {
	if h.authSvc == nil {
		middleware.NotImplemented(c)
		return
	}

	raw := c.GetHeader("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") {
		c.JSON(http.StatusOK, gin.H{"message": "ok"})
		return
	}
	tokenStr := strings.TrimPrefix(raw, "Bearer ")

	claims, err := h.authSvc.ValidateToken(tokenStr)
	if err == nil && claims.ID != "" {
		_ = h.authSvc.RevokeToken(c.Request.Context(), tokenStr)
	}

	c.JSON(http.StatusOK, gin.H{"message": "ok"})
}

// Me handles GET /api/auth/me.
func (h *AuthHandler) Me(c *gin.Context) {
	if h.authSvc == nil {
		middleware.NotImplemented(c)
		return
	}

	raw := c.GetHeader("Authorization")
	if !strings.HasPrefix(raw, "Bearer ") {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": middleware.NewErrorBody("MISSING_TOKEN", "Bearer token required", nil),
		})
		return
	}
	tokenStr := strings.TrimPrefix(raw, "Bearer ")

	claims, err := h.authSvc.ValidateToken(tokenStr)
	if err != nil {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": middleware.NewErrorBody("INVALID_TOKEN", "Token invalid or expired", nil),
		})
		return
	}

	revoked, _ := h.authSvc.IsRevoked(c.Request.Context(), claims.ID)
	if revoked {
		c.JSON(http.StatusUnauthorized, gin.H{
			"error": middleware.NewErrorBody("TOKEN_REVOKED", "Token has been revoked", nil),
		})
		return
	}

	c.JSON(http.StatusOK, gin.H{
		"id":       claims.Subject,
		"username": claims.Username,
		"role":     claims.Role,
		"org_id":   claims.OrgID,
	})
}

// OIDCCallback handles GET /api/auth/oidc/callback.
// Reserved for OIDC SSO integration (system-design.md §5.3.3).
// Phase 1: 501 placeholder.
func (h *AuthHandler) OIDCCallback(c *gin.Context) {
	middleware.NotImplemented(c)
}
