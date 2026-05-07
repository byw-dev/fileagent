package handler_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

func init() {
	gin.SetMode(gin.TestMode)
}

// ── Mock AuthDB ────────────────────────────────────────────────────────────────

type mockAuthDB struct {
	user *db.User
	err  error
}

func (m *mockAuthDB) GetUserByUsername(_ context.Context, _ string) (*db.User, error) {
	return m.user, m.err
}

func (m *mockAuthDB) UpdateUserLastLogin(_ context.Context, _ uuid.UUID) error {
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func newTestAuthUser(t *testing.T) *db.User {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte("testpass"), bcrypt.MinCost)
	require.NoError(t, err)
	return &db.User{
		ID:           uuid.New(),
		OrgID:        uuid.New(),
		Username:     "alice",
		Role:         db.UserRoleOrgAdmin,
		PasswordHash: string(hash),
		IsActive:     true,
	}
}

func setupTestRouter(t *testing.T, authSvc auth.Service, authDB handler.AuthDB) *gin.Engine {
	t.Helper()
	r := gin.New()
	h := handler.NewAuthHandler(authSvc, authDB)
	r.POST("/api/auth/login", h.Login)
	r.POST("/api/auth/refresh", h.Refresh)
	r.POST("/api/auth/logout", h.Logout)
	r.GET("/api/auth/me", h.Me)
	r.GET("/api/auth/oidc/callback", h.OIDCCallback)
	return r
}

func newTestAuthSvc(t *testing.T) auth.Service {
	t.Helper()
	return auth.New("test-secret-at-least-32-bytes!!", nil)
}

type loginRespBody struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	User         struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
		OrgID    string `json:"org_id"`
	} `json:"user"`
}

type meRespBody struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Role     string `json:"role"`
	OrgID    string `json:"org_id"`
}

// ── Tests: Login ──────────────────────────────────────────────────────────────

func TestLogin_NilAuthSvc_Returns501(t *testing.T) {
	r := setupTestRouter(t, nil, nil)
	body := `{"username":"alice","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestLogin_Success(t *testing.T) {
	user := newTestAuthUser(t)
	authSvc := newTestAuthSvc(t)
	db := &mockAuthDB{user: user}
	r := setupTestRouter(t, authSvc, db)

	body := `{"username":"alice","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	var resp loginRespBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.AccessToken)
	assert.NotEmpty(t, resp.RefreshToken)
	assert.Equal(t, user.ID.String(), resp.User.ID)
	assert.Equal(t, user.Username, resp.User.Username)
	assert.Equal(t, string(user.Role), resp.User.Role)
	assert.Equal(t, user.OrgID.String(), resp.User.OrgID)
}

func TestLogin_WrongPassword(t *testing.T) {
	user := newTestAuthUser(t)
	authSvc := newTestAuthSvc(t)
	dbMock := &mockAuthDB{user: user}
	r := setupTestRouter(t, authSvc, dbMock)

	body := `{"username":"alice","password":"wrongpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestLogin_UserNotFound(t *testing.T) {
	authSvc := newTestAuthSvc(t)
	dbMock := &mockAuthDB{user: nil, err: sql.ErrNoRows}
	r := setupTestRouter(t, authSvc, dbMock)

	body := `{"username":"nobody","password":"pass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestLogin_DisabledUser(t *testing.T) {
	user := newTestAuthUser(t)
	user.IsActive = false
	authSvc := newTestAuthSvc(t)
	dbMock := &mockAuthDB{user: user}
	r := setupTestRouter(t, authSvc, dbMock)

	body := `{"username":"alice","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestLogin_MissingFields(t *testing.T) {
	authSvc := newTestAuthSvc(t)
	dbMock := &mockAuthDB{}
	r := setupTestRouter(t, authSvc, dbMock)

	body := `{"username":"alice"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// ── Tests: Refresh ────────────────────────────────────────────────────────────

func TestRefresh_NilAuthSvc_Returns501(t *testing.T) {
	r := setupTestRouter(t, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer some-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestRefresh_WithRefreshToken(t *testing.T) {
	user := newTestAuthUser(t)
	authSvc := newTestAuthSvc(t)
	dbMock := &mockAuthDB{user: user}
	r := setupTestRouter(t, authSvc, dbMock)

	// First login to get tokens
	body := `{"username":"alice","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var loginResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &loginResp))
	refreshToken := loginResp["refresh_token"].(string)

	// Now refresh
	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	req2.Header.Set("Authorization", "Bearer "+refreshToken)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)

	var refreshResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &refreshResp))
	assert.NotEmpty(t, refreshResp["access_token"])
}

func TestRefresh_NoToken(t *testing.T) {
	authSvc := newTestAuthSvc(t)
	r := setupTestRouter(t, authSvc, &mockAuthDB{})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRefresh_WithAccessToken_Rejected(t *testing.T) {
	user := newTestAuthUser(t)
	authSvc := newTestAuthSvc(t)
	dbMock := &mockAuthDB{user: user}
	r := setupTestRouter(t, authSvc, dbMock)

	// Login to get access token
	body := `{"username":"alice","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var loginResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &loginResp))
	accessToken := loginResp["access_token"].(string)

	// Using access token for refresh should fail
	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	req2.Header.Set("Authorization", "Bearer "+accessToken)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusUnauthorized, w2.Code)
}

// ── Tests: Logout ─────────────────────────────────────────────────────────────

func TestLogout_NilAuthSvc_Returns501(t *testing.T) {
	r := setupTestRouter(t, nil, nil)
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestLogout_WithValidToken(t *testing.T) {
	user := newTestAuthUser(t)
	authSvc := newTestAuthSvc(t)
	dbMock := &mockAuthDB{user: user}
	r := setupTestRouter(t, authSvc, dbMock)

	// Login first
	body := `{"username":"alice","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var loginResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &loginResp))
	accessToken := loginResp["access_token"].(string)

	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	req2.Header.Set("Authorization", "Bearer "+accessToken)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	assert.Equal(t, http.StatusOK, w2.Code)
}

func TestLogout_NoToken_ReturnsOK(t *testing.T) {
	authSvc := newTestAuthSvc(t)
	r := setupTestRouter(t, authSvc, &mockAuthDB{})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/logout", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// ── Tests: Me ─────────────────────────────────────────────────────────────────

func TestMe_NilAuthSvc_Returns501(t *testing.T) {
	r := setupTestRouter(t, nil, nil)
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

func TestMe_WithValidAccessToken(t *testing.T) {
	user := newTestAuthUser(t)
	authSvc := newTestAuthSvc(t)
	dbMock := &mockAuthDB{user: user}
	r := setupTestRouter(t, authSvc, dbMock)

	// Login first
	body := `{"username":"alice","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	var loginResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &loginResp))
	accessToken := loginResp["access_token"].(string)

	req2 := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req2.Header.Set("Authorization", "Bearer "+accessToken)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)

	var meResp meRespBody
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &meResp))
	assert.Equal(t, user.ID.String(), meResp.ID)
	assert.Equal(t, "alice", meResp.Username)
	assert.Equal(t, string(user.Role), meResp.Role)
	assert.Equal(t, user.OrgID.String(), meResp.OrgID)
}

func TestMe_InvalidToken(t *testing.T) {
	authSvc := newTestAuthSvc(t)
	r := setupTestRouter(t, authSvc, &mockAuthDB{})
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer invalid-token-here")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestMe_NoToken(t *testing.T) {
	authSvc := newTestAuthSvc(t)
	r := setupTestRouter(t, authSvc, &mockAuthDB{})
	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// ── Tests: OIDCCallback ───────────────────────────────────────────────────────

func TestOIDCCallback_Returns501(t *testing.T) {
	authSvc := newTestAuthSvc(t)
	r := setupTestRouter(t, authSvc, &mockAuthDB{})
	req := httptest.NewRequest(http.MethodGet, "/api/auth/oidc/callback", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}
