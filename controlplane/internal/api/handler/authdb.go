package handler

import (
	"context"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
)

// queriesAuthDB wraps *db.Queries to satisfy the AuthDB interface.
type queriesAuthDB struct {
	q *db.Queries
}

// NewQueriesAuthDB creates an AuthDB backed by sqlc-generated *db.Queries.
func NewQueriesAuthDB(q *db.Queries) AuthDB {
	return &queriesAuthDB{q: q}
}

// GetUserByUsername looks up a user by username.
func (a *queriesAuthDB) GetUserByUsername(ctx context.Context, username string) (*db.User, error) {
	return a.q.GetUserByUsername(ctx, username)
}

// UpdateUserLastLogin updates the last_login_at timestamp for a user.
func (a *queriesAuthDB) UpdateUserLastLogin(ctx context.Context, id uuid.UUID) error {
	return a.q.UpdateUserLastLogin(ctx, id)
}
