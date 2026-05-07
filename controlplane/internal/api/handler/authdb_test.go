package handler_test

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/byw-dev/fileagent/controlplane/internal/api/handler"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userCols mirrors the column order returned by GetUserByUsername.
var userCols = []string{
	"id", "org_id", "username", "email", "password_hash", "role",
	"is_active", "last_login_at", "created_at", "updated_at",
}

// addAuthUserRow appends one user row to rows.
func addAuthUserRow(rows *sqlmock.Rows, id uuid.UUID, username string) *sqlmock.Rows {
	now := time.Now().UTC()
	return rows.AddRow(
		id.String(), uuid.New().String(), username, nil, "hash", "org_admin",
		true, nil, now, now,
	)
}

func newAuthDB(t *testing.T) (handler.AuthDB, sqlmock.Sqlmock) {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mockDB.Close() })
	return handler.NewQueriesAuthDB(db.New(mockDB)), mock
}

func TestAuthDB_GetUserByUsername_Success(t *testing.T) {
	authDB, mock := newAuthDB(t)
	id := uuid.New()
	rows := addAuthUserRow(sqlmock.NewRows(userCols), id, "alice")
	mock.ExpectQuery("SELECT").WillReturnRows(rows)

	user, err := authDB.GetUserByUsername(context.Background(), "alice")
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, id, user.ID)
	assert.Equal(t, "alice", user.Username)
}

func TestAuthDB_GetUserByUsername_DBError(t *testing.T) {
	authDB, mock := newAuthDB(t)
	mock.ExpectQuery("SELECT").WillReturnError(assert.AnError)

	_, err := authDB.GetUserByUsername(context.Background(), "unknown")
	require.Error(t, err)
}

func TestAuthDB_UpdateUserLastLogin_Success(t *testing.T) {
	authDB, mock := newAuthDB(t)
	mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(1, 1))

	err := authDB.UpdateUserLastLogin(context.Background(), uuid.New())
	require.NoError(t, err)
}

func TestAuthDB_UpdateUserLastLogin_DBError(t *testing.T) {
	authDB, mock := newAuthDB(t)
	mock.ExpectExec("UPDATE users").WillReturnError(assert.AnError)

	err := authDB.UpdateUserLastLogin(context.Background(), uuid.New())
	require.Error(t, err)
}
