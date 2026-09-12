// Package queue provides a SQLite-backed local task queue for the Edge Agent.
// It persists upload tasks across restarts and tracks already-processed files
// to prevent duplicate uploads. Rules received from the Control Plane are also
// cached here so the Agent can operate offline.
package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	_ "github.com/mattn/go-sqlite3"
	"go.uber.org/zap"
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
    next_retry_at  INTEGER NOT NULL DEFAULT 0,
    last_error     TEXT,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL,
    file_offset    INTEGER NOT NULL DEFAULT 0,
    append_mode    TEXT    NOT NULL DEFAULT 'overwrite'
);

CREATE TABLE IF NOT EXISTS upload_reports (
    task_id TEXT PRIMARY KEY REFERENCES upload_tasks(id) ON DELETE CASCADE,
    payload BLOB NOT NULL,
    last_reported_at INTEGER NOT NULL DEFAULT 0
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

-- Durable record of a MinIO multipart upload that must be aborted (IC-3 ②/P2).
-- Written BEFORE the owning task row is deleted or marked final so the abort
-- always keeps a retryable local identity; drained by the executor's abort
-- worker with backoff. There is no reliable AbortIncompleteMultipartUpload ILM
-- backstop on current MinIO builds (IC-3 ③), so this table is the safety net
-- for aborts that fail.
CREATE TABLE IF NOT EXISTS multipart_abort_outbox (
    upload_id       TEXT PRIMARY KEY,
    task_id         TEXT NOT NULL,
    bucket          TEXT NOT NULL,
    storage_path    TEXT NOT NULL,
    attempts        INTEGER NOT NULL DEFAULT 0,
    next_attempt_at INTEGER NOT NULL DEFAULT 0,
    last_error      TEXT,
    created_at      INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS rules (
    id         TEXT    PRIMARY KEY,
    payload    TEXT    NOT NULL,
    updated_at INTEGER NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_upload_tasks_status
    ON upload_tasks (status, created_at);

-- Supports the EnqueueIfNoActive dedup guard: its NOT EXISTS subquery filters
-- by (rule_id, local_path). Without this index every enqueue scans the whole
-- active backlog, which serialises the initial scan behind O(N x backlog)
-- write-lock work on the single SQLite connection (PR #100 review F6).
CREATE INDEX IF NOT EXISTS idx_upload_tasks_dedup
    ON upload_tasks (rule_id, local_path);

CREATE INDEX IF NOT EXISTS idx_processed_files_rule
    ON processed_files (rule_id, local_path);
`

// schemaMigrations runs DDL statements that add columns to existing tables
// that may have been created before the current schema version. SQLite does
// not support ADD COLUMN IF NOT EXISTS, so we attempt each migration and
// silently ignore "duplicate column name" errors.
var schemaMigrations = []string{
	`ALTER TABLE upload_tasks ADD COLUMN file_offset INTEGER NOT NULL DEFAULT 0`,
	`ALTER TABLE upload_tasks ADD COLUMN append_mode TEXT NOT NULL DEFAULT 'overwrite'`,
	`ALTER TABLE upload_tasks ADD COLUMN next_retry_at INTEGER NOT NULL DEFAULT 0`,
}

// Status values for upload tasks.
const (
	StatusPending   = "pending"
	StatusRunning   = "running"
	StatusReported  = "reported"
	StatusCompleted = "completed"
	StatusFailed    = "failed"
)

// NextRetryNever marks a failed task as abandoned — no further retry is
// scheduled for it, across restarts included. It is stamped when a task is
// given up on (retry budget exhausted, terminal failure): its in-flight
// multipart upload was aborted at that moment, and resurrecting it on every
// process restart would loop forever on a verdict that will not change.
const NextRetryNever = -1

// ErrTaskNotFound is returned by mutating operations (UpdateStatus, MarkFailed)
// when no task matches the given id — typically because the row was already
// evicted to honour queue_max_size. Callers can use errors.Is to treat this as a
// benign, expected outcome rather than a failure.
var ErrTaskNotFound = errors.New("task not found")

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
	// NextRetryAt is the unix time (seconds) when a failed task becomes
	// eligible for re-queueing (IC-3 P1-d). Zero means "no schedule recorded"
	// (legacy rows, or a task never failed) and is treated as due; a negative
	// value (NextRetryNever) means the task was abandoned — never re-queue it.
	NextRetryAt  int64
	LastError    string
	CreatedAt      int64
	UpdatedAt      int64
	// FileOffset is the byte offset from which to begin uploading in tail mode.
	// Zero means upload from the beginning of the file.
	FileOffset int64
	// AppendMode is "tail", "close_wait", or "overwrite" (full-file upload).
	AppendMode string
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
	// logger is optional; nil keeps queue operations silent. Used only for
	// warnings such as failing to refresh task metadata on reset.
	logger *zap.Logger
}

// Open opens (or creates) a SQLite database at dsn and initialises the schema.
// Use ":memory:" for in-process testing without file I/O.
func Open(dsn string) (*Queue, error) {
	return OpenWithLogger(dsn, nil)
}

// OpenWithLogger is Open with an optional logger for operational warnings.
func OpenWithLogger(dsn string, logger *zap.Logger) (*Queue, error) {
	db, err := sql.Open("sqlite3", dsn+"?_journal_mode=WAL&_busy_timeout=5000")
	if err != nil {
		return nil, fmt.Errorf("queue: open %q: %w", dsn, err)
	}
	db.SetMaxOpenConns(1) // SQLite is single-writer; avoid SQLITE_BUSY under load.
	if _, err = db.Exec(schema); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("queue: apply schema: %w", err)
	}
	// Run column-addition migrations on existing databases (ignore duplicate-column errors).
	for _, stmt := range schemaMigrations {
		if _, merr := db.Exec(stmt); merr != nil {
			// "duplicate column name" is expected when the column already exists.
			if !isDuplicateColumnError(merr) {
				_ = db.Close()
				return nil, fmt.Errorf("queue: schema migration %q: %w", stmt, merr)
			}
		}
	}
	return &Queue{db: db, logger: logger}, nil
}

// Close releases the underlying database connection.
func (q *Queue) Close() error {
	return q.db.Close()
}

// ── Upload Tasks ─────────────────────────────────────────────────────────────

// Enqueue inserts a new upload task with status "pending". The caller is
// responsible for setting task.ID (UUID), task.CreatedAt and task.UpdatedAt.
func (q *Queue) Enqueue(task *UploadTask) error {
	prepareEnqueue(task)

	_, err := q.db.Exec(`
        INSERT INTO upload_tasks
            (id, rule_id, local_path, storage_path, bucket, upload_id,
             completed_parts, file_size, file_mtime, sha256, status,
             retry_count, last_error, created_at, updated_at, file_offset, append_mode)
        VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		task.ID, task.RuleID, task.LocalPath, task.StoragePath, task.Bucket,
		task.UploadID, task.CompletedParts, task.FileSize, task.FileMtime,
		task.SHA256, task.Status, task.RetryCount, task.LastError,
		task.CreatedAt, task.UpdatedAt, task.FileOffset, task.AppendMode,
	)
	if err != nil {
		return fmt.Errorf("queue: enqueue task %q: %w", task.ID, err)
	}
	return nil
}

// EnqueueIfNoActive inserts task unless the same rule, path, modification
// time, and size already has a pending, running, or reported task. The guard
// and insert are one SQLite statement so concurrent producers cannot enqueue
// the same active file version twice. Failed and completed rows do not block a
// later collection attempt.
func (q *Queue) EnqueueIfNoActive(ctx context.Context, task *UploadTask) (bool, error) {
	prepareEnqueue(task)
	result, err := q.db.ExecContext(ctx, `
        INSERT INTO upload_tasks
            (id, rule_id, local_path, storage_path, bucket, upload_id,
             completed_parts, file_size, file_mtime, sha256, status,
             retry_count, last_error, created_at, updated_at, file_offset, append_mode)
        SELECT ?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?
        WHERE NOT EXISTS (
            SELECT 1 FROM upload_tasks
            WHERE rule_id=? AND local_path=? AND file_mtime=? AND file_size=?
              AND status IN (?, ?, ?)
        )`,
		task.ID, task.RuleID, task.LocalPath, task.StoragePath, task.Bucket,
		task.UploadID, task.CompletedParts, task.FileSize, task.FileMtime,
		task.SHA256, task.Status, task.RetryCount, task.LastError,
		task.CreatedAt, task.UpdatedAt, task.FileOffset, task.AppendMode,
		task.RuleID, task.LocalPath, task.FileMtime, task.FileSize,
		StatusPending, StatusRunning, StatusReported,
	)
	if err != nil {
		return false, fmt.Errorf("queue: enqueue task %q if inactive: %w", task.ID, err)
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("queue: inspect enqueue task %q: %w", task.ID, err)
	}
	return inserted > 0, nil
}

// prepareEnqueue fills queue-owned defaults before an upload task is inserted.
func prepareEnqueue(task *UploadTask) {
	now := time.Now().Unix()
	if task.CreatedAt == 0 {
		task.CreatedAt = now
	}
	if task.UpdatedAt == 0 {
		task.UpdatedAt = now
	}
	task.Status = StatusPending
}

// CountPending returns the number of tasks currently waiting to be uploaded
// (status "pending"). It is used to report queue depth in the agent heartbeat.
func (q *Queue) CountPending() (int, error) {
	var n int
	if err := q.db.QueryRow(
		`SELECT COUNT(*) FROM upload_tasks WHERE status = ?`, StatusPending,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("queue: count pending: %w", err)
	}
	return n, nil
}

// CountActive returns the number of non-terminal upload tasks (status "pending",
// "running", or "failed") currently held in the queue. Completed tasks are
// excluded because they represent finished work and do not contribute to backlog
// pressure. It is used to enforce the configured queue_max_size cap.
//
// The active statuses are matched with an explicit IN list rather than
// `status != 'completed'` so SQLite can use the idx_upload_tasks_status index:
// completed rows are never removed (only evicted rows are) and can accumulate
// without bound, so an inequality full-table scan would make every Submit's
// capacity check progressively slower.
func (q *Queue) CountActive() (int, error) {
	var n int
	if err := q.db.QueryRow(
		`SELECT COUNT(*) FROM upload_tasks WHERE status IN (?, ?, ?, ?)`,
		StatusPending, StatusRunning, StatusFailed, StatusReported,
	).Scan(&n); err != nil {
		return 0, fmt.Errorf("queue: count active: %w", err)
	}
	return n, nil
}

// DeleteOldestEvictable removes the oldest evictable upload task (by created_at
// ascending) and returns it so the caller can log the eviction. Evictable means
// status "pending" or "failed": tasks waiting to run or waiting for a retry
// backoff. Tasks in status "running" are excluded because they are in-flight in a
// worker goroutine (deleting the row would orphan the upload); their count is
// bounded by the worker concurrency and is far below the cap. When nothing is
// evictable it returns (nil, nil). It implements the queue_max_size eviction
// policy: drop the oldest backlog task when the local queue is full
// (system-design §4.6). Failed tasks must be evictable, not just pending, because
// under a sustained upload outage tasks continually cycle pending→running→failed,
// so the backlog to bound lives largely in the "failed" state.
//
// excludeID is never evicted (pass "" to exclude nothing). Capacity enforcement
// runs after the new task has been enqueued and passes that task's id here, so a
// freshly collected file is never the one dropped — eviction always sheds older
// backlog first.
//
// The SELECT and DELETE are separate statements, so a worker's DequeuePending
// could transition the chosen row to "running" in between. The DELETE is
// therefore guarded by the same status filter and, if it removes nothing (the row
// transitioned), the selection is retried on the next-oldest evictable task. This
// prevents deleting an in-flight task and orphaning its upload. Retries are
// bounded; if the queue is churning too hard to settle on a victim it returns
// (nil, nil), which the caller treats as "nothing evictable".
//
// Evicting a task whose multipart upload is still in flight (upload_id set)
// writes a durable abort record in the SAME transaction as the delete, BEFORE
// the row is removed (IC-3 ②/P2): once the row is gone no retry would ever
// find the upload identity again, and the best-effort abort at the call site
// can fail (MinIO unreachable). The abort record is what makes the eviction's
// cleanup retryable — there is no ILM backstop on current MinIO builds.
func (q *Queue) DeleteOldestEvictable(excludeID string) (*UploadTask, error) {
	const maxAttempts = 8
	for attempt := 0; attempt < maxAttempts; attempt++ {
		tx, err := q.db.Begin()
		if err != nil {
			return nil, fmt.Errorf("queue: begin eviction transaction: %w", err)
		}
		rows, err := tx.Query(`
        SELECT id, rule_id, local_path, storage_path, bucket, upload_id,
               completed_parts, file_size, file_mtime, sha256, status,
               retry_count, next_retry_at, last_error, created_at, updated_at,
               file_offset, append_mode
        FROM upload_tasks
        WHERE status IN (?, ?) AND id != ?
        ORDER BY created_at ASC
        LIMIT 1`, StatusPending, StatusFailed, excludeID)
		if err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("queue: select oldest evictable: %w", err)
		}
		tasks, err := scanTasks(rows)
		_ = rows.Close() // release the connection before the DELETE below
		if err != nil {
			_ = tx.Rollback()
			return nil, err
		}
		if len(tasks) == 0 {
			_ = tx.Rollback()
			return nil, nil
		}
		t := tasks[0]
		// Durable abort record FIRST, inside the same transaction as the
		// delete: if the DELETE commits, the record commits with it. An
		// aborted-but-unreachable MinIO then still leaves a retryable
		// identity behind instead of a permanently untracked orphan.
		if t.UploadID != "" {
			if _, err := tx.Exec(`
                INSERT INTO multipart_abort_outbox
                    (upload_id, task_id, bucket, storage_path, attempts, next_attempt_at, last_error, created_at)
                VALUES (?,?,?,?,0,0,NULL,?)
                ON CONFLICT(upload_id) DO NOTHING`,
				t.UploadID, t.ID, t.Bucket, t.StoragePath, time.Now().Unix(),
			); err != nil {
				_ = tx.Rollback()
				return nil, fmt.Errorf("queue: record abort of evicted task %q: %w", t.ID, err)
			}
		}
		// Guard the DELETE with the status filter: if the row transitioned to
		// "running" since the SELECT, this removes nothing and we retry.
		res, err := tx.Exec(
			`DELETE FROM upload_tasks WHERE id=? AND status IN (?, ?)`,
			t.ID, StatusPending, StatusFailed,
		)
		if err != nil {
			_ = tx.Rollback()
			return nil, fmt.Errorf("queue: delete oldest evictable %q: %w", t.ID, err)
		}
		if n, _ := res.RowsAffected(); n == 0 {
			// Row changed state under us; try the next-oldest evictable task.
			_ = tx.Rollback()
			continue
		}
		if err := tx.Commit(); err != nil {
			return nil, fmt.Errorf("queue: commit eviction of %q: %w", t.ID, err)
		}
		return t, nil
	}
	return nil, nil
}

// DequeuePending returns up to limit tasks with status "pending", ordered by
// created_at ascending (oldest first), and transitions them to "running".
func (q *Queue) DequeuePending(limit int) ([]*UploadTask, error) {
	rows, err := q.db.Query(`
        SELECT id, rule_id, local_path, storage_path, bucket, upload_id,
               completed_parts, file_size, file_mtime, sha256, status,
               retry_count, next_retry_at, last_error, created_at, updated_at,
               file_offset, append_mode
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

// ResetRunningToPending makes tasks interrupted by a prior agent process
// eligible for dequeue again. Reported tasks are intentionally untouched:
// their objects are already uploaded and the durable report replay path must
// resend only metadata rather than retransmitting file content.
//
// Each reset row's file_size/file_mtime is refreshed from the filesystem:
// files may have been appended to while the agent was down, and uploader
// re-stats at upload time, so a frozen stale tuple would diverge from what the
// initial scan sees and the EnqueueIfNoActive guard would let a duplicate task
// enqueue — both uploading the same current bytes (PR #100 review R1). A stat
// failure keeps the original values and does not block the reset.
//
// The whole reset is one transaction: refreshed metadata is written first,
// then the status is flipped, so a task can never become visible as pending
// while still carrying a stale tuple, and a crash (or write failure) mid-reset
// rolls back to a consistent running state. The reads and writes all run on
// the same tx so the listed rows cannot change underneath the refresh.
func (q *Queue) ResetRunningToPending(ctx context.Context) (int64, error) {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("queue: begin reset transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	rows, err := tx.QueryContext(ctx, `
        SELECT id, rule_id, local_path, storage_path, bucket, upload_id,
               completed_parts, file_size, file_mtime, sha256, status,
               retry_count, next_retry_at, last_error, created_at, updated_at,
               file_offset, append_mode
        FROM upload_tasks
        WHERE status = ?
        ORDER BY created_at ASC`, StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("queue: list running tasks for reset: %w", err)
	}
	running, err := scanTasks(rows)
	rows.Close()
	if err != nil {
		return 0, fmt.Errorf("queue: scan running tasks for reset: %w", err)
	}

	// Refresh each row's metadata from the filesystem while still inside the
	// transaction, BEFORE the status flip: an executor dequeues only pending
	// rows, so this ordering guarantees a dequeued task always carries the
	// refreshed tuple (PR #100 review R1, round-2 atomicity fix). A stat
	// failure keeps the original values and does not block the reset.
	for _, t := range running {
		info, statErr := os.Stat(t.LocalPath)
		if statErr != nil {
			if q.logger != nil {
				q.logger.Warn("queue: cannot refresh task file metadata on reset, keeping stored values",
					zap.String("task_id", t.ID), zap.String("path", t.LocalPath), zap.Error(statErr))
			}
			continue
		}
		t.FileSize = info.Size()
		t.FileMtime = info.ModTime().Unix()
		if _, err := tx.ExecContext(ctx, `
            UPDATE upload_tasks
            SET file_size=?, file_mtime=?, updated_at=?
            WHERE id=?`, t.FileSize, t.FileMtime, time.Now().Unix(), t.ID); err != nil {
			return 0, fmt.Errorf("queue: refresh task %q metadata on reset: %w", t.ID, err)
		}
	}

	result, err := tx.ExecContext(ctx, `
        UPDATE upload_tasks
        SET status=?, updated_at=?
        WHERE status=?`, StatusPending, time.Now().Unix(), StatusRunning)
	if err != nil {
		return 0, fmt.Errorf("queue: reset running tasks: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("queue: reset running tasks: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("queue: commit reset transaction: %w", err)
	}
	return n, nil
}

// SaveMultipartProgress persists a task's in-flight multipart upload state —
// the upload ID and the JSON blob of completed parts — so that a crash or a
// retry can resume the same MinIO multipart upload instead of restarting from
// part 1 (IC-BUG-5). Call it after the upload is initiated and again every
// time a part completes. Like UpdateStatus it returns ErrTaskNotFound when the
// row is gone (evicted), which callers may treat as benign.
func (q *Queue) SaveMultipartProgress(ctx context.Context, id, uploadID, partsJSON string) error {
	res, err := q.db.ExecContext(ctx,
		`UPDATE upload_tasks SET upload_id=?, completed_parts=?, updated_at=? WHERE id=?`,
		uploadID, partsJSON, time.Now().Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("queue: save multipart progress %q: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("queue: task %q: %w", id, ErrTaskNotFound)
	}
	return nil
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
		return fmt.Errorf("queue: task %q: %w", id, ErrTaskNotFound)
	}
	return nil
}

// MarkFailed increments retry_count, records the last error message, sets the
// task status to "failed", and stamps next_retry_at — the persisted retry
// schedule (IC-3 P1-d), so a process that exits during the backoff window can
// recover the task at startup instead of losing it forever. A zero nextRetryAt
// stamps NextRetryNever: the caller gave up on the task (retry budget
// exhausted or terminal failure), so no restart may resurrect it. Call
// UpdateStatus with StatusPending to re-queue the task for the next attempt.
func (q *Queue) MarkFailed(ctx context.Context, id, errMsg string, nextRetryAt time.Time) error {
	stamp := int64(NextRetryNever)
	if !nextRetryAt.IsZero() {
		stamp = nextRetryAt.Unix()
	}
	res, err := q.db.ExecContext(ctx, `
        UPDATE upload_tasks
        SET status=?, retry_count=retry_count+1, next_retry_at=?, last_error=?, updated_at=?
        WHERE id=?`,
		StatusFailed, stamp, errMsg, time.Now().Unix(), id,
	)
	if err != nil {
		return fmt.Errorf("queue: mark failed %q: %w", id, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("queue: task %q: %w", id, ErrTaskNotFound)
	}
	return nil
}

// ListByStatus returns all tasks with the given status.
func (q *Queue) ListByStatus(status string) ([]*UploadTask, error) {
	rows, err := q.db.Query(`
        SELECT id, rule_id, local_path, storage_path, bucket, upload_id,
               completed_parts, file_size, file_mtime, sha256, status,
               retry_count, next_retry_at, last_error, created_at, updated_at,
               file_offset, append_mode
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

// TailOffsets returns, for every processed file of the rule, the byte size at
// its last successful upload. processed_files.file_size is the full file size
// as of the completed upload, which is exactly where the next tail upload must
// resume. Used to rebuild the watcher's in-memory offsets after an agent
// restart (PR #100 review F3).
func (q *Queue) TailOffsets(ctx context.Context, ruleID string) (map[string]int64, error) {
	rows, err := q.db.QueryContext(ctx,
		`SELECT local_path, file_size FROM processed_files WHERE rule_id = ?`, ruleID)
	if err != nil {
		return nil, fmt.Errorf("queue: tail offsets for rule %q: %w", ruleID, err)
	}
	defer rows.Close()

	offsets := make(map[string]int64)
	for rows.Next() {
		var path string
		var size int64
		if err := rows.Scan(&path, &size); err != nil {
			return nil, fmt.Errorf("queue: scan tail offsets for rule %q: %w", ruleID, err)
		}
		offsets[path] = size
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: iterate tail offsets for rule %q: %w", ruleID, err)
	}
	return offsets, nil
}

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

// IsProcessed returns true if processed_files contains the same rule, path,
// modification time, and size as the candidate file.
func (q *Queue) IsProcessed(ctx context.Context, ruleID, localPath string, fileMtime, fileSize int64) (bool, error) {
	var count int
	err := q.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM processed_files WHERE rule_id=? AND local_path=? AND file_mtime=? AND file_size=?`,
		ruleID, localPath, fileMtime, fileSize,
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

// isDuplicateColumnError returns true for SQLite "duplicate column name" errors
// that arise when running ADD COLUMN on an already-migrated database.
func isDuplicateColumnError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "duplicate column name")
}

// scanTasks scans a rows result set into a slice of UploadTask.
func scanTasks(rows *sql.Rows) ([]*UploadTask, error) {
	var tasks []*UploadTask
	for rows.Next() {
		t := &UploadTask{}
		if err := rows.Scan(
			&t.ID, &t.RuleID, &t.LocalPath, &t.StoragePath, &t.Bucket,
			&t.UploadID, &t.CompletedParts, &t.FileSize, &t.FileMtime,
			&t.SHA256, &t.Status, &t.RetryCount, &t.NextRetryAt, &t.LastError,
			&t.CreatedAt, &t.UpdatedAt,
			&t.FileOffset, &t.AppendMode,
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
