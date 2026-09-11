package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"google.golang.org/protobuf/types/known/timestamppb"
)

var testAgentID = uuid.New()

// ── Mock NATS publisher ──────────────────────────────────────────────────────

type mockNATS struct {
	published map[string][][]byte
	err       error
}

func newMockNATS() *mockNATS {
	return &mockNATS{published: make(map[string][][]byte)}
}

func (m *mockNATS) Publish(subject string, data []byte) error {
	if m.err != nil {
		return m.err
	}
	m.published[subject] = append(m.published[subject], data)
	return nil
}

// ── Mock IndexerStore ────────────────────────────────────────────────────────

// mockIndexerStore fully implements IndexerStore with configurable responses.
type mockIndexerStore struct {
	bucket           *db.Bucket
	bucketErr        error
	fileEntry        *db.FileEntry
	upsertErr        error
	suppressed       bool
	deleteSuppressed bool
	ruleAgentID      uuid.UUID
	lastLog          CreateUploadLogParams
	upsertCalls      int
	uploadLogCalls   int
	deletedEntry     *db.FileEntry
	deleteErr        error
	uploadLog        *db.UploadLog
	uploadLogErr     error
	typeRules        []*db.FileTypeRule
	typeRulesErr     error

	ruleMeta         json.RawMessage
	destPathTemplate string
	ruleMetaErr      error
	fileTypeByName   map[string]uuid.UUID
	fileTypeErr      error
	upsertedTags     []UpsertFileTagParams
	upsertTagErr     error
	lastUpsert       UpsertFileEntryParams

	// path-var / pending-queue (MT-3)
	insertedTags   []UpsertFileTagParams
	insertReturns  bool // whether InsertFileTagIfAbsent reports a fresh insert
	insertTagErr   error
	tagKeys        map[string]TagKeyInfo // key → controlled key info
	tagKeyErr      error
	existingValues map[string]bool       // "value" → registered in tag_values
	tagValueErr    error
	similarValue   string
	similarErr     error
	pendingUpserts []UpsertPendingTagValueParams
	pendingErr     error
}

func (m *mockIndexerStore) GetBucketByName(_ context.Context, _ uuid.UUID, _ string) (*db.Bucket, error) {
	return m.bucket, m.bucketErr
}

func (m *mockIndexerStore) UpsertFileEntry(_ context.Context, params UpsertFileEntryParams) (*db.FileEntry, bool, error) {
	m.upsertCalls++
	m.lastUpsert = params
	return m.fileEntry, m.suppressed, m.upsertErr
}

func (m *mockIndexerStore) MarkFileEntryDeleted(_ context.Context, _ db.DeleteIndexedFileParams) (*db.FileEntry, bool, bool, error) {
	if m.deleteErr == sql.ErrNoRows {
		return nil, false, false, nil
	}
	return m.deletedEntry, m.deleteSuppressed, m.deletedEntry != nil, m.deleteErr
}

func (m *mockIndexerStore) CreateUploadLog(_ context.Context, p CreateUploadLogParams) (*db.UploadLog, error) {
	m.uploadLogCalls++
	m.lastLog = p
	return m.uploadLog, m.uploadLogErr
}

func (m *mockIndexerStore) ListFileTypeRules(_ context.Context) ([]*db.FileTypeRule, error) {
	return m.typeRules, m.typeRulesErr
}

func (m *mockIndexerStore) GetRuleTagInfo(_ context.Context, _, _ uuid.UUID) (RuleTagInfo, error) {
	if m.ruleMetaErr != nil {
		return RuleTagInfo{}, m.ruleMetaErr
	}
	owner := m.ruleAgentID
	if owner == uuid.Nil {
		owner = testAgentID
	}
	return RuleTagInfo{AgentID: owner, Metadata: m.ruleMeta, DestPathTemplate: m.destPathTemplate}, nil
}

func (m *mockIndexerStore) InsertFileTagIfAbsent(_ context.Context, params UpsertFileTagParams) (bool, error) {
	if m.insertTagErr != nil {
		return false, m.insertTagErr
	}
	m.insertedTags = append(m.insertedTags, params)
	return m.insertReturns, nil
}

func (m *mockIndexerStore) GetTagKeyByName(_ context.Context, _ uuid.UUID, key string) (TagKeyInfo, bool, error) {
	if m.tagKeyErr != nil {
		return TagKeyInfo{}, false, m.tagKeyErr
	}
	info, ok := m.tagKeys[key]
	return info, ok, nil
}

func (m *mockIndexerStore) TagValueExists(_ context.Context, _ uuid.UUID, value string) (bool, error) {
	if m.tagValueErr != nil {
		return false, m.tagValueErr
	}
	return m.existingValues[value], nil
}

func (m *mockIndexerStore) FindSimilarTagValue(_ context.Context, _ uuid.UUID, _ string) (string, error) {
	return m.similarValue, m.similarErr
}

func (m *mockIndexerStore) UpsertPendingTagValue(_ context.Context, params UpsertPendingTagValueParams) error {
	if m.pendingErr != nil {
		return m.pendingErr
	}
	m.pendingUpserts = append(m.pendingUpserts, params)
	return nil
}

func (m *mockIndexerStore) GetFileTypeIDByName(_ context.Context, _ uuid.UUID, name string) (uuid.UUID, error) {
	if m.fileTypeErr != nil {
		return uuid.Nil, m.fileTypeErr
	}
	return m.fileTypeByName[name], nil
}

func (m *mockIndexerStore) UpsertFileTag(_ context.Context, params UpsertFileTagParams) error {
	if m.upsertTagErr != nil {
		return m.upsertTagErr
	}
	m.upsertedTags = append(m.upsertedTags, params)
	return nil
}

// ── Legacy DBTX mock (kept for tests that use it directly) ──────────────────

type errDBTX struct{ err error }

func (e *errDBTX) ExecContext(_ context.Context, _ string, _ ...interface{}) (sql.Result, error) {
	return nil, e.err
}
func (e *errDBTX) PrepareContext(_ context.Context, _ string) (*sql.Stmt, error) {
	return nil, e.err
}
func (e *errDBTX) QueryContext(_ context.Context, _ string, _ ...interface{}) (*sql.Rows, error) {
	return nil, e.err
}
func (e *errDBTX) QueryRowContext(_ context.Context, _ string, _ ...interface{}) *sql.Row {
	return nil
}

// ── Helpers ──────────────────────────────────────────────────────────────────

func newTestLogger() *zap.Logger {
	l, _ := zap.NewDevelopment()
	return l
}

func newBucket() *db.Bucket {
	return &db.Bucket{
		ID:    uuid.New(),
		OrgID: uuid.New(),
		Name:  "test-bucket",
	}
}

func newFileEntry(bucketID uuid.UUID, storagePath string) *db.FileEntry {
	return &db.FileEntry{
		ID:          uuid.New(),
		BucketID:    bucketID,
		StoragePath: storagePath,
		FileName:    fileNameFromPath(storagePath),
		SizeBytes:   1024,
		Status:      db.FileStatusCompleted,
	}
}

func newUploadLog() *db.UploadLog {
	return &db.UploadLog{ID: uuid.New()}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestNewClassifier_NotNil(t *testing.T) {
	c := NewClassifierWithStore(&mockIndexerStore{})
	require.NotNil(t, c)
}

func TestClassifier_DBError_ReturnsError(t *testing.T) {
	c := NewClassifierWithStore(&mockIndexerStore{typeRulesErr: assert.AnError})
	id, err := c.Classify(context.Background(), "uploads/test.log")
	require.Error(t, err)
	assert.Equal(t, uuid.Nil, id)
}

func TestClassifier_NoRules_ReturnsNil(t *testing.T) {
	c := NewClassifierWithStore(&mockIndexerStore{typeRules: nil})
	id, err := c.Classify(context.Background(), "uploads/test.log")
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, id)
}

func TestClassifier_MatchByStoragePath(t *testing.T) {
	typeID := uuid.New()
	rule := &db.FileTypeRule{
		ID:          uuid.New(),
		FileTypeID:  typeID,
		PathPattern: "uploads/*.log",
		Priority:    10,
	}
	c := NewClassifierWithStore(&mockIndexerStore{typeRules: []*db.FileTypeRule{rule}})
	id, err := c.Classify(context.Background(), "uploads/test.log")
	require.NoError(t, err)
	assert.Equal(t, typeID, id)
}

func TestClassifier_MatchByFileName(t *testing.T) {
	typeID := uuid.New()
	rule := &db.FileTypeRule{
		ID:          uuid.New(),
		FileTypeID:  typeID,
		PathPattern: "*.csv",
		Priority:    5,
	}
	c := NewClassifierWithStore(&mockIndexerStore{typeRules: []*db.FileTypeRule{rule}})
	id, err := c.Classify(context.Background(), "/var/data/report.csv")
	require.NoError(t, err)
	assert.Equal(t, typeID, id)
}

func TestClassifier_NoMatch_ReturnsNil(t *testing.T) {
	rule := &db.FileTypeRule{
		ID:          uuid.New(),
		FileTypeID:  uuid.New(),
		PathPattern: "*.xml",
		Priority:    5,
	}
	c := NewClassifierWithStore(&mockIndexerStore{typeRules: []*db.FileTypeRule{rule}})
	id, err := c.Classify(context.Background(), "/var/data/report.csv")
	require.NoError(t, err)
	assert.Equal(t, uuid.Nil, id)
}

func TestNewIndexer(t *testing.T) {
	ix := NewIndexerWithStore(&mockIndexerStore{}, newMockNATS(), newTestLogger())
	require.NotNil(t, ix)
}

func TestHandleUploadResult_Success(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/file.log")
	store := &mockIndexerStore{
		bucket:    bucket,
		fileEntry: fe,
		uploadLog: newUploadLog(),
	}
	nats := newMockNATS()
	ix := NewIndexerWithStore(store, nats, newTestLogger())

	result := &agentv1.UploadResult{
		RuleId:      uuid.New().String(),
		LocalPath:   "/data/file.log",
		StoragePath: "uploads/file.log",
		Bucket:      "test-bucket",
		SizeBytes:   1024,
		Sha256:      "abc123",
		Etag:        "etag1",
		Success:     true,
		UploadedAt:  timestamppb.New(time.Now()),
	}

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, result)
	require.NoError(t, err)

	// Verify NATS event was published.
	events, ok := nats.published["events.file.uploaded"]
	require.True(t, ok)
	require.Len(t, events, 1)
}

func TestHandleUploadResult_FailedUpload(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/bad.log")
	store := &mockIndexerStore{
		bucket:    bucket,
		fileEntry: fe,
		uploadLog: newUploadLog(),
	}
	nats := newMockNATS()
	ix := NewIndexerWithStore(store, nats, newTestLogger())

	result := &agentv1.UploadResult{
		StoragePath:  "uploads/bad.log",
		Bucket:       "test-bucket",
		Success:      false,
		ErrorMessage: "network error",
	}

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, result)
	require.NoError(t, err)

	assert.Zero(t, store.upsertCalls, "failed uploads must not create or overwrite file_entries")
	assert.Equal(t, 1, store.uploadLogCalls)
	assert.False(t, store.lastLog.FileEntryID.Valid)
	assert.Equal(t, "failed", store.lastLog.Status)
	assert.Equal(t, "network error", store.lastLog.ErrorMessage.String)
	// NATS event should NOT be published for failed uploads.
	_, ok := nats.published["events.file.uploaded"]
	assert.False(t, ok)
}

func TestHandleUploadResult_BucketNotFound(t *testing.T) {
	store := &mockIndexerStore{bucketErr: assert.AnError}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	result := &agentv1.UploadResult{
		StoragePath: "uploads/file.log",
		Bucket:      "missing-bucket",
		Success:     true,
	}

	err := ix.HandleUploadResult(context.Background(), testAgentID, uuid.New(), result)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing-bucket")
}

func TestHandleUploadResult_UpsertError(t *testing.T) {
	bucket := newBucket()
	store := &mockIndexerStore{
		bucket:    bucket,
		upsertErr: assert.AnError,
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	result := &agentv1.UploadResult{
		StoragePath: "uploads/file.log",
		Bucket:      "test-bucket",
		Success:     true,
	}

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, result)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upsert file entry")
}

func TestHandleUploadResult_UploadLogError(t *testing.T) {
	// Upload log error is a soft warning, not a hard failure.
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/file.log")
	store := &mockIndexerStore{
		bucket:       bucket,
		fileEntry:    fe,
		uploadLogErr: assert.AnError,
	}
	nats := newMockNATS()
	ix := NewIndexerWithStore(store, nats, newTestLogger())

	result := &agentv1.UploadResult{
		StoragePath: "uploads/file.log",
		Bucket:      "test-bucket",
		Success:     true,
	}

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, result)
	// Should not fail even if upload log creation fails.
	require.Error(t, err)
}

func TestHandleUploadResult_ClassifyError(t *testing.T) {
	// Classify error is a soft warning; indexing continues.
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/file.log")
	store := &mockIndexerStore{
		bucket:       bucket,
		fileEntry:    fe,
		uploadLog:    newUploadLog(),
		typeRulesErr: assert.AnError, // causes classify to fail
	}
	nats := newMockNATS()
	ix := NewIndexerWithStore(store, nats, newTestLogger())

	result := &agentv1.UploadResult{
		StoragePath: "uploads/file.log",
		Bucket:      "test-bucket",
		Success:     true,
	}

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, result)
	require.NoError(t, err)
}

func TestHandleUploadResult_WithRuleID(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/file.log")
	store := &mockIndexerStore{
		bucket:    bucket,
		fileEntry: fe,
		uploadLog: newUploadLog(),
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	ruleID := uuid.New()
	result := &agentv1.UploadResult{
		RuleId:      ruleID.String(),
		StoragePath: "uploads/file.log",
		Bucket:      "test-bucket",
		Success:     true,
	}

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, result)
	require.NoError(t, err)
}

func TestHandleUploadResult_WithFileMtime(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/file.log")
	store := &mockIndexerStore{
		bucket:    bucket,
		fileEntry: fe,
		uploadLog: newUploadLog(),
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	result := &agentv1.UploadResult{
		StoragePath: "uploads/file.log",
		Bucket:      "test-bucket",
		Success:     true,
		FileMtime:   timestamppb.New(time.Now().Add(-24 * time.Hour)),
	}

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, result)
	require.NoError(t, err)
}

func TestPublishFileUploaded(t *testing.T) {
	nats := newMockNATS()
	ix := NewIndexerWithStore(&mockIndexerStore{}, nats, newTestLogger())

	fe := &db.FileEntry{
		ID:          uuid.New(),
		BucketID:    uuid.New(),
		StoragePath: "uploads/test.log",
		FileName:    "test.log",
		SizeBytes:   1024,
	}
	result := &agentv1.UploadResult{
		Sha256: "deadbeef",
	}
	ix.publishFileUploaded(fe, uuid.New(), result)

	events, ok := nats.published["events.file.uploaded"]
	require.True(t, ok)
	require.Len(t, events, 1)
}

func TestFileEntryParams_NullFields(t *testing.T) {
	// Test that nullable fields are constructed correctly.
	p := UpsertFileEntryParams{
		OrgID:       uuid.New(),
		BucketID:    uuid.New(),
		StoragePath: "bucket/path/file.log",
		FileName:    "file.log",
		SizeBytes:   2048,
		Status:      db.FileStatusCompleted,
		UploadedAt:  sql.NullTime{Time: time.Now(), Valid: true},
	}
	assert.Equal(t, "bucket/path/file.log", p.StoragePath)
	assert.Equal(t, db.FileStatusCompleted, p.Status)
}

func TestFileNameFromPath(t *testing.T) {
	assert.Equal(t, "file.log", fileNameFromPath("/var/log/agent/file.log"))
	assert.Equal(t, "data.csv", fileNameFromPath("data.csv"))
	assert.Equal(t, "report.pdf", fileNameFromPath("path/to/report.pdf"))
}

// TestNewIndexer_DBBacked tests the production constructor that wraps db.DBTX.
func TestNewIndexer_DBBacked(t *testing.T) {
	// Use the errDBTX to satisfy the db.DBTX interface for construction.
	// We only verify the constructor doesn't panic and returns non-nil.
	ix := NewIndexer(&errDBTX{err: nil}, newMockNATS(), newTestLogger())
	require.NotNil(t, ix)
}

// TestNewClassifier_DBBacked tests the production constructor.
func TestNewClassifier_DBBacked(t *testing.T) {
	c := NewClassifier(&errDBTX{err: nil})
	require.NotNil(t, c)
}

// ── IndexUpload ───────────────────────────────────────────────────────────────

func newSampleBucket() *db.Bucket {
	return &db.Bucket{ID: uuid.New(), OrgID: uuid.New(), Name: "data-sensor"}
}

func newSampleFileEntry() *db.FileEntry {
	return &db.FileEntry{
		ID:          uuid.New(),
		OrgID:       uuid.New(),
		BucketID:    uuid.New(),
		StoragePath: "uploads/file.csv",
		FileName:    "file.csv",
		SizeBytes:   1024,
		Status:      db.FileStatusCompleted,
	}
}

func TestIndexUpload_Success(t *testing.T) {
	store := &mockIndexerStore{
		bucket:    newSampleBucket(),
		fileEntry: newSampleFileEntry(),
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())
	err := ix.IndexUpload(context.Background(), "data-sensor", "uploads/file.csv", 1024, "abc123", time.Now().UTC(), "seq")
	require.NoError(t, err)
}

func TestIndexUpload_BucketNotFound(t *testing.T) {
	store := &mockIndexerStore{bucketErr: assert.AnError}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())
	err := ix.IndexUpload(context.Background(), "missing-bucket", "key.csv", 0, "", time.Now().UTC(), "seq")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "get bucket")
}

func TestIndexUpload_UpsertError(t *testing.T) {
	store := &mockIndexerStore{
		bucket:    newSampleBucket(),
		upsertErr: assert.AnError,
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())
	err := ix.IndexUpload(context.Background(), "data-sensor", "key.csv", 100, "", time.Now().UTC(), "seq")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "upsert file entry")
}

// ── IndexDeletion (CC-1) ──────────────────────────────────────────────────────

func TestIndexDeletion_Found_MarksDeletedAndPublishes(t *testing.T) {
	bucketID := uuid.New()
	entryID := uuid.New()
	store := &mockIndexerStore{
		bucket:       &db.Bucket{ID: bucketID, Name: "data-sensor"},
		deletedEntry: &db.FileEntry{ID: entryID, BucketID: bucketID, StoragePath: "uploads/gone.csv", FileName: "gone.csv"},
	}
	nats := newMockNATS()
	ix := NewIndexerWithStore(store, nats, newTestLogger())

	err := ix.IndexDeletion(context.Background(), "data-sensor", "uploads/gone.csv", time.Now().UTC(), "seq")
	require.NoError(t, err)
	published := nats.published["events.file.deleted"]
	require.Len(t, published, 1, "events.file.deleted must be published")
	assert.Contains(t, string(published[0]), entryID.String())
}

func TestIndexDeletion_NotFound_NoOp(t *testing.T) {
	store := &mockIndexerStore{
		bucket:    &db.Bucket{ID: uuid.New(), Name: "data-sensor"},
		deleteErr: sql.ErrNoRows,
	}
	nats := newMockNATS()
	ix := NewIndexerWithStore(store, nats, newTestLogger())

	err := ix.IndexDeletion(context.Background(), "data-sensor", "uploads/never-indexed.csv", time.Now().UTC(), "seq")
	require.NoError(t, err, "deleting an unindexed object is a no-op")
	assert.Empty(t, nats.published["events.file.deleted"], "no event for a no-op delete")
}

func TestIndexDeletion_BucketError(t *testing.T) {
	store := &mockIndexerStore{bucketErr: assert.AnError}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())
	err := ix.IndexDeletion(context.Background(), "data-sensor", "k", time.Now().UTC(), "seq")
	require.Error(t, err)
}

// ── MT-2: tagging engine ───────────────────────────────────────────────────────

// newTagResult builds a successful UploadResult carrying a rule id.
func newTagResult(ruleID uuid.UUID, storagePath string) *agentv1.UploadResult {
	return &agentv1.UploadResult{
		RuleId:      ruleID.String(),
		StoragePath: storagePath,
		Bucket:      "test-bucket",
		SizeBytes:   1,
		Success:     true,
		UploadedAt:  timestamppb.New(time.Now()),
	}
}

func TestHandleUploadResult_AppliesStaticTags(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/p.csv")
	store := &mockIndexerStore{
		bucket:    bucket,
		fileEntry: fe,
		uploadLog: newUploadLog(),
		ruleMeta:  json.RawMessage(`{"static_tags":{"vendor":"omron","site":"tokyo"}}`),
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "uploads/p.csv"))
	require.NoError(t, err)

	require.Len(t, store.upsertedTags, 2)
	got := map[string]UpsertFileTagParams{}
	for _, tag := range store.upsertedTags {
		got[tag.Key] = tag
		assert.Equal(t, fe.ID, tag.FileEntryID)
		assert.Equal(t, "rule_static", tag.Source)
	}
	assert.Equal(t, "omron", got["vendor"].Value)
	assert.Equal(t, "tokyo", got["site"].Value)
}

func TestHandleUploadResult_NoRuleMetadata_NoTags(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/p.csv")
	store := &mockIndexerStore{
		bucket:    bucket,
		fileEntry: fe,
		uploadLog: newUploadLog(),
		// ruleMeta nil → GetRuleMetadata returns empty → no tags.
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "uploads/p.csv"))
	require.NoError(t, err)
	assert.Empty(t, store.upsertedTags)
}

func TestHandleUploadResult_MalformedMetadata_DegradesGracefully(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/p.csv")
	store := &mockIndexerStore{
		bucket:    bucket,
		fileEntry: fe,
		uploadLog: newUploadLog(),
		ruleMeta:  json.RawMessage(`{not valid json`),
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "uploads/p.csv"))
	require.NoError(t, err)
	assert.Empty(t, store.upsertedTags)
}

func TestHandleUploadResult_DeclaredFileTypeOverridesGlob(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/p.csv")
	declaredID := uuid.New()
	globID := uuid.New()
	store := &mockIndexerStore{
		bucket:         bucket,
		fileEntry:      fe,
		uploadLog:      newUploadLog(),
		ruleMeta:       json.RawMessage(`{"file_type":"pressure"}`),
		fileTypeByName: map[string]uuid.UUID{"pressure": declaredID},
		// A glob rule that would otherwise match, to prove the declaration wins.
		typeRules: []*db.FileTypeRule{{ID: uuid.New(), FileTypeID: globID, PathPattern: "*.csv"}},
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "uploads/p.csv"))
	require.NoError(t, err)
	require.True(t, store.lastUpsert.FileTypeID.Valid)
	assert.Equal(t, declaredID, store.lastUpsert.FileTypeID.UUID)
}

func TestHandleUploadResult_DeclaredFileTypeNotFound_FallsBackToGlob(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/p.csv")
	globID := uuid.New()
	store := &mockIndexerStore{
		bucket:         bucket,
		fileEntry:      fe,
		uploadLog:      newUploadLog(),
		ruleMeta:       json.RawMessage(`{"file_type":"unknown"}`),
		fileTypeByName: map[string]uuid.UUID{}, // "unknown" resolves to uuid.Nil
		typeRules:      []*db.FileTypeRule{{ID: uuid.New(), FileTypeID: globID, PathPattern: "*.csv"}},
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "uploads/p.csv"))
	require.NoError(t, err)
	require.True(t, store.lastUpsert.FileTypeID.Valid)
	assert.Equal(t, globID, store.lastUpsert.FileTypeID.UUID)
}

func TestHandleUploadResult_TagUpsertError_DoesNotFailIndexing(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/p.csv")
	store := &mockIndexerStore{
		bucket:       bucket,
		fileEntry:    fe,
		uploadLog:    newUploadLog(),
		ruleMeta:     json.RawMessage(`{"static_tags":{"vendor":"omron"}}`),
		upsertTagErr: assert.AnError,
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "uploads/p.csv"))
	require.NoError(t, err)
}

func TestHandleUploadResult_SkipsOverlongStaticTag(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/p.csv")
	longValue := strings.Repeat("x", 129) // exceeds VARCHAR(128)
	store := &mockIndexerStore{
		bucket:    bucket,
		fileEntry: fe,
		uploadLog: newUploadLog(),
		ruleMeta:  json.RawMessage(`{"static_tags":{"vendor":"omron","note":"` + longValue + `"}}`),
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "uploads/p.csv"))
	require.NoError(t, err)

	// The overlong "note" tag is skipped; the valid "vendor" tag still lands.
	require.Len(t, store.upsertedTags, 1)
	assert.Equal(t, "vendor", store.upsertedTags[0].Key)
}

// ── MT-3: path-variable extraction + pending-value queue ────────────────────────

func pathVarStore(bucket *db.Bucket, fe *db.FileEntry, meta, destTemplate string) *mockIndexerStore {
	return &mockIndexerStore{
		bucket:           bucket,
		fileEntry:        fe,
		uploadLog:        newUploadLog(),
		ruleMeta:         json.RawMessage(meta),
		destPathTemplate: destTemplate,
		insertReturns:    true, // default: treat as a fresh insert
	}
}

func TestHandleUploadResult_ExtractsPathVarTag(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)

	require.Len(t, store.insertedTags, 1)
	assert.Equal(t, "site", store.insertedTags[0].Key)
	assert.Equal(t, "tokyo", store.insertedTags[0].Value)
	assert.Equal(t, "path_var", store.insertedTags[0].Source)
}

// Regression for IC-BUG-16: templates written with a leading "/" — which is
// what the Web UI creates by default — never matched the object key the agent
// writes, so path-variable tagging silently did nothing for those rules.
func TestHandleUploadResult_PathVar_LeadingSlashTemplate(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "/data/{site}/{filename}")
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID,
		newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)

	require.Len(t, store.insertedTags, 1, "leading-slash template must still extract path vars")
	assert.Equal(t, "site", store.insertedTags[0].Key)
	assert.Equal(t, "tokyo", store.insertedTags[0].Value)
}

func TestHandleUploadResult_PathVar_TemplateMismatch_NoTag(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "totally/different/path.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "totally/different/path.csv"))
	require.NoError(t, err)
	assert.Empty(t, store.insertedTags)
}

func TestHandleUploadResult_PathVar_UnregisteredControlledValue_Queued(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	keyID := uuid.New()
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	store.tagKeys = map[string]TagKeyInfo{"site": {ID: keyID, ValueControlled: true, AllowPathVar: true}}
	store.existingValues = map[string]bool{} // tokyo not registered
	store.similarValue = "Tokyo"             // case drift suggestion

	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())
	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)

	require.Len(t, store.pendingUpserts, 1)
	p := store.pendingUpserts[0]
	assert.Equal(t, keyID, p.TagKeyID)
	assert.Equal(t, "tokyo", p.ExtractedValue)
	assert.True(t, p.SuggestedValue.Valid)
	assert.Equal(t, "Tokyo", p.SuggestedValue.String)
}

func TestHandleUploadResult_PathVar_RegisteredValue_NotQueued(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	store.tagKeys = map[string]TagKeyInfo{"site": {ID: uuid.New(), ValueControlled: true, AllowPathVar: true}}
	store.existingValues = map[string]bool{"tokyo": true} // already approved

	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())
	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	assert.Empty(t, store.pendingUpserts)
}

func TestHandleUploadResult_PathVar_UncontrolledKey_NotQueued(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	// "site" not registered as a controlled key at all.
	store.tagKeys = map[string]TagKeyInfo{}

	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())
	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	require.Len(t, store.insertedTags, 1) // tag still recorded
	assert.Empty(t, store.pendingUpserts) // but not queued
}

func TestHandleUploadResult_PathVar_AllowPathVarFalse_Skipped(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	// Key is registered but explicitly forbids path-variable mapping.
	store.tagKeys = map[string]TagKeyInfo{"site": {ID: uuid.New(), ValueControlled: true, AllowPathVar: false}}
	store.existingValues = map[string]bool{}

	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())
	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	assert.Empty(t, store.insertedTags)   // no tag written
	assert.Empty(t, store.pendingUpserts) // no queue side effect
}

func TestHandleUploadResult_PathVar_NotInserted_NotQueued(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	store.insertReturns = false // key already set (e.g. static tag or re-index)
	store.tagKeys = map[string]TagKeyInfo{"site": {ID: uuid.New(), ValueControlled: true, AllowPathVar: true}}
	store.existingValues = map[string]bool{}

	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())
	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	assert.Empty(t, store.pendingUpserts) // idempotent: no re-queue on non-insert
}

func TestTemplateVarName(t *testing.T) {
	assert.Equal(t, "site", templateVarName("{site}"))
	assert.Equal(t, "ts", templateVarName("{ts:yyyy}"))
	assert.Equal(t, "", templateVarName("site"))
	assert.Equal(t, "", templateVarName(""))
	assert.Equal(t, "", templateVarName("{}"))
	assert.Equal(t, "", templateVarName("{a}{b}")) // multi-placeholder rejected
	assert.Equal(t, "", templateVarName("{a}x{b}"))
}
