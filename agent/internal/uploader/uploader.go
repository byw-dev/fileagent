// Package uploader uploads files to MinIO using either single-part or
// multipart uploads depending on file size. It integrates with the SQLite
// queue to support resume after agent restart.
package uploader

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/byw-dev/fileagent/agent/internal/fileid"
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

	// Identity is the kernel content identity of the version this upload
	// READ (IC-BUG-48): stat'ed from the same fd before the bytes were read
	// (post-read Fstat verified it did not change mid-upload). The executor
	// projects it into the processed-files ledger on ack, so the ledger
	// describes exactly the version whose hash it stores.
	Identity queue.FileIdentity
	// IdentityComplete reports whether the post-read Fstat matched the
	// pre-read snapshot. False means the file changed mid-upload: the hash
	// still describes the delivered bytes, but the identity must NOT be
	// trusted into the ledger (the executor stores a zero identity then —
	// the next candidate check hash-confirms instead of skipping).
	IdentityComplete bool
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

// errStaleRemoteParts signals that parts already uploaded to the remote (which
// this attempt would skip) do not match the local bytes of their ranges: the
// recorded multipart upload can never be completed into an object whose bytes
// the local side can describe. UploadFile reacts by discarding the recorded
// upload (durable abort intent + abort, same path as the version gate) and
// uploading the current content from scratch — re-transmitting is always safe,
// splicing mismatched parts never is.
var errStaleRemoteParts = errors.New("uploader: remote parts do not match the local file bytes")

// UploadFile uploads the file described by task to MinIO. It chooses between
// single-part and multipart strategies based on file size, and honours any
// partially-uploaded state stored in the queue.
//
// IC-BUG-51 invariant: on success, result.SHA256 and result.SizeBytes
// describe, by construction, the bytes that were actually delivered to
// PutObject / PutObjectPart on THIS attempt. There is no separate pre-read of
// the file for hashing: the digest is computed over the very byte stream that
// is handed to the object store (single part: one TeeReader over the upload
// stream; multipart: one full-file sha256 fed in part-number order, where
// skipped — already remote — parts are re-read locally and verified against
// their remote ETag first). SizeBytes is the byte count of that same stream,
// never the stat size. Any disagreement between the count and the size
// declared to the store fails the upload loudly; it is never reported as
// success.
//
// ⚠️ IC-BUG-46: tasks with AppendMode "tail" are refused by the executor
// BEFORE UploadFile is reached. UploadFile additionally refuses tail tasks
// with a positive offset itself (see below) so the invariant cannot be
// violated at the code level either: an incremental suffix upload can never be
// described by a full-file hash. The correct implementation (rolling chunks +
// server-side merge) is IC-15.
func (u *Uploader) UploadFile(ctx context.Context, task *queue.UploadTask) (*UploadResult, error) {
	// IC-BUG-48: the content-identity snapshot must come from the SAME fd the
	// bytes are read from, BEFORE the read — a pre-open os.Stat can describe a
	// file that was replaced between the stat and the open. Open first, stat
	// the handle, upload, then re-stat the same handle and verify nothing
	// changed mid-upload.
	f, err := os.Open(task.LocalPath)
	if err != nil {
		return nil, fmt.Errorf("uploader: open %q: %w", task.LocalPath, err)
	}
	defer f.Close()
	handleID, err := fileid.StatHandle(f)
	if err != nil {
		return nil, fmt.Errorf("uploader: stat handle %q: %w", task.LocalPath, err)
	}
	info, err := f.Stat()
	if err != nil {
		return nil, fmt.Errorf("uploader: stat %q: %w", task.LocalPath, err)
	}
	fileSize := info.Size()

	// Tail mode with a positive offset uploads only the bytes after the
	// offset, so a full-content digest could never describe the delivered
	// bytes (IC-BUG-51 invariant) — the old code hashed the whole file and
	// reported that hash alongside a suffix upload. Refuse instead.
	//
	// Which gate is THE authority: the EXECUTOR's fail-closed gate
	// (IC-BUG-46, executor.go's tail check wrapping ErrTerminalUpload) is
	// the one that matters — it rejects every tail task BEFORE the uploader
	// runs, with a TERMINAL error so the task is not retried forever. This
	// uploader-side check is deliberately only a SECOND line of defence for
	// any future caller that bypasses the executor; its error is a plain
	// error, not executor.ErrTerminalUpload, because the executor imports
	// the uploader package and wrapping the executor's sentinel here would
	// create an import cycle. If the executor gate is ever removed, this
	// error must be upgraded to a terminal semantics the caller recognises
	// (and the offset-0 case re-examined) in the same change. Tail mode with
	// offset 0 is a plain full upload and remains supported.
	if task.AppendMode == "tail" && task.FileOffset > 0 {
		return nil, fmt.Errorf("uploader: append_mode=tail with offset %d > 0 is unsupported (IC-BUG-46): an incremental tail upload would both replace the whole object with only the appended bytes and be un-describable by a full-content digest; the correct implementation is IC-15", task.FileOffset)
	}
	offset := int64(0)
	uploadSize := fileSize - offset

	u.logger.Info("uploader: uploading file",
		zap.String("path", task.LocalPath),
		zap.Int64("size", uploadSize),
		zap.Int64("offset", offset),
		zap.String("bucket", task.Bucket),
	)

	var result *UploadResult
	threshold := int64(u.cfg.ThresholdMB) * 1024 * 1024
	// The file version this attempt would read: the full IC-BUG-48 identity
	// from the handle stat (top of the function — we would have returned
	// otherwise), not a separate os.Stat.
	current := queue.FileVersion{
		Mtime:   info.ModTime().Unix(),
		Size:    info.Size(),
		MtimeNs: handleID.MtimeNs,
		CtimeNs: handleID.CtimeNs,
		Dev:     handleID.Dev,
		Ino:     handleID.Ino,
	}
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
		result, err = u.multipartUpload(ctx, f, task, current, fileSize)
		// The ETag verification of skipped parts can fail even when every
		// stat-based gate passed (same size, same mtime — e.g. an in-place
		// rewrite that restored mtime). The recorded parts are then unusable:
		// discard the upload exactly like the version gate does and upload
		// the current content from scratch. Re-transmitting is the safe
		// direction; silently splicing mismatched parts under a fresh hash
		// is the IC-BUG-51 failure mode and must never happen.
		if errors.Is(err, errStaleRemoteParts) {
			u.logger.Warn("uploader: recorded parts do not match the local bytes (ETag mismatch), discarding the upload and starting over",
				zap.String("task_id", task.ID),
				zap.String("upload_id", task.UploadID))
			if derr := u.discardStaleUpload(ctx, task, task.UploadID); derr != nil {
				return nil, fmt.Errorf("uploader: discard stale multipart upload %q: %w", task.UploadID, derr)
			}
			task.UploadID = ""
			task.CompletedParts = ""
			result, err = u.multipartUpload(ctx, f, task, current, fileSize)
		}
	} else {
		result, err = u.singlePartUpload(ctx, f, task, offset, uploadSize)
	}
	if err != nil {
		return nil, err
	}
	// result.SHA256 / result.SizeBytes are already filled by the upload path
	// from the single byte stream it delivered (IC-BUG-51). Deliberately NOT
	// overwritten here from the stat snapshot.
	//
	// IC-BUG-48 post-read verification: re-stat the SAME handle and compare
	// against the pre-read snapshot. A mismatch means the file changed
	// mid-upload — the hash still describes the delivered bytes (the report
	// stays truthful), but the identity is NOT recorded into the ledger:
	// a zero identity makes the next candidate check hash-confirm instead of
	// skip (fail-closed), so a mid-upload rewrite can never be blessed as
	// "already collected".
	if post, statErr := fileid.StatHandle(f); statErr == nil {
		if fileid.PostStatMatches(handleID, post) {
			result.Identity = queue.FileIdentity{
				MtimeNs:  handleID.MtimeNs,
				CtimeNs:  handleID.CtimeNs,
				Dev:      handleID.Dev,
				Ino:      handleID.Ino,
				Size:     handleID.Size,
				SnapAtNs: handleID.SnapAtNs,
			}
			result.IdentityComplete = true
		} else {
			u.logger.Warn("uploader: file changed during upload, recording an incomplete identity (next check hash-confirms)",
				zap.String("path", task.LocalPath),
				zap.Int64("pre_mtime_ns", handleID.MtimeNs),
				zap.Int64("post_mtime_ns", post.MtimeNs),
				zap.Uint64("pre_ino", handleID.Ino),
				zap.Uint64("post_ino", post.Ino))
		}
	} else {
		u.logger.Warn("uploader: post-upload stat failed, recording an incomplete identity",
			zap.String("path", task.LocalPath), zap.Error(statErr))
	}
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
// bytes to upload.
//
// IC-BUG-51: the SHA-256 digest, the MD5 (for the opportunistic ETag
// cross-check) and the delivered-byte count are all computed over the SAME
// reader that is handed to PutObject — there is no second read of the file.
//
// The reader is deliberately NOT *os.File: wrapped in a TeeReader it is a
// plain io.Reader, so minio-go does not pin the stream into an
// io.SectionReader, and the request goes out with streaming signature +
// Content-Length = size. The LimitReader matters here (review-r1 fix 2,
// live-probed with minio-go v7.0.91, see bin/b7-ic-bug-51/review-r1/
// minio_read_probe.go): WITHOUT it, a file that GROWS mid-upload makes the
// streaming signer read past the declared length and the SDK fails LOUDLY
// ("ContentLength != Body length"-style errors — verified: declared 100000,
// reader offering 101024 → request fails). WITH it, exactly the first
// `size` bytes are delivered and the upload succeeds with the reported
// hash/size describing precisely that prefix. A file that SHRINKS mid-upload
// still fails the count check below loudly. Either way the reported metadata
// can only ever describe the bytes that were actually sent.
func (u *Uploader) singlePartUpload(ctx context.Context, f *os.File, task *queue.UploadTask, offset, size int64) (*UploadResult, error) {
	// Rework-2 blocker 1: f is the ONE handle UploadFile opened and stat'ed —
	// re-opening by path here would let the path swap to another file between
	// the identity stat and the read (identity of A, bytes of B).

	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("uploader: seek %q to %d: %w", task.LocalPath, offset, err)
		}
	}

	sha := sha256.New()
	md5h := md5.New()
	// io.LimitReader pins the delivered bytes to the stat size declared in
	// the request: growth beyond it is never read, truncation below it fails
	// the count check below (see the function comment).
	body := &countingTeeReader{r: io.LimitReader(f, size), w: io.MultiWriter(sha, md5h)}

	info, err := u.store.PutObject(ctx, task.Bucket, task.StoragePath, body, size, minio.PutObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("uploader: put object: %w", err)
	}
	if body.n != size {
		return nil, fmt.Errorf("uploader: delivered %d bytes but declared %d (file changed during upload): %w",
			body.n, size, errByteCountMismatch)
	}
	// Opportunistic cross-check: on the supported backends (live-verified for
	// MinIO and SeaweedFS, see bin/b7-ic-bug-51/poc.md) the PutObject ETag is
	// the MD5 of the delivered bytes. If the ETag is a comparable MD5 and it
	// disagrees with what we just hashed, the store received something other
	// than what we believe we sent — fail loudly rather than report success.
	// A non-comparable ETag (e.g. SSE-encrypted objects) skips this check;
	// the invariant above does not depend on it.
	if want := hex.EncodeToString(md5h.Sum(nil)); etagIsComparable(info.ETag) && normalizeETag(info.ETag) != want {
		return nil, fmt.Errorf("uploader: put object ETag %q does not match the delivered bytes (md5 %s)", info.ETag, want)
	}

	return &UploadResult{
		StoragePath: task.StoragePath,
		Bucket:      task.Bucket,
		ETag:        info.ETag,
		SHA256:      hex.EncodeToString(sha.Sum(nil)),
		SizeBytes:   body.n,
	}, nil
}

// errByteCountMismatch reports that the number of bytes the store consumed
// from the upload stream differs from the size declared to it. The upload is
// never reported as success in that case.
var errByteCountMismatch = errors.New("byte count mismatch")

// countingTeeReader is an io.TeeReader that also counts the bytes read. The
// count is the authoritative "bytes actually moved" number for IC-BUG-51:
// SizeBytes is taken from it, not from a stat snapshot.
type countingTeeReader struct {
	r io.Reader
	w io.Writer
	n int64
}

func (c *countingTeeReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		if _, werr := c.w.Write(p[:n]); werr != nil {
			return n, werr
		}
		c.n += int64(n)
	}
	return n, err
}

// normalizeETag canonicalises an S3 ETag for comparison with a locally
// computed digest: surrounding quotes are stripped and hex letters folded to
// lowercase (both appear in the wild; the PoC probe saw quoted ETags from the
// AWS CLI view, unquoted from minio-go).
func normalizeETag(etag string) string {
	return strings.ToLower(strings.Trim(etag, `"`))
}

// etagIsComparable reports whether etag is a plain 32-hex-char MD5 digest —
// the only form that can be compared against a locally computed MD5. Anything
// else (an SSE-encrypted object's opaque ETag, a multipart composite
// "md5-N" tag, a gateway-specific value) is not comparable, and callers treat
// that as "cannot verify" rather than "verified".
func etagIsComparable(etag string) bool {
	e := normalizeETag(etag)
	if len(e) != 32 {
		return false
	}
	for _, c := range e {
		if !strings.ContainsRune("0123456789abcdef", c) {
			return false
		}
	}
	return true
}
