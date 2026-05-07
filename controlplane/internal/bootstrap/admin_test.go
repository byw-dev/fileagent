package bootstrap

import (
	"context"
	"database/sql"
	"testing"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"golang.org/x/crypto/bcrypt"
)

type fakeUsersStore struct {
	countUsersResp    int64
	countUsersErr     error
	getUserResp       *db.User
	getUserErr        error
	createUserResp    *db.User
	createUserErr     error
	updatePasswordErr error

	createUserCalled bool
	updateCalled     bool
	lastCreateArg    db.CreateUserParams
	lastUpdatedHash  string
}

func (f *fakeUsersStore) CountUsers(_ context.Context) (int64, error) {
	return f.countUsersResp, f.countUsersErr
}

func (f *fakeUsersStore) GetUserByUsername(_ context.Context, _ string) (*db.User, error) {
	return f.getUserResp, f.getUserErr
}

func (f *fakeUsersStore) CreateUser(_ context.Context, arg db.CreateUserParams) (*db.User, error) {
	f.createUserCalled = true
	f.lastCreateArg = arg
	return f.createUserResp, f.createUserErr
}

func (f *fakeUsersStore) UpdateUserPassword(_ context.Context, _ uuid.UUID, passwordHash string) error {
	f.updateCalled = true
	f.lastUpdatedHash = passwordHash
	return f.updatePasswordErr
}

func TestEnsureAdminAccount_CreatesRandomPasswordWhenNoUsers(t *testing.T) {
	store := &fakeUsersStore{countUsersResp: 0, createUserResp: &db.User{}}
	res, err := EnsureAdminAccount(context.Background(), store, "admin", "", false)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Created)
	assert.False(t, res.Reset)
	assert.NotEmpty(t, res.Password)
	assert.True(t, store.createUserCalled)
	assert.Equal(t, "admin", store.lastCreateArg.Username)
	assert.Equal(t, db.UserRoleSuperAdmin, store.lastCreateArg.Role)
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(store.lastCreateArg.PasswordHash), []byte(res.Password)))
}

func TestEnsureAdminAccount_CreatesProvidedPasswordWhenNoUsers(t *testing.T) {
	store := &fakeUsersStore{countUsersResp: 0, createUserResp: &db.User{}}
	res, err := EnsureAdminAccount(context.Background(), store, "admin", "Provided123!", false)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.True(t, res.Created)
	assert.Equal(t, "Provided123!", res.Password)
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(store.lastCreateArg.PasswordHash), []byte("Provided123!")))
}

func TestEnsureAdminAccount_ForceResetExistingAdmin(t *testing.T) {
	store := &fakeUsersStore{
		countUsersResp: 1,
		getUserResp:    &db.User{ID: uuid.New(), Username: "admin"},
	}
	res, err := EnsureAdminAccount(context.Background(), store, "admin", "Reset123!", true)
	require.NoError(t, err)
	require.NotNil(t, res)
	assert.False(t, res.Created)
	assert.True(t, res.Reset)
	assert.True(t, store.updateCalled)
	assert.NoError(t, bcrypt.CompareHashAndPassword([]byte(store.lastUpdatedHash), []byte("Reset123!")))
}

func TestEnsureAdminAccount_ForceResetMissingAdminReturnsError(t *testing.T) {
	store := &fakeUsersStore{
		countUsersResp: 1,
		getUserErr:     sql.ErrNoRows,
	}
	_, err := EnsureAdminAccount(context.Background(), store, "admin", "Reset123!", true)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not found")
}
