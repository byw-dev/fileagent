package handler_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── mock UsersDB ──────────────────────────────────────────────────────────────

type mockUsersDB struct {
	users     []*db.User
	listErr   error
	getUser   *db.User
	getErr    error
	createErr error
	updateErr error
	activeErr error
	deleteErr error
	pwdErr    error
	gotActive *bool // captures the last UpdateUserActive value
}

func (m *mockUsersDB) ListUsers(_ context.Context, _ uuid.UUID) ([]*db.User, error) {
	return m.users, m.listErr
}
func (m *mockUsersDB) GetUserByID(_ context.Context, _ uuid.UUID) (*db.User, error) {
	return m.getUser, m.getErr
}
func (m *mockUsersDB) CreateUser(_ context.Context, arg db.CreateUserParams) (*db.User, error) {
	if m.createErr != nil {
		return nil, m.createErr
	}
	return &db.User{
		ID:           uuid.New(),
		OrgID:        arg.OrgID,
		Username:     arg.Username,
		Email:        arg.Email,
		PasswordHash: arg.PasswordHash,
		Role:         arg.Role,
		IsActive:     true,
		CreatedAt:    time.Now(),
	}, nil
}
func (m *mockUsersDB) UpdateUser(_ context.Context, id uuid.UUID, username string, email sql.NullString, role db.UserRole) (*db.User, error) {
	if m.updateErr != nil {
		return nil, m.updateErr
	}
	return &db.User{
		ID:        id,
		OrgID:     uuid.New(),
		Username:  username,
		Email:     email,
		Role:      role,
		IsActive:  true,
		CreatedAt: time.Now(),
	}, nil
}
func (m *mockUsersDB) UpdateUserActive(_ context.Context, _ uuid.UUID, isActive bool) error {
	m.gotActive = &isActive
	return m.activeErr
}
func (m *mockUsersDB) DeleteUser(_ context.Context, _ uuid.UUID) error { return m.deleteErr }
func (m *mockUsersDB) UpdateUserPassword(_ context.Context, _ uuid.UUID, _ string) error {
	return m.pwdErr
}

// ── helpers ───────────────────────────────────────────────────────────────────

func newTestLogger() *zap.Logger {
	l, _ := zap.NewDevelopment()
	return l
}

// injectClaims sets JWT claims into a Gin context so handlers can read them.
func injectClaims(c *gin.Context, role, orgID, userID string) {
	claims := &auth.Claims{}
	claims.Subject = userID
	claims.OrgID = orgID
	claims.Role = role
	c.Set("jwt_claims", claims)
}

func testRouter(h *handler.UsersHandler) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	// Inject dummy super_admin claims for all requests.
	orgID := uuid.New().String()
	userID := uuid.New().String()
	r.Use(func(c *gin.Context) {
		injectClaims(c, "super_admin", orgID, userID)
		c.Next()
	})
	v1 := r.Group("/api/v1")
	v1.GET("/users", h.List)
	v1.POST("/users", h.Create)
	v1.PUT("/users/:id", h.Update)
	v1.PUT("/users/:id/active", h.SetActive)
	v1.DELETE("/users/:id", h.Delete)
	v1.PUT("/users/:id/password", h.UpdatePassword)
	return r
}

// ── List ──────────────────────────────────────────────────────────────────────

func TestUsersHandler_List_Success(t *testing.T) {
	mockDB := &mockUsersDB{users: []*db.User{
		{ID: uuid.New(), OrgID: uuid.New(), Username: "alice", Role: db.UserRoleOrgAdmin, IsActive: true, CreatedAt: time.Now()},
	}}
	h := handler.NewUsersHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/users", nil)
	testRouter(h).ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	data, ok := body["items"].([]interface{})
	require.True(t, ok)
	assert.Len(t, data, 1)
}

func TestUsersHandler_List_DBError(t *testing.T) {
	mockDB := &mockUsersDB{listErr: assert.AnError}
	h := handler.NewUsersHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/users", nil)
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

func TestUsersHandler_List_NilDB_Returns501(t *testing.T) {
	h := handler.NewUsersHandler(nil, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodGet, "/api/v1/users", nil)
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotImplemented, w.Code)
}

// ── Create ────────────────────────────────────────────────────────────────────

func TestUsersHandler_Create_Success(t *testing.T) {
	mockDB := &mockUsersDB{}
	h := handler.NewUsersHandler(mockDB, newTestLogger())
	body := `{"username":"bob","password":"secret123","role":"org_viewer"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusCreated, w.Code)
}

func TestUsersHandler_Create_InvalidRole(t *testing.T) {
	h := handler.NewUsersHandler(&mockUsersDB{}, newTestLogger())
	body := `{"username":"bob","password":"secret123","role":"unknown_role"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUsersHandler_Create_MissingFields(t *testing.T) {
	h := handler.NewUsersHandler(&mockUsersDB{}, newTestLogger())
	body := `{"username":"bob"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUsersHandler_Create_DBError(t *testing.T) {
	h := handler.NewUsersHandler(&mockUsersDB{createErr: assert.AnError}, newTestLogger())
	body := `{"username":"bob","password":"secret123","role":"org_viewer"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPost, "/api/v1/users", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ── Update ────────────────────────────────────────────────────────────────────

func TestUsersHandler_Update_Success(t *testing.T) {
	h := handler.NewUsersHandler(&mockUsersDB{}, newTestLogger())
	id := uuid.New().String()
	body := `{"username":"alice2","role":"org_admin"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/users/"+id, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUsersHandler_Update_InvalidID(t *testing.T) {
	h := handler.NewUsersHandler(&mockUsersDB{}, newTestLogger())
	body := `{"username":"alice2","role":"org_admin"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/users/not-a-uuid", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUsersHandler_Update_NotFound(t *testing.T) {
	h := handler.NewUsersHandler(&mockUsersDB{updateErr: sql.ErrNoRows}, newTestLogger())
	id := uuid.New().String()
	body := `{"username":"alice2","role":"org_admin"}`
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/users/"+id, bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNotFound, w.Code)
}

// ── Delete ────────────────────────────────────────────────────────────────────

func TestUsersHandler_Delete_Success(t *testing.T) {
	h := handler.NewUsersHandler(&mockUsersDB{}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/v1/users/"+uuid.New().String(), nil)
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusNoContent, w.Code)
}

func TestUsersHandler_Delete_InvalidID(t *testing.T) {
	h := handler.NewUsersHandler(&mockUsersDB{}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/v1/users/bad-id", nil)
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

func TestUsersHandler_Delete_DBError(t *testing.T) {
	h := handler.NewUsersHandler(&mockUsersDB{deleteErr: assert.AnError}, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodDelete, "/api/v1/users/"+uuid.New().String(), nil)
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusInternalServerError, w.Code)
}

// ── UpdatePassword ────────────────────────────────────────────────────────────

func TestUsersHandler_UpdatePassword_OwnerSuccess(t *testing.T) {
	// User updating their own password.
	gin.SetMode(gin.TestMode)
	mockDB := &mockUsersDB{}
	h := handler.NewUsersHandler(mockDB, newTestLogger())
	userID := uuid.New()

	r := gin.New()
	r.Use(func(c *gin.Context) {
		// Claims: caller == target user, role = org_viewer
		injectClaims(c, "org_viewer", uuid.New().String(), userID.String())
		c.Next()
	})
	r.PUT("/api/v1/users/:id/password", h.UpdatePassword)

	w := httptest.NewRecorder()
	body := `{"password":"newpass123"}`
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/users/"+userID.String()+"/password", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

func TestUsersHandler_UpdatePassword_ForbiddenOtherUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := handler.NewUsersHandler(&mockUsersDB{}, newTestLogger())

	r := gin.New()
	r.Use(func(c *gin.Context) {
		// Caller is org_viewer but target is a different user.
		injectClaims(c, "org_viewer", uuid.New().String(), uuid.New().String())
		c.Next()
	})
	r.PUT("/api/v1/users/:id/password", h.UpdatePassword)

	w := httptest.NewRecorder()
	body := `{"password":"newpass123"}`
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/users/"+uuid.New().String()+"/password", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)
	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestUsersHandler_UpdatePassword_SuperAdminCanChangeAny(t *testing.T) {
	h := handler.NewUsersHandler(&mockUsersDB{}, newTestLogger())
	// testRouter injects super_admin with a different user ID than the target.
	w := httptest.NewRecorder()
	body := `{"password":"newpass123"}`
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/users/"+uuid.New().String()+"/password", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusOK, w.Code)
}

// Verify middleware.GetClaims pattern works in handler context.
func TestUsersHandler_PasswordTooShort(t *testing.T) {
	h := handler.NewUsersHandler(&mockUsersDB{}, newTestLogger())
	w := httptest.NewRecorder()
	body := `{"password":"short"}`
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/users/"+uuid.New().String()+"/password", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	testRouter(h).ServeHTTP(w, req)
	assert.Equal(t, http.StatusBadRequest, w.Code)
}

// Ensure middleware package is used (import check).
var _ = middleware.GetClaims

// ── SetActive (5b 禁用而非删除) ────────────────────────────────────────────────

func TestUsersHandler_SetActive_Disable(t *testing.T) {
	target := uuid.New()
	mockDB := &mockUsersDB{getUser: &db.User{
		ID: target, OrgID: uuid.New(), Username: "bob", Role: db.UserRoleOrgViewer, IsActive: false, CreatedAt: time.Now(),
	}}
	h := handler.NewUsersHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/users/"+target.String()+"/active", bytes.NewReader([]byte(`{"is_active":false}`)))
	req.Header.Set("Content-Type", "application/json")
	testRouter(h).ServeHTTP(w, req)

	assert.Equal(t, http.StatusOK, w.Code)
	require.NotNil(t, mockDB.gotActive)
	assert.False(t, *mockDB.gotActive)
	var body map[string]interface{}
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &body))
	assert.Equal(t, false, body["is_active"])
}

func TestUsersHandler_SetActive_MissingField(t *testing.T) {
	mockDB := &mockUsersDB{}
	h := handler.NewUsersHandler(mockDB, newTestLogger())
	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/users/"+uuid.New().String()+"/active", bytes.NewReader([]byte(`{}`)))
	req.Header.Set("Content-Type", "application/json")
	testRouter(h).ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Nil(t, mockDB.gotActive)
}

func TestUsersHandler_SetActive_CannotDisableSelf(t *testing.T) {
	self := uuid.New()
	mockDB := &mockUsersDB{}
	h := handler.NewUsersHandler(mockDB, newTestLogger())

	gin.SetMode(gin.TestMode)
	r := gin.New()
	r.Use(func(c *gin.Context) {
		injectClaims(c, "super_admin", uuid.New().String(), self.String())
		c.Next()
	})
	r.PUT("/api/v1/users/:id/active", h.SetActive)

	w := httptest.NewRecorder()
	req, _ := http.NewRequest(http.MethodPut, "/api/v1/users/"+self.String()+"/active", bytes.NewReader([]byte(`{"is_active":false}`)))
	req.Header.Set("Content-Type", "application/json")
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusBadRequest, w.Code)
	assert.Nil(t, mockDB.gotActive) // guard fires before the DB call
}
