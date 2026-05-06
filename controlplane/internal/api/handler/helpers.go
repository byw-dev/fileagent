package handler

import (
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/api/middleware"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
)

// orgIDFromClaims extracts org_id from JWT claims stored in the Gin context.
// Returns uuid.Nil when claims are absent or the value is not a valid UUID.
func orgIDFromClaims(c *gin.Context) uuid.UUID {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return uuid.Nil
	}
	id, err := uuid.Parse(claims.OrgID)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// callerIDFromClaims extracts the caller's user ID (JWT Subject) from the Gin context.
func callerIDFromClaims(c *gin.Context) uuid.UUID {
	claims := middleware.GetClaims(c)
	if claims == nil {
		return uuid.Nil
	}
	id, err := uuid.Parse(claims.Subject)
	if err != nil {
		return uuid.Nil
	}
	return id
}

// parseLimitParam reads the ?limit= query param. Returns defaultLimit if absent
// or invalid, capped at maxLimit.
func parseLimitParam(c *gin.Context) int32 {
	const defaultLimit int32 = 50
	const maxLimit int32 = 200
	s := c.Query("limit")
	if s == "" {
		return defaultLimit
	}
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil || n <= 0 {
		return defaultLimit
	}
	if int32(n) > maxLimit {
		return maxLimit
	}
	return int32(n)
}

// pageToken is the struct encoded into a cursor string.
type pageToken struct {
	CreatedAt time.Time `json:"ca"`
	ID        uuid.UUID `json:"id"`
}

// encodeCursor returns a URL-safe base64 cursor from a (created_at, id) pair.
func encodeCursor(t time.Time, id uuid.UUID) string {
	b, _ := json.Marshal(pageToken{CreatedAt: t, ID: id})
	return base64.URLEncoding.EncodeToString(b)
}

// decodeCursor parses a cursor string produced by encodeCursor.
// Returns zero values and nil error when s is empty (first page).
func decodeCursor(s string) (sql.NullTime, uuid.NullUUID, error) {
	if s == "" {
		return sql.NullTime{}, uuid.NullUUID{}, nil
	}
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return sql.NullTime{}, uuid.NullUUID{}, err
	}
	var pt pageToken
	if err := json.Unmarshal(b, &pt); err != nil {
		return sql.NullTime{}, uuid.NullUUID{}, err
	}
	return sql.NullTime{Time: pt.CreatedAt, Valid: true},
		uuid.NullUUID{UUID: pt.ID, Valid: true},
		nil
}
