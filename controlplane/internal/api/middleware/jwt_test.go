package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const testSecret = "test-jwt-secret"

// buildToken creates a signed JWT for testing.
func buildToken(t *testing.T, claims Claims) string {
	t.Helper()
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, err := token.SignedString([]byte(testSecret))
	require.NoError(t, err)
	return signed
}

// newTestEngine sets up a minimal Gin engine with the JWT middleware and a
// test handler that echos back the claims role.
func newTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	logger, _ := zap.NewDevelopment()
	r.GET("/protected", JWT(testSecret, logger), func(c *gin.Context) {
		claims := GetClaims(c)
		if claims == nil {
			c.JSON(http.StatusInternalServerError, nil)
			return
		}
		c.JSON(http.StatusOK, gin.H{"role": claims.Role})
	})
	r.GET("/admin", JWT(testSecret, logger), RequireRole("super_admin"), func(c *gin.Context) {
		c.JSON(http.StatusOK, gin.H{"ok": true})
	})
	return r
}

func doRequest(t *testing.T, r *gin.Engine, method, path, authHeader string) *httptest.ResponseRecorder {
	t.Helper()
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(method, path, nil)
	if authHeader != "" {
		req.Header.Set("Authorization", authHeader)
	}
	r.ServeHTTP(w, req)
	return w
}

func TestJWT_ValidToken_Passes(t *testing.T) {
	r := newTestEngine()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role:     "org_viewer",
		OrgID:    "org-1",
		Username: "alice",
	}
	token := buildToken(t, claims)
	w := doRequest(t, r, "GET", "/protected", "Bearer "+token)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestJWT_MissingHeader_Returns401(t *testing.T) {
	r := newTestEngine()
	w := doRequest(t, r, "GET", "/protected", "")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWT_WrongScheme_Returns401(t *testing.T) {
	r := newTestEngine()
	w := doRequest(t, r, "GET", "/protected", "Basic dXNlcjpwYXNz")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWT_ExpiredToken_Returns401(t *testing.T) {
	r := newTestEngine()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(-time.Hour)),
		},
		Role: "org_viewer",
	}
	token := buildToken(t, claims)
	w := doRequest(t, r, "GET", "/protected", "Bearer "+token)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWT_WrongSecret_Returns401(t *testing.T) {
	r := newTestEngine()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role: "org_viewer",
	}
	// Sign with a different secret.
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims)
	signed, _ := token.SignedString([]byte("wrong-secret"))
	w := doRequest(t, r, "GET", "/protected", "Bearer "+signed)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireRole_Authorized(t *testing.T) {
	r := newTestEngine()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role: "super_admin",
	}
	token := buildToken(t, claims)
	w := doRequest(t, r, "GET", "/admin", "Bearer "+token)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireRole_InsufficientRole_Returns403(t *testing.T) {
	r := newTestEngine()
	claims := Claims{
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role: "org_viewer",
	}
	token := buildToken(t, claims)
	w := doRequest(t, r, "GET", "/admin", "Bearer "+token)
	assert.Equal(t, http.StatusForbidden, w.Code)
}
