// Package middleware provides Gin middleware used by the Control Plane REST API.
package middleware

import (
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"go.uber.org/zap"
)

// claimsKey is the Gin context key for the parsed JWT claims.
const claimsKey = "jwt_claims"

// Claims holds the custom claims stored in every JWT issued by the Control
// Plane. This struct mirrors system-design.md §5.3.1.
type Claims struct {
	jwt.RegisteredClaims
	OrgID     string `json:"org_id"`
	Role      string `json:"role"`
	Username  string `json:"username"`
	TokenType string `json:"token_type,omitempty"`
}

// JWT returns a Gin middleware that validates the Bearer JWT token in the
// Authorization header.  On success the parsed *Claims are stored in the Gin
// context under claimsKey so downstream handlers can retrieve them via
// GetClaims(c).
//
// The secret parameter is the HMAC-SHA256 signing secret.
func JWT(secret string, logger *zap.Logger) gin.HandlerFunc {
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

		claims := &Claims{}
		token, err := jwt.ParseWithClaims(tokenStr, claims, func(t *jwt.Token) (interface{}, error) {
			if _, ok := t.Method.(*jwt.SigningMethodHMAC); !ok {
				return nil, jwt.ErrSignatureInvalid
			}
			return []byte(secret), nil
		})
		if err != nil || !token.Valid {
			logger.Debug("jwt middleware: invalid token", zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{
				"error": errorBody("INVALID_TOKEN", "Token is invalid or expired", nil),
			})
			return
		}

		// TODO (Phase 2 T2-A1): check jti against Redis blacklist.

		c.Set(claimsKey, claims)
		c.Next()
	}
}

// GetClaims retrieves the parsed JWT claims from the Gin context. It returns
// nil if the JWT middleware has not run or if the claims are not present.
func GetClaims(c *gin.Context) *Claims {
	v, exists := c.Get(claimsKey)
	if !exists {
		return nil
	}
	claims, _ := v.(*Claims)
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
