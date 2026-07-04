package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// NATSPublisher publishes messages to NATS subjects.
type NATSPublisher interface {
	Publish(subject string, data []byte) error
}

// defaultOrgID is the single-org UUID (Phase 1). MinIO webhook events carry no
// org, so the webhook indexing path resolves buckets/entries under this org.
// (Mirrors bootstrap.DefaultOrgID / agent.defaultOrgID; single-org MVP.)
var defaultOrgID = uuid.MustParse("00000000-0000-0000-0000-000000000001")

// IndexerStore is the minimal database interface used by the Indexer. Using an
// interface (rather than db.DBTX directly) makes the Indexer unit-testable
// without a real database.
type IndexerStore interface {
	GetBucketByName(ctx context.Context, orgID uuid.UUID, name string) (*db.Bucket, error)
	UpsertFileEntry(ctx context.Context, params UpsertFileEntryParams) (*db.FileEntry, error)
	MarkFileEntryDeleted(ctx context.Context, bucketID uuid.UUID, storagePath string) (*db.FileEntry, error)
	CreateUploadLog(ctx context.Context, params CreateUploadLogParams) (*db.UploadLog, error)
	ListFileTypeRules(ctx context.Context) ([]*db.FileTypeRule, error)
}

// dbtxIndexerStore adapts db.DBTX to IndexerStore.
type dbtxIndexerStore struct {
	dbtx db.DBTX
}

// GetBucketByName delegates to the package-level function.
func (d *dbtxIndexerStore) GetBucketByName(ctx context.Context, orgID uuid.UUID, name string) (*db.Bucket, error) {
	return GetBucketByName(ctx, d.dbtx, orgID, name)
}

// UpsertFileEntry delegates to the package-level function.
func (d *dbtxIndexerStore) UpsertFileEntry(ctx context.Context, params UpsertFileEntryParams) (*db.FileEntry, error) {
	return UpsertFileEntry(ctx, d.dbtx, params)
}

// MarkFileEntryDeleted delegates to the package-level function.
func (d *dbtxIndexerStore) MarkFileEntryDeleted(ctx context.Context, bucketID uuid.UUID, storagePath string) (*db.FileEntry, error) {
	return MarkFileEntryDeleted(ctx, d.dbtx, bucketID, storagePath)
}

// CreateUploadLog delegates to the package-level function.
func (d *dbtxIndexerStore) CreateUploadLog(ctx context.Context, params CreateUploadLogParams) (*db.UploadLog, error) {
	return CreateUploadLog(ctx, d.dbtx, params)
}

// ListFileTypeRules delegates to the package-level function.
func (d *dbtxIndexerStore) ListFileTypeRules(ctx context.Context) ([]*db.FileTypeRule, error) {
	return ListFileTypeRules(ctx, d.dbtx)
}

// Indexer processes upload results from agents and maintains the file index.
type Indexer struct {
	store      IndexerStore
	nats       NATSPublisher
	classifier *Classifier
	logger     *zap.Logger
}

// NewIndexer creates a new Indexer backed by a db.DBTX. This is the primary
// constructor used in production; tests should use NewIndexerWithStore.
func NewIndexer(dbtx db.DBTX, nats NATSPublisher, logger *zap.Logger) *Indexer {
	store := &dbtxIndexerStore{dbtx: dbtx}
	return &Indexer{
		store:      store,
		nats:       nats,
		classifier: NewClassifierWithStore(store),
		logger:     logger,
	}
}

// NewIndexerWithStore creates an Indexer using an explicit IndexerStore. This
// constructor is intended for unit tests where the DB layer is mocked.
func NewIndexerWithStore(store IndexerStore, nats NATSPublisher, logger *zap.Logger) *Indexer {
	return &Indexer{
		store:      store,
		nats:       nats,
		classifier: NewClassifierWithStore(store),
		logger:     logger,
	}
}

// HandleUploadResult processes an UploadResult from an agent, updating the
// file index and emitting a NATS event.
func (ix *Indexer) HandleUploadResult(ctx context.Context, agentID uuid.UUID, orgID uuid.UUID, result *agentv1.UploadResult) error {
	// Look up the bucket.
	bucket, err := ix.store.GetBucketByName(ctx, orgID, result.GetBucket())
	if err != nil {
		return fmt.Errorf("indexer: get bucket %q: %w", result.GetBucket(), err)
	}

	// Classify file type.
	fileTypeID, err := ix.classifier.Classify(ctx, result.GetStoragePath())
	if err != nil {
		ix.logger.Warn("indexer: classify file failed", zap.Error(err))
		fileTypeID = uuid.Nil
	}

	// Parse rule ID if provided.
	var ruleID uuid.NullUUID
	if ruleStr := result.GetRuleId(); ruleStr != "" {
		if id, err := uuid.Parse(ruleStr); err == nil {
			ruleID = uuid.NullUUID{UUID: id, Valid: true}
		}
	}

	// Determine file status.
	fileStatus := db.FileStatusCompleted
	if !result.GetSuccess() {
		fileStatus = db.FileStatusFailed
	}

	// Build timestamps.
	var fileMtime sql.NullTime
	if ts := result.GetFileMtime(); ts != nil && ts.IsValid() {
		fileMtime = sql.NullTime{Time: ts.AsTime(), Valid: true}
	}
	var uploadedAt sql.NullTime
	if ts := result.GetUploadedAt(); ts != nil && ts.IsValid() {
		uploadedAt = sql.NullTime{Time: ts.AsTime(), Valid: true}
	} else {
		uploadedAt = sql.NullTime{Time: time.Now().UTC(), Valid: true}
	}

	// Upsert file entry.
	fileEntry, err := ix.store.UpsertFileEntry(ctx, UpsertFileEntryParams{
		OrgID:        orgID,
		FileTypeID:   uuid.NullUUID{UUID: fileTypeID, Valid: fileTypeID != uuid.Nil},
		AgentID:      uuid.NullUUID{UUID: agentID, Valid: true},
		RuleID:       ruleID,
		BucketID:     bucket.ID,
		StoragePath:  result.GetStoragePath(),
		OriginalPath: sql.NullString{String: result.GetLocalPath(), Valid: result.GetLocalPath() != ""},
		FileName:     fileNameFromPath(result.GetStoragePath()),
		SizeBytes:    result.GetSizeBytes(),
		Sha256:       sql.NullString{String: result.GetSha256(), Valid: result.GetSha256() != ""},
		Etag:         sql.NullString{String: result.GetEtag(), Valid: result.GetEtag() != ""},
		FileMtime:    fileMtime,
		Status:       fileStatus,
		UploadedAt:   uploadedAt,
	})
	if err != nil {
		return fmt.Errorf("indexer: upsert file entry: %w", err)
	}

	// Create upload log.
	logStatus := "completed"
	var errMsg sql.NullString
	if !result.GetSuccess() {
		logStatus = "failed"
		errMsg = sql.NullString{String: result.GetErrorMessage(), Valid: result.GetErrorMessage() != ""}
	}

	_, err = ix.store.CreateUploadLog(ctx, CreateUploadLogParams{
		OrgID:            orgID,
		AgentID:          agentID,
		FileEntryID:      uuid.NullUUID{UUID: fileEntry.ID, Valid: true},
		RuleID:           ruleID,
		OriginalPath:     result.GetLocalPath(),
		StoragePath:      result.GetStoragePath(),
		SizeBytes:        result.GetSizeBytes(),
		BytesTransferred: result.GetSizeBytes(),
		Status:           logStatus,
		ErrorMessage:     errMsg,
		RetryCount:       result.GetRetryCount(),
		StartedAt:        time.Now().UTC(),
		FinishedAt:       sql.NullTime{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		ix.logger.Warn("indexer: create upload log failed", zap.Error(err))
	}

	// Publish NATS event.
	if ix.nats != nil && result.GetSuccess() {
		ix.publishFileUploaded(fileEntry, agentID, result)
	}

	ix.logger.Info("file indexed",
		zap.String("file_entry_id", fileEntry.ID.String()),
		zap.String("storage_path", result.GetStoragePath()),
		zap.String("status", string(fileStatus)),
	)
	return nil
}

func (ix *Indexer) publishFileUploaded(fe *db.FileEntry, agentID uuid.UUID, result *agentv1.UploadResult) {
	payload := map[string]interface{}{
		"file_entry_id": fe.ID.String(),
		"agent_id":      agentID.String(),
		"bucket_id":     fe.BucketID.String(),
		"storage_path":  fe.StoragePath,
		"file_name":     fe.FileName,
		"size_bytes":    fe.SizeBytes,
		"sha256":        result.GetSha256(),
	}
	data, err := json.Marshal(payload)
	if err != nil {
		ix.logger.Error("indexer: marshal file uploaded event", zap.Error(err))
		return
	}
	if err := ix.nats.Publish("events.file.uploaded", data); err != nil {
		ix.logger.Error("indexer: publish file uploaded event", zap.Error(err))
	}
}

// IndexUpload records a file upload event that arrived via the MinIO webhook
// path (i.e., not via an agent UploadResult). It looks up the bucket by name
// within the default org scope, upserts a file entry, and publishes a NATS
// event. This method implements handler.IndexerClient.
func (ix *Indexer) IndexUpload(ctx context.Context, bucketName, objectKey string, sizeBytes int64, etag string) error {
	bucket, err := ix.store.GetBucketByName(ctx, defaultOrgID, bucketName)
	if err != nil {
		return fmt.Errorf("indexer: get bucket %q for minio event: %w", bucketName, err)
	}

	fileTypeID, err := ix.classifier.Classify(ctx, objectKey)
	if err != nil {
		ix.logger.Warn("indexer: classify file failed", zap.Error(err))
		fileTypeID = uuid.Nil
	}

	fileEntry, err := ix.store.UpsertFileEntry(ctx, UpsertFileEntryParams{
		OrgID:       defaultOrgID,
		FileTypeID:  uuid.NullUUID{UUID: fileTypeID, Valid: fileTypeID != uuid.Nil},
		BucketID:    bucket.ID,
		StoragePath: objectKey,
		FileName:    fileNameFromPath(objectKey),
		SizeBytes:   sizeBytes,
		Etag:        sql.NullString{String: etag, Valid: etag != ""},
		Status:      db.FileStatusCompleted,
		UploadedAt:  sql.NullTime{Time: time.Now().UTC(), Valid: true},
	})
	if err != nil {
		return fmt.Errorf("indexer: upsert file entry for minio event: %w", err)
	}

	ix.logger.Info("minio event indexed",
		zap.String("file_entry_id", fileEntry.ID.String()),
		zap.String("bucket", bucketName),
		zap.String("key", objectKey),
	)
	return nil
}

// IndexDeletion handles a MinIO ObjectRemoved event: it soft-deletes the
// matching file entry and publishes events.file.deleted. Deleting an object that
// was never indexed (or already deleted) is a no-op.
func (ix *Indexer) IndexDeletion(ctx context.Context, bucketName, objectKey string) error {
	bucket, err := ix.store.GetBucketByName(ctx, defaultOrgID, bucketName)
	if err != nil {
		return fmt.Errorf("indexer: get bucket %q for delete event: %w", bucketName, err)
	}

	fileEntry, err := ix.store.MarkFileEntryDeleted(ctx, bucket.ID, objectKey)
	if err != nil {
		if err == sql.ErrNoRows {
			ix.logger.Info("minio delete event: no matching file entry (no-op)",
				zap.String("bucket", bucketName), zap.String("key", objectKey))
			return nil
		}
		return fmt.Errorf("indexer: mark file entry deleted for minio event: %w", err)
	}

	ix.publishFileDeleted(fileEntry)
	ix.logger.Info("minio delete event indexed",
		zap.String("file_entry_id", fileEntry.ID.String()),
		zap.String("bucket", bucketName),
		zap.String("key", objectKey),
	)
	return nil
}

// publishFileDeleted emits events.file.deleted for a soft-deleted entry.
func (ix *Indexer) publishFileDeleted(fe *db.FileEntry) {
	payload := map[string]interface{}{
		"file_entry_id": fe.ID.String(),
		"bucket_id":     fe.BucketID.String(),
		"storage_path":  fe.StoragePath,
		"file_name":     fe.FileName,
	}
	data, err := json.Marshal(payload)
	if err != nil {
		ix.logger.Error("indexer: marshal file deleted event", zap.Error(err))
		return
	}
	if err := ix.nats.Publish("events.file.deleted", data); err != nil {
		ix.logger.Error("indexer: publish file deleted event", zap.Error(err))
	}
}
