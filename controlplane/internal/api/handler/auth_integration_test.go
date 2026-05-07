//go:build integration

package handler_test

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/auth"
	controlplanedb "github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/crypto/bcrypt"
)

const integrationDefaultOrgID = "00000000-0000-0000-0000-000000000001"

type integrationLoginRespBody struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	User         struct {
		ID       string `json:"id"`
		Username string `json:"username"`
		Role     string `json:"role"`
		OrgID    string `json:"org_id"`
	} `json:"user"`
}

func integrationTestDSN(t *testing.T) string {
	t.Helper()
	if dsn := os.Getenv("TEST_DATABASE_URL"); dsn != "" {
		return dsn
	}
	return "postgres://fileagent:fileagent@localhost:5432/fileagent_test?sslmode=disable"
}

func setupIntegrationTestRouter(t *testing.T) (*controlplanedb.DB, *controlplanedb.Queries, *gin.Engine) {
	t.Helper()

	logger := zap.NewNop()
	dsn := integrationTestDSN(t)
	require.NoError(t, controlplanedb.Migrate(dsn, "../../../migrations", logger))

	conn, err := controlplanedb.Open(context.Background(), dsn, logger)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })

	authSvc := auth.New("integration-secret-at-least-32-bytes", nil)
	router := setupTestRouter(t, authSvc, handler.NewQueriesAuthDB(conn.Queries()))
	return conn, conn.Queries(), router
}

func createIntegrationAuthUser(t *testing.T, q *controlplanedb.Queries) *controlplanedb.User {
	t.Helper()

	hash, err := bcrypt.GenerateFromPassword([]byte("testpass"), bcrypt.MinCost)
	require.NoError(t, err)

	user, err := q.CreateUser(context.Background(), controlplanedb.CreateUserParams{
		OrgID:        uuid.MustParse(integrationDefaultOrgID),
		Username:     "auth-it-" + uuid.NewString()[:8],
		Email:        sql.NullString{String: "auth-it@example.com", Valid: true},
		PasswordHash: string(hash),
		Role:         controlplanedb.UserRoleSuperAdmin,
	})
	require.NoError(t, err)
	t.Cleanup(func() {
		require.NoError(t, q.DeleteUser(context.Background(), user.ID))
	})
	return user
}

func TestLogin_Integration_IncludesUserPayload(t *testing.T) {
	_, queries, router := setupIntegrationTestRouter(t)
	user := createIntegrationAuthUser(t, queries)

	body := `{"username":"` + user.Username + `","password":"testpass"}`
	req := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)

	var resp integrationLoginRespBody
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &resp))
	assert.NotEmpty(t, resp.AccessToken)
	assert.NotEmpty(t, resp.RefreshToken)
	assert.Equal(t, user.ID.String(), resp.User.ID)
	assert.Equal(t, user.Username, resp.User.Username)
	assert.Equal(t, string(user.Role), resp.User.Role)
	assert.Equal(t, user.OrgID.String(), resp.User.OrgID)
}

func TestMe_Integration_ReturnsIDField(t *testing.T) {
	_, queries, router := setupIntegrationTestRouter(t)
	user := createIntegrationAuthUser(t, queries)

	loginBody := `{"username":"` + user.Username + `","password":"testpass"}`
	loginReq := httptest.NewRequest(http.MethodPost, "/api/auth/login", bytes.NewBufferString(loginBody))
	loginReq.Header.Set("Content-Type", "application/json")
	loginResp := httptest.NewRecorder()
	router.ServeHTTP(loginResp, loginReq)
	require.Equal(t, http.StatusOK, loginResp.Code)

	var authResp integrationLoginRespBody
	require.NoError(t, json.Unmarshal(loginResp.Body.Bytes(), &authResp))

	meReq := httptest.NewRequest(http.MethodGet, "/api/auth/me", nil)
	meReq.Header.Set("Authorization", "Bearer "+authResp.AccessToken)
	meResp := httptest.NewRecorder()
	router.ServeHTTP(meResp, meReq)

	require.Equal(t, http.StatusOK, meResp.Code)

	var payload map[string]string
	require.NoError(t, json.Unmarshal(meResp.Body.Bytes(), &payload))
	assert.Equal(t, user.ID.String(), payload["id"])
	assert.Equal(t, user.Username, payload["username"])
	assert.Equal(t, string(user.Role), payload["role"])
	assert.Equal(t, user.OrgID.String(), payload["org_id"])
	_, hasLegacyField := payload["user_id"]
	assert.False(t, hasLegacyField)
}
