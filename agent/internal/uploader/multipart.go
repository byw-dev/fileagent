package uploader

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/byw-dev/fileagent/agent/internal/queue"
	"github.com/minio/minio-go/v7"
	"go.uber.org/zap"
)

// completedParts is persisted as JSON in queue.UploadTask.CompletedParts.
type completedParts struct {
	Parts []minio.CompletePart `json:"parts"`
}

// multipartUpload uploads a large file using MinIO multipart upload with
// support for resuming interrupted transfers via the SQLite queue.
func (u *Uploader) multipartUpload(ctx context.Context, task *queue.UploadTask, size int64) (*UploadResult, error) {
	partSize := int64(u.cfg.PartSizeMB) * 1024 * 1024

	uploadID := task.UploadID
	var doneParts []minio.CompletePart

	// Attempt to resume a prior upload.
	if uploadID != "" {
		doneParts = u.loadCompletedParts(task.CompletedParts)
		if err := u.verifyRemoteParts(ctx, task, uploadID, &doneParts); err != nil {
			u.logger.Warn("uploader: cannot verify remote parts, starting fresh",
				zap.String("task_id", task.ID), zap.Error(err))
			uploadID = ""
			doneParts = nil
		} else if skipped := len(doneParts); skipped > 0 {
			// The acceptance evidence for IC-BUG-5 lives in this line: a resume
			// must be visible in the log together with how many parts were
			// skipped, and the upload ID must be the one recorded before the
			// interruption — not a freshly initiated one.
			u.logger.Info("uploader: resuming multipart upload from remote state",
				zap.String("task_id", task.ID),
				zap.String("upload_id", uploadID),
				zap.Int("skipped_parts", skipped))
		}
	}

	if uploadID == "" {
		var err error
		uploadID, err = u.store.NewMultipartUpload(ctx, task.Bucket, task.StoragePath, minio.PutObjectOptions{})
		if err != nil {
			return nil, fmt.Errorf("uploader: initiate multipart upload: %w", err)
		}
		task.UploadID = uploadID
		// Persist the initiated upload ID immediately: a crash between this
		// and the first completed part must still leave a resumable upload ID
		// behind, not an orphan with no local trace.
		if u.queue != nil {
			if err := u.queue.SaveMultipartProgress(ctx, task.ID, task.UploadID, ""); err != nil && !errors.Is(err, queue.ErrTaskNotFound) {
				u.logger.Warn("uploader: persist multipart initiation",
					zap.String("task_id", task.ID), zap.Error(err))
			}
		}
	}

	doneSet := make(map[int]struct{}, len(doneParts))
	for _, p := range doneParts {
		doneSet[p.PartNumber] = struct{}{}
	}

	totalParts := int((size + partSize - 1) / partSize)

	for partNum := 1; partNum <= totalParts; partNum++ {
		if _, already := doneSet[partNum]; already {
			continue
		}

		offset := int64(partNum-1) * partSize
		pSize := partSize
		if remaining := size - offset; remaining < pSize {
			pSize = remaining
		}

		r, err := newSectionReader(task.LocalPath, offset, pSize)
		if err != nil {
			return nil, fmt.Errorf("uploader: open part %d of %q: %w", partNum, task.LocalPath, err)
		}

		part, err := u.store.PutObjectPart(ctx, task.Bucket, task.StoragePath, uploadID,
			partNum, r, pSize, minio.PutObjectPartOptions{})
		r.close()
		if err != nil {
			return nil, fmt.Errorf("uploader: upload part %d: %w", partNum, err)
		}

		doneParts = append(doneParts, minio.CompletePart{
			PartNumber: partNum,
			ETag:       part.ETag,
		})

		// Persist progress to the queue.
		if u.queue != nil {
			raw, _ := json.Marshal(completedParts{Parts: doneParts})
			task.CompletedParts = string(raw)
			// IC-BUG-5: writing task.CompletedParts alone only mutates memory.
			// Every completed part is flushed to SQLite here so a retry or a
			// process restart rescans the real progress instead of empty
			// values and restarts from part 1.
			if err := u.queue.SaveMultipartProgress(ctx, task.ID, task.UploadID, task.CompletedParts); err != nil && !errors.Is(err, queue.ErrTaskNotFound) {
				u.logger.Warn("uploader: persist multipart progress",
					zap.String("task_id", task.ID),
					zap.Int("part_number", partNum), zap.Error(err))
			}
		}
	}

	info, err := u.store.CompleteMultipartUpload(ctx, task.Bucket, task.StoragePath, uploadID, doneParts, minio.PutObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("uploader: complete multipart upload: %w", err)
	}

	return &UploadResult{
		StoragePath: task.StoragePath,
		Bucket:      task.Bucket,
		ETag:        info.ETag,
	}, nil
}

// loadCompletedParts deserialises the parts JSON blob from the queue task.
func (u *Uploader) loadCompletedParts(raw string) []minio.CompletePart {
	if raw == "" {
		return nil
	}
	var cp completedParts
	if err := json.Unmarshal([]byte(raw), &cp); err != nil {
		return nil
	}
	return cp.Parts
}

// verifyRemoteParts fetches the uploaded-parts list from MinIO and reconciles
// it with the locally-cached list.
func (u *Uploader) verifyRemoteParts(ctx context.Context, task *queue.UploadTask, uploadID string, doneParts *[]minio.CompletePart) error {
	result, err := u.store.ListObjectParts(ctx, task.Bucket, task.StoragePath, uploadID, 0, 10000)
	if err != nil {
		return err
	}
	remote := make([]minio.CompletePart, 0, len(result.ObjectParts))
	for _, p := range result.ObjectParts {
		remote = append(remote, minio.CompletePart{PartNumber: p.PartNumber, ETag: p.ETag})
	}
	*doneParts = remote
	return nil
}

// sectionReader reads a fixed-length section of a file starting at offset.
type sectionReader struct {
	f      *os.File
	reader io.Reader
}

// newSectionReader opens path and positions a LimitedReader at [offset, offset+size).
func newSectionReader(path string, offset, size int64) (*sectionReader, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return &sectionReader{f: f, reader: io.LimitReader(f, size)}, nil
}

func (s *sectionReader) Read(p []byte) (int, error) { return s.reader.Read(p) }
func (s *sectionReader) close()                     { _ = s.f.Close() }
