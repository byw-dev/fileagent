package uploader

import (
	"bytes"
	"context"
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
	putErr        error
	initErr       error
	uploadErr     error
	completeErr   error
	uploadID      string
	parts         []minio.ObjectPart
}

func (m *mockStore) PutObject(_ context.Context, _, _ string, r io.Reader, _ int64, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
	m.putCalls++
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
	if m.uploadErr != nil {
		return minio.ObjectPart{}, m.uploadErr
	}
	_, _ = io.Copy(io.Discard, r)
	return minio.ObjectPart{PartNumber: m.uploadCalls, ETag: fmt.Sprintf("part-etag-%d", m.uploadCalls)}, nil
}

func (m *mockStore) ListObjectParts(_ context.Context, _, _, _ string, _, _ int) (minio.ListObjectPartsResult, error) {
	m.listCalls++
	return minio.ListObjectPartsResult{ObjectParts: m.parts}, nil
}

func (m *mockStore) CompleteMultipartUpload(_ context.Context, _, _, _ string, _ []minio.CompletePart, _ minio.PutObjectOptions) (minio.UploadInfo, error) {
	m.completeCalls++
	if m.completeErr != nil {
		return minio.UploadInfo{}, m.completeErr
	}
	return minio.UploadInfo{ETag: "etag-multi"}, nil
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
