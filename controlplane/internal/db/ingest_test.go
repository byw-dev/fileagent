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

var indexedFileColumns = []string{
	"id", "org_id", "file_type_id", "agent_id", "rule_id", "bucket_id",
	"storage_path", "original_path", "file_name", "size_bytes", "sha256", "etag",
	"content_type", "file_mtime", "status", "uploaded_at", "created_at", "updated_at",
	"observed_at", "source", "event_seq", "meta_incomplete",
}

// indexedFileRow returns a complete row shaped like the ingest queries' RETURNING clause.
func indexedFileRow(id, orgID, bucketID uuid.UUID, storagePath, status string) *sqlmock.Rows {
	now := time.Now().UTC()
	return sqlmock.NewRows(indexedFileColumns).AddRow(
		id.String(), orgID.String(), nil, nil, nil, bucketID.String(),
		storagePath, nil, "report.csv", int64(42), nil, nil,
		nil, nil, status, nil, now, now,
		now, "agent", nil, false,
	)
}

// observedFileArgs returns the minimum valid arguments used by wrapper tests.
func observedFileArgs(bucketID uuid.UUID, storagePath string) UpsertIndexedFileParams {
	return UpsertIndexedFileParams{
		OrgID:       uuid.New(),
		BucketID:    bucketID,
		StoragePath: storagePath,
		FileName:    "report.csv",
		SizeBytes:   42,
		Status:      FileStatusCompleted,
		Source:      "agent",
		ObservedAt:  time.Now().UTC(),
	}
}

// TestUpsertObservedFileReturnsAppliedRow verifies the wrapper preserves a normal upsert result.
func TestUpsertObservedFileReturnsAppliedRow(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	bucketID, entryID := uuid.New(), uuid.New()
	arg := observedFileArgs(bucketID, "reports/current.csv")
	mock.ExpectQuery("INSERT INTO file_entries").
		WillReturnRows(indexedFileRow(entryID, arg.OrgID, bucketID, arg.StoragePath, "completed"))

	entry, suppressed, err := q.UpsertObservedFile(context.Background(), arg)

	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, entryID, entry.ID)
	assert.False(t, suppressed)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUpsertObservedFileReturnsExistingRowWhenGuardSuppresses verifies ErrNoRows triggers the fallback lookup.
func TestUpsertObservedFileReturnsExistingRowWhenGuardSuppresses(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	bucketID, entryID := uuid.New(), uuid.New()
	arg := observedFileArgs(bucketID, "reports/stale.csv")
	mock.ExpectQuery("INSERT INTO file_entries").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT .* FROM file_entries WHERE").
		WithArgs(bucketID, arg.StoragePath).
		WillReturnRows(indexedFileRow(entryID, arg.OrgID, bucketID, arg.StoragePath, "completed"))

	entry, suppressed, err := q.UpsertObservedFile(context.Background(), arg)

	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, entryID, entry.ID)
	assert.True(t, suppressed)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUpsertObservedFileReturnsDatabaseError verifies real upsert errors are not mistaken for suppression.
func TestUpsertObservedFileReturnsDatabaseError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	arg := observedFileArgs(uuid.New(), "reports/error.csv")
	mock.ExpectQuery("INSERT INTO file_entries").WillReturnError(assert.AnError)

	entry, suppressed, err := q.UpsertObservedFile(context.Background(), arg)

	assert.NotNil(t, entry)
	assert.False(t, suppressed)
	require.ErrorIs(t, err, assert.AnError)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestUpsertObservedFileReturnsFallbackError verifies a failed suppression lookup remains an error.
func TestUpsertObservedFileReturnsFallbackError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	bucketID := uuid.New()
	arg := observedFileArgs(bucketID, "reports/fallback-error.csv")
	mock.ExpectQuery("INSERT INTO file_entries").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT .* FROM file_entries WHERE").
		WithArgs(bucketID, arg.StoragePath).
		WillReturnError(assert.AnError)

	entry, suppressed, err := q.UpsertObservedFile(context.Background(), arg)

	assert.NotNil(t, entry)
	assert.True(t, suppressed)
	require.ErrorIs(t, err, assert.AnError)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkObservedFileDeletedReturnsAppliedRow verifies a deletion reports that it changed the row.
func TestMarkObservedFileDeletedReturnsAppliedRow(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	bucketID, orgID, entryID := uuid.New(), uuid.New(), uuid.New()
	arg := DeleteIndexedFileParams{BucketID: bucketID, StoragePath: "reports/deleted.csv", ObservedAt: time.Now().UTC(), Source: "minio_event"}
	mock.ExpectQuery("UPDATE file_entries AS fe").
		WillReturnRows(indexedFileRow(entryID, orgID, bucketID, arg.StoragePath, "deleted"))

	entry, suppressed, applied, err := q.MarkObservedFileDeleted(context.Background(), arg)

	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, entryID, entry.ID)
	assert.False(t, suppressed)
	assert.True(t, applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkObservedFileDeletedReturnsExistingRowWhenGuardSuppresses verifies stale deletes are successful no-ops.
func TestMarkObservedFileDeletedReturnsExistingRowWhenGuardSuppresses(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	bucketID, orgID, entryID := uuid.New(), uuid.New(), uuid.New()
	arg := DeleteIndexedFileParams{BucketID: bucketID, StoragePath: "reports/current.csv", ObservedAt: time.Now().UTC(), Source: "minio_event"}
	mock.ExpectQuery("UPDATE file_entries AS fe").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT .* FROM file_entries WHERE").
		WithArgs(bucketID, arg.StoragePath).
		WillReturnRows(indexedFileRow(entryID, orgID, bucketID, arg.StoragePath, "completed"))

	entry, suppressed, applied, err := q.MarkObservedFileDeleted(context.Background(), arg)

	require.NoError(t, err)
	require.NotNil(t, entry)
	assert.Equal(t, entryID, entry.ID)
	assert.True(t, suppressed)
	assert.True(t, applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkObservedFileDeletedTreatsMissingRowAsNoOp verifies an absent object is not retried as a failure.
func TestMarkObservedFileDeletedTreatsMissingRowAsNoOp(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	bucketID := uuid.New()
	arg := DeleteIndexedFileParams{BucketID: bucketID, StoragePath: "reports/missing.csv", ObservedAt: time.Now().UTC(), Source: "minio_event"}
	mock.ExpectQuery("UPDATE file_entries AS fe").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT .* FROM file_entries WHERE").
		WithArgs(bucketID, arg.StoragePath).
		WillReturnError(sql.ErrNoRows)

	entry, suppressed, applied, err := q.MarkObservedFileDeleted(context.Background(), arg)

	require.NoError(t, err)
	assert.Nil(t, entry)
	assert.False(t, suppressed)
	assert.False(t, applied)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkObservedFileDeletedReturnsDatabaseError verifies real delete errors are propagated.
func TestMarkObservedFileDeletedReturnsDatabaseError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	arg := DeleteIndexedFileParams{BucketID: uuid.New(), StoragePath: "reports/error.csv", ObservedAt: time.Now().UTC(), Source: "minio_event"}
	mock.ExpectQuery("UPDATE file_entries AS fe").WillReturnError(assert.AnError)

	entry, suppressed, applied, err := q.MarkObservedFileDeleted(context.Background(), arg)

	assert.NotNil(t, entry)
	assert.False(t, suppressed)
	assert.False(t, applied)
	require.ErrorIs(t, err, assert.AnError)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestMarkObservedFileDeletedReturnsFallbackError verifies lookup failures are not classified as absence.
func TestMarkObservedFileDeletedReturnsFallbackError(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	bucketID := uuid.New()
	arg := DeleteIndexedFileParams{BucketID: bucketID, StoragePath: "reports/fallback-error.csv", ObservedAt: time.Now().UTC(), Source: "minio_event"}
	mock.ExpectQuery("UPDATE file_entries AS fe").WillReturnError(sql.ErrNoRows)
	mock.ExpectQuery("SELECT .* FROM file_entries WHERE").
		WithArgs(bucketID, arg.StoragePath).
		WillReturnError(assert.AnError)

	entry, suppressed, applied, err := q.MarkObservedFileDeleted(context.Background(), arg)

	assert.NotNil(t, entry)
	assert.False(t, suppressed)
	assert.False(t, applied)
	require.ErrorIs(t, err, assert.AnError)
	require.NoError(t, mock.ExpectationsWereMet())
}
