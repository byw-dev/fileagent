// Package executor manages a worker pool that consumes upload tasks from the
// local SQLite queue, deduplicates already-processed files, and applies
// exponential-backoff retries on failure.
package executor

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/byw-dev/fileagent/agent/internal/uploader"
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// UploadFunc is the function signature for uploading a single task. The
// executor calls this for every task dequeued from the SQLite queue.
type UploadFunc func(ctx context.Context, task *queue.UploadTask) (*uploader.UploadResult, error)

// AbandonFunc releases resources a task still holds when it is given up on —
// for a multipart task that is the in-flight MinIO upload recorded in
// task.UploadID, which must be aborted or its parts leak without bound
// (IC-3 ②). It returns an error for logging only: abandonment proceeds
// regardless. A failed abort is NOT silently delegated to any ILM rule — the
// current MinIO builds do not implement AbortIncompleteMultipartUpload (IC-3
// ③) — but is retried from the durable abort outbox (queue.MultipartAbort)
// with backoff, so the failure must never wedge the executor.
type AbandonFunc func(ctx context.Context, task *queue.UploadTask) error

// ErrTerminalUpload marks an upload failure whose cause will not go away by
// retrying — e.g. a second AccessDenied after one credential refresh
// (IC-BUG-20). Tasks failing with it are failed terminally: no backoff retry,
// reported with success=false so the UI shows the reason.
var ErrTerminalUpload = errors.New("terminal upload failure")

// retryDelays defines the wait duration before each retry attempt (1-indexed).
// Indices beyond the slice length use the last value. The same ladder backs
// off the durable abort worker's retries.
var defaultRetryDelays = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	60 * time.Minute,
}

// maxRetries is the maximum number of retry attempts before permanently failing.
const maxRetries = 10

// Defaults for the durable abort outbox worker: how long a single Abort call
// may run before it is treated as failed (and retried), and how often the
// worker wakes to drain due records.
const (
	defaultAbortTimeout = 30 * time.Second
	defaultAbortPoll    = 30 * time.Second
)

// Executor manages a pool of upload workers consuming from the queue.
type Executor struct {
	workers       int
	queue         *queue.Queue
	uploader      UploadFunc
	logger        *zap.Logger
	retryDelays   []time.Duration
	queueMaxSize  int
	reportSender  ReportSender
	reportTimeout time.Duration
	reportNotify  chan struct{}
	retryMax      int
	abandon       AbandonFunc
	abortTimeout  time.Duration
	abortPoll     time.Duration

	mu      sync.Mutex
	notify  chan struct{}
	stopCh  chan struct{}
	wg      sync.WaitGroup
	retryWg sync.WaitGroup
}

// New creates an Executor with the given number of worker goroutines. queueMaxSize
// caps the number of active (non-completed) tasks retained in the local queue;
// when exceeded, Submit drops the oldest evictable (pending or failed) task.
//
// A non-positive queueMaxSize disables capacity enforcement. This is an internal
// safety default (and convenience for tests); the agent's config validation
// requires queue_max_size > 0 (agent/internal/config), so in a running agent the
// cap is always active.
func New(workers int, q *queue.Queue, uploader UploadFunc, logger *zap.Logger, queueMaxSize int) *Executor {
	if workers <= 0 {
		workers = 1
	}
	return &Executor{
		workers:      workers,
		queue:        q,
		uploader:     uploader,
		logger:       logger,
		retryDelays:  defaultRetryDelays,
		queueMaxSize: queueMaxSize,
		retryMax:     maxRetries,
		abortTimeout: defaultAbortTimeout,
		abortPoll:    defaultAbortPoll,
		reportNotify: make(chan struct{}, 1),
		notify:       make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
	}
}

// ConfigureAbandon installs the hook that aborts a task's in-flight multipart
// upload when the task is given up on (dedup-completion, retry budget
// exhausted, terminal failure, or eviction). Passing nil disables the cleanup:
// pending abort records then stay in the durable outbox (identity preserved,
// retried once a hook is configured) — but with no hook, live abandonments
// during this process are never attempted, so the hook should always be
// configured in production. Note there is no reliable ILM backstop: current
// MinIO builds do not implement AbortIncompleteMultipartUpload (IC-3 ③).
func (e *Executor) ConfigureAbandon(fn AbandonFunc) error {
	if fn == nil {
		return fmt.Errorf("executor: invalid abandon configuration")
	}
	e.abandon = fn
	return nil
}

// Submit enqueues task into the SQLite queue and signals workers. The caller
// must have set task.LocalPath, task.Bucket, task.StoragePath, and task.RuleID.
// Submit assigns a new UUID if task.ID is empty.
func (e *Executor) Submit(ctx context.Context, task *queue.UploadTask) error {
	if task.ID == "" {
		task.ID = uuid.New().String()
	}
	enqueued, err := e.queue.EnqueueIfNoActive(ctx, task)
	if err != nil {
		return fmt.Errorf("executor: enqueue task: %w", err)
	}
	if !enqueued {
		e.logger.Debug("executor: skipped duplicate active task",
			zap.String("rule_id", task.RuleID),
			zap.String("path", task.LocalPath),
			zap.Int64("file_mtime", task.FileMtime),
			zap.Int64("file_size", task.FileSize),
		)
		return nil
	}
	// Trim the queue back to the cap only after the new task is safely persisted,
	// so a failed Enqueue never costs us already-queued tasks.
	e.enforceCapacity(task.ID)
	// Signal workers non-blocking.
	select {
	case e.notify <- struct{}{}:
	default:
	}
	return nil
}

// enforceCapacity trims the local queue back to queue_max_size after a task has
// been enqueued. While the number of active (non-completed) tasks exceeds the
// cap, it drops the oldest evictable task (pending or failed) and logs a warning,
// matching the design's queue-full policy (system-design §4.6). newID is the
// just-enqueued task, which is excluded from eviction so a freshly collected file
// is never the one dropped — the queue always sheds older backlog first. If
// nothing is evictable — every other active task is running, whose count is
// bounded by the worker concurrency and so far below the cap — it warns once and
// leaves the queue slightly over the cap rather than blocking collection. A cap
// of zero or less disables enforcement.
//
// Enforcement is best-effort: the enqueue/count/evict steps are not a single
// transaction, so under concurrent Submit calls the active count may briefly
// exceed the cap. This is acceptable for a soft backpressure limit.
func (e *Executor) enforceCapacity(newID string) {
	if e.queueMaxSize <= 0 {
		return
	}
	for {
		n, err := e.queue.CountActive()
		if err != nil {
			e.logger.Warn("executor: queue capacity check failed", zap.Error(err))
			return
		}
		if n <= e.queueMaxSize {
			return
		}
		dropped, err := e.queue.DeleteOldestEvictable(newID)
		if err != nil {
			e.logger.Warn("executor: queue eviction failed", zap.Error(err))
			return
		}
		if dropped == nil {
			e.logger.Warn("executor: queue over capacity but no evictable task; keeping task anyway",
				zap.Int("queue_max_size", e.queueMaxSize),
				zap.Int("active", n),
			)
			return
		}
		e.logger.Warn("executor: queue full, dropped oldest task",
			zap.Int("queue_max_size", e.queueMaxSize),
			zap.Int("active", n),
			zap.String("dropped_task_id", dropped.ID),
			zap.String("dropped_path", dropped.LocalPath),
		)
		// A dropped failed task may carry an in-flight multipart upload that
		// would never be retried again once the row is gone.
		e.abandonTaskUpload(context.Background(), dropped)
	}
}

// Start launches the worker goroutines. It returns immediately; workers run
// until Stop is called or ctx is cancelled.
func (e *Executor) Start(ctx context.Context) {
	if e.reportSender != nil {
		if err := e.queue.ResetReportTimers(ctx); err != nil {
			e.logger.Error("executor: reset report timers", zap.Error(err))
		}
		e.wg.Add(1)
		go e.runReporter(ctx)
	}
	// The durable abort worker runs from startup so abort records written by
	// a previous process (before it exited) are drained too.
	e.wg.Add(1)
	go e.runAbortWorker(ctx)
	e.recoverFailedTasks(ctx)
	for i := 0; i < e.workers; i++ {
		e.wg.Add(1)
		go e.runWorker(ctx)
	}
}

// Stop signals all workers to stop and waits for them to finish current tasks,
// including any pending retry goroutines.
func (e *Executor) Stop() {
	close(e.stopCh)
	e.wg.Wait()
	e.retryWg.Wait()
}

// runWorker is the main loop for a single worker goroutine.
func (e *Executor) runWorker(ctx context.Context) {
	defer e.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-e.notify:
			e.drainQueue(ctx)
		case <-time.After(5 * time.Second):
			// Periodic drain to catch tasks re-queued after backoff.
			e.drainQueue(ctx)
		}
	}
}

// drainQueue dequeues and processes all available pending tasks.
func (e *Executor) drainQueue(ctx context.Context) {
	for {
		tasks, err := e.queue.DequeuePending(1)
		if err != nil {
			e.logger.Error("executor: dequeue error", zap.Error(err))
			return
		}
		if len(tasks) == 0 {
			return
		}
		e.processTask(ctx, tasks[0])
	}
}

// processTask handles deduplication, invokes the upload function, and manages
// retry scheduling.
func (e *Executor) processTask(ctx context.Context, task *queue.UploadTask) {
	// Deduplication: skip only when rule, path, mtime, and size all match.
	already, err := e.queue.IsProcessed(ctx, task.RuleID, task.LocalPath, task.FileMtime, task.FileSize)
	if err != nil {
		e.logger.Warn("executor: dedup check error", zap.String("task_id", task.ID), zap.Error(err))
	}
	if already {
		e.logger.Info("executor: skipping duplicate task",
			zap.String("task_id", task.ID),
			zap.String("path", task.LocalPath),
		)
		// P1-b/R2/B: `completed` is not only reached via
		// CompleteMultipartUpload. This task may carry an upload ID from an
		// earlier partial attempt (it failed; another task for the same file
		// version completed, which the EnqueueIfNoActive guard does not block
		// against failed rows). The completion transition and the durable
		// abort intent are ONE transaction — the same order law as the
		// terminal path: a completed row is permanent, so its abort identity
		// must be durable in the same commit, not a best-effort write after
		// it. The direct abort still runs afterwards (fast path); its record
		// is removed again on success.
		if task.UploadID != "" {
			if err := e.queue.MarkCompletedAbandoningUpload(ctx, task); err != nil {
				if !errors.Is(err, queue.ErrTaskNotFound) {
					// R2/B: the completion transition and its abort intent
					// are one transaction; if it could not commit, the task
					// is NOT completed and NOT abandoned. Fall back to the
					// normal retryable-failure path so the row lands back in
					// `failed` with a persisted schedule — schedulable by
					// THIS process (a bare return would stall it as running
					// until the next restart). The retry hits the dedup
					// shortcut again and re-attempts the atomic completion.
					e.logger.Error("executor: record dedup completion with abort failed, keeping the task retryable",
						zap.String("task_id", task.ID),
						zap.String("upload_id", task.UploadID),
						zap.Error(err))
					e.handleFailure(ctx, task, fmt.Errorf("dedup completion could not be recorded: %w", err))
					return
				}
				// Row evicted while we worked: the eviction already wrote
				// the abort record transactionally with the DELETE.
				e.abandonTaskUpload(ctx, task)
				return
			}
			e.abandonTaskUpload(ctx, task)
			return
		}
		_ = e.queue.UpdateStatus(task.ID, queue.StatusCompleted)
		return
	}

	result, err := e.uploader(ctx, task)
	if err != nil {
		e.handleFailure(ctx, task, err)
		return
	}

	if result == nil {
		e.handleFailure(ctx, task, fmt.Errorf("uploader returned nil result"))
		return
	}
	e.persistResult(ctx, task, result, nil)

}

// abandonTaskUpload aborts the in-flight multipart upload of a task that is
// being given up on (dedup-completion, retry budget exhausted, terminal
// failure, or eviction while the hook is set). The abort identity is first
// recorded durably in the queue's multipart_abort_outbox (IC-3 ②/P2): once the
// task row is gone or final, this record is the only retryable local identity
// left — there is no ILM backstop on current MinIO builds (IC-3 ③). The direct
// abort then runs with a timeout; on failure the outbox worker retries it with
// backoff, and on success the record is removed again. Tasks without an
// upload ID — single-part uploads, or tasks whose multipart upload never
// initiated — have nothing to release.
func (e *Executor) abandonTaskUpload(ctx context.Context, task *queue.UploadTask) {
	if e.abandon == nil || task == nil || task.UploadID == "" {
		return
	}
	if e.queue != nil {
		if err := e.queue.EnqueueMultipartAbort(ctx, &queue.AbortOutboxEntry{
			UploadID:    task.UploadID,
			TaskID:      task.ID,
			Bucket:      task.Bucket,
			StoragePath: task.StoragePath,
		}); err != nil {
			// Without this record a subsequent abort failure would leave an
			// orphan nobody can retry — surface it loudly.
			e.logger.Error("executor: record durable multipart abort intent failed",
				zap.String("task_id", task.ID),
				zap.String("upload_id", task.UploadID),
				zap.Error(err))
		}
	}

	actx, cancel := context.WithTimeout(ctx, e.abortTimeout)
	defer cancel()
	if err := e.abandon(actx, task); err != nil {
		e.logger.Warn("executor: abort of abandoned multipart upload failed; the durable abort record will be retried",
			zap.String("task_id", task.ID),
			zap.String("upload_id", task.UploadID),
			zap.Error(err))
		return
	}
	if e.queue != nil {
		if err := e.queue.DeleteMultipartAbort(ctx, task.UploadID); err != nil {
			e.logger.Warn("executor: remove durable abort record after successful abort",
				zap.String("task_id", task.ID),
				zap.String("upload_id", task.UploadID),
				zap.Error(err))
		}
	}
	e.logger.Info("executor: aborted multipart upload of abandoned task",
		zap.String("task_id", task.ID),
		zap.String("upload_id", task.UploadID))
}

// runAbortWorker drains the durable multipart-abort outbox until shutdown. It
// exists because a best-effort abort is not enough: MinIO may be unreachable
// at abandonment time, and no ILM rule backstop exists on current MinIO builds
// (IC-3 ③) — the durable record plus this retrying worker is the last line of
// defence against unbounded part leaks.
func (e *Executor) runAbortWorker(ctx context.Context) {
	defer e.wg.Done()
	ticker := time.NewTicker(e.abortPoll)
	defer ticker.Stop()
	for {
		e.drainAbortOutbox(ctx)
		select {
		case <-ctx.Done():
			return
		case <-e.stopCh:
			return
		case <-ticker.C:
		}
	}
}

// drainAbortOutbox attempts every due abort record, each under the abort
// timeout. Success (or a confirmed NoSuchUpload, tolerated by the abandon
// implementation) deletes the record; failure schedules the next attempt with
// the shared backoff ladder.
func (e *Executor) drainAbortOutbox(ctx context.Context) {
	if e.abandon == nil {
		// No hook configured: keep the records (identity stays retryable),
		// warn so the missing ConfigureAbandon is visible in logs.
		e.logger.Warn("executor: multipart abort records pending but no abandon hook configured")
		return
	}
	entries, err := e.queue.DueMultipartAborts(ctx, time.Now(), 100)
	if err != nil {
		e.logger.Warn("executor: list due multipart aborts", zap.Error(err))
		return
	}
	for _, entry := range entries {
		task := &queue.UploadTask{
			ID:          entry.TaskID,
			Bucket:      entry.Bucket,
			StoragePath: entry.StoragePath,
			UploadID:    entry.UploadID,
		}
		actx, cancel := context.WithTimeout(ctx, e.abortTimeout)
		err := e.abandon(actx, task)
		cancel()
		if err == nil {
			if derr := e.queue.DeleteMultipartAbort(ctx, entry.UploadID); derr != nil {
				e.logger.Warn("executor: remove durable abort record after successful abort",
					zap.String("upload_id", entry.UploadID), zap.Error(derr))
			}
			e.logger.Info("executor: aborted multipart upload from the durable abort record",
				zap.String("task_id", entry.TaskID),
				zap.String("upload_id", entry.UploadID),
				zap.Int("attempts", entry.Attempts))
			continue
		}
		next := time.Now().Add(e.abortBackoffDelay(entry.Attempts))
		if rerr := e.queue.RecordMultipartAbortAttempt(ctx, entry.UploadID, next, err.Error()); rerr != nil && !errors.Is(rerr, queue.ErrTaskNotFound) {
			e.logger.Warn("executor: record abort retry timer", zap.String("upload_id", entry.UploadID), zap.Error(rerr))
		}
		e.logger.Warn("executor: durable multipart abort failed, scheduled for retry",
			zap.String("task_id", entry.TaskID),
			zap.String("upload_id", entry.UploadID),
			zap.Time("next_attempt", next),
			zap.Error(err))
	}
}

// abortBackoffDelay returns the wait before the next abort retry. attempts is
// the number of failed attempts so far (0 → first retry uses the first delay).
// It shares the executor's retry ladder (and thus its test overrides).
func (e *Executor) abortBackoffDelay(attempts int) time.Duration {
	idx := attempts
	if idx >= len(e.retryDelays) {
		idx = len(e.retryDelays) - 1
	}
	return e.retryDelays[idx]
}

// handleFailure marks a task as failed and re-queues it with exponential
// backoff unless the retry limit has been reached. Terminal failures
// (ErrTerminalUpload) skip the retry entirely and report immediately —
// UNLESS their abandonment could not be durably recorded (see below), in
// which case the task falls back to the backoff path and stays retryable.
//
// Abandonment (retry exhausted or terminal failure) also aborts the task's
// in-flight multipart upload: a task that will never be retried again must not
// keep uploading parts on MinIO. A task that is merely awaiting backoff keeps
// its upload ID so the retry resumes rather than restarts.
//
// The backoff schedule is PERSISTED (IC-3 P1-d): MarkFailed stamps
// next_retry_at, so a process that exits during the backoff window recovers
// the task at startup (recoverFailedTasks) instead of losing it — the old
// in-memory-only retry goroutine left failed rows dead across restarts, their
// uploads neither resumed nor aborted. Abandoned tasks (give-up, terminal)
// are stamped with the never-retry sentinel so no restart resurrects them.
//
// The abandonment marking (never-retry sentinel + abort record) is one
// transaction (R2). When that marking FAILS (and the row was not evicted),
// the verdict is NOT durable — so the task is deliberately kept RETRYABLE:
// all three abandonment branches (terminal, give-up, dedup-completion) fall
// through to the backoff path, landing the row back in `failed` with a
// persisted schedule that THIS process re-schedules (B). A bare return would
// strand the row in `running`, which nothing but the next startup's
// ResetRunningToPending would ever touch. On the retry the verdict is
// re-derived and the atomic marking re-attempted; one extra attempt while the
// local DB is broken is cheaper than a stalled row or a stranded upload.
func (e *Executor) handleFailure(ctx context.Context, task *queue.UploadTask, err error) {
	e.logger.Warn("executor: task failed",
		zap.String("task_id", task.ID),
		zap.String("path", task.LocalPath),
		zap.Int("retry_count", task.RetryCount),
		zap.Error(err),
	)

	if errors.Is(err, ErrTerminalUpload) {
		e.logger.Error("executor: task failed terminally, not retrying",
			zap.String("task_id", task.ID),
			zap.String("path", task.LocalPath),
			zap.Error(err),
		)
		if merr := e.queue.MarkFailedAbandoned(ctx, task, err.Error()); merr != nil {
			if !errors.Is(merr, queue.ErrTaskNotFound) {
				// R2/B: the abandoned state and its abort record are one
				// transaction; if it could not commit, the task is NOT
				// durably recorded as abandoned. Abandoning it in memory
				// anyway would strand the upload (row says never-retry, no
				// abort record exists). Same ordering rule as the uploader's
				// first persist (P1-c): make the state trackable BEFORE
				// acting on it — so fall through to the BACKOFF path below:
				// the row lands back in `failed` with a persisted schedule,
				// schedulable again by THIS process (B: a bare return would
				// leave it running, which nothing but the next restart's
				// ResetRunningToPending would ever touch). The retry re-runs
				// the upload, re-derives the verdict, and re-attempts the
				// atomic marking — one extra attempt on a broken local DB is
				// cheaper than a stalled row or a stranded upload.
				e.logger.Error("executor: record terminal abandonment failed, keeping the task retryable",
					zap.String("task_id", task.ID),
					zap.String("upload_id", task.UploadID),
					zap.Error(merr))
				err = fmt.Errorf("%w (terminal abandonment not durably recorded: %v)", err, merr)
			} else {
				// Row evicted while we worked: the eviction already wrote the
				// abort record transactionally with the DELETE.
				task.RetryCount++
				e.abandonTaskUpload(context.Background(), task)
				e.persistResult(context.Background(), task, nil, err)
				return
			}
		} else {
			task.RetryCount++
			e.abandonTaskUpload(context.Background(), task)
			e.persistResult(context.Background(), task, nil, err)
			return
		}
	}

	newRetry := task.RetryCount + 1
	if newRetry >= e.retryMax {
		e.logger.Error("executor: task exceeded max retries, giving up",
			zap.String("task_id", task.ID),
			zap.String("path", task.LocalPath),
		)
		if merr := e.queue.MarkFailedAbandoned(ctx, task, err.Error()); merr != nil {
			if !errors.Is(merr, queue.ErrTaskNotFound) {
				// R2/B: identical reasoning to the terminal branch — no
				// durable abandoned state means no abandonment; fall through
				// to the backoff path so THIS process keeps the row
				// schedulable instead of stalling it as running. The retry
				// re-derives the give-up verdict once the marking can commit.
				e.logger.Error("executor: record give-up abandonment failed, keeping the task retryable",
					zap.String("task_id", task.ID),
					zap.String("upload_id", task.UploadID),
					zap.Error(merr))
				err = fmt.Errorf("%w (give-up abandonment not durably recorded: %v)", err, merr)
			} else {
				task.RetryCount = newRetry
				e.abandonTaskUpload(context.Background(), task)
				e.persistResult(context.Background(), task, nil, err)
				return
			}
		} else {
			task.RetryCount = newRetry
			e.abandonTaskUpload(context.Background(), task)
			e.persistResult(context.Background(), task, nil, err)
			return
		}
	}

	// Stamp the persisted retry schedule before arming the in-memory timer:
	// the timer is an optimisation, the DB row is the truth (IC-3 P1-d).
	delay := e.retryDelay(newRetry)
	nextRetryAt := time.Now().Add(delay)
	if merr := e.queue.MarkFailed(ctx, task.ID, err.Error(), nextRetryAt); merr != nil {
		e.logger.Warn("executor: stamp persisted retry schedule",
			zap.String("task_id", task.ID), zap.Error(merr))
	}
	e.logger.Info("executor: scheduling retry",
		zap.String("task_id", task.ID),
		zap.Int("attempt", newRetry),
		zap.Duration("delay", delay),
		zap.Time("next_retry_at", nextRetryAt),
	)

	// Add to the WaitGroup before starting the goroutine: calling Add inside the
	// goroutine races with Stop's retryWg.Wait() and can let Wait return early or
	// panic when the counter is momentarily zero.
	e.retryWg.Add(1)
	go func() {
		defer e.retryWg.Done()
		select {
		case <-time.After(delay):
		case <-e.stopCh:
			return
		}
		// Re-queue by resetting status to pending.
		if rerr := e.queue.UpdateStatus(task.ID, queue.StatusPending); rerr != nil {
			// A not-found row was evicted to honour queue_max_size while this
			// retry was sleeping — an expected outcome, not a failure. The row
			// is gone for good, so its in-flight multipart upload (if any) is
			// now an orphan and must be aborted here.
			if errors.Is(rerr, queue.ErrTaskNotFound) {
				e.logger.Debug("executor: retry skipped, task was evicted",
					zap.String("task_id", task.ID))
				e.abandonTaskUpload(context.Background(), task)
				return
			}
			e.logger.Warn("executor: re-queue failed", zap.String("task_id", task.ID), zap.Error(rerr))
			return
		}
		select {
		case e.notify <- struct{}{}:
		default:
		}
	}()
}

// retryDelay returns the backoff duration for the nth retry attempt (1-indexed).
func (e *Executor) retryDelay(attempt int) time.Duration {
	idx := attempt - 1
	if idx >= len(e.retryDelays) {
		idx = len(e.retryDelays) - 1
	}
	return e.retryDelays[idx]
}

// recoverFailedTasks re-queues tasks that were awaiting a backoff retry when
// the previous process exited (IC-3 P1-d). The retry schedule is persisted in
// next_retry_at (stamped by handleFailure), so recovery reads it instead of
// trusting memory: tasks already due become pending immediately;
// still-future schedules are armed as in-memory timers that flip the row when
// due. Failed tasks carrying an in-flight multipart upload keep their upload
// ID — the re-queued attempt RESUMES it; nothing is aborted here.
//
// next_retry_at has three shapes, and recovery treats each explicitly:
//   - negative (NextRetryNever): the task was abandoned (give-up / terminal
//     failure) — never resurrected. Its upload was either aborted at
//     abandonment time or is covered by the durable abort record written in
//     the same transaction as the sentinel (R2); the abort worker drains it.
//   - positive: the persisted backoff schedule — honour it.
//   - zero: a LEGACY row written by a build before the schedule was persisted
//     (the ALTER TABLE default). Such a row is UNDECIDABLE between "awaiting
//     backoff" and "abandoned on a terminal verdict" — an older build's
//     terminal give-up can carry a LOW retry count, so the retry-count guard
//     below cannot tell them apart either. Recovery errs toward abandoned: it
//     does NOT resurrect the row (re-running an abandoned upload would be
//     wrong, and an awaiting-backoff task is re-collected naturally by the
//     scan if the file still matters), and it durably records an abort intent
//     for the row's in-flight upload so it cannot leak either. No direct
//     abort happens here: recovery runs on the startup path and must not do
//     remote I/O — the abort worker drains the record with backoff.
//
// The retry-count guard remains as defence for rows whose persisted schedule
// is positive but whose retry_count has outgrown a retry_max that was lowered
// between runs.
func (e *Executor) recoverFailedTasks(ctx context.Context) {
	failed, err := e.queue.ListByStatus(queue.StatusFailed)
	if err != nil {
		e.logger.Error("executor: recover failed tasks", zap.Error(err))
		return
	}
	now := time.Now()
	recovered := 0
	for _, t := range failed {
		if t.RetryCount >= e.retryMax || t.NextRetryAt == queue.NextRetryNever {
			continue
		}
		if t.NextRetryAt == 0 {
			e.logger.Warn("executor: legacy failed row without a persisted retry schedule treated as abandoned",
				zap.String("task_id", t.ID),
				zap.String("upload_id", t.UploadID))
			if e.abandon != nil && t.UploadID != "" {
				if err := e.queue.EnqueueMultipartAbort(ctx, &queue.AbortOutboxEntry{
					UploadID:    t.UploadID,
					TaskID:      t.ID,
					Bucket:      t.Bucket,
					StoragePath: t.StoragePath,
				}); err != nil {
					e.logger.Error("executor: record durable multipart abort intent failed",
						zap.String("task_id", t.ID),
						zap.String("upload_id", t.UploadID),
						zap.Error(err))
				}
			}
			continue
		}
		due := time.Unix(t.NextRetryAt, 0)
		if !due.After(now) {
			if err := e.queue.UpdateStatus(t.ID, queue.StatusPending); err != nil {
				if !errors.Is(err, queue.ErrTaskNotFound) {
					e.logger.Warn("executor: recover failed task",
						zap.String("task_id", t.ID), zap.Error(err))
				}
				continue
			}
			recovered++
			e.signalWorkers()
			continue
		}
		e.scheduleFailedRequeue(t, due)
	}
	if recovered > 0 {
		e.logger.Info("executor: recovered failed tasks from a previous process",
			zap.Int("task_count", recovered))
	}
}

// scheduleFailedRequeue flips a failed task to pending once its persisted
// next_retry_at passes. The timer survives only this process (like the old
// retry goroutine) — but the schedule itself is durable, so if the process
// exits again the next startup recovers from the row.
func (e *Executor) scheduleFailedRequeue(t *queue.UploadTask, due time.Time) {
	e.retryWg.Add(1)
	go func() {
		defer e.retryWg.Done()
		timer := time.NewTimer(time.Until(due))
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-e.stopCh:
			return
		}
		if err := e.queue.UpdateStatus(t.ID, queue.StatusPending); err != nil {
			if !errors.Is(err, queue.ErrTaskNotFound) {
				e.logger.Warn("executor: re-queue recovered task",
					zap.String("task_id", t.ID), zap.Error(err))
			}
			return
		}
		e.signalWorkers()
	}()
}

// signalWorkers wakes one worker non-blockingly.
func (e *Executor) signalWorkers() {
	select {
	case e.notify <- struct{}{}:
	default:
	}
}
