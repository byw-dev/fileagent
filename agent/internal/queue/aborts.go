package queue

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// AbortOutboxEntry is one durable record of a MinIO multipart upload that must
// be aborted. It is written BEFORE the owning task row is removed or marked
// final, so the abort always keeps a retryable local identity (bucket, object
// key, upload ID) even when the immediate abort fails — MinIO may be
// unreachable, the process may crash, and on the current MinIO builds there is
// NO reliable ILM backstop (IC-3 ③: the AbortIncompleteMultipartUpload
// lifecycle action is rejected, or silently stripped next to an Expiration
// rule). The executor's abort worker drains this outbox with backoff.
type AbortOutboxEntry struct {
	UploadID      string
	TaskID        string
	Bucket        string
	StoragePath   string
	Attempts      int
	NextAttemptAt int64 // unix seconds; 0 = due immediately
	LastError     string
	CreatedAt     int64
}

// EnqueueMultipartAbort durably records that uploadID must be aborted. The
// record is keyed by upload ID and INSERT OR IGNORE: re-recording the same
// upload (e.g. a direct attempt racing the eviction that already wrote the
// entry) keeps the original entry and its accumulated retry state.
func (q *Queue) EnqueueMultipartAbort(ctx context.Context, e *AbortOutboxEntry) error {
	if e.CreatedAt == 0 {
		e.CreatedAt = time.Now().Unix()
	}
	_, err := q.db.ExecContext(ctx, `
        INSERT INTO multipart_abort_outbox
            (upload_id, task_id, bucket, storage_path, attempts, next_attempt_at, last_error, created_at)
        VALUES (?,?,?,?,?,?,?,?)
        ON CONFLICT(upload_id) DO NOTHING`,
		e.UploadID, e.TaskID, e.Bucket, e.StoragePath,
		e.Attempts, e.NextAttemptAt, e.LastError, e.CreatedAt,
	)
	if err != nil {
		return fmt.Errorf("queue: enqueue multipart abort %q: %w", e.UploadID, err)
	}
	return nil
}

// DueMultipartAborts returns up to limit abort records whose next attempt time
// has passed, oldest first.
func (q *Queue) DueMultipartAborts(ctx context.Context, now time.Time, limit int) ([]*AbortOutboxEntry, error) {
	rows, err := q.db.QueryContext(ctx, `
        SELECT upload_id, task_id, bucket, storage_path, attempts, next_attempt_at, last_error, created_at
        FROM multipart_abort_outbox
        WHERE next_attempt_at <= ?
        ORDER BY created_at ASC
        LIMIT ?`, now.Unix(), limit)
	if err != nil {
		return nil, fmt.Errorf("queue: list due multipart aborts: %w", err)
	}
	defer rows.Close()

	var entries []*AbortOutboxEntry
	for rows.Next() {
		e := &AbortOutboxEntry{}
		var lastError sql.NullString
		if err := rows.Scan(&e.UploadID, &e.TaskID, &e.Bucket, &e.StoragePath,
			&e.Attempts, &e.NextAttemptAt, &lastError, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("queue: scan multipart abort: %w", err)
		}
		e.LastError = lastError.String
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("queue: iterate multipart aborts: %w", err)
	}
	return entries, nil
}

// RecordMultipartAbortAttempt bumps the attempt counter, records the failure,
// and schedules the next attempt. A deleted row (the abort succeeded elsewhere
// and the record was removed concurrently) returns ErrTaskNotFound, which
// callers may treat as benign.
func (q *Queue) RecordMultipartAbortAttempt(ctx context.Context, uploadID string, nextAttemptAt time.Time, errMsg string) error {
	res, err := q.db.ExecContext(ctx, `
        UPDATE multipart_abort_outbox
        SET attempts=attempts+1, next_attempt_at=?, last_error=?
        WHERE upload_id=?`,
		nextAttemptAt.Unix(), errMsg, uploadID,
	)
	if err != nil {
		return fmt.Errorf("queue: record multipart abort attempt %q: %w", uploadID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("queue: multipart abort record %q: %w", uploadID, ErrTaskNotFound)
	}
	return nil
}

// DeleteMultipartAbort removes the abort record for an upload that was
// successfully aborted (or confirmed already gone).
func (q *Queue) DeleteMultipartAbort(ctx context.Context, uploadID string) error {
	_, err := q.db.ExecContext(ctx, `DELETE FROM multipart_abort_outbox WHERE upload_id=?`, uploadID)
	if err != nil {
		return fmt.Errorf("queue: delete multipart abort record %q: %w", uploadID, err)
	}
	return nil
}

// CountMultipartAborts returns the number of pending abort records. It exists
// for tests and operational diagnostics.
func (q *Queue) CountMultipartAborts(ctx context.Context) (int, error) {
	var n int
	if err := q.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM multipart_abort_outbox`).Scan(&n); err != nil {
		return 0, fmt.Errorf("queue: count multipart aborts: %w", err)
	}
	return n, nil
}
// MarkFailedAbandoned atomically marks a task failed with the never-retry
// sentinel (given up: terminal failure or retry budget exhausted) AND records
// the durable abort intent for its in-flight multipart upload in the same
// transaction (R2). Writing the two separately lets a crash — or a failed
// record write — strand a failed-with-sentinel row whose upload ID nobody can
// ever retry again: the row says "never resume", and no abort record exists.
// Returns ErrTaskNotFound when the row is already gone: the eviction path
// wrote the abort record transactionally together with the DELETE, so callers
// may treat that as benign.
func (q *Queue) MarkFailedAbandoned(ctx context.Context, task *UploadTask, errMsg string) error {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("queue: begin abandoned-marking transaction for %q: %w", task.ID, err)
	}
	defer func() { _ = tx.Rollback() }()
	res, err := tx.ExecContext(ctx, `
        UPDATE upload_tasks
        SET status=?, retry_count=retry_count+1, next_retry_at=?, last_error=?, updated_at=?
        WHERE id=?`,
		StatusFailed, int64(NextRetryNever), errMsg, time.Now().Unix(), task.ID,
	)
	if err != nil {
		return fmt.Errorf("queue: mark failed abandoned %q: %w", task.ID, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("queue: task %q: %w", task.ID, ErrTaskNotFound)
	}
	if task.UploadID != "" {
		if _, err := tx.ExecContext(ctx, `
            INSERT INTO multipart_abort_outbox
                (upload_id, task_id, bucket, storage_path, attempts, next_attempt_at, last_error, created_at)
            VALUES (?,?,?,?,0,0,NULL,?)
            ON CONFLICT(upload_id) DO NOTHING`,
			task.UploadID, task.ID, task.Bucket, task.StoragePath, time.Now().Unix(),
		); err != nil {
			return fmt.Errorf("queue: record abort of abandoned task %q: %w", task.ID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("queue: commit abandoned-marking of %q: %w", task.ID, err)
	}
	return nil
}
