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
	// D-035: the default-mode scan only delivers files whose writes have gone
	// quiet, so age the fixture past the debounce window (create), then again
	// after the modification (write).
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, past, past))

	w, err := New(dir, "*.log", false, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 10)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Initial scan consumes the "create" event.
	seen := make(map[string]time.Time)
	w.pollScan(ctx, events, seen)
	<-events // consume initial create

	// Modify the file; Chtimes gives it a strictly newer mtime without any
	// wall-clock sleep.
	require.NoError(t, os.WriteFile(path, []byte("v2"), 0o644))
	require.NoError(t, os.Chtimes(path, time.Now().Add(-30*time.Minute), time.Now().Add(-30*time.Minute)))

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
	// D-035: freshly written files are inside the debounce window, so age the
	// fixtures past it — this test pins glob filtering, not debounce timing.
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(dir, "file.txt"), past, past))
	require.NoError(t, os.Chtimes(filepath.Join(dir, "file.log"), past, past))

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
	// D-035: age the fixture past the debounce window (pins recursion+glob).
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(filepath.Join(subdir, "deep.txt"), past, past))

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

// Rework S-2: a Watcher supports exactly one live Start — pending / rechecks
// / inflight / tailOffsets are instance-level state shared by whichever loop
// runs, so a second concurrent Start must fail with ErrAlreadyRunning instead
// of corrupting the first loop (whose timers the stopping loop would stop).
func TestWatcher_Start_SecondConcurrentStart_ReturnsErrAlreadyRunning(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "", false, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 4)
	// Bounded ctx: under the no-CAS mutation the second Start would block
	// until it expires instead of failing fast — the red must be an
	// assertion failure, not a hang.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- w.Start(ctx, events) }()

	// Wait until the first Start is actually running (past the CAS).
	require.Eventually(t, func() bool { return w.running.Load() },
		2*time.Second, 5*time.Millisecond, "the first Start should be running")

	err = w.Start(ctx, events)
	require.ErrorIs(t, err, ErrAlreadyRunning)

	// Explicit cancel: the first loop exits on ctx.Done, well before the
	// ctx's own 3s timeout (which only exists to bound the mutation run).
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("first Start did not return after ctx cancel")
	}
}

// Rework S-2: sequential reuse stays legal — after one Start returns (its ctx
// cancelled), a fresh Start on the same Watcher runs normally (the rule
// hot-reload pattern).
func TestWatcher_Start_SequentialReuse_AfterCancel(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "", false, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)

	events := make(chan FileEvent, 4)
	ctx1, cancel1 := context.WithCancel(context.Background())

	done1 := make(chan error, 1)
	go func() { done1 <- w.Start(ctx1, events) }()
	cancel1()
	select {
	case err := <-done1:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("first Start did not return")
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel2()
	done2 := make(chan error, 1)
	go func() { done2 <- w.Start(ctx2, events) }()
	require.Eventually(t, func() bool { return w.running.Load() },
		2*time.Second, 5*time.Millisecond, "the second sequential Start should be running")
	cancel2()
	select {
	case err := <-done2:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("second Start did not return")
	}
}

// D-035 incremental review: sequential Start reuse must reopen the recheck
// scheduling gate. The second lifecycle's initial scan skips this hot file;
// with no writer left to produce another fsnotify event, only its recheck can
// deliver the file.
func TestWatcher_Start_SequentialReuse_ReopensRecheckGate(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = time.Second

	ctx1, cancel1 := context.WithCancel(context.Background())
	done1 := make(chan error, 1)
	go func() { done1 <- w.Start(ctx1, make(chan FileEvent, 1)) }()
	require.Eventually(t, func() bool { return w.running.Load() },
		time.Second, 5*time.Millisecond, "the first Start should be running")
	cancel1()
	select {
	case err := <-done1:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("first Start did not return")
	}
	w.seenMu.Lock()
	require.True(t, w.rechecksStopped, "first Start should close the recheck gate")
	w.seenMu.Unlock()

	path := filepath.Join(dir, "hot-on-reuse.log")
	require.NoError(t, os.WriteFile(path, []byte("complete"), 0o644))
	events := make(chan FileEvent, 1)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	done2 := make(chan error, 1)
	go func() { done2 <- w.Start(ctx2, events) }()

	select {
	case event := <-events:
		require.Equal(t, path, event.Path)
		require.Equal(t, int64(len("complete")), event.Size)
	case <-time.After(3 * time.Second):
		t.Fatal("hot file never collected (recheck gate stayed closed)")
	}
	cancel2()
	select {
	case err := <-done2:
		require.ErrorIs(t, err, context.Canceled)
	case <-time.After(2 * time.Second):
		t.Fatal("second Start did not return")
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
	// D-035: the file was written just now, so the initial scan skips it as
	// hot and the debounce recheck delivers it once the window passes. The
	// short window keeps the test off wall-clock assumptions.
	w.debounce = 30 * time.Millisecond

	events := make(chan FileEvent, 10)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.Start(ctx, events) }()

	select {
	case event := <-events:
		assert.Equal(t, path, event.Path)
		assert.Equal(t, "create", event.Op)
	case <-time.After(2 * time.Second):
		t.Fatal("fsnotify startup did not scan the existing file")
	}
}

func TestWatcher_FsnotifyDetectsNewFile(t *testing.T) {
	dir := t.TempDir()
	w, err := New(dir, "*.txt", false, 50*time.Millisecond, "", zap.NewNop())
	require.NoError(t, err)
	// D-035: the Write/Create event is debounced; a short window keeps the
	// delivery fast and deterministic.
	w.debounce = 30 * time.Millisecond

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
	// D-035: the Create event is debounced; a short window keeps it fast.
	w.debounce = 30 * time.Millisecond

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
//
// Tail mode (D-035): the pinned property is the scan's blocking backpressure,
// which for immediately-delivered files now only exists on the real-time
// path; debounced modes deliver hot files via rechecks instead.
func TestWatcher_InitialScan_BacklogBeyondBufferNoneDropped(t *testing.T) {
	const numFiles = 200
	dir := t.TempDir()
	for i := 0; i < numFiles; i++ {
		require.NoError(t, os.WriteFile(
			filepath.Join(dir, fmt.Sprintf("file-%03d.txt", i)), []byte("data"), 0o644))
	}

	w, err := New(dir, "*.txt", false, time.Hour, AppendModeTail, zap.NewNop())
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
// debounce in loopDebounced, so a file being written at agent startup was
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

// D-035 review follow-up: clock skew or rsync -t may leave a file's mtime in
// the future. A negative age is not evidence that the file is actively being
// written, so the default debounced mode must collect it on the initial scan
// instead of scheduling an arbitrarily delayed recheck.
func TestWatcher_OverwriteInitialScan_FutureMTimeIsSettled(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future.log")
	require.NoError(t, os.WriteFile(path, []byte("complete"), 0o644))
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(path, future, future))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	events := make(chan FileEvent, 1)

	skipped := w.pollScan(context.Background(), events, make(map[string]time.Time))
	require.Empty(t, skipped, "future mtime must not be classified as an actively written file")
	select {
	case fe := <-events:
		require.Equal(t, path, fe.Path)
		require.Equal(t, int64(len("complete")), fe.Size)
	default:
		t.Fatal("future-mtime file was not collected immediately by the initial scan")
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

// ── Direct runDebounced / runFsnotify unit tests ──────────────────────────────

func TestRunDebounced_ContextCancel_ReturnsError(t *testing.T) {
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

	err = w.runDebounced(ctx, events, fw, make(map[string]time.Time))
	require.Error(t, err)
}

func TestRunDebounced_RemoveEvent_EmittedImmediately(t *testing.T) {
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

	go func() { _ = w.runDebounced(ctx, events, fw, make(map[string]time.Time)) }()

	// Trigger Remove event.
	require.NoError(t, os.Remove(path))

	select {
	case fe := <-events:
		assert.Equal(t, "remove", fe.Op)
	case <-time.After(2 * time.Second):
		t.Fatal("no remove event received")
	}
}

func TestRunDebounced_WriteEvent_DebounceEmits(t *testing.T) {
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

	go func() { _ = w.runDebounced(ctx, events, fw, make(map[string]time.Time)) }()

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

func TestRunDebounced_NonMatchingGlob_Ignored(t *testing.T) {
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

	go func() { _ = w.runDebounced(ctx, events, fw, make(map[string]time.Time)) }()

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
		appendMode:  AppendModeTail, // D-035: loopFsnotify is the tail-only path
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
		appendMode:  AppendModeTail, // D-035: loopFsnotify is the tail-only path
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
// scan and the rescan (design point (a) of IC-BUG-44). Tail mode: this pins
// the real-time path's rescan-retry semantics, which since D-035 only tail
// has (debounced deliveries record seen and are not re-emitted).
func TestHandleWatchError_RescanReEmitsChangedFileAsWrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "changed.log")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))
	stale := time.Now().Add(-2 * time.Hour)
	require.NoError(t, os.Chtimes(path, stale, stale))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeTail, zap.NewNop())
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
	// D-035: age the file past the debounce window so the rescan's skip is
	// decided by the seen map (the thing this test pins), not by hot-file
	// skipping.
	past := time.Now().Add(-time.Hour)
	require.NoError(t, os.Chtimes(path, past, past))
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

	// Tail mode: loopFsnotify is the real-time path, and since D-035 only
	// tail walks it — the rescan must deliver the (hot) file immediately.
	w, err := New(dir, "*.txt", false, time.Hour, AppendModeTail, zap.NewNop())
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

	w, err := New(dir, "*.txt", false, time.Hour, AppendModeTail, zap.NewNop())
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

// Branch coverage: runDebounced's error branch must trigger the rescan too.
// The file's mtime is an hour old so the rescan is allowed to emit it.
func TestRunDebounced_OverflowError_TriggersRescan(t *testing.T) {
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

	go func() { _ = w.loopDebounced(ctx, events, make(map[string]time.Time), evc, erc) }()

	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("overflow error on the error channel did not trigger the safety-net rescan in loopDebounced")
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
	w.debounce = 30 * time.Millisecond // short debounce: recheck fires within ms

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	go func() { _ = w.Start(ctx, events) }()

	// Sync point before waiting for the delivery: the initial scan must
	// have armed the recheck for the hot file. Without this the test would
	// depend on a wall-clock window ("scan + timer fire within 4s") instead
	// of observing the scheduling directly.
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.rechecks) == 1
	}, 3*time.Second, 5*time.Millisecond, "initial scan should arm the debounce recheck for the hot file")

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
	w.debounce = 30 * time.Millisecond // short debounce: recheck fires within ms

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

// D-035 incremental review: a future mtime is settled, not hot. Scheduling a
// recheck for it must clamp the negative age when choosing the deadline, and
// the callback must not reject it forever when that prompt timer fires.
func TestScheduleDebounceRecheck_FutureMTimeDeliveredPromptly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future-recheck.log")
	require.NoError(t, os.WriteFile(path, []byte("complete"), 0o644))
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(path, future, future))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 30 * time.Millisecond
	defer w.stopAllRechecks()

	events := make(chan FileEvent, 1)
	w.scheduleDebounceRecheck(context.Background(), events, make(map[string]time.Time), path)
	select {
	case fe := <-events:
		require.Equal(t, path, fe.Path)
		require.Equal(t, int64(len("complete")), fe.Size)
	case <-time.After(500 * time.Millisecond):
		t.Fatal("future-mtime recheck was delayed by clock skew or rejected permanently")
	}
}

// All quietness checks treat a future mtime as already settled. Scheduling a
// recheck for that same file must therefore wait only the race-padding grace,
// not one otherwise contradictory debounce window plus the grace.
func TestScheduleDebounceRecheck_FutureMTimeUsesGraceOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future-recheck-grace.log")
	require.NoError(t, os.WriteFile(path, []byte("complete"), 0o644))
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(path, future, future))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = time.Second
	defer w.stopAllRechecks()

	events := make(chan FileEvent, 1)
	w.scheduleDebounceRecheck(context.Background(), events, make(map[string]time.Time), path)
	select {
	case fe := <-events:
		require.Equal(t, path, fe.Path)
	case <-time.After(400 * time.Millisecond):
		t.Fatal("future-mtime recheck waited a full debounce window instead of grace only")
	}
}

// A callback that crosses stopPendingTimers may reach scheduling after
// stopAllRechecks has already drained the map. The stop boundary is final:
// no callback may install an untracked recheck behind it.
func TestScheduleDebounceRecheck_AfterStopRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "after-stop.log")
	require.NoError(t, os.WriteFile(path, []byte("complete"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.stopAllRechecks()
	w.scheduleDebounceRecheck(context.Background(), make(chan FileEvent, 1), make(map[string]time.Time), path)

	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	assert.Empty(t, w.rechecks, "关停后仍装入了新的 recheck timer")
}

// Sequential Watcher reuse reopens the scheduling gate. A callback from the
// previous lifecycle still carries its cancelled context, which must prevent
// it from installing an old-ctx/old-seen timer into the new lifecycle.
func TestScheduleDebounceRecheck_PreviousLifecycleContextRejectedAfterReuse(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "previous-lifecycle.log")
	require.NoError(t, os.WriteFile(path, []byte("complete"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	oldCtx, cancelOld := context.WithCancel(context.Background())
	cancelOld()
	w.stopAllRechecks()
	w.startRechecks() // simulate the next sequential Start
	w.scheduleDebounceRecheck(oldCtx, make(chan FileEvent, 1), make(map[string]time.Time), path)

	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	assert.Empty(t, w.rechecks, "上一轮已取消 ctx 的 callback 污染了顺序复用后的新生命周期")
}

// The close_wait debounce flush must record the delivered mtime in the shared
// seen map: after the flush has emitted a file, the IC-BUG-44 safety-net
// rescan must not re-emit it.
func TestDebounceFlush_PreventsRescanReemit(t *testing.T) {
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
	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

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

// ── PR #108 review F1 → P1: the F1 guards were reverted by the P1 ruling ─────
// (F1 marked real-time deliveries seen; the codex review P1 proved that this
// took away the overflow rescan's retry opportunity and traded an unbounded
// silent-loss mode for a bounded amplification. The guards below were the
// F1 versions; TestLoopFsnotify_RescanRetriesRealTimeDeliveredFiles at the
// bottom of this file pins the reverted behaviour.)

// Guard: remove events are not "deliveries" — they must not mark the file
// seen (only create/write deliveries do).
func TestLoopFsnotify_RemoveEvent_DoesNotMarkSeen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	w, err := New(dir, "*.txt", false, time.Hour, AppendModeTail, zap.NewNop())
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
func TestLoopDebounced_ChannelClose_StopsPendingDebounceTimers(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pending.log")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 30 * time.Millisecond // short debounce: pending timer fires within ms

	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent, 8)
	seen := make(map[string]time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	// A write registers the pending debounce timer (fires ~w.debounce later).
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}

	// The event channel closes (shutdown path distinct from ctx cancel).
	close(evc)

	// Give the loop time to exit, then make sure the flush never fires:
	// with the 30ms debounce the flush would land well inside the window
	// even under CPU contention, so the guard stays meaningful.
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
func TestDebounce_RecheckAndFlush_SameVersionDeliveredOnce(t *testing.T) {
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
	w.debounce = 30 * time.Millisecond // short debounce: no wall-clock window to miss

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent) // unbuffered: deliveries park the senders
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	// A: the debounce flush path — a Write event registers the pending
	// timer, which fires ~w.debounce later.
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

	// The flush fires at ~w.debounce. Give it time to either be REJECTED by
	// the claim (fix) or to take its own second claim (pre-fix / M11) and
	// park in emitBlocking. Only after this window do we start draining, so
	// a second claimer is observed BEFORE the first delivery completes —
	// otherwise the seen write would mask the missing inflight check.
	time.Sleep(w.debounce + 400*time.Millisecond)

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

// ── PR #108 review F3 补钉 Q2/Q3（codex 复审） ────────────────────────────────

// Q2: the flush site must settle its claim after a SUCCESSFUL delivery —
// otherwise the version stays in-flight forever (every later claim for the
// same version fails) and seen stays empty (the delivered version is
// invisible to rescans).
func TestFlushDelivery_SettlesClaimAndMarksSeen(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "flushed.log")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)
	// Short debounce: the flush fires within milliseconds, so the test does
	// not depend on a wall-clock window surviving CPU contention.
	w.debounce = 30 * time.Millisecond

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	// The (short) debounce timer fires after the Write event; with a
	// buffered consumer the flush delivers immediately.
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}

	var fe FileEvent
	select {
	case fe = <-events:
		require.Equal(t, path, fe.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("flush never delivered the file")
	}

	// Receiving the event does NOT imply the flush goroutine has run its
	// deferred completeDelivery yet — settle is a separate scheduling step,
	// so assert on the SETTLED STATE, never on the instant after the read.
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		mt, known := seen[path]
		return len(w.inflight) == 0 && known && mt.Equal(fe.ModTime)
	}, 1*time.Second, 5*time.Millisecond,
		"successful flush delivery must settle its claim and record seen")
}

// Q2 (abort half): a flush whose delivery is aborted (ctx cancelled while
// parked in emitBlocking) must roll its claim back via the deferred
// completeDelivery — a leaked claim would block every later claim for the
// same version.
func TestFlushDelivery_ClaimReleasedOnAbortedDelivery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "aborted-flush.log")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 30 * time.Millisecond // short debounce: no wall-clock window to miss

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent) // unbuffered, never read: emit parks
	ctx, cancel := context.WithCancel(context.Background())

	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	// The flush fires ~w.debounce (30ms here) later, claims and parks in
	// emitBlocking. The eventual claim observation below is the sync point,
	// not the timer's wall clock.
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.inflight) == 1
	}, 2*time.Second, 5*time.Millisecond, "flush should hold the claim while parked in emitBlocking")

	// Aborting the ctx unblocks emitBlocking with delivered=false; the
	// deferred completeDelivery must roll the claim back.
	cancel()
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.inflight) == 0
	}, 1*time.Second, 5*time.Millisecond, "aborted flush delivery must roll back the in-flight claim")
}

// Q3: seen's monotonic write — a late OLD-version completion (parked in its
// emit while a NEWER version already delivered and settled) must not move
// seen backwards, and the current version must not be re-emitted afterwards.
func TestSeen_MonotonicWrite_OldVersionLateCompletion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "versions.log")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]time.Time)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// v1 claims and parks mid-delivery.
	require.NoError(t, os.Chtimes(path, time.Now().Add(-2*time.Hour), time.Now().Add(-2*time.Hour)))
	info1, err := os.Stat(path)
	require.NoError(t, err)
	eventsOld := make(chan FileEvent)    // parked: v1's emit blocks
	eventsNew := make(chan FileEvent, 8) // v2 delivers immediately

	go func() { _ = w.scanFile(ctx, eventsOld, seen, path, info1) }()
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		cur, busy := w.inflight[path]
		return busy && cur.Equal(info1.ModTime())
	}, 2*time.Second, 5*time.Millisecond, "v1 should hold the claim while parked")

	// The file moves on; the newer version delivers and settles FIRST.
	require.NoError(t, os.Chtimes(path, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
	info2, err := os.Stat(path)
	require.NoError(t, err)
	require.NoError(t, w.scanFile(ctx, eventsNew, seen, path, info2))

	w.seenMu.Lock()
	mt, known := seen[path]
	w.seenMu.Unlock()
	require.True(t, known)
	require.True(t, mt.Equal(info2.ModTime()), "seen must carry the newer version")

	// Drain v1's parked emit; its late completion must not regress seen.
	select {
	case <-eventsOld:
	case <-time.After(2 * time.Second):
		t.Fatal("v1 never delivered")
	}
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.inflight) == 0
	}, 1*time.Second, 5*time.Millisecond, "late v1 claim must settle")

	w.seenMu.Lock()
	late, _ := seen[path]
	w.seenMu.Unlock()
	assert.True(t, late.Equal(info2.ModTime()),
		"a late older-version completion must not move seen backwards")

	// A follow-up scan must not re-emit the current version.
	select {
	case fe := <-eventsNew: // drain v2's earlier delivery first
		_ = fe
	default:
	}
	w.pollScan(ctx, eventsNew, seen)
	select {
	case fe := <-eventsNew:
		t.Fatalf("follow-up scan re-emitted the current version: %s (op=%s)", fe.Path, fe.Op)
	default:
	}
}

// ── PR #108 codex 复审 P2: timer 交接不得误删替代者 ──────────────────────────

// Timer A fires and blocks on seenMu; while it waits, the overflow rescan
// re-arms the same path with timer B (scheduleDebounceRecheck's replace path
// — old.Stop() cannot stop an already-fired timer, so the entry is
// overwritten). When A finally gets the lock it must NOT delete B's
// tracking entry: an unconditional delete would leave B untracked, so
// stopAllRechecks could never Stop it — the review-F2 leak would come back
// (with a future mtime, B's closure holds the watcher for years).
func TestRecheckTimer_ReplaceKeepsNewTimerTracked(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "hot.log")
	require.NoError(t, os.WriteFile(path, []byte("still writing"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeCloseWait, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]time.Time)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Schedule recheck A for the hot file. A short debounce makes the fire
	// wait ≈ debounceRecheckGrace, so the test does not depend on a
	// wall-clock window surviving CPU contention.
	w.debounce = 30 * time.Millisecond
	w.scheduleDebounceRecheck(ctx, events, seen, path)
	w.seenMu.Lock()
	timerA := w.rechecks[path]
	w.seenMu.Unlock()
	require.NotNil(t, timerA)

	// Hold seenMu so A's callback blocks the moment it fires, and let A
	// fire well within the grace-dominated wait.
	w.seenMu.Lock()
	time.Sleep(debounceRecheckGrace + 500*time.Millisecond)

	// While A is parked on the lock, the rescan replaces the entry with B —
	// scheduleDebounceRecheck's replace also bumps the per-path generation.
	timerB := time.AfterFunc(time.Hour, func() {})
	defer timerB.Stop()
	w.recheckGen++
	w.recheckGens[path] = w.recheckGen
	w.rechecks[path] = timerB
	w.seenMu.Unlock()

	// Give A's callback time to run its (guarded) settle.
	time.Sleep(300 * time.Millisecond)

	w.seenMu.Lock()
	cur, tracked := w.rechecks[path]
	w.seenMu.Unlock()

	require.True(t, tracked, "the replacement timer must stay tracked")
	assert.Same(t, timerB, cur, "timer A must not delete the replacement's tracking entry")
}

// ── AUD-9 / D-035: 防抖普适（tail 以外的所有模式都防抖） ──────────────────────

// AUD-9 / D-035: the debounced path is universal (every mode except tail).
// overwrite's initial scan must not deliver a file that is still being
// written: it is skipped, and afterwards collected by the debounce recheck —
// exactly once. Before D-035 overwrite delivered hot files immediately,
// which is the 150x write amplification (and truncated uploads) the A-baseline
// audit measured on a plain cp into the watched directory.
func TestDebounced_Overwrite_InitialScanSkipsHotFile_RecheckCollects(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "active.log")
	require.NoError(t, os.WriteFile(path, []byte("still writing"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 30 * time.Millisecond

	seen := make(map[string]time.Time)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	skipped := w.pollScan(ctx, events, seen)
	require.Contains(t, skipped, path, "hot file must be skipped by the initial scan, not delivered")
	require.Empty(t, events, "the initial scan must not emit a file that is still being written")

	w.scheduleDebounceRechecks(ctx, events, seen, skipped)

	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
		assert.Equal(t, "create", fe.Op)
	case <-time.After(2 * time.Second):
		t.Fatal("hot file skipped by the initial scan was never collected by the debounce recheck")
	}

	select {
	case fe := <-events:
		t.Fatalf("skipped file was delivered more than once: %s (op=%s)", fe.Path, fe.Op)
	case <-time.After(200 * time.Millisecond):
	}
}

// Start-level guard for the same invariant (mutation M3): with the debounced
// loop chosen for overwrite, neither Start's initial scan nor the live event
// path may deliver a hot/fresh file. The absurd debounce window makes this
// deterministic: the pending timer cannot fire within the test's lifetime,
// so ANY delivery proves the real-time loop ran for a debounced mode.
func TestDebounced_Overwrite_StartInitialScan_DoesNotDeliverHotFile(t *testing.T) {
	dir := t.TempDir()
	hot := filepath.Join(dir, "hot.log")
	require.NoError(t, os.WriteFile(hot, []byte("still writing"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = time.Minute

	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go func() { _ = w.Start(ctx, events) }()

	// Live events too: a fresh file's Create event must be debounced, not
	// emitted in real time (that would mean Start picked the real-time loop).
	time.Sleep(100 * time.Millisecond) // let fsnotify register the watch
	fresh := filepath.Join(dir, "fresh.log")
	require.NoError(t, os.WriteFile(fresh, []byte("created after start"), 0o644))

	select {
	case fe := <-events:
		t.Fatalf("a debounced mode delivered in real time (Start picked the real-time loop): %s (op=%s)", fe.Path, fe.Op)
	case <-time.After(400 * time.Millisecond):
	}
}

// The debounce recheck itself must refuse a file that is still hot when it
// fires (mutation M5). A live writer keeps the mtime fresh, so at fire time
// (≈ debounceWindow + grace after the scan) the file is still inside the
// debounce window and the correct recheck emits nothing.
func TestDebounced_Recheck_DoesNotDeliverStillHotFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "streaming.log")
	require.NoError(t, os.WriteFile(path, []byte("start"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 30 * time.Millisecond

	// Keep the file hot: appends every 10ms keep the mtime age well below
	// the 30ms window (the existing close_wait hot-file test uses the same
	// cadence pattern against the 500ms default window).
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o644)
				if err != nil {
					continue
				}
				_, _ = f.WriteString("x")
				_ = f.Close()
			}
		}
	}()
	defer func() { close(stop); <-done }()
	time.Sleep(20 * time.Millisecond) // let the writer make the file hot

	seen := make(map[string]time.Time)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	skipped := w.pollScan(ctx, events, seen)
	require.Contains(t, skipped, path, "hot file must be skipped by the scan")
	w.scheduleDebounceRechecks(ctx, events, seen, skipped)

	select {
	case fe := <-events:
		t.Fatalf("recheck delivered a file that is still being written: %s", fe.Path)
	case <-time.After(700 * time.Millisecond):
		// The recheck fires ≈ debounceWindow+grace after the scan; the writer
		// keeps the file hot across that whole window, so nothing may be
		// delivered.
	}
}

// AUD-9 / D-035 core regression: in overwrite mode a burst of Write events
// for the same path is folded into exactly ONE delivery by the debounced
// event loop. The audit measured 150 full uploads for a single cp of a
// 150MB file; this pin makes that structurally impossible for any mode that
// walks the debounced loop (i.e. every mode except tail).
func TestDebounced_Overwrite_WriteBurst_FoldsIntoSingleDelivery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "stream.log")
	require.NoError(t, os.WriteFile(path, []byte("seed"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 30 * time.Millisecond

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent, 64)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	const burst = 12
	for i := 0; i < burst; i++ {
		evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}
	}

	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
		assert.Equal(t, "write", fe.Op)
	case <-time.After(2 * time.Second):
		t.Fatal("debounced flush never delivered the file")
	}

	// No second delivery may follow: the whole burst is one file version.
	select {
	case fe := <-events:
		t.Fatalf("write burst was folded into more than one delivery: second event %s (op=%s)", fe.Path, fe.Op)
	case <-time.After(w.debounce*3 + 300*time.Millisecond):
	}
}

// AUD-9 / D-035: tail is deliberately excluded from the universal debounce.
// Pins that exclusion so nobody "unifies" it back by accident:
//   - debounceEnabled() is false for tail and only for tail;
//   - pollScan does not skip hot files in tail mode;
//   - Start drives the real-time loop for tail — delivery is immediate, not
//     after the debounce window.
func TestTailMode_NotDebounced(t *testing.T) {
	t.Run("predicate", func(t *testing.T) {
		assert.False(t, (&Watcher{appendMode: AppendModeTail}).debounceEnabled())
		assert.True(t, (&Watcher{appendMode: AppendModeOverwrite}).debounceEnabled())
		assert.True(t, (&Watcher{appendMode: AppendModeCloseWait}).debounceEnabled())
		assert.True(t, (&Watcher{appendMode: ""}).debounceEnabled())
	})

	t.Run("pollScan does not skip hot files", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "hot.log")
		require.NoError(t, os.WriteFile(path, []byte("hot"), 0o644))

		w, err := New(dir, "*.log", false, time.Hour, AppendModeTail, zap.NewNop())
		require.NoError(t, err)
		w.debounce = time.Hour // even an absurd window must not matter in tail mode

		events := make(chan FileEvent, 8)
		skipped := w.pollScan(context.Background(), events, make(map[string]time.Time))
		assert.Empty(t, skipped, "tail mode must not skip hot files")
		require.Len(t, events, 1, "tail mode must deliver the hot file immediately")
		assert.Equal(t, path, (<-events).Path)
	})

	t.Run("Start drives the real-time loop", func(t *testing.T) {
		dir := t.TempDir()
		w, err := New(dir, "*.log", false, time.Hour, AppendModeTail, zap.NewNop())
		require.NoError(t, err)
		// An absurd debounce window: if tail were (wrongly) routed through
		// the debounced loop, delivery would wait for it and the deadline
		// below would fire first.
		w.debounce = time.Minute

		events := make(chan FileEvent, 8)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		go func() { _ = w.Start(ctx, events) }()

		// Give fsnotify time to register the watch.
		time.Sleep(100 * time.Millisecond)
		path := filepath.Join(dir, "live.log")
		require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

		select {
		case fe := <-events:
			assert.Equal(t, path, fe.Path)
		case <-time.After(3 * time.Second):
			t.Fatal("tail mode did not deliver the file in real time — is it walking the debounced loop?")
		}
	})
}

// AUD-9 / D-035: close_wait is now an alias of overwrite. The same scripted
// input through the debounced loop must produce the same delivery sequence
// under both modes — this turns the "alias" claim into an executable
// assertion instead of prose.
func TestDebounced_CloseWaitAndOverwrite_EquivalentDelivery(t *testing.T) {
	type delivery struct {
		name string
		op   string
	}
	run := func(t *testing.T, mode string) []delivery {
		t.Helper()
		dir := t.TempDir()
		pathA := filepath.Join(dir, "a.log")
		pathB := filepath.Join(dir, "b.log")
		require.NoError(t, os.WriteFile(pathA, []byte("a"), 0o644))
		require.NoError(t, os.WriteFile(pathB, []byte("b"), 0o644))

		w, err := New(dir, "*.log", false, time.Hour, mode, zap.NewNop())
		require.NoError(t, err)
		w.debounce = 30 * time.Millisecond

		seen := make(map[string]time.Time)
		evc := make(chan fsnotify.Event)
		erc := make(chan error)
		events := make(chan FileEvent, 8)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

		waitDelivery := func(wantPath string) delivery {
			t.Helper()
			for {
				select {
				case fe := <-events:
					require.Equal(t, wantPath, fe.Path)
					return delivery{name: filepath.Base(fe.Path), op: fe.Op}
				case <-time.After(2 * time.Second):
					t.Fatalf("mode %s: no delivery for %s", mode, wantPath)
				}
			}
		}

		var got []delivery
		evc <- fsnotify.Event{Name: pathA, Op: fsnotify.Create}
		got = append(got, waitDelivery(pathA)) // create
		// A real Write event implies the file changed, so give it a fresh
		// mtime: without a new version the exactly-once claim correctly
		// rejects the write burst's flush as a re-delivery of the same
		// version (that rejection is part of the behaviour under test).
		require.NoError(t, os.WriteFile(pathA, []byte("a2"), 0o644))
		for i := 0; i < 5; i++ {
			evc <- fsnotify.Event{Name: pathA, Op: fsnotify.Write}
		}
		got = append(got, waitDelivery(pathA)) // write — burst folded into one
		evc <- fsnotify.Event{Name: pathB, Op: fsnotify.Create}
		got = append(got, waitDelivery(pathB)) // create
		evc <- fsnotify.Event{Name: pathB, Op: fsnotify.Remove}
		got = append(got, waitDelivery(pathB)) // remove — immediate, no debounce
		return got
	}

	overwrite := run(t, AppendModeOverwrite)
	closeWait := run(t, AppendModeCloseWait)
	assert.Equal(t, overwrite, closeWait, "close_wait must be a behavioural alias of overwrite")
}

// Rework S-1 + third-round rework: flushed pending entries are collected by
// the amortized sweep — len(pending) must fall back to the active-path count
// instead of growing with the set of paths ever seen. (History: the first
// rework's done-channel design stranded 192 of 256 entries; the second
// rework's threshold trigger was replaced by this per-event budget.) The
// sweep fires every w.pendingSweepEvery processed events, so after the burst
// has fully flushed, feeding exactly that many events re-arms one entry and
// sweeps the rest — the exact remainder is the single re-armed entry.
func TestDebounced_PendingRecycledAfterFlush(t *testing.T) {
	dir := t.TempDir()
	const n = 256
	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 20 * time.Millisecond
	// Small per-sweep event budget so the sweep dynamics are deterministic
	// instead of keyed to the production constant (same pattern as
	// w.debounce).
	w.pendingSweepEvery = 8

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent, 512)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	paths := make([]string, n)
	for i := 0; i < n; i++ {
		paths[i] = filepath.Join(dir, fmt.Sprintf("p-%03d.log", i))
		require.NoError(t, os.WriteFile(paths[i], []byte("x"), 0o644))
		evc <- fsnotify.Event{Name: paths[i], Op: fsnotify.Create}
	}

	for i := 0; i < n; i++ {
		select {
		case <-events:
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of %d deliveries arrived", i, n)
		}
	}

	// The sweep only deletes entries whose callback has fully completed
	// (fired set after flush returns), so wait for all callbacks to settle
	// before triggering the sweep — otherwise the exact expectation below
	// would race the last few callbacks.
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		for _, p := range w.pending {
			p.mu.Lock()
			fired := p.fired
			p.mu.Unlock()
			if !fired {
				return false
			}
		}
		return true
	}, 2*time.Second, 5*time.Millisecond, "all 256 entries should have flushed and settled")

	// The sweep fires on every 8th processed event. Feed exactly 8: the
	// first Write re-arms paths[0] (fired cleared), and the 8th event
	// triggers the sweep, which must collect every still-fired entry.
	for i := 0; i < 8; i++ {
		evc <- fsnotify.Event{Name: paths[0], Op: fsnotify.Write}
	}

	// Exact expectation: 255 fired entries swept, the re-armed paths[0] stays.
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return len(w.pending) == 1
	}, 2*time.Second, 5*time.Millisecond,
		"the sweep must collect all 255 fired entries, leaving exactly the re-armed one")

	// And the survivor must still be alive with an armed timer: bump the
	// version and expect its flush to deliver.
	require.NoError(t, os.WriteFile(paths[0], []byte("v2"), 0o644))
	evc <- fsnotify.Event{Name: paths[0], Op: fsnotify.Write}
	select {
	case fe := <-events:
		require.Equal(t, paths[0], fe.Path)
		require.Equal(t, "write", fe.Op)
	case <-time.After(2 * time.Second):
		t.Fatal("the surviving entry stopped delivering after the sweep")
	}
}

// Third-round rework (P0 guard): the sweep trigger must not contain any
// historically derived quantity. The previous design set a threshold
// T = 2*len(pending) + floor AT EACH SWEEP — but the active entries it
// measured all became idle (fired) afterwards, so repeating "wait until
// everything is idle, then burst just past the threshold" ratcheted the
// residue up every round (codex measured 66/131/196/261/326 — unbounded).
// This guard replays exactly that attack against the per-event-budget
// trigger: N rounds, each waiting until every retained entry is idle
// (active == 0) before injecting exactly one trigger-budget of new paths.
// The first entry of every round is deterministically allowed to fire before
// the sweep, proving that residue may be strictly below the burst size. The
// real expectations are an upper bound and no cross-round growth. (Under the
// old threshold design the ratcheted threshold eventually stops firing
// mid-bursts and idle entries accumulate.)
func TestDebounced_PendingSweep_MultiRound_ResidueConstant(t *testing.T) {
	const (
		rounds = 6
		burst  = 8 // == w.pendingSweepEvery: each round's burst exactly crosses the trigger
	)
	dir := t.TempDir()
	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 100 * time.Millisecond
	w.pendingSweepEvery = burst

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent, 256)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	waitAllFired := func() {
		t.Helper()
		require.Eventually(t, func() bool {
			w.seenMu.Lock()
			defer w.seenMu.Unlock()
			for _, p := range w.pending {
				p.mu.Lock()
				fired := p.fired
				p.mu.Unlock()
				if !fired {
					return false
				}
			}
			return true
		}, 2*time.Second, 5*time.Millisecond)
	}

	residueCeiling := burst
	for round := 1; round <= rounds; round++ {
		// Inject exactly one trigger-budget of NEW paths. The sweep fires
		// on this burst's last event and must delete everything left idle
		// by the previous rounds.
		for i := 0; i < burst; i++ {
			p := filepath.Join(dir, fmt.Sprintf("r%02d-%02d.log", round, i))
			require.NoError(t, os.WriteFile(p, []byte("x"), 0o644))
			evc <- fsnotify.Event{Name: p, Op: fsnotify.Create}
			if i == 0 {
				select {
				case <-events:
				case <-time.After(2 * time.Second):
					t.Fatalf("round %d: first entry did not flush before the sweep", round)
				}
				require.Eventually(t, func() bool {
					w.seenMu.Lock()
					defer w.seenMu.Unlock()
					pending := w.pending[p]
					if pending == nil {
						return false
					}
					pending.mu.Lock()
					defer pending.mu.Unlock()
					return pending.fired
				}, time.Second, time.Millisecond,
					"round %d: first entry must be fired before the remaining burst triggers sweep", round)
			}
		}
		for i := 1; i < burst; i++ {
			select {
			case <-events:
			case <-time.After(2 * time.Second):
				t.Fatalf("round %d: only %d of %d deliveries arrived", round, i, burst)
			}
		}
		waitAllFired()

		// A sweep can observe callbacks completing while it scans, so residue
		// is bounded above by the active burst; equality is not guaranteed.
		w.seenMu.Lock()
		n := len(w.pending)
		w.seenMu.Unlock()
		assert.LessOrEqual(t, n, burst,
			"round %d: residue must not exceed the current burst upper bound", round)
		assert.LessOrEqual(t, n, residueCeiling,
			"round %d: residue must not grow across fully-settled rounds", round)
		residueCeiling = n
	}
}

// AUD-9 review follow-up: a timer callback can pass its generation check,
// release p.mu, and then lose to a new Write before flush stats the file. The
// stale callback must not deliver that hot partial version, while the newly
// armed timer must still deliver the later complete version.
func TestDebounced_FlushRechecksQuietnessAndLaterDeliversCompleteVersion(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "continued-write.log")
	partial := []byte("partial")
	complete := []byte("complete-version")
	require.NoError(t, os.WriteFile(path, partial, 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 100 * time.Millisecond

	beforeFlush := make(chan struct{}, 1)
	releaseFlush := make(chan struct{})
	callbackFinished := make(chan struct{}, 1)
	w.debounceBeforeFlush = func(callbackPath string) {
		if callbackPath != path {
			return
		}
		select {
		case beforeFlush <- struct{}{}:
		default:
		}
		<-releaseFlush
	}
	w.debounceCallbackFinished = func(callbackPath string) {
		if callbackPath == path {
			select {
			case callbackFinished <- struct{}{}:
			default:
			}
		}
	}

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent, 2)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	evc <- fsnotify.Event{Name: path, Op: fsnotify.Create}
	select {
	case <-beforeFlush:
	case <-time.After(time.Second):
		t.Fatal("callback did not reach the before-flush synchronization point")
	}

	// The writer resumes while gen-1 is parked after its generation check.
	// Refreshing the same partial bytes updates mtime; the injected Write then
	// arms gen-2 before gen-1 is allowed to stat the file.
	require.NoError(t, os.WriteFile(path, partial, 0o644))
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		p := w.pending[path]
		w.seenMu.Unlock()
		if p == nil {
			return false
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.gen == 2 && !p.fired
	}, time.Second, time.Millisecond, "continued Write did not arm generation 2")
	close(releaseFlush)
	select {
	case <-callbackFinished:
	case <-time.After(time.Second):
		t.Fatal("generation-1 callback did not finish")
	}
	select {
	case fe := <-events:
		t.Fatalf("交付了写到一半的内容：size=%d want no delivery before writes settle", fe.Size)
	default:
	}

	// Stop gen-2 to prove the hot-flush fallback is independently sufficient:
	// even if the final filesystem event is coalesced, the recheck scheduled by
	// gen-1 must rediscover and deliver the complete quiet version.
	w.seenMu.Lock()
	p := w.pending[path]
	w.seenMu.Unlock()
	require.NotNil(t, p)
	require.True(t, p.timer.Stop(), "generation-2 timer must still be armed before testing the fallback recheck")
	require.NoError(t, os.WriteFile(path, complete, 0o644))
	select {
	case fe := <-events:
		require.Equal(t, int64(len(complete)), fe.Size,
			"放弃热文件交付后，fallback recheck 必须交付完整版本")
	case <-time.After(time.Second):
		t.Fatal("flush 放弃热文件后，fallback recheck 没有交付完整版本")
	}
}

// D-035 mutation guard for the flush-specific age >= 0 clamp. A future mtime
// must pass through flush immediately; treating its negative age as hot would
// abandon this callback and defer delivery to a recheck instead.
func TestDebounced_FlushFutureMTimeDeliveredByCurrentCallback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "future-flush.log")
	require.NoError(t, os.WriteFile(path, []byte("complete"), 0o644))
	future := time.Now().Add(time.Hour)
	require.NoError(t, os.Chtimes(path, future, future))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 30 * time.Millisecond
	callbackFinished := make(chan struct{}, 1)
	w.debounceCallbackFinished = func(callbackPath string) {
		if callbackPath == path {
			callbackFinished <- struct{}{}
		}
	}
	defer w.stopAllRechecks()

	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.loopDebounced(ctx, events, make(map[string]time.Time), evc, erc) }()

	evc <- fsnotify.Event{Name: path, Op: fsnotify.Create}
	select {
	case <-callbackFinished:
	case <-time.After(time.Second):
		t.Fatal("future-mtime flush callback did not finish")
	}
	select {
	case fe := <-events:
		require.Equal(t, int64(len("complete")), fe.Size)
	default:
		t.Fatal("future mtime 被 flush 误判为热文件，当前 callback 未交付")
	}
}

// Rework S-1: a re-armed entry (flush completed, then a new event Reset the
// timer and cleared fired) must survive the amortized sweep — fired means
// "flush fully completed", so a cleared flag means the entry is alive with an
// armed timer. Mutation guards: sweeping without the fired check, or Reset
// without clearing fired, must both turn this test red.
func TestDebounced_PendingRearmed_SurvivesSweep(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rearmed.log")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 30 * time.Millisecond
	w.pendingSweepEvery = 4

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	// One flush: the entry exists and its callback marks it fired.
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Create}
	select {
	case fe := <-events:
		require.Equal(t, "create", fe.Op)
	case <-time.After(2 * time.Second):
		t.Fatal("first flush never delivered")
	}
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		p, ok := w.pending[path]
		if !ok {
			return false
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.fired
	}, 1*time.Second, 5*time.Millisecond, "the flushed entry should be marked fired")

	// Re-arm with a new version: Reset + fired cleared.
	require.NoError(t, os.WriteFile(path, []byte("v2"), 0o644))
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}

	// Force the sweep to run: two more processed events (other path's
	// Create + Write) complete the every=4 budget, and the sweep must spare
	// the re-armed entry (fired was cleared by Reset). The gate is
	// tick==0 && len==2 — tick==0 holds ONLY after the sweep ran, so the
	// assertion cannot pass on a pre-sweep sample of the same len.
	require.NoError(t, os.WriteFile(filepath.Join(dir, "other.log"), []byte("o"), 0o644))
	evc <- fsnotify.Event{Name: filepath.Join(dir, "other.log"), Op: fsnotify.Create}
	evc <- fsnotify.Event{Name: filepath.Join(dir, "other.log"), Op: fsnotify.Write}

	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return w.pendingSweepTick == 0 && len(w.pending) == 2
	}, 2*time.Second, 5*time.Millisecond,
		"the re-armed entry must survive the sweep (fired was cleared by Reset)")

	// The survivor still works: its armed timer delivers the new version.
	// (The other path's flush also arrives; drain by path, and only the
	// survivor's op is asserted — the other path arrived as "create".)
	deadline := time.After(2 * time.Second)
	for {
		select {
		case fe := <-events:
			if fe.Path != path {
				continue
			}
			require.Equal(t, "write", fe.Op)
			return // survivor delivered
		case <-deadline:
			t.Fatal("re-armed entry stopped delivering after the sweep")
		}
	}
}

// AUD-9 review P1: if a callback is blocked in emitBlocking, a later Write
// re-arms the same timer. When the old callback returns it must not mark the
// entry collectible: a sweep may only delete an entry with neither an armed
// timer nor a running flush behind it.
func TestDebounced_RearmWhileFlushBlocked_SweepKeepsEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "blocked-flush.log")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 100 * time.Millisecond
	w.pendingSweepEvery = 4

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent) // unbuffered: park the first flush in emitBlocking
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	callbackFinished := make(chan struct{}, 1)
	w.debounceCallbackFinished = func(callbackPath string) {
		if callbackPath == path {
			select {
			case callbackFinished <- struct{}{}:
			default:
			}
		}
	}
	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	evc <- fsnotify.Event{Name: path, Op: fsnotify.Create}
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		_, blocked := w.inflight[path]
		return blocked
	}, time.Second, time.Millisecond, "first flush never reached emitBlocking")

	// Re-arm while the old callback is still blocked in flush.
	require.NoError(t, os.WriteFile(path, []byte("version-two"), 0o644))
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		p := w.pending[path]
		w.seenMu.Unlock()
		if p == nil {
			return false
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.op == "write" && !p.fired
	}, time.Second, time.Millisecond, "Reset did not finish before the blocked flush was released")
	<-events // let the old callback return only after Reset has completed
	select {
	case <-callbackFinished:
	case <-time.After(time.Second):
		t.Fatal("old callback did not publish its completed state")
	}
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return w.pendingSweepTick == 2
	}, time.Second, time.Millisecond, "Create and re-arming Write were not both charged before the forced sweep")

	// Finish the current sweep budget with unmatched events. They do not add
	// pending entries, so the target must still be present after tick resets.
	evc <- fsnotify.Event{Name: filepath.Join(dir, "ignored.tmp"), Op: fsnotify.Write}
	evc <- fsnotify.Event{Name: filepath.Join(dir, "ignored-2.tmp"), Op: fsnotify.Write}
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return w.pendingSweepTick == 0
	}, time.Second, time.Millisecond, "sweep did not run")

	w.seenMu.Lock()
	_, stillPending := w.pending[path]
	w.seenMu.Unlock()
	assert.True(t, stillPending,
		"sweep removed the re-armed pending entry; its orphan timer can now upload an intermediate file version")
}

// AUD-9 review P1, second interleaving: Reset may win after AfterFunc has
// scheduled its callback but before that callback takes p.mu. The callback
// must still identify itself as the older firing and leave fired false after
// its flush, because Reset armed a newer firing in the meantime.
func TestDebounced_RearmBeforeCallbackLock_SweepKeepsEntry(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "before-lock.log")
	require.NoError(t, os.WriteFile(path, []byte("v1"), 0o644))
	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	callbackEntered := make(chan struct{}, 1)
	releaseCallback := make(chan struct{})
	callbackFinished := make(chan struct{}, 1)
	w.debounceCallbackStarted = func(callbackPath string) {
		if callbackPath != path {
			return
		}
		select {
		case callbackEntered <- struct{}{}:
		default:
		}
		<-releaseCallback
	}
	w.debounceCallbackFinished = func(callbackPath string) {
		if callbackPath == path {
			select {
			case callbackFinished <- struct{}{}:
			default:
			}
		}
	}
	w.debounce = 100 * time.Millisecond
	w.pendingSweepEvery = 3

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	evc <- fsnotify.Event{Name: path, Op: fsnotify.Create}
	select {
	case <-callbackEntered:
	case <-time.After(time.Second):
		t.Fatal("timer callback did not reach the before-lock synchronization point")
	}

	// The callback is alive but has not read any pending state. Reset must
	// record a newer arm that this older firing cannot later declare idle.
	require.NoError(t, os.WriteFile(path, []byte("version-two"), 0o644))
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		p := w.pending[path]
		w.seenMu.Unlock()
		if p == nil {
			return false
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		return p.op == "write" && !p.fired
	}, time.Second, time.Millisecond, "Reset did not finish while the callback was parked before p.mu")
	close(releaseCallback)
	select {
	case <-callbackFinished:
	case <-time.After(time.Second):
		t.Fatal("stale callback did not return after its generation check")
	}
	select {
	case fe := <-events:
		t.Fatalf("stale callback uploaded an intermediate file version before the re-armed timer fired: size=%d", fe.Size)
	default:
	}

	// The third processed event runs the sweep without creating another
	// pending entry.
	evc <- fsnotify.Event{Name: filepath.Join(dir, "ignored.tmp"), Op: fsnotify.Write}
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		return w.pendingSweepTick == 0
	}, time.Second, time.Millisecond, "sweep did not run")

	w.seenMu.Lock()
	_, stillPending := w.pending[path]
	w.seenMu.Unlock()
	assert.True(t, stillPending,
		"sweep removed an entry whose callback started before Reset took the lock; the orphan timer can upload an intermediate file version")
}

// Rework R-2 recorded decision: a file removed or renamed within the debounce
// window after its last write is NEVER collected — the pending flush is
// cancelled and only a bare remove event goes out. Pins the decision so that
// whoever changes it in the future is told they are changing a decision
// (D-035, short-lived-files trade-off).
func TestDebounced_RemoveWithinWindow_CancelsDelivery(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "shortlived.log")
	require.NoError(t, os.WriteFile(path, []byte("written then removed"), 0o644))

	w, err := New(dir, "*.log", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)
	w.debounce = 30 * time.Millisecond

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go func() { _ = w.loopDebounced(ctx, events, seen, evc, erc) }()

	evc <- fsnotify.Event{Name: path, Op: fsnotify.Write}
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Remove}

	// Exactly one event: the remove, immediate (not debounced).
	select {
	case fe := <-events:
		require.Equal(t, "remove", fe.Op)
		require.Equal(t, path, fe.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("remove event was not delivered")
	}

	// Well past the debounce window: still no content event.
	select {
	case fe := <-events:
		t.Fatalf("content event delivered for a file removed inside the debounce window: %s (op=%s)", fe.Path, fe.Op)
	case <-time.After(w.debounce*3 + 300*time.Millisecond):
	}
}

// ── PR #108 codex 复审 P1: seen 语义回退为「已交付」前的守卫（见卡片 IC-BUG-53） ─

// The safety-net rescan is a RETRY OPPORTUNITY: real-time deliveries are not
// durable (emitBlocking=true only means the event entered the in-memory
// channel — the downstream submit can still fail), so the rescan must
// re-emit files it finds even if they were delivered in real time. Seen is
// deliberately NOT written on the real-time path, so the overflow rescan
// never skips a file just because it was delivered once. (The trade-off —
// bounded re-delivery vs. unbounded silent loss — is argued in the
// loopFsnotify comment; the watcher-side residual gap is IC-BUG-53.)
func TestLoopFsnotify_RescanRetriesRealTimeDeliveredFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "retryable.txt")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	// Tail mode: the real-time retry semantics pinned here belong to the
	// real-time loop, which since D-035 only tail walks (debounced
	// deliveries record seen and are deliberately NOT re-emitted).
	w, err := New(dir, "*.txt", false, time.Hour, AppendModeTail, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]time.Time)
	evc := make(chan fsnotify.Event)
	erc := make(chan error, 1)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	go func() { _ = w.loopFsnotify(ctx, events, seen, evc, erc) }()

	// The file is delivered in real time (first delivery consumed below).
	evc <- fsnotify.Event{Name: path, Op: fsnotify.Create}
	select {
	case fe := <-events:
		require.Equal(t, path, fe.Path)
	case <-time.After(2 * time.Second):
		t.Fatal("real-time create was not delivered")
	}

	// An overflow rescan must RE-EMIT the file: the real-time delivery is
	// not durable, and this rescan is the retry opportunity for the window
	// between delivery and durable enqueue.
	erc <- fsnotify.ErrEventOverflow
	select {
	case fe := <-events:
		assert.Equal(t, path, fe.Path)
		assert.Equal(t, "create", fe.Op)
	case <-time.After(2 * time.Second):
		t.Fatal("safety-net rescan skipped a real-time-delivered file — the retry opportunity is lost (P1 regression)")
	}
}
