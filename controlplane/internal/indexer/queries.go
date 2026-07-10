// Package indexer provides file classification and indexing functionality
// for the Control Plane. It processes upload results from agents and maintains
// the file_entries and upload_logs tables.
package indexer

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/google/uuid"
)

// ── UpsertFileEntry ──────────────────────────────────────────────────────────

// UpsertFileEntryParams holds the parameters for UpsertFileEntry.
type UpsertFileEntryParams struct {
	OrgID        uuid.UUID
	FileTypeID   uuid.NullUUID
	AgentID      uuid.NullUUID
	RuleID       uuid.NullUUID
	BucketID     uuid.UUID
	StoragePath  string
	OriginalPath sql.NullString
	FileName     string
	SizeBytes    int64
	Sha256       sql.NullString
	Etag         sql.NullString
	ContentType  sql.NullString
	FileMtime    sql.NullTime
	Status       db.FileStatus
	UploadedAt   sql.NullTime
}

const upsertFileEntrySQL = `
INSERT INTO file_entries (
    org_id, file_type_id, agent_id, rule_id, bucket_id,
    storage_path, original_path, file_name, size_bytes,
    sha256, etag, content_type, file_mtime, status, uploaded_at
) VALUES (
    $1, $2, $3, $4, $5,
    $6, $7, $8, $9,
    $10, $11, $12, $13, $14, $15
)
ON CONFLICT (bucket_id, storage_path) DO UPDATE SET
    file_type_id  = EXCLUDED.file_type_id,
    agent_id      = EXCLUDED.agent_id,
    rule_id       = EXCLUDED.rule_id,
    file_name     = EXCLUDED.file_name,
    size_bytes    = EXCLUDED.size_bytes,
    sha256        = EXCLUDED.sha256,
    etag          = EXCLUDED.etag,
    content_type  = EXCLUDED.content_type,
    file_mtime    = EXCLUDED.file_mtime,
    status        = EXCLUDED.status,
    uploaded_at   = EXCLUDED.uploaded_at,
    updated_at    = NOW()
RETURNING id, org_id, file_type_id, agent_id, rule_id, bucket_id,
    storage_path, original_path, file_name, size_bytes,
    sha256, etag, content_type, file_mtime, status, uploaded_at, created_at, updated_at
`

// UpsertFileEntry inserts or updates a file_entries row.
func UpsertFileEntry(ctx context.Context, dbtx db.DBTX, arg UpsertFileEntryParams) (*db.FileEntry, error) {
	row := dbtx.QueryRowContext(ctx, upsertFileEntrySQL,
		arg.OrgID,
		arg.FileTypeID,
		arg.AgentID,
		arg.RuleID,
		arg.BucketID,
		arg.StoragePath,
		arg.OriginalPath,
		arg.FileName,
		arg.SizeBytes,
		arg.Sha256,
		arg.Etag,
		arg.ContentType,
		arg.FileMtime,
		arg.Status,
		arg.UploadedAt,
	)
	var i db.FileEntry
	err := row.Scan(
		&i.ID,
		&i.OrgID,
		&i.FileTypeID,
		&i.AgentID,
		&i.RuleID,
		&i.BucketID,
		&i.StoragePath,
		&i.OriginalPath,
		&i.FileName,
		&i.SizeBytes,
		&i.Sha256,
		&i.Etag,
		&i.ContentType,
		&i.FileMtime,
		&i.Status,
		&i.UploadedAt,
		&i.CreatedAt,
		&i.UpdatedAt,
	)
	return &i, err
}

// ── CreateUploadLog ──────────────────────────────────────────────────────────

// CreateUploadLogParams holds the parameters for CreateUploadLog.
type CreateUploadLogParams struct {
	OrgID            uuid.UUID
	AgentID          uuid.UUID
	FileEntryID      uuid.NullUUID
	RuleID           uuid.NullUUID
	OriginalPath     string
	StoragePath      string
	SizeBytes        int64
	BytesTransferred int64
	Status           string
	ErrorMessage     sql.NullString
	RetryCount       int32
	StartedAt        time.Time
	FinishedAt       sql.NullTime
}

const createUploadLogSQL = `
INSERT INTO upload_logs (
    org_id, agent_id, file_entry_id, rule_id,
    original_path, storage_path, size_bytes, bytes_transferred,
    status, error_message, retry_count, started_at, finished_at
) VALUES (
    $1, $2, $3, $4,
    $5, $6, $7, $8,
    $9, $10, $11, $12, $13
)
RETURNING id, org_id, agent_id, file_entry_id, rule_id,
    original_path, storage_path, size_bytes, bytes_transferred,
    status, error_message, retry_count, started_at, finished_at, created_at
`

// CreateUploadLog inserts an upload_log record.
func CreateUploadLog(ctx context.Context, dbtx db.DBTX, arg CreateUploadLogParams) (*db.UploadLog, error) {
	row := dbtx.QueryRowContext(ctx, createUploadLogSQL,
		arg.OrgID,
		arg.AgentID,
		arg.FileEntryID,
		arg.RuleID,
		arg.OriginalPath,
		arg.StoragePath,
		arg.SizeBytes,
		arg.BytesTransferred,
		arg.Status,
		arg.ErrorMessage,
		arg.RetryCount,
		arg.StartedAt,
		arg.FinishedAt,
	)
	var i db.UploadLog
	err := row.Scan(
		&i.ID,
		&i.OrgID,
		&i.AgentID,
		&i.FileEntryID,
		&i.RuleID,
		&i.OriginalPath,
		&i.StoragePath,
		&i.SizeBytes,
		&i.BytesTransferred,
		&i.Status,
		&i.ErrorMessage,
		&i.RetryCount,
		&i.StartedAt,
		&i.FinishedAt,
		&i.CreatedAt,
	)
	return &i, err
}

// ── GetBucketByName ──────────────────────────────────────────────────────────

const getBucketByNameSQL = `
SELECT id, org_id, name, description, policy_json, sts_role_arn, created_at, updated_at
FROM buckets
WHERE org_id = $1 AND name = $2
LIMIT 1
`

// GetBucketByName looks up a bucket by (org_id, name).
func GetBucketByName(ctx context.Context, dbtx db.DBTX, orgID uuid.UUID, name string) (*db.Bucket, error) {
	row := dbtx.QueryRowContext(ctx, getBucketByNameSQL, orgID, name)
	var b db.Bucket
	err := row.Scan(
		&b.ID,
		&b.OrgID,
		&b.Name,
		&b.Description,
		&b.PolicyJson,
		&b.StsRoleArn,
		&b.CreatedAt,
		&b.UpdatedAt,
	)
	return &b, err
}

// ── MarkFileEntryDeleted ─────────────────────────────────────────────────────

const markFileEntryDeletedSQL = `
UPDATE file_entries
SET status = 'deleted', updated_at = NOW()
WHERE bucket_id = $1 AND storage_path = $2 AND status != 'deleted'
RETURNING id, org_id, file_type_id, agent_id, rule_id, bucket_id,
    storage_path, original_path, file_name, size_bytes,
    sha256, etag, content_type, file_mtime, status, uploaded_at, created_at, updated_at
`

// MarkFileEntryDeleted soft-deletes the file entry identified by (bucket, path),
// returning the updated row. It returns sql.ErrNoRows when no matching entry
// exists or it was already deleted, so the caller can treat it as a no-op.
func MarkFileEntryDeleted(ctx context.Context, dbtx db.DBTX, bucketID uuid.UUID, storagePath string) (*db.FileEntry, error) {
	row := dbtx.QueryRowContext(ctx, markFileEntryDeletedSQL, bucketID, storagePath)
	var i db.FileEntry
	err := row.Scan(
		&i.ID,
		&i.OrgID,
		&i.FileTypeID,
		&i.AgentID,
		&i.RuleID,
		&i.BucketID,
		&i.StoragePath,
		&i.OriginalPath,
		&i.FileName,
		&i.SizeBytes,
		&i.Sha256,
		&i.Etag,
		&i.ContentType,
		&i.FileMtime,
		&i.Status,
		&i.UploadedAt,
		&i.CreatedAt,
		&i.UpdatedAt,
	)
	return &i, err
}

// ── ListEnabledEventRules ────────────────────────────────────────────────────

const listEnabledEventRulesSQL = `
SELECT id, org_id, name, event_type, filter, action_type, action_config, enabled, created_by, created_at, updated_at
FROM event_rules
WHERE org_id = $1 AND event_type = $2 AND enabled = TRUE
ORDER BY created_at DESC
`

// ListEnabledEventRules returns all enabled event rules matching an event type.
func ListEnabledEventRules(ctx context.Context, dbtx db.DBTX, orgID uuid.UUID, eventType db.EventType) ([]*db.EventRule, error) {
	rows, err := dbtx.QueryContext(ctx, listEnabledEventRulesSQL, orgID, eventType)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []*db.EventRule
	for rows.Next() {
		var r db.EventRule
		if err := rows.Scan(
			&r.ID,
			&r.OrgID,
			&r.Name,
			&r.EventType,
			&r.Filter,
			&r.ActionType,
			&r.ActionConfig,
			&r.Enabled,
			&r.CreatedBy,
			&r.CreatedAt,
			&r.UpdatedAt,
		); err != nil {
			return nil, err
		}
		rules = append(rules, &r)
	}
	return rules, rows.Err()
}

// ── ListFileTypeRules ────────────────────────────────────────────────────────

const listFileTypeRulesSQL = `
SELECT id, file_type_id, path_pattern, priority, created_at
FROM file_type_rules
ORDER BY priority DESC
`

// ListFileTypeRules returns all file_type_rules ordered by priority DESC.
func ListFileTypeRules(ctx context.Context, dbtx db.DBTX) ([]*db.FileTypeRule, error) {
	rows, err := dbtx.QueryContext(ctx, listFileTypeRulesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var rules []*db.FileTypeRule
	for rows.Next() {
		var r db.FileTypeRule
		if err := rows.Scan(&r.ID, &r.FileTypeID, &r.PathPattern, &r.Priority, &r.CreatedAt); err != nil {
			return nil, err
		}
		rules = append(rules, &r)
	}
	return rules, rows.Err()
}

// ── GetRuleTagInfo ───────────────────────────────────────────────────────────

// RuleTagInfo is the subset of a collection rule the tagging engine needs: the
// metadata declaration plus the dest_path_template that the storage path was
// rendered from (needed to reverse-extract path variables).
type RuleTagInfo struct {
	Metadata         json.RawMessage
	DestPathTemplate string
}

const getRuleTagInfoSQL = `
SELECT metadata, dest_path_template FROM collection_rules WHERE id = $1 AND org_id = $2 LIMIT 1
`

// GetRuleTagInfo returns a rule's metadata + dest_path_template scoped to an org,
// so an agent that sends an arbitrary rule UUID cannot read another tenant's rule
// (defense-in-depth). It returns sql.ErrNoRows when no such rule exists in the
// org, so callers can treat it as "no declaration".
func GetRuleTagInfo(ctx context.Context, dbtx db.DBTX, orgID, ruleID uuid.UUID) (RuleTagInfo, error) {
	row := dbtx.QueryRowContext(ctx, getRuleTagInfoSQL, ruleID, orgID)
	var info RuleTagInfo
	if err := row.Scan(&info.Metadata, &info.DestPathTemplate); err != nil {
		return RuleTagInfo{}, err
	}
	return info, nil
}

// ── GetFileTypeIDByName ──────────────────────────────────────────────────────

const getFileTypeIDByNameSQL = `
SELECT id FROM file_types WHERE org_id = $1 AND name = $2 LIMIT 1
`

// GetFileTypeIDByName resolves a declared file-type name to its id within an
// org. It returns uuid.Nil (with a nil error) when no file type matches, so the
// caller can fall back to glob classification.
func GetFileTypeIDByName(ctx context.Context, dbtx db.DBTX, orgID uuid.UUID, name string) (uuid.UUID, error) {
	row := dbtx.QueryRowContext(ctx, getFileTypeIDByNameSQL, orgID, name)
	var id uuid.UUID
	if err := row.Scan(&id); err != nil {
		if err == sql.ErrNoRows {
			return uuid.Nil, nil
		}
		return uuid.Nil, err
	}
	return id, nil
}

// ── UpsertFileTag ────────────────────────────────────────────────────────────

// UpsertFileTagParams holds the parameters for UpsertFileTag.
type UpsertFileTagParams struct {
	FileEntryID uuid.UUID
	Key         string
	Value       string
	Source      string
}

// upsertFileTagSQL is idempotent on the (file_entry_id, key) primary key: a
// repeated UploadResult re-applies the same tag rather than duplicating it.
const upsertFileTagSQL = `
INSERT INTO file_tags (file_entry_id, key, value, source)
VALUES ($1, $2, $3, $4)
ON CONFLICT (file_entry_id, key) DO UPDATE SET
    value  = EXCLUDED.value,
    source = EXCLUDED.source
`

// UpsertFileTag inserts or updates a single file tag.
func UpsertFileTag(ctx context.Context, dbtx db.DBTX, arg UpsertFileTagParams) error {
	_, err := dbtx.ExecContext(ctx, upsertFileTagSQL,
		arg.FileEntryID, arg.Key, arg.Value, arg.Source)
	return err
}

// ── InsertFileTagIfAbsent ────────────────────────────────────────────────────

// insertFileTagIfAbsentSQL does NOT overwrite an existing (file_entry_id, key):
// an explicit tag (e.g. a rule static_tag) wins over a path-extracted one, and a
// re-processed UploadResult is a no-op. The RETURNING row is present only when a
// new row was inserted, which the caller uses to gate one-time side effects
// (e.g. bumping a pending value's hit_count).
const insertFileTagIfAbsentSQL = `
INSERT INTO file_tags (file_entry_id, key, value, source)
VALUES ($1, $2, $3, $4)
ON CONFLICT (file_entry_id, key) DO NOTHING
RETURNING file_entry_id
`

// InsertFileTagIfAbsent inserts a file tag only when the (file_entry_id, key) is
// not already set, returning inserted=true when a new row was created.
func InsertFileTagIfAbsent(ctx context.Context, dbtx db.DBTX, arg UpsertFileTagParams) (bool, error) {
	row := dbtx.QueryRowContext(ctx, insertFileTagIfAbsentSQL,
		arg.FileEntryID, arg.Key, arg.Value, arg.Source)
	var id uuid.UUID
	err := row.Scan(&id)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// ── Tag vocabulary lookups (governance) ──────────────────────────────────────

const getTagKeyByNameSQL = `
SELECT id, value_controlled FROM tag_keys WHERE org_id = $1 AND key = $2 LIMIT 1
`

// TagKeyInfo identifies a controlled tag key for governance checks.
type TagKeyInfo struct {
	ID              uuid.UUID
	ValueControlled bool
}

// GetTagKeyByName looks up a tag key by (org, key). found=false (nil error) when
// the key is not registered as a vocabulary key.
func GetTagKeyByName(ctx context.Context, dbtx db.DBTX, orgID uuid.UUID, key string) (TagKeyInfo, bool, error) {
	row := dbtx.QueryRowContext(ctx, getTagKeyByNameSQL, orgID, key)
	var info TagKeyInfo
	if err := row.Scan(&info.ID, &info.ValueControlled); err != nil {
		if err == sql.ErrNoRows {
			return TagKeyInfo{}, false, nil
		}
		return TagKeyInfo{}, false, err
	}
	return info, true, nil
}

const tagValueExistsSQL = `
SELECT EXISTS (SELECT 1 FROM tag_values WHERE tag_key_id = $1 AND value = $2)
`

// TagValueExists reports whether value is a registered value of the tag key.
func TagValueExists(ctx context.Context, dbtx db.DBTX, tagKeyID uuid.UUID, value string) (bool, error) {
	var exists bool
	err := dbtx.QueryRowContext(ctx, tagValueExistsSQL, tagKeyID, value).Scan(&exists)
	return exists, err
}

const findSimilarTagValueSQL = `
SELECT value FROM tag_values
WHERE tag_key_id = $1 AND lower(value) = lower($2)
LIMIT 1
`

// FindSimilarTagValue returns a registered value that differs from value only by
// case (the common tokyo/Tokyo drift), for use as a suggested_value. It returns
// "" when there is no such near-match.
func FindSimilarTagValue(ctx context.Context, dbtx db.DBTX, tagKeyID uuid.UUID, value string) (string, error) {
	var v string
	err := dbtx.QueryRowContext(ctx, findSimilarTagValueSQL, tagKeyID, value).Scan(&v)
	if err == sql.ErrNoRows {
		return "", nil
	}
	return v, err
}

// ── UpsertPendingTagValue ────────────────────────────────────────────────────

// UpsertPendingTagValueParams holds the parameters for UpsertPendingTagValue.
type UpsertPendingTagValueParams struct {
	OrgID          uuid.UUID
	TagKeyID       uuid.UUID
	ExtractedValue string
	Source         string
	SourceRuleID   uuid.NullUUID
	SuggestedValue sql.NullString
}

// upsertPendingTagValueSQL bumps hit_count on repeat sightings of the same
// unregistered value, keeping a single queue row per (tag_key_id, value).
const upsertPendingTagValueSQL = `
INSERT INTO pending_tag_values (org_id, tag_key_id, extracted_value, source, source_rule_id, suggested_value)
VALUES ($1, $2, $3, $4, $5, $6)
ON CONFLICT (tag_key_id, extracted_value) DO UPDATE SET
    hit_count = pending_tag_values.hit_count + 1
`

// UpsertPendingTagValue queues an unregistered controlled value for admin review,
// incrementing hit_count when it has been seen before.
func UpsertPendingTagValue(ctx context.Context, dbtx db.DBTX, arg UpsertPendingTagValueParams) error {
	_, err := dbtx.ExecContext(ctx, upsertPendingTagValueSQL,
		arg.OrgID, arg.TagKeyID, arg.ExtractedValue, arg.Source, arg.SourceRuleID, arg.SuggestedValue)
	return err
}

// ── CreateEventDelivery ──────────────────────────────────────────────────────

// CreateEventDeliveryParams holds the parameters for CreateEventDelivery.
type CreateEventDeliveryParams struct {
	EventRuleID uuid.UUID
	EventType   db.EventType
	Payload     []byte
	Status      string
	NextRetryAt sql.NullTime
}

const createEventDeliverySQL = `
INSERT INTO event_deliveries (event_rule_id, event_type, payload, status, next_retry_at)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, event_rule_id, event_type, payload, status, response_code, response_body,
    attempt_count, next_retry_at, created_at, delivered_at
`

// CreateEventDelivery inserts an event_delivery record.
func CreateEventDelivery(ctx context.Context, dbtx db.DBTX, arg CreateEventDeliveryParams) (*db.EventDelivery, error) {
	row := dbtx.QueryRowContext(ctx, createEventDeliverySQL,
		arg.EventRuleID, arg.EventType, arg.Payload, arg.Status, arg.NextRetryAt,
	)
	var d db.EventDelivery
	err := row.Scan(
		&d.ID,
		&d.EventRuleID,
		&d.EventType,
		&d.Payload,
		&d.Status,
		&d.ResponseCode,
		&d.ResponseBody,
		&d.AttemptCount,
		&d.NextRetryAt,
		&d.CreatedAt,
		&d.DeliveredAt,
	)
	return &d, err
}

// ── UpdateEventDelivery ──────────────────────────────────────────────────────

// UpdateEventDeliveryParams holds the parameters for UpdateEventDelivery.
type UpdateEventDeliveryParams struct {
	ID           uuid.UUID
	Status       string
	ResponseCode sql.NullInt32
	ResponseBody sql.NullString
	AttemptCount int32
	NextRetryAt  sql.NullTime
	DeliveredAt  sql.NullTime
}

const updateEventDeliverySQL = `
UPDATE event_deliveries SET
    status        = $2,
    response_code = $3,
    response_body = $4,
    attempt_count = $5,
    next_retry_at = $6,
    delivered_at  = $7
WHERE id = $1
`

// UpdateEventDelivery updates delivery status and attempt information.
func UpdateEventDelivery(ctx context.Context, dbtx db.DBTX, arg UpdateEventDeliveryParams) error {
	_, err := dbtx.ExecContext(ctx, updateEventDeliverySQL,
		arg.ID, arg.Status, arg.ResponseCode, arg.ResponseBody,
		arg.AttemptCount, arg.NextRetryAt, arg.DeliveredAt,
	)
	return err
}

// ── ListPendingEventDeliveries ───────────────────────────────────────────────

const listPendingEventDeliveriesSQL = `
SELECT id, event_rule_id, event_type, payload, status, response_code, response_body,
    attempt_count, next_retry_at, created_at, delivered_at
FROM event_deliveries
WHERE status IN ('pending', 'failed')
  AND (next_retry_at IS NULL OR next_retry_at <= NOW())
ORDER BY created_at ASC
`

// ListPendingEventDeliveries returns failed deliveries that are due for retry.
func ListPendingEventDeliveries(ctx context.Context, dbtx db.DBTX) ([]*db.EventDelivery, error) {
	rows, err := dbtx.QueryContext(ctx, listPendingEventDeliveriesSQL)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var deliveries []*db.EventDelivery
	for rows.Next() {
		var d db.EventDelivery
		if err := rows.Scan(
			&d.ID,
			&d.EventRuleID,
			&d.EventType,
			&d.Payload,
			&d.Status,
			&d.ResponseCode,
			&d.ResponseBody,
			&d.AttemptCount,
			&d.NextRetryAt,
			&d.CreatedAt,
			&d.DeliveredAt,
		); err != nil {
			return nil, err
		}
		deliveries = append(deliveries, &d)
	}
	return deliveries, rows.Err()
}

// fileNameFromPath extracts the filename from a storage path.
func fileNameFromPath(storagePath string) string {
	return filepath.Base(storagePath)
}
