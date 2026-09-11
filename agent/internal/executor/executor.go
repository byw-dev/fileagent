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

// ErrTerminalUpload marks an upload failure whose cause will not go away by
// retrying — e.g. a second AccessDenied after one credential refresh
// (IC-BUG-20). Tasks failing with it are failed terminally: no backoff retry,
// reported with success=false so the UI shows the reason.
var ErrTerminalUpload = errors.New("terminal upload failure")

// retryDelays defines the wait duration before each retry attempt (1-indexed).
// Indices beyond the slice length use the last value.
var defaultRetryDelays = []time.Duration{
	1 * time.Minute,
	5 * time.Minute,
	15 * time.Minute,
	60 * time.Minute,
}

// maxRetries is the maximum number of retry attempts before permanently failing.
const maxRetries = 10

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
		retryMax:     maxRetries, reportNotify: make(chan struct{}, 1),
		notify: make(chan struct{}, 1),
		stopCh: make(chan struct{}),
	}
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
		_ = e.queue.UpdateStatus(task.ID, queue.StatusCompleted)
		return
	}

	result, err := e.uploader(ctx, task)
	if err != nil {
		e.handleFailure(task, err)
		return
	}

	if result == nil {
		e.handleFailure(task, fmt.Errorf("uploader returned nil result"))
		return
	}
	e.persistResult(ctx, task, result, nil)

}

// handleFailure marks a task as failed and re-queues it with exponential
// backoff unless the retry limit has been reached. Terminal failures
// (ErrTerminalUpload) skip the retry entirely and report immediately.
func (e *Executor) handleFailure(task *queue.UploadTask, err error) {
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
		_ = e.queue.MarkFailed(task.ID, err.Error())
		task.RetryCount++
		e.persistResult(context.Background(), task, nil, err)
		return
	}

	_ = e.queue.MarkFailed(task.ID, err.Error())

	newRetry := task.RetryCount + 1
	if newRetry >= e.retryMax {
		e.logger.Error("executor: task exceeded max retries, giving up",
			zap.String("task_id", task.ID),
			zap.String("path", task.LocalPath),
		)
		task.RetryCount = newRetry
		e.persistResult(context.Background(), task, nil, err)
		return
	}

	delay := e.retryDelay(newRetry)
	e.logger.Info("executor: scheduling retry",
		zap.String("task_id", task.ID),
		zap.Int("attempt", newRetry),
		zap.Duration("delay", delay),
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
			// retry was sleeping — an expected outcome, not a failure.
			if errors.Is(rerr, queue.ErrTaskNotFound) {
				e.logger.Debug("executor: retry skipped, task was evicted",
					zap.String("task_id", task.ID))
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
