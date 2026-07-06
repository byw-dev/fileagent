package db

import (
	"context"
	"database/sql"
	"testing"
	"testing/fstest"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── db.go (New, Queries, WithTx) ─────────────────────────────────────────────

func TestNew_ReturnsNonNil(t *testing.T) {
	mockDB, _, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	q := New(mockDB)
	require.NotNil(t, q)
}

func TestWithTx_Success(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	mock.ExpectBegin()
	mock.ExpectCommit()

	db := &DB{DB: mockDB, logger: zap.NewNop()}
	err = db.WithTx(context.Background(), func(q *Queries) error {
		require.NotNil(t, q)
		return nil
	})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithTx_FnError_Rollsback(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	mock.ExpectBegin()
	mock.ExpectRollback()

	db := &DB{DB: mockDB, logger: zap.NewNop()}
	err = db.WithTx(context.Background(), func(q *Queries) error {
		return assert.AnError
	})
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestWithTx_BeginError(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	mock.ExpectBegin().WillReturnError(assert.AnError)

	db := &DB{DB: mockDB, logger: zap.NewNop()}
	err = db.WithTx(context.Background(), func(q *Queries) error {
		return nil
	})
	require.Error(t, err)
}

func TestQueries_WithTx_ReturnsBound(t *testing.T) {
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	mock.ExpectBegin()

	tx, err := mockDB.Begin()
	require.NoError(t, err)

	q := New(mockDB)
	q2 := q.WithTx(tx)
	require.NotNil(t, q2)
	// Discard tx to avoid leaking.
	_ = tx.Rollback()
}

// ── postgres.go (Open) ────────────────────────────────────────────────────────

func TestOpen_InvalidDSN_ReturnsError(t *testing.T) {
	logger := zap.NewNop()
	// Passing an unreachable host causes the ping to fail.
	_, err := Open(context.Background(), "postgres://user:pass@127.0.0.1:19999/nonexistent?sslmode=disable", logger)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "ping")
}

// ── migrate.go (Migrate) ──────────────────────────────────────────────────────

func TestMigrate_EmptySource_ReturnsError(t *testing.T) {
	logger := zap.NewNop()
	// An empty source FS (no *.sql files) causes iofs.New to fail before any DB
	// connection is attempted, so this stays a pure unit test.
	err := Migrate("postgres://user:pass@127.0.0.1:19999/nonexistent?sslmode=disable",
		fstest.MapFS{}, logger)
	require.Error(t, err)
}

// ── DB.Ping ───────────────────────────────────────────────────────────────────

func TestDB_Ping_Success(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	require.NoError(t, err)
	defer mockDB.Close()

	mock.ExpectPing()
	mock.ExpectPing() // one for Open, one for Ping

	db := &DB{DB: mockDB, logger: zap.NewNop()}
	ctx := context.Background()

	// Simulate a successful ping.
	err = db.Ping(ctx)
	require.NoError(t, err)
}

func TestDB_Ping_Failure(t *testing.T) {
	mockDB, mock, err := sqlmock.New(sqlmock.MonitorPingsOption(true))
	require.NoError(t, err)
	defer mockDB.Close()

	mock.ExpectPing().WillReturnError(sql.ErrConnDone)

	db := &DB{DB: mockDB, logger: zap.NewNop()}
	err = db.Ping(context.Background())
	require.Error(t, err)
}

func TestDB_Queries_NotNil(t *testing.T) {
	mockDB, _, err := sqlmock.New()
	require.NoError(t, err)
	defer mockDB.Close()

	db := &DB{DB: mockDB, logger: zap.NewNop()}
	q := db.Queries()
	require.NotNil(t, q)
}
