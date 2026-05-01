package indexer

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// ── helpers ───────────────────────────────────────────────────────────────────

func newMockDB(t *testing.T) (*sql.DB, sqlmock.Sqlmock) {
	t.Helper()
	mockDB, mock, err := sqlmock.New()
	require.NoError(t, err)
	t.Cleanup(func() { mockDB.Close() })
	return mockDB, mock
}

// ── UpsertFileEntry ───────────────────────────────────────────────────────────

func TestUpsertFileEntry_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)

	now := time.Now().UTC()
	bucketID := uuid.New()
	orgID := uuid.New()
	entryID := uuid.New()

	rows := sqlmock.NewRows([]string{
		"id", "org_id", "file_type_id", "agent_id", "rule_id", "bucket_id",
		"storage_path", "original_path", "file_name", "size_bytes",
		"sha256", "etag", "content_type", "file_mtime", "status",
		"uploaded_at", "created_at", "updated_at",
	}).AddRow(
		entryID, orgID, nil, nil, nil, bucketID,
		"uploads/file.log", nil, "file.log", int64(1024),
		nil, nil, nil, nil, db.FileStatusCompleted,
		now, now, now,
	)
	mock.ExpectQuery("INSERT INTO file_entries").WillReturnRows(rows)

	arg := UpsertFileEntryParams{
		OrgID:       orgID,
		BucketID:    bucketID,
		StoragePath: "uploads/file.log",
		FileName:    "file.log",
		SizeBytes:   1024,
		Status:      db.FileStatusCompleted,
		UploadedAt:  sql.NullTime{Time: now, Valid: true},
	}
	fe, err := UpsertFileEntry(context.Background(), mockDB, arg)
	require.NoError(t, err)
	require.NotNil(t, fe)
	assert.Equal(t, entryID, fe.ID)
	assert.Equal(t, "uploads/file.log", fe.StoragePath)
}

func TestUpsertFileEntry_DBError(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("INSERT INTO file_entries").WillReturnError(assert.AnError)

	_, err := UpsertFileEntry(context.Background(), mockDB, UpsertFileEntryParams{
		OrgID:    uuid.New(),
		BucketID: uuid.New(),
	})
	require.Error(t, err)
}

// ── CreateUploadLog ───────────────────────────────────────────────────────────

func TestCreateUploadLog_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)

	now := time.Now().UTC()
	logID := uuid.New()
	orgID := uuid.New()
	agentID := uuid.New()

	rows := sqlmock.NewRows([]string{
		"id", "org_id", "agent_id", "file_entry_id", "rule_id",
		"original_path", "storage_path", "size_bytes", "bytes_transferred",
		"status", "error_message", "retry_count", "started_at", "finished_at", "created_at",
	}).AddRow(
		logID, orgID, agentID, nil, nil,
		"/local/file.log", "uploads/file.log", int64(1024), int64(1024),
		"completed", nil, int32(0), now, now, now,
	)
	mock.ExpectQuery("INSERT INTO upload_logs").WillReturnRows(rows)

	arg := CreateUploadLogParams{
		OrgID:            orgID,
		AgentID:          agentID,
		OriginalPath:     "/local/file.log",
		StoragePath:      "uploads/file.log",
		SizeBytes:        1024,
		BytesTransferred: 1024,
		Status:           "completed",
		StartedAt:        now,
		FinishedAt:       sql.NullTime{Time: now, Valid: true},
	}
	ul, err := CreateUploadLog(context.Background(), mockDB, arg)
	require.NoError(t, err)
	require.NotNil(t, ul)
	assert.Equal(t, logID, ul.ID)
}

func TestCreateUploadLog_DBError(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("INSERT INTO upload_logs").WillReturnError(assert.AnError)

	_, err := CreateUploadLog(context.Background(), mockDB, CreateUploadLogParams{
		OrgID:   uuid.New(),
		AgentID: uuid.New(),
	})
	require.Error(t, err)
}

// ── GetBucketByName ───────────────────────────────────────────────────────────

func TestGetBucketByName_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)

	now := time.Now().UTC()
	bucketID := uuid.New()
	orgID := uuid.New()

	rows := sqlmock.NewRows([]string{
		"id", "org_id", "name", "description", "policy_json", "sts_role_arn",
		"created_at", "updated_at",
	}).AddRow(
		bucketID, orgID, "my-bucket", nil, nil, nil, now, now,
	)
	mock.ExpectQuery("SELECT .* FROM buckets").WillReturnRows(rows)

	b, err := GetBucketByName(context.Background(), mockDB, orgID, "my-bucket")
	require.NoError(t, err)
	require.NotNil(t, b)
	assert.Equal(t, bucketID, b.ID)
	assert.Equal(t, "my-bucket", b.Name)
}

func TestGetBucketByName_NotFound(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .* FROM buckets").WillReturnError(sql.ErrNoRows)

	_, err := GetBucketByName(context.Background(), mockDB, uuid.New(), "missing")
	require.Error(t, err)
}

// ── ListEnabledEventRules ─────────────────────────────────────────────────────

func TestListEnabledEventRules_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)

	now := time.Now().UTC()
	ruleID := uuid.New()
	orgID := uuid.New()

	rows := sqlmock.NewRows([]string{
		"id", "org_id", "name", "event_type", "filter",
		"action_type", "action_config", "enabled", "created_by",
		"created_at", "updated_at",
	}).AddRow(
		ruleID, orgID, "rule1", db.EventTypeFileUploaded, []byte(`{}`),
		db.ActionTypeWebhook, []byte(`{"url":"http://example.com"}`), true, nil,
		now, now,
	)
	mock.ExpectQuery("SELECT .* FROM event_rules").WillReturnRows(rows)

	rules, err := ListEnabledEventRules(context.Background(), mockDB, orgID, db.EventTypeFileUploaded)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, ruleID, rules[0].ID)
}

func TestListEnabledEventRules_Empty(t *testing.T) {
	mockDB, mock := newMockDB(t)
	rows := sqlmock.NewRows([]string{
		"id", "org_id", "name", "event_type", "filter",
		"action_type", "action_config", "enabled", "created_by",
		"created_at", "updated_at",
	})
	mock.ExpectQuery("SELECT .* FROM event_rules").WillReturnRows(rows)

	rules, err := ListEnabledEventRules(context.Background(), mockDB, uuid.New(), db.EventTypeFileUploaded)
	require.NoError(t, err)
	assert.Nil(t, rules)
}

func TestListEnabledEventRules_DBError(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .* FROM event_rules").WillReturnError(assert.AnError)

	_, err := ListEnabledEventRules(context.Background(), mockDB, uuid.New(), db.EventTypeFileUploaded)
	require.Error(t, err)
}

// ── ListFileTypeRules ─────────────────────────────────────────────────────────

func TestListFileTypeRules_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)

	now := time.Now().UTC()
	ruleID := uuid.New()
	fileTypeID := uuid.New()

	rows := sqlmock.NewRows([]string{"id", "file_type_id", "path_pattern", "priority", "created_at"}).
		AddRow(ruleID, fileTypeID, "*.log", 10, now)
	mock.ExpectQuery("SELECT .* FROM file_type_rules").WillReturnRows(rows)

	rules, err := ListFileTypeRules(context.Background(), mockDB)
	require.NoError(t, err)
	require.Len(t, rules, 1)
	assert.Equal(t, "*.log", rules[0].PathPattern)
}

func TestListFileTypeRules_DBError(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .* FROM file_type_rules").WillReturnError(assert.AnError)

	_, err := ListFileTypeRules(context.Background(), mockDB)
	require.Error(t, err)
}

// ── CreateEventDelivery ───────────────────────────────────────────────────────

func TestCreateEventDelivery_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)

	now := time.Now().UTC()
	deliveryID := uuid.New()
	ruleID := uuid.New()

	rows := sqlmock.NewRows([]string{
		"id", "event_rule_id", "event_type", "payload", "status",
		"response_code", "response_body", "attempt_count",
		"next_retry_at", "created_at", "delivered_at",
	}).AddRow(
		deliveryID, ruleID, db.EventTypeFileUploaded, []byte(`{}`), "pending",
		nil, nil, int32(0),
		nil, now, nil,
	)
	mock.ExpectQuery("INSERT INTO event_deliveries").WillReturnRows(rows)

	d, err := CreateEventDelivery(context.Background(), mockDB, CreateEventDeliveryParams{
		EventRuleID: ruleID,
		EventType:   db.EventTypeFileUploaded,
		Payload:     []byte(`{}`),
		Status:      "pending",
	})
	require.NoError(t, err)
	require.NotNil(t, d)
	assert.Equal(t, deliveryID, d.ID)
}

func TestCreateEventDelivery_DBError(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("INSERT INTO event_deliveries").WillReturnError(assert.AnError)

	_, err := CreateEventDelivery(context.Background(), mockDB, CreateEventDeliveryParams{
		EventRuleID: uuid.New(),
	})
	require.Error(t, err)
}

// ── UpdateEventDelivery ───────────────────────────────────────────────────────

func TestUpdateEventDelivery_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectExec("UPDATE event_deliveries").WillReturnResult(sqlmock.NewResult(1, 1))

	err := UpdateEventDelivery(context.Background(), mockDB, UpdateEventDeliveryParams{
		ID:     uuid.New(),
		Status: "delivered",
	})
	require.NoError(t, err)
}

func TestUpdateEventDelivery_DBError(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectExec("UPDATE event_deliveries").WillReturnError(assert.AnError)

	err := UpdateEventDelivery(context.Background(), mockDB, UpdateEventDeliveryParams{
		ID:     uuid.New(),
		Status: "failed",
	})
	require.Error(t, err)
}

// ── ListPendingEventDeliveries ────────────────────────────────────────────────

func TestListPendingEventDeliveries_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)

	now := time.Now().UTC()
	deliveryID := uuid.New()
	ruleID := uuid.New()

	rows := sqlmock.NewRows([]string{
		"id", "event_rule_id", "event_type", "payload", "status",
		"response_code", "response_body", "attempt_count",
		"next_retry_at", "created_at", "delivered_at",
	}).AddRow(
		deliveryID, ruleID, db.EventTypeFileUploaded, []byte(`{}`), "pending",
		nil, nil, int32(0), nil, now, nil,
	)
	mock.ExpectQuery("SELECT .* FROM event_deliveries").WillReturnRows(rows)

	deliveries, err := ListPendingEventDeliveries(context.Background(), mockDB)
	require.NoError(t, err)
	require.Len(t, deliveries, 1)
	assert.Equal(t, deliveryID, deliveries[0].ID)
}

func TestListPendingEventDeliveries_Empty(t *testing.T) {
	mockDB, mock := newMockDB(t)
	rows := sqlmock.NewRows([]string{
		"id", "event_rule_id", "event_type", "payload", "status",
		"response_code", "response_body", "attempt_count",
		"next_retry_at", "created_at", "delivered_at",
	})
	mock.ExpectQuery("SELECT .* FROM event_deliveries").WillReturnRows(rows)

	deliveries, err := ListPendingEventDeliveries(context.Background(), mockDB)
	require.NoError(t, err)
	assert.Nil(t, deliveries)
}

func TestListPendingEventDeliveries_DBError(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("SELECT .* FROM event_deliveries").WillReturnError(assert.AnError)

	_, err := ListPendingEventDeliveries(context.Background(), mockDB)
	require.Error(t, err)
}
