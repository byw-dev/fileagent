package queue

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// racyGranularityNs is the timestamp granularity the racy-window judgement
// allows for (IC-BUG-48, git "racy git" semantics). Coarse filesystems stamp
// mtimes at 2s (FAT) or 1s (some SMB) resolution, so a file stamped within G
// before the snapshot moment cannot be ordered against it with confidence.
const racyGranularityNs = int64(2 * 1e9)

// RacyGranularityNs exposes racyGranularityNs for tests and scratch tools.
const RacyGranularityNs = racyGranularityNs

// FileIdentity is the kernel metadata snapshot of one file version — the
// IC-BUG-48 content-identity fields shared by the processed-files dedup and
// the multipart resume gate. CtimeNs is the status-change time (a writer
// cannot forge it via utimes/touch); Dev+Ino is the file identity (a
// delete-and-recreate always changes it). A zero field means "unknown": the
// platform or filesystem did not provide it. Unknowns never compare equal —
// an unknown must lead to hash confirmation (fail-closed), never to "same".
//
// SnapAtNs is the moment THIS identity was stat'ed (Unix ns) — the racy
// anchor. It is not part of the equality comparison; zero means "unknown
// snapshot moment", which also forces the undecidable path.
type FileIdentity struct {
	MtimeNs int64
	CtimeNs int64
	Dev     uint64
	Ino     uint64
	Size    int64

	SnapAtNs int64
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

// IdentityDiffs is the exported form of identityMismatch for callers outside
// the queue package (the multipart resume gate names the changed fields in
// its fail-closed reason).
func IdentityDiffs(rec FileIdentity, cur FileIdentity) []string {
	return identityMismatch(rec, cur)
}

// IdentityFullyKnown is the exported form of identityFullyKnown (the resume
// gate must reject snapshots with unknown fields without reaching the
// verdict logic).
func IdentityFullyKnown(id FileIdentity) bool {
	return identityFullyKnown(id)
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
//   - everything equal but any stamp (mtime or ctime) is ≥ recordAtNs − G
//     (G = 2s: a stamp within the granularity before the snapshot moment can
//     hide a same-tick rewrite behind equal coarse stamps — git racily-clean)
//     → VerdictUndecidable
//   - recordAtNs unknown (0)                              → VerdictUndecidable
//   - otherwise                                           → VerdictSame
//
// recordAtNs is the SNAPSHOT moment — when the ledger's identity was stat'ed
// (Unix ns), NOT when the record row was written (an ack can lag the upload by
// hours; anchoring on it would let a same-tick rewrite slip through — the
// delayed-ack test pins this). Convergence works by REFRESH: a hash-confirmed
// identical file overwrites the ledger with the new snapshot and its moment,
// so once G has passed the stamps sort cleanly before the new anchor and Same
// is reachable again (git's racily-clean convergence).
//
// A metadata mismatch (VerdictChanged) proves the file's observable STATE
// changed — rewrite, chmod, rename, delete-and-recreate — but NOT that the
// content differs: callers decide via the hash-confirm flow in
// DecideProcessed.
func PrefilterVerdict(rec FileIdentity, cur FileIdentity, recordAtNs int64) Verdict {
	if !identityFullyKnown(rec) || !identityFullyKnown(cur) {
		return VerdictUndecidable
	}
	if diffs := identityMismatch(rec, cur); len(diffs) > 0 {
		return VerdictChanged
	}
	if recordAtNs == 0 {
		// Unknown snapshot moment: the racy judgement cannot run, so the safe
		// outcome is "confirm by content".
		return VerdictUndecidable
	}
	// Racy window (git racily-clean), single-sided: a stamp at or after
	// snapshotAt − G may belong to a rewrite in the same coarse tick as the
	// snapshot — the wall clock cannot order them. Stamps strictly before the
	// window are fully covered by the field equality above: the kernel
	// observed no state change since the snapshot.
	g := racyGranularityNs
	if cur.MtimeNs >= recordAtNs-g || cur.CtimeNs >= recordAtNs-g {
		return VerdictUndecidable
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
	Verdict     CandidateVerdict
	Prefilter   Verdict
	RecordedSHA string
}

// DecideProcessed decides whether a candidate file must be re-collected,
// replacing the bare IsProcessed tuple check for the IC-BUG-48 flow:
//
//  1. no processed-file row for (ruleID, localPath) → Recollect;
//  2. PrefilterVerdict(rec.ident, cur.ident, rec.snapshotAtNs):
//     a. VerdictSame → Skip — the kernel saw no state change since the
//     ledger's stat snapshot; the file is not even read;
//     b. VerdictChanged or VerdictUndecidable → hash-confirm: read the full
//     content (hashFn) and compare with the recorded sha256. A metadata
//     mismatch proves the file's STATE changed (rewrite, chmod, rebuild) but
//     NOT that the content differs — a content-identical rebuild must not be
//     re-uploaded. Equal hash → RefreshAndSkip: the row's identity AND racy
//     anchor are overwritten with THIS check's snapshot (cur with snapAtNs) —
//     that refresh is what lets the prefilter converge after G has passed
//     (git racily-clean). Different hash → Recollect. An empty recorded
//     sha256 (legacy row, nothing to compare) forces Recollect: "cannot prove
//     it is the same" never means "assume it is".
//
// cur.snapAtNs anchors the racy judgement of the CURRENT check; the ledger's
// rec.snapshotAtNs anchors the recorded side. The legacy IsProcessed stays
// for callers that genuinely only want the tuple predicate; the agent's
// candidate path routes through this method.
func (q *Queue) DecideProcessed(ctx context.Context, ruleID, localPath string, cur FileIdentity, hashFn func() (HashResult, error)) (CandidateDecision, error) {
	rec, err := q.getProcessedRecord(ctx, ruleID, localPath)
	if err != nil {
		return CandidateDecision{}, err
	}
	if rec == nil {
		return CandidateDecision{Verdict: Recollect}, nil
	}

	pv := PrefilterVerdict(rec.ident, cur, rec.snapshotAtNs)
	if pv == VerdictSame {
		return CandidateDecision{Verdict: Skip, Prefilter: pv, RecordedSHA: rec.sha256}, nil
	}
	// Changed or undecidable: the content itself is unproven — hash-confirm.
	if rec.sha256 == "" {
		// Nothing recorded to compare against: the ledger cannot prove this
		// is the same content, so it must be re-collected. (The upload will
		// refresh the row with a fresh hash + identity.)
		return CandidateDecision{Verdict: Recollect, Prefilter: pv, RecordedSHA: rec.sha256}, nil
	}
	hr, err := hashFn()
	if err != nil {
		return CandidateDecision{}, fmt.Errorf("queue: hash confirm %q: %w", localPath, err)
	}
	if hr.SHA256 == rec.sha256 {
		if err := q.refreshProcessedIdentity(ctx, ruleID, localPath, cur, cur.SnapAtNs); err != nil {
			return CandidateDecision{}, err
		}
		return CandidateDecision{Verdict: RefreshAndSkip, Prefilter: pv, RecordedSHA: rec.sha256}, nil
	}
	return CandidateDecision{Verdict: Recollect, Prefilter: pv, RecordedSHA: rec.sha256}, nil
}

// processedRecord is the ledger row relevant to a candidate decision.
type processedRecord struct {
	ident        FileIdentity
	snapshotAtNs int64 // the moment the ledger's identity was stat'ed
	sha256       string
}

// getProcessedRecord loads the processed-file row's identity fields; nil when
// no row exists. uploaded_at_ns stores the identity's SNAPSHOT moment (when
// the stat was taken), not the ack time.
func (q *Queue) getProcessedRecord(ctx context.Context, ruleID, localPath string) (*processedRecord, error) {
	row := q.db.QueryRowContext(ctx,
		`SELECT file_mtime_ns, file_ctime_ns, file_dev, file_ino, file_size, uploaded_at_ns, sha256
         FROM processed_files WHERE rule_id=? AND local_path=?`,
		ruleID, localPath,
	)
	var r processedRecord
	var mtimeNs, ctimeNs, snapAtNs int64
	var dev, ino uint64
	var size int64
	var sha256 string
	if err := row.Scan(&mtimeNs, &ctimeNs, &dev, &ino, &size, &snapAtNs, &sha256); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, fmt.Errorf("queue: load processed record %q: %w", localPath, err)
	}
	r.ident = FileIdentity{MtimeNs: mtimeNs, CtimeNs: ctimeNs, Dev: dev, Ino: ino, Size: size}
	r.snapshotAtNs = snapAtNs
	r.sha256 = sha256
	return &r, nil
}

// refreshProcessedIdentity updates the ledger row's identity fields to the
// current file's values after a hash confirmation proved the content
// identical. Ledger semantics: no new content to upload, but the bookkeeping
// must track reality, or every future candidate check pays the full hash
// again. snapAtNs is the moment cur was stat'ed — the NEW racy anchor, which
// is what lets the prefilter converge (git racily-clean: the refreshed anchor
// eventually sorts strictly before the stamps and Same is reachable).
func (q *Queue) refreshProcessedIdentity(ctx context.Context, ruleID, localPath string, cur FileIdentity, snapAtNs int64) error {
	res, err := q.db.ExecContext(ctx,
		`UPDATE processed_files
         SET file_mtime_ns=?, file_ctime_ns=?, file_dev=?, file_ino=?, file_size=?, uploaded_at_ns=?
         WHERE rule_id=? AND local_path=?`,
		cur.MtimeNs, cur.CtimeNs, cur.Dev, cur.Ino, cur.Size, snapAtNs, ruleID, localPath,
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

// GetProcessedIdentity returns the recorded identity of a processed-file row
// (scratch/test inspection helper; zero identity when no row exists).
func (q *Queue) GetProcessedIdentity(ruleID, localPath string) (ProcessedIdentity, error) {
	rec, err := q.getProcessedRecord(context.Background(), ruleID, localPath)
	if err != nil {
		return ProcessedIdentity{}, err
	}
	if rec == nil {
		return ProcessedIdentity{}, nil
	}
	return ProcessedIdentity{Ident: rec.ident, SnapshotAtNs: rec.snapshotAtNs, SHA256: rec.sha256}, nil
}

// SetProcessedIdentityAnchor backdates the racy-window anchor of a
// processed-file row. Test/scratch helper: production code always anchors at
// the snapshot moment of the write.
func (q *Queue) SetProcessedIdentityAnchor(ruleID, localPath string, snapAtNs int64) error {
	_, err := q.db.Exec(
		`UPDATE processed_files SET uploaded_at_ns=? WHERE rule_id=? AND local_path=?`,
		snapAtNs, ruleID, localPath,
	)
	if err != nil {
		return fmt.Errorf("queue: set processed identity anchor %q: %w", localPath, err)
	}
	return nil
}

// ProcessedIdentity is the recorded identity of a processed-file row.
type ProcessedIdentity struct {
	Ident        FileIdentity
	SnapshotAtNs int64
	SHA256       string
}
