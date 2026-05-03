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

// ── column sets ───────────────────────────────────────────────────────────────

var fileEntryColumns = []string{
	"id", "org_id", "file_type_id", "agent_id", "rule_id", "bucket_id",
	"storage_path", "original_path", "file_name", "size_bytes",
	"sha256", "etag", "content_type", "file_mtime", "status", "uploaded_at",
	"created_at", "updated_at",
}

var uploadLogColumns = []string{
	"id", "org_id", "agent_id", "file_entry_id", "rule_id",
	"original_path", "storage_path", "size_bytes", "bytes_transferred",
	"status", "error_message", "retry_count", "started_at", "finished_at", "created_at",
}

var eventDeliveryColumns = []string{
	"id", "event_rule_id", "event_type", "payload", "status",
	"response_code", "response_body", "attempt_count", "next_retry_at",
	"created_at", "delivered_at",
}

// ── row helpers ───────────────────────────────────────────────────────────────

func fileEntryRow(id, orgID uuid.UUID, storagePath string) *sqlmock.Rows {
	now := time.Now().UTC()
	return sqlmock.NewRows(fileEntryColumns).AddRow(
		id.String(), orgID.String(),
		nil, nil, nil, uuid.New().String(), // file_type_id, agent_id, rule_id, bucket_id
		storagePath, nil, "file.txt", int64(1024),
		nil, nil, nil, nil, // sha256, etag, content_type, file_mtime
		"completed", now,
		now, now,
	)
}

func uploadLogRow(id, orgID, agentID uuid.UUID) *sqlmock.Rows {
	now := time.Now().UTC()
	return sqlmock.NewRows(uploadLogColumns).AddRow(
		id.String(), orgID.String(),
		agentID.String(), nil, nil, // agent_id, file_entry_id, rule_id
		"/src/file.txt", "/dest/file.txt", int64(1024), int64(1024),
		"completed", nil, int32(0), now, now, now,
	)
}

func deliveryRow(id, ruleID uuid.UUID, status string) *sqlmock.Rows {
	now := time.Now().UTC()
	return sqlmock.NewRows(eventDeliveryColumns).AddRow(
		id.String(), ruleID.String(), "file_uploaded", []byte(`{}`), status,
		200, nil, int32(1), nil, now, now,
	)
}

// ── ListFileEntries ───────────────────────────────────────────────────────────

func TestListFileEntries_NoFilter_ReturnsRows(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID := uuid.New()
	id := uuid.New()

	mock.ExpectQuery("SELECT.*FROM file_entries").
		WillReturnRows(fileEntryRow(id, orgID, "/data/file.txt"))

	result, err := q.ListFileEntries(context.Background(), ListFileEntriesParams{
		OrgID: orgID,
		Limit: 20,
	})
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, id, result[0].ID)
}

func TestListFileEntries_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM file_entries").WillReturnError(assert.AnError)

	_, err := q.ListFileEntries(context.Background(), ListFileEntriesParams{
		OrgID: uuid.New(),
		Limit: 20,
	})
	require.Error(t, err)
}

func TestListFileEntries_EmptyResult(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM file_entries").
		WillReturnRows(sqlmock.NewRows(fileEntryColumns))

	result, err := q.ListFileEntries(context.Background(), ListFileEntriesParams{
		OrgID: uuid.New(),
		Limit: 20,
	})
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestListFileEntries_WithAgentFilter(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID := uuid.New()
	agentID := uuid.New()
	id := uuid.New()

	mock.ExpectQuery("SELECT.*FROM file_entries").
		WillReturnRows(fileEntryRow(id, orgID, "/data/file.txt"))

	result, err := q.ListFileEntries(context.Background(), ListFileEntriesParams{
		OrgID:   orgID,
		AgentID: uuid.NullUUID{UUID: agentID, Valid: true},
		Limit:   10,
	})
	require.NoError(t, err)
	require.Len(t, result, 1)
}

func TestListFileEntries_WithStatusFilter(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID := uuid.New()

	mock.ExpectQuery("SELECT.*FROM file_entries").
		WillReturnRows(fileEntryRow(uuid.New(), orgID, "/data/a.txt"))

	result, err := q.ListFileEntries(context.Background(), ListFileEntriesParams{
		OrgID:  orgID,
		Status: NullFileStatus{FileStatus: FileStatusCompleted, Valid: true},
		Limit:  10,
	})
	require.NoError(t, err)
	assert.Len(t, result, 1)
}

func TestListFileEntries_WithCursor(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID := uuid.New()
	now := time.Now().UTC()

	mock.ExpectQuery("SELECT.*FROM file_entries").
		WillReturnRows(sqlmock.NewRows(fileEntryColumns))

	_, err := q.ListFileEntries(context.Background(), ListFileEntriesParams{
		OrgID:           orgID,
		CursorCreatedAt: sql.NullTime{Time: now, Valid: true},
		CursorID:        uuid.NullUUID{UUID: uuid.New(), Valid: true},
		Limit:           10,
	})
	require.NoError(t, err)
}

// ── GetFileEntryByID ──────────────────────────────────────────────────────────

func TestGetFileEntryByID_Found(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("SELECT.*FROM file_entries").
		WillReturnRows(fileEntryRow(id, orgID, "/data/file.txt"))

	got, err := q.GetFileEntryByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
	assert.Equal(t, FileStatusCompleted, got.Status)
}

func TestGetFileEntryByID_NotFound(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM file_entries").
		WillReturnRows(sqlmock.NewRows(fileEntryColumns))

	_, err := q.GetFileEntryByID(context.Background(), uuid.New())
	require.Error(t, err)
	assert.Equal(t, sql.ErrNoRows, err)
}

func TestGetFileEntryByID_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM file_entries").WillReturnError(assert.AnError)

	_, err := q.GetFileEntryByID(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── ListUploadLogs ────────────────────────────────────────────────────────────

func TestListUploadLogs_NoFilter_ReturnsRows(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID := uuid.New()
	id := uuid.New()

	mock.ExpectQuery("SELECT.*FROM upload_logs").
		WillReturnRows(uploadLogRow(id, orgID, uuid.New()))

	result, err := q.ListUploadLogs(context.Background(), ListUploadLogsParams{
		OrgID: orgID,
		Limit: 20,
	})
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, id, result[0].ID)
}

func TestListUploadLogs_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM upload_logs").WillReturnError(assert.AnError)

	_, err := q.ListUploadLogs(context.Background(), ListUploadLogsParams{
		OrgID: uuid.New(),
		Limit: 20,
	})
	require.Error(t, err)
}

func TestListUploadLogs_WithAgentFilter(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	orgID := uuid.New()
	agentID := uuid.New()
	id := uuid.New()

	mock.ExpectQuery("SELECT.*FROM upload_logs").
		WillReturnRows(uploadLogRow(id, orgID, agentID))

	result, err := q.ListUploadLogs(context.Background(), ListUploadLogsParams{
		OrgID:   orgID,
		AgentID: uuid.NullUUID{UUID: agentID, Valid: true},
		Limit:   10,
	})
	require.NoError(t, err)
	require.Len(t, result, 1)
}

func TestListUploadLogs_WithCursor(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	now := time.Now().UTC()

	mock.ExpectQuery("SELECT.*FROM upload_logs").
		WillReturnRows(sqlmock.NewRows(uploadLogColumns))

	_, err := q.ListUploadLogs(context.Background(), ListUploadLogsParams{
		OrgID:           uuid.New(),
		CursorCreatedAt: sql.NullTime{Time: now, Valid: true},
		CursorID:        uuid.NullUUID{UUID: uuid.New(), Valid: true},
		Limit:           10,
	})
	require.NoError(t, err)
}

// ── GetUploadLogByID ──────────────────────────────────────────────────────────

func TestGetUploadLogByID_Found(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	id := uuid.New()
	orgID := uuid.New()

	mock.ExpectQuery("SELECT.*FROM upload_logs").
		WillReturnRows(uploadLogRow(id, orgID, uuid.New()))

	got, err := q.GetUploadLogByID(context.Background(), id)
	require.NoError(t, err)
	assert.Equal(t, id, got.ID)
}

func TestGetUploadLogByID_NotFound(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM upload_logs").
		WillReturnRows(sqlmock.NewRows(uploadLogColumns))

	_, err := q.GetUploadLogByID(context.Background(), uuid.New())
	require.Error(t, err)
	assert.Equal(t, sql.ErrNoRows, err)
}

func TestGetUploadLogByID_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM upload_logs").WillReturnError(assert.AnError)

	_, err := q.GetUploadLogByID(context.Background(), uuid.New())
	require.Error(t, err)
}

// ── ListDeliveriesByRule ──────────────────────────────────────────────────────

func TestListDeliveriesByRule_ReturnsRows(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	ruleID := uuid.New()
	id := uuid.New()

	mock.ExpectQuery("SELECT.*FROM event_deliveries").
		WillReturnRows(deliveryRow(id, ruleID, "delivered"))

	result, err := q.ListDeliveriesByRule(context.Background(), ListDeliveriesByRuleParams{
		EventRuleID: ruleID,
		Limit:       20,
	})
	require.NoError(t, err)
	require.Len(t, result, 1)
	assert.Equal(t, id, result[0].ID)
	assert.Equal(t, ruleID, result[0].EventRuleID)
}

func TestListDeliveriesByRule_DBError(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM event_deliveries").WillReturnError(assert.AnError)

	_, err := q.ListDeliveriesByRule(context.Background(), ListDeliveriesByRuleParams{
		EventRuleID: uuid.New(),
		Limit:       20,
	})
	require.Error(t, err)
}

func TestListDeliveriesByRule_EmptyResult(t *testing.T) {
	q, mock, _ := newTestQueries(t)

	mock.ExpectQuery("SELECT.*FROM event_deliveries").
		WillReturnRows(sqlmock.NewRows(eventDeliveryColumns))

	result, err := q.ListDeliveriesByRule(context.Background(), ListDeliveriesByRuleParams{
		EventRuleID: uuid.New(),
		Limit:       20,
	})
	require.NoError(t, err)
	assert.Empty(t, result)
}

func TestListDeliveriesByRule_WithCursor(t *testing.T) {
	q, mock, _ := newTestQueries(t)
	ruleID := uuid.New()
	now := time.Now().UTC()
	cursorID := uuid.New()

	mock.ExpectQuery("SELECT.*FROM event_deliveries").
		WillReturnRows(deliveryRow(uuid.New(), ruleID, "failed"))

	result, err := q.ListDeliveriesByRule(context.Background(), ListDeliveriesByRuleParams{
		EventRuleID:     ruleID,
		CursorCreatedAt: sql.NullTime{Time: now, Valid: true},
		CursorID:        uuid.NullUUID{UUID: cursorID, Valid: true},
		Limit:           10,
	})
	require.NoError(t, err)
	assert.Len(t, result, 1)
}

// ── NewCursorFrom* helpers ────────────────────────────────────────────────────

func TestNewCursorFromFileEntry(t *testing.T) {
	id := uuid.New()
	now := time.Now().UTC()
	e := &FileEntry{ID: id, CreatedAt: now}

	ts, uid := NewCursorFromFileEntry(e)
	assert.True(t, ts.Valid)
	assert.Equal(t, now, ts.Time)
	assert.True(t, uid.Valid)
	assert.Equal(t, id, uid.UUID)
}

func TestNewCursorFromUploadLog(t *testing.T) {
	id := uuid.New()
	now := time.Now().UTC()
	l := &UploadLog{ID: id, CreatedAt: now}

	ts, uid := NewCursorFromUploadLog(l)
	assert.True(t, ts.Valid)
	assert.Equal(t, now, ts.Time)
	assert.True(t, uid.Valid)
	assert.Equal(t, id, uid.UUID)
}

func TestNewCursorFromDelivery(t *testing.T) {
	id := uuid.New()
	now := time.Now().UTC()
	d := &EventDelivery{ID: id, CreatedAt: now}

	ts, uid := NewCursorFromDelivery(d)
	assert.True(t, ts.Valid)
	assert.Equal(t, now, ts.Time)
	assert.True(t, uid.Valid)
	assert.Equal(t, id, uid.UUID)
}
