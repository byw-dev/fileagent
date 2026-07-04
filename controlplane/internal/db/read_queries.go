// Package db provides data access layer for the FileAgent control plane.
// This file contains hand-written cursor-based pagination queries that cannot
// be expressed in sqlc due to optional filter parameters.
package db

import (
	"context"
	"database/sql"
	"time"

	"github.com/google/uuid"
)

// ── ListFileEntries ───────────────────────────────────────────────────────────

// ListFileEntriesParams holds parameters for ListFileEntries.
type ListFileEntriesParams struct {
	OrgID      uuid.UUID
	AgentID    uuid.NullUUID
	BucketID   uuid.NullUUID
	FileTypeID uuid.NullUUID
	Status     NullFileStatus
	// Cursor pagination: items with (created_at, id) < (CursorCreatedAt, CursorID)
	CursorCreatedAt sql.NullTime
	CursorID        uuid.NullUUID
	Limit           int32
}

const listFileEntriesSQL = `
SELECT id, org_id, file_type_id, agent_id, rule_id, bucket_id,
       storage_path, original_path, file_name, size_bytes,
       sha256, etag, content_type, file_mtime, status, uploaded_at,
       created_at, updated_at
FROM file_entries
WHERE org_id = $1
  AND ($2::UUID IS NULL OR agent_id = $2)
  AND ($3::UUID IS NULL OR bucket_id = $3)
  AND ($4::UUID IS NULL OR file_type_id = $4)
  AND ($5::file_status IS NULL OR status = $5)
  AND ($6::TIMESTAMPTZ IS NULL OR (created_at, id) < ($6, $7::UUID))
ORDER BY created_at DESC, id DESC
LIMIT $8
`

// ListFileEntries returns a cursor-paginated list of file entries with optional filters.
func (q *Queries) ListFileEntries(ctx context.Context, arg ListFileEntriesParams) ([]*FileEntry, error) {
	rows, err := q.db.QueryContext(ctx, listFileEntriesSQL,
		arg.OrgID,
		arg.AgentID,
		arg.BucketID,
		arg.FileTypeID,
		arg.Status,
		arg.CursorCreatedAt,
		arg.CursorID,
		arg.Limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*FileEntry
	for rows.Next() {
		var i FileEntry
		if err := rows.Scan(
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
		); err != nil {
			return nil, err
		}
		items = append(items, &i)
	}
	return items, rows.Err()
}

// ── GetFileEntryByID ─────────────────────────────────────────────────────────

const getFileEntryByIDSQL = `
SELECT id, org_id, file_type_id, agent_id, rule_id, bucket_id,
       storage_path, original_path, file_name, size_bytes,
       sha256, etag, content_type, file_mtime, status, uploaded_at,
       created_at, updated_at
FROM file_entries
WHERE id = $1
LIMIT 1
`

// GetFileEntryByID returns a single file entry by its UUID.
func (q *Queries) GetFileEntryByID(ctx context.Context, id uuid.UUID) (*FileEntry, error) {
	row := q.db.QueryRowContext(ctx, getFileEntryByIDSQL, id)
	var i FileEntry
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

// ── ListUploadLogs ────────────────────────────────────────────────────────────

// ListUploadLogsParams holds parameters for ListUploadLogs.
type ListUploadLogsParams struct {
	OrgID   uuid.UUID
	AgentID uuid.NullUUID
	// Cursor pagination
	CursorCreatedAt sql.NullTime
	CursorID        uuid.NullUUID
	Limit           int32
}

const listUploadLogsSQL = `
SELECT id, org_id, agent_id, file_entry_id, rule_id,
       original_path, storage_path, size_bytes, bytes_transferred,
       status, error_message, retry_count, started_at, finished_at, created_at
FROM upload_logs
WHERE org_id = $1
  AND ($2::UUID IS NULL OR agent_id = $2)
  AND ($3::TIMESTAMPTZ IS NULL OR (created_at, id) < ($3, $4::UUID))
ORDER BY created_at DESC, id DESC
LIMIT $5
`

// ListUploadLogs returns a cursor-paginated list of upload logs, optionally filtered by agent.
func (q *Queries) ListUploadLogs(ctx context.Context, arg ListUploadLogsParams) ([]*UploadLog, error) {
	rows, err := q.db.QueryContext(ctx, listUploadLogsSQL,
		arg.OrgID,
		arg.AgentID,
		arg.CursorCreatedAt,
		arg.CursorID,
		arg.Limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*UploadLog
	for rows.Next() {
		var i UploadLog
		if err := rows.Scan(
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
		); err != nil {
			return nil, err
		}
		items = append(items, &i)
	}
	return items, rows.Err()
}

// ── GetUploadLogByID ─────────────────────────────────────────────────────────

const getUploadLogByIDSQL = `
SELECT id, org_id, agent_id, file_entry_id, rule_id,
       original_path, storage_path, size_bytes, bytes_transferred,
       status, error_message, retry_count, started_at, finished_at, created_at
FROM upload_logs
WHERE id = $1
LIMIT 1
`

// GetUploadLogByID returns a single upload log record by its UUID.
func (q *Queries) GetUploadLogByID(ctx context.Context, id uuid.UUID) (*UploadLog, error) {
	row := q.db.QueryRowContext(ctx, getUploadLogByIDSQL, id)
	var i UploadLog
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

// ── ListDeliveriesByRule ─────────────────────────────────────────────────────

// ListDeliveriesByRuleParams holds parameters for ListDeliveriesByRule.
type ListDeliveriesByRuleParams struct {
	EventRuleID uuid.UUID
	// Cursor pagination
	CursorCreatedAt sql.NullTime
	CursorID        uuid.NullUUID
	Limit           int32
}

const listDeliveriesByRuleSQL = `
SELECT id, event_rule_id, event_type, payload, status,
       response_code, response_body, attempt_count, next_retry_at,
       created_at, delivered_at
FROM event_deliveries
WHERE event_rule_id = $1
  AND ($2::TIMESTAMPTZ IS NULL OR (created_at, id) < ($2, $3::UUID))
ORDER BY created_at DESC, id DESC
LIMIT $4
`

// ListDeliveriesByRule returns cursor-paginated event deliveries for a given rule.
func (q *Queries) ListDeliveriesByRule(ctx context.Context, arg ListDeliveriesByRuleParams) ([]*EventDelivery, error) {
	rows, err := q.db.QueryContext(ctx, listDeliveriesByRuleSQL,
		arg.EventRuleID,
		arg.CursorCreatedAt,
		arg.CursorID,
		arg.Limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var items []*EventDelivery
	for rows.Next() {
		var d EventDelivery
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
		items = append(items, &d)
	}
	return items, rows.Err()
}

// ── CountFileEntries ─────────────────────────────────────────────────────────

// CountFileEntriesFilter holds the optional filter fields for CountFileEntries.
// It mirrors the filter fields of ListFileEntriesParams without cursor / limit.
type CountFileEntriesFilter struct {
	OrgID      uuid.UUID
	AgentID    uuid.NullUUID
	BucketID   uuid.NullUUID
	FileTypeID uuid.NullUUID
	Status     NullFileStatus
}

const countFileEntriesSQL = `
SELECT COUNT(*)
FROM file_entries
WHERE org_id = $1
  AND ($2::UUID IS NULL OR agent_id = $2)
  AND ($3::UUID IS NULL OR bucket_id = $3)
  AND ($4::UUID IS NULL OR file_type_id = $4)
  AND ($5::file_status IS NULL OR status = $5)
`

// CountFileEntries returns the total number of file entries matching the
// given optional filters (no cursor/limit applied).
func (q *Queries) CountFileEntries(ctx context.Context, f CountFileEntriesFilter) (int64, error) {
	row := q.db.QueryRowContext(ctx, countFileEntriesSQL,
		f.OrgID,
		f.AgentID,
		f.BucketID,
		f.FileTypeID,
		f.Status,
	)
	var n int64
	return n, row.Scan(&n)
}

// ── CountUploadLogs ───────────────────────────────────────────────────────────

// CountUploadLogsFilter holds the optional filter fields for CountUploadLogs.
type CountUploadLogsFilter struct {
	OrgID   uuid.UUID
	AgentID uuid.NullUUID
}

const countUploadLogsSQL = `
SELECT COUNT(*)
FROM upload_logs
WHERE org_id = $1
  AND ($2::UUID IS NULL OR agent_id = $2)
`

// CountUploadLogs returns the total number of upload log entries matching the
// given optional filters (no cursor/limit applied).
func (q *Queries) CountUploadLogs(ctx context.Context, f CountUploadLogsFilter) (int64, error) {
	row := q.db.QueryRowContext(ctx, countUploadLogsSQL, f.OrgID, f.AgentID)
	var n int64
	return n, row.Scan(&n)
}

// ── CountDeliveriesByRule ─────────────────────────────────────────────────────

const countDeliveriesByRuleSQL = `
SELECT COUNT(*) FROM event_deliveries WHERE event_rule_id = $1
`

// CountDeliveriesByRule returns the total number of event deliveries for the
// given event rule (no cursor/limit applied).
func (q *Queries) CountDeliveriesByRule(ctx context.Context, eventRuleID uuid.UUID) (int64, error) {
	row := q.db.QueryRowContext(ctx, countDeliveriesByRuleSQL, eventRuleID)
	var n int64
	return n, row.Scan(&n)
}

// ── DashboardStats ────────────────────────────────────────────────────────────

// DayCount is one bucket of the upload trend: a UTC date and the number of
// files uploaded that day.
type DayCount struct {
	Date  string `json:"date"` // YYYY-MM-DD (UTC)
	Count int64  `json:"count"`
}

// DashboardStats holds the aggregate figures shown on the dashboard.
type DashboardStats struct {
	TotalAgents  int64      `json:"total_agents"`
	OnlineAgents int64      `json:"online_agents"`
	TotalFiles   int64      `json:"total_files"`
	StorageBytes int64      `json:"storage_bytes"`
	TodayUploads int64      `json:"today_uploads"`
	UploadTrend  []DayCount `json:"upload_trend"` // last 7 days, oldest first
}

const trendDays = 7

// DashboardStats computes the dashboard aggregates for an org as of now (UTC).
// An agent counts as online when its last_seen_at is within the offline
// threshold (90s); storage is the sum of indexed file sizes; today's uploads and
// the 7-day trend are keyed on uploaded_at in UTC.
func (q *Queries) DashboardStats(ctx context.Context, orgID uuid.UUID, now time.Time) (*DashboardStats, error) {
	now = now.UTC()
	onlineSince := now.Add(-90 * time.Second)
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	trendStart := todayStart.AddDate(0, 0, -(trendDays - 1))

	stats := &DashboardStats{}

	scalar := func(query string, args ...interface{}) (int64, error) {
		var n sql.NullInt64
		if err := q.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
			return 0, err
		}
		return n.Int64, nil
	}

	var err error
	if stats.TotalAgents, err = scalar(
		`SELECT COUNT(*) FROM agents WHERE org_id = $1`, orgID); err != nil {
		return nil, err
	}
	if stats.OnlineAgents, err = scalar(
		`SELECT COUNT(*) FROM agents WHERE org_id = $1 AND last_seen_at >= $2`, orgID, onlineSince); err != nil {
		return nil, err
	}
	if stats.TotalFiles, err = scalar(
		`SELECT COUNT(*) FROM file_entries WHERE org_id = $1`, orgID); err != nil {
		return nil, err
	}
	if stats.StorageBytes, err = scalar(
		`SELECT COALESCE(SUM(size_bytes), 0) FROM file_entries WHERE org_id = $1`, orgID); err != nil {
		return nil, err
	}
	if stats.TodayUploads, err = scalar(
		`SELECT COUNT(*) FROM file_entries WHERE org_id = $1 AND uploaded_at >= $2`, orgID, todayStart); err != nil {
		return nil, err
	}

	// Upload trend: query days with data, then fill a dense 7-day skeleton so
	// days with zero uploads still appear.
	rows, err := q.db.QueryContext(ctx,
		`SELECT to_char(date_trunc('day', uploaded_at), 'YYYY-MM-DD') AS day, COUNT(*)
		 FROM file_entries
		 WHERE org_id = $1 AND uploaded_at >= $2
		 GROUP BY day`, orgID, trendStart)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int64)
	for rows.Next() {
		var day string
		var n int64
		if err := rows.Scan(&day, &n); err != nil {
			return nil, err
		}
		counts[day] = n
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	stats.UploadTrend = make([]DayCount, trendDays)
	for i := 0; i < trendDays; i++ {
		day := trendStart.AddDate(0, 0, i).Format("2006-01-02")
		stats.UploadTrend[i] = DayCount{Date: day, Count: counts[day]}
	}
	return stats, nil
}

// ── helpers (package-internal) ───────────────────────────────────────────────

// NewCursorFromFileEntry builds cursor fields from the last FileEntry in a page.
func NewCursorFromFileEntry(e *FileEntry) (sql.NullTime, uuid.NullUUID) {
	return sql.NullTime{Time: e.CreatedAt, Valid: true},
		uuid.NullUUID{UUID: e.ID, Valid: true}
}

// NewCursorFromUploadLog builds cursor fields from the last UploadLog in a page.
func NewCursorFromUploadLog(l *UploadLog) (sql.NullTime, uuid.NullUUID) {
	return sql.NullTime{Time: l.CreatedAt, Valid: true},
		uuid.NullUUID{UUID: l.ID, Valid: true}
}

// NewCursorFromDelivery builds cursor fields from the last EventDelivery in a page.
func NewCursorFromDelivery(d *EventDelivery) (sql.NullTime, uuid.NullUUID) {
	return sql.NullTime{Time: d.CreatedAt, Valid: true},
		uuid.NullUUID{UUID: d.ID, Valid: true}
}
