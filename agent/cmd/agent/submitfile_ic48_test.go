package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/executor"
	"github.com/byw-dev/fileagent/agent/internal/fileid"
	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/byw-dev/fileagent/agent/internal/scheduler"
	uploadpkg "github.com/byw-dev/fileagent/agent/internal/uploader"
	"github.com/byw-dev/fileagent/pkg/trollsift"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestSubmitFile_DedupError_StillEnqueues pins the rework-1 fix 1: a failed
// dedup lookup must NOT drop the candidate. CandidateDecision's zero value is
// {VerdictUnset, ...} — with the pre-fix switch (`case Skip, RefreshAndSkip:
// return`), a zero-value decision on the error path hit Skip (Skip was iota 0)
// and the file was silently lost. Now: the error branch forces Recollect
// (same as executor.processTask), and VerdictUnset/unknown verdicts fall into
// the queue via the default arm.
//
// The red reproducer: a REAL file whose hash-confirm READ fails (the file is
// removed after the ledger row exists, so DecideProcessed takes the
// undecidable→hash path and hashFn errors on the missing file). Master's
// pre-fix behaviour: error → zero decision → Skip → dropped (red). Fixed:
// error → Recollect → enqueued (green).
func TestSubmitFile_DedupError_StillEnqueues(t *testing.T) {
	q := openTestQueue(t)
	dir := t.TempDir()
	fpath := filepath.Join(dir, "locked.txt")
	require.NoError(t, os.WriteFile(fpath, []byte("DEDUP"), 0o644))

	// Seed the ledger with a full identity + a hash that can never match
	// (the file will be gone before the hash-confirm runs), so the candidate
	// takes the undecidable→hash path and the injected error fires.
	id, err := fileid.Stat(fpath)
	require.NoError(t, err)
	require.NoError(t, q.UpsertProcessedFile(&queue.ProcessedFile{
		ID: "pf-err", RuleID: "r-err", LocalPath: fpath,
		FileSize: 5, FileMtime: id.MtimeNs / 1e9, SHA256: "sha-that-will-not-match",
		Ident: queue.FileIdentity{
			MtimeNs: id.MtimeNs, CtimeNs: id.CtimeNs, Dev: id.Dev, Ino: id.Ino,
			Size: id.Size, SnapAtNs: id.SnapAtNs - queue.RacyGranularityNs*10,
		},
		UploadedAtNs: id.SnapAtNs - queue.RacyGranularityNs*10,
	}))

	submitted := make(chan struct{}, 1)
	exec := executor.New(1, q, func(_ context.Context, _ *queue.UploadTask) (*uploadpkg.UploadResult, error) {
		submitted <- struct{}{}
		return &uploadpkg.UploadResult{StoragePath: "bucket/key", Bucket: "b", SHA256: "x", SizeBytes: 5}, nil
	}, zap.NewNop(), 0)
	exec.Start(context.Background())
	defer exec.Stop()

	// The file disappears before submitFile: identity stat fails (zero
	// identity) → undecidable → hash-confirm → open fails → DecideProcessed
	// errors. The candidate must still be enqueued.
	require.NoError(t, os.Remove(fpath))

	rule := scheduler.CollectionRule{RuleID: "r-err", BasePath: dir, UploadBucket: "bkt", DestPathTemplate: "logs/{filename}"}
	submitFile(context.Background(), exec, q, rule, fpath, 5, time.Now(), 0, "", trollsift.AgentContext{}, zap.NewNop())

	select {
	case <-submitted:
		// fix behaviour: the candidate was enqueued despite the dedup error
	case <-time.After(time.Second):
		t.Fatal("a failed dedup lookup silently dropped the file (fail-open): it must be enqueued")
	}
}
