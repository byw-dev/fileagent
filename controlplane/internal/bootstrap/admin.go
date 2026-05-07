// Package bootstrap provides first-start initialization helpers.
package bootstrap

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"golang.org/x/crypto/bcrypt"
)

var defaultOrgID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

const generatedPasswordLength = 16

// UsersStore defines the database methods required by admin bootstrap logic.
type UsersStore interface {
	CountUsers(ctx context.Context) (int64, error)
	GetUserByUsername(ctx context.Context, username string) (*db.User, error)
	CreateUser(ctx context.Context, arg db.CreateUserParams) (*db.User, error)
	UpdateUserPassword(ctx context.Context, iD uuid.UUID, passwordHash string) error
}

// AdminBootstrapResult reports which bootstrap action was performed.
type AdminBootstrapResult struct {
	Created  bool
	Reset    bool
	Username string
	Password string
}

// EnsureAdminAccount ensures a bootstrap admin account exists on first startup.
// When forceReset is true and bootstrapPassword is set, it resets the target
// admin password even if users already exist (for recovery).
func EnsureAdminAccount(
	ctx context.Context,
	store UsersStore,
	username, bootstrapPassword string,
	forceReset bool,
) (*AdminBootstrapResult, error) {
	if username == "" {
		return nil, errors.New("bootstrap admin username cannot be empty")
	}
	if forceReset && bootstrapPassword == "" {
		return nil, errors.New("bootstrap admin password is required when force reset is enabled")
	}
	if bootstrapPassword != "" && len(bootstrapPassword) < 8 {
		return nil, errors.New("bootstrap admin password must be at least 8 characters")
	}

	count, err := store.CountUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("bootstrap admin: count users: %w", err)
	}

	if count == 0 {
		password := bootstrapPassword
		if password == "" {
			password, err = generatePassword(generatedPasswordLength)
			if err != nil {
				return nil, fmt.Errorf("bootstrap admin: generate password: %w", err)
			}
		}
		hash, err := bcrypt.GenerateFromPassword([]byte(password), bcrypt.DefaultCost)
		if err != nil {
			return nil, fmt.Errorf("bootstrap admin: hash password: %w", err)
		}
		_, err = store.CreateUser(ctx, db.CreateUserParams{
			OrgID:        defaultOrgID,
			Username:     username,
			PasswordHash: string(hash),
			Role:         db.UserRoleSuperAdmin,
		})
		if err != nil {
			return nil, fmt.Errorf("bootstrap admin: create user: %w", err)
		}
		return &AdminBootstrapResult{
			Created:  true,
			Username: username,
			Password: password,
		}, nil
	}

	if !forceReset {
		return &AdminBootstrapResult{}, nil
	}

	user, err := store.GetUserByUsername(ctx, username)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, fmt.Errorf("bootstrap admin: user %q not found for force reset", username)
		}
		return nil, fmt.Errorf("bootstrap admin: get user by username: %w", err)
	}

	hash, err := bcrypt.GenerateFromPassword([]byte(bootstrapPassword), bcrypt.DefaultCost)
	if err != nil {
		return nil, fmt.Errorf("bootstrap admin: hash reset password: %w", err)
	}
	if err := store.UpdateUserPassword(ctx, user.ID, string(hash)); err != nil {
		return nil, fmt.Errorf("bootstrap admin: update user password: %w", err)
	}
	return &AdminBootstrapResult{
		Reset:    true,
		Username: username,
		Password: bootstrapPassword,
	}, nil
}

// generatePassword returns a cryptographically random password.
func generatePassword(length int) (string, error) {
	const alphabet = "ABCDEFGHJKLMNPQRSTUVWXYZabcdefghijkmnopqrstuvwxyz23456789!@#$%^&*()-_=+"
	if length <= 0 {
		return "", errors.New("password length must be greater than 0")
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	out := make([]byte, length)
	for i, b := range buf {
		out[i] = alphabet[int(b)%len(alphabet)]
	}
	return string(out), nil
}
