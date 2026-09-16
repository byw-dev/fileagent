// Package watcher monitors file system directories and emits FileEvents for
// new or modified files. It uses fsnotify for real-time notifications and
// falls back to periodic polling when fsnotify is unavailable (watch
// creation or watch-path registration fails).
package watcher

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"

	"github.com/byw-dev/fileagent/agent/internal/queue"
)

var newFSWatcher = fsnotify.NewWatcher

// FileEvent represents a file system change detected by the Watcher.
type FileEvent struct {
	// Path is the absolute path of the affected file.
	Path string
	// ModTime is the file's modification time.
	ModTime time.Time
	// Size is the file size in bytes.
	Size int64
	// Op describes the operation: "create", "write", or "remove".
	Op string
	// FileOffset is the byte offset from which new content starts. Non-zero
	// only in "tail" append mode. Zero means upload from the beginning.
	FileOffset int64
}

// Watcher monitors a directory and emits FileEvents on a channel.
type Watcher struct {
	sourcePath   string
	fileGlob     string
	recursive    bool
	pollInterval time.Duration
	appendMode   string // "overwrite" | "tail" | "close_wait"
	logger       *zap.Logger

	// tailOffsets tracks the last known byte offset per file for tail mode.
	tailOffsets map[string]int64

	// rechecks holds the one-shot close_wait debounce recheck timers keyed by
	// path (IC-BUG-43). At most one live timer per path: re-arming replaces
	// the previous timer. recheckGens carries the per-path generation used
	// by settleRecheck to tell "my own entry" from "a replacement's entry"
	// (codex review P2) — a fired callback waiting on seenMu must not delete
	// the timer that replaced it. recheckGen is the monotonic counter.
	// All three guarded by seenMu; lazily initialized so a Watcher built by
	// struct literal (tests) works without New.
	rechecks    map[string]*time.Timer
	recheckGens map[string]uint64
	recheckGen  uint64

	// seenMu guards rechecks and every access to a scan's seen map while
	// debounce recheck callbacks (timer goroutines) may run concurrently
	// with a scan on the event-loop goroutine.
	seenMu sync.Mutex

	// inflight tracks paths whose delivery is currently in progress, keyed
	// by the mtime being delivered (PR #108 review F3). It is the exactly-
	// once gate for a file version across the goroutines that all run
	// "check seen → send → record seen": the close_wait debounce flush, the
	// debounce recheck and the overflow-rescan scan. Guarded by seenMu;
	// lazily initialized so a Watcher built by struct literal works.
	inflight map[string]time.Time
}

// Append-mode constants. The canonical values live in the queue package (they
// are persisted in upload_tasks.append_mode); these aliases keep the watcher's
// public API stable.
const (
	// AppendModeOverwrite means upload the full file on each change (default mode).
	AppendModeOverwrite = queue.AppendModeOverwrite
	// AppendModeTail tracks the byte offset of each file and uploads only the
	// bytes added since the last successful upload.
	//
	// ⚠️ IC-BUG-46: tail is fail-closed blocked — the incremental tail upload
	// path would replace the whole object with only the appended bytes,
	// silently losing previously collected data. The executor refuses tail
	// tasks before any upload runs; the correct implementation is IC-15.
	AppendModeTail = queue.AppendModeTail
	// AppendModeCloseWait debounces Write/Create events by waiting a short idle
	// period before emitting, approximating "file was closed after writing".
	AppendModeCloseWait = queue.AppendModeCloseWait
)

// closeWaitDebounce is the idle period used in close_wait mode.
const closeWaitDebounce = 500 * time.Millisecond

// New creates a Watcher for the given source directory.
// fileGlob is matched against file base names (e.g. "*.log").
// If recursive is true, subdirectories are watched as well.
// pollInterval controls the fallback polling cadence (default 30 s when 0).
// appendMode controls append-mode behaviour: "overwrite", "tail", or "close_wait".
func New(sourcePath, fileGlob string, recursive bool, pollInterval time.Duration, appendMode string, logger *zap.Logger) (*Watcher, error) {
	if pollInterval <= 0 {
		pollInterval = 30 * time.Second
	}
	return &Watcher{
		sourcePath:   sourcePath,
		fileGlob:     fileGlob,
		recursive:    recursive,
		pollInterval: pollInterval,
		appendMode:   appendMode,
		logger:       logger,
		tailOffsets:  make(map[string]int64),
	}, nil
}

// SeedTailOffsets pre-loads per-file byte offsets recovered from persisted
// state (processed_files) so that tail-mode events emitted after an agent
// restart resume from where the last upload finished instead of re-sending
// the whole file (PR #100 review F3). Only meaningful in tail mode; merging
// into the existing map keeps any offsets recorded earlier in this process.
func (w *Watcher) SeedTailOffsets(offsets map[string]int64) {
	for path, off := range offsets {
		w.tailOffsets[path] = off
	}
}

// Start begins watching the source directory and emits FileEvents on events.
// It first attempts to use fsnotify; if adding the watch path fails, it
// transparently falls back to periodic polling. Start blocks until ctx is
// cancelled.
func (w *Watcher) Start(ctx context.Context, events chan<- FileEvent) error {
	fw, err := newFSWatcher()
	if err != nil {
		w.logger.Warn("watcher: fsnotify unavailable, using polling", zap.Error(err))
		return w.runPolling(ctx, events)
	}
	defer fw.Close()
	// PR #108 review F2: drop every debounce recheck timer when the watcher
	// shuts down, whatever path returns below.
	defer w.stopAllRechecks()

	if err := w.addWatchPaths(fw); err != nil {
		w.logger.Warn("watcher: cannot add watch paths, using polling", zap.Error(err))
		return w.runPolling(ctx, events)
	}

	// Use the same scan as the polling fallback so files that predate watcher
	// startup are collected consistently on both paths. Register watches first
	// so changes made during the scan are still observed by fsnotify.
	//
	// seen is shared with the event loop below so that the IC-BUG-44
	// safety-net rescan does not re-emit unchanged files as "create", and so
	// files emitted by an IC-BUG-43 debounce recheck are not re-emitted by a
	// later rescan.
	seen := make(map[string]time.Time)
	w.scheduleDebounceRechecks(ctx, events, seen, w.pollScan(ctx, events, seen))

	w.logger.Info("watcher: fsnotify started", zap.String("path", w.sourcePath))
	if w.appendMode == AppendModeCloseWait {
		return w.runCloseWait(ctx, events, fw, seen)
	}
	return w.runFsnotify(ctx, events, fw, seen)
}

// addWatchPaths registers the source path (and subdirectories when recursive)
// with the fsnotify Watcher.
func (w *Watcher) addWatchPaths(fw *fsnotify.Watcher) error {
	if !w.recursive {
		return fw.Add(w.sourcePath)
	}
	return filepath.WalkDir(w.sourcePath, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return fw.Add(path)
		}
		return nil
	})
}

// runCloseWait drives the fsnotify event loop in close_wait mode. Write and
// Create events are debounced: a per-file timer is reset on every event, and
// the FileEvent is emitted only once the timer fires (i.e., once writes stop
// for at least closeWaitDebounce). This approximates "file closed after write"
// on platforms that do not expose a native close-write notification.
//
// Errors on fw.Errors (including watch-queue overflows) are handled by
// handleWatchError, which triggers the IC-BUG-44 safety-net rescan on
// overflow so files whose events were lost are recovered by mtime.
func (w *Watcher) runCloseWait(ctx context.Context, events chan<- FileEvent, fw *fsnotify.Watcher, seen map[string]time.Time) error {
	return w.loopCloseWait(ctx, events, seen, fw.Events, fw.Errors)
}

// closeWaitPending is one file's in-flight close_wait debounce state.
type closeWaitPending struct {
	timer *time.Timer
	// mu guards op: the loop goroutine updates it on every event while a
	// firing timer goroutine reads it for the flush.
	mu sync.Mutex
	op string
}

// loopCloseWait is the close_wait event loop proper. The event and error
// channels are parameters so tests can drive the loop deterministically
// without a live fsnotify backend (whose channel lifecycle would race with
// test-side injections).
func (w *Watcher) loopCloseWait(ctx context.Context, events chan<- FileEvent, seen map[string]time.Time, evc <-chan fsnotify.Event, erc <-chan error) error {
	pending := make(map[string]*closeWaitPending)

	flush := func(path, op string) {
		fe, err := w.buildEvent(path, op)
		if err != nil {
			return
		}
		// Exactly-once arbitration for the same file version (PR #108
		// review F3): the debounce recheck and this flush can race on the
		// same mtime; the claim makes exactly one of them deliver.
		//
		// Skipping on a lost claim does NOT orphan the file: the claimant
		// either delivers it (done) or rolls its claim back — and a
		// rollback happens only when emitBlocking fails, which only happens
		// on ctx cancellation, i.e. the shutdown path (agent stop / rule
		// cancel / hot reload). There is no periodic scan in close_wait
		// mode, so "the next scan retries" does not exist at runtime; the
		// shutdown closes the loop instead: after a reload/restart the new
		// Watcher starts with a fresh seen map and its initial scan
		// re-discovers the file (scheduling a fresh recheck if it is still
		// hot), and a cancelled rule leaves the file with no rule to belong
		// to. See completeDelivery for the same reasoning.
		if !w.claimDelivery(seen, path, fe.ModTime) {
			return
		}
		delivered := false
		defer func() { w.completeDelivery(seen, path, fe.ModTime, delivered) }()
		// IC-BUG-47: blocking send — a dropped event here means the file is
		// never collected (mtime/size never change again). Backpressure
		// delays the debounce-flush goroutine instead.
		if w.emitBlocking(ctx, events, fe) {
			delivered = true
		}
	}

	for {
		select {
		case <-ctx.Done():
			// Cancel all pending timers before returning.
			stopPendingTimers(pending)
			return ctx.Err()
		case ev, ok := <-evc:
			if !ok {
				stopPendingTimers(pending)
				return nil
			}
			if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
				if w.matchGlob(ev.Name) {
					// Remove events are immediate (no debounce needed).
					if p, ok := pending[ev.Name]; ok {
						p.timer.Stop()
						delete(pending, ev.Name)
					}
					// IC-BUG-47: blocking send — never drop.
					w.emitBlocking(ctx, events, FileEvent{Path: ev.Name, Op: "remove"})
				}
				continue
			}
			if !ev.Has(fsnotify.Create) && !ev.Has(fsnotify.Write) {
				continue
			}
			if !w.matchGlob(ev.Name) {
				continue
			}
			op := opString(ev)
			if p, ok := pending[ev.Name]; ok {
				// Reset existing timer.
				p.timer.Reset(closeWaitDebounce)
				p.mu.Lock()
				p.op = op
				p.mu.Unlock()
			} else {
				path := ev.Name // capture for closure
				p := &closeWaitPending{op: op}
				p.timer = time.AfterFunc(closeWaitDebounce, func() {
					p.mu.Lock()
					op := p.op
					p.mu.Unlock()
					flush(path, op)
				})
				pending[path] = p
			}
		case err, ok := <-erc:
			if !ok {
				stopPendingTimers(pending)
				return nil
			}
			w.handleWatchError(ctx, events, seen, err)
		}
	}
}

// runFsnotify drives the fsnotify event loop, translating raw events into
// FileEvents and emitting them on the events channel.
//
// IC-BUG-47: both send sites below use emitBlocking. The earlier non-blocking
// emit dropped events whenever the consumer channel (buffer 64) was full — a
// dropped create/write event means that file is never collected, because its
// mtime/size do not change again and no further event fires. Backpressure is
// the correct semantics here, as it already is for pollScan (PR #100 F1) and
// runCloseWait.
//
// Known trade-off (IC-BUG-47 + IC-BUG-44, accepted): while the send blocks,
// fw.Events is not drained, so fsnotify's backend stops reading the kernel
// watch queue; a sustained burst can overflow it. Overflow is visible on both
// production platforms: Linux (inotify) reports it as IN_Q_OVERFLOW on
// fw.Errors, and Windows (production, ReadDirectoryChangesW backend in
// fsnotify v1.8.0) surfaces a buffer overflow as fsnotify.ErrEventOverflow on
// fw.Errors — both are the same sentinel, fsnotify.ErrEventOverflow. When
// that happens, handleWatchError triggers a safety-net rescan that recovers
// the files missed during the overflow window. Only macOS kqueue (dev
// machines, not production) may drop the overflow silently.
func (w *Watcher) runFsnotify(ctx context.Context, events chan<- FileEvent, fw *fsnotify.Watcher, seen map[string]time.Time) error {
	return w.loopFsnotify(ctx, events, seen, fw.Events, fw.Errors)
}

// loopFsnotify is the plain (non-close_wait) fsnotify event loop proper. The
// event and error channels are parameters so tests can drive the loop
// deterministically without a live fsnotify backend (whose channel lifecycle
// would race with test-side injections).
func (w *Watcher) loopFsnotify(ctx context.Context, events chan<- FileEvent, seen map[string]time.Time, evc <-chan fsnotify.Event, erc <-chan error) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-evc:
			if !ok {
				return nil
			}
			if ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) {
				if fe, err := w.buildEvent(ev.Name, opString(ev)); err == nil {
					// PR #108 review F1 → P1 (codex 复审): the real-time
					// path deliberately does NOT write seen. Seen means
					// "delivered", and emitBlocking=true only proves the
					// event entered the in-memory channel — the downstream
					// submit can still fail (only Warn-logged, see
					// IC-BUG-53). Marking seen here would take away the
					// overflow rescan's retry opportunity for exactly those
					// files, turning a transient downstream failure into a
					// permanent silent miss for the watcher's lifetime.
					// Trade-off, deliberately accepted: after an overflow
					// the rescan re-emits files that were already delivered
					// in real time. That amplification is BOUNDED — one
					// rescan round per overflow, downstream IsProcessed
					// (rule+path+mtime+size) deduplicates, and the known
					// amplification point is IC-BUG-42 (EnqueueIfNoActive
					// does not block `failed`) — while keeping markSeen
					// would be UNBOUNDED silent loss. Trading unbounded for
					// bounded is what this fix is for.
					//
					// Asymmetry vs. close_wait, on purpose: the close_wait
					// flush/recheck/scan path keeps recording seen because
					// (1) the exactly-once claim arbitration (F3) depends on
					// the seen write, and (2) that path's "no retry after a
					// downstream failure" behaviour predates this knife —
					// it is not a regression introduced here. Only the
					// real-time path's behaviour changed (F1 added markSeen,
					// which P1 reverts).
					//
					// NOTE (PR #108 review F3): this loop deliberately does
					// NOT participate in claimDelivery/completeDelivery. In
					// non-close_wait mode there are no debounce rechecks and
					// no flush, and handleWatchError (→ safetyNetRescan →
					// pollScan) is invoked from THIS select loop, so the
					// real-time delivery and the rescan's scanFile run on
					// the same goroutine, serially — the non-atomic
					// "check seen → send → record seen" never races here.
					// If the rescan is ever moved to its own goroutine, it
					// MUST be routed through claimDelivery like every other
					// delivery site, or the exactly-once guarantee breaks.
					w.emitBlocking(ctx, events, fe)
				}
			}
			if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
				if w.matchGlob(ev.Name) {
					w.emitBlocking(ctx, events, FileEvent{Path: ev.Name, Op: "remove"})
				}
			}
		case err, ok := <-erc:
			if !ok {
				return nil
			}
			w.handleWatchError(ctx, events, seen, err)
		}
	}
}

// runPolling scans the source directory on every pollInterval tick and emits
// FileEvents for files that are new or have been modified since the last scan.
func (w *Watcher) runPolling(ctx context.Context, events chan<- FileEvent) error {
	seen := make(map[string]time.Time)
	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	// Initial scan.
	w.pollScan(ctx, events, seen)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			w.pollScan(ctx, events, seen)
		}
	}
}

// pollScan walks the source directory and emits events for new/changed files.
// It returns the paths that were skipped because they are still inside the
// close_wait debounce window (empty outside close_wait mode). On the polling
// path the caller ignores the result — the next tick re-checks those files
// anyway; on the fsnotify path the caller arms one-shot recheck timers for
// them so a file whose writer exited during the window is still collected
// (IC-BUG-43).
func (w *Watcher) pollScan(ctx context.Context, events chan<- FileEvent, seen map[string]time.Time) []string {
	var skipped []string
	walk := func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !w.matchGlob(path) {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		// close_wait must never emit a file that is still being written: the
		// scan path bypasses the debounce timer in runCloseWait, so emitting
		// here would upload a truncated file under its own {time} storage key
		// that the later full upload never overwrites (PR #100 review F2).
		// The path is returned to the caller: on the fsnotify path a one-shot
		// recheck re-examines it once the debounce window has passed
		// (IC-BUG-43); on the polling path the next tick does.
		if w.appendMode == AppendModeCloseWait && time.Since(info.ModTime()) < closeWaitDebounce {
			skipped = append(skipped, path)
			return nil
		}
		return w.scanFile(ctx, events, seen, path, info)
	}

	if w.recursive {
		_ = filepath.WalkDir(w.sourcePath, walk)
	} else {
		entries, err := os.ReadDir(w.sourcePath)
		if err != nil {
			w.logger.Warn("watcher: read dir error", zap.Error(err))
			return skipped
		}
		for _, d := range entries {
			if d.IsDir() {
				continue
			}
			if walk(filepath.Join(w.sourcePath, d.Name()), d, nil) != nil {
				return skipped // ctx cancelled, stop scanning
			}
		}
	}
	return skipped
}

// scanFile emits an event for a single file if it is new or has changed since
// it was last recorded in seen. It is the per-file core shared by pollScan
// and the debounce recheck (IC-BUG-43). seen may be touched concurrently by
// recheck timer goroutines, so every access is guarded by seenMu; the emit
// itself happens outside the lock (emitBlocking must not hold it).
func (w *Watcher) scanFile(ctx context.Context, events chan<- FileEvent, seen map[string]time.Time, path string, info os.FileInfo) error {
	w.seenMu.Lock()
	prev, known := seen[path]
	w.seenMu.Unlock()
	if known && !info.ModTime().After(prev) {
		return nil
	}
	op := "write"
	if !known {
		op = "create"
	}

	var offset int64
	if w.appendMode == AppendModeTail {
		offset = w.tailOffsets[path]
	}

	fe := FileEvent{
		Path:       path,
		ModTime:    info.ModTime(),
		Size:       info.Size(),
		Op:         op,
		FileOffset: offset,
	}
	// Exactly-once arbitration for the same file version (PR #108 review
	// F3): a rescan, a recheck and the debounce flush all funnel through
	// here or through flush. completeDelivery runs via defer so a panic or
	// an early return can never leak the claim.
	if !w.claimDelivery(seen, path, info.ModTime()) {
		return nil // already delivered, or same/newer version in flight
	}
	// Mark as seen and record the tail offset only after the event has
	// been delivered: if the send is aborted (ctx cancelled) or would
	// drop the event, the next scan must retry the file instead of
	// silently skipping it forever (PR #100 review F1). The seen write
	// itself is done by completeDelivery (monotonic) below.
	// The defer guarantees the claim is settled on EVERY exit path —
	// including panics and future early returns. A leaked claim would make
	// every future claim for the same version fail and a file that never
	// changes again would silently never be collected.
	delivered := false
	defer func() { w.completeDelivery(seen, path, info.ModTime(), delivered) }()
	if !w.emitBlocking(ctx, events, fe) {
		return errWalkAborted
	}
	delivered = true
	if w.appendMode == AppendModeTail {
		w.tailOffsets[path] = info.Size()
	}
	return nil
}

// markSeen records the delivered mtime in the shared seen map. seen may be
// touched concurrently by recheck timer goroutines, so every access is
// guarded by seenMu. Callers must only call it after the event has actually
// been delivered: an aborted send must leave the file retryable (PR #100 F1).
func (w *Watcher) markSeen(seen map[string]time.Time, path string, modTime time.Time) {
	w.seenMu.Lock()
	seen[path] = modTime
	w.seenMu.Unlock()
}

// claimDelivery atomically claims the right to deliver one version (mtime)
// of path — PR #108 review F3. Several goroutines run the same non-atomic
// "check seen → send → record seen" sequence for the same file: the
// close_wait debounce flush (timer goroutine), the debounce recheck (timer
// goroutine) and the overflow-rescan scan (event-loop goroutine). Without
// arbitration two of them can deliver the same version twice. The claim
// does the seen check and the placeholder insert in ONE locked section;
// the send itself must happen OUTSIDE seenMu (emitBlocking can block, and
// holding the lock across it would stall the whole watcher).
//
// Returns false — meaning "do not deliver" — when:
//   - seen already holds this or a newer mtime (already delivered), or
//   - another goroutine holds the claim for this or a newer mtime (its
//     owner delivers); an OLDER in-flight version is superseded, because
//     the file has moved on and the newer version wins.
//
// The caller MUST pair the claim with completeDelivery via defer, so panics
// and early returns can never leave a stuck claim: a leaked claim would
// make every future claim for the same version fail and a file that never
// changes again would silently never be collected.
func (w *Watcher) claimDelivery(seen map[string]time.Time, path string, modTime time.Time) bool {
	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	if prev, known := seen[path]; known && !modTime.After(prev) {
		return false // already delivered at this or a newer version
	}
	if cur, busy := w.inflight[path]; busy && !modTime.After(cur) {
		return false // same or newer version already in flight; its owner delivers
	}
	if w.inflight == nil {
		w.inflight = make(map[string]time.Time)
	}
	w.inflight[path] = modTime
	return true
}

// completeDelivery settles a claim made by claimDelivery: it releases the
// in-flight placeholder — only if it still belongs to this claim, so a
// superseded claim cannot disturb its replacement — and, when delivered,
// records the mtime in seen monotonically (a late older-version delivery
// must never move the seen cursor backwards). delivered=false only rolls
// the placeholder back; the file stays retryable (PR #100 F1).
//
// Rollback (delivered=false) happens only when emitBlocking fails, and
// emitBlocking fails only on ctx cancellation — i.e. on the shutdown path
// (agent stop / rule cancel / hot reload). There is no periodic scan in
// close_wait mode, so "the next scan retries it" is NOT available at
// runtime; what actually closes the loop is the shutdown itself: after a
// reload or restart the new Watcher starts with a fresh seen map and its
// initial scan re-discovers the file (scheduling a fresh recheck if it is
// still hot), and if the rule was cancelled the file has no rule left to
// belong to. So no file is orphaned by a rolled-back claim.
func (w *Watcher) completeDelivery(seen map[string]time.Time, path string, modTime time.Time, delivered bool) {
	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	if delivered {
		if prev, known := seen[path]; !known || modTime.After(prev) {
			seen[path] = modTime
		}
	}
	if cur, busy := w.inflight[path]; busy && cur == modTime {
		delete(w.inflight, path)
	}
}

// stopAllRechecks stops and forgets every pending debounce recheck timer.
// Called when Start returns (rule cancelled / hot reload): the timer
// closures otherwise keep the watcher, the seen map, the context and the
// events channel alive per skipped path — with a future mtime the wait can
// be arbitrarily long. The events channel itself is never closed in
// production, so the consequence is retention, not a send-after-close panic.
func (w *Watcher) stopAllRechecks() {
	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	for path, t := range w.rechecks {
		t.Stop()
		delete(w.rechecks, path)
		delete(w.recheckGens, path)
	}
}

// stopPendingTimers stops every live close_wait debounce timer in pending.
// Called on every exit path of loopCloseWait.
func stopPendingTimers(pending map[string]*closeWaitPending) {
	for _, p := range pending {
		p.timer.Stop()
	}
}

// handleWatchError reacts to an error delivered on fw.Errors; it is shared by
// the runFsnotify and runCloseWait event loops. Overflow errors mean the
// kernel/OS watch queue dropped queued events while no one was draining
// fw.Events, so the only recovery is a safety-net rescan of the watched tree:
// any file created or modified during the overflow window is re-discovered by
// mtime (IC-BUG-44). fsnotify reports the same sentinel on both production
// platforms — fsnotify.ErrEventOverflow (Linux inotify IN_Q_OVERFLOW and the
// Windows ReadDirectoryChangesW buffer overflow) — so a single errors.Is
// check covers both. Any other error is only logged.
func (w *Watcher) handleWatchError(ctx context.Context, events chan<- FileEvent, seen map[string]time.Time, err error) {
	if !errors.Is(err, fsnotify.ErrEventOverflow) {
		w.logger.Warn("watcher: fsnotify error", zap.Error(err))
		return
	}
	w.logger.Warn("watcher: fsnotify watch queue overflow, triggering safety-net rescan", zap.Error(err))
	w.safetyNetRescan(ctx, events, seen)
}

// safetyNetRescan re-walks the watched tree after an overflow window and
// re-arms debounce rechecks for files that are still being written.
//
// Trade-off (IC-BUG-44, deliberate): while this scan runs, fw.Events is not
// drained, and the scan itself sends through emitBlocking — a consumer that
// is slow again can in theory trigger a second overflow during the rescan.
// That risk is bounded and acceptable: the rescan exists precisely to
// recover from such windows, and the alternative (dropping the files
// silently) is strictly worse. emitBlocking's backpressure semantics are
// intentional (IC-BUG-47) and are deliberately not changed here.
// Trade-off (IC-BUG-44, deliberate): while this scan runs, fw.Events is not
// drained, and the scan itself sends through emitBlocking — a consumer that
// is slow again can in theory trigger a second overflow during the rescan.
// That risk is bounded and acceptable: the rescan exists precisely to
// recover from such windows, and the alternative (dropping the files
// silently) is strictly worse. emitBlocking's backpressure semantics are
// intentional (IC-BUG-47) and are deliberately not changed here.
//
// Bounded, honestly stated (PR #108 review P1): the rescan re-emits files
// that were already delivered in real time, because seen on the real-time
// path is deliberately not written (see loopFsnotify for the trade-off
// argument). The amplification per overflow round is bounded: one rescan
// pass, and the downstream IsProcessed check (rule+path+mtime+size)
// deduplicates the re-deliveries; the known remaining amplification point
// is IC-BUG-42. What the rescan must never do is skip a file it finds —
// it is the retry opportunity for every delivery that did not durably
// enqueue downstream.
func (w *Watcher) safetyNetRescan(ctx context.Context, events chan<- FileEvent, seen map[string]time.Time) {
	w.scheduleDebounceRechecks(ctx, events, seen, w.pollScan(ctx, events, seen))
}

// scheduleDebounceRechecks arms one-shot recheck timers for the paths the
// scan skipped because they were still inside the close_wait debounce window
// (see pollScan).
func (w *Watcher) scheduleDebounceRechecks(ctx context.Context, events chan<- FileEvent, seen map[string]time.Time, paths []string) {
	for _, path := range paths {
		w.scheduleDebounceRecheck(ctx, events, seen, path)
	}
}

// debounceRecheckGrace pads the recheck deadline past the close_wait debounce
// window so the quiet check at fire time does not race the writer's final
// flush.
const debounceRecheckGrace = 100 * time.Millisecond

// scheduleDebounceRecheck arms a one-shot timer that re-examines path once
// the close_wait debounce window has passed (IC-BUG-43). Without it, a file
// whose writer finished and exited within closeWaitDebounce of watcher
// startup would never be collected: the scan skips it (PR #100 review F2),
// no fsnotify event ever fires for it again, and nothing else looks at it.
// Re-arming for a path that already has a live timer replaces the timer.
func (w *Watcher) scheduleDebounceRecheck(ctx context.Context, events chan<- FileEvent, seen map[string]time.Time, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return // gone; nothing to collect
	}
	wait := closeWaitDebounce - time.Since(info.ModTime()) + debounceRecheckGrace
	if wait < debounceRecheckGrace {
		wait = debounceRecheckGrace
	}
	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	if w.rechecks == nil {
		w.rechecks = make(map[string]*time.Timer)
	}
	if w.recheckGens == nil {
		w.recheckGens = make(map[string]uint64)
	}
	if old, ok := w.rechecks[path]; ok {
		old.Stop()
	}
	w.recheckGen++
	gen := w.recheckGen
	w.recheckGens[path] = gen
	w.rechecks[path] = time.AfterFunc(wait, func() {
		// gen is captured by value (fixed at closure creation, race-free);
		// settleRecheck only drops the tracking entry when this callback is
		// still the current generation — a fired-but-locked callback must
		// not delete the timer that replaced it (codex review P2).
		w.settleRecheck(path, gen)
		w.recheckAfterDebounce(ctx, events, seen, path)
	})
}

// settleRecheck removes the recheck tracking entry for path only if this
// callback is still the current generation: scheduleDebounceRecheck's
// replace path installs a new timer (and a new generation) while an old,
// already-fired callback may still be waiting on seenMu — an unconditional
// delete would drop the replacement's entry, leaving that timer untracked
// and un-Stop-able, so the review-F2 timer leak would come back through
// this exact race (codex review P2).
func (w *Watcher) settleRecheck(path string, gen uint64) {
	w.seenMu.Lock()
	if w.recheckGens[path] == gen {
		delete(w.rechecks, path)
		delete(w.recheckGens, path)
	}
	w.seenMu.Unlock()
}

// recheckAfterDebounce is the timer callback for scheduleDebounceRecheck: it
// re-examines a previously skipped path and emits it if it has gone quiet.
// A file that is still hot is left alone — its live Write/Create events
// drive the runCloseWait debounce flush instead.
func (w *Watcher) recheckAfterDebounce(ctx context.Context, events chan<- FileEvent, seen map[string]time.Time, path string) {
	info, err := os.Stat(path)
	if err != nil {
		return // gone; nothing to collect
	}
	// F2 invariant (PR #100): never emit a file that may still be written.
	if w.appendMode == AppendModeCloseWait && time.Since(info.ModTime()) < closeWaitDebounce {
		return
	}
	// errWalkAborted only means ctx was cancelled; the timer callback has
	// nowhere to report it and the next scan retries the file (PR #100 F1).
	_ = w.scanFile(ctx, events, seen, path, info)
}

// buildEvent constructs a FileEvent for the file at path using its current
// metadata. Returns an error if the file does not exist or is a directory.
// In tail mode, FileOffset is set to the previous file size so the consumer
// knows from where to start uploading new content.
func (w *Watcher) buildEvent(path, op string) (FileEvent, error) {
	if !w.matchGlob(path) {
		return FileEvent{}, errSkipped
	}
	info, err := os.Stat(path)
	if err != nil {
		return FileEvent{}, err
	}
	if info.IsDir() {
		return FileEvent{}, errSkipped
	}

	var offset int64
	if w.appendMode == AppendModeTail {
		offset = w.tailOffsets[path]
		w.tailOffsets[path] = info.Size()
	}

	return FileEvent{
		Path:       path,
		ModTime:    info.ModTime(),
		Size:       info.Size(),
		Op:         op,
		FileOffset: offset,
	}, nil
}

// emitBlocking sends fe to the events channel, blocking until the consumer
// takes it or ctx is cancelled. It returns false only when the context was
// cancelled before the event could be delivered. Backpressure is the correct
// semantics for ALL event paths — the initial scan (PR #100 F1), the
// close_wait debounce flush, and the real-time fsnotify loop (IC-BUG-47) —
// because a dropped event means the file is never collected: the consumer
// keeps draining, so blocking only delays delivery, it never loses it (this
// holds at the watcher level; the OS layer below fsnotify has its own loss
// modes — see the IC-BUG-44 note on runFsnotify).
func (w *Watcher) emitBlocking(ctx context.Context, events chan<- FileEvent, fe FileEvent) bool {
	select {
	case <-ctx.Done():
		return false
	case events <- fe:
		return true
	}
}

// matchGlob returns true if the file's base name matches the configured glob
// pattern, or if no glob pattern is set.
func (w *Watcher) matchGlob(path string) bool {
	if w.fileGlob == "" {
		return true
	}
	base := filepath.Base(path)
	matched, err := filepath.Match(w.fileGlob, base)
	if err != nil {
		return false
	}
	return matched
}

// opString converts an fsnotify.Op to a human-readable operation string.
func opString(ev fsnotify.Event) string {
	switch {
	case ev.Has(fsnotify.Create):
		return "create"
	case ev.Has(fsnotify.Write):
		return "write"
	case ev.Has(fsnotify.Remove):
		return "remove"
	case ev.Has(fsnotify.Rename):
		return "remove"
	default:
		return "write"
	}
}

// errSkipped is returned internally when a file does not match the glob filter.
var errSkipped = skippedErr("skipped")

// errWalkAborted signals that a scan's event delivery was cancelled by ctx;
// it is swallowed by WalkDir (the walk simply stops marking progress) and is
// never surfaced to callers.
var errWalkAborted = skippedErr("walk aborted")

type skippedErr string

func (e skippedErr) Error() string { return string(e) }
