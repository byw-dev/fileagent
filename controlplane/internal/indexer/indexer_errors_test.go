package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// ── loadRuleMetadata ──────────────────────────────────────────────────────────

// TestLoadRuleMetadata pins the degrade-gracefully contract of loadRuleMetadata:
// a missing rule is silent, a DB failure logs a warning, and only a healthy rule
// yields its parsed declaration.
func TestLoadRuleMetadata(t *testing.T) {
	t.Run("rule exists", func(t *testing.T) {
		store := &mockIndexerStore{
			ruleMeta:         json.RawMessage(`{"file_type":"pressure","static_tags":{"vendor":"omron"}}`),
			destPathTemplate: "data/{site}/{filename}",
		}
		ix := NewIndexerWithStore(store, newMockNATS(), zap.NewNop())

		meta, dest := ix.loadRuleMetadata(context.Background(), uuid.New(), uuid.New())
		assert.Equal(t, "pressure", meta.FileType)
		assert.Equal(t, "omron", meta.StaticTags["vendor"])
		assert.Equal(t, "data/{site}/{filename}", dest)
	})
	t.Run("rule missing is silent", func(t *testing.T) {
		store := &mockIndexerStore{ruleMetaErr: sql.ErrNoRows}
		core, logs := observer.New(zap.InfoLevel)
		ix := NewIndexerWithStore(store, newMockNATS(), zap.New(core))

		meta, dest := ix.loadRuleMetadata(context.Background(), uuid.New(), uuid.New())
		assert.Equal(t, ruleMetadata{}, meta)
		assert.Empty(t, dest)
		assert.Zero(t, logs.FilterLevelExact(zap.WarnLevel).Len(),
			"an absent rule is the steady state after rule deletion, not a warning")
	})
	t.Run("db error logs and degrades", func(t *testing.T) {
		store := &mockIndexerStore{ruleMetaErr: context.DeadlineExceeded}
		core, logs := observer.New(zap.InfoLevel)
		ix := NewIndexerWithStore(store, newMockNATS(), zap.New(core))

		meta, dest := ix.loadRuleMetadata(context.Background(), uuid.New(), uuid.New())
		assert.Equal(t, ruleMetadata{}, meta)
		assert.Empty(t, dest)
		assert.NotZero(t, logs.FilterMessage("indexer: load rule metadata").Len(),
			"a real DB failure must be visible in logs, unlike an absent rule")
	})
	t.Run("malformed metadata keeps dest template", func(t *testing.T) {
		store := &mockIndexerStore{
			ruleMeta:         json.RawMessage(`{not valid json`),
			destPathTemplate: "data/{site}/{filename}",
		}
		ix := NewIndexerWithStore(store, newMockNATS(), zap.NewNop())

		meta, dest := ix.loadRuleMetadata(context.Background(), uuid.New(), uuid.New())
		assert.Equal(t, ruleMetadata{}, meta)
		assert.Equal(t, "data/{site}/{filename}", dest)
	})
}

// ── applyPathVarTags error/skip branches ──────────────────────────────────────

// An unparsable dest_path_template must skip extraction without failing indexing.
func TestHandleUploadResult_PathVar_TemplateParseError_SkipsExtraction(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "{site") // unbalanced '{'
	core, logs := observer.New(zap.InfoLevel)
	ix := NewIndexerWithStore(store, newMockNATS(), zap.New(core))

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	assert.Equal(t, 1, store.upsertCalls, "the file must still be indexed")
	assert.Empty(t, store.insertedTags)
	assert.NotZero(t, logs.FilterMessage("indexer: parse dest_path_template").Len())
}

// A path_tag_map entry whose key or {var} reference is malformed is ignored.
func TestHandleUploadResult_PathVar_EmptyKeyOrRef_Skipped(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"":"{site}","region":"plain-text"}}`, "data/{site}/{filename}")
	ix := NewIndexerWithStore(store, newMockNATS(), zap.NewNop())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	assert.Empty(t, store.insertedTags, "an empty key and a non-{var} reference must both be skipped")
}

// A path_tag_map variable that the template never extracted must not produce a tag.
func TestHandleUploadResult_PathVar_VarNotInPath_Skipped(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"region":"{region}"}}`, "data/{site}/{filename}")
	ix := NewIndexerWithStore(store, newMockNATS(), zap.NewNop())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	assert.Empty(t, store.insertedTags)
}

// An extracted value exceeding the VARCHAR(128) limit must be skipped, mirroring
// the static-tag limit so a long path segment cannot spam DB errors.
func TestHandleUploadResult_PathVar_OverlongValue_Skipped(t *testing.T) {
	bucket := newBucket()
	longSeg := strings.Repeat("x", 129)
	fe := newFileEntry(bucket.ID, "data/"+longSeg+"/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	ix := NewIndexerWithStore(store, newMockNATS(), zap.NewNop())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/"+longSeg+"/p.csv"))
	require.NoError(t, err)
	assert.Empty(t, store.insertedTags)
}

// A tag-key vocabulary lookup failure must skip that tag, not fail the upload.
func TestHandleUploadResult_PathVar_TagKeyLookupError_SkipsTag(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	store.tagKeyErr = context.DeadlineExceeded
	ix := NewIndexerWithStore(store, newMockNATS(), zap.NewNop())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	assert.Empty(t, store.insertedTags)
	assert.Empty(t, store.pendingUpserts)
}

// A failure writing the path_var tag itself must skip governance queueing for it.
func TestHandleUploadResult_PathVar_InsertTagError_SkipsQueue(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	store.insertTagErr = assert.AnError
	store.tagKeys = map[string]TagKeyInfo{"site": {ID: uuid.New(), ValueControlled: true, AllowPathVar: true}}
	ix := NewIndexerWithStore(store, newMockNATS(), zap.NewNop())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	assert.Empty(t, store.insertedTags, "mock only records successful inserts")
	assert.Empty(t, store.pendingUpserts, "a tag that failed to land must not be queued for review")
}

// ── queuePendingIfUnregistered error branches ────────────────────────────────

// A failure checking whether the value is registered must not queue a garbage
// pending row: the queue is only for values known to be unregistered.
func TestHandleUploadResult_PendingQueue_TagValueCheckError_SkipsQueue(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	store.tagKeys = map[string]TagKeyInfo{"site": {ID: uuid.New(), ValueControlled: true, AllowPathVar: true}}
	store.tagValueErr = context.DeadlineExceeded
	ix := NewIndexerWithStore(store, newMockNATS(), zap.NewNop())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	require.Len(t, store.insertedTags, 1, "the tag itself is still recorded")
	assert.Empty(t, store.pendingUpserts)
}

// A failed similarity lookup must not block queueing — the value goes in without
// a suggestion instead of being silently dropped from admin review.
func TestHandleUploadResult_PendingQueue_SimilarLookupError_QueuesWithoutSuggestion(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	store.tagKeys = map[string]TagKeyInfo{"site": {ID: uuid.New(), ValueControlled: true, AllowPathVar: true}}
	store.existingValues = map[string]bool{}
	store.similarErr = context.DeadlineExceeded
	ix := NewIndexerWithStore(store, newMockNATS(), zap.NewNop())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	require.Len(t, store.pendingUpserts, 1)
	assert.False(t, store.pendingUpserts[0].SuggestedValue.Valid)
	assert.Equal(t, "tokyo", store.pendingUpserts[0].ExtractedValue)
}

// A failure writing the pending row is logged but must not fail the upload.
func TestHandleUploadResult_PendingQueue_UpsertError_DoesNotFailIndexing(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "data/tokyo/p.csv")
	store := pathVarStore(bucket, fe,
		`{"path_tag_map":{"site":"{site}"}}`, "data/{site}/{filename}")
	store.tagKeys = map[string]TagKeyInfo{"site": {ID: uuid.New(), ValueControlled: true, AllowPathVar: true}}
	store.existingValues = map[string]bool{}
	store.pendingErr = assert.AnError
	ix := NewIndexerWithStore(store, newMockNATS(), zap.NewNop())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "data/tokyo/p.csv"))
	require.NoError(t, err)
	assert.Equal(t, 1, store.upsertCalls)
}

// ── static tags / file-type resolution ───────────────────────────────────────

// A static tag with an empty value must not be written as a tag row.
func TestHandleUploadResult_StaticTagEmptyValue_Skipped(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/p.csv")
	store := &mockIndexerStore{
		bucket:    bucket,
		fileEntry: fe,
		uploadLog: newUploadLog(),
		ruleMeta:  json.RawMessage(`{"static_tags":{"vendor":"","site":"tokyo"}}`),
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "uploads/p.csv"))
	require.NoError(t, err)
	require.Len(t, store.upsertedTags, 1)
	assert.Equal(t, "site", store.upsertedTags[0].Key)
}

// A DB error resolving the declared file_type must degrade to glob matching,
// not fail the upload — same fallback as an unresolvable name.
func TestHandleUploadResult_DeclaredFileTypeResolveError_FallsBackToGlob(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/p.csv")
	globID := uuid.New()
	store := &mockIndexerStore{
		bucket:      bucket,
		fileEntry:   fe,
		uploadLog:   newUploadLog(),
		ruleMeta:    json.RawMessage(`{"file_type":"pressure"}`),
		fileTypeErr: context.DeadlineExceeded, // hard error, not just "not found"
		typeRules:   []*db.FileTypeRule{{ID: uuid.New(), FileTypeID: globID, PathPattern: "*.csv"}},
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "uploads/p.csv"))
	require.NoError(t, err)
	require.True(t, store.lastUpsert.FileTypeID.Valid)
	assert.Equal(t, globID, store.lastUpsert.FileTypeID.UUID)
}

// ── NATS publish failures ─────────────────────────────────────────────────────

// A NATS publish failure on the uploaded event is logged but must not fail the
// upload report handling.
func TestHandleUploadResult_NATSPublishError_DoesNotFailIndexing(t *testing.T) {
	bucket := newBucket()
	fe := newFileEntry(bucket.ID, "uploads/file.log")
	store := &mockIndexerStore{bucket: bucket, fileEntry: fe, uploadLog: newUploadLog()}
	nats := newMockNATS()
	nats.err = assert.AnError
	core, logs := observer.New(zap.InfoLevel)
	ix := NewIndexerWithStore(store, nats, zap.New(core))

	err := ix.HandleUploadResult(context.Background(), testAgentID, bucket.OrgID, newTagResult(uuid.New(), "uploads/file.log"))
	require.NoError(t, err)
	assert.NotZero(t, logs.FilterMessage("indexer: publish file uploaded event").Len(),
		"the publish failure must be visible in logs rather than swallowed")
}

// A NATS publish failure on the deleted event is logged but not returned.
func TestIndexDeletion_PublishError_LoggedOnly(t *testing.T) {
	bucketID := uuid.New()
	store := &mockIndexerStore{
		bucket:       &db.Bucket{ID: bucketID, Name: "data-sensor"},
		deletedEntry: &db.FileEntry{ID: uuid.New(), BucketID: bucketID, StoragePath: "uploads/gone.csv", FileName: "gone.csv"},
	}
	nats := newMockNATS()
	nats.err = assert.AnError
	core, logs := observer.New(zap.InfoLevel)
	ix := NewIndexerWithStore(store, nats, zap.New(core))

	err := ix.IndexDeletion(context.Background(), "data-sensor", "uploads/gone.csv", time.Now().UTC(), "seq")
	require.NoError(t, err)
	assert.NotZero(t, logs.FilterMessage("indexer: publish file deleted event").Len())
}

// A nil NATS publisher (production misconfiguration) must not panic on deletion.
func TestIndexDeletion_NilPublisher_NoPanic(t *testing.T) {
	bucketID := uuid.New()
	store := &mockIndexerStore{
		bucket:       &db.Bucket{ID: bucketID, Name: "data-sensor"},
		deletedEntry: &db.FileEntry{ID: uuid.New(), BucketID: bucketID, StoragePath: "uploads/gone.csv", FileName: "gone.csv"},
	}
	ix := NewIndexerWithStore(store, nil, zap.NewNop())

	require.NotPanics(t, func() {
		err := ix.IndexDeletion(context.Background(), "data-sensor", "uploads/gone.csv", time.Now().UTC(), "seq")
		require.NoError(t, err)
	})
}

// ── IndexUpload / IndexDeletion error branches ────────────────────────────────

// A classifier failure during a MinIO-event index must leave the entry untyped
// rather than dropping the event.
func TestIndexUpload_ClassifyError_StillIndexes(t *testing.T) {
	store := &mockIndexerStore{
		bucket:       newSampleBucket(),
		fileEntry:    newSampleFileEntry(),
		typeRulesErr: context.DeadlineExceeded,
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.IndexUpload(context.Background(), "data-sensor", "uploads/file.csv", 1024, "abc123", time.Now().UTC(), "seq")
	require.NoError(t, err)
	assert.False(t, store.lastUpsert.FileTypeID.Valid, "classify failure must degrade to no file type")
}

// A hard DB error marking the entry deleted must be returned, not swallowed
// (unlike the ErrNoRows no-op case).
func TestIndexDeletion_MarkError_ReturnsError(t *testing.T) {
	store := &mockIndexerStore{
		bucket:    &db.Bucket{ID: uuid.New(), Name: "data-sensor"},
		deleteErr: context.DeadlineExceeded,
	}
	ix := NewIndexerWithStore(store, newMockNATS(), newTestLogger())

	err := ix.IndexDeletion(context.Background(), "data-sensor", "uploads/gone.csv", time.Now().UTC(), "seq")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mark file entry deleted")
}

// ── queries.go: ErrNoRows must stay the only soft error ──────────────────────

// TestGetFileTypeIDByName_DBError pins that a non-ErrNoRows failure is returned
// to the caller instead of masquerading as "not found" (uuid.Nil).
func TestGetFileTypeIDByName_DBError(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("SELECT id FROM file_types").WillReturnError(assert.AnError)

	_, err := GetFileTypeIDByName(context.Background(), mockDB, uuid.New(), "pressure")
	require.Error(t, err)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestInsertFileTagIfAbsent_DBError pins that a non-ErrNoRows failure is
// returned instead of being treated as an ON CONFLICT no-op.
func TestInsertFileTagIfAbsent_DBError(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("INSERT INTO file_tags").WillReturnError(assert.AnError)

	inserted, err := InsertFileTagIfAbsent(context.Background(), mockDB, UpsertFileTagParams{
		FileEntryID: uuid.New(), Key: "site", Value: "tokyo", Source: "path_var"})
	require.Error(t, err)
	assert.False(t, inserted)
	require.NoError(t, mock.ExpectationsWereMet())
}

// TestGetTagKeyByName_DBError pins that a non-ErrNoRows failure is returned
// instead of being treated as an unregistered key.
func TestGetTagKeyByName_DBError(t *testing.T) {
	mockDB, mock := newMockDB(t)
	mock.ExpectQuery("SELECT id, value_controlled, allow_path_var FROM tag_keys").WillReturnError(assert.AnError)

	_, found, err := GetTagKeyByName(context.Background(), mockDB, uuid.New(), "site")
	require.Error(t, err)
	assert.False(t, found)
	require.NoError(t, mock.ExpectationsWereMet())
}
