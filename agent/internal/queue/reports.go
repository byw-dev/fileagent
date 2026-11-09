package queue

import (
	"context"
	"fmt"
	"time"
)

// Report is the durable serialized UploadResult waiting for a CP acknowledgement.
type Report struct {
	TaskID  string
	Payload []byte
}

// SaveReport atomically persists the exact result before making it reportable.
func (q *Queue) SaveReport(ctx context.Context, id string, payload []byte) error {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE upload_tasks SET status=?,updated_at=? WHERE id=? AND status IN (?,?)", StatusReported, time.Now().Unix(), id, StatusRunning, StatusFailed)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrTaskNotFound
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO upload_reports(task_id,payload,last_reported_at) VALUES(?,?,0) ON CONFLICT(task_id) DO UPDATE SET payload=excluded.payload,last_reported_at=0", id, payload)
	if err != nil {
		return err
	}
	return tx.Commit()
}

// DueReports returns unacknowledged reports whose retry deadline has passed.
func (q *Queue) DueReports(ctx context.Context, before time.Time) ([]Report, error) {
	rows, err := q.db.QueryContext(ctx, "SELECT task_id,payload FROM upload_reports WHERE last_reported_at<=? ORDER BY last_reported_at,task_id LIMIT 100", before.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var reports []Report
	for rows.Next() {
		var r Report
		if err := rows.Scan(&r.TaskID, &r.Payload); err != nil {
			return nil, err
		}
		reports = append(reports, r)
	}
	return reports, rows.Err()
}

// TouchReport starts the ack deadline before sending, avoiding a racing ack.
func (q *Queue) TouchReport(ctx context.Context, id string, now time.Time) error {
	_, err := q.db.ExecContext(ctx, "UPDATE upload_reports SET last_reported_at=? WHERE task_id=?", now.UnixMilli(), id)
	return err
}

// ResetReportTimers makes persisted reports immediately retryable at startup.
func (q *Queue) ResetReportTimers(ctx context.Context) error {
	_, err := q.db.ExecContext(ctx, "UPDATE upload_reports SET last_reported_at=0")
	return err
}

// GetReport returns the persisted result; an absent or already acked task yields ErrNoRows.
func (q *Queue) GetReport(ctx context.Context, id string) ([]byte, error) {
	var payload []byte
	err := q.db.QueryRowContext(ctx, "SELECT payload FROM upload_reports WHERE task_id=?", id).Scan(&payload)
	return payload, err
}

// CompleteReported marks a reported task completed only after a successful CP ack.
// Successful uploads also update dedup state in the same transaction; failures do not.
func (q *Queue) CompleteReported(ctx context.Context, id string, uploaded bool, sha string) error {
	return q.CompleteReportedIdentity(ctx, id, uploaded, sha, FileIdentity{}, false)
}

// CompleteReportedIdentity is CompleteReported with the IC-BUG-48 identity of
// the uploaded version. The ledger row's identity fields (mtime-ns / ctime-ns
// / dev / ino / size) and the racy anchor (uploaded_at_ns) all come from the
// uploader's same-fd PRE-READ snapshot, NOT from the enqueue-time task fields
// (those can be hours older than the bytes actually read — offline queueing).
// ok=false (identity unknown: mid-upload change, replayed report, or an
// upload path that produced none) stores a zero identity → undecidable → the
// next candidate check hash-confirms (fail-closed), never a wrong skip.
func (q *Queue) CompleteReportedIdentity(ctx context.Context, id string, uploaded bool, sha string, ident FileIdentity, ok bool) error {
	tx, err := q.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, "UPDATE upload_tasks SET status=?,sha256=?,updated_at=? WHERE id=? AND status=?", StatusCompleted, sha, time.Now().Unix(), id, StatusReported)
	if err != nil {
		return err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return nil
	}
	if uploaded {
		// Zero identity (ok=false) stays zero: the INSERT..SELECT below then
		// writes the parts_* columns' defaults (0) — undecidable by design.
		_, err = tx.ExecContext(ctx, `INSERT INTO processed_files(id,rule_id,local_path,file_size,file_mtime,sha256,uploaded_at,
           file_mtime_ns,file_ctime_ns,file_dev,file_ino,uploaded_at_ns)
   SELECT id,rule_id,local_path,?,?,sha256,?,
           ?,?,?,?,?
   FROM upload_tasks WHERE id=?
   ON CONFLICT(rule_id,local_path) DO UPDATE SET file_size=excluded.file_size,file_mtime=excluded.file_mtime,sha256=excluded.sha256,uploaded_at=excluded.uploaded_at,
           file_mtime_ns=excluded.file_mtime_ns,file_ctime_ns=excluded.file_ctime_ns,file_dev=excluded.file_dev,file_ino=excluded.file_ino,uploaded_at_ns=excluded.uploaded_at_ns`,
			ident.Size, ident.MtimeNs/1e9, time.Now().Unix(),
			ident.MtimeNs, ident.CtimeNs, ident.Dev, ident.Ino, ident.SnapAtNs, id)
		if err != nil {
			return fmt.Errorf("queue: acknowledge processed file: %w", err)
		}
	}
	_, err = tx.ExecContext(ctx, "DELETE FROM upload_reports WHERE task_id=?", id)
	if err != nil {
		return err
	}
	return tx.Commit()
}
