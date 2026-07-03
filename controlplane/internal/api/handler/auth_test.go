package handler_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/golang-jwt/jwt/v5"
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

type mockAuthService struct {
	generateAccessTokenFunc  func(subject, orgID, role, username string, ttl time.Duration) (string, error)
	generateRefreshTokenFunc func(subject, orgID, role, username string, ttl time.Duration) (string, error)
	validateTokenFunc        func(tokenStr string) (*auth.Claims, error)
	revokeTokenFunc          func(ctx context.Context, tokenStr string) error
	isRevokedFunc            func(ctx context.Context, jti string) (bool, error)
}

func (m *mockAuthService) GenerateAccessToken(subject, orgID, role, username string, ttl time.Duration) (string, error) {
	if m.generateAccessTokenFunc != nil {
		return m.generateAccessTokenFunc(subject, orgID, role, username, ttl)
	}
	return "", nil
}

func (m *mockAuthService) GenerateRefreshToken(subject, orgID, role, username string, ttl time.Duration) (string, error) {
	if m.generateRefreshTokenFunc != nil {
		return m.generateRefreshTokenFunc(subject, orgID, role, username, ttl)
	}
	return "", nil
}

func (m *mockAuthService) ValidateToken(tokenStr string) (*auth.Claims, error) {
	if m.validateTokenFunc != nil {
		return m.validateTokenFunc(tokenStr)
	}
	return nil, nil
}

func (m *mockAuthService) RevokeToken(ctx context.Context, tokenStr string) error {
	if m.revokeTokenFunc != nil {
		return m.revokeTokenFunc(ctx, tokenStr)
	}
	return nil
}

func (m *mockAuthService) IsRevoked(ctx context.Context, jti string) (bool, error) {
	if m.isRevokedFunc != nil {
		return m.isRevokedFunc(ctx, jti)
	}
	return false, nil
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

func TestLogin_DBError(t *testing.T) {
	authSvc := newTestAuthSvc(t)
	dbMock := &mockAuthDB{err: assert.AnError}
	r := setupTestRouter(t, authSvc, dbMock)

	body := `{"username":"alice","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
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

func TestLogin_AccessTokenGenerationError(t *testing.T) {
	user := newTestAuthUser(t)
	authSvc := &mockAuthService{
		generateAccessTokenFunc: func(subject, orgID, role, username string, ttl time.Duration) (string, error) {
			return "", errors.New("sign access token")
		},
	}
	r := setupTestRouter(t, authSvc, &mockAuthDB{user: user})

	body := `{"username":"alice","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestLogin_RefreshTokenGenerationError(t *testing.T) {
	user := newTestAuthUser(t)
	authSvc := &mockAuthService{
		generateAccessTokenFunc: func(subject, orgID, role, username string, ttl time.Duration) (string, error) {
			return "access-token", nil
		},
		generateRefreshTokenFunc: func(subject, orgID, role, username string, ttl time.Duration) (string, error) {
			return "", errors.New("sign refresh token")
		},
	}
	r := setupTestRouter(t, authSvc, &mockAuthDB{user: user})

	body := `{"username":"alice","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
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

// TestRefresh_WithRefreshToken exercises the canonical contract: the refresh
// token is presented in the JSON body, and the response rotates credentials by
// returning both a new access_token and a new refresh_token.
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

	// Refresh using the JSON body contract (as the Web UI and SDK do).
	refreshBody := `{"refresh_token":"` + refreshToken + `"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", bytes.NewBufferString(refreshBody))
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)

	var refreshResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &refreshResp))
	assert.NotEmpty(t, refreshResp["access_token"])
	// Rotation: the response must carry a fresh refresh_token for the next cycle.
	assert.NotEmpty(t, refreshResp["refresh_token"], "refresh must return a rotated refresh_token")
}

// TestRefresh_HeaderFallback verifies that presenting the refresh token via the
// Authorization: Bearer header still works, for backward compatibility.
func TestRefresh_HeaderFallback(t *testing.T) {
	user := newTestAuthUser(t)
	authSvc := newTestAuthSvc(t)
	dbMock := &mockAuthDB{user: user}
	r := setupTestRouter(t, authSvc, dbMock)

	body := `{"username":"alice","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	require.Equal(t, http.StatusOK, w.Code)

	var loginResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &loginResp))
	refreshToken := loginResp["refresh_token"].(string)

	req2 := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	req2.Header.Set("Authorization", "Bearer "+refreshToken)
	w2 := httptest.NewRecorder()
	r.ServeHTTP(w2, req2)
	require.Equal(t, http.StatusOK, w2.Code)

	var refreshResp map[string]interface{}
	require.NoError(t, json.Unmarshal(w2.Body.Bytes(), &refreshResp))
	assert.NotEmpty(t, refreshResp["access_token"])
	assert.NotEmpty(t, refreshResp["refresh_token"])
}

func TestRefresh_NoToken(t *testing.T) {
	authSvc := newTestAuthSvc(t)
	r := setupTestRouter(t, authSvc, &mockAuthDB{})
	req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

// fakeRedis is an in-memory auth.RedisClient for exercising the JWT blacklist
// (revocation) without a real Redis. It only needs Set + Exists.
type fakeRedis struct {
	keys map[string]struct{}
}

func newFakeRedis() *fakeRedis { return &fakeRedis{keys: map[string]struct{}{}} }

func (f *fakeRedis) Set(_ context.Context, key string, _ interface{}, _ time.Duration) error {
	f.keys[key] = struct{}{}
	return nil
}

func (f *fakeRedis) Exists(_ context.Context, keys ...string) (int64, error) {
	var n int64
	for _, k := range keys {
		if _, ok := f.keys[k]; ok {
			n++
		}
	}
	return n, nil
}

// TestRefresh_RotationRevokesPresentedToken locks the security half of the
// rotation contract: after a successful refresh, the *presented* refresh token
// must be revoked and rejected on reuse. It wires a real Redis-backed auth
// service (via fakeRedis) so revocation is actually effective — a plain
// auth.New(secret, nil) would make revocation a no-op and hide regressions.
func TestRefresh_RotationRevokesPresentedToken(t *testing.T) {
	user := newTestAuthUser(t)
	authSvc := auth.New("test-secret-at-least-32-bytes!!", newFakeRedis())
	r := setupTestRouter(t, authSvc, &mockAuthDB{user: user})

	// Login to obtain the first refresh token.
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login",
		bytes.NewBufferString(`{"username":"alice","password":"testpass"}`))
	loginReq.Header.Set("Content-Type", "application/json")
	loginW := httptest.NewRecorder()
	r.ServeHTTP(loginW, loginReq)
	require.Equal(t, http.StatusOK, loginW.Code)
	var login map[string]any
	require.NoError(t, json.Unmarshal(loginW.Body.Bytes(), &login))
	firstRefresh := login["refresh_token"].(string)

	// Refresh once — succeeds and rotates.
	body := `{"refresh_token":"` + firstRefresh + `"}`
	okReq := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", bytes.NewBufferString(body))
	okReq.Header.Set("Content-Type", "application/json")
	okW := httptest.NewRecorder()
	r.ServeHTTP(okW, okReq)
	require.Equal(t, http.StatusOK, okW.Code)

	// Reusing the original (now revoked) refresh token must be rejected.
	reuseReq := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", bytes.NewBufferString(body))
	reuseReq.Header.Set("Content-Type", "application/json")
	reuseW := httptest.NewRecorder()
	r.ServeHTTP(reuseW, reuseReq)
	assert.Equal(t, http.StatusUnauthorized, reuseW.Code,
		"the presented refresh token must be unusable after rotation")
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

func TestRefresh_RevokedToken_Rejected(t *testing.T) {
	authSvc := &mockAuthService{
		validateTokenFunc: func(tokenStr string) (*auth.Claims, error) {
			return &auth.Claims{
				RegisteredClaims: jwt.RegisteredClaims{ID: uuid.NewString()},
				TokenType:        "refresh",
				OrgID:            uuid.NewString(),
				Role:             string(db.UserRoleSuperAdmin),
				Username:         "alice",
			}, nil
		},
		isRevokedFunc: func(ctx context.Context, jti string) (bool, error) {
			return true, nil
		},
	}
	r := setupTestRouter(t, authSvc, &mockAuthDB{})

	req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer refresh-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusUnauthorized, w.Code)
}

func TestRefresh_GenerateAccessTokenError(t *testing.T) {
	authSvc := &mockAuthService{
		validateTokenFunc: func(tokenStr string) (*auth.Claims, error) {
			return &auth.Claims{
				RegisteredClaims: jwt.RegisteredClaims{Subject: uuid.NewString()},
				OrgID:            uuid.NewString(),
				Role:             string(db.UserRoleSuperAdmin),
				Username:         "alice",
				TokenType:        "refresh",
			}, nil
		},
		generateAccessTokenFunc: func(subject, orgID, role, username string, ttl time.Duration) (string, error) {
			return "", errors.New("sign access token")
		},
	}
	r := setupTestRouter(t, authSvc, &mockAuthDB{})

	req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", nil)
	req.Header.Set("Authorization", "Bearer refresh-token")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// TestRefresh_GenerateRefreshTokenError verifies that a failure to mint the new
// (rotated) refresh token surfaces as a 500, and does not leave the client with
// only half a token pair.
func TestRefresh_GenerateRefreshTokenError(t *testing.T) {
	authSvc := &mockAuthService{
		validateTokenFunc: func(tokenStr string) (*auth.Claims, error) {
			return &auth.Claims{
				RegisteredClaims: jwt.RegisteredClaims{Subject: uuid.NewString()},
				OrgID:            uuid.NewString(),
				Role:             string(db.UserRoleSuperAdmin),
				Username:         "alice",
				TokenType:        "refresh",
			}, nil
		},
		generateAccessTokenFunc: func(subject, orgID, role, username string, ttl time.Duration) (string, error) {
			return "new-access", nil
		},
		generateRefreshTokenFunc: func(subject, orgID, role, username string, ttl time.Duration) (string, error) {
			return "", errors.New("sign refresh token")
		},
	}
	r := setupTestRouter(t, authSvc, &mockAuthDB{})

	body := `{"refresh_token":"some-refresh-token"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/refresh", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusInternalServerError, w.Code)
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

func TestMe_RevokedToken(t *testing.T) {
	authSvc := &mockAuthService{
		validateTokenFunc: func(tokenStr string) (*auth.Claims, error) {
			return &auth.Claims{
				RegisteredClaims: jwt.RegisteredClaims{
					Subject: uuid.NewString(),
					ID:      uuid.NewString(),
				},
				OrgID:     uuid.NewString(),
				Role:      string(db.UserRoleSuperAdmin),
				Username:  "alice",
				TokenType: "access",
			}, nil
		},
		isRevokedFunc: func(ctx context.Context, jti string) (bool, error) {
			return true, nil
		},
	}
	r := setupTestRouter(t, authSvc, &mockAuthDB{})

	req := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	req.Header.Set("Authorization", "Bearer revoked-token")
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
