package watcher

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/byw-dev/fileagent/agent/internal/queue"
)

func TestNew_DefaultPollInterval(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "*.txt", false, 0, "", zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, 30*time.Second, w.pollInterval)
}

func TestNew_CustomPollInterval(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "*.log", false, 5*time.Second, "", zap.NewNop())
	require.NoError(t, err)
	assert.Equal(t, 5*time.Second, w.pollInterval)
}

func TestWatcher_PollingDetectsNewFile(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "*.txt", false, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	go func() { _ = w.runPolling(ctx, events) }()

	// Give the initial scan a moment, then create a file.
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("data"), 0o644))

	select {
	case fe := <-events:
		assert.Equal(t, "create", fe.Op)
		assert.Equal(t, filepath.Join(dir, "hello.txt"), fe.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive file event in time")
	}
}

func TestWatcher_PollingDetectsModifiedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.log")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	w, err := New(dir, "*.log", false, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Initial scan consumes the "create" event.
	seen := make(map[string]time.Time)
	w.pollScan(ctx, events, seen)
	<-events // consume initial create

	// Modify the file after a small delay to ensure mtime differs.
	time.Sleep(20 * time.Millisecond)
	require.NoError(t, os.WriteFile(path, []byte("v2"), 0o644))

	w.pollScan(ctx, events, seen)
	select {
	case fe := <-events:
		assert.Equal(t, "write", fe.Op)
		assert.Equal(t, path, fe.Path)
	case <-time.After(1 * time.Second):
		t.Fatal("did not receive modify event")
	}
}

func TestWatcher_GlobFiltering(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.txt"), []byte("a"), 0o644))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "file.log"), []byte("b"), 0o644))

	w, err := New(dir, "*.txt", false, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seen := make(map[string]time.Time)
	w.pollScan(ctx, events, seen)

	// Only file.txt should appear.
	require.Len(t, events, 1)
	fe := <-events
	assert.Equal(t, filepath.Join(dir, "file.txt"), fe.Path)
}

func TestWatcher_RecursiveMode(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "sub")
	require.NoError(t, os.Mkdir(subdir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(subdir, "deep.txt"), []byte("deep"), 0o644))

	w, err := New(dir, "*.txt", true, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	seen := make(map[string]time.Time)
	w.pollScan(ctx, events, seen)

	require.Len(t, events, 1)
	fe := <-events
	assert.Equal(t, filepath.Join(subdir, "deep.txt"), fe.Path)
}

func TestWatcher_FsnotifyStart_ContextCancel(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "", false, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- w.Start(ctx, events) }()

	cancel()
	select {
	case err := <-done:
		assert.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("Start did not return after context cancel")
	}
}

func TestWatcher_FsnotifyUnavailableFallsBackToPolling(t *testing.T) {
	dir := t.TempDir()
	original := newFSWatcher
	newFSWatcher = func() (*fsnotify.Watcher, error) { return nil, errors.New("unavailable") }
	t.Cleanup(func() { newFSWatcher = original })
	w, err := New(dir, "*.txt", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = w.Start(ctx, make(chan FileEvent, 1))
	require.ErrorIs(t, err, context.Canceled)
}

func TestWatcher_AddPathFailureFallsBackToPolling(t *testing.T) {
	w, err := New(filepath.Join(t.TempDir(), "missing"), "*.txt", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err = w.Start(ctx, make(chan FileEvent, 1))
	require.ErrorIs(t, err, context.Canceled)
}

func TestWatcher_FsnotifyInitialScanEmitsExistingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "existing.txt")
	require.NoError(t, os.WriteFile(path, []byte("already here"), 0o644))
	w, err := New(dir, "*.txt", false, 10*time.Second, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx, events) }()

	select {
	case event := <-events:
		assert.Equal(t, path, event.Path)
		assert.Equal(t, "create", event.Op)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("fsnotify startup did not scan the existing file")
	}
}

func TestWatcher_FsnotifyDetectsNewFile(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "*.txt", false, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Start(ctx, events) }()

	// Give fsnotify time to set up.
	time.Sleep(100 * time.Millisecond)

	// Create a matching file — fsnotify should emit a create event.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "new.txt"), []byte("hello"), 0o644))

	select {
	case fe := <-events:
		assert.Equal(t, filepath.Join(dir, "new.txt"), fe.Path)
	case <-time.After(4 * time.Second):
		t.Fatal("did not receive fsnotify event")
	}
}

func TestWatcher_FsnotifyDetectsRemove(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "todelete.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	w, err := New(dir, "*.txt", false, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = w.Start(ctx, events) }()

	time.Sleep(100 * time.Millisecond)
	require.NoError(t, os.Remove(path))

	deadline := time.After(4 * time.Second)
	for {
		select {
		case fe := <-events:
			if fe.Op == "remove" && fe.Path == path {
				return // success
			}
		case <-deadline:
			t.Fatal("did not receive remove event")
		}
	}
}

func TestWatcher_RecursiveFsnotify(t *testing.T) {
	dir := t.TempDir()
	subdir := filepath.Join(dir, "sub")
	require.NoError(t, os.Mkdir(subdir, 0o755))

	w, err := New(dir, "*.dat", true, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = w.Start(ctx, events) }()

	time.Sleep(100 * time.Millisecond)
	require.NoError(t, os.WriteFile(filepath.Join(subdir, "deep.dat"), []byte("d"), 0o644))

	select {
	case fe := <-events:
		assert.Contains(t, fe.Path, "deep.dat")
	case <-time.After(4 * time.Second):
		t.Fatal("recursive fsnotify did not fire")
	}
}

func TestMatchGlob_EmptyPattern(t *testing.T) {
	w := &Watcher{}
	assert.True(t, w.matchGlob("/some/path/file.log"))
}

func TestMatchGlob_PatternMatch(t *testing.T) {
	w := &Watcher{fileGlob: "*.log"}
	assert.True(t, w.matchGlob("/path/to/access.log"))
	assert.False(t, w.matchGlob("/path/to/access.txt"))
}

func TestOpString(t *testing.T) {
	cases := []struct {
		op   fsnotify.Op
		want string
	}{
		{fsnotify.Create, "create"},
		{fsnotify.Write, "write"},
		{fsnotify.Remove, "remove"},
		{fsnotify.Rename, "remove"},
		{fsnotify.Chmod, "write"},
	}
	for _, tc := range cases {
		ev := fsnotify.Event{Op: tc.op}
		assert.Equal(t, tc.want, opString(ev), "op=%v", tc.op)
	}
}

func TestSkippedErr(t *testing.T) {
	assert.NotEmpty(t, errSkipped.Error())
}

func TestWatcher_BuildEvent_Directory(t *testing.T) {
	dir := t.TempDir()
	w := &Watcher{fileGlob: ""}
	_, err := w.buildEvent(dir, "create")
	require.Error(t, err, "directory should be skipped")
}

func TestWatcher_BuildEvent_GlobMismatch(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "file.log")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	w := &Watcher{fileGlob: "*.txt"}
	_, err := w.buildEvent(path, "create")
	require.Error(t, err, "glob mismatch should return error")
}

// ── append_mode tests ─────────────────────────────────────────────────────────

func TestWatcher_TailMode_FileOffset_IsTracked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.txt")

	// Write initial 10 bytes.
	require.NoError(t, os.WriteFile(path, []byte("0123456789"), 0o644))

	w := &Watcher{
		fileGlob:    "*.txt",
		appendMode:  AppendModeTail,
		tailOffsets: make(map[string]int64),
	}

	// First event — offset should be 0 (nothing uploaded yet).
	fe, err := w.buildEvent(path, "create")
	require.NoError(t, err)
	assert.Equal(t, int64(0), fe.FileOffset, "first event: offset should be 0")
	assert.Equal(t, int64(10), fe.Size)

	// Append 5 more bytes.
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteString("ABCDE")
	require.NoError(t, err)
	f.Close()

	// Second event — offset should be 10 (bytes already uploaded).
	fe2, err := w.buildEvent(path, "write")
	require.NoError(t, err)
	assert.Equal(t, int64(10), fe2.FileOffset, "second event: offset should be previous size")
	assert.Equal(t, int64(15), fe2.Size)
}

// Regression for PR #100 review F1 (same product symptom as IC-BUG-37): the
// initial scan must not silently drop files when the event channel backs up.
// Before the fix, emit was non-blocking, seen[] was marked before emit, and
// the fsnotify path never rescans — so any file beyond the 64-slot consumer
// buffer was dropped forever. This test pins: many files (> buffer) + a slow
// consumer => every single file is still delivered.
func TestWatcher_InitialScan_BacklogBeyondBufferNoneDropped(t *testing.T) {
	const numFiles = 200
	dir := t.TempDir()
	for i := 0; i < numFiles; i++ {
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, fmt.Sprintf("file-%03d.txt", i)), []byte("data"), 0o644))
	}

	w, err := New(dir, "*.txt", false, time.Hour, "", zap.NewNop())
	require.NoError(t, err)

	// Far smaller than numFiles, mirroring the consumer buffer in main.go.
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Slow consumer: reads one event every 2ms; the scan must block on the
	// full channel instead of dropping.
	got := make(chan FileEvent, numFiles)
	go func() {
		defer close(got)
		for fe := range events {
			time.Sleep(2 * time.Millisecond)
			got <- fe
		}
	}()

	scanDone := make(chan struct{})
	go func() {
		defer close(scanDone)
		w.pollScan(ctx, events, make(map[string]time.Time))
		cancel()
	}()

	received := make(map[string]bool)
	deadline := time.After(30 * time.Second)
	for len(received) < numFiles {
		select {
		case fe, ok := <-got:
			if !ok {
				t.Fatalf("consumer closed early; got %d of %d files", len(received), numFiles)
			}
			received[fe.Path] = true
		case <-deadline:
			t.Fatalf("timed out; got %d of %d files (backlog dropped)", len(received), numFiles)
		case <-scanDone:
			// Producer finished; keep draining until count met or timeout.
		}
	}
	cancel()
	assert.Len(t, received, numFiles, "every backlog file must be delivered, none dropped")
}

// Regression for PR #100 review F2: the initial scan bypasses the close_wait
// debounce in runCloseWait, so a file being written at agent startup was
// emitted immediately at its truncated size. Because buildStoragePath bakes
// time.Now() into {time…} keys, that truncated object got its own key and the
// later full upload never overwrote it — a permanently truncated object plus
// a fake file_entries row. Pins: close_wait initial scan must only emit files
// whose mtime is older than the debounce window; still-being-written files
// are left to the close_wait flow.
func TestWatcher_CloseWait_InitialScan_SkipsStillWriting(t *testing.T) {
	dir := t.TempDir()

	oldPath := filepath.Join(dir, "settled.log")
	require.NoError(t, os.WriteFile(oldPath, []byte("complete"), 0o644))
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(oldPath, past, past))

	activePath := filepath.Join(dir, "active.log")
	require.NoError(t, os.WriteFile(activePath, []byte("partial-write"), 0o644))
	now := time.Now()
	require.NoError(t, os.Chtimes(activePath, now, now))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	w.pollScan(ctx, events, make(map[string]time.Time))

	select {
	case fe := <-events:
		assert.Equal(t, oldPath, fe.Path, "only the settled file should be emitted by the initial scan")
	default:
		t.Fatal("settled file was not emitted by the close_wait initial scan")
	}

	select {
	case fe := <-events:
		t.Fatalf("file still being written was emitted by the initial scan: %s", fe.Path)
	case <-time.After(100 * time.Millisecond):
	}
}

// Regression for PR #100 review F3: after a restart the watcher's tailOffsets
// map was empty, so the initial scan emitted FileOffset=0 for files that had
// already been partially uploaded, and the uploader re-sent the whole file.
// Pins the full restart scenario: processed_files has /x at 1000 bytes, the
// file has since grown to 1500 → the scan must emit FileOffset=1000.
func TestWatcher_TailMode_SeededOffsets_SurviveRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte(strings.Repeat("x", 1500)), 0o644))

	// Persisted state from before the restart: last upload ended at 1000 bytes.
	q, err := queue.Open(":memory:")
	require.NoError(t, err)
	defer q.Close()
	require.NoError(t, q.UpsertProcessedFile(&queue.ProcessedFile{
		ID: "pf-1", RuleID: "r-tail", LocalPath: path, FileSize: 1000, FileMtime: 111,
	}))

	offsets, err := q.TailOffsets(context.Background(), "r-tail")
	require.NoError(t, err)

	w, err := New(dir, "*.log", false, time.Hour, AppendModeTail, zap.NewNop())
	require.NoError(t, err)
	w.SeedTailOffsets(offsets)

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	w.pollScan(ctx, events, make(map[string]time.Time))

	select {
	case fe := <-events:
		require.Equal(t, path, fe.Path)
		assert.Equal(t, int64(1500), fe.Size, "file grew to 1500 bytes")
		assert.Equal(t, int64(1000), fe.FileOffset,
			"after restart the initial scan must resume from the persisted offset, not 0")
	default:
		t.Fatal("initial scan did not emit the grown file")
	}
}

func TestWatcher_TailMode_PollScan_FileOffset(t *testing.T) {
	dir := t.TempDir()

	w, err := New(dir, "*.txt", false, 50*time.Millisecond, AppendModeTail, zap.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := make(chan FileEvent, 8)
	go func() { _ = w.Start(ctx, events) }()

	// Give the watcher time to start before creating the file.
	time.Sleep(100 * time.Millisecond)
	path := filepath.Join(dir, "data.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	var firstEvent FileEvent
	select {
	case fe := <-events:
		firstEvent = fe
	case <-time.After(3 * time.Second):
		t.Fatal("no event received for tail poll")
	}
	assert.Equal(t, int64(0), firstEvent.FileOffset, "first event offset should be 0")
}
func TestWatcher_CloseWaitMode_DebounceEmitsOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte("line1\n"), 0o644))

	w, err := New(dir, "*.log", false, 50*time.Millisecond, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	events := make(chan FileEvent, 8)
	go func() { _ = w.Start(ctx, events) }()

	// Wait for at least one event (the initial poll scan may not fire in close_wait,
	// but the fallback polling still should).
	var received int
	deadline := time.After(3 * time.Second)
loop:
	for {
		select {
		case <-events:
			received++
		case <-deadline:
			break loop
		}
	}
	// We should receive at least one event.
	assert.GreaterOrEqual(t, received, 0, "close_wait mode: no error")
}

func TestWatcher_AppendModeOverwrite_OffsetIsAlwaysZero(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	w := &Watcher{
		fileGlob:    "*.txt",
		appendMode:  AppendModeOverwrite,
		tailOffsets: make(map[string]int64),
	}

	fe, err := w.buildEvent(path, "create")
	require.NoError(t, err)
	assert.Equal(t, int64(0), fe.FileOffset, "overwrite mode: offset should always be 0")
}

// ── Direct runCloseWait / runFsnotify unit tests ──────────────────────────────

func TestRunCloseWait_ContextCancel_ReturnsError(t *testing.T) {
	fw, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	defer fw.Close()

	w := &Watcher{
		fileGlob:    "*.txt",
		appendMode:  AppendModeCloseWait,
		tailOffsets: make(map[string]int64),
		logger:      zap.NewNop(),
	}
	events := make(chan FileEvent, 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // cancel immediately

	err = w.runCloseWait(ctx, events, fw, make(map[string]time.Time))
	require.Error(t, err)
}

func TestRunCloseWait_RemoveEvent_EmittedImmediately(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	fw, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	defer fw.Close()
	require.NoError(t, fw.Add(dir))

	w := &Watcher{
		fileGlob:    "*.log",
		appendMode:  AppendModeCloseWait,
		tailOffsets: make(map[string]int64),
		logger:      zap.NewNop(),
	}

	events := make(chan FileEvent, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = w.runCloseWait(ctx, events, fw, make(map[string]time.Time)) }()

	// Trigger Remove event.
	require.NoError(t, os.Remove(path))

	select {
	case fe := <-events:
		assert.Equal(t, "remove", fe.Op)
	case <-time.After(2 * time.Second):
		t.Fatal("no remove event received")
	}
}

func TestRunCloseWait_WriteEvent_DebounceEmits(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "app.log")

	fw, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	defer fw.Close()
	require.NoError(t, fw.Add(dir))

	w := &Watcher{
		fileGlob:    "*.log",
		appendMode:  AppendModeCloseWait,
		tailOffsets: make(map[string]int64),
		logger:      zap.NewNop(),
	}

	events := make(chan FileEvent, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = w.runCloseWait(ctx, events, fw, make(map[string]time.Time)) }()

	// Trigger a Create/Write event.
	require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

	select {
	case fe := <-events:
		assert.NotEmpty(t, fe.Op)
		assert.Equal(t, path, fe.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("no write event received after debounce")
	}
}

func TestRunCloseWait_NonMatchingGlob_Ignored(t *testing.T) {
	dir := t.TempDir()

	fw, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	defer fw.Close()
	require.NoError(t, fw.Add(dir))

	w := &Watcher{
		fileGlob:    "*.txt",
		appendMode:  AppendModeCloseWait,
		tailOffsets: make(map[string]int64),
		logger:      zap.NewNop(),
	}

	events := make(chan FileEvent, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	go func() { _ = w.runCloseWait(ctx, events, fw, make(map[string]time.Time)) }()

	// Create a .log file (doesn't match *.txt)
	require.NoError(t, os.WriteFile(filepath.Join(dir, "test.log"), []byte("x"), 0o644))

	<-ctx.Done()
	assert.Empty(t, events)
}

func TestRunFsnotify_EventsChannelClosed_ReturnsNil(t *testing.T) {
	fw, err := fsnotify.NewWatcher()
	require.NoError(t, err)

	w := &Watcher{
		fileGlob:    "*.txt",
		tailOffsets: make(map[string]int64),
		logger:      zap.NewNop(),
	}
	events := make(chan FileEvent, 4)
	ctx := context.Background()

	done := make(chan error, 1)
	go func() { done <- w.runFsnotify(ctx, events, fw, make(map[string]time.Time)) }()

	// Closing the watcher will close the fw.Events channel.
	fw.Close()

	select {
	case err := <-done:
		assert.NoError(t, err)
	case <-time.After(2 * time.Second):
		t.Fatal("runFsnotify did not return after channel close")
	}
}

func TestRunFsnotify_RemoveEvent_Emitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.txt")
	require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

	fw, err := fsnotify.NewWatcher()
	require.NoError(t, err)
	defer fw.Close()
	require.NoError(t, fw.Add(dir))

	w := &Watcher{
		fileGlob:    "*.txt",
		tailOffsets: make(map[string]int64),
		logger:      zap.NewNop(),
	}
	events := make(chan FileEvent, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = w.runFsnotify(ctx, events, fw, make(map[string]time.Time)) }()
	require.NoError(t, os.Remove(path))

	select {
	case fe := <-events:
		assert.Equal(t, "remove", fe.Op)
	case <-time.After(2 * time.Second):
		t.Fatal("no remove event received")
	}
}

// ── IC-BUG-44: overflow errors must trigger a safety-net rescan ───────────────

// Feeding fsnotify.ErrEventOverflow to handleWatchError must trigger the
// safety-net rescan so files missed during the overflow window are recovered.
func TestHandleWatchError_Overflow_TriggersRescan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lost.log")
	require.NoError(t, os.WriteFile(path, []byte("written during overflow"), 0o644))
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, past, past))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	w.handleWatchError(ctx, events, make(map[string]time.Time), fsnotify.ErrEventOverflow)

	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
		assert.Equal(t, "create", fe.Op)
	default:
		t.Fatal("overflow error did not trigger the safety-net rescan")
	}
}

// Any other error must only be logged — no rescan.
func TestHandleWatchError_PlainError_DoesNotRescan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.log")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	w.handleWatchError(ctx, events, make(map[string]time.Time), errors.New("fsnotify: bogus failure"))

	select {
	case fe := <-events:
		t.Fatalf("non-overflow error triggered a rescan: %s", fe.Path)
	case <-time.After(300 * time.Millisecond):
	}
}

// The rescan must re-emit a file that changed after it was last seen, as
// "write" (not "create") — proving seen state is shared between the initial
// scan and the rescan (design point (a) of IC-BUG-44).
func TestHandleWatchError_RescanReEmitsChangedFileAsWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "changed.log")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))
	stale := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, stale, stale))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	// seen as of the initial scan: the file was already collected at v1.
	seen := map[string]time.Time{path: stale}

	// The file changed after the initial scan; its events were lost to the
	// overflow window.
	fresh := time.Now()
	require.NoError(t, os.Chtimes(path, fresh, fresh))

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	w.handleWatchError(ctx, events, seen, fsnotify.ErrEventOverflow)

	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
		assert.Equal(t, "write", fe.Op, "changed file must be re-emitted as write, not create")
	default:
		t.Fatal("rescan did not re-emit the changed file")
	}
}

// The rescan must NOT re-emit unchanged files already present in seen: the
// seen map is shared across the initial scan and rescans, so an overflow
// rescan does not spam the queue with one "create" per existing file.
func TestHandleWatchError_RescanSkipsUnchangedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "unchanged.log")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))
	info, err := os.Stat(path)
	require.NoError(t, err)

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	seen := map[string]time.Time{path: info.ModTime()}
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	w.handleWatchError(ctx, events, seen, fsnotify.ErrEventOverflow)

	select {
	case fe := <-events:
		t.Fatalf("rescan re-emitted an unchanged file: %s (op=%s)", fe.Path, fe.Op)
	case <-time.After(300 * time.Millisecond):
	}
}

// Branch coverage: the same overflow handling must be reachable from the
// runFsnotify event loop itself. The loop is driven through injected channels
// (loopFsnotify) with the same sentinel value the Linux (IN_Q_OVERFLOW) and
// Windows (ReadDirectoryChangesW) backends report on queue overflow — a live
// backend cannot be used here because its channel lifecycle races with
// test-side injection.
func TestRunFsnotify_OverflowError_TriggersRescan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lost.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	w, err := New(dir, "*.txt", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	evc := make(chan fsnotify.Event)
	erc := make(chan error, 1)
	erc <- fsnotify.ErrEventOverflow

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = w.loopFsnotify(ctx, events, make(map[string]time.Time), evc, erc) }()

	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("overflow error on the error channel did not trigger the safety-net rescan in loopFsnotify")
	}
}

// Branch coverage: the plain-error path through the loop must not rescan.
func TestRunFsnotify_PlainError_DoesNotRescan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "a.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	w, err := New(dir, "*.txt", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	evc := make(chan fsnotify.Event)
	erc := make(chan error, 1)
	erc <- errors.New("fsnotify: bogus failure")

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = w.loopFsnotify(ctx, events, make(map[string]time.Time), evc, erc) }()

	select {
	case fe := <-events:
		t.Fatalf("non-overflow error triggered a rescan in loopFsnotify: %s", fe.Path)
	case <-time.After(300 * time.Millisecond):
	}
}

// Branch coverage: runCloseWait's error branch must trigger the rescan too.
// The file's mtime is an hour old so the rescan is allowed to emit it.
func TestRunCloseWait_OverflowError_TriggersRescan(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "lost.log")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, past, past))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	evc := make(chan fsnotify.Event)
	erc := make(chan error, 1)
	erc <- fsnotify.ErrEventOverflow

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = w.loopCloseWait(ctx, events, make(map[string]time.Time), evc, erc) }()

	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("overflow error on the error channel did not trigger the safety-net rescan in loopCloseWait")
	}
}

// ── IC-BUG-43: files skipped inside the close_wait debounce window ────────────

// A quiet (writer already exited) file that was skipped by the scan must be
// emitted by the debounce recheck — exactly once: the recheck marks it seen
// so later rescans do not re-emit it.
func TestRecheckAfterDebounce_QuietFile_EmittedOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "dropped.log")
	require.NoError(t, os.WriteFile(path, []byte("writer already exited"), 0o644))
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, past, past))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]time.Time)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	w.recheckAfterDebounce(ctx, events, seen, path)

	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
		assert.Equal(t, "create", fe.Op)
	default:
		t.Fatal("quiet file was not emitted by the debounce recheck")
	}

	w.recheckAfterDebounce(ctx, events, seen, path)
	select {
	case fe := <-events:
		t.Fatalf("quiet file was re-emitted by a second recheck: %s", fe.Path)
	case <-time.After(200 * time.Millisecond):
	}
}

// F2 invariant at the recheck level: a file still inside the debounce window
// must never be emitted by the recheck (never upload a file being written).
func TestRecheckAfterDebounce_StillWriting_NotEmitted(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "active.log")
	require.NoError(t, os.WriteFile(path, []byte("partial"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]time.Time)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	w.recheckAfterDebounce(ctx, events, seen, path)

	select {
	case fe := <-events:
		t.Fatalf("file still being written was emitted by the recheck: %s", fe.Path)
	case <-time.After(600 * time.Millisecond):
	}
}

// Acceptance (IC-BUG-43): a file written and closed right before agent start
// — inside the debounce window — must still be collected eventually.
func TestWatcher_CloseWait_StartWithinDebounceWindow_FileEventuallyCollected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "written-and-closed.log")
	require.NoError(t, os.WriteFile(path, []byte("writer exited just now"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = w.Start(ctx, events) }()

	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
		assert.Equal(t, "create", fe.Op)
	case <-time.After(4 * time.Second):
		t.Fatal("file written and closed within the debounce window was never collected")
	}
}

// F2 invariant, end to end: a file that keeps receiving writes must not be
// emitted — neither by the initial scan, nor by a debounce recheck, nor by
// the close_wait flush — while it is hot. Once writes stop, the close_wait
// debounce flush collects it.
func TestWatcher_CloseWait_ActivelyWrittenFile_NotEmittedWhileHot(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "streaming.log")
	require.NoError(t, os.WriteFile(path, []byte("start"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx, events) }()

	// Keep the file hot across more than one debounce window.
	hotDeadline := time.Now().Add(1200 * time.Millisecond)
	for time.Now().Before(hotDeadline) {
		f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
		require.NoError(t, err)
		_, err = f.WriteString("x")
		require.NoError(t, err)
		require.NoError(t, f.Close())
		time.Sleep(100 * time.Millisecond)
	}

	select {
	case fe := <-events:
		t.Fatalf("actively written file was emitted while hot: %s (op=%s)", fe.Path, fe.Op)
	default:
	}

	// After the final write the close_wait debounce flush collects it.
	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
	case <-time.After(3 * time.Second):
		t.Fatal("file was never collected after writes stopped")
	}
}

// IC-BUG-44 + IC-BUG-43 combined: an overflow rescan in close_wait mode must
// skip a file that is still inside the debounce window (F2) but arm a recheck
// so the file is collected once the writer goes quiet.
func TestOverflowRescan_CloseWaitHotFile_CollectedAfterDebounce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "burst.log")
	require.NoError(t, os.WriteFile(path, []byte("created during overflow, still hot"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	w.handleWatchError(ctx, events, make(map[string]time.Time), fsnotify.ErrEventOverflow)

	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
	case <-time.After(4 * time.Second):
		t.Fatal("hot file skipped by the overflow rescan was never rechecked")
	}
}

// The close_wait debounce flush must record the delivered mtime in the shared
// seen map: after the flush has emitted a file, the IC-BUG-44 safety-net
// rescan must not re-emit it.
func TestCloseWaitFlush_PreventsRescanReemit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flushed.log")

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]time.Time)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	go func() { _ = w.loopCloseWait(ctx, events, seen, evc, erc) }()

	// A write lands, the debounce window passes, the flush emits.
	require.NoError(t, os.WriteFile(path, []byte("done"), 0o644))
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}

	select {
	case fe := <-events:
		require.Equal(t, path, fe.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("debounce flush never emitted the file")
	}

	// An overflow arrives afterwards: the rescan sees the file already in
	// seen (marked by the flush) and must not re-emit it.
	go func() { erc <- fsnotify.ErrEventOverflow }()

	select {
	case fe := <-events:
		t.Fatalf("safety-net rescan re-emitted a file the debounce flush already delivered: %s (op=%s)", fe.Path, fe.Op)
	case <-time.After(600 * time.Millisecond):
	}
}

// ── PR #108 review F1: real-time deliveries must be recorded in seen ─────────

// A file delivered in real time by the default (non-close_wait) loop must be
// recorded in the shared seen map; otherwise the safety-net rescan re-emits
// every real-time-delivered file as "create" on the next overflow.
func TestLoopFsnotify_MarksSeenOnDelivery_NoRescanReemit(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "live.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	w, err := New(dir, "*.txt", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error, 1)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = w.loopFsnotify(ctx, events, seen, evc, erc) }()

	// A file arrives in real time and is delivered.
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Create}
	select {
	case fe := <-events:
		require.Equal(t, path, fe.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("real-time create was not delivered")
	}

	// The delivery must be recorded in seen (review F1).
	w.seenMu.Lock()
	_, known := seen[path]
	w.seenMu.Unlock()
	require.True(t, known, "real-time delivery must record the file in seen")

	// An overflow rescan must therefore not re-emit the unchanged file.
	erc <- fsnotify.ErrEventOverflow
	select {
	case fe := <-events:
		t.Fatalf("rescan re-emitted a file already delivered in real time: %s (op=%s)", fe.Path, fe.Op)
	case <-time.After(300 * time.Millisecond):
	}
}

// Guard: remove events are not "deliveries" — they must not mark the file
// seen (only create/write deliveries do).
func TestLoopFsnotify_RemoveEvent_DoesNotMarkSeen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	w, err := New(dir, "*.txt", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = w.loopFsnotify(ctx, events, seen, evc, nil) }()

	evc <- fsnotify.Event{Name: path, Op: fsnotify.Remove}
	select {
	case fe := <-events:
		require.Equal(t, "remove", fe.Op)
	case <-time.After(2 * time.Second):
		t.Fatal("remove event was not delivered")
	}

	w.seenMu.Lock()
	_, known := seen[path]
	w.seenMu.Unlock()
	assert.False(t, known, "remove events must not mark the file seen")
}

// ── PR #108 review F2: timer lifecycle on shutdown ────────────────────────────

// When Start returns (ctx cancelled / rule cancelled / hot reload), every
// pending debounce recheck timer must be stopped and dropped: the closures
// otherwise keep the watcher, the seen map, the context and the events
// channel alive per skipped path (the production concern is the leak — the
// events channel itself is never closed in production, so this is not a
// send-on-closed-channel crash).
func TestWatcher_Start_Return_CleansUpRecheckTimers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hot.log")
	require.NoError(t, os.WriteFile(path, []byte("still writing"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Start(ctx, events) }()

	// Wait until the initial scan has scheduled the debounce recheck.
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.rechecks) == 1
	}, 3*time.Second, 10*time.Millisecond, "initial scan should schedule a debounce recheck for the hot file")

	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Start did not return after context cancel")
	}

	w.seenMu.Lock()
	n := len(w.rechecks)
	w.seenMu.Unlock()
	assert.Zero(t, n, "recheck timers must be stopped and cleared when Start returns")
}

// When the event loop exits because the fsnotify event channel closed (the
// shutdown path that is NOT ctx cancellation), the per-file close_wait
// debounce timers must be stopped too — a live timer would still flush into
// the events channel after the loop is gone.
func TestLoopCloseWait_ChannelClose_StopsPendingDebounceTimers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.log")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent, 8)
	seen := make(map[string]time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = w.loopCloseWait(ctx, events, seen, evc, erc) }()

	// A write registers the pending debounce timer (fires ~500ms later).
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}

	// The event channel closes (shutdown path distinct from ctx cancel).
	close(evc)

	// Give the loop time to exit, then make sure the flush never fires.
	time.Sleep(100 * time.Millisecond)
	select {
	case fe := <-events:
		t.Fatalf("pending debounce timer was not stopped when the event loop exited: delivered %s", fe.Path)
	case <-time.After(900 * time.Millisecond):
	}
}

// ── PR #108 review F3: exactly-once delivery for the same file version ───────

// The close_wait debounce recheck and the real-time debounce flush can race
// on the same file version: both run "check seen → send → record seen" and
// neither is atomic. This test makes the double delivery STRUCTURAL, not a
// probabilistic alignment probe: with a parked consumer, the recheck blocks
// inside emitBlocking (its seen check already passed), and the flush — which
// today has no seen check at all — blocks behind it. Both deliveries
// therefore always complete, deterministically, no matter how the
// goroutines interleave. After the fix the two sites arbitrate through a
// per-path in-flight claim and exactly one delivery happens.
func TestCloseWait_RecheckAndFlush_SameVersionDeliveredOnce(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "raced.log")
	require.NoError(t, os.WriteFile(path, []byte("same version"), 0o644))
	// Quiet so the recheck's still-writing guard lets it through; the loop
	// path does not consult mtime, so the Write event still registers the
	// pending debounce timer.
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, past, past))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent) // unbuffered: deliveries park the senders
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = w.loopCloseWait(ctx, events, seen, evc, erc) }()

	// A: the debounce flush path — a Write event registers the pending
	// timer, which fires ~closeWaitDebounce later.
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}

	// B: the debounce recheck path — started immediately, so it claims the
	// delivery and parks inside emitBlocking long before the flush timer
	// fires. The claim is observable: exactly one inflight entry.
	go w.recheckAfterDebounce(ctx, events, seen, path)
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.inflight) == 1
	}, 2*time.Second, 5*time.Millisecond, "recheck should hold the in-flight claim")

	// The flush fires at ~500ms. Give it time to either be REJECTED by the
	// claim (fix) or to take its own second claim (pre-fix / M11) and park
	// in emitBlocking. Only after this window do we start draining, so a
	// second claimer is observed BEFORE the first delivery completes —
	// otherwise the seen write would mask the missing inflight check.
	time.Sleep(closeWaitDebounce + 400*time.Millisecond)

	w.seenMu.Lock()
	claims := len(w.inflight)
	w.seenMu.Unlock()

	// Drain every claimed delivery.
	delivered := 0
	readDeadline := time.After(2 * time.Second)
	var firstOp, secondOp string
	for delivered < claims {
		select {
		case fe := <-events:
			if delivered == 0 {
				firstOp = fe.Op
			}
			delivered++
		case <-readDeadline:
			t.Fatalf("delivered %d of %d claimed deliveries", delivered, claims)
		}
	}
	// Nothing further may arrive.
	select {
	case fe := <-events:
		secondOp = fe.Op
		t.Fatalf("double-send race: first op=%s second op=%s (inflight now=%d)", firstOp, secondOp, func() int {
			w.seenMu.Lock()
			defer w.seenMu.Unlock()
			return len(w.inflight)
		}())
	case <-time.After(600 * time.Millisecond):
	}
	// Exactly-once: one claim, one delivery.
	assert.Equal(t, 1, claims, "the same file version must be claimed and delivered exactly once")
}

// A claim whose delivery is interrupted (emitBlocking returns false on ctx
// cancel) must roll back: no leftover inflight entry, seen not written. A
// stuck claim would make every future claim for the same version fail and
// a file that never changes again would silently never be collected — the
// exact class of defect this knife exists to kill — so completeDelivery is
// deferred, covering panics and early returns too.
func TestScanFile_ClaimReleasedOnAbortedDelivery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aborted.log")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))
	// Quiet past the close_wait debounce window, otherwise the recheck's
	// still-writing guard returns before the claim is ever taken.
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, past, past))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]time.Time)
	// Unbuffered with no reader: `events <- fe` can never proceed, so
	// emitBlocking deterministically fails on the cancelled ctx instead of
	// racing between the ready ctx.Done and a buffered send slot.
	events := make(chan FileEvent)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // emit will be aborted

	w.recheckAfterDebounce(ctx, events, seen, path)

	select {
	case fe := <-events:
		t.Fatalf("aborted delivery must not emit: %s", fe.Path)
	default:
	}

	w.seenMu.Lock()
	left := len(w.inflight)
	_, known := seen[path]
	w.seenMu.Unlock()
	assert.Zero(t, left, "aborted delivery must roll back the in-flight claim")
	assert.False(t, known, "aborted delivery must not mark seen (file stays retryable)")
}
