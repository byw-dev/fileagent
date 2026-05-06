//go:build integration

package uploader_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"testing"
	"time"

	miniogo "github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/byw-dev/fileagent/agent/internal/uploader"
)

const (
	itEndpoint = "localhost:9000"
	itUser     = "minioadmin"
	itPassword = "minioadmin"
)

// newTestQueue creates a temp-file-backed SQLite queue and registers cleanup.
func newTestQueue(t *testing.T) *queue.Queue {
	t.Helper()
	tmp, err := os.CreateTemp(t.TempDir(), "queue-*.db")
	require.NoError(t, err)
	require.NoError(t, tmp.Close())
	q, err := queue.Open(tmp.Name())
	require.NoError(t, err)
	t.Cleanup(func() { _ = q.Close() })
	return q
}

// newTestBucket creates a uniquely-named MinIO bucket and registers cleanup.
func newTestBucket(t *testing.T, mc *miniogo.Client) string {
	t.Helper()
	bucket := fmt.Sprintf("uploader-it-%d", time.Now().UnixNano())
	err := mc.MakeBucket(context.Background(), bucket, miniogo.MakeBucketOptions{})
	require.NoError(t, err, "create test bucket")
	t.Cleanup(func() {
		for obj := range mc.ListObjects(context.Background(), bucket, miniogo.ListObjectsOptions{Recursive: true}) {
			_ = mc.RemoveObject(context.Background(), bucket, obj.Key, miniogo.RemoveObjectOptions{})
		}
		_ = mc.RemoveBucket(context.Background(), bucket)
	})
	return bucket
}

// writeTempFile creates a temporary file of sizeBytes filled with a repeating
// ASCII pattern and returns its path.
func writeTempFile(t *testing.T, sizeBytes int) string {
	t.Helper()
	f, err := os.CreateTemp(t.TempDir(), "upload-*.bin")
	require.NoError(t, err)
	pattern := []byte("0123456789ABCDEF")
	written := 0
	for written < sizeBytes {
		chunk := pattern
		if rem := sizeBytes - written; rem < len(chunk) {
			chunk = chunk[:rem]
		}
		n, err := f.Write(chunk)
		require.NoError(t, err)
		written += n
	}
	require.NoError(t, f.Close())
	return f.Name()
}

// sha256File returns the hex-encoded SHA-256 digest of the file at path.
func sha256File(t *testing.T, path string) string {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	defer f.Close()
	h := sha256.New()
	_, err = io.Copy(h, f)
	require.NoError(t, err)
	return hex.EncodeToString(h.Sum(nil))
}

// TestUploader_SinglePart_Integration verifies that a small file (below
// ThresholdMB) is uploaded via PutObject and the result matches the expected
// SHA-256 and file size.
func TestUploader_SinglePart_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	mc, err := miniogo.New(itEndpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(itUser, itPassword, ""),
		Secure: false,
	})
	require.NoError(t, err)

	bucket := newTestBucket(t, mc)
	q := newTestQueue(t)

	// 2 MiB file — below ThresholdMB=4 → single-part upload.
	const fileSize = 2 * 1024 * 1024
	filePath := writeTempFile(t, fileSize)
	expectedSHA := sha256File(t, filePath)

	logger, _ := zap.NewDevelopment()
	u, err := uploader.New(uploader.Config{
		Endpoint:    itEndpoint,
		AccessKey:   itUser,
		SecretKey:   itPassword,
		UseSSL:      false,
		ThresholdMB: 4,
	}, q, logger)
	require.NoError(t, err)

	task := &queue.UploadTask{
		ID:          "it-single-1",
		RuleID:      "rule-1",
		LocalPath:   filePath,
		StoragePath: "uploads/single-test.bin",
		Bucket:      bucket,
	}

	result, err := u.UploadFile(ctx, task)
	require.NoError(t, err)

	assert.Equal(t, expectedSHA, result.SHA256)
	assert.Equal(t, int64(fileSize), result.SizeBytes)
	assert.Equal(t, bucket, result.Bucket)
	assert.Equal(t, "uploads/single-test.bin", result.StoragePath)
	assert.NotEmpty(t, result.ETag)

	info, err := mc.StatObject(ctx, bucket, "uploads/single-test.bin", miniogo.StatObjectOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(fileSize), info.Size)
}

// TestUploader_Multipart_Integration verifies that a large file (above
// ThresholdMB) is uploaded via multipart upload and the result matches the
// expected SHA-256.
func TestUploader_Multipart_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	mc, err := miniogo.New(itEndpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(itUser, itPassword, ""),
		Secure: false,
	})
	require.NoError(t, err)

	bucket := newTestBucket(t, mc)
	q := newTestQueue(t)

	// 11 MiB file — above ThresholdMB=6; PartSizeMB=5 → parts: 5+5+1 MiB.
	const fileSize = 11 * 1024 * 1024
	filePath := writeTempFile(t, fileSize)
	expectedSHA := sha256File(t, filePath)

	logger, _ := zap.NewDevelopment()
	u, err := uploader.New(uploader.Config{
		Endpoint:    itEndpoint,
		AccessKey:   itUser,
		SecretKey:   itPassword,
		UseSSL:      false,
		ThresholdMB: 6,
		PartSizeMB:  5,
	}, q, logger)
	require.NoError(t, err)

	task := &queue.UploadTask{
		ID:          "it-multi-1",
		RuleID:      "rule-1",
		LocalPath:   filePath,
		StoragePath: "uploads/multipart-test.bin",
		Bucket:      bucket,
	}

	result, err := u.UploadFile(ctx, task)
	require.NoError(t, err)

	assert.Equal(t, expectedSHA, result.SHA256)
	assert.Equal(t, int64(fileSize), result.SizeBytes)
	assert.Equal(t, bucket, result.Bucket)
	assert.NotEmpty(t, result.ETag)

	info, err := mc.StatObject(ctx, bucket, "uploads/multipart-test.bin", miniogo.StatObjectOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(fileSize), info.Size)
}

// TestUploader_Resume_Integration verifies that UploadFile resumes a
// multipart upload where part 1 has already been uploaded.  It pre-uploads
// part 1 via minio.Core directly, then calls UploadFile with the existing
// UploadID; verifyRemoteParts fetches the real state from MinIO and only
// part 2 is uploaded by the Uploader.
func TestUploader_Resume_Integration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	mc, err := miniogo.New(itEndpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(itUser, itPassword, ""),
		Secure: false,
	})
	require.NoError(t, err)

	bucket := newTestBucket(t, mc)
	q := newTestQueue(t)

	// 10 MiB file → exactly 2 parts of 5 MiB each.
	const partSize = 5 * 1024 * 1024
	filePath := writeTempFile(t, 2*partSize)
	expectedSHA := sha256File(t, filePath)

	// Use minio.Core to initiate the multipart upload and pre-upload part 1.
	core, err := miniogo.NewCore(itEndpoint, &miniogo.Options{
		Creds:  credentials.NewStaticV4(itUser, itPassword, ""),
		Secure: false,
	})
	require.NoError(t, err)

	const objectKey = "uploads/resume-test.bin"
	uploadID, err := core.NewMultipartUpload(ctx, bucket, objectKey, miniogo.PutObjectOptions{})
	require.NoError(t, err, "initiate multipart upload")

	f, err := os.Open(filePath)
	require.NoError(t, err)
	defer f.Close()

	_, err = core.PutObjectPart(ctx, bucket, objectKey, uploadID, 1,
		io.LimitReader(f, partSize), partSize, miniogo.PutObjectPartOptions{})
	require.NoError(t, err, "pre-upload part 1")

	// Create the upload task with the existing UploadID. CompletedParts is
	// intentionally empty — verifyRemoteParts will fetch the real list from MinIO.
	task := &queue.UploadTask{
		ID:          "it-resume-1",
		RuleID:      "rule-1",
		LocalPath:   filePath,
		StoragePath: objectKey,
		Bucket:      bucket,
		UploadID:    uploadID,
	}

	logger, _ := zap.NewDevelopment()
	u, err := uploader.New(uploader.Config{
		Endpoint:    itEndpoint,
		AccessKey:   itUser,
		SecretKey:   itPassword,
		UseSSL:      false,
		ThresholdMB: 6,
		PartSizeMB:  5,
	}, q, logger)
	require.NoError(t, err)

	result, err := u.UploadFile(ctx, task)
	require.NoError(t, err)

	assert.Equal(t, expectedSHA, result.SHA256)
	assert.Equal(t, int64(2*partSize), result.SizeBytes)
	assert.NotEmpty(t, result.ETag)

	info, err := mc.StatObject(ctx, bucket, objectKey, miniogo.StatObjectOptions{})
	require.NoError(t, err)
	assert.Equal(t, int64(2*partSize), info.Size)
}
