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

	// The file-version snapshot every persist binds the recorded parts to
	// (IC-3 R-A): captured once per attempt, written in the same UPDATE as
	// the upload ID and the parts. A resume whose current file no longer
	// matches this version must discard the parts — resuming them would
	// splice old and new content into one object with a wrong SHA.
	var partsVersion queue.FileVersion
	if info, err := os.Stat(task.LocalPath); err == nil {
		partsVersion = queue.FileVersion{Mtime: info.ModTime().Unix(), Size: info.Size()}
	} else {
		u.logger.Error("uploader: stat file for multipart version snapshot",
			zap.String("task_id", task.ID), zap.Error(err))
	}

	uploadID := task.UploadID
	var doneParts []minio.CompletePart

	// Attempt to resume a prior upload.
	if uploadID != "" {
		doneParts = u.loadCompletedParts(task.CompletedParts)
		// R-A: the recorded parts belong to ONE file version — the snapshot
		// persisted alongside them. If the current file no longer matches
		// (mtime+size), or no snapshot exists (legacy rows written before the
		// snapshot existed), the parts are untrustworthy: resuming them would
		// skip old-version parts and splice NEW content underneath — a
		// mixed-version object with a wrong SHA (the IC-5 reset deliberately
		// refreshes file metadata while keeping the upload state, so version
		// binding is what makes "resumable" and "unchanged" the same
		// question). Discard the resume state, durably record an abort intent
		// for the stale upload, and start fresh.
		snap := queue.FileVersion{Mtime: task.PartsFileMtime, Size: task.PartsFileSize}
		if snap != partsVersion {
			u.logger.Warn("uploader: file changed since the recorded parts were uploaded, discarding resume state",
				zap.String("task_id", task.ID),
				zap.String("stale_upload_id", uploadID),
				zap.Int64("parts_mtime", snap.Mtime),
				zap.Int64("parts_size", snap.Size),
				zap.Int64("current_mtime", partsVersion.Mtime),
				zap.Int64("parts_file_size", partsVersion.Size))
			doneParts = nil
			u.discardStaleUpload(ctx, task, uploadID)
			uploadID = ""
		} else if err := u.verifyRemoteParts(ctx, task, uploadID, &doneParts); err != nil {
			if !isNoSuchUpload(err) {
				// P1-a: any OTHER error (timeout, network jitter, 5xx) is
				// transient — it says nothing about whether the recorded
				// upload still exists. Starting a fresh multipart here and
				// overwriting the SQLite upload ID would orphan the old
				// upload (never aborted, no reference left) — one orphan per
				// network blip. Fail the attempt instead: the executor's
				// backoff retries, and the next attempt reconciles again.
				return nil, fmt.Errorf("uploader: verify remote parts of upload %q: %w", uploadID, err)
			}
			// NoSuchUpload is the only error that CONFIRMS the recorded
			// upload is gone (completed, aborted or expired) — only then is
			// dropping the recorded state and initiating fresh safe. The
			// fresh initiation below persists the new upload ID over the
			// stale one.
			u.logger.Info("uploader: recorded multipart upload no longer exists, starting fresh",
				zap.String("task_id", task.ID),
				zap.String("stale_upload_id", uploadID),
				zap.Error(err))
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
		//
		// Policy (P1-c): the FIRST persist is deliberately fatal on failure,
		// unlike the per-part writes below. Until this write succeeds the
		// upload ID exists NOWHERE else — a crash right now would leave an
		// untracked orphan (exactly the leak IC-BUG-5 closes), and a row that
		// vanished (ErrTaskNotFound, evicted) means the task will never
		// resume. So on failure the fresh upload is aborted here and the
		// attempt fails; a retry re-initiates cleanly.
		//
		// After this persist succeeds the ID is durable: a per-part progress
		// failure only loses ETag bookkeeping for that one part, and the next
		// attempt reconciles the authoritative remote state via
		// ListObjectParts (verifyRemoteParts). Aborting there would needlessly
		// discard already-uploaded parts — hence the asymmetry.
		if u.queue != nil {
			if err := u.queue.SaveMultipartProgress(ctx, task.ID, task.UploadID, "", partsVersion); err != nil {
				u.logger.Error("uploader: persist multipart initiation failed, aborting the fresh upload",
					zap.String("task_id", task.ID),
					zap.String("upload_id", uploadID), zap.Error(err))
				// The upload was created seconds ago; a straight abort is the
				// right call (NoSuchUpload tolerance is unnecessary here, and
				// clearing task.UploadID first would no-op AbandonUpload).
				if abortErr := u.store.AbortMultipartUpload(ctx, task.Bucket, task.StoragePath, uploadID); abortErr != nil {
					u.logger.Error("uploader: abort of the fresh upload after persist failure also failed",
						zap.String("task_id", task.ID),
						zap.String("upload_id", uploadID), zap.Error(abortErr))
				}
				task.UploadID = ""
				return nil, fmt.Errorf("uploader: persist multipart initiation of upload %q: %w", uploadID, err)
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
			//
			// Deliberately best-effort (P1-c policy asymmetry, see the first
			// persist above): the upload ID is already durable after the
			// initiation persist, so a failure here only loses this part's
			// ETag bookkeeping — the next attempt reconciles the authoritative
			// remote state via ListObjectParts and re-uploads only what MinIO
			// does not have. Known characteristic (documented, deliberate, not
			// scheduled for a fix): the growing completed_parts JSON blob is
			// rewritten IN FULL on every part — O(n²) write amplification on
			// the single SQLite connection, significant only for very large
			// part counts.
			if err := u.queue.SaveMultipartProgress(ctx, task.ID, task.UploadID, task.CompletedParts, partsVersion); err != nil && !errors.Is(err, queue.ErrTaskNotFound) {
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

// discardStaleUpload drops a recorded multipart upload whose parts belong to a
// file version that no longer exists (or that was never versioned). It follows
// the R2 order law: the durable abort intent is recorded FIRST — once the
// resume state is discarded the record is the only retryable identity left,
// and there is no ILM backstop on current MinIO builds (IC-3 ③) — then the
// direct abort runs, and a success removes the record again. A failed abort
// leaves the record for the executor's abort worker to retry with backoff.
func (u *Uploader) discardStaleUpload(ctx context.Context, task *queue.UploadTask, uploadID string) {
	if u.queue != nil {
		if err := u.queue.EnqueueMultipartAbort(ctx, &queue.AbortOutboxEntry{
			UploadID:    uploadID,
			TaskID:      task.ID,
			Bucket:      task.Bucket,
			StoragePath: task.StoragePath,
		}); err != nil {
			u.logger.Error("uploader: record durable abort intent for stale multipart upload",
				zap.String("task_id", task.ID),
				zap.String("upload_id", uploadID),
				zap.Error(err))
		}
	}
	if err := u.store.AbortMultipartUpload(ctx, task.Bucket, task.StoragePath, uploadID); err != nil && !isNoSuchUpload(err) {
		u.logger.Warn("uploader: abort of stale multipart upload failed; the durable abort record will be retried",
			zap.String("task_id", task.ID),
			zap.String("upload_id", uploadID),
			zap.Error(err))
		return
	}
	if u.queue != nil {
		if err := u.queue.DeleteMultipartAbort(ctx, uploadID); err != nil {
			u.logger.Warn("uploader: remove durable abort record after successful abort",
				zap.String("task_id", task.ID),
				zap.String("upload_id", uploadID),
				zap.Error(err))
		}
	}
	u.logger.Info("uploader: aborted stale multipart upload",
		zap.String("task_id", task.ID),
		zap.String("upload_id", uploadID))
}
