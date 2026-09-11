package uploader

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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

// ── Mock ObjectStore ──────────────────────────────────────────────────────────

type mockStore struct {
	putCalls      int
	initCalls     int
	uploadCalls   int
	completeCalls int
	listCalls     int
	abortCalls    int
	// failOnPart, when non-zero, makes PutObjectPart fail on that part number
	// (1-indexed) and on every later part, simulating a mid-transfer failure.
	failOnPart   int
	putErr       error
	initErr      error
	uploadErr    error
	completeErr  error
	abortErr     error
	listErr      error
	uploadID     string
	parts        []minio.ObjectPart
	putFn        func(ctx context.Context, bucket, object string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
}

func (m *mockStore) PutObject(ctx context.Context, bucket, object string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	m.putCalls++
	if m.putFn != nil {
		return m.putFn(ctx, bucket, object, r, size, opts)
	}
	if m.putErr != nil {
		return minio.UploadInfo{}, m.putErr
	}
	// Drain the reader.
	_, _ = io.Copy(io.Discard, r)
	return minio.UploadInfo{ETag: "etag-single"}, nil
}

func (m *mockStore) NewMultipartUpload(_ context.Context, _, _ string, _ minio.PutObjectOptions) (string, error) {
	m.initCalls++
	if m.initErr != nil {
		return "", m.initErr
	}
	if m.uploadID == "" {
		m.uploadID = "upload-id-1"
	}
	return m.uploadID, nil
}

func (m *mockStore) PutObjectPart(_ context.Context, _, _, _ string, _ int, r io.Reader, _ int64, _ minio.PutObjectPartOptions) (minio.ObjectPart, error) {
	m.uploadCalls++
	if m.failOnPart > 0 && m.uploadCalls >= m.failOnPart {
		return minio.ObjectPart{}, fmt.Errorf("simulated part %d failure", m.uploadCalls)
	}
	if m.uploadErr != nil {
		return minio.ObjectPart{}, m.uploadErr
	}
	_, _ = io.Copy(io.Discard, r)
	return minio.ObjectPart{PartNumber: m.uploadCalls, ETag: fmt.Sprintf("part-etag-%d", m.uploadCalls)}, nil
}

func (m *mockStore) ListObjectParts(_ context.Context, _, _, _ string, _, _ int) (minio.ListObjectPartsResult, error) {
	m.listCalls++
	if m.listErr != nil {
		return minio.ListObjectPartsResult{}, m.listErr
	}
	return minio.ListObjectPartsResult{ObjectParts: m.parts}, nil
}

func (m *mockStore) CompleteMultipartUpload(_ context.Context, _, _, _ string, _ []minio.CompletePart, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
	m.completeCalls++
	if m.completeErr != nil {
		return minio.UploadInfo{}, m.completeErr
	}
	return minio.UploadInfo{ETag: "etag-multi"}, nil
}

func (m *mockStore) AbortMultipartUpload(_ context.Context, _, _, _ string) error {
	m.abortCalls++
	if m.abortErr != nil {
		return m.abortErr
	}
	return nil
}

// ── Helpers ───────────────────────────────────────────────────────────────────

func newTestQueue(t *testing.T) *queue.Queue {
	t.Helper()
	q, err := queue.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Close() })
	return q
}

func newTask(t *testing.T, path, bucket, storagePath string) *queue.UploadTask {
	t.Helper()
	return &queue.UploadTask{
		ID:          "task-" + t.Name(),
		RuleID:      "rule-1",
		LocalPath:   path,
		StoragePath: storagePath,
		Bucket:      bucket,
		Status:      queue.StatusPending,
	}
}

// ── Tests ─────────────────────────────────────────────────────────────────────

func TestUploadFile_SinglePart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.dat")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("x"), 1024), 0o644))

	store := &mockStore{}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "prefix/small.dat")
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(store, Config{PartSizeMB: 64}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err)

	assert.Equal(t, 1, store.putCalls)
	assert.Equal(t, 0, store.initCalls)
	assert.Equal(t, "etag-single", result.ETag)
	assert.Equal(t, "bucket1", result.Bucket)
	assert.Equal(t, "prefix/small.dat", result.StoragePath)
	assert.NotEmpty(t, result.SHA256)
	assert.Equal(t, int64(1024), result.SizeBytes)
}

func TestUploadFile_SinglePart_PutError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "small.dat")
	require.NoError(t, os.WriteFile(path, []byte("data"), 0o644))

	store := &mockStore{putErr: fmt.Errorf("network error")}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket1", "key")
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(store, Config{PartSizeMB: 64}, q, zap.NewNop())
	_, err := u.UploadFile(context.Background(), task)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network error")
}

func TestUploadFile_Multipart(t *testing.T) {
	dir := t.TempDir()
	// 3 MiB file with 1 MiB parts → 3 parts; use ThresholdMB=1 to force multipart
	size := 3 * 1024 * 1024
	path := filepath.Join(dir, "large.dat")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("A"), size), 0o644))

	store := &mockStore{}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket2", "large/file.dat")
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(store, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err)

	assert.Equal(t, 1, store.initCalls)
	assert.Equal(t, 3, store.uploadCalls)
	assert.Equal(t, 1, store.completeCalls)
	assert.Equal(t, "etag-multi", result.ETag)
	assert.Equal(t, int64(size), result.SizeBytes)
}

func TestUploadFile_Multipart_ResumeFromQueue(t *testing.T) {
	dir := t.TempDir()
	size := 2 * 1024 * 1024
	path := filepath.Join(dir, "resume.dat")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("B"), size), 0o644))

	// Pre-populate task with an existing upload ID and one completed part.
	task := newTask(t, path, "bucket3", "resume/file.dat")
	task.UploadID = "existing-upload-id"
	// Part 1 already done.
	task.CompletedParts = `{"parts":[{"part_number":1,"etag":"part-etag-1"}]}`

	store := &mockStore{
		uploadID: "existing-upload-id",
		parts:    []minio.ObjectPart{{PartNumber: 1, ETag: "part-etag-1"}},
	}
	q := newTestQueue(t)
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(store, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	result, err := u.UploadFile(context.Background(), task)
	require.NoError(t, err)

	// Should only upload part 2 (part 1 was already done).
	assert.Equal(t, 0, store.initCalls, "should not start a new multipart upload")
	assert.Equal(t, 1, store.uploadCalls, "should upload only the remaining part")
	assert.Equal(t, 1, store.completeCalls)
	assert.Equal(t, "etag-multi", result.ETag)
}

// TestUploadFile_Multipart_ProgressPersisted reproduces IC-BUG-5: after a
// mid-transfer failure, the initiated upload ID and the completed parts must
// be readable back from the SQLite queue, not only from the in-memory task —
// otherwise every retry rescans empty values and restarts from part 1.
func TestUploadFile_Multipart_ProgressPersisted(t *testing.T) {
	dir := t.TempDir()
	size := 2 * 1024 * 1024
	path := filepath.Join(dir, "progress.dat")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("C"), size), 0o644))

	store := &mockStore{failOnPart: 2}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket4", "progress/file.dat")
	require.NoError(t, q.Enqueue(task))

	u := newWithStore(store, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	_, err := u.UploadFile(context.Background(), task)
	require.Error(t, err, "part 2 must fail")

	// Simulate a restart: re-read the task from SQLite the way DequeuePending
	// would, and check the multipart state survived the round trip.
	rows, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	persisted := rows[0]

	assert.Equal(t, store.uploadID, persisted.UploadID,
		"upload ID must be persisted, not only held in memory")
	require.NotEmpty(t, persisted.CompletedParts,
		"completed part 1 must be persisted when it completes")
	var cp completedParts
	require.NoError(t, json.Unmarshal([]byte(persisted.CompletedParts), &cp))
	require.Len(t, cp.Parts, 1)
	assert.Equal(t, 1, cp.Parts[0].PartNumber,
		"the persisted progress must record part 1 as completed")
}

// TestUploadFile_Multipart_ResumeAfterCrash walks the full IC-BUG-5 scenario:
// attempt 1 fails mid-transfer (part 2), the process "crashes", the task is
// rescanned from SQLite (memory gone), and attempt 2 must resume the SAME
// upload — no new multipart initiation, part 1 not retransmitted.
func TestUploadFile_Multipart_ResumeAfterCrash(t *testing.T) {
	dir := t.TempDir()
	size := 2 * 1024 * 1024
	path := filepath.Join(dir, "crash.dat")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("D"), size), 0o644))

	store1 := &mockStore{failOnPart: 2}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket5", "crash/file.dat")
	require.NoError(t, q.Enqueue(task))

	u1 := newWithStore(store1, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	_, err := u1.UploadFile(context.Background(), task)
	require.Error(t, err)

	// "Restart": scan the task back from SQLite into a fresh in-memory struct.
	rows, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	restarted := rows[0]

	// Attempt 2 on a fresh uploader. MinIO still reports part 1 of the same
	// upload as uploaded. When the state survived the round trip, this is the
	// same upload ID attempt 1 initiated; when it did not (the bug), the empty
	// value forces a fresh initiation and the test fails below.
	store2 := &mockStore{
		uploadID: restarted.UploadID,
		parts:    []minio.ObjectPart{{PartNumber: 1, ETag: "part-etag-1"}},
	}
	u2 := newWithStore(store2, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	result, err := u2.UploadFile(context.Background(), restarted)
	require.NoError(t, err)

	assert.Equal(t, 0, store2.initCalls,
		"must resume the persisted upload ID, not initiate a new multipart upload")
	assert.Equal(t, 1, store2.uploadCalls,
		"part 1 must not be retransmitted; only part 2 remains")
	assert.Equal(t, 1, store2.completeCalls)
	assert.Equal(t, "etag-multi", result.ETag)
}

func TestUploadFile_FileMissing(t *testing.T) {
	store := &mockStore{}
	q := newTestQueue(t)
	task := newTask(t, "/nonexistent/path.dat", "bucket", "key")

	u := newWithStore(store, Config{PartSizeMB: 64}, q, zap.NewNop())
	_, err := u.UploadFile(context.Background(), task)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "stat")
}

func TestFileSHA256(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "checksum.txt")
	require.NoError(t, os.WriteFile(path, []byte("hello world"), 0o644))

	sha, err := fileSHA256(path)
	require.NoError(t, err)
	assert.Len(t, sha, 64, "SHA-256 hex string should be 64 characters")
	assert.NotEmpty(t, sha)
}

func TestLoadCompletedParts_InvalidJSON(t *testing.T) {
	u := newWithStore(&mockStore{}, Config{}, nil, zap.NewNop())
	parts := u.loadCompletedParts("not-valid-json")
	assert.Nil(t, parts)
}

func TestLoadCompletedParts_Empty(t *testing.T) {
	u := newWithStore(&mockStore{}, Config{}, nil, zap.NewNop())
	assert.Nil(t, u.loadCompletedParts(""))
}

func TestNewSectionReader_BadPath(t *testing.T) {
	_, err := newSectionReader("/nonexistent/file.dat", 0, 100)
	require.Error(t, err)
}

func TestNewSectionReader_WithOffset(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "data.bin")
	require.NoError(t, os.WriteFile(path, []byte("AAABBBCCC"), 0o644))

	sr, err := newSectionReader(path, 3, 3)
	require.NoError(t, err)
	defer sr.f.Close()

	buf, err := io.ReadAll(sr)
	require.NoError(t, err)
	assert.Equal(t, []byte("BBB"), buf)
}

func TestFileSHA256_BadPath(t *testing.T) {
	_, err := fileSHA256("/nonexistent/file.dat")
	require.Error(t, err)
}

func TestNewWithStore_DefaultPartSize(t *testing.T) {
	u := newWithStore(&mockStore{}, Config{PartSizeMB: 0}, nil, zap.NewNop())
	assert.Equal(t, 64, u.cfg.PartSizeMB)
	assert.Equal(t, 64, u.cfg.ThresholdMB)
}

func TestUploadFile_TailMode_UploadsTailOnly(t *testing.T) {
// Create a file with 10 bytes total.
dir := t.TempDir()
path := filepath.Join(dir, "data.txt")
require.NoError(t, os.WriteFile(path, []byte("0123456789"), 0o644))

var received []byte
store := &mockStore{
putFn: func(_ context.Context, _, _ string, r io.Reader, _ int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
b, err := io.ReadAll(r)
if err != nil {
return minio.UploadInfo{}, err
}
received = b
return minio.UploadInfo{ETag: "etag-tail"}, nil
},
}
q, _ := queue.Open(":memory:")
defer q.Close()

u := newWithStore(store, Config{ThresholdMB: 1}, q, zap.NewNop())
task := &queue.UploadTask{
ID:          "t1",
LocalPath:   path,
StoragePath: "obj",
Bucket:      "b",
FileOffset:  5,
AppendMode:  "tail",
}

res, err := u.UploadFile(context.Background(), task)
require.NoError(t, err)
assert.Equal(t, int64(5), res.SizeBytes)
assert.Equal(t, []byte("56789"), received)
}

func TestUploadFile_TailMode_OffsetBeyondEnd_NoOp(t *testing.T) {
dir := t.TempDir()
path := filepath.Join(dir, "data.txt")
require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

store := &mockStore{}
q, _ := queue.Open(":memory:")
defer q.Close()

u := newWithStore(store, Config{ThresholdMB: 1}, q, zap.NewNop())
task := &queue.UploadTask{
ID:          "t2",
LocalPath:   path,
StoragePath: "obj",
Bucket:      "b",
FileOffset:  10, // beyond file end
AppendMode:  "tail",
}

res, err := u.UploadFile(context.Background(), task)
require.NoError(t, err)
assert.Equal(t, int64(0), res.SizeBytes)
}

func TestUploadFile_SinglePart_SeekError(t *testing.T) {
// Create a tiny file (1 byte) and try to seek to offset 5 — stat says size 1
// so offset 5 >= size 1 so this becomes a no-op (no bytes to upload).
dir := t.TempDir()
path := filepath.Join(dir, "small.txt")
require.NoError(t, os.WriteFile(path, []byte("x"), 0o644))

store := &mockStore{}
q, _ := queue.Open(":memory:")
defer q.Close()

u := newWithStore(store, Config{ThresholdMB: 1}, q, zap.NewNop())
task := &queue.UploadTask{
ID: "s1", LocalPath: path, StoragePath: "obj", Bucket: "b",
FileOffset: 100, AppendMode: "tail",
}
// Offset >= file size: should return SizeBytes=0 without error.
res, err := u.UploadFile(context.Background(), task)
require.NoError(t, err)
assert.Equal(t, int64(0), res.SizeBytes)
}

func TestNewSectionReader_InvalidPath(t *testing.T) {
_, err := newSectionReader("/nonexistent/path.dat", 0, 10)
require.Error(t, err)
}

func TestNewSectionReader_ZeroSize(t *testing.T) {
dir := t.TempDir()
path := filepath.Join(dir, "data.bin")
require.NoError(t, os.WriteFile(path, []byte("hello"), 0o644))

sr, err := newSectionReader(path, 2, 3)
require.NoError(t, err)
defer sr.f.Close()

data, err := io.ReadAll(sr)
require.NoError(t, err)
assert.Len(t, data, 3)
}

// TestNew_ValidEndpoint verifies that New creates an Uploader without requiring
// an actual MinIO connection (minio.NewCore is lazy).
func TestNew_ValidEndpoint(t *testing.T) {
q, _ := queue.Open(":memory:")
defer q.Close()

u, err := New(Config{
Endpoint:  "localhost:9000",
AccessKey: "minioadmin",
SecretKey: "minioadmin",
UseSSL:    false,
}, q, zap.NewNop())
require.NoError(t, err)
assert.NotNil(t, u)
}

// ── AbandonUpload (IC-3 ②: terminal-state part cleanup) ──────────────────────

func TestAbandonUpload_AbortsRecordedUpload(t *testing.T) {
	store := &mockStore{}
	u := newWithStore(store, Config{}, nil, zap.NewNop())
	task := newTask(t, "/nonexistent", "bucket6", "key/obj")
	task.UploadID = "upload-live-1"

	require.NoError(t, u.AbandonUpload(context.Background(), task))
	assert.Equal(t, 1, store.abortCalls)
}

func TestAbandonUpload_NoUploadIDIsNoOp(t *testing.T) {
	store := &mockStore{}
	u := newWithStore(store, Config{}, nil, zap.NewNop())

	require.NoError(t, u.AbandonUpload(context.Background(), newTask(t, "/p", "b", "k")))
	require.NoError(t, u.AbandonUpload(context.Background(), nil))
	assert.Zero(t, store.abortCalls)
}

func TestAbandonUpload_ToleratesNoSuchUpload(t *testing.T) {
	// An upload already completed/aborted/expired reports NoSuchUpload — there
	// is nothing left to clean, so this is success, not an error.
	store := &mockStore{abortErr: minio.ErrorResponse{Code: "NoSuchUpload"}}
	u := newWithStore(store, Config{}, nil, zap.NewNop())
	task := newTask(t, "/p", "b", "k")
	task.UploadID = "upload-gone"

	require.NoError(t, u.AbandonUpload(context.Background(), task))
}

func TestAbandonUpload_PropagatesOtherErrors(t *testing.T) {
	store := &mockStore{abortErr: fmt.Errorf("network down")}
	u := newWithStore(store, Config{}, nil, zap.NewNop())
	task := newTask(t, "/p", "b", "k")
	task.UploadID = "upload-live-2"

	err := u.AbandonUpload(context.Background(), task)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "network down")
}

// TestUploadFile_Multipart_ResumeVerifyFails starts fresh without aborting:
// verifyRemoteParts may fail transiently (e.g. network down) and the recorded
// upload may still be resumable, so a failed verification must NOT abort it —
// the next attempt reconciles again (IC-3 ② deliberately keeps this path
// abort-free).
func TestUploadFile_Multipart_ResumeVerifyFails(t *testing.T) {
	dir := t.TempDir()
	size := 2 * 1024 * 1024
	path := filepath.Join(dir, "verifyfail.dat")
	require.NoError(t, os.WriteFile(path, bytes.Repeat([]byte("E"), size), 0o644))

	store1 := &mockStore{failOnPart: 2}
	q := newTestQueue(t)
	task := newTask(t, path, "bucket7", "verifyfail/file.dat")
	require.NoError(t, q.Enqueue(task))

	u1 := newWithStore(store1, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	_, err := u1.UploadFile(context.Background(), task)
	require.Error(t, err)

	rows, err := q.ListByStatus(queue.StatusPending)
	require.NoError(t, err)
	require.Len(t, rows, 1)
	restarted := rows[0]
	require.NotEmpty(t, restarted.UploadID, "precondition: progress persisted")

	// Second attempt: ListObjectParts fails (transient outage), but the abort
	// hook must not be consulted here — the upload is abandoned via a fresh
	// initiation only because verification failed.
	store2 := &mockStore{listErr: fmt.Errorf("minio unreachable"), uploadID: restarted.UploadID}
	u2 := newWithStore(store2, Config{PartSizeMB: 1, ThresholdMB: 1}, q, zap.NewNop())
	_, err = u2.UploadFile(context.Background(), restarted)
	require.NoError(t, err, "verification failure falls back to a fresh upload")
	assert.Zero(t, store2.abortCalls,
		"a resumable upload must not be aborted just because the remote check failed")
}
