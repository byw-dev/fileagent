package db

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// userColumns lists all columns returned by user queries.
var userColumns = []string{
	"id", "org_id", "username", "email", "password_hash", "role",
	"is_active", "last_login_at", "created_at", "updated_at",
}

// addUserRow adds a minimal User row to a rows builder.
func addUserRow(rows *sqlmock.Rows, id, orgID uuid.UUID, username string) *sqlmock.Rows {
	now := time.Now().UTC()
	return rows.AddRow(
		id.String(), orgID.String(), username, nil, "hash", "admin",
		true, nil, now, now,
	)
}

// ── CreateUser ────────────────────────────────────────────────────────────────

func TestCreateUser_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id := uuid.New()
	orgID := uuid.New()
	rows := addUserRow(sqlmock.NewRows(userColumns), id, orgID, "admin")
	mock.ExpectQuery("INSERT INTO users").WillReturnRows(rows)

	user, err := q.CreateUser(context.Background(), CreateUserParams{
		OrgID:        orgID,
		Username:     "admin",
		PasswordHash: "hash",
		Role:         UserRoleOrgAdmin,
	})
	require.NoError(t, err)
	require.NotNil(t, user)
	assert.Equal(t, id, user.ID)
}

func TestCreateUser_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("INSERT INTO users").WillReturnError(assert.AnError)

	_, err := q.CreateUser(context.Background(), CreateUserParams{OrgID: uuid.New()})
	require.Error(t, err)
}

// ── GetUserByID ───────────────────────────────────────────────────────────────

func TestGetUserByID_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id := uuid.New()
	rows := addUserRow(sqlmock.NewRows(userColumns), id, uuid.New(), "user1")
	mock.ExpectQuery("SELECT .* FROM users").WillReturnRows(rows)

	user, err := q.GetUserByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, id, user.ID)
}

func TestGetUserByID_NotFound(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("SELECT .* FROM users").WillReturnError(sql.ErrNoRows)

	_, err := q.GetUserByID(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── GetUserByUsername ─────────────────────────────────────────────────────────

func TestGetUserByUsername_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	id := uuid.New()
	rows := addUserRow(sqlmock.NewRows(userColumns), id, uuid.New(), "admin")
	mock.ExpectQuery("SELECT .* FROM users").WillReturnRows(rows)

	user, err := q.GetUserByUsername(context.Background(), "admin")
	require.NoError(t, err)
	assert.Equal(t, id, user.ID)
}

func TestGetUserByUsername_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("SELECT .* FROM users").WillReturnError(assert.AnError)

	_, err := q.GetUserByUsername(context.Background(), "missing")
	require.Error(t, err)
}

// ── ListUsers ─────────────────────────────────────────────────────────────────

func TestListUsers_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	orgID := uuid.New()
	rows := addUserRow(addUserRow(sqlmock.NewRows(userColumns),
		uuid.New(), orgID, "user1"),
		uuid.New(), orgID, "user2")
	mock.ExpectQuery("SELECT .* FROM users").WillReturnRows(rows)

	users, err := q.ListUsers(context.Background(), orgID)
	require.NoError(t, err)
	require.Len(t, users, 2)
}

func TestListUsers_Empty(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	rows := sqlmock.NewRows(userColumns)
	mock.ExpectQuery("SELECT .* FROM users").WillReturnRows(rows)

	users, err := q.ListUsers(context.Background(), uuid.New())
	require.NoError(t, err)
	assert.Empty(t, users)
}

func TestListUsers_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectQuery("SELECT .* FROM users").WillReturnError(assert.AnError)

	_, err := q.ListUsers(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── UpdateUserActive ──────────────────────────────────────────────────────────

func TestUpdateUserActive_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(1, 1))

	err := q.UpdateUserActive(context.Background(), uuid.New(), false)
	require.NoError(t, err)
}

func TestUpdateUserActive_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("UPDATE users").WillReturnError(assert.AnError)

	err := q.UpdateUserActive(context.Background(), uuid.New(), true)
	require.Error(t, err)
}

// ── UpdateUserLastLogin ───────────────────────────────────────────────────────

func TestUpdateUserLastLogin_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(1, 1))

	err := q.UpdateUserLastLogin(context.Background(), uuid.New())
	require.NoError(t, err)
}

func TestUpdateUserLastLogin_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("UPDATE users").WillReturnError(assert.AnError)

	err := q.UpdateUserLastLogin(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── UpdateUserPassword ────────────────────────────────────────────────────────

func TestUpdateUserPassword_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("UPDATE users").WillReturnResult(sqlmock.NewResult(1, 1))

	err := q.UpdateUserPassword(context.Background(), uuid.New(), "new-hash")
	require.NoError(t, err)
}

func TestUpdateUserPassword_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	mock.ExpectExec("UPDATE users").WillReturnError(assert.AnError)

	err := q.UpdateUserPassword(context.Background(), uuid.New(), "bad-hash")
	require.Error(t, err)
}

// ── UpdateUser ────────────────────────────────────────────────────────────────

func TestUpdateUser_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	orgID := uuid.New()

	rows := addUserRow(sqlmock.NewRows(userColumns), id, orgID, "new-username")
	mock.ExpectQuery("UPDATE users").WillReturnRows(rows)

	got, err := q.UpdateUser(context.Background(), id, "new-username",
		sql.NullString{String: "new@example.com", Valid: true}, UserRoleOrgAdmin)
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
}

func TestUpdateUser_NotFound(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("UPDATE users").WillReturnRows(sqlmock.NewRows(userColumns))

	_, err := q.UpdateUser(context.Background(), uuid.New(), "user",
		sql.NullString{}, UserRoleOrgViewer)
	require.Error(t, err)
}

func TestUpdateUser_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("UPDATE users").WillReturnError(assert.AnError)

	_, err := q.UpdateUser(context.Background(), uuid.New(), "user",
		sql.NullString{}, UserRoleOrgViewer)
	require.Error(t, err)
}

// ── DeleteUser ────────────────────────────────────────────────────────────────

func TestDeleteUser_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectExec("DELETE FROM users").WillReturnResult(sqlmock.NewResult(1, 1))

	err := q.DeleteUser(context.Background(), uuid.New())
	require.NoError(t, err)
}

func TestDeleteUser_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectExec("DELETE FROM users").WillReturnError(assert.AnError)

	err := q.DeleteUser(context.Background(), uuid.New())
	require.Error(t, err)
}
