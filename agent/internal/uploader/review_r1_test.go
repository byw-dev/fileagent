package uploader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── Review-r1 blocker 1 regressions: one handle for identity AND bytes ──────

// swapStore is a mockStore that re-points the task path's symlink between the
// internal calls: NewMultipartUpload swaps the path to B, Complete swaps it
// back to A. With the pre-fix code (path re-opened per part) the parts then
// read B's bytes while the identity snapshot (and the post-read verification
// on the pre-opened handle) still describe A.
type swapStore struct {
	mockStore
	path, a, b string
}

func (s *swapStore) NewMultipartUpload(c context.Context, b, o string, p minio.PutObjectOptions) (string, error) {
	require.NoError(nil, swapPath(s.path, s.b))
	return s.mockStore.NewMultipartUpload(c, b, o, p)
}

func (s *swapStore) CompleteMultipartUpload(c context.Context, b, o, id string, parts []minio.CompletePart, p minio.PutObjectOptions) (minio.UploadInfo, error) {
	require.NoError(nil, swapPath(s.path, s.a))
	return s.mockStore.CompleteMultipartUpload(c, b, o, id, parts, p)
}

func swapPath(path, target string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	return os.Symlink(target, path)
}

// TestReviewMultipartHandleSwap_Ported — blocker 1, multipart path (ported
// from the reviewer's sandbox test). The upload reads must come from the ONE
// handle UploadFile opened and stat'ed: the result's SHA256 must describe the
// CURRENT path content (B), and the identity must be complete (the pre/post
// handle stat saw the same file — A — throughout; the bytes were B's by
// construction of the swap, but the handle never followed the symlink, so
// bytes and identity stay consistent: the upload delivers the HANDLE's file).
//
// With the pre-fix code (per-part path re-open), the parts read B's bytes
// while the handle identity stayed A → a blended identity+content row.
func TestReviewMultipartHandleSwap_Ported(t *testing.T) {
	d := t.TempDir()
	a := filepath.Join(d, "a")
	b := filepath.Join(d, "b")
	p := filepath.Join(d, "link")
	aBytes := bytes.Repeat([]byte("A"), 2*1024*1024)
	bBytes := bytes.Repeat([]byte("B"), 2*1024*1024)
	require.NoError(t, os.WriteFile(a, aBytes, 0o600))
	require.NoError(t, os.WriteFile(b, bBytes, 0o600))
	require.NoError(t, os.Symlink(a, p))

	q := newTestQueue(t)
	task := newTask(t, p, "bucket", "key")
	require.NoError(t, q.Enqueue(task))

	store := &swapStore{path: p, a: a, b: b}
	u := newWithStore(store, Config{ThresholdMB: 1, PartSizeMB: 1}, q, zap.NewNop())
	r, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err)

	// FIXED behaviour: the handle never follows the symlink swap, so the
	// uploaded bytes are the handle's file (A) and the identity describes the
	// same A — consistent by construction. The hash of the delivered bytes
	// must equal the hash of A.
	data, err := os.ReadFile(p) // p → a after Complete swapped back
	require.NoError(t, err)
	assert.EqualValues(t, byte('A'), data[0], "the path is back to a")
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(aBytes)), r.SHA256,
		"the upload must deliver the HANDLE's file (a): identity and bytes from one fd")
	assert.True(t, r.IdentityComplete,
		"the pre/post stat on the same handle must agree (the handle never saw the swap)")
}

// swapPutStore swaps the path inside PutObject — BEFORE draining the reader
// — so any path re-open during the read would see b's bytes.
type swapPutStore struct {
	mockStore
	path, a, b string
	swapCount  int
}

func (s *swapPutStore) PutObject(c context.Context, b, o string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	// Swap BEFORE the store consumes the reader: with the pre-fix code
	// (singlePartUpload re-opened task.LocalPath by path) a path re-open
	// during/after this point would read b's bytes; with the fix the read
	// comes from the pre-opened handle (a) regardless.
	require.Nil(nil, swapPath(s.path, s.b))
	s.swapCount++
	return s.mockStore.PutObject(c, b, o, r, size, opts)
}

// TestReviewSinglePartHandleSwap — blocker 1, single-part path (rework-5
// fix 5: the swap now ACTUALLY happens inside PutObject, asserted via
// swapCount==1 and the post-upload path state). The read must come from the
// pre-opened handle: delivered bytes = a's, identity = a's, complete.
// Mutation "single-part re-opens by path" turns this red (uploaded bytes
// become b's).
func TestReviewSinglePartHandleSwap(t *testing.T) {
	d := t.TempDir()
	a := filepath.Join(d, "a")
	b := filepath.Join(d, "b")
	p := filepath.Join(d, "link")
	aBytes := []byte("AAAAAAAA")
	bBytes := []byte("BBBBBBBB")
	require.NoError(t, os.WriteFile(a, aBytes, 0o600))
	require.NoError(t, os.WriteFile(b, bBytes, 0o600))
	require.NoError(t, os.Symlink(a, p))

	store := &swapPutStore{path: p, a: a, b: b}
	q := newTestQueue(t)
	task := newTask(t, p, "bucket", "key")
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(store, Config{PartSizeMB: 64}, q, zap.NewNop())
	r, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err)

	// The swap really happened: exactly one PutObject, and after it the path
	// resolves to b.
	assert.Equal(t, 1, store.swapCount, "the swap hook must have run exactly once")
	target, err := os.Readlink(p)
	require.NoError(t, err)
	assert.Equal(t, b, target, "the path must currently resolve to b (swap really happened)")

	// The handle was opened on p→a and never re-opened: the delivered bytes
	// must be a's, and the identity a's — consistent. A path re-open (pre-fix
	// singlePartUpload opened by path) is exactly the hole this pins shut.
	assert.Equal(t, fmt.Sprintf("%x", sha256.Sum256(aBytes)), r.SHA256,
		"single-part bytes must come from the pre-opened handle (a), not a path re-open")
	assert.True(t, r.IdentityComplete)
	assert.EqualValues(t, 8, r.SizeBytes)
}
