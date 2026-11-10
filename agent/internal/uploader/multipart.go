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
	"strings"

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
// upload (IC-3 R-A/A1/A2 + IC-BUG-48): the recorded parts may be skipped ONLY
// when ALL of the following hold — anything undecidable means "do not trust
// the parts", because resuming stale parts splices old and new file content
// into one object with a wrong SHA:
//
//  1. the row carries the full identity snapshot (parts_file_mtime/size +
//     parts_file_mtime_ns/ctime_ns/dev/ino): a legacy row missing any
//     identity field is untrusted — its parts may belong to ANY version of
//     the file;
//  2. the current file was stat'ed successfully and its identity is fully
//     known (no unknown fields);
//  3. the snapshot matches the current identity on EVERY field (mtime,
//     mtime-ns, ctime-ns, dev, ino, size) — one shared comparison with the
//     processed-files dedup (queue.PrefilterVerdict metadata stage);
//  4. this attempt actually goes the multipart path: the upload would
//     otherwise be a PutObject (a file that shrank below the threshold, or a
//     tail-mode offset shrinking the increment), which can never consume the
//     recorded parts — the old multipart upload would be orphaned with no
//     reference left.
//
// Content-level staleness (bytes changed under identical metadata) is NOT
// this gate's job: IC-BUG-51's per-part ETag verification
// (verifyResumePartETag/hashResumePartChecked) covers it downstream.
//
// The single gate exists so that "why can't we resume" is directly assertable
// by tests and logs instead of being scattered across branches — scattered
// conditions are exactly how the fail-open holes (A1/A2) slipped in.
func canResumeParts(task *queue.UploadTask, current queue.FileVersion, statOK, isMultipart bool) (bool, string) {
	snap := queue.FileIdentity{
		MtimeNs: task.PartsFileMtimeNs,
		CtimeNs: task.PartsFileCtimeNs,
		Dev:     task.PartsFileDev,
		Ino:     task.PartsFileIno,
		Size:    task.PartsFileSize,
	}
	if !queue.IdentityFullyKnown(snap) {
		return false, "no full identity snapshot for the recorded parts (legacy row, untrusted)"
	}
	if !statOK {
		return false, "current file stat failed"
	}
	cur := queue.IdentityFromFileVersion(current)
	if !queue.IdentityFullyKnown(cur) {
		return false, "current file identity has unknown fields"
	}
	if diffs := queue.IdentityDiffs(snap, cur); len(diffs) > 0 {
		return false, "file identity changed since the parts were uploaded: " + strings.Join(diffs, ",")
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
func (u *Uploader) multipartUpload(ctx context.Context, f *os.File, task *queue.UploadTask, fv queue.FileVersion, size int64) (*UploadResult, error) {
	// Rework-2 blocker 1: f is the ONE handle UploadFile opened and stat'ed.
	// Every part read — uploaded or resumed — comes from THIS fd via
	// io.NewSectionReader (ReadAt), never from a path re-open: a path swap
	// between the identity stat and any part read would splice another
	// file's bytes into this object under this file's identity.
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
				if err := u.verifyResumePartETag(f, task, size, partSize, p); err != nil {
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
			// Skipped part: its bytes are already on the remote. Stream the local
			// range through the digest AT THIS POSITION (order law) and verify
			// the remote ETag over THAT SAME read — the correctness check
			// (rework-2 fix 1): the up-front pre-check used a different read,
			// and the file may have changed since. Rework-2 blocker 1: the
			// read comes from the SAME fd UploadFile stat'ed (no path re-open).
			if err := u.hashResumePartChecked(f, task, size, partSize, partNum, remoteETags[partNum], fullSHA, &hashedBytes); err != nil {
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

		// IC-BUG-51: the part's bytes stream through the full-file digest on
		// their way to the store — one read feeds both. A short file (the
		// source shrank mid-upload) makes the count check below fail loudly.
		// Rework-2 blocker 1: SectionReader over the ONE handle UploadFile
		// opened — ReadAt leaves f's offset untouched across parts.
		md5h := md5.New()
		body := &countingTeeReader{r: io.NewSectionReader(f, offset, pSize), w: io.MultiWriter(fullSHA, md5h)}

		part, err := u.store.PutObjectPart(ctx, task.Bucket, task.StoragePath, uploadID,
			partNum, body, pSize, minio.PutObjectPartOptions{})
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

// verifyResumePartETag is the EARLY pre-check pass over a skipped
// (already-remote) part: it streams the part's local byte range once, feeds
// it ONLY into a throwaway MD5 (no digest), and compares that against the
// part's remote ETag. Running it for every remote part up front lets a
// stale upload be discarded before anything is hashed or uploaded — fail
// fast. It returns an error when the local content does not match what the
// remote holds; the caller reacts by discarding the upload and starting
// over (IC-BUG-51: never splice mismatched parts under a fresh digest).
//
// ⚠️ This pre-check is an OPTIMISATION, not the correctness gate. It and the
// digest-feeding read in hashResumePartChecked are two separate reads of the
// same range; the file can change in between. The check that actually
// decides correctness is the one inside the SAME read that feeds the digest
// (see hashResumePartChecked) — this pass only fails fast on what was
// already stale when the resume began.
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
func (u *Uploader) verifyResumePartETag(f *os.File, task *queue.UploadTask, totalSize, partSize int64, part minio.CompletePart) error {
	offset, pSize, err := partRange(part.PartNumber, totalSize, partSize)
	if err != nil {
		return err
	}
	if !etagIsComparable(part.ETag) {
		return fmt.Errorf("remote ETag %q is not a comparable md5 digest", part.ETag)
	}
	got, _, err := streamRangeMD5(f, offset, pSize)
	if err != nil {
		return err
	}
	if normalizeETag(part.ETag) != got {
		return fmt.Errorf("remote ETag %s != local md5 %s for range [%d,%d)", normalizeETag(part.ETag), got, offset, offset+pSize)
	}
	return nil
}

// hashResumePartChecked feeds the local byte range of a skipped
// (already-remote) part into the full-file digest at the part's own position
// in the ordered loop — the ORDER LAW half for skipped parts — and, IN THE
// SAME STREAMING READ, computes the MD5 and re-verifies it against the part's
// remote ETag. This is the check that decides correctness (rework-2 fix 1):
// the up-front pre-check (verifyResumePartETag) and this read are two
// separate reads, and the file may be rewritten in between; only a check
// over the very bytes entering the SHA-256 guarantees that the digest
// describes the object the remote will assemble. A mismatch here is
// errStaleRemoteParts (the caller discards the upload and starts over) —
// by that point some later parts may already have been uploaded, which is
// wasted work but never a wrong result.
func (u *Uploader) hashResumePartChecked(f *os.File, task *queue.UploadTask, totalSize, partSize int64, partNum int, remoteETag string, fullSHA io.Writer, hashedBytes *int64) error {
	offset, pSize, err := partRange(partNum, totalSize, partSize)
	if err != nil {
		return err
	}
	md5h := md5.New()
	n, err := streamRangeInto(f, offset, pSize, io.MultiWriter(fullSHA, md5h))
	if err != nil {
		return err
	}
	if n != pSize {
		// Belt over the short-read guard inside streamRangeInto (defensive;
		// kept so a future refactor of the helper cannot silently drop it).
		return fmt.Errorf("local range [%d,%d) yielded only %d bytes (file shrank since the part was uploaded)", offset, offset+pSize, n)
	}
	if !etagIsComparable(remoteETag) {
		return fmt.Errorf("remote ETag %q is not a comparable md5 digest", remoteETag)
	}
	if got := hex.EncodeToString(md5h.Sum(nil)); normalizeETag(remoteETag) != got {
		return fmt.Errorf("remote ETag %s != local md5 %s for range [%d,%d) read into the digest", normalizeETag(remoteETag), got, offset, offset+pSize)
	}
	*hashedBytes += n
	return nil
}

// streamRangeMD5 streams the [offset, offset+pSize) range through an MD5
// from the given fd (rework-2 blocker 1: same fd as the identity stat — no
// path re-open) without buffering the range (a part can be 64 MiB and the
// agent runs on edge devices), returning the hex digest and the number of
// bytes read. A short read or I/O error is returned as an error — never
// silently under-counted. The fd's file offset is left unmodified (ReadAt).
func streamRangeMD5(f *os.File, offset, pSize int64) (string, int64, error) {
	md5h := md5.New()
	n, err := streamRangeInto(f, offset, pSize, md5h)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(md5h.Sum(nil)), n, nil
}

// streamRangeInto streams exactly pSize bytes starting at offset from the fd
// into w (no whole-range buffering, ReadAt so the fd offset is untouched)
// and returns the number of bytes written. A short read means the file no
// longer contains the bytes the remote part holds (truncated since the part
// was uploaded) — the local copy cannot describe the object, so it is an
// error, not a partial success.
func streamRangeInto(f *os.File, offset, pSize int64, w io.Writer) (int64, error) {
	n, err := io.Copy(w, io.NewSectionReader(f, offset, pSize))
	if err != nil {
		return n, fmt.Errorf("read range [%d,%d): %w", offset, offset+pSize, err)
	}
	if n != pSize {
		return n, fmt.Errorf("local range [%d,%d) yielded only %d bytes (file shrank since the part was uploaded)", offset, offset+pSize, n)
	}
	return n, nil
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
