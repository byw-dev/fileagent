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
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"

	"github.com/byw-dev/fileagent/agent/internal/fileid"
	"github.com/byw-dev/fileagent/agent/internal/queue"
)

var newFSWatcher = fsnotify.NewWatcher

// versionOf is the watcher-side content-version key (rework-2 blocker 1):
// the kernel identity of one file version, so "already delivered" means
// "this exact version was delivered" — not merely "mtime did not move".
// A same-mtime rewrite, a cp -p rebuild, or any other identity change is
// therefore delivered downstream, where DecideProcessed's hash confirmation
// decides skip vs re-upload (identical content never re-uploads).
//
// The identity comes from fileid.Stat (open once → StatHandle on all
// platforms), never from the FileInfo blob alone (its platform-dependent
// provenance cannot be trusted to match one file). An identity with unknown
// fields (zero dev/ino/ctime — filesystems that cannot provide them) is NOT
// comparable: it must never equal a known version, so such files keep the
// pre-rework behaviour (mtime-gated) via the equalOnlyWhenKnown check —
// unknown → treated as changed → delivered (hash confirms; worst case a
// duplicate hash-confirm, never a miss).
type versionOf = fileid.Identity

// seenEntry is one delivered-version record in the watcher's seen map
// (rework-5 blocker 1): the identity AND the snapshot moment it was taken
// at. The anchor is what makes the racy-window check possible — without it
// the watcher's equality bypassed the coarse-granularity (FAT 2s / SMB 1s)
// same-tick rewrite case, where ctime/mtime/dev/ino/size can all come out
// identical while the content differs. Equality uses the queue's ONE
// SameVersionProvable predicate: only a provably-same version is suppressed.
type seenEntry struct {
	ident    versionOf
	anchorNs int64 // snapshot moment of ident (Unix ns)
}

// seenEqual reports whether the candidate identity is provably the same
// version as the seen entry (queue.SameVersionProvable; racy window and
// unknown fields included). Anything else → deliver.
func seenEqual(se seenEntry, cur versionOf) bool {
	return queue.SameVersionProvable(queue.IdentityFromFileid(se.ident),
		queue.IdentityFromFileid(cur), se.anchorNs)
}

// claimToken is an opaque ownership handle for an in-flight delivery
// (rework-5 fix 4): release matches by token identity, NOT by version
// equality — version equality can never match for unknown identities
// (zero fields), which leaked one inflight entry per unknown-identity path.
// It carries the claimed version's identity + anchor so the seen write can
// anchor the entry at the claim's snapshot moment.
type claimToken struct {
	identKnown bool
	ident      versionOf
	anchorNs   int64
	// dirty is set when a candidate for this path arrived while the claim
	// was in flight (rework-7 blocker 1): the candidate could not be proven
	// the same version, so it was not delivered — the claim's settle must
	// re-evaluate the file instead of writing seen (its version may differ
	// from the delivered one).
	dirty bool
}

// identityEqualVersions reports whether two identities describe the same
// delivered version. Only FULLY-known identities can ever compare equal;
// any zero field makes them unequal (fail-closed → deliver). Uses the
// queue package's shared field comparison (mtime-ns, ctime-ns, dev, ino,
// size).
func identityEqualVersions(a, b versionOf) bool {
	// SnapAtNs is the per-check wall clock anchor — it differs on every
	// stat call by design and must NOT participate in the version equality
	// (only the kernel stamps/dev/ino/size identify a version). The racy
	// anchor lives in the ledger, not in the watcher's seen map.
	as, bs := a, b
	as.SnapAtNs, bs.SnapAtNs = 0, 0
	return queue.IdentityFullyKnown(queue.IdentityFromFileid(as)) &&
		queue.IdentityFullyKnown(queue.IdentityFromFileid(bs)) && as == bs
}

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

	// rechecks holds the one-shot debounce recheck timers keyed by
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
	// rechecksStopped closes the current Start lifecycle's scheduling gate.
	// A late debounce callback must not install a replacement after shutdown.
	rechecksStopped bool

	// seenMu guards rechecks, the debounced loop's pending map (w.pending),
	// and every access to a scan's seen map while
	// debounce recheck callbacks (timer goroutines) may run concurrently
	// with a scan on the event-loop goroutine.
	seenMu sync.Mutex

	// pending holds the per-file in-flight debounce state of the debounced
	// event loop (loopDebounced). The loop goroutine owns the map; seenMu
	// guards it so tests can read it and the sweep stays race-free. Entries
	// are collected by the amortized sweep (sweepPendingIfDue) — without
	// recycling the map grows without bound with the set of paths ever
	// seen, and D-035 spread that growth to the default overwrite mode.
	pending map[string]*debouncePending

	// pendingSweepTick counts fsnotify events processed by the debounced
	// loop since the last sweep (see sweepPendingIfDue). Guarded by seenMu.
	pendingSweepTick int

	// pendingSweepEvery overrides defaultPendingSweepEvery when > 0 (tests
	// set a small value so sweep assertions are deterministic — same
	// pattern as w.debounce). See sweepEvery.
	pendingSweepEvery int

	// debounceCallbackStarted, debounceBeforeFlush and
	// debounceCallbackFinished are per-watcher test synchronization hooks for
	// otherwise unobservable callback scheduling windows around p.mu and
	// flush. They are always nil in production.
	debounceCallbackStarted  func(string)
	debounceBeforeFlush      func(string)
	debounceCallbackFinished func(string)

	// rescanSkippedHot and rescanRecheckArmed are per-watcher test
	// observation hooks for the IC-BUG-44 safety-net rescan's debounced
	// branches (AUD-10 round 2): rescanSkippedHot fires for every path the
	// rescan's pollScan skipped because the file was still inside the
	// debounce window; rescanRecheckArmed fires for every skipped path for
	// which the rescan actually installed a recheck timer.
	//
	// Why hooks are the only way to pin those branches: neither branch has
	// an externally distinguishable effect. A rescan that finds a file gone
	// QUIET emits it directly (pollScan), and a recheck-armed file is
	// delivered through the same channel as a flush-delivered one — so no
	// delivery pattern and no wall-clock arrangement can attribute a
	// collection to the hot-skip + re-arm path: on a slow runner the
	// rescan's pollScan stats the file past the window and emits directly,
	// and a surviving event's flush timer delivers the file anyway. Tests
	// that must PROVE the branches ran — rather than assume them from
	// timing — observe them here. Always nil in production, zero cost;
	// same shape as the debounce* hooks above.
	rescanSkippedHot   func(string)
	rescanRecheckArmed func(string)

	// dirtyRecheckFired is a test hook (rework-8 fix 2): called when the
	// dirty-settle re-examination timer actually FIRES (not merely armed).
	// Nil in production; lets tests prove the dirty → settle → recheck →
	// delivery chain ran instead of attributing a delivery to a concurrent
	// pollScan. Same shape as the rescan* hooks above.
	dirtyRecheckFired func(string)

	// running guards the single-active-Start invariant (see ErrAlreadyRunning
	// and the Start godoc).
	running atomic.Bool

	// inflight tracks paths whose delivery is currently in progress, keyed
	// by path (PR #108 review F3; rework-7: the claim no longer compares
	// versions — a racing candidate marks the token dirty instead). It is
	// the exactly-once gate for a file version across the goroutines that
	// all run "check seen → send → record seen": the debounce flush, the
	// debounce recheck and the overflow-rescan scan. Guarded by seenMu;
	// lazily initialized so a Watcher built by struct literal works.
	inflight map[string]*claimToken

	// rework-7 blocker 1: the settle-time recheck needs the loop's context,
	// event channel and seen map — the exact triple scheduleDebounceRecheck
	// already takes — but completeDelivery's callers do not carry them.
	// Start installs them before any delivery can run and clears them on
	// return; guarded by recheckMu. nil context = the loop is not running
	// (unit tests drive claimDelivery/completeDelivery directly) and the
	// dirty-settle recheck degrades to a no-op (claims still settle; the
	// re-examination story belongs to the live loop).
	recheckMu     sync.Mutex
	recheckCtx    context.Context
	recheckEvents chan<- FileEvent
	recheckSeen   map[string]seenEntry

	// debounce is the write-quiet (debounce) window every mode except tail
	// waits before emitting Write/Create events (D-035). Zero means the
	// production default (defaultDebounceWindow); tests may set a short
	// value to avoid wall-clock timing assumptions. See debounceWindow.
	debounce time.Duration
}

// Append-mode constants. The canonical values live in the queue package (they
// are persisted in upload_tasks.append_mode); these aliases keep the watcher's
// public API stable.
const (
	// AppendModeOverwrite means upload the full file once writes have gone
	// quiet for the debounce window (D-035). It is the default mode; since
	// D-035 the full-file upload happens once per write burst, never per
	// Write event.
	AppendModeOverwrite = queue.AppendModeOverwrite
	// AppendModeTail tracks the byte offset of each file and uploads only the
	// bytes added since the last successful upload.
	//
	// ⚠️ IC-BUG-46: tail is fail-closed blocked — the incremental tail upload
	// path would replace the whole object with only the appended bytes,
	// silently losing previously collected data. The executor refuses tail
	// tasks before any upload runs; the correct implementation is IC-15.
	AppendModeTail = queue.AppendModeTail
	// AppendModeCloseWait waits a short idle period after Write/Create events
	// before emitting. Since D-035 it is an exact alias of AppendModeOverwrite:
	// both take the debounced path (the watcher's three debounce branch points
	// treat them identically). The value is kept so pre-existing rules and
	// persisted rows stay valid.
	AppendModeCloseWait = queue.AppendModeCloseWait
)

// defaultDebounceWindow is the write-quiet period every mode except tail
// waits before emitting Write/Create events (D-035).
const defaultDebounceWindow = 500 * time.Millisecond

// defaultPendingSweepEvery is how many fsnotify events the debounced loop
// processes between two sweeps (see sweepPendingIfDue).
const defaultPendingSweepEvery = 64

// Degradation WARN messages: what Start logs when it falls back from fsnotify
// to polling (newFSWatcher failure, addWatchPaths failure). These are a
// CONTRACT, not free-form log text: the inotify overflow E2E
// (overflow_debounced_linux_test.go) matches them to refuse a run whose
// watcher silently degraded — on the polling path there is no kernel watch
// queue to overflow, so that test would otherwise only fail much later as a
// misleading "files not collected" timeout — and
// TestWatcher_FsnotifyUnavailableFallsBackToPolling /
// TestWatcher_AddPathFailureFallsBackToPolling pin that each one is really
// logged at Warn level. Reword or delete a message here ONLY in lockstep with
// those tests: dropping a WARN on its own would make the guards match nothing
// and pass trivially.
const (
	// msgFallbackFsnotifyUnavailable is logged when fsnotify itself cannot be
	// initialized (see Start).
	msgFallbackFsnotifyUnavailable = "watcher: fsnotify unavailable, using polling"
	// msgFallbackAddWatchFailed is logged when the watch paths cannot be
	// registered with fsnotify (see Start).
	msgFallbackAddWatchFailed = "watcher: cannot add watch paths, using polling"
)

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

// debounceWindow returns the effective write-quiet (debounce) window. Tests
// may shorten the window per-watcher (w.debounce) so timing-sensitive cases
// make no wall-clock assumptions; zero means the production default.
func (w *Watcher) debounceWindow() time.Duration {
	if w.debounce > 0 {
		return w.debounce
	}
	return defaultDebounceWindow
}

// debounceEnabled reports whether this watcher debounces Write/Create events
// before emitting them: every append mode except tail holds a write burst
// quiet for debounceWindow and delivers the file once, after the burst
// (D-035, AUD-9 — the universal debounce that removed overwrite's per-Write-
// event full-file upload). tail is excluded on purpose: it is fail-closed
// blocked (IC-BUG-46) and its real-time event semantics belong to IC-15, so
// loopFsnotify stays the tail-only path.
func (w *Watcher) debounceEnabled() bool { return w.appendMode != AppendModeTail }

// sweepEvery returns the effective events-per-sweep budget (tests may set a
// smaller per-watcher value via w.pendingSweepEvery; zero means the
// production default).
func (w *Watcher) sweepEvery() int {
	if w.pendingSweepEvery > 0 {
		return w.pendingSweepEvery
	}
	return defaultPendingSweepEvery
}

// sweepPendingIfDue is the debounced loop's amortized recycling. It runs on
// the loop goroutine after every processed fsnotify event and charges one
// tick per event; every sweepEvery events it deletes all fired entries and
// resets the tick. (History: rework S-1 replaced the per-flush done-channel
// design, whose fixed-size send queue permanently stranded idle entries;
// S-1's own trigger — a threshold set to 2*len(pending)+floor at each sweep —
// was replaced in turn (third-round rework) because the threshold only ever
// ratcheted UP: the active count it recorded at sweep time all became idle
// afterwards, so rounds of "wait for idle, then burst just past the
// threshold" grew the residue without bound. The trigger is now a plain
// per-event budget with NO historically derived term, so nothing can
// bootstrap.)
//
// Deleting fired entries here is safe because debouncePending maintains this
// invariant under p.mu: fired=true implies that the current generation has no
// armed timer and its flush has returned. Re-arming first advances gen and
// clears fired; an older callback cannot publish fired after a newer arm. An
// older callback that already passed its first generation check may overlap a
// newer arm, so flush separately rechecks the file's quiet age before delivery
// and schedules an independent recheck when it finds a hot file. Therefore an
// entry deleted here has no timer that can later initiate another flush.
//
// Boundedness, honestly stated (D-035): a sweep retains only the entries it
// observed as still active (unflushed); post-sweep len(pending) is AT MOST
// the active count observed at sweep start — an entry observed as unfired is
// kept, but its timer callback may set fired immediately afterwards, so an
// already-idle entry can still be present in the map at sweep return (the
// inequality, not an equality, is what matters here). Before the
// next sweep at most sweepEvery further events are processed, each adding at
// most one entry, so len(pending) ≤ max_active + sweepEvery, ALWAYS. The
// trigger contains no historically derived quantity, so there is no
// bootstrap: past peaks never enlarge future budgets.
//
// Amortized cost, honestly stated: one O(len) sweep per sweepEvery events,
// i.e. O(len/sweepEvery) per event, with len itself bounded by
// max_active + sweepEvery. (The earlier "geometric threshold" claim of O(1)
// amortized was the same design whose boundedness turned out to be wrong.)
//
// After a burst whose events then stop entirely, the residue stays at that
// burst's watermark — bounded by the burst size, but not drained until
// further events arrive (no event, no sweep). That is bounded residue, a
// different property from the unbounded bootstrap above.
func (w *Watcher) sweepPendingIfDue() {
	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	w.pendingSweepTick++
	if w.pendingSweepTick < w.sweepEvery() {
		return
	}
	w.pendingSweepTick = 0
	for path, p := range w.pending {
		p.mu.Lock()
		fired := p.fired
		p.mu.Unlock()
		if fired {
			delete(w.pending, path)
		}
	}
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

// ErrAlreadyRunning is returned by Start when this Watcher already has a
// live Start call. One Watcher supports exactly one running Start; sequential
// reuse (Start → ctx cancelled → Start again) is allowed.
var ErrAlreadyRunning = errors.New("watcher: already running")

// Start begins watching the source directory and emits FileEvents on events.
// It first attempts to use fsnotify; if adding the watch path fails, it
// transparently falls back to periodic polling. Start blocks until ctx is
// cancelled.
//
// A Watcher supports exactly ONE live Start: pending, rechecks, inflight and
// tailOffsets are instance-level state shared by whichever loop is running,
// so concurrent Starts would corrupt each other (a stopping loop's
// stopPendingTimers would also kill the other loop's timers). A second
// concurrent Start returns ErrAlreadyRunning. Sequential reuse — Start,
// ctx cancelled, Start again (the rule hot-reload pattern) — is fine: each
// run rebuilds its per-run state.
func (w *Watcher) Start(ctx context.Context, events chan<- FileEvent) error {
	if !w.running.CompareAndSwap(false, true) {
		return ErrAlreadyRunning
	}
	defer w.running.Store(false)
	// Rework-7 blocker 1: publish the loop context/channel/seen triple so a
	// dirty-settle (completeDelivery seeing tok.dirty) can arm the shared
	// recheck. Installed before any delivery and cleared on return.
	w.recheckMu.Lock()
	w.recheckCtx = ctx
	w.recheckEvents = events
	w.recheckSeen = make(map[string]seenEntry)
	w.recheckMu.Unlock()
	defer func() {
		w.recheckMu.Lock()
		w.recheckCtx = nil
		w.recheckEvents = nil
		w.recheckSeen = nil
		w.recheckMu.Unlock()
	}()
	// Keep the lifecycle gate pair at the outer Start scope as a defensive
	// invariant. The polling path currently ignores pollScan's skipped paths
	// and never schedules rechecks, so this placement does not change its
	// behavior; the observable requirement is that every fsnotify lifecycle
	// calls startRechecks before its initial scan.
	w.startRechecks()
	defer w.stopAllRechecks()

	fw, err := newFSWatcher()
	if err != nil {
		w.logger.Warn(msgFallbackFsnotifyUnavailable, zap.Error(err))
		return w.runPolling(ctx, events)
	}
	defer fw.Close()

	if err := w.addWatchPaths(fw); err != nil {
		w.logger.Warn(msgFallbackAddWatchFailed, zap.Error(err))
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
	seen := make(map[string]seenEntry)
	w.scheduleDebounceRechecks(ctx, events, seen, w.pollScan(ctx, events, seen))

	w.logger.Info("watcher: fsnotify started", zap.String("path", w.sourcePath))
	// D-035: every mode except tail takes the debounced loop — overwrite
	// included (close_wait is its alias). Only tail walks runFsnotify.
	if w.debounceEnabled() {
		return w.runDebounced(ctx, events, fw, seen)
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

// runDebounced drives the fsnotify event loop in the debounced mode, i.e.
// every append mode except tail (D-035). Write and Create events are
// debounced: a per-file timer is reset on every event, and the FileEvent is
// emitted only once the timer fires (i.e., once writes stop for at least
// debounceWindow). This approximates "file closed after write" on platforms
// that do not expose a native close-write notification, and it is what keeps
// one cp into the watched directory from becoming one full-file upload per
// Write event (AUD-9).
//
// Errors on fw.Errors (including watch-queue overflows) are handled by
// handleWatchError, which triggers the IC-BUG-44 safety-net rescan on
// overflow so files whose events were lost are recovered by mtime.
func (w *Watcher) runDebounced(ctx context.Context, events chan<- FileEvent, fw *fsnotify.Watcher, seen map[string]seenEntry) error {
	return w.loopDebounced(ctx, events, seen, fw.Events, fw.Errors)
}

// debouncePending is one file's in-flight debounce state.
type debouncePending struct {
	timer *time.Timer
	// mu guards op, gen and fired: the loop goroutine updates them on every
	// event while timer goroutines validate their captured generation.
	mu sync.Mutex
	op string
	// gen identifies the one-shot timer that is currently armed. Every event
	// stops the previous timer, advances gen and installs a new timer whose
	// closure captures that generation. A callback checks gen before flush
	// and again after flush, closing both Reset-vs-callback interleavings.
	gen uint64
	// fired is true only after the current generation's flush returns. Thus
	// fired=true implies there is neither an armed timer nor a running flush;
	// sweepPendingIfDue may safely delete exactly those entries.
	fired bool
	// cancel is the current generation's delivery-cancellation channel. The
	// timer callback captures it (under mu, together with its passing gen
	// check) and hands it to flush, whose send selects on it. Whatever
	// invalidates the generation — a re-arm (Write/Create) or the pending
	// entry being removed (Remove/Rename) — closes the channel, so a
	// delivery already parked in emitBlocking is abandoned instead of being
	// released to the consumer with a snapshot the file has already moved
	// past (PR #118 round 7, independent review P1: the quiet recheck's
	// verdict goes stale under backpressure, and the post-flush
	// `gen == p.gen` check runs after the send, when it is too late to
	// retract anything).
	//
	// Guarded by mu; every generation change closes the old channel and
	// installs a fresh one, so a channel is closed exactly once.
	cancel chan struct{}
}

// loopDebounced is the debounced event loop proper. The event and error
// channels are parameters so tests can drive the loop deterministically
// without a live fsnotify backend (whose channel lifecycle would race with
// test-side injections).
func (w *Watcher) loopDebounced(ctx context.Context, events chan<- FileEvent, seen map[string]seenEntry, evc <-chan fsnotify.Event, erc <-chan error) error {
	// The map lives on the Watcher (guarded by seenMu) so tests can observe
	// recycling; every access below takes seenMu, and the loop goroutine
	// remains the only writer.
	w.seenMu.Lock()
	w.pending = make(map[string]*debouncePending)
	w.seenMu.Unlock()

	// cancel is the generation's delivery-cancellation channel captured
	// under p.mu together with the passing gen check (see armTimer). It is
	// closed the moment this generation is invalidated, cancelling a send
	// that is parked — or about to park — in emitBlocking.
	flush := func(path, op string, cancel <-chan struct{}) {
		fe, err := w.buildEvent(path, op)
		if err != nil {
			return
		}
		age := time.Since(fe.ModTime)
		if w.debounceEnabled() && age >= 0 && age < w.debounceWindow() {
			// A Write can re-arm this path after armTimer's generation check but
			// before buildEvent stats it. Never deliver that hot intermediate
			// version. Usually the Write event has already armed a newer pending
			// timer; this independent recheck also closes the case where the
			// fsnotify event is still queued (or was coalesced). The normal
			// claimDelivery gate arbitrates if both paths become ready together.
			// In production the gate stays open while this loop is running:
			// fsnotify closes Events/Errors only during Close, and Start defers
			// Close until after the loop returns. The ok=false branches below are
			// defensive support for tests that inject their own channels.
			w.scheduleDebounceRecheck(ctx, events, seen, path)
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
		// cancel / hot reload). Debounced modes have no periodic scan, so
		// "the next scan retries" does not exist at runtime; the
		// shutdown closes the loop instead: after a reload/restart the new
		// Watcher starts with a fresh seen map and its initial scan
		// re-discovers the file (scheduling a fresh recheck if it is still
		// hot), and a cancelled rule leaves the file with no rule to belong
		// to. See completeDelivery for the same reasoning.
		//
		// Seen semantics (AUD-9 / D-035, explicit and accepted): this flush
		// records the delivered mtime in seen via completeDelivery — the
		// exactly-once claim arbitration (PR #108 review F3) depends on it.
		// That means a debounced delivery whose downstream submit later
		// fails silently is NOT retried by an overflow rescan (the rescan
		// skips files already in seen; IC-BUG-53). overwrite accepted the
		// same trade-off the moment it joined the debounced path: what it
		// gave up is the rescan's retry opportunity; what it got is no more
		// per-Write-event full-file uploads and no more truncated uploads.
		// The real-time loop (tail only) keeps the opposite choice — see
		// loopFsnotify.
		// Rework-2 blocker 1: claim by VERSION identity (fileid.Stat), not
		// mtime — a same-mtime rewrite or cp -p rebuild is a different
		// version and must be delivered again (DecideProcessed hash-confirms).
		ident, serr := fileid.Stat(path)
		if serr != nil {
			return // gone between quiet-check and flush; nothing to collect
		}
		// Rework-7 fix 2: the claim/settle anchor is the identity's own
		// snapshot moment — fileid.Stat recorded it BEFORE the stat. A
		// time.Now() taken here (after the stat) would let a goroutine
		// stall longer than G between stat and claim LAUNDER the stamps out
		// of the racy window: settle writes the late anchor into seen and
		// the next candidate of an identical coarse version is then
		// provably "same" and suppressed forever (the reviewer's
		// TestReviewR4LateFlushAnchor scenario). No other anchor in this
		// file is sampled after a stat (grep: anchor sites all use
		// ident.SnapAtNs).
		tok, ok := w.claimDelivery(seen, path, ident, ident.SnapAtNs)
		if !ok {
			return
		}
		delivered := false
		defer func() { w.completeDelivery(seen, path, tok, delivered) }()
		// IC-BUG-47: blocking send — a dropped event here means the file is
		// never collected (mtime/size never change again). Backpressure
		// delays the debounce-flush goroutine instead.
		//
		// PR #118 round 7: the send is additionally cancellable by the
		// generation's cancel channel. The quiet recheck above proved the
		// file quiet at stat time, but that verdict goes stale while the
		// send waits out backpressure: a Write parked here re-arms the
		// entry and closes cancel, and this stale generation's snapshot
		// must then be abandoned, not delivered (the post-flush
		// `gen == p.gen` check cannot help — it runs after the send).
		// emitCancellable documents the residual window that remains.
		switch outcome := w.emitCancellable(ctx, events, fe, cancel); outcome {
		case emitDelivered:
			delivered = true
		case emitSuperseded:
			// The generation was invalidated mid-park (re-arm or pending
			// removal). Roll the claim back (completeDelivery via defer)
			// and re-arm a fallback recheck so the file cannot be lost:
			//
			// Why this cannot miss (acceptance (b)): a closed cancel means
			// exactly one of
			//   1. a Write/Create re-armed the entry — a newer generation's
			//      timer is armed and its flush will deliver the newer
			//      version; if that version is in turn rewritten, the chain
			//      repeats, and whichever generation finally finds the file
			//      quiet delivers it;
			//   2. a Remove/Rename deleted the entry — the file is gone and
			//      per the recorded R-2/D-035 decision there is nothing
			//      left to collect; the recheck below stats the path,
			//      finds nothing, and stops;
			//   3. loop shutdown — recheck scheduling is gated by
			//      rechecksStopped / ctx.Err() and stays closed.
			// Scheduling the recheck unconditionally is therefore always
			// safe: in case 1 it is redundant with the armed timer (the
			// claimDelivery/seen arbitration makes whichever path stats
			// first deliver and the other skip — no double delivery), and
			// in cases 2–3 it is a no-op. But if the newer generation's
			// timer is later stopped or its flush bails out for any
			// reason, this recheck is what still delivers the final quiet
			// version — the no-miss guarantee must not depend on the
			// newer timer surviving.
			w.scheduleDebounceRecheck(ctx, events, seen, path)
		}
	}
	armTimer := func(p *debouncePending, path string, gen uint64) *time.Timer {
		return time.AfterFunc(w.debounceWindow(), func() {
			if w.debounceCallbackStarted != nil {
				w.debounceCallbackStarted(path)
			}
			p.mu.Lock()
			if gen != p.gen {
				p.mu.Unlock()
				if w.debounceCallbackFinished != nil {
					w.debounceCallbackFinished(path)
				}
				return
			}
			op := p.op
			// Capture this generation's cancellation channel under the same
			// lock as the gen check: whatever invalidates the generation
			// later closes exactly this channel (re-arm installs a fresh one).
			cancel := p.cancel
			p.mu.Unlock()
			if w.debounceBeforeFlush != nil {
				w.debounceBeforeFlush(path)
			}
			flush(path, op, cancel)
			p.mu.Lock()
			if gen == p.gen {
				p.fired = true
			}
			p.mu.Unlock()
			if w.debounceCallbackFinished != nil {
				w.debounceCallbackFinished(path)
			}
		})
	}

	for {
		select {
		case <-ctx.Done():
			// Cancel all pending timers before returning.
			w.seenMu.Lock()
			stopPendingTimers(w.pending)
			w.seenMu.Unlock()
			return ctx.Err()
		case ev, ok := <-evc:
			if !ok {
				w.seenMu.Lock()
				stopPendingTimers(w.pending)
				w.seenMu.Unlock()
				return nil
			}
			switch {
			case ev.Has(fsnotify.Remove) || ev.Has(fsnotify.Rename):
				if w.matchGlob(ev.Name) {
					// Remove/Rename is immediate (no debounce) and CANCELS
					// the pending flush: a file whose last write is less
					// than one debounce window old is never collected —
					// only the bare "remove" event below goes out, and the
					// agent does not upload removes. This is a recorded
					// decision, not an accident (rework R-2): before D-035
					// overwrite raced the deleter and sometimes uploaded
					// the file first; now the window always loses. The
					// dominant temp-file-then-rename write pattern strictly
					// benefits — see D-035's short-lived-files trade-off.
					w.seenMu.Lock()
					if p, ok := w.pending[ev.Name]; ok {
						p.timer.Stop()
						// Also invalidate a delivery this entry may still
						// have parked in emitBlocking (PR #118 round 7):
						// the file is gone, so releasing the stale snapshot
						// would enqueue a version that no longer exists.
						// This is the same recorded R-2/D-035 decision as
						// the timer Stop — remove cancels the pending
						// flush; nothing is re-armed and nothing needs a
						// recheck (the recheck's stat would find no file).
						p.mu.Lock()
						if p.cancel != nil {
							close(p.cancel)
						}
						p.mu.Unlock()
						delete(w.pending, ev.Name)
					}
					w.seenMu.Unlock()
					// IC-BUG-47: blocking send — never drop.
					w.emitBlocking(ctx, events, FileEvent{Path: ev.Name, Op: "remove"})
				}
			case ev.Has(fsnotify.Create) || ev.Has(fsnotify.Write):
				if w.matchGlob(ev.Name) {
					path := ev.Name
					op := opString(ev)
					w.seenMu.Lock()
					if p, ok := w.pending[path]; ok {
						// Re-arm with a new one-shot timer and generation.
						// Reusing Timer.Reset cannot tell an already-running
						// callback from the newly armed firing; generation
						// checks make that distinction explicit.
						p.timer.Stop()
						p.mu.Lock()
						p.op = op
						p.gen++
						// Invalidate the superseded generation's delivery
						// BEFORE going on: close wakes a flush parked in
						// emitBlocking and makes it abandon the stale
						// snapshot (PR #118 round 7); a flush that has not
						// reached the send yet observes the closed channel
						// in emitCancellable's entry check. The fresh
						// channel belongs to the new generation.
						if p.cancel != nil {
							close(p.cancel)
						}
						p.cancel = make(chan struct{})
						gen := p.gen
						p.fired = false
						p.timer = armTimer(p, path, gen)
						p.mu.Unlock()
						w.seenMu.Unlock()
					} else {
						p := &debouncePending{op: op, gen: 1, cancel: make(chan struct{})}
						p.timer = armTimer(p, path, p.gen)
						w.pending[path] = p
						w.seenMu.Unlock()
					}
				}
			}
			// Amortized recycling (rework S-1) after every processed event.
			w.sweepPendingIfDue()
		case err, ok := <-erc:
			if !ok {
				w.seenMu.Lock()
				stopPendingTimers(w.pending)
				w.seenMu.Unlock()
				return nil
			}
			w.handleWatchError(ctx, events, seen, err)
		}
	}
}

// runFsnotify drives the fsnotify event loop, translating raw events into
// FileEvents and emitting them on the events channel.
//
// ⚠️ D-035: since the debounce became universal, ONLY tail walks this loop
// (see debounceEnabled). tail is fail-closed blocked upstream (IC-BUG-46:
// CP rejects the rule with 422, the executor refuses tail tasks), so in
// practice no production watcher reaches here today; the loop is kept
// deliberately because it is the path IC-15 (the correct tail
// implementation) will build on, and because the real-time semantics pinned
// by its tests (emitBlocking backpressure, seen deliberately not written,
// overflow rescan retry) live here.
//
// IC-BUG-47: both send sites below use emitBlocking. The earlier non-blocking
// emit dropped events whenever the consumer channel (buffer 64) was full — a
// dropped create/write event means that file is never collected, because its
// mtime/size do not change again and no further event fires. Backpressure is
// the correct semantics here, as it already is for pollScan (PR #100 F1) and
// runDebounced.
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
func (w *Watcher) runFsnotify(ctx context.Context, events chan<- FileEvent, fw *fsnotify.Watcher, seen map[string]seenEntry) error {
	return w.loopFsnotify(ctx, events, seen, fw.Events, fw.Errors)
}

// loopFsnotify is the real-time (non-debounced) fsnotify event loop proper.
// Since D-035 only tail mode reaches it (see runFsnotify). The event and
// error channels are parameters so tests can drive the loop
// deterministically without a live fsnotify backend (whose channel lifecycle
// would race with test-side injections).
func (w *Watcher) loopFsnotify(ctx context.Context, events chan<- FileEvent, seen map[string]seenEntry, evc <-chan fsnotify.Event, erc <-chan error) error {
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
					// Asymmetry vs. the debounced path, on purpose: the
					// debounce flush/recheck/scan path keeps recording seen
					// because (1) the exactly-once claim arbitration (F3)
					// depends on the seen write, and (2) that path's "no
					// retry after a downstream failure" behaviour predates
					// this reasoning — close_wait always accepted it, and
					// since D-035 overwrite accepts it too (its explicitly
					// documented cost: the overflow rescan no longer retries
					// overwrite's silent downstream failures — see the flush
					// comment in loopDebounced and D-035's trade-off section).
					// Only the real-time path's behaviour changed (F1 added
					// markSeen, which P1 reverts).
					//
					// NOTE (PR #108 review F3): this loop deliberately does
					// NOT participate in claimDelivery/completeDelivery. In
					// tail mode (the only mode left here) there are no
					// debounce rechecks and
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
	seen := make(map[string]seenEntry)
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
// debounce window (empty in tail mode, the only non-debounced mode). On the
// polling path the caller ignores the result — the next tick re-checks those
// files anyway; on the fsnotify path the caller arms one-shot recheck timers
// for them so a file whose writer exited during the window is still collected
// (IC-BUG-43).
func (w *Watcher) pollScan(ctx context.Context, events chan<- FileEvent, seen map[string]seenEntry) []string {
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
		// Debounced modes (all modes except tail, D-035) must never emit a file
		// that is still being written: the scan path bypasses the debounce
		// timer in loopDebounced, so emitting here would upload a truncated
		// file under its own {time} storage key
		// that the later full upload never overwrites (PR #100 review F2).
		// The path is returned to the caller: on the fsnotify path a one-shot
		// recheck re-examines it once the debounce window has passed
		// (IC-BUG-43); on the polling path the next tick does.
		age := time.Since(info.ModTime())
		if w.debounceEnabled() && age >= 0 && age < w.debounceWindow() {
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
func (w *Watcher) scanFile(ctx context.Context, events chan<- FileEvent, seen map[string]seenEntry, path string, info os.FileInfo) error {
	// Rework-2 blocker 1: the version identity comes from fileid.Stat (one
	// open → StatHandle), so a same-mtime rewrite or a cp -p rebuild has a
	// DIFFERENT version key (ctime/inode moved) and is delivered downstream
	// — where DecideProcessed's hash confirmation decides skip vs re-upload.
	// An unobtainable identity (zero fields) is never equal to a known
	// version → delivered (fail-closed).
	ident, err := fileid.Stat(path)
	if err != nil {
		// File vanished between the walk stat and this stat: nothing to
		// collect; the next event (create/remove) will handle it.
		return nil
	}
	w.seenMu.Lock()
	prevEntry, known := seen[path]
	w.seenMu.Unlock()
	if known && seenEqual(prevEntry, ident) {
		return nil // provably the same version (racy window included) — skip
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
	anchorNs := ident.SnapAtNs
	tok, ok := w.claimDelivery(seen, path, ident, anchorNs)
	if !ok {
		return nil // provably-same version already delivered or in flight
	}
	// Mark as seen and record the tail offset only after the event has
	// been delivered: if the send is aborted (ctx cancelled) or would
	// drop the event, the next scan must retry the file instead of
	// silently skipping it forever (PR #100 review F1). The seen write
	// itself is done by completeDelivery below.
	// The defer guarantees the claim is settled on EVERY exit path —
	// including panics and future early returns. A leaked claim would make
	// every future claim for the same version fail and a file that never
	// changes again would silently never be collected.
	delivered := false
	defer func() { w.completeDelivery(seen, path, tok, delivered) }()
	if !w.emitBlocking(ctx, events, fe) {
		return errWalkAborted
	}
	delivered = true
	if w.appendMode == AppendModeTail {
		w.tailOffsets[path] = info.Size()
	}
	return nil
}

// markSeen records the delivered version in the shared seen map (identity +
// its snapshot anchor). seen may be touched concurrently by recheck timer
// goroutines, so every access is guarded by seenMu. Callers must only call
// it after the event has actually been delivered: an aborted send must leave
// the file retryable (PR #100 F1).
func (w *Watcher) markSeen(seen map[string]seenEntry, path string, ident versionOf, anchorNs int64) {
	w.seenMu.Lock()
	seen[path] = seenEntry{ident: ident, anchorNs: anchorNs}
	w.seenMu.Unlock()
}

// claimDelivery atomically claims the right to deliver one version of path —
// PR #108 review F3. Several goroutines run the same non-atomic "check seen →
// send → record seen" sequence for the same file: the debounce flush (timer
// goroutine), the debounce recheck (timer goroutine) and the overflow-rescan
// scan (event-loop goroutine). Without arbitration two of them can deliver
// the same version twice. The claim does the seen check and the placeholder
// insert in ONE locked section; the send itself must happen OUTSIDE seenMu
// (emitBlocking can block, and holding the lock across it would stall the
// whole watcher).
//
// Rework-5 blocker 1: the seen suppression uses the queue's ONE
// SameVersionProvable predicate — identity equality alone bypassed the racy
// window, so a coarse-granularity (FAT 2s / SMB 1s) same-tick same-size
// rewrite with restored mtime was swallowed before any content dedup ran.
// Only a PROVABLY-same version (all fields known+equal, both stamps before
// anchor−G, anchor known and not in the future) is suppressed; anything else
// — including any unknown identity field — is delivered, and the
// downstream DecideProcessed hash-confirms (identical content never
// re-uploads).
//
// Rework-5 fix 4: the claim returns an opaque TOKEN; ownership release
// matches by token, not version equality (which never matches for unknown
// identities — the leak the reviewer reproduced).
//
// Returns (nil, false) — meaning "do not deliver" — when:
//   - another goroutine holds an in-flight claim for this path (Rework-7
//     blocker 1 + Rework-8 fix 1: checked FIRST), or
//   - seen already holds a provably-same version AND nothing is in flight.
//
// THE ORDER MATTERS (rework-8 fix 1): the in-flight check must come before
// the seen check. Scenario with the old order: A delivered, seen's anchor has
// aged past the racy window; the path (symlink) switches to B, B claims and
// parks mid-delivery; the path switches BACK to still-existing A — A's
// candidate hits the old seen entry and returned false WITHOUT marking B's
// token dirty. B's settle then saw clean and never re-examined the path: the
// returned-A candidate was lost, contradicting the invariant that EVERY
// candidate arriving during flight makes the settle re-examine. With the
// in-flight check first, A's candidate marks B's token dirty → B's settle
// schedules the recheck → the path is re-evaluated through the ONE
// SameVersionProvable predicate (seenEqual) — a stale seen hit there is
// legitimate suppression (nothing in flight, version provably delivered);
// a version change is delivered and DecideProcessed hash-confirms.
//
// Rework-7 blocker 1: NO version comparison against the in-flight token. The
// old bare identity equality swallowed a coarse-FS same-tick rewrite (B)
// while an identical-identity A was parked mid-delivery: A's anchor sits
// inside the racy window of the shared stamps, so B is NOT provably the
// same version, yet bare equality rejected its claim — and with no further
// fs events B could be lost.
//
// Why dirty instead of SameVersionProvable on the in-flight identity: the
// in-flight claim's own anchor is the SNAPSHOT of the version being
// delivered — by construction any candidate observed while that delivery is
// still parked falls inside its racy window (the candidate's stamps cannot
// sort before anchor−G when they equal the delivered version's stamps), so
// the verdict is almost always Undecidable and the "dedup" degenerates into
// delivering every racing duplicate of the same version (an amplification
// vector under backpressure: each parked duplicate re-emits). The dirty flag
// is simpler and strictly safe: duplicates during flight are collapsed into
// ONE re-examination after settle, which the unified predicate arbitrates.
//
// The caller MUST settle the returned token via completeDelivery (deferred),
// so panics and early returns can never leave a stuck claim: a leaked claim
// would make every future claim for this path fail and a file that never
// changes again would silently never be collected.
func (w *Watcher) claimDelivery(seen map[string]seenEntry, path string, ident versionOf, anchorNs int64) (*claimToken, bool) {
	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	// FIRST: in-flight? Any candidate arriving during flight — regardless of
	// version, INCLUDING one that provably matches an older seen entry —
	// marks the token dirty so the settle re-examines the path (rework-8
	// fix 1; see the godoc for the lost-returned-A scenario).
	if cur, busy := w.inflight[path]; busy && cur != nil {
		cur.dirty = true
		return nil, false // in flight; its settle re-checks the path
	}
	// THEN: seen suppression — only legitimate when nothing is in flight
	// (nobody's settle needs re-arming).
	if se, known := seen[path]; known && seenEqual(se, ident) {
		return nil, false // provably the same version: already delivered
	}
	if w.inflight == nil {
		w.inflight = make(map[string]*claimToken)
	}
	tok := &claimToken{identKnown: identityKnown(ident), ident: ident, anchorNs: anchorNs}
	w.inflight[path] = tok
	return tok, true
}

// identityKnown reports whether all kernel fields of the identity are known
// (no zero ctime/dev/ino). Unknown identities are delivered on every
// observation (never suppressed) and never written to seen.
func identityKnown(id versionOf) bool {
	return queue.IdentityFullyKnown(queue.IdentityFromFileid(id))
}

// completeDelivery settles a claim made by claimDelivery: it releases the
// in-flight placeholder — matching by TOKEN (rework-5 fix 4), so the release
// works for unknown identities too — and, when delivered, records the
// delivered version in seen (identity + anchor). delivered=false only rolls
// the placeholder back; the file stays retryable (PR #100 F1).
//
// Rework-7 blocker 1 (dirty flag): a candidate that arrived while this claim
// was in flight set tok.dirty — that candidate could not be proven the same
// version, and it was NOT delivered. Therefore:
//   - if dirty and delivered: do NOT write seen unconditionally — the file
//     on disk may be a NEWER version than the one this claim delivered (the
//     coarse same-tick rewrite case). Instead schedule a debounce recheck:
//     the recheck stats the path and runs scanFile → seenEqual (the ONE
//     SameVersionProvable path). A same-version candidate is suppressed by
//     the just-written seen (its stamps still fall inside the racy window —
//     the recheck only re-delivers once G has passed, i.e. exactly the
//     convergence behaviour shared with the ledger); a NEWER version fails
//     seenEqual and is delivered → DecideProcessed hash-confirms. WHY the
//     recheck (rather than an inline re-claim): completeDelivery runs under
//     seenMu on the delivering goroutine — inline re-claiming would recurse
//     into the claim logic while still holding the lock and risk an
//     unbounded retry loop on a hot file; the recheck mechanism already
//     exists (IC-BUG-43), is generation-tracked, debounced (rate-limiting a
//     hot file's re-evaluations) and stats the CURRENT file, so it is the
//     natural carrier. The recheck is armed for EVERY mode — poll mode and
//     tail mode included — because it re-examines via scanFile directly and
//     needs no fsnotify event: the loop is exactly "no further events are
//     needed".
//   - if dirty and NOT delivered (rolled back): the rollback story (below)
//     already leaves the file retryable, and the arriving candidate is
//     itself a claim attempt — also schedule the recheck so the candidate's
//     arrival is guaranteed a re-examination even if its own delivery raced.
//
// Seen write rules (rework-5, amended rework-7):
//   - unknown identities are NEVER written to seen: they cannot be proven
//     the same version later, so writing them would suppress future
//     deliveries of genuinely-changed content; unknown-identity files are
//     re-delivered on every observation (rate-limited by the debounce
//     window) and hash-confirmed downstream.
//   - known identities are written with the claim's snapshot anchor — the
//     convergence mechanism shared with the ledger (the anchor advances on
//     each delivery, letting Same become provable once G has passed). ONLY
//     when the claim is not dirty — a dirty claim's settle must not bless
//     the path with the delivered version's identity, because the file may
//     have moved on mid-flight.
//   - the liveness guard (from rework-4) stands: seen is only updated when
//     the FILE'S CURRENT identity still matches the delivered one — a late
//     older-version delivery must not overwrite a newer entry.
//
// Known limitation (pre-existing on master, not introduced here, recorded in
// the IC-BUG-48 fix record): entries for deleted paths are never removed;
// path-set growth is unbounded over the process lifetime.
//
// Rollback (delivered=false) happens only when emitBlocking fails, and
// emitBlocking fails only on ctx cancellation — i.e. on the shutdown path
// (agent stop / rule cancel / hot reload). Debounced modes have no periodic
// scan, so "the next scan retries it" is NOT available at
// runtime; what actually closes the loop is the shutdown itself: after a
// reload or restart the new Watcher starts with a fresh seen map and its
// initial scan re-discovers the file (scheduling a fresh recheck if it is
// still hot), and if the rule was cancelled the file has no rule left to
// belong to. So no file is orphaned by a rolled-back claim.
//
// completeDelivery must NOT be called while holding seenMu: it takes the
// lock itself (and the recheck scheduler takes it too).
func (w *Watcher) completeDelivery(seen map[string]seenEntry, path string, tok *claimToken, delivered bool) {
	w.seenMu.Lock()
	var dirty bool
	if cur, busy := w.inflight[path]; busy && cur == tok {
		// token match: release our own claim (works for unknown identities)
		delete(w.inflight, path)
		dirty = tok.dirty
	} else if busy {
		// someone else superseded this claim; leave theirs alone
		w.seenMu.Unlock()
		return
	} else {
		dirty = tok.dirty
	}
	w.seenMu.Unlock()

	if dirty {
		w.recheckAfterDirtySettle(path)
		// Rework-7: a dirty settle must NOT write seen — the file may have
		// moved on mid-flight (the candidate that set dirty could not be
		// proven the same version). Writing the delivered identity here
		// would bless the path with the OLD version and suppress the next
		// candidate of the NEW version (the reviewer's blocker). The settle
		// recheck re-examines the file and its own clean delivery writes
		// seen with the CURRENT identity instead.
		return
	}
	if !delivered || tok == nil || !tok.identKnown {
		return
	}
	w.seenMu.Lock()
	live, err := fileid.Stat(path)
	if err == nil && identityEqualVersions(live, tok.ident) {
		seen[path] = seenEntry{ident: tok.ident, anchorNs: tok.anchorNs}
	} else {
		w.logger.Debug("watcher: settle skipped, live identity no longer matches the delivered one",
			zap.String("path", path),
			zap.Int64("live_mtime_ns", live.MtimeNs),
			zap.Int64("delivered_mtime_ns", tok.ident.MtimeNs))
	}
	w.seenMu.Unlock()
	// stat failed (file gone): leave seen untouched — the removal event
	// or the next scan handles it; a stale seen entry for a deleted file
	// is harmless (a recreated file has a new inode → new identity).
}

// recheckAfterDirtySettle arms the post-settle re-examination for a path
// whose in-flight claim observed a candidate mid-delivery (rework-7 blocker
// 1). It uses the shared scheduleDebounceRecheck timer (IC-BUG-43
// mechanism): debounced (rate-limited), generation-tracked, and it stats the
// CURRENT file so a same-tick rewrite delivered late is judged by the one
// SameVersionProvable predicate. Safe to call with no locks held; a no-op
// when the loop is stopping or the file is gone.
func (w *Watcher) recheckAfterDirtySettle(path string) {
	w.recheckMu.Lock()
	ctx, events, seen := w.recheckCtx, w.recheckEvents, w.recheckSeen
	w.recheckMu.Unlock()
	if ctx == nil {
		return // loop not running (unit tests drive claims directly)
	}
	w.scheduleDebounceRecheck(ctx, events, seen, path)
}

// stopAllRechecks stops and forgets every pending debounce recheck timer.
// Called when Start returns (rule cancelled / hot reload): the timer
// closures otherwise keep the watcher, the seen map, the context and the
// events channel alive per skipped path. The events channel itself is never
// closed in production, so the consequence is retention, not a
// send-after-close panic. It also closes the scheduling gate before draining,
// preventing a callback that already crossed Timer.Stop from installing a
// replacement behind the shutdown boundary. In production fsnotify closes
// Events/Errors only while Close runs, and Start defers Close until after its
// event loop returns; loopDebounced therefore cannot observe those channels
// closing under a still-live Start. Its ok=false paths exist for defensive
// handling of package tests' injected channels, not as a production shutdown
// path or an accepted data-loss trade-off.
func (w *Watcher) stopAllRechecks() {
	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	w.rechecksStopped = true
	for path, t := range w.rechecks {
		t.Stop()
		delete(w.rechecks, path)
		delete(w.recheckGens, path)
	}
}

// startRechecks opens the recheck scheduling gate for a new Start lifecycle.
// A cancelled context is the second fence against callbacks from the previous
// lifecycle after sequential Watcher reuse opens this gate again.
func (w *Watcher) startRechecks() {
	w.seenMu.Lock()
	w.rechecksStopped = false
	w.seenMu.Unlock()
}

// stopPendingTimers stops every live debounce timer in pending.
// Called on every exit path of loopDebounced; the caller holds seenMu.
func stopPendingTimers(pending map[string]*debouncePending) {
	for _, p := range pending {
		p.timer.Stop()
	}
}

// handleWatchError reacts to an error delivered on fw.Errors; it is shared by
// the runFsnotify and runDebounced event loops. Overflow errors mean the
// kernel/OS watch queue dropped queued events while no one was draining
// fw.Events, so the only recovery is a safety-net rescan of the watched tree:
// any file created or modified during the overflow window is re-discovered by
// mtime (IC-BUG-44). fsnotify reports the same sentinel on both production
// platforms — fsnotify.ErrEventOverflow (Linux inotify IN_Q_OVERFLOW and the
// Windows ReadDirectoryChangesW buffer overflow) — so a single errors.Is
// check covers both. Any other error is only logged.
func (w *Watcher) handleWatchError(ctx context.Context, events chan<- FileEvent, seen map[string]seenEntry, err error) {
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
//
// Bounded, honestly stated (PR #108 review P1): the rescan re-emits files
// that were already delivered in real time, because seen on the real-time
// path is deliberately not written (see loopFsnotify for the trade-off
// argument). The amplification per overflow round is bounded: one rescan
// pass, and the downstream IsProcessed check (rule+path+mtime+size)
// deduplicates the re-deliveries; the known remaining amplification point
// is IC-BUG-42. What the rescan must never skip is a file that has NOT yet
// been recorded in seen — for the real-time path that is every file whose
// downstream enqueue did not durably succeed, and the rescan is those
// files' retry opportunity. It DOES skip files already in seen (initial
// scan, debounced deliveries, earlier rescan rounds) — see IC-BUG-53 for
// the watcher-side residual gap that implies.
func (w *Watcher) safetyNetRescan(ctx context.Context, events chan<- FileEvent, seen map[string]seenEntry) {
	skipped := w.pollScan(ctx, events, seen)
	// Test observation for the two debounced branches below (see the field
	// comments on rescanSkippedHot / rescanRecheckArmed for why no delivery
	// pattern can substitute for this). Nil hooks cost nothing in production.
	if w.rescanSkippedHot != nil {
		for _, path := range skipped {
			w.rescanSkippedHot(path)
		}
	}
	for _, path := range skipped {
		if armed := w.scheduleDebounceRecheck(ctx, events, seen, path); armed && w.rescanRecheckArmed != nil {
			w.rescanRecheckArmed(path)
		}
	}
}

// scheduleDebounceRechecks arms one-shot recheck timers for the paths the
// scan skipped because they were still inside the debounce window
// (see pollScan).
func (w *Watcher) scheduleDebounceRechecks(ctx context.Context, events chan<- FileEvent, seen map[string]seenEntry, paths []string) {
	for _, path := range paths {
		w.scheduleDebounceRecheck(ctx, events, seen, path)
	}
}

// debounceRecheckGrace pads the recheck deadline past the debounce
// window so the quiet check at fire time does not race the writer's final
// flush.
const debounceRecheckGrace = 100 * time.Millisecond

// scheduleDebounceRecheck arms a one-shot timer that re-examines path once
// the debounce window has passed (IC-BUG-43). Without it, a file
// whose writer finished and exited within defaultDebounceWindow of watcher
// startup would never be collected: the scan skips it (PR #100 review F2),
// no fsnotify event ever fires for it again, and nothing else looks at it.
// Re-arming for a path that already has a live timer replaces the timer.
// It reports whether a recheck timer was actually installed: the file had
// to exist, the loop still had to be running, and the scheduling gate had
// to be open. The safety-net rescan uses the return value to observe its
// re-arm branch (see rescanRecheckArmed).
func (w *Watcher) scheduleDebounceRecheck(ctx context.Context, events chan<- FileEvent, seen map[string]seenEntry, path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false // gone; nothing to collect
	}
	age := time.Since(info.ModTime())
	// Every delivery path treats a future mtime as already settled, so it
	// needs only the grace that keeps the callback off the stat boundary.
	wait := debounceRecheckGrace
	if age >= 0 {
		wait = w.debounceWindow() - age + debounceRecheckGrace
		if wait < debounceRecheckGrace {
			wait = debounceRecheckGrace
		}
	}
	w.seenMu.Lock()
	defer w.seenMu.Unlock()
	if w.rechecksStopped || ctx.Err() != nil {
		return false
	}
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
	return true
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
// drive the debounce flush in loopDebounced instead.
func (w *Watcher) recheckAfterDebounce(ctx context.Context, events chan<- FileEvent, seen map[string]seenEntry, path string) {
	// Rework-8 fix 2: observe that the recheck timer FIRED before the
	// hot-check — tests attribute a delivery to the recheck (not a racing
	// pollScan) via this hook, and the fired-mark survives an early return
	// for a still-hot file (the timer DID fire).
	if w.dirtyRecheckFired != nil {
		w.dirtyRecheckFired(path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return // gone; nothing to collect
	}
	// F2 invariant (PR #100): never emit a file that may still be written.
	age := time.Since(info.ModTime())
	if w.debounceEnabled() && age >= 0 && age < w.debounceWindow() {
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

// emitOutcome is the result of a cancellable delivery attempt (see
// emitCancellable).
type emitOutcome int

const (
	// emitDelivered means the consumer accepted the event.
	emitDelivered emitOutcome = iota
	// emitAborted means ctx was cancelled (shutdown path): no delivery, and
	// the file's retry story is the existing shutdown one (fresh seen map on
	// the next Start; see completeDelivery).
	emitAborted
	// emitSuperseded means the sending generation was invalidated while the
	// send was pending (re-arm or pending removal closed its cancel
	// channel): the stale snapshot was abandoned on purpose.
	emitSuperseded
)

// emitCancellable sends fe to the events channel, blocking until the consumer
// takes it, ctx is cancelled, or cancel is closed (the sending generation was
// invalidated — see debouncePending.cancel). It shares emitBlocking's
// backpressure semantics (IC-BUG-47): the send itself is never dropped while
// the generation stays valid.
//
// ⚠️ What this does and does not guarantee (PR #118 round 7, stated plainly
// so no later comment can overclaim it):
//
//   - Guarantee: a delivery parked in emitBlocking when its generation is
//     invalidated does NOT reach the consumer (once cancel is closed and no
//     consumer is ready, the select deterministically takes the cancel
//     branch), and the entry pre-check below abandons without even offering
//     the send when invalidation already happened.
//
//   - Residual hairline that CANNOT be closed here: if the generation is
//     invalidated at the exact moment a consumer is already ready, Go's
//     select picks uniformly between the send and the cancel branch, so the
//     stale snapshot can still go out. The window is the few instructions
//     between the pre-check and the parked select (versus the unbounded
//     backpressure window the select does cover), and Go offers no
//     atomic test-and-send on channels. This is a deliberate residual risk,
//     not a fix claim.
//
//   - Even a delivery that leaves the watcher at a perfectly quiet moment
//     does NOT mean the uploader uploads those bytes: between
//     watcher → queue → uploader the file can be rewritten again, and the
//     uploader re-stats and hashes the file AT UPLOAD TIME (uploader.go
//     UploadFile), with per-upload {submit_time} keys so a truncated object
//     is never overwritten by the later complete upload. Closing the
//     watcher-side window therefore does not close the truncated-upload
//     problem; that belongs to downstream verification (comparing
//     mtime/size at upload time against the enqueued values), which the
//     watcher layer cannot provide. See D-035 round-7 addendum.
func (w *Watcher) emitCancellable(ctx context.Context, events chan<- FileEvent, fe FileEvent, cancel <-chan struct{}) emitOutcome {
	// Entry check: if the generation was already invalidated, abandon
	// without offering the send — otherwise a ready consumer (buffered
	// space) and the closed cancel would race 50/50 inside the select.
	select {
	case <-cancel:
		return emitSuperseded
	default:
	}
	select {
	case <-ctx.Done():
		return emitAborted
	case <-cancel:
		return emitSuperseded
	case events <- fe:
		return emitDelivered
	}
}

// emitBlocking sends fe to the events channel, blocking until the consumer
// takes it or ctx is cancelled. It returns false only when the context was
// cancelled before the event could be delivered. Backpressure is the correct
// semantics for ALL event paths — the initial scan (PR #100 F1), the
// debounce flush, and the real-time fsnotify loop (IC-BUG-47) —
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

// racyGranularityNsW is the watcher package's view of the queue's racy
// granularity (2s), for tests that need to age a seen anchor past G.
const racyGranularityNsW = int64(2 * 1e9)
