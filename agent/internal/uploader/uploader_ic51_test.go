package uploader

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sort"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── IC-BUG-51 reproduction tests ─────────────────────────────────────────────
//
// These tests pin the INVARIANT, not the implementation: on a successful
// upload, UploadResult.SHA256 and SizeBytes must describe the bytes that were
// actually delivered to PutObject / PutObjectPart — byte for byte.
//
// They were first run RED against master (see bin/b7-ic-bug-51/logs/) and are
// the acceptance tests for the fix.

// md5Hex returns the hex-encoded MD5 digest of b — the format S3 part ETags
// are expected to use (verified live against MinIO and SeaweedFS in
// bin/b7-ic-bug-51/poc.md).
func md5Hex(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
}

func sha256Hex(b []byte) string {
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

// ic51SingleStore captures the exact bytes its PutObject consumes, with an
// optional hook fired after the first read chunk so a test can rewrite the
// source file MID-READ (simulating a writer that is still appending to or
// rewriting the file while the upload drains it).
//
// readLimit mirrors production semantics when > 0: minio-go pins the HTTP
// request to Content-Length = declared size, so the body reader is consumed
// for EXACTLY that many bytes even if more are available. With readLimit == 0
// the store drains to EOF (reader-hostile upper bound).
type ic51SingleStore struct {
	*mockStore
	received       []byte
	reads          int
	afterFirstRead func()
	readLimit      int64
}

func (s *ic51SingleStore) PutObject(_ context.Context, _, _ string, r io.Reader, size int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
	chunk := make([]byte, 512)
	for {
		if s.readLimit > 0 && int64(len(s.received)) >= s.readLimit {
			break // production stops at Content-Length
		}
		n, err := r.Read(chunk)
		s.reads++
		s.received = append(s.received, chunk[:n]...)
		if s.reads == 1 && s.afterFirstRead != nil {
			s.afterFirstRead()
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return minio.UploadInfo{}, err
		}
	}
	return minio.UploadInfo{ETag: "etag-single"}, nil
}

// TestIC51_SinglePart_ReportedSHA256MustDescribeDeliveredBytes is the core
// single-part reproduction: the source file is rewritten in place (same size)
// AFTER the uploader has started reading it. Whatever byte sequence the store
// actually receives is the object; the reported SHA256 must be the hash of
// exactly those bytes, not of some other version of the file.
func TestIC51_SinglePart_ReportedSHA256MustDescribeDeliveredBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "rewrite.dat")
	require.NoError(t, os.WriteFile(path, bytesRepeat("A", 1024), 0o644))

	store := &ic51SingleStore{mockStore: &mockStore{}}
	// After the first 512-byte chunk has been read, rewrite the whole file
	// in place (same size — invisible to any stat-based version check).
	store.afterFirstRead = func() {
		require.NoError(t, os.WriteFile(path, bytesRepeat("B", 1024), 0o644))
	}

	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/rewrite.dat")
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(store, Config{PartSizeMB: 64}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err, "the store succeeds; the upload itself must succeed")

	assert.NotEqual(t, sha256Hex(bytesRepeat("A", 1024)), sha256Hex(store.received),
		"precondition: the mid-read rewrite must actually reach the reader (object differs from the AAAA version)")
	assert.Equal(t, sha256Hex(store.received), result.SHA256,
		"reported SHA256 must equal the hash of the bytes actually delivered to PutObject")
	assert.Equal(t, int64(len(store.received)), result.SizeBytes,
		"reported SizeBytes must equal the number of bytes actually delivered")
}

// ic51MultipartStore is a stateful multipart fake: it records the bytes of
// every part actually uploaded, replays a test-defined remote part list from
// ListObjectParts (with REAL md5 ETags, like MinIO/SeaweedFS do), and assembles
// the final object from the parts at CompleteMultipartUpload time.
type ic51MultipartStore struct {
	*mockStore
	partData          map[int][]byte
	remotePartData    map[int][]byte // bytes the "remote" already holds (skipped parts)
	listParts         []minio.ObjectPart
	afterList         func()            // fired after ListObjectParts returns, before the resume consumes the parts
	beforePutPart     func(partNum int) // fired inside PutObjectPart BEFORE the part's bytes are drained
	uploadedPartNum   []int
	completedPartNums []int // the part numbers CompleteMultipartUpload received, in sent order
	object            []byte
}

func newIC51MultipartStore() *ic51MultipartStore {
	return &ic51MultipartStore{
		mockStore:      &mockStore{},
		partData:       map[int][]byte{},
		remotePartData: map[int][]byte{},
	}
}

func (s *ic51MultipartStore) PutObjectPart(_ context.Context, _, _, _ string, partNum int, r io.Reader, _ int64, _ minio.PutObjectPartOptions) (minio.ObjectPart, error) {
	if s.beforePutPart != nil {
		s.beforePutPart(partNum)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		return minio.ObjectPart{}, err
	}
	s.partData[partNum] = b
	s.uploadedPartNum = append(s.uploadedPartNum, partNum)
	return minio.ObjectPart{PartNumber: partNum, ETag: md5Hex(b)}, nil
}

func (s *ic51MultipartStore) ListObjectParts(ctx context.Context, b, o, u string, pn, mp int) (minio.ListObjectPartsResult, error) {
	res := minio.ListObjectPartsResult{ObjectParts: s.listParts}
	if s.afterList != nil {
		s.afterList()
	}
	return res, nil
}

func (s *ic51MultipartStore) CompleteMultipartUpload(_ context.Context, _, _, _ string, parts []minio.CompletePart, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
	// Assemble the object BY PART NUMBER (review-r1 fix 1): a real S3 backend
	// assembles parts by their number, not by the order the client lists
	// them. The previous fake appended in list order, which silently masked
	// the non-contiguous-resume bug (it "assembled" A+C+B and the wrong-order
	// digest happened to match its own wrong assembly).
	nums := make([]int, 0, len(parts))
	for _, p := range parts {
		nums = append(nums, p.PartNumber)
	}
	sort.Ints(nums)
	for _, n := range nums {
		b, ok := s.partData[n]
		if !ok {
			// A part skipped by the resume was never uploaded in THIS session;
			// the remote already holds its bytes from the earlier attempt.
			b = s.remotePartData[n]
		}
		s.object = append(s.object, b...)
	}
	for _, p := range parts {
		s.completedPartNums = append(s.completedPartNums, p.PartNumber)
	}
	return minio.UploadInfo{ETag: "etag-obj"}, nil
}

// TestIC51_Multipart_ResumeStalePart_ReportedSHA256MustDescribeObject is the
// multipart-resume reproduction: part 1 was uploaded in a previous attempt;
// before the resume, the local bytes of that part's range are rewritten in
// place with mtime/size restored (so every stat-based gate still says "same
// version"). The remote part 1 still holds the OLD bytes. The final object is
// old-part-1 + new-part-2, and the reported SHA256 must describe exactly that
// splice — or the resume must be refused.
func TestIC51_Multipart_ResumeStalePart_ReportedSHA256MustDescribeObject(t *testing.T) {
	const mib = 1024 * 1024
	dir := t.TempDir()
	path := filepath.Join(dir, "stale.dat")
	require.NoError(t, os.WriteFile(path, bytesRepeat("A", 2*mib), 0o644))

	// Attempt 1: part 1 (first 1 MiB of "A"s) uploaded, part 2 fails.
	store1 := &mockStore{failOnPart: 2}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/stale.dat")
	require.NoError(t, q.Enqueue(task))
	u1 := newWithStore(store1, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	_, err := u1.UploadFile(context.Background(), task)
	require.Error(t, err, "precondition: attempt 1 fails on part 2")

	rows, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	restarted := rows[0]
	require.NotEmpty(t, restarted.UploadID)
	require.NotZero(t, restarted.PartsFileMtime, "precondition: version snapshot persisted")

	// The local content of part 1's range changes, size and mtime restored —
	// every stat-based version gate still passes.
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteAt(bytesRepeat("B", mib), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	snapshotMtime := restarted.PartsFileMtime
	require.NoError(t, os.Chtimes(path, timeUnix(snapshotMtime), timeUnix(snapshotMtime)))

	// Attempt 2: the remote still reports part 1 with the md5 ETag of the
	// ORIGINAL bytes ("A" * 1 MiB), exactly like MinIO/SeaweedFS do.
	store2 := newIC51MultipartStore()
	store2.mockStore.uploadID = restarted.UploadID
	store2.listParts = []minio.ObjectPart{{PartNumber: 1, ETag: md5Hex(bytesRepeat("A", mib)), Size: int64(mib)}}
	store2.remotePartData[1] = bytesRepeat("A", mib)

	u2 := newWithStore(store2, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	result, err := u2.UploadFile(context.Background(), restarted)
	require.NoError(t, err, "scenario: the resume path completes (that is exactly the silent bug surface)")

	objectSHA := sha256Hex(store2.object)
	assert.Equal(t, objectSHA, result.SHA256,
		"reported SHA256 must equal the hash of the assembled object (old part 1 + whatever part 2 bytes were delivered)")
	assert.Equal(t, int64(len(store2.object)), result.SizeBytes,
		"reported SizeBytes must equal the size of the assembled object")
}

func bytesRepeat(s string, n int) []byte {
	b := []byte(s)
	out := make([]byte, 0, n)
	for len(out) < n {
		out = append(out, b...)
	}
	return out[:n]
}

func timeUnix(sec int64) time.Time { return time.Unix(sec, 0) }
