package worker

// RetagWorker drains the retag_jobs outbox. The API enqueues merge / batch_tag /
// rule_retag jobs so a large file_tags rewrite runs out of band of the request
// that triggered it (metadata 6c, Phase 1 — MT-5, design §P1.2(5)). MT-5a handles
// the "merge" kind (fold a typo'd tag value into its canonical value); batch_tag
// and rule-retriggered retag land in MT-5b on this same channel.

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/byw-dev/fileagent/controlplane/internal/db"
	"github.com/byw-dev/fileagent/controlplane/internal/retag"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// defaultRetagPollInterval is how often RetagWorker.Run polls for pending jobs
// when no interval is supplied. Retro-tagging is not latency-sensitive, so a
// few-second poll keeps the worker cheap; each tick drains the whole backlog.
const defaultRetagPollInterval = 5 * time.Second

// RetagJobsDB is the subset of DB queries the worker needs.
//
// ClaimNextRetagJob atomically transitions the oldest pending job to running and
// returns it, or sql.ErrNoRows when the queue is empty.
type RetagJobsDB interface {
	ClaimNextRetagJob(ctx context.Context) (*db.RetagJob, error)
	MergeTagValue(ctx context.Context, arg db.MergeTagValueParams) (int64, error)
	BatchSetFileTag(ctx context.Context, p db.BatchSetFileTagParams) (int64, error)
	BatchClearFileTag(ctx context.Context, p db.BatchClearFileTagParams) (int64, error)
	DeletePendingTagValue(ctx context.Context, id uuid.UUID, orgID uuid.UUID) (int64, error)
	MarkRetagJobDone(ctx context.Context, id uuid.UUID, affectedCount int32) error
	MarkRetagJobFailed(ctx context.Context, id uuid.UUID, lastError sql.NullString) error
	RequeueRunningRetagJobs(ctx context.Context) (int64, error)
}

// RetagWorker executes retag jobs on a single Control Plane instance (the v1
// deployment model), so no distributed lock is used beyond the claim's
// FOR UPDATE SKIP LOCKED.
type RetagWorker struct {
	db     RetagJobsDB
	logger *zap.Logger
}

// NewRetagWorker constructs a RetagWorker.
func NewRetagWorker(rdb RetagJobsDB, logger *zap.Logger) *RetagWorker {
	return &RetagWorker{db: rdb, logger: logger}
}

// DrainOnce claims and runs pending jobs until the queue is empty, returning the
// number of jobs processed. Per-job failures are recorded on the job and skipped
// so one bad job does not stall the queue.
func (w *RetagWorker) DrainOnce(ctx context.Context) int {
	processed := 0
	for {
		if ctx.Err() != nil {
			return processed // shutdown started: stop claiming new work
		}
		job, err := w.db.ClaimNextRetagJob(ctx)
		if errors.Is(err, sql.ErrNoRows) {
			return processed // queue drained
		}
		if err != nil {
			if ctx.Err() == nil {
				w.logger.Warn("retag: claim job failed", zap.Error(err))
			}
			return processed
		}
		w.runJob(ctx, job)
		processed++
	}
}

// runJob dispatches a claimed job by kind.
func (w *RetagWorker) runJob(ctx context.Context, job *db.RetagJob) {
	switch job.Kind {
	case retag.KindMerge:
		w.runMerge(ctx, job)
	case retag.KindBatchTag:
		w.runBatchTag(ctx, job)
	default:
		w.fail(ctx, job, fmt.Errorf("unknown retag job kind %q", job.Kind))
	}
}

// runMerge executes a merge job: rewrite file_tags occurrences of the queued
// value to the canonical value (writing per-file audit rows), then drop the
// pending queue entry. The rewrite+audit is a single atomic statement, so a
// failure leaves file_tags untouched and the job is marked failed.
func (w *RetagWorker) runMerge(ctx context.Context, job *db.RetagJob) {
	var spec retag.MergeSpec
	if err := json.Unmarshal(job.Spec, &spec); err != nil {
		w.fail(ctx, job, fmt.Errorf("decode merge spec: %w", err))
		return
	}
	affected, err := w.db.MergeTagValue(ctx, db.MergeTagValueParams{
		OrgID:       job.OrgID,
		TagKeyID:    spec.TagKeyID,
		FromValue:   spec.FromValue,
		ToValue:     spec.ToValue,
		ActorUserID: job.ActorUserID,
	})
	if err != nil {
		w.fail(ctx, job, fmt.Errorf("merge tag value: %w", err))
		return
	}
	// The queued value is now folded into the canonical value, so it should leave
	// the review queue. Best-effort: a leftover pending row is harmless (a re-run
	// of an already-merged value rewrites nothing) and must not fail the job.
	if _, err := w.db.DeletePendingTagValue(ctx, spec.PendingID, job.OrgID); err != nil {
		if ctx.Err() == nil {
			w.logger.Warn("retag merge: delete pending value failed",
				zap.String("job_id", job.ID.String()), zap.Error(err))
		}
	}
	if err := w.db.MarkRetagJobDone(ctx, job.ID, clampInt32(affected)); err != nil {
		// The merge already applied (and is idempotent). Leaving the job in
		// `running` would strand it — ClaimNextRetagJob only picks `pending`, so it
		// is never retried, and /retag-jobs polling hangs. Record the bookkeeping
		// failure so the state is observable and an operator can recover.
		w.fail(ctx, job, fmt.Errorf("merge applied but marking job done failed: %w", err))
		return
	}
	w.logger.Info("retag merge done",
		zap.String("job_id", job.ID.String()),
		zap.String("from", spec.FromValue),
		zap.String("to", spec.ToValue),
		zap.Int64("affected", affected))
}

// Batch tagging writes file_tags.source=manual and tag_audit action/source to
// mirror single-file manual tagging (it is an admin action, just applied in bulk
// and executed asynchronously).
const (
	batchTagSource   = "manual"
	batchActionSet   = "set"
	batchActionClear = "clear"
	batchAuditSource = "manual"
)

// runBatchTag executes a batch_tag job: apply each key's set/clear to every file
// matching the spec's filter, summing changed-file counts across keys. Governance
// (registered key, pending queue for unregistered controlled values) is enforced
// at enqueue, so the executor only applies. A key is processed at most once.
func (w *RetagWorker) runBatchTag(ctx context.Context, job *db.RetagJob) {
	var spec retag.BatchTagSpec
	if err := json.Unmarshal(job.Spec, &spec); err != nil {
		w.fail(ctx, job, fmt.Errorf("decode batch_tag spec: %w", err))
		return
	}
	filter, err := batchFilter(job.OrgID, spec.Filter)
	if err != nil {
		w.fail(ctx, job, fmt.Errorf("batch_tag filter: %w", err))
		return
	}

	var total int64
	// Deterministic order so audit/logs are stable and tests are reproducible.
	keys := make([]string, 0, len(spec.Tags))
	for k := range spec.Tags {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, key := range keys {
		val := spec.Tags[key]
		var n int64
		var err error
		if val == nil {
			n, err = w.db.BatchClearFileTag(ctx, db.BatchClearFileTagParams{
				Filter: filter, Key: key,
				Action: batchActionClear, AuditSource: batchAuditSource, ActorUserID: job.ActorUserID,
			})
		} else {
			n, err = w.db.BatchSetFileTag(ctx, db.BatchSetFileTagParams{
				Filter: filter, Key: key, Value: *val, Source: batchTagSource,
				Action: batchActionSet, AuditSource: batchAuditSource, ActorUserID: job.ActorUserID,
			})
		}
		if err != nil {
			w.fail(ctx, job, fmt.Errorf("batch_tag apply key %q: %w", key, err))
			return
		}
		total += n
	}
	if err := w.db.MarkRetagJobDone(ctx, job.ID, clampInt32(total)); err != nil {
		w.fail(ctx, job, fmt.Errorf("batch applied but marking job done failed: %w", err))
		return
	}
	w.logger.Info("retag batch_tag done",
		zap.String("job_id", job.ID.String()),
		zap.Int("keys", len(keys)), zap.Int64("changed", total))
}

// batchFilter converts a JSON-friendly BatchTagFilter (org from the job) into the
// db selection filter, parsing UUIDs/status/tag predicates. Malformed values are
// errors (they are validated at enqueue, so this is a defensive guard).
func batchFilter(orgID uuid.UUID, f retag.BatchTagFilter) (db.BatchTagFilter, error) {
	out := db.BatchTagFilter{OrgID: orgID}
	parseNU := func(s string) (uuid.NullUUID, error) {
		if s == "" {
			return uuid.NullUUID{}, nil
		}
		id, err := uuid.Parse(s)
		if err != nil {
			return uuid.NullUUID{}, err
		}
		return uuid.NullUUID{UUID: id, Valid: true}, nil
	}
	var err error
	if out.AgentID, err = parseNU(f.AgentID); err != nil {
		return out, fmt.Errorf("agent_id: %w", err)
	}
	if out.BucketID, err = parseNU(f.BucketID); err != nil {
		return out, fmt.Errorf("bucket_id: %w", err)
	}
	if out.FileTypeID, err = parseNU(f.FileTypeID); err != nil {
		return out, fmt.Errorf("file_type_id: %w", err)
	}
	if f.Status != "" {
		st := db.FileStatus(f.Status)
		if !st.Valid() {
			return out, fmt.Errorf("invalid status %q", f.Status)
		}
		out.Status = db.NullFileStatus{FileStatus: st, Valid: true}
	}
	for _, t := range f.Tags {
		out.Tags = append(out.Tags, db.FileTagFilter{Key: t.Key, Value: t.Value})
	}
	return out, nil
}

// clampInt32 caps a non-negative row count to MaxInt32 before it is stored in the
// int-typed retag_jobs.affected_count, so an extreme batch cannot overflow into a
// negative/incorrect count. Realistic batches never approach this bound.
func clampInt32(n int64) int32 {
	if n > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(n)
}

// fail records cause on the job. It is a no-op on shutdown so a cancelled context
// does not overwrite a job with a spurious error.
func (w *RetagWorker) fail(ctx context.Context, job *db.RetagJob, cause error) {
	if ctx.Err() != nil {
		return
	}
	w.logger.Warn("retag job failed",
		zap.String("job_id", job.ID.String()),
		zap.String("kind", job.Kind), zap.Error(cause))
	if err := w.db.MarkRetagJobFailed(ctx, job.ID,
		sql.NullString{String: cause.Error(), Valid: true}); err != nil {
		w.logger.Error("retag: mark job failed errored",
			zap.String("job_id", job.ID.String()), zap.Error(err))
	}
}

// requeueStale resets jobs left in `running` back to `pending` so they are
// re-claimed. Safe on a single-instance CP because a just-started worker has
// nothing in flight; merge execution is idempotent, so re-running is harmless.
func (w *RetagWorker) requeueStale(ctx context.Context) {
	n, err := w.db.RequeueRunningRetagJobs(ctx)
	if err != nil {
		if ctx.Err() == nil {
			w.logger.Warn("retag: requeue stale running jobs failed", zap.Error(err))
		}
		return
	}
	if n > 0 {
		w.logger.Info("retag: requeued stale running jobs", zap.Int64("count", n))
	}
}

// Run drains the queue on a ticker until ctx is cancelled. A non-positive
// interval falls back to defaultRetagPollInterval. It drains once on startup so a
// backlog left by a restart is handled without waiting for the first tick.
func (w *RetagWorker) Run(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = defaultRetagPollInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	w.logger.Info("retag worker started", zap.Duration("interval", interval))
	// Recover jobs stranded in `running` by a previous stopped/crashed process
	// (single-instance CP, so nothing is in-flight at startup) before draining.
	w.requeueStale(ctx)
	w.DrainOnce(ctx)
	for {
		select {
		case <-ctx.Done():
			w.logger.Info("retag worker stopped")
			return
		case <-ticker.C:
			if ctx.Err() != nil {
				w.logger.Info("retag worker stopped")
				return
			}
			w.DrainOnce(ctx)
		}
	}
}
