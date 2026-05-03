// Package middleware provides Gin middleware used by the Control Plane REST API.
package middleware

import (
	"context"
	"net/http"
	"strings"

	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

// claimsKey is the Gin context key for the parsed JWT claims.
const claimsKey = "jwt_claims"

// JWT returns a Gin middleware that validates the Bearer JWT token in the
// Authorization header. On success the parsed *auth.Claims are stored in the
// Gin context under claimsKey so downstream handlers can retrieve them via
// GetClaims(c).
//
// Token revocation is checked against the Redis blacklist via jwtSvc.IsRevoked.
func JWT(jwtSvc auth.Service, logger *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		raw := c.GetHeader("Authorization")
		if raw == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": errorBody("MISSING_TOKEN", "Authorization header is required", nil),
			})
			return
		}

		if !strings.HasPrefix(raw, "Bearer ") {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": errorBody("INVALID_TOKEN_SCHEME", "Authorization header must use Bearer scheme", nil),
			})
			return
		}

		tokenStr := strings.TrimPrefix(raw, "Bearer ")
		if tokenStr == "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": errorBody("EMPTY_TOKEN", "Token is empty", nil),
			})
			return
		}

		claims, err := jwtSvc.ValidateToken(tokenStr)
		if err != nil {
			logger.Debug("jwt middleware: invalid token", zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": errorBody("INVALID_TOKEN", "Token is invalid or expired", nil),
			})
			return
		}

		// Check Redis blacklist — covers logged-out tokens and revoked agents.
		if claims.ID != "" {
			revoked, err := jwtSvc.IsRevoked(context.Background(), claims.ID)
			if err != nil {
				logger.Warn("jwt middleware: blacklist check error", zap.Error(err))
				// Fail open on transient Redis errors to avoid locking users out.
			} else if revoked {
				c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
					"error": errorBody("TOKEN_REVOKED", "Token has been revoked", nil),
				})
				return
			}
		}

		c.Set(claimsKey, claims)
		c.Next()
	}
}

// GetClaims retrieves the parsed JWT claims from the Gin context. It returns
// nil if the JWT middleware has not run or if the claims are not present.
func GetClaims(c *gin.Context) *auth.Claims {
	v, exists := c.Get(claimsKey)
	if !exists {
		return nil
	}
	claims, _ := v.(*auth.Claims)
	return claims
}

// RequireRole returns a Gin middleware that ensures the caller has at least
// one of the specified roles. Must be used after the JWT middleware.
func RequireRole(roles ...string) gin.HandlerFunc {
	allowed := make(map[string]bool, len(roles))
	for _, r := range roles {
		allowed[r] = true
	}
	return func(c *gin.Context) {
		claims := GetClaims(c)
		if claims == nil || !allowed[claims.Role] {
			c.AbortWithStatusJSON(http.StatusForbidden, gin.H{
				"error": errorBody("FORBIDDEN", "Insufficient role", nil),
			})
			return
		}
		c.Next()
	}
}

// errorBody builds the "error" sub-object used in all API error responses
// (system-design.md §5.11).
func errorBody(code, message string, detail interface{}) map[string]interface{} {
	body := map[string]interface{}{
		"code":    code,
		"message": message,
	}
	if detail != nil {
		body["detail"] = detail
	}
	return body
}

// NewErrorBody builds the "error" sub-object used in all API error responses
// (system-design.md §5.11). It is exported so that handlers in sibling
// packages can produce consistent error responses.
func NewErrorBody(code, message string, detail interface{}) map[string]interface{} {
	return errorBody(code, message, detail)
}
