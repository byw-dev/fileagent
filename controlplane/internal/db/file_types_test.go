package db

import (
	"context"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var fileTypeColumns = []string{"id", "org_id", "name", "description", "created_by", "created_at"}

var fileTypeRuleColumns = []string{"id", "file_type_id", "path_pattern", "priority", "created_at"}

func fileTypeRow(id, orgID uuid.UUID, name string) *sqlmock.Rows {
	now := time.Now().UTC()
	return sqlmock.NewRows(fileTypeColumns).AddRow(
		id.String(), orgID.String(), name, nil, nil, now,
	)
}

func fileTypeRuleRow(id, fileTypeID uuid.UUID, pattern string) *sqlmock.Rows {
	now := time.Now().UTC()
	return sqlmock.NewRows(fileTypeRuleColumns).AddRow(
		id.String(), fileTypeID.String(), pattern, int32(10), now,
	)
}

// ── ListFileTypes ─────────────────────────────────────────────────────────────

func TestListFileTypes_ReturnsRows(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID := uuid.New()
	id := uuid.New()

	mock.ExpectQuery("SELECT.*FROM file_types").
		WillReturnRows(fileTypeRow(id, orgID, "images"))

	result, err := q.ListFileTypes(context.Background(), orgID)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, id, result[0].ID)
	assert.Equal(t, "images", result[0].Name)
}

func TestListFileTypes_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM file_types").WillReturnError(assert.AnError)

	_, err := q.ListFileTypes(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── GetFileTypeByID ───────────────────────────────────────────────────────────

func TestGetFileTypeByID_Found(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("SELECT.*FROM file_types").
		WillReturnRows(fileTypeRow(id, orgID, "logs"))

	got, err := q.GetFileTypeByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "logs", got.Name)
}

func TestGetFileTypeByID_NotFound(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM file_types").
		WillReturnRows(sqlmock.NewRows(fileTypeColumns))

	_, err := q.GetFileTypeByID(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── CreateFileType ────────────────────────────────────────────────────────────

func TestCreateFileType_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("INSERT INTO file_types").
		WillReturnRows(fileTypeRow(id, orgID, "csv"))

	got, err := q.CreateFileType(context.Background(), orgID, "csv", strNullable("CSV files"), uuidNullable(uuid.New()))
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
}

func TestCreateFileType_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("INSERT INTO file_types").WillReturnError(assert.AnError)

	_, err := q.CreateFileType(context.Background(), uuid.New(), "csv", strNullable(""), uuidNullable(uuid.New()))
	require.Error(t, err)
}

// ── UpdateFileType ────────────────────────────────────────────────────────────

func TestUpdateFileType_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("UPDATE file_types").
		WillReturnRows(fileTypeRow(id, orgID, "renamed"))

	got, err := q.UpdateFileType(context.Background(), id, "renamed", strNullable("new desc"))
	require.NoError(t, err)
	assert.Equal(t, "renamed", got.Name)
}

func TestUpdateFileType_NotFound(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("UPDATE file_types").
		WillReturnRows(sqlmock.NewRows(fileTypeColumns))

	_, err := q.UpdateFileType(context.Background(), uuid.New(), "renamed", strNullable(""))
	require.Error(t, err)
}

// ── DeleteFileType ────────────────────────────────────────────────────────────

func TestDeleteFileType_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectExec("DELETE FROM file_types").WillReturnResult(sqlmock.NewResult(1, 1))

	err := q.DeleteFileType(context.Background(), uuid.New())
	require.NoError(t, err)
}

func TestDeleteFileType_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectExec("DELETE FROM file_types").WillReturnError(assert.AnError)

	err := q.DeleteFileType(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── ListFileTypeRulesByFileTypeID ─────────────────────────────────────────────

func TestListFileTypeRulesByFileTypeID_ReturnsRows(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	fileTypeID := uuid.New()
	id := uuid.New()

	mock.ExpectQuery("SELECT.*FROM file_type_rules").
		WillReturnRows(fileTypeRuleRow(id, fileTypeID, "*.csv"))

	result, err := q.ListFileTypeRulesByFileTypeID(context.Background(), fileTypeID)
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, id, result[0].ID)
	assert.Equal(t, "*.csv", result[0].PathPattern)
}

func TestListFileTypeRulesByFileTypeID_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM file_type_rules").WillReturnError(assert.AnError)

	_, err := q.ListFileTypeRulesByFileTypeID(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── CreateFileTypeRule ────────────────────────────────────────────────────────

func TestCreateFileTypeRule_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	fileTypeID := uuid.New()

	mock.ExpectQuery("INSERT INTO file_type_rules").
		WillReturnRows(fileTypeRuleRow(id, fileTypeID, "*.log"))

	got, err := q.CreateFileTypeRule(context.Background(), fileTypeID, "*.log", 5)
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, "*.log", got.PathPattern)
}

func TestCreateFileTypeRule_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("INSERT INTO file_type_rules").WillReturnError(assert.AnError)

	_, err := q.CreateFileTypeRule(context.Background(), uuid.New(), "*.log", 5)
	require.Error(t, err)
}

// ── DeleteFileTypeRulesByFileTypeID ───────────────────────────────────────────

func TestDeleteFileTypeRulesByFileTypeID_Success(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectExec("DELETE FROM file_type_rules").WillReturnResult(sqlmock.NewResult(3, 3))

	err := q.DeleteFileTypeRulesByFileTypeID(context.Background(), uuid.New())
	require.NoError(t, err)
}

func TestDeleteFileTypeRulesByFileTypeID_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectExec("DELETE FROM file_type_rules").WillReturnError(assert.AnError)

	err := q.DeleteFileTypeRulesByFileTypeID(context.Background(), uuid.New())
	require.Error(t, err)
}
