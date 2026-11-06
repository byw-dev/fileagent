package uploader

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/hex"
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
// canResumeParts is the fail-closed gate for resuming a recorded multipart
// upload (IC-3 R-A/A1/A2): the recorded parts may be skipped ONLY when ALL of
// the following hold — anything undecidable means "do not trust the parts",
// because resuming stale parts splices old and new file content into one
// object with a wrong SHA:
//
//  1. the row carries a parts version snapshot (parts_file_mtime /
//     parts_file_size): a legacy row without one is untrusted — its parts may
//     belong to ANY version of the file;
//  2. the current file was stat'ed successfully: a failed stat leaves the
//     current version unknown, and comparing against an unknown proves
//     nothing (a zero-value current version must never compare "equal" to a
//     zero-value legacy snapshot);
//  3. the snapshot matches the current version (mtime + size);
//  4. this attempt actually goes the multipart path: the upload would
//     otherwise be a PutObject (a file that shrank below the threshold, or a
//     tail-mode offset shrinking the increment), which can never consume the
//     recorded parts — the old multipart upload would be orphaned with no
//     reference left.
//
// The single gate exists so that "why can't we resume" is directly assertable
// by tests and logs instead of being scattered across branches — scattered
// conditions are exactly how the fail-open holes (A1/A2) slipped in.
// Deliberate blind spot, shared with IC-BUG-10's mtime+size identity: a file
// rewritten within the same second at the same size is undetectable —
// IC-BUG-51 closes that hole for the RESUME path via per-part ETag
// verification (see hashResumePart below), not via stat identity.
func canResumeParts(task *queue.UploadTask, current queue.FileVersion, statOK, isMultipart bool) (bool, string) {
	if task.PartsFileMtime == 0 && task.PartsFileSize == 0 {
		return false, "no version snapshot for the recorded parts (legacy row, untrusted)"
	}
	if !statOK {
		return false, "current file stat failed"
	}
	if current != (queue.FileVersion{Mtime: task.PartsFileMtime, Size: task.PartsFileSize}) {
		return false, "file version changed since the parts were uploaded"
	}
	if !isMultipart {
		return false, "this attempt no longer uses multipart upload"
	}
	return true, ""
}

// multipartUpload uploads a large file using multipart upload.
//
// IC-BUG-51 ORDER LAW — the full-file SHA-256 must be fed in PART-NUMBER
// order, 1..totalParts, with exactly one write per byte of the final object:
//   - a part uploaded in THIS attempt is hashed by TeeReader as it streams to
//     PutObjectPart (the same bytes, the same read);
//   - a part SKIPPED because the remote already has it is re-read from the
//     local file at its range and hashed after its ETag has been verified
//     against the re-read bytes' MD5 (hashResumePart). Those bytes entered the
//     object during the earlier attempt; this attempt re-describes them from
//     the local copy.
//
// Any other feeding order (e.g. hashing skipped parts lazily after later
// parts) would produce a digest of a byte sequence that never existed as an
// object. The digests and the delivered-byte count are the ONLY source for
// UploadResult.SHA256 / SizeBytes — the stat snapshot is never used for them.
func (u *Uploader) multipartUpload(ctx context.Context, task *queue.UploadTask, fv queue.FileVersion, size int64) (*UploadResult, error) {
	partSize := int64(u.cfg.PartSizeMB) * 1024 * 1024

	uploadID := task.UploadID
	var doneParts []minio.CompletePart
	// fullSHA is the ONE full-content digest required by the IC-BUG-51
	// invariant. It is fed strictly in part-number order (see the order law
	// above); SizeBytes is its byte counter, likewise.
	fullSHA := sha256.New()
	var hashedBytes int64

	// The fail-closed resume gate already ran in UploadFile: reaching here
	// with a non-empty upload ID means the recorded parts ARE trustworthy
	// (snapshot present, stat succeeded, version unchanged, multipart path).
	// Reconcile with the remote side (the authority, P1-a) and continue.
	if uploadID != "" {
		doneParts = u.loadCompletedParts(task.CompletedParts)
		if err := u.verifyRemoteParts(ctx, task, uploadID, &doneParts); err != nil {
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
			// IC-BUG-51, rework-1 fix 1: verify EVERY remote part's ETag against
			// the MD5 of its current local bytes UP FRONT — a mismatch (or an
			// unreadable / short read of the range) aborts the resume via
			// errStaleRemoteParts before anything is hashed or uploaded, so a
			// stale upload is discarded as early as possible. The stat-based
			// gates above cannot see an in-place rewrite that restores mtime
			// and size; the ETag can.
			//
			// The digest is NOT fed here: feeding happens in the part-number
			// loop below, at each part's own position (see the order law).
			// Pre-feeding here was the review-r1 blocker: with remote parts
			// {1,3} and part 2 missing, the digest ate 1+3+2 while the object
			// is assembled 1+2+3 — equal total sizes, wrong order, silently
			// wrong SHA. Parts are equal length, so no size check can catch
			// that; only the feeding position can.
			//
			// Cost note: verification re-reads the skipped ranges (pure local
			// I/O, no network) — the standard trade-off of S3 resume uploaders,
			// and far cheaper than re-transmitting. The ranges are read AGAIN
			// in the ordered loop below; that second read is the price of
			// feeding the digest strictly in order, and it is still local I/O.
			for _, p := range doneParts {
				if err := u.verifyResumePartETag(task, size, partSize, p); err != nil {
					return nil, fmt.Errorf("%w: part %d: %v", errStaleRemoteParts, p.PartNumber, err)
				}
			}
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
			if err := u.queue.SaveMultipartProgress(ctx, task.ID, task.UploadID, "", fv); err != nil {
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
	remoteETags := make(map[int]string, len(doneParts))
	for _, p := range doneParts {
		doneSet[p.PartNumber] = struct{}{}
		remoteETags[p.PartNumber] = p.ETag
	}
	totalParts := int((size + partSize - 1) / partSize)
	// The complete list sent to CompleteMultipartUpload is rebuilt in strict
	// ascending part order (review-r1 fix 1): minio-go does NOT sort it, and a
	// hole in the remote part set (e.g. {1,3}) would otherwise marshal the
	// parts in "remote-then-uploaded" order — [1,3,2] — which servers are free
	// to reject and which never matches the digest's feeding order anyway.
	completeParts := make([]minio.CompletePart, 0, totalParts)

	// ORDER LAW (IC-BUG-51): every part — skipped (already remote) or uploaded
	// in this attempt — feeds the full-file digest exactly at its own position
	// in this loop. A skipped part's bytes are re-read from the local range
	// here (ETag verified up front above), an uploaded part's bytes flow
	// through the TeeReader as they stream to the store. Any other feeding
	// order produces a digest of a byte sequence that was never assembled.
	for partNum := 1; partNum <= totalParts; partNum++ {
		if _, already := doneSet[partNum]; already {
			// Skipped part: its bytes are already on the remote (ETag verified
			// up front). Re-read the local range and feed the digest AT THIS
			// POSITION so the digest describes the assembled object.
			if err := u.hashResumePartAt(task, size, partSize, partNum, fullSHA, &hashedBytes); err != nil {
				return nil, fmt.Errorf("%w: part %d: %v", errStaleRemoteParts, partNum, err)
			}
			completeParts = append(completeParts, minio.CompletePart{PartNumber: partNum, ETag: remoteETags[partNum]})
			continue
		}

		offset := int64(partNum-1) * partSize
		pSize := partSize
		if remaining := size - offset; remaining < pSize {
			pSize = remaining
		}

		f, err := os.Open(task.LocalPath)
		if err != nil {
			return nil, fmt.Errorf("uploader: open part %d of %q: %w", partNum, task.LocalPath, err)
		}
		if offset > 0 {
			if _, err = f.Seek(offset, io.SeekStart); err != nil {
				_ = f.Close()
				return nil, fmt.Errorf("uploader: seek %q to %d for part %d: %w", task.LocalPath, offset, partNum, err)
			}
		}
		// IC-BUG-51: the part's bytes stream through the full-file digest on
		// their way to the store — one read feeds both. A short file (the
		// source shrank mid-upload) makes the count check below fail loudly.
		md5h := md5.New()
		body := &countingTeeReader{r: io.LimitReader(f, pSize), w: io.MultiWriter(fullSHA, md5h)}

		part, err := u.store.PutObjectPart(ctx, task.Bucket, task.StoragePath, uploadID,
			partNum, body, pSize, minio.PutObjectPartOptions{})
		_ = f.Close()
		if err != nil {
			return nil, fmt.Errorf("uploader: upload part %d: %w", partNum, err)
		}
		if body.n != pSize {
			return nil, fmt.Errorf("uploader: part %d delivered %d bytes but declared %d (file changed during upload): %w",
				partNum, body.n, pSize, errByteCountMismatch)
		}
		hashedBytes += body.n
		// Cross-check the returned ETag against the MD5 of the bytes this
		// call actually delivered (same rationale as the single-part check;
		// non-comparable ETags are skipped, the byte-count check above does
		// not depend on it).
		if want := hex.EncodeToString(md5h.Sum(nil)); etagIsComparable(part.ETag) && normalizeETag(part.ETag) != want {
			return nil, fmt.Errorf("uploader: part %d ETag %q does not match the delivered bytes (md5 %s)", partNum, part.ETag, want)
		}

		completeParts = append(completeParts, minio.CompletePart{
			PartNumber: partNum,
			ETag:       part.ETag,
		})

		// Persist progress to the queue. completedParts (the persisted JSON)
		// keeps the same ascending order as the complete list.
		if u.queue != nil {
			task.CompletedParts = marshalCompletedParts(completeParts)
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
			if err := u.queue.SaveMultipartProgress(ctx, task.ID, task.UploadID, task.CompletedParts, fv); err != nil && !errors.Is(err, queue.ErrTaskNotFound) {
				u.logger.Warn("uploader: persist multipart progress",
					zap.String("task_id", task.ID),
					zap.Int("part_number", partNum), zap.Error(err))
			}
		}
	}

	// The digest was fed in strict part order over every part of the object;
	// its byte count must therefore equal the size this attempt planned to
	// deliver. Any divergence means the ordering law above was violated —
	// fail loudly rather than report a digest of a sequence that was never
	// assembled.
	if hashedBytes != size {
		return nil, fmt.Errorf("uploader: hashed %d bytes across parts but the object size is %d: %w",
			hashedBytes, size, errByteCountMismatch)
	}

	info, err := u.store.CompleteMultipartUpload(ctx, task.Bucket, task.StoragePath, uploadID, completeParts, minio.PutObjectOptions{})
	if err != nil {
		return nil, fmt.Errorf("uploader: complete multipart upload: %w", err)
	}

	return &UploadResult{
		StoragePath: task.StoragePath,
		Bucket:      task.Bucket,
		ETag:        info.ETag,
		SHA256:      hex.EncodeToString(fullSHA.Sum(nil)),
		SizeBytes:   hashedBytes,
	}, nil
}

// verifyResumePartETag re-reads the local byte range of a skipped
// (already-remote) part and verifies the part's remote ETag against the MD5
// of the re-read bytes — WITHOUT feeding any digest. It runs for every
// remote part up front, before hashing or uploading starts, so a stale
// upload is discarded as early as possible. It returns an error when the
// local content no longer matches what the remote holds — the caller reacts
// by discarding the upload and starting over (IC-BUG-51: never splice
// mismatched parts under a fresh digest).
//
// Failure modes all mean "cannot describe the object from local bytes":
//   - ETag mismatch: the range's content changed since the part was uploaded
//     (an in-place rewrite that restored mtime+size is invisible to every
//     stat gate — the ETag is the only witness);
//   - the remote ETag is not a comparable 32-hex MD5 (e.g. SSE, gateway
//     quirk): treated as a mismatch — the safe direction is re-transmitting,
//     never assuming compatibility;
//   - the local read is short or errors (file truncated between attempts):
//     the bytes the object holds cannot be re-derived, so the resume is off;
//   - the part's range lies outside the planned object layout (remote list
//     disagrees with the local plan): undescribable, treat as stale.
func (u *Uploader) verifyResumePartETag(task *queue.UploadTask, totalSize, partSize int64, part minio.CompletePart) error {
	offset, pSize, err := partRange(part.PartNumber, totalSize, partSize)
	if err != nil {
		return err
	}
	data, err := readLocalRange(task.LocalPath, offset, pSize)
	if err != nil {
		return err
	}
	if !etagIsComparable(part.ETag) {
		return fmt.Errorf("remote ETag %q is not a comparable md5 digest", part.ETag)
	}
	if got := md5HexOf(data); normalizeETag(part.ETag) != got {
		return fmt.Errorf("remote ETag %s != local md5 %s for range [%d,%d)", normalizeETag(part.ETag), got, offset, offset+pSize)
	}
	return nil
}

// hashResumePartAt feeds the local byte range of a skipped (already-remote,
// ETag-verified up front) part into the full-file digest at the part's own
// position in the ordered loop — the ORDER LAW half for skipped parts. The
// ETag is re-checked here too: between the up-front verification and this
// position the file could have changed again, and a digest fed with bytes
// that differ from the remote part's would describe an object that was never
// assembled.
func (u *Uploader) hashResumePartAt(task *queue.UploadTask, totalSize, partSize int64, partNum int, fullSHA interface{ Write([]byte) (int, error) }, hashedBytes *int64) error {
	offset, pSize, err := partRange(partNum, totalSize, partSize)
	if err != nil {
		return err
	}
	data, err := readLocalRange(task.LocalPath, offset, pSize)
	if err != nil {
		return err
	}
	// The bytes' ETag equality with the remote part was established up front
	// (verifyResumePartETag); here we only feed the digest. A second change to
	// the file between the two reads would feed different bytes than the
	// remote holds — indistinguishable locally, but the server's own Complete
	// + any later reconciliation would expose it; the up-front window is small
	// and this is the same trust model as the upload path's TeeReader.
	if _, werr := fullSHA.Write(data); werr != nil {
		return fmt.Errorf("hash range [%d,%d): %w", offset, offset+pSize, werr)
	}
	*hashedBytes += int64(len(data))
	return nil
}

// partRange computes the [offset, offset+size) byte range of partNum within
// a totalSize object laid out in partSize chunks, rejecting parts outside
// the planned layout.
func partRange(partNum int, totalSize, partSize int64) (offset, pSize int64, err error) {
	offset = int64(partNum-1) * partSize
	pSize = partSize
	if remaining := totalSize - offset; remaining < pSize {
		pSize = remaining
	}
	if pSize <= 0 || offset >= totalSize {
		// A recorded part outside the planned object layout can never be
		// described; treat as stale.
		return 0, 0, fmt.Errorf("part range [%d,%d) is outside the object size %d", offset, offset+pSize, totalSize)
	}
	return offset, pSize, nil
}

// readLocalRange reads exactly pSize bytes at offset from path; a short read
// or I/O error is returned as an error (never silently under-counted).
func readLocalRange(path string, offset, pSize int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open %q: %w", path, err)
	}
	defer f.Close()
	if offset > 0 {
		if _, err := f.Seek(offset, io.SeekStart); err != nil {
			return nil, fmt.Errorf("seek %q to %d: %w", path, offset, err)
		}
	}
	data, err := io.ReadAll(io.LimitReader(f, pSize))
	if err != nil {
		return nil, fmt.Errorf("read range [%d,%d): %w", offset, offset+pSize, err)
	}
	if int64(len(data)) != pSize {
		// Short read: the file no longer contains the bytes the remote part
		// holds (truncated between attempts). Not a mismatch to "verify"
		// further — the local copy simply cannot describe the object.
		return nil, fmt.Errorf("local range [%d,%d) yielded only %d bytes (file shrank since the part was uploaded)", offset, offset+pSize, len(data))
	}
	return data, nil
}

// md5HexOf returns the hex-encoded MD5 digest of b (the format S3 part
// ETags use on both supported backends — live-verified, see
// bin/b7-ic-bug-51/poc.md).
func md5HexOf(b []byte) string {
	h := md5.Sum(b)
	return hex.EncodeToString(h[:])
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

// marshalCompletedParts serialises the ordered complete list for the queue's
// completed_parts JSON blob.
func marshalCompletedParts(parts []minio.CompletePart) string {
	raw, _ := json.Marshal(completedParts{Parts: parts})
	return string(raw)
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

// discardStaleUpload drops a recorded multipart upload whose parts the
// fail-closed gate rejected. Order law (P1-c / R2 / A3): the durable abort
// intent MUST be recorded before anything is destroyed — the record is the
// only retryable identity once the resume state is dropped, and there is no
// ILM backstop on current MinIO builds (IC-3 ③). If the record write fails,
// NOTHING is aborted and the error is returned: the caller fails the attempt
// with the row untouched (upload ID and parts stay trackable) and the retry
// redoes the discard. A direct-abort failure is different: the record is
// already durable, so the attempt proceeds and the executor's abort worker
// retries the abort with backoff. A successful abort removes the record.
func (u *Uploader) discardStaleUpload(ctx context.Context, task *queue.UploadTask, uploadID string) error {
	if u.queue != nil {
		if err := u.queue.EnqueueMultipartAbort(ctx, &queue.AbortOutboxEntry{
			UploadID:    uploadID,
			TaskID:      task.ID,
			Bucket:      task.Bucket,
			StoragePath: task.StoragePath,
		}); err != nil {
			u.logger.Error("uploader: record durable abort intent for stale multipart upload failed; refusing to discard",
				zap.String("task_id", task.ID),
				zap.String("upload_id", uploadID),
				zap.Error(err))
			return err
		}
	}
	if err := u.store.AbortMultipartUpload(ctx, task.Bucket, task.StoragePath, uploadID); err != nil && !isNoSuchUpload(err) {
		u.logger.Warn("uploader: abort of stale multipart upload failed; the durable abort record will be retried",
			zap.String("task_id", task.ID),
			zap.String("upload_id", uploadID),
			zap.Error(err))
		return nil
	}
	if u.queue != nil {
		if err := u.queue.DeleteMultipartAbort(ctx, uploadID); err != nil {
			u.logger.Warn("uploader: remove durable abort record after successful abort",
				zap.String("task_id", task.ID),
				zap.String("upload_id", uploadID),
				zap.Error(err))
		}
	}
	u.logger.Info("uploader: discarded stale multipart upload",
		zap.String("task_id", task.ID),
		zap.String("upload_id", uploadID))
	return nil
}
