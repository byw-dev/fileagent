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
)

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
}

// Watcher monitors a directory and emits FileEvents on a channel.
type Watcher struct {
	sourcePath   string
	fileGlob     string
	recursive    bool
	pollInterval time.Duration
	logger       *zap.Logger
}

// New creates a Watcher for the given source directory.
// fileGlob is matched against file base names (e.g. "*.log").
// If recursive is true, subdirectories are watched as well.
// pollInterval controls the fallback polling cadence (default 30 s when 0).
func New(sourcePath, fileGlob string, recursive bool, pollInterval time.Duration, logger *zap.Logger) (*Watcher, error) {
	if pollInterval <= 0 {
		pollInterval = 30 * time.Second
	}
	return &Watcher{
		sourcePath:   sourcePath,
		fileGlob:     fileGlob,
		recursive:    recursive,
		pollInterval: pollInterval,
		logger:       logger,
	}, nil
}

// Start begins watching the source directory and emits FileEvents on events.
// It first attempts to use fsnotify; if adding the watch path fails, it
// transparently falls back to periodic polling. Start blocks until ctx is
// cancelled.
func (w *Watcher) Start(ctx context.Context, events chan<- FileEvent) error {
	fw, err := fsnotify.NewWatcher()
	if err != nil {
		w.logger.Warn("watcher: fsnotify unavailable, using polling", zap.Error(err))
		return w.runPolling(ctx, events)
	}
	defer fw.Close()

	if err := w.addWatchPaths(fw); err != nil {
		w.logger.Warn("watcher: cannot add watch paths, using polling", zap.Error(err))
		return w.runPolling(ctx, events)
	}

	w.logger.Info("watcher: fsnotify started", zap.String("path", w.sourcePath))
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

// runFsnotify drives the fsnotify event loop, translating raw events into
// FileEvents and emitting them on the events channel.
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
					w.emit(ctx, events, fe)
				}
			}
			if ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename) {
				if w.matchGlob(ev.Name) {
					w.emit(ctx, events, FileEvent{Path: ev.Name, Op: "remove"})
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
		prev, known := seen[path]
		if !known || info.ModTime().After(prev) {
			op := "write"
			if !known {
				op = "create"
			}
			seen[path] = info.ModTime()
			fe := FileEvent{
				Path:    path,
				ModTime: info.ModTime(),
				Size:    info.Size(),
				Op:      op,
			}
			w.emit(ctx, events, fe)
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
			_ = walk(filepath.Join(w.sourcePath, d.Name()), d, nil)
		}
	}
}

// buildEvent constructs a FileEvent for the file at path using its current
// metadata. Returns an error if the file does not exist or is a directory.
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
	return FileEvent{
		Path:    path,
		ModTime: info.ModTime(),
		Size:    info.Size(),
		Op:      op,
	}, nil
}

// emit sends fe to the events channel in a non-blocking manner. If the channel
// is full, the event is dropped and a warning is logged.
func (w *Watcher) emit(ctx context.Context, events chan<- FileEvent, fe FileEvent) {
	select {
	case <-ctx.Done():
	case events <- fe:
	default:
		w.logger.Warn("watcher: event channel full, dropping event", zap.String("path", fe.Path))
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

type skippedErr string

func (e skippedErr) Error() string { return string(e) }
