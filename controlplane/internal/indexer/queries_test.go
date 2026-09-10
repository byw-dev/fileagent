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
		"uploaded_at", "created_at", "updated_at", "observed_at", "source", "event_seq", "meta_incomplete",
	}).AddRow(
		entryID, orgID, nil, nil, nil, bucketID,
		"uploads/file.log", nil, "file.log", int64(1024),
		nil, nil, nil, nil, db.FileStatusCompleted,
		now, now, now, now, "agent", nil, false,
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
	fe, _, err := UpsertFileEntry(context.Background(), mockDB, arg)
	require.NoError(t, err)
	require.NotNil(t, fe)
	assert.Equal(t, entryID, fe.ID)
	assert.Equal(t, "uploads/file.log", fe.StoragePath)
}

func TestUpsertFileEntry_DBError(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("INSERT INTO file_entries").WillReturnError(assert.AnError)

	_, _, err := UpsertFileEntry(context.Background(), mockDB, UpsertFileEntryParams{
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

// ── MarkFileEntryDeleted ──────────────────────────────────────────────────────

func TestMarkFileEntryDeleted_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)
	now := time.Now().UTC()
	bucketID := uuid.New()
	entryID := uuid.New()

	rows := sqlmock.NewRows([]string{
		"id", "org_id", "file_type_id", "agent_id", "rule_id", "bucket_id",
		"storage_path", "original_path", "file_name", "size_bytes",
		"sha256", "etag", "content_type", "file_mtime", "status",
		"uploaded_at", "created_at", "updated_at", "observed_at", "source", "event_seq", "meta_incomplete",
	}).AddRow(
		entryID, uuid.New(), nil, nil, nil, bucketID,
		"uploads/gone.csv", nil, "gone.csv", int64(10),
		nil, nil, nil, nil, db.FileStatusDeleted,
		now, now, now, now, "minio_event", nil, false,
	)
	mock.ExpectQuery("UPDATE file_entries").WillReturnRows(rows)

	fe, _, _, err := MarkFileEntryDeleted(context.Background(), mockDB, db.DeleteIndexedFileParams{BucketID: bucketID, StoragePath: "uploads/gone.csv"})
	require.NoError(t, err)
	assert.Equal(t, entryID, fe.ID)
	assert.Equal(t, db.FileStatusDeleted, fe.Status)
}

func TestMarkFileEntryDeleted_NotFound(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("UPDATE file_entries").WillReturnError(sql.ErrNoRows)

	mock.ExpectQuery("SELECT .* FROM file_entries").WillReturnError(sql.ErrNoRows)
	_, suppressed, found, err := MarkFileEntryDeleted(context.Background(), mockDB, db.DeleteIndexedFileParams{BucketID: uuid.New(), StoragePath: "missing"})
	require.NoError(t, err)
	require.False(t, suppressed)
	require.False(t, found)
}

// ── MT-2/MT-3 tag queries ───────────────────────────────────────────────────────

func TestUpsertFileTag_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectExec("INSERT INTO file_tags").
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := UpsertFileTag(context.Background(), mockDB, UpsertFileTagParams{
		FileEntryID: uuid.New(), Key: "vendor", Value: "omron", Source: "rule_static"})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetFileTypeIDByName_FoundAndMissing(t *testing.T) {
	mockDB, mock := newMockDB(t)
	id := uuid.New()
	mock.ExpectQuery("SELECT id FROM file_types").
		WillReturnRows(sqlmock.NewRows([]string{"id"}).AddRow(id))
	got, err := GetFileTypeIDByName(context.Background(), mockDB, uuid.New(), "pressure")
	require.NoError(t, err)
	assert.Equal(t, id, got)

	mock.ExpectQuery("SELECT id FROM file_types").WillReturnError(sql.ErrNoRows)
	got, err = GetFileTypeIDByName(context.Background(), mockDB, uuid.New(), "nope")
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, got)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetRuleTagInfo_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("SELECT agent_id, metadata, dest_path_template FROM collection_rules").
		WillReturnRows(sqlmock.NewRows([]string{"agent_id", "metadata", "dest_path_template"}).
			AddRow(uuid.New(), []byte(`{"file_type":"pressure"}`), "data/{site}/{filename}"))
	info, err := GetRuleTagInfo(context.Background(), mockDB, uuid.New(), uuid.New())
	require.NoError(t, err)
	assert.JSONEq(t, `{"file_type":"pressure"}`, string(info.Metadata))
	assert.Equal(t, "data/{site}/{filename}", info.DestPathTemplate)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetRuleTagInfo_NotFound(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("SELECT agent_id, metadata, dest_path_template FROM collection_rules").
		WillReturnError(sql.ErrNoRows)
	_, err := GetRuleTagInfo(context.Background(), mockDB, uuid.New(), uuid.New())
	require.ErrorIs(t, err, sql.ErrNoRows)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestInsertFileTagIfAbsent_InsertedAndConflict(t *testing.T) {
	mockDB, mock := newMockDB(t)
	// Fresh insert returns the file_entry_id.
	fid := uuid.New()
	mock.ExpectQuery("INSERT INTO file_tags").
		WillReturnRows(sqlmock.NewRows([]string{"file_entry_id"}).AddRow(fid))
	inserted, err := InsertFileTagIfAbsent(context.Background(), mockDB, UpsertFileTagParams{
		FileEntryID: fid, Key: "site", Value: "tokyo", Source: "path_var"})
	require.NoError(t, err)
	assert.True(t, inserted)

	// Conflict → DO NOTHING → no row.
	mock.ExpectQuery("INSERT INTO file_tags").WillReturnError(sql.ErrNoRows)
	inserted, err = InsertFileTagIfAbsent(context.Background(), mockDB, UpsertFileTagParams{
		FileEntryID: fid, Key: "site", Value: "tokyo", Source: "path_var"})
	require.NoError(t, err)
	assert.False(t, inserted)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestGetTagKeyByName_FoundAndMissing(t *testing.T) {
	mockDB, mock := newMockDB(t)
	id := uuid.New()
	mock.ExpectQuery("SELECT id, value_controlled, allow_path_var FROM tag_keys").
		WillReturnRows(sqlmock.NewRows([]string{"id", "value_controlled", "allow_path_var"}).AddRow(id, true, true))
	info, found, err := GetTagKeyByName(context.Background(), mockDB, uuid.New(), "site")
	require.NoError(t, err)
	assert.True(t, found)
	assert.Equal(t, id, info.ID)
	assert.True(t, info.ValueControlled)
	assert.True(t, info.AllowPathVar)

	mock.ExpectQuery("SELECT id, value_controlled, allow_path_var FROM tag_keys").WillReturnError(sql.ErrNoRows)
	_, found, err = GetTagKeyByName(context.Background(), mockDB, uuid.New(), "nope")
	require.NoError(t, err)
	assert.False(t, found)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestTagValueExists(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("SELECT EXISTS").
		WillReturnRows(sqlmock.NewRows([]string{"exists"}).AddRow(true))
	ok, err := TagValueExists(context.Background(), mockDB, uuid.New(), "tokyo")
	require.NoError(t, err)
	assert.True(t, ok)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestFindSimilarTagValue_MatchAndNone(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("SELECT value FROM tag_values").
		WillReturnRows(sqlmock.NewRows([]string{"value"}).AddRow("Tokyo"))
	v, err := FindSimilarTagValue(context.Background(), mockDB, uuid.New(), "tokyo")
	require.NoError(t, err)
	assert.Equal(t, "Tokyo", v)

	mock.ExpectQuery("SELECT value FROM tag_values").WillReturnError(sql.ErrNoRows)
	v, err = FindSimilarTagValue(context.Background(), mockDB, uuid.New(), "osaka")
	require.NoError(t, err)
	assert.Equal(t, "", v)
	require.NoError(t, mock.ExpectationsWereMet())
}

func TestUpsertPendingTagValue_Success(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectExec("INSERT INTO pending_tag_values").
		WillReturnResult(sqlmock.NewResult(0, 1))
	err := UpsertPendingTagValue(context.Background(), mockDB, UpsertPendingTagValueParams{
		OrgID: uuid.New(), TagKeyID: uuid.New(), ExtractedValue: "tokyo", Source: "path_var"})
	require.NoError(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}
