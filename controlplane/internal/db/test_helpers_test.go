package db

import (
	"database/sql"

	"github.com/google/uuid"
)

// strNullable returns a valid sql.NullString for s (empty string yields valid but empty).
func strNullable(s string) sql.NullString {
	return sql.NullString{String: s, Valid: true}
}

// uuidNullable returns a valid uuid.NullUUID for id.
func uuidNullable(id uuid.UUID) uuid.NullUUID {
	return uuid.NullUUID{UUID: id, Valid: true}
}
