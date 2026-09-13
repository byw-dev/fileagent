// Package watcher monitors file system directories and emits FileEvents for
// new or modified files. It uses fsnotify for real-time notifications and
// falls back to periodic polling when inotify is unavailable.
package watcher

import (
	"context"
	"os"
	"path/filepath"
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

	if err := w.addWatchPaths(fw); err != nil {
		w.logger.Warn("watcher: cannot add watch paths, using polling", zap.Error(err))
		return w.runPolling(ctx, events)
	}

	// Use the same scan as the polling fallback so files that predate watcher
	// startup are collected consistently on both paths. Register watches first
	// so changes made during the scan are still observed by fsnotify.
	w.pollScan(ctx, events, make(map[string]time.Time))

	w.logger.Info("watcher: fsnotify started", zap.String("path", w.sourcePath))
	if w.appendMode == AppendModeCloseWait {
		return w.runCloseWait(ctx, events, fw)
	}
	return w.runFsnotify(ctx, events, fw)
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
func (w *Watcher) runCloseWait(ctx context.Context, events chan<- FileEvent, fw *fsnotify.Watcher) error {
	type pendingEntry struct {
		timer *time.Timer
		op    string
	}
	pending := make(map[string]*pendingEntry)

	flush := func(path, op string) {
		fe, err := w.buildEvent(path, op)
		if err != nil {
			return
		}
		// IC-BUG-47: blocking send — a dropped event here means the file is
		// never collected (mtime/size never change again). Backpressure
		// delays the debounce-flush goroutine instead.
		w.emitBlocking(ctx, events, fe)
	}

	for {
		select {
		case <-ctx.Done():
			// Cancel all pending timers before returning.
			for _, p := range pending {
				p.timer.Stop()
			}
			return ctx.Err()
		case ev, ok := <-fw.Events:
			if !ok {
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
				p.op = op
			} else {
				path := ev.Name // capture for closure
				p := &pendingEntry{op: op}
				p.timer = time.AfterFunc(closeWaitDebounce, func() {
					flush(path, p.op)
				})
				pending[path] = p
			}
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			w.logger.Warn("watcher: fsnotify error", zap.Error(err))
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
// runCloseWait. Known trade-off (IC-BUG-44, kept out of scope): while the
// send blocks, fw.Events is not drained, so a sustained burst can overflow
// the kernel's inotify/kqueue queue — fsnotify surfaces that as an error
// (logged below), which is visible, unlike the old silent per-event drop.
func (w *Watcher) runFsnotify(ctx context.Context, events chan<- FileEvent, fw *fsnotify.Watcher) error {
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case ev, ok := <-fw.Events:
			if !ok {
				return nil
			}
			if ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write) {
				if fe, err := w.buildEvent(ev.Name, opString(ev)); err == nil {
					w.emitBlocking(ctx, events, fe)
				}
			}
			if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
				if w.matchGlob(ev.Name) {
					w.emitBlocking(ctx, events, FileEvent{Path: ev.Name, Op: "remove"})
				}
			}
		case err, ok := <-fw.Errors:
			if !ok {
				return nil
			}
			w.logger.Warn("watcher: fsnotify error", zap.Error(err))
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
func (w *Watcher) pollScan(ctx context.Context, events chan<- FileEvent, seen map[string]time.Time) {
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
		// Skip it without marking seen — the close_wait flow (or a later scan,
		// once the file is quiet) picks it up.
		if w.appendMode == AppendModeCloseWait && time.Since(info.ModTime()) < closeWaitDebounce {
			return nil
		}
		prev, known := seen[path]
		if !known || info.ModTime().After(prev) {
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
			// Mark as seen and record the tail offset only after the event has
			// been delivered: if the send is aborted (ctx cancelled) or would
			// drop the event, the next scan must retry the file instead of
			// silently skipping it forever (PR #100 review F1).
			if !w.emitBlocking(ctx, events, fe) {
				return errWalkAborted
			}
			seen[path] = info.ModTime()
			if w.appendMode == AppendModeTail {
				w.tailOffsets[path] = info.Size()
			}
		}
		return nil
	}

	if w.recursive {
		_ = filepath.WalkDir(w.sourcePath, walk)
	} else {
		entries, err := os.ReadDir(w.sourcePath)
		if err != nil {
			w.logger.Warn("watcher: read dir error", zap.Error(err))
			return
		}
		for _, d := range entries {
			if d.IsDir() {
				continue
			}
			if walk(filepath.Join(w.sourcePath, d.Name()), d, nil) != nil {
				return // ctx cancelled, stop scanning
			}
		}
	}
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
// keeps draining, so blocking only delays delivery, it never loses it. Note
// the IC-BUG-44 interaction recorded on runFsnotify: blocking holds the
// fsnotify event loop, so a sustained burst can overflow the kernel watch
// queue (surfaced as a visible fsnotify error, not a silent drop).
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
