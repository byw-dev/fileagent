package uploader

import (
	"context"
	"crypto/md5"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/minio/minio-go/v7"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// ── IC-BUG-51 acceptance tests (spec 第二步 1) ────────────────────────────────

// TestIC51_SinglePart_NormalPath_HashAndSizeFromTheStream pins the happy
// path: the reported sha256/size are the hash/count of exactly the bytes the
// store received (here: a store that fully drains its reader).
func TestIC51_SinglePart_NormalPath_HashAndSizeFromTheStream(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "normal.dat")
	content := bytesRepeat("Z", 4096)
	require.NoError(t, os.WriteFile(path, content, 0o644))

	store := &ic51SingleStore{mockStore: &mockStore{}}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/normal.dat")
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(store, Config{PartSizeMB: 64}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err)

	assert.Equal(t, sha256Hex(store.received), result.SHA256)
	assert.Equal(t, sha256Hex(content), result.SHA256, "unchanged file: stream hash equals the file hash")
	assert.Equal(t, int64(len(content)), result.SizeBytes)
}

// TestIC51_SinglePart_TruncatedMidUpload_FailsLoudly: the source file is
// truncated after the first chunk has been read. The declared Content-Length
// can no longer be satisfied — the upload must FAIL, never report success
// with a size or hash that does not describe an object.
func TestIC51_SinglePart_TruncatedMidUpload_FailsLoudly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "trunc.dat")
	require.NoError(t, os.WriteFile(path, bytesRepeat("A", 4096), 0o644))

	store := &ic51SingleStore{mockStore: &mockStore{}, readLimit: 2048}
	store.afterFirstRead = func() {
		// Shrink the file mid-upload: the reader now hits EOF at 2048 bytes
		// although 4096 were declared.
		require.NoError(t, os.Truncate(path, 2048))
	}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/trunc.dat")
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(store, Config{PartSizeMB: 64}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.Error(t, err, "a truncated source must fail the upload, not succeed with a wrong description")
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "byte count mismatch")
}

// TestIC51_SinglePart_GrowsMidUpload_ReportsDeliveredBytes: the source grows
// during the upload. Production pins the request to the stat size, so exactly
// the first N bytes are delivered (readLimit models that). The reported
// sha256/size must describe THOSE bytes — the prefix — and both must agree.
func TestIC51_SinglePart_GrowsMidUpload_ReportsDeliveredBytes(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "grow.dat")
	require.NoError(t, os.WriteFile(path, bytesRepeat("A", 4096), 0o644))

	store := &ic51SingleStore{mockStore: &mockStore{}, readLimit: 4096}
	store.afterFirstRead = func() {
		// The writer appends after the uploader's stat: the object still gets
		// exactly the 4096 declared bytes.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o644)
		require.NoError(t, err)
		_, err = f.Write(bytesRepeat("B", 1024))
		require.NoError(t, err)
		require.NoError(t, f.Close())
	}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/grow.dat")
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(store, Config{PartSizeMB: 64}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err)

	assert.Len(t, store.received, 4096, "store received exactly the declared prefix")
	assert.Equal(t, sha256Hex(store.received), result.SHA256,
		"reported sha256 describes the delivered prefix, not the grown file")
	assert.Equal(t, int64(4096), result.SizeBytes)
	grown := append(append([]byte{}, bytesRepeat("A", 4096)...), bytesRepeat("B", 1024)...)
	assert.NotEqual(t, sha256Hex(grown), result.SHA256)
}

// TestIC51_Multipart_ResumeMatchingETag_NoRetransmission: part 1's remote
// ETag matches the re-read local bytes. The resume must NOT re-upload part 1
// (proven by the upload-call counter), must complete, and the reported
// sha256/size must describe old-part-1 + new-part-2.
func TestIC51_Multipart_ResumeMatchingETag_NoRetransmission(t *testing.T) {
	const mib = 1024 * 1024
	dir := t.TempDir()
	path := filepath.Join(dir, "match.dat")
	require.NoError(t, os.WriteFile(path, bytesRepeat("M", 2*mib), 0o644))

	// Attempt 1: part 1 up, part 2 fails.
	store1 := &mockStore{failOnPart: 2}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/match.dat")
	require.NoError(t, q.Enqueue(task))
	u1 := newWithStore(store1, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	_, err := u1.UploadFile(context.Background(), task)
	require.Error(t, err)

	rows, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	restarted := rows[0]

	// Attempt 2: remote part 1 ETag matches local bytes ("M"*1MiB).
	store2 := newIC51MultipartStore()
	store2.mockStore.uploadID = restarted.UploadID
	store2.listParts = []minio.ObjectPart{{PartNumber: 1, ETag: md5Hex(bytesRepeat("M", mib)), Size: int64(mib)}}
	store2.remotePartData[1] = bytesRepeat("M", mib)

	u2 := newWithStore(store2, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	result, err := u2.UploadFile(context.Background(), restarted)
	require.NoError(t, err)

	assert.Equal(t, []int{2}, store2.uploadedPartNum,
		"part 1 must NOT be retransmitted — only part 2 goes up")
	assert.Zero(t, store2.mockStore.initCalls, "the recorded upload is resumed, not re-initiated")
	assert.Equal(t, sha256Hex(store2.object), result.SHA256,
		"reported sha256 = hash of assembled object (remote part1 + delivered part2)")
	assert.Equal(t, sha256Hex(bytesRepeat("M", 2*mib)), result.SHA256)
	assert.Equal(t, int64(2*mib), result.SizeBytes)
}

// TestIC51_Multipart_ResumeETagMismatch_DiscardsAndStartsOver: the local
// bytes of a recorded part changed (mtime/size restored so the stat gates
// pass). The ETag verification must fail → discard the recorded upload
// (durable abort intent + abort) → upload BOTH parts from the current file.
func TestIC51_Multipart_ResumeETagMismatch_DiscardsAndStartsOver(t *testing.T) {
	const mib = 1024 * 1024
	dir := t.TempDir()
	path := filepath.Join(dir, "mismatch.dat")
	require.NoError(t, os.WriteFile(path, bytesRepeat("A", 2*mib), 0o644))

	store1 := &mockStore{failOnPart: 2}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/mismatch.dat")
	require.NoError(t, q.Enqueue(task))
	u1 := newWithStore(store1, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	_, err := u1.UploadFile(context.Background(), task)
	require.Error(t, err)

	rows, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	restarted := rows[0]
	oldUploadID := restarted.UploadID

	// Rewrite part 1's range in place, restore mtime: only the ETag can see it.
	f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
	require.NoError(t, err)
	_, err = f.WriteAt(bytesRepeat("B", mib), 0)
	require.NoError(t, err)
	require.NoError(t, f.Close())
	require.NoError(t, os.Chtimes(path, timeUnix(restarted.PartsFileMtime), timeUnix(restarted.PartsFileMtime)))

	// Remote part 1 ETag = md5 of the ORIGINAL bytes.
	store2 := newIC51MultipartStore()
	store2.mockStore.uploadID = "fresh-after-mismatch"
	store2.listParts = []minio.ObjectPart{{PartNumber: 1, ETag: md5Hex(bytesRepeat("A", mib)), Size: int64(mib)}}
	store2.remotePartData[1] = bytesRepeat("A", mib)

	u2 := newWithStore(store2, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	result, err := u2.UploadFile(context.Background(), restarted)
	require.NoError(t, err)

	assert.Equal(t, 1, store2.mockStore.abortCalls,
		"the mismatched recorded upload must be aborted (durable discard path)")
	assert.Equal(t, 1, store2.mockStore.initCalls,
		"a fresh multipart upload must be initiated after the discard")
	assert.Equal(t, []int{1, 2}, store2.uploadedPartNum,
		"NO part may be skipped after the discard: both parts come from the current file")
	assert.Equal(t, sha256Hex(store2.object), result.SHA256,
		"reported sha256 = hash of the object actually assembled (all current bytes)")
	spliced := append(append([]byte{}, bytesRepeat("B", mib)...), bytesRepeat("A", mib)...)
	assert.Equal(t, sha256Hex(spliced), result.SHA256,
		"object = current file content: rewritten part 1 + untouched part 2")
	assert.Equal(t, int64(2*mib), result.SizeBytes)

	entries, err := q.DueMultipartAborts(context.Background(), timeUnixNowPlus(0), 10)
	require.NoError(t, err)
	assert.Empty(t, entries, "the successful abort removed the durable abort record")

	rows, err = q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	assert.Equal(t, "fresh-after-mismatch", rows[0].UploadID,
		"the fresh upload id replaced the discarded one")
	assert.NotEqual(t, oldUploadID, rows[0].UploadID)
}

// Coordinator point 1: a remote ETag that is NOT a comparable md5 digest
// (SSE-encrypted object, gateway quirk, future backend change) must be
// treated as a MISMATCH — discard and re-transmit — never as "verified".
func TestIC51_Multipart_ResumeNonComparableETag_Discards(t *testing.T) {
	const mib = 1024 * 1024
	dir := t.TempDir()
	path := filepath.Join(dir, "opaque.dat")
	require.NoError(t, os.WriteFile(path, bytesRepeat("O", 2*mib), 0o644))

	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/opaque.dat")
	task.UploadID = "opaque-upload-id"
	task.CompletedParts = `{"parts":[{"part_number":1,"etag":"opaque-not-md5"}]}`
	if info, statErr := os.Stat(path); assert.NoError(t, statErr) {
		task.PartsFileMtime = info.ModTime().Unix()
		task.PartsFileSize = info.Size()
	}
	require.NoError(t, q.Enqueue(task))

	store := newIC51MultipartStore()
	store.mockStore.uploadID = "opaque-fresh"
	store.listParts = []minio.ObjectPart{{PartNumber: 1, ETag: "opaque-not-md5", Size: int64(mib)}}

	u := newWithStore(store, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err)

	assert.Equal(t, 1, store.mockStore.abortCalls, "an unverifiable resume is discarded, not trusted")
	assert.Equal(t, 1, store.mockStore.initCalls)
	assert.Equal(t, []int{1, 2}, store.uploadedPartNum, "both parts re-transmitted from the local file")
	assert.Equal(t, sha256Hex(store.object), result.SHA256)
}

// Coordinator point 2: the local file SHRANK between attempts, so re-reading
// a recorded part's range yields fewer bytes than the part declares. This is
// not a panic and not a silent under-count: the resume is discarded and the
// current (short) file is uploaded from scratch.
func TestIC51_Multipart_ResumeLocalShortRead_Discards(t *testing.T) {
	const mib = 1024 * 1024
	dir := t.TempDir()
	path := filepath.Join(dir, "short.dat")
	require.NoError(t, os.WriteFile(path, bytesRepeat("S", 2*mib), 0o644))

	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/short.dat")
	task.UploadID = "short-upload-id"
	task.CompletedParts = fmt.Sprintf(`{"parts":[{"part_number":1,"etag":"%s"}]}`, md5Hex(bytesRepeat("S", mib)))
	if info, statErr := os.Stat(path); assert.NoError(t, statErr) {
		task.PartsFileMtime = info.ModTime().Unix()
		task.PartsFileSize = info.Size()
	}
	require.NoError(t, q.Enqueue(task))

	store := newIC51MultipartStore()
	store.mockStore.uploadID = "short-fresh"
	store.listParts = []minio.ObjectPart{{PartNumber: 1, ETag: md5Hex(bytesRepeat("S", mib)), Size: int64(mib)}}
	// The shrink happens AFTER UploadFile's entry stat and AFTER the resume
	// decision, but BEFORE the part range is re-read: exactly the window the
	// short-read guard exists for. The uploader does not re-stat, so the
	// version gate cannot catch this — only the byte count can.
	store.afterList = func() {
		require.NoError(t, os.Truncate(path, 512*1024))
		require.NoError(t, os.Chtimes(path, timeUnix(task.PartsFileMtime), timeUnix(task.PartsFileMtime)))
	}

	u := newWithStore(store, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	// The resume itself is discarded safely (abort + fresh initiation), but
	// the fresh attempt then hits the truncated file: part 1 can only deliver
	// 512 KiB of the planned 1 MiB, and THAT fails loudly — the task retries
	// from scratch with a correct view of the (new) file size. Loud failure
	// is the correct outcome; a silent splice would not be.
	require.Error(t, err)
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "byte count mismatch")

	assert.Equal(t, 1, store.mockStore.abortCalls,
		"the shrunken local file cannot describe the remote part: discard, don't splice")
	assert.Equal(t, 1, store.mockStore.initCalls)
	assert.Equal(t, []int{1}, store.uploadedPartNum,
		"the fresh attempt re-uploads from part 1 and trips the byte-count guard on the truncated source")
	assert.Empty(t, store.object, "no object may be assembled from a short read")
}

// Coordinator point 1 (single-part half): a PutObject ETag that IS comparable
// but does not match the delivered bytes' md5 must fail the upload.
func TestIC51_SinglePart_ETagMismatch_FailsLoudly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "etagbad.dat")
	require.NoError(t, os.WriteFile(path, bytesRepeat("Q", 1024), 0o644))

	// A store that claims a WRONG md5 etag for what it received.
	h := md5.Sum(bytesRepeat("X", 1024))
	wrongETag := `"` + hex.EncodeToString(h[:]) + `"`
	store := &mockStore{putFn: func(_ context.Context, _, _ string, r io.Reader, _ int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
		_, _ = io.Copy(io.Discard, r)
		return minio.UploadInfo{ETag: wrongETag}, nil
	}}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/etagbad.dat")
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(store, Config{PartSizeMB: 64}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.Error(t, err, "a mismatching comparable ETag must fail the upload")
	assert.Nil(t, result)
	assert.Contains(t, err.Error(), "ETag")
}

func timeUnixNowPlus(sec int64) time.Time { return time.Now().Add(time.Duration(sec) * time.Second) }

// ── Review-r1 fix 1: non-contiguous remote part sets ─────────────────────────

// TestIC51_Multipart_ResumeNonContiguous_HashMatchesAssembledObject is the
// review-r1 blocker case: the remote already holds parts 1 and 3, part 2 is
// missing. The digest must be fed 1+2+3 (each part at its own position), the
// complete list must ascend, and the reported hash must equal the hash of the
// object the backend assembles BY PART NUMBER. The fake store assembles by
// part number like a real S3 backend — the previous fake appended in list
// order and masked exactly this bug.
func TestIC51_Multipart_ResumeNonContiguous_HashMatchesAssembledObject(t *testing.T) {
	const mib = 1024 * 1024
	dir := t.TempDir()
	path := filepath.Join(dir, "noncontig.dat")
	content := append(append([]byte{}, bytesRepeat("A", mib)...), bytesRepeat("B", mib)...)
	content = append(content, bytesRepeat("C", mib)...)
	require.NoError(t, os.WriteFile(path, content, 0o644))

	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/noncontig.dat")
	task.UploadID = "noncontig-upload-id"
	// SQLite's recorded parts say only part 1 — the remote list (the
	// authority) will correct this to {1,3}.
	task.CompletedParts = `{"parts":[{"part_number":1,"etag":"` + md5Hex(content[:mib]) + `"}]}`
	st, err := os.Stat(path)
	require.NoError(t, err)
	task.PartsFileMtime = st.ModTime().Unix()
	task.PartsFileSize = st.Size()
	require.NoError(t, q.Enqueue(task))

	store := newIC51MultipartStore()
	store.mockStore.uploadID = "noncontig-upload-id"
	store.listParts = []minio.ObjectPart{
		{PartNumber: 1, ETag: md5Hex(content[:mib]), Size: int64(mib)},
		{PartNumber: 3, ETag: md5Hex(content[2*mib:]), Size: int64(mib)},
	}
	store.remotePartData[1] = content[:mib]
	store.remotePartData[3] = content[2*mib:]

	u := newWithStore(store, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err)

	assert.Equal(t, []int{2}, store.uploadedPartNum,
		"only the missing part 2 is transmitted; parts 1 and 3 are resumed")
	assert.Zero(t, store.mockStore.initCalls, "the recorded upload is resumed")
	// The object is assembled 1+2+3 by part number; the reported hash must
	// describe exactly that, NOT the feeding-order artifact 1+3+2.
	assert.Equal(t, sha256Hex(store.object), result.SHA256,
		"reported sha256 must equal the hash of the object assembled BY PART NUMBER")
	assert.Equal(t, sha256Hex(content), result.SHA256)
	assert.Equal(t, int64(3*mib), result.SizeBytes)
	// And the complete list the client sent must ascend 1,2,3 — minio-go
	// does not sort it, we must.
	assert.Equal(t, []int{1, 2, 3}, store.completedPartNums,
		"CompleteMultipartUpload must receive parts in ascending order")
}

// TestIC51_Multipart_ResumeRemoteDisagreesWithSQLite: the remote part list is
// the authority (P1-a) — here SQLite claims parts {1,2} while the remote only
// holds part 2. The resume must follow the REMOTE set (upload 1 and 3, skip
// 2), hash in order 1+2+3, and succeed.
func TestIC51_Multipart_ResumeRemoteDisagreesWithSQLite(t *testing.T) {
	const mib = 1024 * 1024
	dir := t.TempDir()
	path := filepath.Join(dir, "remote-authority.dat")
	content := append(append([]byte{}, bytesRepeat("X", mib)...), bytesRepeat("Y", mib)...)
	content = append(content, bytesRepeat("Z", mib)...)
	require.NoError(t, os.WriteFile(path, content, 0o644))

	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/remote-authority.dat")
	task.UploadID = "ra-upload-id"
	// SQLite optimistically recorded parts 1 and 2; the remote (authority)
	// only has part 2 — e.g. part 1's UploadPart request never landed.
	cp := fmt.Sprintf(`{"parts":[{"part_number":1,"etag":"%s"},{"part_number":2,"etag":"%s"}]}`,
		md5Hex(content[:mib]), md5Hex(content[mib:2*mib]))
	task.CompletedParts = cp
	st, err := os.Stat(path)
	require.NoError(t, err)
	task.PartsFileMtime = st.ModTime().Unix()
	task.PartsFileSize = st.Size()
	require.NoError(t, q.Enqueue(task))

	store := newIC51MultipartStore()
	store.mockStore.uploadID = "ra-upload-id"
	store.listParts = []minio.ObjectPart{
		{PartNumber: 2, ETag: md5Hex(content[mib : 2*mib]), Size: int64(mib)},
	}
	store.remotePartData[2] = content[mib : 2*mib]

	u := newWithStore(store, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err)

	assert.Equal(t, []int{1, 3}, store.uploadedPartNum,
		"the remote list is the authority: parts 1 and 3 are missing there and must be uploaded; SQLite's stale part-1 claim is ignored")
	assert.Equal(t, sha256Hex(store.object), result.SHA256)
	assert.Equal(t, sha256Hex(content), result.SHA256,
		"object = part1(new) + part2(remote, verified) + part3(new), hashed in order")
	assert.Equal(t, int64(3*mib), result.SizeBytes)
	assert.Equal(t, []int{1, 2, 3}, store.completedPartNums)
}

// ── Rework-2 fix 1: the digest-feeding read and the ETag check are ONE read ──

// TestIC51_Multipart_RewriteBetweenPrecheckAndHashFeed is the rework-2
// blocker case: the up-front ETag pre-check and the digest-feeding read are
// two separate reads of the same range. If the file is rewritten between
// them, the OLD code fed the SECOND read's bytes into the SHA-256 while only
// the FIRST read was ever verified — the reported hash could describe bytes
// the object does not contain. The fix re-verifies the remote ETag over the
// very bytes entering the digest (hashResumePartChecked); this test rewrites
// part 3's range while part 2 is being uploaded — after the pre-check of
// part 3 passed, before part 3 is fed — and requires the resume to be
// discarded and the file re-uploaded from scratch with a correct hash.
func TestIC51_Multipart_RewriteBetweenPrecheckAndHashFeed(t *testing.T) {
	const mib = 1024 * 1024
	dir := t.TempDir()
	path := filepath.Join(dir, "window.dat")
	content := append(append([]byte{}, bytesRepeat("A", mib)...), bytesRepeat("B", mib)...)
	content = append(content, bytesRepeat("C", mib)...)
	require.NoError(t, os.WriteFile(path, content, 0o644))

	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "ic51/window.dat")
	task.UploadID = "window-upload-id"
	task.CompletedParts = `{"parts":[]}`
	st, err := os.Stat(path)
	require.NoError(t, err)
	task.PartsFileMtime = st.ModTime().Unix()
	task.PartsFileSize = st.Size()
	require.NoError(t, q.Enqueue(task))

	store := newIC51MultipartStore()
	store.mockStore.uploadID = "window-fresh"
	// Remote holds parts 1 and 3 (non-contiguous), both with ETags of the
	// ORIGINAL bytes — the pre-check over parts 1 and 3 passes on the
	// original content.
	store.listParts = []minio.ObjectPart{
		{PartNumber: 1, ETag: md5Hex(content[:mib]), Size: int64(mib)},
		{PartNumber: 3, ETag: md5Hex(content[2*mib:]), Size: int64(mib)},
	}
	store.remotePartData[1] = content[:mib]
	store.remotePartData[3] = content[2*mib:]

	// The ordered loop: part 1 (skipped — read+verified against the ORIGINAL
	// content), part 2 (uploaded), part 3 (skipped). Rewrite part 3's range
	// while part 2 streams: the pre-check for part 3 has ALREADY passed, but
	// the bytes about to enter the digest differ from what the remote holds.
	rewritten := false
	store.beforePutPart = func(partNum int) {
		if partNum == 2 && !rewritten {
			rewritten = true
			f, err := os.OpenFile(path, os.O_WRONLY, 0o644)
			require.NoError(t, err)
			_, err = f.WriteAt(bytesRepeat("D", mib), 2*mib)
			require.NoError(t, err)
			require.NoError(t, f.Close())
		}
	}

	u := newWithStore(store, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err, "the in-read ETag check discards the resume and the fresh upload of the current file succeeds")

	// The stale upload (with remote part 3 of the ORIGINAL bytes) must be
	// discarded: abort + fresh initiation + the current file fully uploaded
	// — no splice, and the reported hash describes the current content.
	assert.Equal(t, 1, store.mockStore.abortCalls,
		"the rewrite window between pre-check and digest-feed must discard the resume")
	assert.Equal(t, 1, store.mockStore.initCalls, "a fresh multipart upload is initiated after the discard")
	// First attempt uploaded part 2 before part 3's in-read check tripped; the
	// fresh attempt uploads ALL THREE parts from the current file.
	assert.Equal(t, []int{2, 1, 2, 3}, store.uploadedPartNum,
		"first attempt: part 2 up, part 3's in-read ETag check trips; after the discard NO part may be skipped")
	current := append(append([]byte{}, content[:2*mib]...), bytesRepeat("D", mib)...)
	assert.Equal(t, sha256Hex(store.object), result.SHA256,
		"reported sha256 = hash of the object actually assembled")
	assert.Equal(t, sha256Hex(current), result.SHA256,
		"the object is the CURRENT file content (A+B+D), not a splice with the stale remote part 3")
	assert.Equal(t, int64(3*mib), result.SizeBytes)
}

// TestIC51_Multipart_StreamedRanges_NoFullPartBuffer pins the rework-2 fix 2
// helper contract: skipped-part ranges are processed STREAMING (io.Copy into
// the digest/MD5 writers via LimitReader — no whole-part buffering; a part
// can be 64 MiB and the agent runs on edge devices). Correctness of the
// streamed digests and the short-read error path are asserted directly.
func TestIC51_Multipart_StreamedRanges_NoFullPartBuffer(t *testing.T) {
	const mib = 1024 * 1024
	dir := t.TempDir()
	path := filepath.Join(dir, "stream.dat")
	content := bytesRepeat("S", 16*mib)
	require.NoError(t, os.WriteFile(path, content, 0o644))

	// The streamed MD5 of the whole range equals the in-memory digest, and
	// the byte count is exact.
	got, n, err := streamRangeMD5(path, 0, int64(len(content)))
	require.NoError(t, err)
	assert.Equal(t, int64(len(content)), n)
	h := md5.Sum(content)
	assert.Equal(t, hex.EncodeToString(h[:]), got)

	var count int64
	sink := countingWriter{n: &count}
	n, err = streamRangeInto(path, 4*mib, 8*mib, sink)
	require.NoError(t, err)
	assert.Equal(t, int64(8*mib), n)
	assert.Equal(t, int64(8*mib), count)

	// Short read is an error, not a partial success.
	_, _, err = streamRangeMD5(path, 0, int64(len(content))+1)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "shrank")
}

// countingWriter counts bytes written to it (a test sink for
// streamRangeInto).
type countingWriter struct{ n *int64 }

func (w countingWriter) Write(p []byte) (int, error) {
	*w.n += int64(len(p))
	return len(p), nil
}
