package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// racyGranularityNs is the timestamp granularity the racy-window judgement
// allows for (IC-BUG-48, git "racy git" semantics). Coarse filesystems stamp
// mtimes at 2s (FAT) or 1s (some SMB) resolution, so a record written at T and
// a file stamped anywhere in [T-2s, +∞) cannot be ordered against the record
// with confidence. The constant is the union of those granularities; a
// narrower value would re-open the same-second blind spot on coarse stamps.
const racyGranularityNs = int64(2 * 1e9)

// FileIdentity is the kernel metadata snapshot of one file version — the
// IC-BUG-48 content-identity fields shared by the processed-files dedup and
// the multipart resume gate. CtimeNs is the status-change time (a writer
// cannot forge it via utimes/touch); Dev+Ino is the file identity (a
// delete-and-recreate always changes it). A zero field means "unknown": the
// platform or filesystem did not provide it. Unknowns never compare equal —
// an unknown must lead to hash confirmation (fail-closed), never to "same".
type FileIdentity struct {
	MtimeNs int64
	CtimeNs int64
	Dev     uint64
	Ino     uint64
	Size    int64
}

// identityMismatch reports which identity fields differ between a recorded
// row and the current file. Both sides are already known to be fully known
// (no zero fields). Returned as a slice of field names for logging/tests.
func identityMismatch(rec FileIdentity, cur FileIdentity) []string {
	var diffs []string
	if rec.MtimeNs != cur.MtimeNs {
		diffs = append(diffs, "mtime_ns")
	}
	if rec.CtimeNs != cur.CtimeNs {
		diffs = append(diffs, "ctime_ns")
	}
	if rec.Dev != cur.Dev {
		diffs = append(diffs, "dev")
	}
	if rec.Ino != cur.Ino {
		diffs = append(diffs, "ino")
	}
	if rec.Size != cur.Size {
		diffs = append(diffs, "size")
	}
	return diffs
}

// identityFullyKnown reports whether every field of the identity carries a
// value. Any zero field makes the identity undecidable: the comparison input
// is missing, and comparing against "unknown" proves nothing (the exact
// reasoning the IC-3 resume gate applies to zero-value snapshots).
func identityFullyKnown(id FileIdentity) bool {
	return id.MtimeNs != 0 && id.CtimeNs != 0 && id.Dev != 0 && id.Ino != 0
}

// Verdict is the outcome of the prefilter stage of the IC-BUG-48 content
// identity check. It decides whether the file may be skipped without reading
// it (VerdictSame), must be re-collected without further checks
// (VerdictChanged — kernel metadata a writer cannot forge says the content
// changed), or must be confirmed by hashing the full content
// (VerdictUndecidable — any input missing, or the racy window applies).
type Verdict int

// Verdict values; see the Verdict doc comment.
const (
	// VerdictSame: prefilter matched on every field and no racy window
	// applies. The file was not touched by any writer operation the kernel
	// can observe since the record was written — safe to skip without
	// reading it.
	VerdictSame Verdict = iota
	// VerdictChanged: at least one forge-proof kernel field differs. The
	// content (or at minimum its observable state) changed — re-collect. No
	// hash confirmation is needed to reach this verdict.
	VerdictChanged
	// VerdictUndecidable: a comparison input is missing (legacy row, unknown
	// field) or the racy window applies. The caller must hash-confirm the
	// full content against the recorded sha256 before deciding.
	VerdictUndecidable
)

// String renders the verdict for logs and tests.
func (v Verdict) String() string {
	switch v {
	case VerdictSame:
		return "same"
	case VerdictChanged:
		return "changed"
	case VerdictUndecidable:
		return "undecidable"
	default:
		return fmt.Sprintf("verdict(%d)", int(v))
	}
}

// PrefilterVerdict compares the recorded identity of a processed-file row
// against the current file's identity. This is the ONE content-identity
// comparison in the codebase: the processed-files dedup and the multipart
// resume gate both route through it.
//
//   - any recorded field unknown (zero / legacy row)      → VerdictUndecidable
//   - any current field unknown (stat blob lacked it)     → VerdictUndecidable
//   - any known field differs                             → VerdictChanged
//   - everything equal but the file's mtime or ctime lies within the racy
//     granularity of the record time (recordAtNs)         → VerdictUndecidable
//   - otherwise                                           → VerdictSame
//
// recordAtNs is when the processed-file record was written (Unix ns); pass 0
// for "unknown record time", which forces the undecidable path — the racy
// judgement needs it, exactly like git needs the index write time to clear
// racily-clean entries.
func PrefilterVerdict(rec FileIdentity, cur FileIdentity, recordAtNs int64) Verdict {
	if !identityFullyKnown(rec) || !identityFullyKnown(cur) {
		return VerdictUndecidable
	}
	if diffs := identityMismatch(rec, cur); len(diffs) > 0 {
		return VerdictChanged
	}
	// Racy window (git racily-clean): if the record was written within the
	// timestamp granularity around the file's stamps, a same-second rewrite
	// could hide behind equal coarse stamps. Even a fully matching prefilter
	// must then be confirmed by content. Guarded by recordAtNs > 0: callers
	// with an unknown record time have already been routed to undecidable by
	// the ledger row's zero uploaded_at_ns, but stay defensive here.
	if recordAtNs > 0 {
		if cur.MtimeNs >= recordAtNs-racyGranularityNs || cur.CtimeNs >= recordAtNs-racyGranularityNs {
			return VerdictUndecidable
		}
	}
	return VerdictSame
}

// ── Processed-file candidate decision (hash-confirming dedup) ────────────────

// HashResult carries the outcome of hashing the candidate file's full
// content, injected by the caller so the queue package stays I/O-free: the
// production caller reads the file and computes sha256; tests inject canned
// values. size is informational (the ledger keeps the recorded size).
type HashResult struct {
	SHA256 string
	Size   int64
}

// CandidateVerdict is the final decision for one candidate file: skip it
// (content confirmed unchanged), re-collect it (content changed or could not
// be confirmed), or refresh-and-skip (content identical but the ledger row
// was stale — its identity fields are refreshed to the current values so the
// next candidate check can trust the prefilter again).
type CandidateVerdict int

// Candidate verdict values; see the CandidateVerdict doc comment.
const (
	// Skip: no new content to upload.
	Skip CandidateVerdict = iota
	// Recollect: enqueue an upload for the current content.
	Recollect
	// RefreshAndSkip: content identical; the row's identity metadata was
	// refreshed to the current file's values (the ledger caught up with
	// reality — same content, fresher kernel stamps). Next time the
	// prefilter can decide on its own.
	RefreshAndSkip
)

// String renders the candidate verdict for logs and tests.
func (v CandidateVerdict) String() string {
	switch v {
	case Skip:
		return "skip"
	case Recollect:
		return "recollect"
	case RefreshAndSkip:
		return "refresh-and-skip"
	default:
		return fmt.Sprintf("candidate-verdict(%d)", int(v))
	}
}

// CandidateDecision is the full outcome of DecideProcessed: the verdict plus
// the recorded sha256 (empty when the row did not exist or carried none) and
// the prefilter verdict, for logging and tests.
type CandidateDecision struct {
	Verdict    CandidateVerdict
	Prefilter  Verdict
	RecordedSHA string
}

// DecideProcessed decides whether a candidate file must be re-collected,
// replacing the bare IsProcessed tuple check for the IC-BUG-48 flow:
//
//  1. no processed-file row for (ruleID, localPath) → Recollect;
//  2. PrefilterVerdict(rec.ident, cur.ident, rec.uploadedAtNs):
//     a. VerdictChanged → Recollect (forge-proof kernel metadata says the
//     file's state changed since the record);
//     b. VerdictSame → Skip (no read of the file at all);
//     c. VerdictUndecidable → hash-confirm: hashFn() reads the full content
//     and the digest is compared with the recorded sha256 — equal →
//     RefreshAndSkip (row's identity refreshed to current values), unequal
//     → Recollect. An empty recorded sha256 (legacy row) forces Recollect:
//     "cannot prove it is the same" never means "assume it is".
//
// The legacy IsProcessed stays for callers that genuinely only want the
// tuple predicate; the agent's candidate path routes through this method.
func (q *Queue) DecideProcessed(ctx context.Context, ruleID, localPath string, cur FileIdentity, hashFn func() (HashResult, error)) (CandidateDecision, error) {
	rec, err := q.getProcessedRecord(ctx, ruleID, localPath)
	if err != nil {
		return CandidateDecision{}, err
	}
	if rec == nil {
		return CandidateDecision{Verdict: Recollect}, nil
	}

	pv := PrefilterVerdict(rec.ident, cur, rec.uploadedAtNs)
	switch pv {
	case VerdictChanged:
		return CandidateDecision{Verdict: Recollect, Prefilter: pv, RecordedSHA: rec.sha256}, nil
	case VerdictSame:
		return CandidateDecision{Verdict: Skip, Prefilter: pv, RecordedSHA: rec.sha256}, nil
	default: // VerdictUndecidable
		if rec.sha256 == "" {
			// Nothing recorded to compare against: the ledger cannot prove
			// this is the same content, so it must be re-collected. (The
			// upload will refresh the row with a fresh hash + identity.)
			return CandidateDecision{Verdict: Recollect, Prefilter: pv, RecordedSHA: rec.sha256}, nil
		}
		hr, err := hashFn()
		if err != nil {
			return CandidateDecision{}, fmt.Errorf("queue: hash confirm %q: %w", localPath, err)
		}
		if hr.SHA256 == rec.sha256 {
			if err := q.refreshProcessedIdentity(ctx, ruleID, localPath, cur); err != nil {
				return CandidateDecision{}, err
			}
			return CandidateDecision{Verdict: RefreshAndSkip, Prefilter: pv, RecordedSHA: rec.sha256}, nil
		}
		return CandidateDecision{Verdict: Recollect, Prefilter: pv, RecordedSHA: rec.sha256}, nil
	}
}

// processedRecord is the ledger row relevant to a candidate decision.
type processedRecord struct {
	ident        FileIdentity
	uploadedAtNs int64
	sha256       string
}

// getProcessedRecord loads the processed-file row's identity fields; nil when
// no row exists.
func (q *Queue) getProcessedRecord(ctx context.Context, ruleID, localPath string) (*processedRecord, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT file_mtime_ns, file_ctime_ns, file_dev, file_ino, file_size, uploaded_at_ns, sha256
         FROM processed_files WHERE rule_id=? AND local_path=?`,
		ruleID, localPath,
	)
	var r processedRecord
	var mtimeNs, ctimeNs, uploadedAtNs int64
	var dev, ino uint64
	var size int64
	var sha256 string
	if err := row.Scan(&mtimeNs, &ctimeNs, &dev, &ino, &size, &uploadedAtNs, &sha256); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("queue: load processed record %q: %w", localPath, err)
	}
	r.ident = FileIdentity{MtimeNs: mtimeNs, CtimeNs: ctimeNs, Dev: dev, Ino: ino, Size: size}
	r.uploadedAtNs = uploadedAtNs
	r.sha256 = sha256
	return &r, nil
}

// refreshProcessedIdentity updates the ledger row's identity fields to the
// current file's values after a hash confirmation proved the content
// identical. Ledger semantics: no new content to upload, but the bookkeeping
// must track reality, or every future candidate check pays the full hash
// again.
func (q *Queue) refreshProcessedIdentity(ctx context.Context, ruleID, localPath string, cur FileIdentity) error {
	res, err := q.db.ExecContext(ctx,
		`UPDATE processed_files
         SET file_mtime_ns=?, file_ctime_ns=?, file_dev=?, file_ino=?, file_size=?, uploaded_at_ns=?
         WHERE rule_id=? AND local_path=?`,
		cur.MtimeNs, cur.CtimeNs, cur.Dev, cur.Ino, cur.Size, nowNs(), ruleID, localPath,
	)
	if err != nil {
		return fmt.Errorf("queue: refresh processed identity %q: %w", localPath, err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		// The row vanished between the load and the refresh; nothing to do —
		// the next candidate check will see no record and re-collect.
		return nil
	}
	return nil
}
