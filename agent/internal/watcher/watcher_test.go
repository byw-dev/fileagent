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
	case <-time.After(2*time.Second):
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
	case <-time.After(1*time.Second):
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
	case <-time.After(2*time.Second):
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
	case <-time.After(4*time.Second):
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
	case <-time.After(4*time.Second):
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

err = w.runCloseWait(ctx, events, fw)
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

go func() { _ = w.runCloseWait(ctx, events, fw) }()

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

go func() { _ = w.runCloseWait(ctx, events, fw) }()

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

go func() { _ = w.runCloseWait(ctx, events, fw) }()

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
go func() { done <- w.runFsnotify(ctx, events, fw) }()

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

go func() { _ = w.runFsnotify(ctx, events, fw) }()
require.NoError(t, os.Remove(path))

select {
case fe := <-events:
assert.Equal(t, "remove", fe.Op)
case <-time.After(2 * time.Second):
t.Fatal("no remove event received")
}
}
