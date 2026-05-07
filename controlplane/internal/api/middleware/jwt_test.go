package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

const testSecret = "test-jwt-secret"

// testAuthSvc returns an auth.Service backed by testSecret with no Redis (revocation disabled).
func testAuthSvc() auth.Service {
	return auth.New(testSecret, nil)
}

// buildToken creates a valid access token for testing.
func buildToken(t *testing.T, role, orgID, username string, ttl time.Duration) string {
	t.Helper()
	svc := testAuthSvc()
	token, err := svc.GenerateAccessToken("test-subject", orgID, role, username, ttl)
	require.NoError(t, err)
	return token
}

// newTestEngine sets up a minimal Gin engine with the JWT middleware and a
// test handler that echoes back the claims role.
func newTestEngine() *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	logger, _ := zap.NewDevelopment()
	svc := testAuthSvc()
	r.GET("/protected", JWT(svc, logger), func(c *gin.Context) {
		claims := GetClaims(c)
		if claims == nil {
			c.JSON(http.StatusInternalServerError, nil)
			return
		}
		c.JSON(http.StatusOK, gin.H{"role": claims.Role})
	})
	r.GET("/admin", JWT(svc, logger), RequireRole("super_admin"), func(c *gin.Context) {
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
	token := buildToken(t, "org_viewer", "org-1", "alice", time.Hour)
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

func TestJWT_EmptyToken_Returns401(t *testing.T) {
	r := newTestEngine()
	w := doRequest(t, r, "GET", "/protected", "Bearer ")
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWT_ExpiredToken_Returns401(t *testing.T) {
	r := newTestEngine()
	// GenerateAccessToken with negative TTL produces an already-expired token.
	token := buildToken(t, "org_viewer", "org-1", "alice", -time.Hour)
	w := doRequest(t, r, "GET", "/protected", "Bearer "+token)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestJWT_WrongSecret_Returns401(t *testing.T) {
	r := newTestEngine()
	// Build a token signed with a different secret.
	wrongSvc := auth.New("wrong-secret", nil)
	wrongToken, _ := wrongSvc.GenerateAccessToken("subj", "org", "org_viewer", "bob", time.Hour)
	w := doRequest(t, r, "GET", "/protected", "Bearer "+wrongToken)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRequireRole_Authorized(t *testing.T) {
	r := newTestEngine()
	token := buildToken(t, "super_admin", "org-1", "admin", time.Hour)
	w := doRequest(t, r, "GET", "/admin", "Bearer "+token)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestRequireRole_InsufficientRole_Returns403(t *testing.T) {
	r := newTestEngine()
	token := buildToken(t, "org_viewer", "org-1", "alice", time.Hour)
	w := doRequest(t, r, "GET", "/admin", "Bearer "+token)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestJWT_GetClaims_ReturnsNilWhenNotSet(t *testing.T) {
	gin.SetMode(gin.TestMode)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	assert.Nil(t, GetClaims(c))
}
