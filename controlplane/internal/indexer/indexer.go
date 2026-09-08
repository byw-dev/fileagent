package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	agentv1 "github.com/byw-dev/fileagent/api/v1"
	"github.com/byw-dev/fileagent/controlplane/internal/bootstrap"
	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/pkg/trollsift"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// NATSPublisher publishes messages to NATS subjects.
type NATSPublisher interface {
	Publish(subject string, data []byte) error
}

// IndexerStore is the minimal database interface used by the Indexer. Using an
// interface (rather than db.DBTX directly) makes the Indexer unit-testable
// without a real database.
type IndexerStore interface {
	GetBucketByName(ctx context.Context, orgID uuid.UUID, name string) (*db.Bucket, error)
	UpsertFileEntry(ctx context.Context, params UpsertFileEntryParams) (*db.FileEntry, error)
	MarkFileEntryDeleted(ctx context.Context, bucketID uuid.UUID, storagePath string) (*db.FileEntry, error)
	CreateUploadLog(ctx context.Context, params CreateUploadLogParams) (*db.UploadLog, error)
	ListFileTypeRules(ctx context.Context) ([]*db.FileTypeRule, error)
	GetRuleTagInfo(ctx context.Context, orgID, ruleID uuid.UUID) (RuleTagInfo, error)
	GetFileTypeIDByName(ctx context.Context, orgID uuid.UUID, name string) (uuid.UUID, error)
	UpsertFileTag(ctx context.Context, params UpsertFileTagParams) error
	InsertFileTagIfAbsent(ctx context.Context, params UpsertFileTagParams) (bool, error)
	GetTagKeyByName(ctx context.Context, orgID uuid.UUID, key string) (TagKeyInfo, bool, error)
	TagValueExists(ctx context.Context, tagKeyID uuid.UUID, value string) (bool, error)
	FindSimilarTagValue(ctx context.Context, tagKeyID uuid.UUID, value string) (string, error)
	UpsertPendingTagValue(ctx context.Context, params UpsertPendingTagValueParams) error
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

// GetRuleTagInfo delegates to the package-level function.
func (d *dbtxIndexerStore) GetRuleTagInfo(ctx context.Context, orgID, ruleID uuid.UUID) (RuleTagInfo, error) {
	return GetRuleTagInfo(ctx, d.dbtx, orgID, ruleID)
}

// GetFileTypeIDByName delegates to the package-level function.
func (d *dbtxIndexerStore) GetFileTypeIDByName(ctx context.Context, orgID uuid.UUID, name string) (uuid.UUID, error) {
	return GetFileTypeIDByName(ctx, d.dbtx, orgID, name)
}

// UpsertFileTag delegates to the package-level function.
func (d *dbtxIndexerStore) UpsertFileTag(ctx context.Context, params UpsertFileTagParams) error {
	return UpsertFileTag(ctx, d.dbtx, params)
}

// InsertFileTagIfAbsent delegates to the package-level function.
func (d *dbtxIndexerStore) InsertFileTagIfAbsent(ctx context.Context, params UpsertFileTagParams) (bool, error) {
	return InsertFileTagIfAbsent(ctx, d.dbtx, params)
}

// GetTagKeyByName delegates to the package-level function.
func (d *dbtxIndexerStore) GetTagKeyByName(ctx context.Context, orgID uuid.UUID, key string) (TagKeyInfo, bool, error) {
	return GetTagKeyByName(ctx, d.dbtx, orgID, key)
}

// TagValueExists delegates to the package-level function.
func (d *dbtxIndexerStore) TagValueExists(ctx context.Context, tagKeyID uuid.UUID, value string) (bool, error) {
	return TagValueExists(ctx, d.dbtx, tagKeyID, value)
}

// FindSimilarTagValue delegates to the package-level function.
func (d *dbtxIndexerStore) FindSimilarTagValue(ctx context.Context, tagKeyID uuid.UUID, value string) (string, error) {
	return FindSimilarTagValue(ctx, d.dbtx, tagKeyID, value)
}

// UpsertPendingTagValue delegates to the package-level function.
func (d *dbtxIndexerStore) UpsertPendingTagValue(ctx context.Context, params UpsertPendingTagValueParams) error {
	return UpsertPendingTagValue(ctx, d.dbtx, params)
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

	// Parse rule ID if provided.
	var ruleID uuid.NullUUID
	if ruleStr := result.GetRuleId(); ruleStr != "" {
		if id, err := uuid.Parse(ruleStr); err == nil {
			ruleID = uuid.NullUUID{UUID: id, Valid: true}
		}
	}

	// Load the rule's metadata declaration (file_type / static_tags / path_tag_map)
	// and dest_path_template. Best effort: a missing or malformed declaration
	// degrades to glob-only classification with no tags.
	var ruleMeta ruleMetadata
	var destTemplate string
	if ruleID.Valid {
		ruleMeta, destTemplate = ix.loadRuleMetadata(ctx, orgID, ruleID.UUID)
	}

	// Classify file type: a rule-declared file_type takes priority over the
	// glob file_type_rules, which remain the fallback for undeclared data.
	fileTypeID := ix.classifyFileType(ctx, orgID, result.GetStoragePath(), ruleMeta.FileType)

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

	// Apply rule-declared static tags (source=rule_static), idempotent on
	// (file_entry_id, key). Best effort: a tag failure is logged but does not
	// fail indexing (mirrors upload-log handling).
	ix.applyStaticTags(ctx, fileEntry.ID, ruleMeta.StaticTags)

	// Extract path-variable tags (source=path_var) from the storage path.
	// Runs after static tags and does not overwrite them (explicit wins).
	ix.applyPathVarTags(ctx, orgID, fileEntry.ID, result.GetStoragePath(), destTemplate, ruleMeta.PathTagMap, ruleID)

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

// Tag source markers record how a file_tag was derived.
const (
	tagSourceRuleStatic = "rule_static" // from a rule's static_tags declaration
	tagSourcePathVar    = "path_var"    // extracted from a storage-path variable
)

// pendingSourcePathVar marks a pending value discovered during live indexing
// (as opposed to a historical backfill).
const pendingSourcePathVar = "path_var"

// Tag key/value length limits mirror the file_tags column widths (VARCHAR(64) /
// VARCHAR(128)). Overlong values are skipped before the insert so a misconfigured
// rule cannot spam a DB error + warning on every single upload.
const (
	maxTagKeyLen   = 64
	maxTagValueLen = 128
)

// ruleMetadata is the declared shape of collection_rules.metadata consumed by
// the tagging engine (metadata model 6c, Phase 1). PathTagMap maps a tag key to
// a path-template variable reference (e.g. {"site": "{site}"}).
type ruleMetadata struct {
	FileType   string            `json:"file_type"`
	StaticTags map[string]string `json:"static_tags"`
	PathTagMap map[string]string `json:"path_tag_map"`
}

// loadRuleMetadata fetches and parses a rule's metadata declaration and returns
// its dest_path_template. It returns a zero-value ruleMetadata (logging
// non-ErrNoRows failures) when the rule is missing or its metadata cannot be
// parsed, so indexing degrades gracefully.
func (ix *Indexer) loadRuleMetadata(ctx context.Context, orgID, ruleID uuid.UUID) (ruleMetadata, string) {
	var meta ruleMetadata
	info, err := ix.store.GetRuleTagInfo(ctx, orgID, ruleID)
	if err != nil {
		if err != sql.ErrNoRows {
			ix.logger.Warn("indexer: load rule metadata",
				zap.String("rule_id", ruleID.String()), zap.Error(err))
		}
		return meta, ""
	}
	if len(info.Metadata) == 0 {
		return meta, info.DestPathTemplate
	}
	if err := json.Unmarshal(info.Metadata, &meta); err != nil {
		ix.logger.Warn("indexer: parse rule metadata",
			zap.String("rule_id", ruleID.String()), zap.Error(err))
		return ruleMetadata{}, info.DestPathTemplate
	}
	return meta, info.DestPathTemplate
}

// classifyFileType resolves the file type for a storage path. A non-empty
// declared file-type name (from rule metadata) takes priority and is resolved to
// its id within the org; otherwise, or when the name does not resolve, it falls
// back to glob file_type_rules. Returns uuid.Nil when nothing matches.
func (ix *Indexer) classifyFileType(ctx context.Context, orgID uuid.UUID, storagePath, declared string) uuid.UUID {
	if declared != "" {
		id, err := ix.store.GetFileTypeIDByName(ctx, orgID, declared)
		switch {
		case err != nil:
			ix.logger.Warn("indexer: resolve declared file_type",
				zap.String("file_type", declared), zap.Error(err))
		case id != uuid.Nil:
			return id
		default:
			ix.logger.Warn("indexer: declared file_type not found, falling back to glob",
				zap.String("file_type", declared))
		}
	}
	id, err := ix.classifier.Classify(ctx, storagePath)
	if err != nil {
		ix.logger.Warn("indexer: classify file failed", zap.Error(err))
		return uuid.Nil
	}
	return id
}

// tagWithinLimits reports whether a tag key/value fits the file_tags columns
// (VARCHAR 64/128 by character). Rune count matches VARCHAR(n) semantics; it logs
// and returns false for overlong values so a misconfigured rule cannot spam a DB
// length error on every upload.
func (ix *Indexer) tagWithinLimits(fileEntryID uuid.UUID, key, value string) bool {
	if utf8.RuneCountInString(key) > maxTagKeyLen || utf8.RuneCountInString(value) > maxTagValueLen {
		ix.logger.Warn("indexer: skip overlong tag",
			zap.String("file_entry_id", fileEntryID.String()),
			zap.String("key", key))
		return false
	}
	return true
}

// applyStaticTags writes rule-declared static tags to file_tags with
// source=rule_static. Empty keys/values are skipped; per-tag failures are
// logged but do not fail indexing.
func (ix *Indexer) applyStaticTags(ctx context.Context, fileEntryID uuid.UUID, tags map[string]string) {
	for key, value := range tags {
		if key == "" || value == "" {
			continue
		}
		if !ix.tagWithinLimits(fileEntryID, key, value) {
			continue
		}
		if err := ix.store.UpsertFileTag(ctx, UpsertFileTagParams{
			FileEntryID: fileEntryID,
			Key:         key,
			Value:       value,
			Source:      tagSourceRuleStatic,
		}); err != nil {
			ix.logger.Warn("indexer: upsert static tag",
				zap.String("file_entry_id", fileEntryID.String()),
				zap.String("key", key), zap.Error(err))
		}
	}
}

// templateVarName extracts the variable name from a path_tag_map reference such
// as "{site}" or "{site:fmt}". It returns "" for anything that is not a single
// bare {var} reference — including multi-placeholder shapes like "{a}{b}", where
// a leftover brace in the inner content signals more than one placeholder.
func templateVarName(ref string) string {
	ref = strings.TrimSpace(ref)
	if len(ref) < 3 || ref[0] != '{' || ref[len(ref)-1] != '}' {
		return ""
	}
	inner := ref[1 : len(ref)-1]
	if strings.ContainsAny(inner, "{}") {
		return ""
	}
	if i := strings.IndexByte(inner, ':'); i >= 0 {
		inner = inner[:i]
	}
	return strings.TrimSpace(inner)
}

// applyPathVarTags extracts tag values from the storage path via the rule's
// dest_path_template (trollsift reverse-parse) and writes them with
// source=path_var. It runs after static tags and does NOT overwrite an existing
// (file, key) — an explicit static tag wins. For a controlled key whose value is
// not yet in the vocabulary, the raw value is still recorded but is queued in
// pending_tag_values for admin review. Best effort throughout: any failure is
// logged without failing indexing.
func (ix *Indexer) applyPathVarTags(ctx context.Context, orgID, fileEntryID uuid.UUID, storagePath, destTemplate string, pathTagMap map[string]string, ruleID uuid.NullUUID) {
	if len(pathTagMap) == 0 || destTemplate == "" {
		return
	}
	// Normalise before parsing: the agent strips the leading "/" when composing
	// the object key, so reverse-parsing the raw template never matched for any
	// template written as "/{...}" — which is what the Web UI creates by default
	// (IC-BUG-16). See docs/design/contracts.md V-3.
	parser, err := trollsift.New(trollsift.NormalizeTemplate(destTemplate))
	if err != nil {
		ix.logger.Warn("indexer: parse dest_path_template",
			zap.String("template", destTemplate), zap.Error(err))
		return
	}
	vals, err := parser.Parse(trollsift.NormalizeObjectKey(storagePath))
	if err != nil {
		// The stored object may not match the template (e.g. legacy/hand-placed);
		// skip path-var extraction rather than failing the index.
		ix.logger.Warn("indexer: storage path does not match template",
			zap.String("storage_path", storagePath), zap.Error(err))
		return
	}
	for key, ref := range pathTagMap {
		varName := templateVarName(ref)
		if key == "" || varName == "" {
			continue
		}
		val, ok := vals[varName]
		if !ok {
			ix.logger.Warn("indexer: path_tag_map variable not present in path",
				zap.String("key", key), zap.String("var", varName))
			continue
		}
		value := val.Raw
		if value == "" || !ix.tagWithinLimits(fileEntryID, key, value) {
			continue
		}

		// Look up the vocabulary key once for governance. A registered key may
		// explicitly forbid path-variable mapping (allow_path_var=false), in which
		// case path extraction must not populate it at all — no tag, no queue.
		keyInfo, found, err := ix.store.GetTagKeyByName(ctx, orgID, key)
		if err != nil {
			ix.logger.Warn("indexer: lookup tag key", zap.String("key", key), zap.Error(err))
			continue
		}
		if found && !keyInfo.AllowPathVar {
			continue
		}

		inserted, err := ix.store.InsertFileTagIfAbsent(ctx, UpsertFileTagParams{
			FileEntryID: fileEntryID, Key: key, Value: value, Source: tagSourcePathVar,
		})
		if err != nil {
			ix.logger.Warn("indexer: insert path_var tag",
				zap.String("file_entry_id", fileEntryID.String()),
				zap.String("key", key), zap.Error(err))
			continue
		}
		// Only queue governance on a fresh insert, so a re-processed UploadResult
		// (row already present) does not inflate pending hit_count. Uncontrolled or
		// unregistered keys carry the raw value without vocabulary governance.
		if inserted && found && keyInfo.ValueControlled {
			ix.queuePendingIfUnregistered(ctx, orgID, keyInfo, key, value, ruleID)
		}
	}
}

// queuePendingIfUnregistered queues an extracted value for admin review when it
// is not yet a registered value of a controlled key. keyInfo is the already
// looked-up vocabulary key. Best effort: failures are logged, not fatal.
func (ix *Indexer) queuePendingIfUnregistered(ctx context.Context, orgID uuid.UUID, keyInfo TagKeyInfo, key, value string, ruleID uuid.NullUUID) {
	exists, err := ix.store.TagValueExists(ctx, keyInfo.ID, value)
	if err != nil {
		ix.logger.Warn("indexer: check tag value", zap.String("key", key), zap.Error(err))
		return
	}
	if exists {
		return // already an approved value
	}
	suggested, err := ix.store.FindSimilarTagValue(ctx, keyInfo.ID, value)
	if err != nil {
		ix.logger.Warn("indexer: find similar tag value", zap.String("key", key), zap.Error(err))
		// proceed with no suggestion
	}
	if err := ix.store.UpsertPendingTagValue(ctx, UpsertPendingTagValueParams{
		OrgID:          orgID,
		TagKeyID:       keyInfo.ID,
		ExtractedValue: value,
		Source:         pendingSourcePathVar,
		SourceRuleID:   ruleID,
		SuggestedValue: sql.NullString{String: suggested, Valid: suggested != ""},
	}); err != nil {
		ix.logger.Warn("indexer: queue pending tag value",
			zap.String("key", key), zap.Error(err))
	}
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
// within the default org scope and upserts a file entry. It deliberately does
// NOT publish events.file.uploaded: agent uploads already emit that event via
// HandleUploadResult, and a MinIO ObjectCreated webhook fires for the same
// object, so publishing here would double-emit. This method implements
// handler.IndexerClient.
func (ix *Indexer) IndexUpload(ctx context.Context, bucketName, objectKey string, sizeBytes int64, etag string) error {
	bucket, err := ix.store.GetBucketByName(ctx, bootstrap.DefaultOrgID, bucketName)
	if err != nil {
		return fmt.Errorf("indexer: get bucket %q for minio event: %w", bucketName, err)
	}

	fileTypeID, err := ix.classifier.Classify(ctx, objectKey)
	if err != nil {
		ix.logger.Warn("indexer: classify file failed", zap.Error(err))
		fileTypeID = uuid.Nil
	}

	fileEntry, err := ix.store.UpsertFileEntry(ctx, UpsertFileEntryParams{
		OrgID:       bootstrap.DefaultOrgID,
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
	bucket, err := ix.store.GetBucketByName(ctx, bootstrap.DefaultOrgID, bucketName)
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
