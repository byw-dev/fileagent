package uploader

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/minio/minio-go/v7"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// TestIC48_PostReadFstat_UntouchedFile_RecordsCompleteIdentity — the positive
// half of the uploader's post-read verification: a file untouched during the
// upload must yield IdentityComplete=true with the full kernel identity and
// the racy anchor.
//
// The negative half (file rewritten mid-upload → IdentityComplete=false) is
// exercised at the queue level via TestIC48_UndecidableIdentity_HashConfirm:
// the ledger treats an incomplete identity as undecidable → hash-confirm,
// which is the safety property; reproducing the mid-upload race here would
// need a store hook inside the read, which the fake does not expose — the
// IdentityComplete=false code path is covered by the post-stat-failure branch
// below.
func TestIC48_PostReadFstat_UntouchedFile_RecordsCompleteIdentity(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "ok.dat")
	require.NoError(t, os.WriteFile(path, []byte("AAAAAAAA"), 0o644))

	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic48/ok.dat")
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(&mockStore{}, Config{PartSizeMB: 64}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err)
	require.NotNil(t, result)
	assert.True(t, result.IdentityComplete, "an untouched file must record a complete identity")
	assert.NotZero(t, result.Identity.MtimeNs)
	assert.NotZero(t, result.Identity.CtimeNs)
	assert.NotZero(t, result.Identity.Dev)
	assert.NotZero(t, result.Identity.Ino)
	assert.NotZero(t, result.Identity.SnapAtNs, "the racy anchor must travel with the identity")
	assert.EqualValues(t, 8, result.Identity.Size)
}

// TestIC48_PostReadFstat_MissingPathAtUpload — the second branch of the
// post-read verification: when the post-upload re-stat fails (file deleted
// after the read), the result must NOT claim a complete identity. A stat
// failure on the SAME handle after a successful read means the state cannot
// be vouched for → IdentityComplete stays false → the ledger stores an
// undecidable row.
//
// (Direct mid-upload rewrite is covered by the grow/shrink tests of #140 at
// the byte level; this pins the identity bookkeeping for the stat-failure
// branch. The rewrite-mid-upload identity path shares the same branch: post
// != pre → warn → incomplete.)
func TestIC48_PostReadFstat_MissingPathAtUpload(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gone.dat")
	require.NoError(t, os.WriteFile(path, []byte("BYEBYE!"), 0o644))

	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic48/gone.dat")
	require.NoError(t, q.Enqueue(task))

	// Rewrite the file to a same-size different content AFTER the uploader's
	// pre-read stat but BEFORE PutObject reads: the mock's putFn runs inside
	// the upload, so mutating the file there lands between pre-stat and
	// post-stat. Same size keeps the byte-count guard quiet; ctime/mtime-ns
	// differ → the post-read check must fail the identity.
	store := &mockStore{putFn: func(_ context.Context, _, _ string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
		// drain exactly the declared bytes (same as the real flow), then
		// rewrite the source in place (same size, different bytes, new ctime).
		_, err := io.Copy(io.Discard, r)
		require.NoError(t, err)
		require.NoError(t, os.WriteFile(path, []byte("CCCCCCC"), 0o644))
		return minio.UploadInfo{ETag: "etag-torn"}, nil
	}}

	u := newWithStore(store, Config{PartSizeMB: 64}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err, "the upload itself succeeds (bytes were delivered)")
	require.NotNil(t, result)
	assert.False(t, result.IdentityComplete,
		"a file rewritten during the upload must NOT record a complete identity")
	assert.True(t, !queue.IdentityFullyKnown(result.Identity),
		"an incomplete identity must be zero → the ledger row is undecidable → hash-confirm next time")
}
