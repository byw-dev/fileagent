package db

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// deadLetterSchema runs all up-migrations inside one transaction scoped to a
// fresh schema and returns the tx (also the DBTX for Queries). Everything is
// rolled back by t.Cleanup.
func deadLetterSchema(t *testing.T) *sql.Tx {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL dead-letter tests")
	}
	conn, err := sql.Open("postgres", dsn)
	require.NoError(t, err)
	t.Cleanup(func() { _ = conn.Close() })
	ctx := t.Context()
	tx, err := conn.BeginTx(ctx, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = tx.Rollback() })
	schema := "ic4a_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	_, err = tx.ExecContext(ctx, "CREATE SCHEMA "+schema)
	require.NoError(t, err)
	_, err = tx.ExecContext(ctx, "SET LOCAL search_path TO "+schema+",public")
	require.NoError(t, err)
	migrations, err := filepath.Glob("../../migrations/*.up.sql")
	require.NoError(t, err)
	for _, path := range migrations {
		body, e := os.ReadFile(path)
		require.NoError(t, e)
		_, e = tx.ExecContext(ctx, strings.ReplaceAll(strings.ReplaceAll(string(body), "BEGIN;", ""), "COMMIT;", ""))
		require.NoError(t, e, path)
	}
	return tx
}

// TestDeadLetterUpsertMutation pins the IC-4a dead-letter write path against
// real PostgreSQL:
//  1. first write inserts;
//  2. a second write with the SAME dedup key refreshes the row (no duplicate)
//     — a re-drowned event must refresh, not duplicate;
//  3. the ObjectRemoved force-active flag (d) round-trips;
//  4. DeleteDeadLetter removes exactly the one row.
func TestDeadLetterUpsertMutation(t *testing.T) {
	tx := deadLetterSchema(t)
	q := New(tx)
	ctx := t.Context()

	base := UpsertDeadLetterParams{
		DedupKey:  "b1/k1/seq-1",
		EventName: "s3:ObjectCreated:Put",
		Bucket:    "b1",
		Key:       "k1",
		SizeBytes: 100,
		Etag:      sql.NullString{String: "ET1", Valid: true},
		FailCount: 5,
		LastError: sql.NullString{String: "boom", Valid: true},
	}

	// 1. insert
	row, err := q.UpsertDeadLetter(ctx, base)
	require.NoError(t, err)
	assert.Equal(t, "b1/k1/seq-1", row.DedupKey)
	assert.Equal(t, int32(5), row.FailCount)
	assert.False(t, row.Active)

	// 2. same dedup key → refresh, still one row
	base.FailCount = 9
	row2, err := q.UpsertDeadLetter(ctx, base)
	require.NoError(t, err)
	assert.Equal(t, row.ID, row2.ID, "upsert must refresh the same row")
	var n int
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM webhook_dead_letters").Scan(&n))
	assert.Equal(t, 1, n, "a re-drowned event must not duplicate its dead letter")
	assert.Equal(t, int32(9), row2.FailCount)

	// 3. (d) removed flag + raw payload (S2) round-trip
	removed := base
	removed.DedupKey = "b1/k1/seq-2"
	removed.EventName = "s3:ObjectRemoved:Delete"
	removed.Active = true
	removed.RawPayload = sql.NullString{String: "raw-bytes", Valid: true}
	row3, err := q.UpsertDeadLetter(ctx, removed)
	require.NoError(t, err)
	assert.True(t, row3.Active, "ObjectRemoved dead letters must carry the force-active flag")
	assert.Equal(t, "s3:ObjectRemoved:Delete", row3.EventName)
	assert.Equal(t, "raw-bytes", row3.RawPayload.String, "the raw payload must round-trip (S2)")
	var seq sql.NullString
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT event_seq FROM webhook_dead_letters WHERE dedup_key=$1", "b1/k1/seq-2").Scan(&seq))

	// 4. delete removes exactly one
	require.NoError(t, q.DeleteDeadLetter(ctx, "b1/k1/seq-1"))
	require.NoError(t, tx.QueryRowContext(ctx, "SELECT count(*) FROM webhook_dead_letters").Scan(&n))
	assert.Equal(t, 1, n)
	_ = seq
	_ = time.Now
}
