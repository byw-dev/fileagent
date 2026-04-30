// Package queue provides a SQLite-backed local task queue for the Edge Agent.
// It persists upload tasks across restarts and tracks already-processed files
// to prevent duplicate uploads. Rules received from the Control Plane are also
// cached here so the Agent can operate offline.
package queue

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "github.com/mattn/go-sqlite3"
)

const schema = `
CREATE TABLE IF NOT EXISTS upload_tasks (
    id             TEXT    PRIMARY KEY,
    rule_id        TEXT    NOT NULL,
    local_path     TEXT    NOT NULL,
    storage_path   TEXT    NOT NULL,
    bucket         TEXT    NOT NULL,
    upload_id      TEXT,
    completed_parts TEXT,
    file_size      INTEGER NOT NULL DEFAULT 0,
    file_mtime     INTEGER NOT NULL DEFAULT 0,
    sha256         TEXT,
    status         TEXT    NOT NULL DEFAULT 'pending',
    retry_count    INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS processed_files (
    id          TEXT    PRIMARY KEY,
    rule_id     TEXT    NOT NULL,
    local_path  TEXT    NOT NULL,
    file_size   INTEGER NOT NULL,
    file_mtime  INTEGER NOT NULL,
    sha256      TEXT,
    uploaded_at INTEGER NOT NULL,
    UNIQUE (rule_id, local_path)
);

CREATE TABLE IF NOT EXISTS rules (
    id         TEXT    PRIMARY KEY,
    payload    TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_upload_tasks_status
    ON upload_tasks (status, created_at);

CREATE INDEX IF NOT EXISTS idx_processed_files_rule
    ON processed_files (rule_id, local_path);
`

// Status values for upload tasks.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// UploadTask represents a row in the upload_tasks table.
type UploadTask struct {
	ID             string
	RuleID         string
	LocalPath      string
	StoragePath    string
	Bucket         string
	UploadID       string
	CompletedParts string
	FileSize       int64
	FileMtime      int64
	SHA256         string
	Status         string
	RetryCount     int
	LastError      string
	CreatedAt      int64
	UpdatedAt      int64
}

// ProcessedFile represents a row in the processed_files table.
type ProcessedFile struct {
	ID         string
	RuleID     string
	LocalPath  string
	FileSize   int64
	FileMtime  int64
	SHA256     string
	UploadedAt int64
}

// Rule represents a row in the rules table.
type Rule struct {
	ID        string
	Payload   string
	UpdatedAt int64
}

// Queue wraps a SQLite database providing queue operations for the Agent.
type Queue struct {
	db *sql.DB
}

// Open opens (or creates) a SQLite database at dsn and initialises the schema.
// Use ":memory:" for in-process testing without file I/O.
func Open(dsn string) (*Queue, error) {
	db, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("queue: open %q: %w", dsn, err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer; avoid SQLITE_BUSY under load.
	if _, err = db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("queue: apply schema: %w", err)
	}
	return &Queue{db: db}, nil
}

// Close releases the underlying database connection.
func (q *Queue) Close() error {
	return q.db.Close()
}

// ── Upload Tasks ─────────────────────────────────────────────────────────────

// Enqueue inserts a new upload task with status "pending". The caller is
// responsible for setting task.ID (UUID), task.CreatedAt and task.UpdatedAt.
func (q *Queue) Enqueue(task *UploadTask) error {
	now := time.Now().Unix()
	if task.CreatedAt == 0 {
		task.CreatedAt = now
	}
	if task.UpdatedAt == 0 {
		task.UpdatedAt = now
	}
	task.Status = StatusPending

	_, err := q.db.Exec(`
        INSERT INTO upload_tasks
            (id, rule_id, local_path, storage_path, bucket, upload_id,
             completed_parts, file_size, file_mtime, sha256, status,
             retry_count, last_error, created_at, updated_at)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		task.ID, task.RuleID, task.LocalPath, task.StoragePath, task.Bucket,
		task.UploadID, task.CompletedParts, task.FileSize, task.FileMtime,
		task.SHA256, task.Status, task.RetryCount, task.LastError,
		task.CreatedAt, task.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("queue: enqueue task %q: %w", task.ID, err)
	}
	return nil
}

// DequeuePending returns up to limit tasks with status "pending", ordered by
// created_at ascending (oldest first), and transitions them to "running".
func (q *Queue) DequeuePending(limit int) ([]*UploadTask, error) {
	rows, err := q.db.Query(`
        SELECT id, rule_id, local_path, storage_path, bucket, upload_id,
               completed_parts, file_size, file_mtime, sha256, status,
               retry_count, last_error, created_at, updated_at
        FROM upload_tasks
        WHERE status = ?
        ORDER BY created_at ASC
        LIMIT ?`, StatusPending, limit)
	if err != nil {
		return nil, fmt.Errorf("queue: dequeue pending: %w", err)
	}
	defer rows.Close()

	tasks, err := scanTasks(rows)
	if err != nil {
		return nil, err
	}

	now := time.Now().Unix()
	for _, t := range tasks {
		if _, err = q.db.Exec(
			`UPDATE upload_tasks SET status=?, updated_at=? WHERE id=?`,
			StatusRunning, now, t.ID,
		); err != nil {
			return nil, fmt.Errorf("queue: mark running %q: %w", t.ID, err)
		}
		t.Status = StatusRunning
		t.UpdatedAt = now
	}
	return tasks, nil
}

// UpdateStatus sets the status of a task identified by id.
func (q *Queue) UpdateStatus(id, status string) error {
	res, err := q.db.Exec(
		`UPDATE upload_tasks SET status=?, updated_at=? WHERE id=?`,
		status, time.Now().Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("queue: update status %q: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("queue: task %q not found", id)
	}
	return nil
}

// MarkFailed increments retry_count, records the last error message, and sets
// the task status back to "pending" so it can be retried.
func (q *Queue) MarkFailed(id, errMsg string) error {
	res, err := q.db.Exec(`
        UPDATE upload_tasks
        SET status=?, retry_count=retry_count+1, last_error=?, updated_at=?
        WHERE id=?`,
		StatusFailed, errMsg, time.Now().Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("queue: mark failed %q: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("queue: task %q not found", id)
	}
	return nil
}

// ListByStatus returns all tasks with the given status.
func (q *Queue) ListByStatus(status string) ([]*UploadTask, error) {
	rows, err := q.db.Query(`
        SELECT id, rule_id, local_path, storage_path, bucket, upload_id,
               completed_parts, file_size, file_mtime, sha256, status,
               retry_count, last_error, created_at, updated_at
        FROM upload_tasks
        WHERE status = ?
        ORDER BY created_at ASC`, status)
	if err != nil {
		return nil, fmt.Errorf("queue: list by status %q: %w", status, err)
	}
	defer rows.Close()
	return scanTasks(rows)
}

// ── Processed Files ───────────────────────────────────────────────────────────

// UpsertProcessedFile inserts or replaces a processed-file record. On conflict
// on (rule_id, local_path) the existing row is overwritten.
func (q *Queue) UpsertProcessedFile(f *ProcessedFile) error {
	if f.UploadedAt == 0 {
		f.UploadedAt = time.Now().Unix()
	}
	_, err := q.db.Exec(`
        INSERT INTO processed_files
            (id, rule_id, local_path, file_size, file_mtime, sha256, uploaded_at)
        VALUES (?,?,?,?,?,?,?)
        ON CONFLICT(rule_id, local_path) DO UPDATE SET
            id=excluded.id, file_size=excluded.file_size,
            file_mtime=excluded.file_mtime, sha256=excluded.sha256,
            uploaded_at=excluded.uploaded_at`,
		f.ID, f.RuleID, f.LocalPath, f.FileSize, f.FileMtime, f.SHA256, f.UploadedAt,
	)
	if err != nil {
		return fmt.Errorf("queue: upsert processed file: %w", err)
	}
	return nil
}

// IsProcessed returns true if a record for (ruleID, localPath) already exists
// in processed_files.
func (q *Queue) IsProcessed(ruleID, localPath string) (bool, error) {
	var count int
	err := q.db.QueryRow(
		`SELECT COUNT(*) FROM processed_files WHERE rule_id=? AND local_path=?`,
		ruleID, localPath,
	).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("queue: is processed: %w", err)
	}
	return count > 0, nil
}

// ── Rules ─────────────────────────────────────────────────────────────────────

// UpsertRule inserts or updates a rule record.
func (q *Queue) UpsertRule(r *Rule) error {
	if r.UpdatedAt == 0 {
		r.UpdatedAt = time.Now().Unix()
	}
	_, err := q.db.Exec(`
        INSERT INTO rules (id, payload, updated_at)
        VALUES (?,?,?)
        ON CONFLICT(id) DO UPDATE SET payload=excluded.payload, updated_at=excluded.updated_at`,
		r.ID, r.Payload, r.UpdatedAt,
	)
	if err != nil {
		return fmt.Errorf("queue: upsert rule %q: %w", r.ID, err)
	}
	return nil
}

// GetRule returns the Rule for id, or an error wrapping sql.ErrNoRows when not found.
func (q *Queue) GetRule(id string) (*Rule, error) {
	r := &Rule{}
	err := q.db.QueryRow(
		`SELECT id, payload, updated_at FROM rules WHERE id=?`, id,
	).Scan(&r.ID, &r.Payload, &r.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, fmt.Errorf("queue: rule %q not found: %w", id, sql.ErrNoRows)
	}
	if err != nil {
		return nil, fmt.Errorf("queue: get rule %q: %w", id, err)
	}
	return r, nil
}

// ── helpers ───────────────────────────────────────────────────────────────────

func scanTasks(rows *sql.Rows) ([]*UploadTask, error) {
	var tasks []*UploadTask
	for rows.Next() {
		t := &UploadTask{}
		if err := rows.Scan(
			&t.ID, &t.RuleID, &t.LocalPath, &t.StoragePath, &t.Bucket,
			&t.UploadID, &t.CompletedParts, &t.FileSize, &t.FileMtime,
			&t.SHA256, &t.Status, &t.RetryCount, &t.LastError,
			&t.CreatedAt, &t.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("queue: scan task: %w", err)
		}
		tasks = append(tasks, t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: rows error: %w", err)
	}
	return tasks, nil
}
