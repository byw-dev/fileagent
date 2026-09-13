// Package uploader uploads files to MinIO using either single-part or
// multipart uploads depending on file size. It integrates with the SQLite
// queue to support resume after agent restart.
package uploader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/minio/minio-go/v7"
	"github.com/minio/minio-go/v7/pkg/credentials"
	"go.uber.org/zap"
)

const (
	// defaultSinglePartThresholdMB is the default maximum file size (in MiB)
	// for single-part uploads. Files larger than this use multipart upload.
	defaultSinglePartThresholdMB = 64
)

// Config holds the configuration for the MinIO client.
type Config struct {
	// Endpoint is the MinIO server address (host:port).
	Endpoint string
	// AccessKey is the access key ID.
	AccessKey string
	// SecretKey is the secret access key.
	SecretKey string
	// SessionToken is an optional STS session token.
	SessionToken string
	// UseSSL enables TLS when connecting to MinIO.
	UseSSL bool
	// PartSizeMB is the multipart upload part size in mebibytes (default 64).
	PartSizeMB int
	// ThresholdMB is the file size threshold in mebibytes below which single-part
	// upload is used (default 64). Set lower in tests to avoid large file I/O.
	ThresholdMB int
	// Concurrency is the number of concurrent part uploads (handled by executor).
	Concurrency int
}

// UploadResult carries metadata about a completed upload.
type UploadResult struct {
	// StoragePath is the object key in the bucket.
	StoragePath string
	// Bucket is the target bucket name.
	Bucket string
	// SHA256 is the hex-encoded SHA-256 digest of the uploaded file.
	SHA256 string
	// ETag is the ETag returned by MinIO after the upload.
	ETag string
	// SizeBytes is the file size in bytes.
	SizeBytes int64
}

// ObjectStore abstracts the MinIO client to allow unit-testing without a real
// MinIO server.
type ObjectStore interface {
	// PutObject uploads a complete object from r and returns the upload info.
	PutObject(ctx context.Context, bucket, object string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	// NewMultipartUpload starts a new multipart upload and returns the upload ID.
	NewMultipartUpload(ctx context.Context, bucket, object string, opts minio.PutObjectOptions) (string, error)
	// PutObjectPart uploads a single part of a multipart upload.
	PutObjectPart(ctx context.Context, bucket, object, uploadID string, partNumber int, r io.Reader, size int64, opts minio.PutObjectPartOptions) (minio.ObjectPart, error)
	// ListObjectParts lists the parts already uploaded for a multipart upload.
	ListObjectParts(ctx context.Context, bucket, object, uploadID string, partNumber, maxParts int) (minio.ListObjectPartsResult, error)
	// CompleteMultipartUpload finalises a multipart upload.
	CompleteMultipartUpload(ctx context.Context, bucket, object, uploadID string, parts []minio.CompletePart, opts minio.PutObjectOptions) (minio.UploadInfo, error)
	// AbortMultipartUpload discards an unfinished multipart upload and all its
	// uploaded parts.
	AbortMultipartUpload(ctx context.Context, bucket, object, uploadID string) error
}

// minioAdapter wraps *minio.Core to satisfy the ObjectStore interface.
// minio.Core embeds *minio.Client and exposes the low-level multipart API.
type minioAdapter struct {
	c *minio.Core
}

func (a *minioAdapter) PutObject(ctx context.Context, bucket, object string, r io.Reader, size int64, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	return a.c.Client.PutObject(ctx, bucket, object, r, size, opts)
}

func (a *minioAdapter) NewMultipartUpload(ctx context.Context, bucket, object string, opts minio.PutObjectOptions) (string, error) {
	return a.c.NewMultipartUpload(ctx, bucket, object, opts)
}

func (a *minioAdapter) PutObjectPart(ctx context.Context, bucket, object, uploadID string, partNumber int, r io.Reader, size int64, opts minio.PutObjectPartOptions) (minio.ObjectPart, error) {
	return a.c.PutObjectPart(ctx, bucket, object, uploadID, partNumber, r, size, opts)
}

func (a *minioAdapter) ListObjectParts(ctx context.Context, bucket, object, uploadID string, partNumber, maxParts int) (minio.ListObjectPartsResult, error) {
	return a.c.ListObjectParts(ctx, bucket, object, uploadID, partNumber, maxParts)
}

func (a *minioAdapter) CompleteMultipartUpload(ctx context.Context, bucket, object, uploadID string, parts []minio.CompletePart, opts minio.PutObjectOptions) (minio.UploadInfo, error) {
	return a.c.CompleteMultipartUpload(ctx, bucket, object, uploadID, parts, opts)
}

func (a *minioAdapter) AbortMultipartUpload(ctx context.Context, bucket, object, uploadID string) error {
	return a.c.AbortMultipartUpload(ctx, bucket, object, uploadID)
}

// Uploader uploads files to MinIO and integrates with the local queue.
type Uploader struct {
	store  ObjectStore
	queue  *queue.Queue
	cfg    Config
	logger *zap.Logger
}

// New creates an Uploader backed by a real MinIO client (via minio.Core).
func New(cfg Config, q *queue.Queue, logger *zap.Logger) (*Uploader, error) {
	normalise(&cfg)
	mc, err := minio.NewCore(cfg.Endpoint, &minio.Options{
		Creds:  credentials.NewStaticV4(cfg.AccessKey, cfg.SecretKey, cfg.SessionToken),
		Secure: cfg.UseSSL,
	})
	if err != nil {
		return nil, fmt.Errorf("uploader: create minio client: %w", err)
	}
	return &Uploader{
		store:  &minioAdapter{c: mc},
		queue:  q,
		cfg:    cfg,
		logger: logger,
	}, nil
}

// newWithStore creates an Uploader using a provided ObjectStore (for testing).
func newWithStore(store ObjectStore, cfg Config, q *queue.Queue, logger *zap.Logger) *Uploader {
	normalise(&cfg)
	return &Uploader{
		store:  store,
		queue:  q,
		cfg:    cfg,
		logger: logger,
	}
}

// normalise applies defaults to cfg fields that were left at zero.
func normalise(cfg *Config) {
	if cfg.PartSizeMB <= 0 {
		cfg.PartSizeMB = defaultSinglePartThresholdMB
	}
	if cfg.ThresholdMB <= 0 {
		cfg.ThresholdMB = defaultSinglePartThresholdMB
	}
}

// UploadFile uploads the file described by task to MinIO. It chooses between
// single-part and multipart strategies based on file size, and honours any
// partially-uploaded state stored in the queue.
//
// When task.AppendMode is "tail" and task.FileOffset > 0, only the bytes
// starting from FileOffset are uploaded (i.e., the tail appended since the
// last upload). The object key is the same (task.StoragePath), so the
// caller must ensure unique keys per chunk if full history is required.
func (u *Uploader) UploadFile(ctx context.Context, task *queue.UploadTask) (*UploadResult, error) {
	info, err := os.Stat(task.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("uploader: stat %q: %w", task.LocalPath, err)
	}
	fileSize := info.Size()

	// For tail mode, only upload the new bytes since the last upload.
	offset := task.FileOffset
	if task.AppendMode == "tail" && offset > 0 {
		if offset >= fileSize {
			// No new bytes — nothing to upload.
			return &UploadResult{
				StoragePath: task.StoragePath,
				Bucket:      task.Bucket,
				SizeBytes:   0,
			}, nil
		}
	} else {
		offset = 0
	}
	uploadSize := fileSize - offset

	sha, err := fileSHA256(task.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("uploader: sha256 %q: %w", task.LocalPath, err)
	}

	u.logger.Info("uploader: uploading file",
		zap.String("path", task.LocalPath),
		zap.Int64("size", uploadSize),
		zap.Int64("offset", offset),
		zap.String("bucket", task.Bucket),
	)

	var result *UploadResult
	threshold := int64(u.cfg.ThresholdMB) * 1024 * 1024
	// The file version this attempt would read (the top stat succeeded, or we
	// would have returned above — "stat failed" can never reach the gate).
	current := queue.FileVersion{Mtime: info.ModTime().Unix(), Size: info.Size()}
	isMultipart := uploadSize > threshold

	// Fail-closed resume gate (IC-3 R-A/A1/A2): the recorded parts may be
	// skipped ONLY when canResumeParts holds for EVERY condition. Anything
	// else — no snapshot, failed stat, changed version, shrunken single-part
	// file — means the recorded multipart upload is discarded first (abort
	// intent + abort) or the attempt fails (discard refused, A3), and the
	// current content is uploaded from scratch. This gate lives at the ONE
	// place every upload path flows through, so no path can bypass it.
	if task.UploadID != "" {
		if ok, reason := canResumeParts(task, current, true, isMultipart); !ok {
			u.logger.Warn("uploader: recorded multipart upload cannot be resumed, discarding it",
				zap.String("task_id", task.ID),
				zap.String("upload_id", task.UploadID),
				zap.String("reason", reason))
			if derr := u.discardStaleUpload(ctx, task, task.UploadID); derr != nil {
				return nil, fmt.Errorf("uploader: discard stale multipart upload %q: %w", task.UploadID, derr)
			}
			task.UploadID = ""
			task.CompletedParts = ""
		}
	}

	if isMultipart {
		result, err = u.multipartUpload(ctx, task, current, fileSize)
	} else {
		result, err = u.singlePartUpload(ctx, task, offset, uploadSize)
	}
	if err != nil {
		return nil, err
	}
	result.SHA256 = sha
	result.SizeBytes = uploadSize
	return result, nil
}

// isNoSuchUpload reports whether err is (or wraps) a MinIO NoSuchUpload
// response: the referenced multipart upload is confirmed gone (already
// completed, aborted or expired), so there is nothing left to clean up.
func isNoSuchUpload(err error) bool {
	var resp minio.ErrorResponse
	return errors.As(err, &resp) && resp.Code == "NoSuchUpload"
}

// AbandonUpload aborts the in-flight multipart upload recorded on task, if
// any. It is called when a task is given up on — retry budget exhausted,
// terminal failure, or queue eviction — because a multipart upload that will
// never be completed or resumed leaks its uploaded parts without bound
// (IC-3 ②). Single-part tasks (and tasks whose upload never initiated) carry
// no upload ID and are a no-op.
//
// Tolerated as success: NoSuchUpload, i.e. the upload was already completed,
// aborted or expired — in every case there is nothing left to clean up. Any
// other error is returned so the caller can log and RETRY it: note that on the
// current MinIO builds the bucket's AbortIncompleteMultipartUpload ILM rule is
// NOT a reliable backstop (IC-3 ③: the action is rejected outright, or silently
// stripped alongside Expiration), so a failed abort must keep a retryable local
// identity — the executor's durable abort record (IC-3 P2) provides it.
func (u *Uploader) AbandonUpload(ctx context.Context, task *queue.UploadTask) error {
	if task == nil || task.UploadID == "" {
		return nil
	}
	err := u.store.AbortMultipartUpload(ctx, task.Bucket, task.StoragePath, task.UploadID)
	if err == nil {
		return nil
	}
	if isNoSuchUpload(err) {
		return nil
	}
	return fmt.Errorf("uploader: abort multipart upload %q of %q: %w",
		task.UploadID, task.StoragePath, err)
}

// singlePartUpload uploads a file (or a portion of it) using PutObject.
// offset is the byte position to start reading from; size is the number of
// bytes to upload. When offset is 0 and size equals the full file size, the
// entire file is uploaded.
func (u *Uploader) singlePartUpload(ctx context.Context, task *queue.UploadTask, offset, size int64) (*UploadResult, error) {
	f, err := os.Open(task.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("uploader: open %q: %w", task.LocalPath, err)
	}
	defer f.Close()

	if offset > 0 {
		if _, err = f.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("uploader: seek %q to %d: %w", task.LocalPath, offset, err)
		}
	}

	info, err := u.store.PutObject(ctx, task.Bucket, task.StoragePath, f, size, minio.PutObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("uploader: put object: %w", err)
	}

	return &UploadResult{
		StoragePath: task.StoragePath,
		Bucket:      task.Bucket,
		ETag:        info.ETag,
	}, nil
}

// fileSHA256 calculates the SHA-256 hash of the file at path.
func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
