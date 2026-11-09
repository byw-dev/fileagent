package queue

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestEnqueueIfNoActive_PreservedMtimeRewriteNotSuppressed — rework-4
// blocker 1 sub-item: while v1's upload is still ACTIVE (pending), a
// preserved-mtime rewrite to v2 (same second-mtime + size, different
// ctime/inode) must NOT be suppressed by the active-task guard — it must
// enqueue a second task. (The legacy tuple guard suppressed it: coordinator
// question "上一版本上传未完成期间，保留 mtime 的新版本会不会被吞掉" —
// answer: yes, pre-rework; fixed by the identity comparison.)
func TestEnqueueIfNoActive_PreservedMtimeRewriteNotSuppressed(t *testing.T) {
	ctx := context.Background()
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	now := time.Now().UnixNano()
	v1 := FileIdentity{MtimeNs: now - 20e9, CtimeNs: now - 20e9, Dev: 1, Ino: 1, Size: 4, SnapAtNs: now - 10e9}
	v2 := v1
	v2.Ino = 2
	v2.CtimeNs = now - 5e9 // rewrite bumped ctime

	// v1's task is still pending (upload in flight).
	t1 := &UploadTask{ID: "t1", RuleID: "r", LocalPath: "p", FileSize: 4,
		FileMtime: v1.MtimeNs / 1e9, FileMtimeNs: v1.MtimeNs,
		FileCtimeNs: v1.CtimeNs, FileDev: v1.Dev, FileIno: v1.Ino}
	ok, err := q.EnqueueIfNoActive(ctx, t1)
	require.NoError(t, err)
	require.True(t, ok)

	// v2 arrives (same second-mtime & size): must enqueue a second task.
	t2 := &UploadTask{ID: "t2", RuleID: "r", LocalPath: "p", FileSize: 4,
		FileMtime: v2.MtimeNs / 1e9, FileMtimeNs: v2.MtimeNs,
		FileCtimeNs: v2.CtimeNs, FileDev: v2.Dev, FileIno: v2.Ino}
	ok, err = q.EnqueueIfNoActive(ctx, t2)
	require.NoError(t, err)
	require.True(t, ok,
		"a preserved-mtime rewrite during an in-flight upload must enqueue (identity differs)")

	// And a literal duplicate of v2 (same identity) must still be suppressed.
	t3 := &UploadTask{ID: "t3", RuleID: "r", LocalPath: "p", FileSize: 4,
		FileMtime: v2.MtimeNs / 1e9, FileMtimeNs: v2.MtimeNs,
		FileCtimeNs: v2.CtimeNs, FileDev: v2.Dev, FileIno: v2.Ino}
	ok, err = q.EnqueueIfNoActive(ctx, t3)
	require.NoError(t, err)
	require.False(t, ok, "the same version (same identity) must stay suppressed")
}

// TestReviewR2HybridWindowsPrefilter_Ported — rework-4 blocker 2 (ported;
// deterministic simulation of the Windows hybrid identity): the caller's
// path-stat identity (cur) matches the LEDGER row exactly (the ledger row IS
// for the same file-version b), so the prefilter sees Same → Skip WITHOUT
// hashing — even though the path may have been re-pointed. This pins the
// unsafe skip the reviewer showed; on Windows the fix is that Stat(path)
// opens once and takes ALL fields from that handle, so such a hybrid
// identity cannot be produced by statting a swapped path. On unix this
// construction documents the behaviour: identical identity to the ledger →
// skip without hash (by design — same identity == same version).
//
// The real Windows regression test lives in
// internal/fileid/handle_windows_test.go (CI windows-latest).
func TestReviewR2HybridWindowsPrefilter_Ported(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	n := time.Now().UnixNano()
	b := FileIdentity{MtimeNs: n - 20e9, CtimeNs: n - 20e9, Dev: 1, Ino: 2, Size: 4, SnapAtNs: n - 10e9}
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "b", RuleID: "r", LocalPath: "p", FileSize: 4, SHA256: "hash-B",
		Ident: b, UploadedAtNs: b.SnapAtNs,
	}))

	// A "hybrid" identity that exactly equals the ledger's b: pre-fix, the
	// Windows path-stat could produce exactly this while the bytes were A's.
	hybrid := b
	hashed := false
	d, err := q.DecideProcessed(context.Background(), "r", "p", hybrid,
		func() (HashResult, error) {
			hashed = true
			return HashResult{SHA256: "hash-A"}, nil
		})
	require.NoError(t, err)
	t.Logf("hybrid verdict=%s hashed=%v path_content=A ledger_content=B", d.Verdict, hashed)
	// Documented: an identity equal to the ledger row skips without hashing
	// (same version ⇒ same content by the kernel-identity assumption). The
	// fix removes the ABILITY to observe such a hybrid identity on Windows;
	// this test pins the verdict so a future change that breaks the skip
	// contract is visible.
	require.Equal(t, Skip, d.Verdict)
	require.False(t, hashed)
}

// TestReviewR2RollbackCatchup_Documented — rework-4 blocker 3, NOT fixed this
// round (needs user decision; coordinator said leave it). Ported as a
// documentation test: after a rollback into the old coarse timestamp bucket,
// an in-place overwrite with restored mtime, then the clock catching up past
// the anchor — all fields equal and stamps before anchor−G → Same. This is
// the known boundary written into the fix record; if it ever flips to
// Undecidable the boundary text must be updated too.
func TestReviewR2RollbackCatchup_Documented(t *testing.T) {
	n := time.Now().UnixNano()
	old := FileIdentity{MtimeNs: n - 20e9, CtimeNs: n - 20e9, Dev: 1, Ino: 2, Size: 4}
	anchor := n - 10e9
	require.Equal(t, VerdictSame, PrefilterVerdict(old, old, anchor),
		"known boundary: rollback-then-catchup is indistinguishable → Same (documented, accepted)")
}

// TestReviewR2ReverseAcks_NeverFreeReadSkip — rework-4 fix 4: reverse-order
// acks CAN roll the ledger back to an older version (A overwrites B) — the
// reviewer's sandbox proved it and it is NOT fail-open (an older ledger row
// mismatches the current file's identity → hash-confirm → Recollect). This
// test pins that no-miss property: after the rollback, a check for the
// CURRENT file (b) must be Recollect or a hash-confirmed RefreshAndSkip —
// never a free-read Skip.
func TestReviewR2ReverseAcks_NeverFreeReadSkip(t *testing.T) {
	ctx := context.Background()
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	n := time.Now().UnixNano()
	a := FileIdentity{MtimeNs: n - 20e9, CtimeNs: n - 20e9, Dev: 1, Ino: 1, Size: 4, SnapAtNs: n - 10e9}
	b := a
	b.Ino = 2
	b.Size = 5
	b.SnapAtNs = n - 5e9

	// Two different-version tasks both admitted (different mtime/size).
	for _, v := range []struct {
		id   string
		size int64
	}{{"a", 4}, {"b", 5}} {
		x := &UploadTask{ID: v.id, RuleID: "r", LocalPath: "p", FileSize: v.size, FileMtime: v.size}
		ok, err := q.EnqueueIfNoActive(ctx, x)
		require.NoError(t, err)
		require.True(t, ok)
	}
	for _, id := range []string{"a", "b"} {
		if _, err := q.DequeuePending(1); err != nil {
			t.Fatal(err)
		}
		require.NoError(t, q.SaveReport(ctx, id, []byte(id)))
	}

	// Reverse-order acks: B first, then the OLD A — the ledger rolls back.
	require.NoError(t, q.CompleteReportedIdentity(ctx, "b", true, "sha-B", b, true))
	require.NoError(t, q.CompleteReportedIdentity(ctx, "a", true, "sha-A", a, true))

	rec, err := q.GetProcessedIdentity("r", "p")
	require.NoError(t, err)
	t.Logf("reverse_acks ledger_sha=%s ledger_ino=%d", rec.SHA256, rec.Ident.Ino)
	require.Equal(t, "sha-A", rec.SHA256, "the old ack does overwrite the ledger (documented)")

	// The no-miss property: a check for b (current file = b) must NOT be a
	// free-read Skip. b's identity differs from the rolled-back A row →
	// Changed → hash-confirm → hash-B ≠ sha-A → Recollect.
	dec, err := q.DecideProcessed(ctx, "r", "p", b,
		func() (HashResult, error) {
			return HashResult{
				SHA256:           "sha-B",
				Identity:         b,
				IdentityComplete: true,
			}, nil
		})
	require.NoError(t, err)
	require.NotEqual(t, Skip, dec.Verdict,
		"after a reverse-ack rollback the current version must never be free-read-skipped (no-miss)")
	require.Equal(t, Recollect, dec.Verdict,
		"b's content (sha-B) differs from the rolled-back ledger (sha-A) → re-collect")
}
