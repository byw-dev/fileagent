//go:build integration

package uploader

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

// minioITConfig reads MinIO connection parameters from environment variables,
// falling back to the docker-compose.test.yml defaults.
func minioITConfig() (endpoint, accessKey, secretKey string) {
	endpoint = os.Getenv("TEST_MINIO_ENDPOINT")
	if endpoint == "" {
		endpoint = "localhost:9000"
	}
	accessKey = os.Getenv("TEST_MINIO_ACCESS_KEY")
	if accessKey == "" {
		accessKey = "minioadmin"
	}
	secretKey = os.Getenv("TEST_MINIO_SECRET_KEY")
	if secretKey == "" {
		secretKey = "minioadmin"
	}
	return
}

const itBucket = "uploader-integration-test"

// setupITBucket creates the shared integration-test bucket if it does not yet
// exist.  It is idempotent and safe to call from multiple tests.
func setupITBucket(t *testing.T, client *minio.Client) {
	t.Helper()
	ctx := context.Background()
	exists, err := client.BucketExists(ctx, itBucket)
	require.NoError(t, err)
	if !exists {
		require.NoError(t, client.MakeBucket(ctx, itBucket, minio.MakeBucketOptions{}))
	}
}

// makeTempFile creates a temporary file of exactly size bytes filled with
// repeating ASCII data and returns its path.
func makeTempFile(t *testing.T, size int) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "testfile.dat")
	// Deterministic fill so SHA-256 is predictable.
	chunk := bytes.Repeat([]byte("ABCDEFGHIJ"), 1024) // 10 KiB chunk
	f, err := os.Create(path)
	require.NoError(t, err)
	written := 0
	for written < size {
		n := size - written
		if n > len(chunk) {
			n = len(chunk)
		}
		_, err = f.Write(chunk[:n])
		require.NoError(t, err)
		written += n
	}
	require.NoError(t, f.Close())
	return path
}

// sha256OfFile computes the hex-encoded SHA-256 digest of a local file.
func sha256OfFile(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	require.NoError(t, err)
	return hex.EncodeToString(h.Sum(nil))
}

// newTestQueue opens an in-memory SQLite queue and registers cleanup.
func newITQueue(t *testing.T) *queue.Queue {
	t.Helper()
	q, err := queue.Open(":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Close() })
	return q
}

// TestUploadFile_SinglePart_Integration verifies a <5 MiB file is uploaded
// via a single PutObject call and that the returned SHA-256 matches the local
// file digest.
//
// Prerequisites: docker compose -f deploy/docker-compose.test.yml up -d
func TestUploadFile_SinglePart_Integration(t *testing.T) {
	endpoint, accessKey, secretKey := minioITConfig()
	ctx := context.Background()

	rootClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	require.NoError(t, err)
	setupITBucket(t, rootClient)

	const fileSize = 1 * 1024 * 1024 // 1 MiB — well below the 5 MiB threshold
	localPath := makeTempFile(t, fileSize)
	wantSHA := sha256OfFile(t, localPath)
	objectKey := "single/upload-test.dat"

	t.Cleanup(func() {
		_ = rootClient.RemoveObject(ctx, itBucket, objectKey, minio.RemoveObjectOptions{})
	})

	q := newITQueue(t)
	task := &queue.UploadTask{
		ID:          "it-single",
		RuleID:      "rule-1",
		LocalPath:   localPath,
		Bucket:      itBucket,
		StoragePath: objectKey,
		Status:      queue.StatusPending,
	}
	require.NoError(t, q.Enqueue(task))

	u, err := New(Config{
		Endpoint:    endpoint,
		AccessKey:   accessKey,
		SecretKey:   secretKey,
		UseSSL:      false,
		ThresholdMB: 5, // 1 MiB file → single-part path
	}, q, zap.NewNop())
	require.NoError(t, err)

	result, err := u.UploadFile(ctx, task)
	require.NoError(t, err, "single-part upload must succeed")

	assert.Equal(t, wantSHA, result.SHA256, "SHA-256 must match the local file")
	assert.Equal(t, int64(fileSize), result.SizeBytes)
	assert.Equal(t, itBucket, result.Bucket)
	assert.Equal(t, objectKey, result.StoragePath)
	assert.NotEmpty(t, result.ETag)

	// Verify the object exists in MinIO with the correct size.
	info, err := rootClient.StatObject(ctx, itBucket, objectKey, minio.StatObjectOptions{})
	require.NoError(t, err, "uploaded object must be visible in MinIO")
	assert.Equal(t, int64(fileSize), info.Size)
}

// TestUploadFile_Multipart_Integration verifies a >5 MiB file is split into
// multiple parts, all parts are uploaded, and the assembled object has the
// correct SHA-256.
//
// The test uses a 7 MiB file with a 5 MiB part size, producing 2 parts
// (5 MiB + 2 MiB).  The non-final part meets MinIO's 5 MiB minimum.
//
// Prerequisites: docker compose -f deploy/docker-compose.test.yml up -d
func TestUploadFile_Multipart_Integration(t *testing.T) {
	endpoint, accessKey, secretKey := minioITConfig()
	ctx := context.Background()

	rootClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	require.NoError(t, err)
	setupITBucket(t, rootClient)

	const fileSize = 7 * 1024 * 1024 // 7 MiB → 2 parts (5 MiB + 2 MiB)
	localPath := makeTempFile(t, fileSize)
	wantSHA := sha256OfFile(t, localPath)
	objectKey := "multipart/upload-test.dat"

	t.Cleanup(func() {
		_ = rootClient.RemoveObject(ctx, itBucket, objectKey, minio.RemoveObjectOptions{})
	})

	q := newITQueue(t)
	task := &queue.UploadTask{
		ID:          "it-multipart",
		RuleID:      "rule-1",
		LocalPath:   localPath,
		Bucket:      itBucket,
		StoragePath: objectKey,
		Status:      queue.StatusPending,
	}
	require.NoError(t, q.Enqueue(task))

	u, err := New(Config{
		Endpoint:    endpoint,
		AccessKey:   accessKey,
		SecretKey:   secretKey,
		UseSSL:      false,
		ThresholdMB: 5, // 7 MiB > 5 MiB → multipart path
		PartSizeMB:  5, // 5 MiB parts → part1=5 MiB, part2=2 MiB (last)
	}, q, zap.NewNop())
	require.NoError(t, err)

	result, err := u.UploadFile(ctx, task)
	require.NoError(t, err, "multipart upload must succeed")

	assert.Equal(t, wantSHA, result.SHA256, "SHA-256 must match the local file")
	assert.Equal(t, int64(fileSize), result.SizeBytes)
	assert.Equal(t, itBucket, result.Bucket)
	assert.Equal(t, objectKey, result.StoragePath)
	assert.NotEmpty(t, result.ETag)

	// Verify the assembled object exists in MinIO with the correct size.
	info, err := rootClient.StatObject(ctx, itBucket, objectKey, minio.StatObjectOptions{})
	require.NoError(t, err, "assembled multipart object must be visible in MinIO")
	assert.Equal(t, int64(fileSize), info.Size)
}

// TestUploadFile_Resume_Integration verifies that a multipart upload
// interrupted after the first part can be correctly resumed.
//
// The test manually initiates a multipart upload and uploads part 1 via
// minio.Core, then constructs an UploadTask that records the upload ID.
// When UploadFile is called, it detects the partial state via ListObjectParts,
// skips part 1, uploads part 2, and completes the transfer.
//
// File layout: 6 MiB file, 5 MiB part size → 2 parts (5 MiB + 1 MiB).
//
// Prerequisites: docker compose -f deploy/docker-compose.test.yml up -d
func TestUploadFile_Resume_Integration(t *testing.T) {
	endpoint, accessKey, secretKey := minioITConfig()
	ctx := context.Background()

	rootClient, err := minio.New(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	require.NoError(t, err)
	setupITBucket(t, rootClient)

	const (
		fileSize   = 6 * 1024 * 1024 // 6 MiB
		partSizeMB = 5               // 5 MiB parts → part1=5 MiB (≥5 MiB min), part2=1 MiB (last)
		part1Size  = partSizeMB * 1024 * 1024
	)
	localPath := makeTempFile(t, fileSize)
	wantSHA := sha256OfFile(t, localPath)
	objectKey := "resume/upload-test.dat"

	t.Cleanup(func() {
		_ = rootClient.RemoveObject(ctx, itBucket, objectKey, minio.RemoveObjectOptions{})
	})

	// Manually initiate a multipart upload and upload part 1 to simulate an
	// interrupted transfer.
	core, err := minio.NewCore(endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(accessKey, secretKey, ""),
		Secure: false,
	})
	require.NoError(t, err)

	uploadID, err := core.NewMultipartUpload(ctx, itBucket, objectKey, minio.PutObjectOptions{})
	require.NoError(t, err, "initiating multipart upload must succeed")

	f, err := os.Open(localPath)
	require.NoError(t, err)
	part1, err := core.PutObjectPart(ctx, itBucket, objectKey, uploadID, 1,
		io.LimitReader(f, part1Size), part1Size, minio.PutObjectPartOptions{})
	f.Close()
	require.NoError(t, err, "uploading part 1 must succeed")
	_ = part1 // ETag will be retrieved by verifyRemoteParts during resume

	// Build a task with UploadID set; CompletedParts is intentionally empty
	// because verifyRemoteParts will query the actual MinIO state and override it.
	q := newITQueue(t)
	task := &queue.UploadTask{
		ID:          "it-resume",
		RuleID:      "rule-1",
		LocalPath:   localPath,
		Bucket:      itBucket,
		StoragePath: objectKey,
		Status:      queue.StatusPending,
		UploadID:    uploadID,
	}
	require.NoError(t, q.Enqueue(task))

	u, err := New(Config{
		Endpoint:    endpoint,
		AccessKey:   accessKey,
		SecretKey:   secretKey,
		UseSSL:      false,
		ThresholdMB: partSizeMB, // 6 MiB > 5 MiB → multipart path
		PartSizeMB:  partSizeMB,
	}, q, zap.NewNop())
	require.NoError(t, err)

	// UploadFile should detect that part 1 already exists (via ListObjectParts),
	// skip it, upload part 2, and complete the multipart upload.
	result, err := u.UploadFile(ctx, task)
	require.NoError(t, err, "resumed upload must complete successfully")

	assert.Equal(t, wantSHA, result.SHA256, "SHA-256 must match the local file after resume")
	assert.Equal(t, int64(fileSize), result.SizeBytes)
	assert.NotEmpty(t, result.ETag)

	// Verify the final object exists in MinIO with the correct size.
	info, err := rootClient.StatObject(ctx, itBucket, objectKey, minio.StatObjectOptions{})
	require.NoError(t, err, "completed object must be visible in MinIO after resume")
	assert.Equal(t, int64(fileSize), info.Size)
}
