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
	"github.com/google/uuid"
	"go.uber.org/zap"
)

// UploadFunc is the function signature for uploading a single task. The
// executor calls this for every task dequeued from the SQLite queue.
type UploadFunc func(ctx context.Context, task *queue.UploadTask) error

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
	workers      int
	queue        *queue.Queue
	uploader     UploadFunc
	logger       *zap.Logger
	retryDelays  []time.Duration
	queueMaxSize int

	mu      sync.Mutex
	notify  chan struct{}
	stopCh  chan struct{}
	wg      sync.WaitGroup
	retryWg sync.WaitGroup
}

// New creates an Executor with the given number of worker goroutines. queueMaxSize
// caps the number of active (non-completed) tasks retained in the local queue;
// when exceeded, Submit drops the oldest evictable (pending or failed) task. A
// value <= 0 disables the cap (unbounded queue).
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
		notify:       make(chan struct{}, 1),
		stopCh:       make(chan struct{}),
	}
}

// Submit enqueues task into the SQLite queue and signals workers. The caller
// must have set task.LocalPath, task.Bucket, task.StoragePath, and task.RuleID.
// Submit assigns a new UUID if task.ID is empty.
func (e *Executor) Submit(task *queue.UploadTask) error {
	if task.ID == "" {
		task.ID = uuid.New().String()
	}
	e.enforceCapacity()
	if err := e.queue.Enqueue(task); err != nil {
		return fmt.Errorf("executor: enqueue task: %w", err)
	}
	// Signal workers non-blocking.
	select {
	case e.notify <- struct{}{}:
	default:
	}
	return nil
}

// enforceCapacity applies the configured queue_max_size cap before a new task is
// enqueued. While the number of active (non-completed) tasks meets or exceeds the
// cap, it drops the oldest evictable task (pending or failed) and logs a warning,
// matching the design's queue-full policy (system-design §4.6). If nothing is
// evictable — every active task is running, whose count is bounded by the worker
// concurrency and so far below the cap — it warns once and accepts the new task
// rather than blocking collection. A cap of zero or less disables enforcement.
//
// Enforcement is best-effort: the count/evict/enqueue steps are not a single
// transaction, so under concurrent Submit calls the active count may briefly
// exceed the cap. This is acceptable for a soft backpressure limit.
func (e *Executor) enforceCapacity() {
	if e.queueMaxSize <= 0 {
		return
	}
	for {
		n, err := e.queue.CountActive()
		if err != nil {
			e.logger.Warn("executor: queue capacity check failed", zap.Error(err))
			return
		}
		if n < e.queueMaxSize {
			return
		}
		dropped, err := e.queue.DeleteOldestEvictable()
		if err != nil {
			e.logger.Warn("executor: queue eviction failed", zap.Error(err))
			return
		}
		if dropped == nil {
			e.logger.Warn("executor: queue full but no evictable task; accepting task anyway",
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
	// Deduplication: skip if already processed with the same path+mtime+size.
	already, err := e.queue.IsProcessed(task.RuleID, task.LocalPath)
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

	if err := e.uploader(ctx, task); err != nil {
		e.handleFailure(task, err)
		return
	}

	// Success path.
	_ = e.queue.UpdateStatus(task.ID, queue.StatusCompleted)
	_ = e.queue.UpsertProcessedFile(&queue.ProcessedFile{
		ID:        uuid.New().String(),
		RuleID:    task.RuleID,
		LocalPath: task.LocalPath,
		FileSize:  task.FileSize,
		FileMtime: task.FileMtime,
		SHA256:    task.SHA256,
	})
	e.logger.Info("executor: task completed", zap.String("task_id", task.ID), zap.String("path", task.LocalPath))
}

// handleFailure marks a task as failed and re-queues it with exponential
// backoff unless the retry limit has been reached.
func (e *Executor) handleFailure(task *queue.UploadTask, err error) {
	e.logger.Warn("executor: task failed",
		zap.String("task_id", task.ID),
		zap.String("path", task.LocalPath),
		zap.Int("retry_count", task.RetryCount),
		zap.Error(err),
	)

	_ = e.queue.MarkFailed(task.ID, err.Error())

	newRetry := task.RetryCount + 1
	if newRetry >= maxRetries {
		e.logger.Error("executor: task exceeded max retries, giving up",
			zap.String("task_id", task.ID),
			zap.String("path", task.LocalPath),
		)
		return
	}

	delay := e.retryDelay(newRetry)
	e.logger.Info("executor: scheduling retry",
		zap.String("task_id", task.ID),
		zap.Int("attempt", newRetry),
		zap.Duration("delay", delay),
	)

	go func() {
		e.retryWg.Add(1)
		defer e.retryWg.Done()
		time.Sleep(delay)
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
