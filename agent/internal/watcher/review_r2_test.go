package watcher

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/fileid"
	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Review-r2 blocker 1 regressions: the watcher delivers by version ────────

// TestReviewR2PreservedMtimeMiss_Ported — blocker 1 (ported from the
// reviewer's sandbox): a same-mtime rewrite (touch -t restored) must be
// DELIVERED by scanFile — the watcher no longer drops it before
// submitFile/DecideProcessed ever run.
func TestReviewR2PreservedMtimeMiss_Ported(t *testing.T) {
	p := filepath.Join(t.TempDir(), "x")
	require.NoError(t, os.WriteFile(p, []byte("AAAA"), 0o600))
	old, err := os.Stat(p)
	require.NoError(t, err)

	// seen as of the initial delivery: v1's identity.
	id1, err := fileid.Stat(p)
	require.NoError(t, err)
	seen := map[string]versionOf{p: id1}

	// Same-mtime rewrite: content B, mtime restored.
	require.NoError(t, os.WriteFile(p, []byte("BBBB"), 0o600))
	require.NoError(t, os.Chtimes(p, old.ModTime(), old.ModTime()))
	info, err := os.Stat(p)
	require.NoError(t, err)
	require.True(t, info.ModTime().Equal(old.ModTime()), "mtime must be restored for the repro")

	events := make(chan FileEvent, 1)
	w := &Watcher{}
	require.NoError(t, w.scanFile(context.Background(), events, seen, p, info))
	require.Len(t, events, 1,
		"a preserved-mtime rewrite is a different version → the watcher must deliver it (pre-fix: dropped)")

	// And the delivered version is recorded in seen (settle).
	require.Eventually(t, func() bool {
		w.seenMu.Lock()
		defer w.seenMu.Unlock()
		cur, known := seen[p]
		idNow, err := fileid.Stat(p)
		return err == nil && known && identityEqualVersions(cur, idNow)
	}, time.Second, 5*time.Millisecond, "delivered version must settle into seen")
}

// TestReviewR2_PollScan_DeliversPreservedMtimeRewrite — blocker 1, poll
// mode: pollScan's per-file skip must be identity-based, so the same rewrite
// is delivered on the next poll tick without any restart.
func TestReviewR2_PollScan_DeliversPreservedMtimeRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "poll.csv")
	require.NoError(t, os.WriteFile(path, []byte("AAAA"), 0o600))
	old, err := os.Stat(path)
	require.NoError(t, err)

	w, err := New(dir, "*.csv", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]versionOf)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Poll 1: age out of the debounce window, deliver initial.
	require.NoError(t, os.Chtimes(path, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
	w.pollScan(ctx, events, seen)
	select {
	case fe := <-events:
		require.Equal(t, path, fe.Path)
	case <-time.After(time.Second):
		t.Fatal("poll 1 did not deliver the initial file")
	}

	// Same-mtime rewrite (mtime restored to the SAME pre-poll value), then
	// age out again (the rewrite made it hot).
	require.NoError(t, os.WriteFile(path, []byte("BBBB"), 0o600))
	require.NoError(t, os.Chtimes(path, old.ModTime(), old.ModTime()))
	require.NoError(t, os.Chtimes(path, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))

	// Poll 2: must deliver the rewrite (pre-fix: dropped, mtime unchanged).
	w.pollScan(ctx, events, seen)
	select {
	case fe := <-events:
		require.Equal(t, path, fe.Path)
		require.Equal(t, "write", fe.Op)
	case <-time.After(time.Second):
		t.Fatal("poll 2 dropped a preserved-mtime rewrite (fail-open at the watcher layer)")
	}
}

// TestReviewR2_DebounceFlushClaim_DeliversPreservedMtimeRewrite — blocker 1,
// debounce flush path: the flush claims by version identity, so a rewrite
// that restored mtime is delivered (pre-fix claim: suppressed by mtime).
func TestReviewR2_DebounceFlushClaim_DeliversPreservedMtimeRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "deb.csv")
	require.NoError(t, os.WriteFile(path, []byte("AAAA"), 0o600))
	old, err := os.Stat(path)
	require.NoError(t, err)

	w, err := New(dir, "*.csv", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]versionOf)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Round 1 delivery via scanFile (sets seen to v1's identity).
	require.NoError(t, os.Chtimes(path, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
	require.NoError(t, w.scanFile(ctx, events, seen, path, old))
	<-events

	// Same-mtime rewrite.
	require.NoError(t, os.WriteFile(path, []byte("BBBB"), 0o600))
	require.NoError(t, os.Chtimes(path, old.ModTime(), old.ModTime()))

	// Round 2 via the debounced flush claim — must NOT be suppressed.
	id2, err := fileid.Stat(path)
	require.NoError(t, err)
	require.True(t, w.claimDelivery(seen, path, id2),
		"the flush claim was suppressed for a preserved-mtime rewrite (fail-open at the claim layer)")
	w.completeDelivery(seen, path, id2, true)
}

// TestReviewR2_EndToEnd_WatcherToDecide_NoRestart — the END-TO-END blocker 1
// pin: a real Watcher delivering into a real Queue, WITHOUT any restart.
// Collect v1 → rewrite in place preserving mtime → the watcher must deliver
// v2 and the queue's DecideProcessed must verdict Recollect (content
// differs) — the full path the reviewer showed was severed.
func TestReviewR2_EndToEnd_WatcherToDecide_NoRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "e2e.csv")
	require.NoError(t, os.WriteFile(path, []byte("v1-content"), 0o600))
	old, err := os.Stat(path)
	require.NoError(t, err)

	w, err := New(dir, "*.csv", false, time.Hour, AppendModeOverwrite, zap.NewNop())
	require.NoError(t, err)

	seen := make(map[string]versionOf)
	events := make(chan FileEvent, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Age v1 out of the debounce window and deliver it through the watcher.
	require.NoError(t, os.Chtimes(path, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))
	w.pollScan(ctx, events, seen)
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("initial delivery missing")
	}

	// The queue side collected v1 (ack): ledger = v1's identity + hash.
	id1, err := fileid.Stat(path)
	require.NoError(t, err)
	sum := sha256.Sum256([]byte("v1-content"))
	sumV1 := hex.EncodeToString(sum[:])
	q, err := queue.Open(":memory:")
	require.NoError(t, err)
	defer q.Close()
	require.NoError(t, q.UpsertProcessedFile(&queue.ProcessedFile{
		ID: "pf-v1", RuleID: "r", LocalPath: path,
		FileSize: int64(len("v1-content")), SHA256: sumV1,
		Ident: queue.IdentityFromFileid(id1), UploadedAtNs: id1.SnapAtNs,
	}))

	// Preserved-mtime rewrite: v2 content, mtime restored, aged past debounce.
	require.NoError(t, os.WriteFile(path, []byte("v2-content!"), 0o600))
	require.NoError(t, os.Chtimes(path, old.ModTime(), old.ModTime()))
	require.NoError(t, os.Chtimes(path, time.Now().Add(-time.Hour), time.Now().Add(-time.Hour)))

	// The watcher (still running, seen NOT cleared) must deliver v2.
	w.pollScan(ctx, events, seen)
	select {
	case <-events:
	case <-time.After(time.Second):
		t.Fatal("watcher swallowed a preserved-mtime rewrite without a restart (blocker 1)")
	}

	// Downstream DecideProcessed must re-collect v2 (content differs from v1).
	id2, err := fileid.Stat(path)
	require.NoError(t, err)
	dec, err := q.DecideProcessed(context.Background(), "r", path,
		queue.IdentityFromFileid(id2),
		func() (queue.HashResult, error) {
			bb, err := os.ReadFile(path)
			if err != nil {
				return queue.HashResult{}, err
			}
			fid, err := fileid.Stat(path)
			if err != nil {
				return queue.HashResult{}, err
			}
			sum := sha256.Sum256(bb)
			return queue.HashResult{
				SHA256:           hex.EncodeToString(sum[:]),
				Identity:         queue.IdentityFromFileid(fid),
				IdentityComplete: true,
			}, nil
		})
	require.NoError(t, err)
	require.Equal(t, queue.Recollect, dec.Verdict,
		"v2 content differs from the recorded v1 hash → Recollect (end-to-end, no restart)")
	_ = fmt.Sprintf
}
