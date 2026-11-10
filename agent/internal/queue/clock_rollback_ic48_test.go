package queue

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestPrefilterVerdict_ClockRollback_Undecidable — rework-2 fix 5: a ledger
// anchor in the FUTURE relative to the current clock (system clock rolled
// back) makes the wall clock unable to order the stamps against the anchor →
// Undecidable → hash-confirm. Also pins the boundary: an anchor exactly at
// now is fine (not future).
func TestPrefilterVerdict_ClockRollback_Undecidable(t *testing.T) {
	now := nowNs()
	known := FileIdentity{MtimeNs: now - 10e9, CtimeNs: now - 10e9, Dev: 1, Ino: 2, Size: 4}

	// Anchor 1h in the future → clock was rolled back → Undecidable.
	future := now + 3600e9
	assert.Equal(t, VerdictUndecidable, PrefilterVerdict(known, known, int64(future)),
		"a ledger anchor in the future (clock rollback) must hash-confirm")

	// Anchor at/behind now with stamps well before anchor−G → the normal
	// path (Same, fields equal & aged).
	aged := FileIdentity{MtimeNs: now - 100e9, CtimeNs: now - 100e9, Dev: 1, Ino: 2, Size: 4}
	assert.Equal(t, VerdictSame, PrefilterVerdict(aged, aged, now),
		"an anchor in the past with equal, aged stamps is not racy")
}

// TestDecideProcessed_ClockRollbackLedger_HashConfirms — the end-to-end
// consequence: a ledger row anchored in the future must hash-confirm (and on
// identical content converge via refresh, which re-anchors at the current
// clock), never bare-skip.
func TestDecideProcessed_ClockRollbackLedger_HashConfirms(t *testing.T) {
	q, err := Open(":memory:")
	require.NoError(t, err)
	defer q.Close()

	path := ic48WriteFile(t, "rollback.bin", "RBK1")
	hr := ic48SHA(t, path)
	// Ledger anchored 1h in the future (clock rolled back after the write).
	future := nowNs() + 3600e9
	id := ic48Stat(t, path)
	require.NoError(t, q.UpsertProcessedFile(&ProcessedFile{
		ID: "pf-rb", RuleID: "rule", LocalPath: path,
		FileSize: 4, FileMtime: id.MtimeNs / 1e9, SHA256: hr,
		Ident: FileIdentity{
			MtimeNs: id.MtimeNs, CtimeNs: id.CtimeNs, Dev: id.Dev, Ino: id.Ino,
			Size: id.Size, SnapAtNs: future,
		},
		UploadedAtNs: future,
	}))

	dec := ic48Decide(t, q, "rule", path)
	require.Equal(t, RefreshAndSkip, dec.Verdict,
		"a future-anchored ledger must hash-confirm (identical → refresh re-anchors at the current clock), never bare-skip")

	// The refresh re-anchored at the current clock: a second decision now
	// takes the normal path.
	rec, err := q.GetProcessedIdentity("rule", path)
	require.NoError(t, err)
	require.LessOrEqual(t, rec.SnapshotAtNs, nowNs(),
		"the refresh must re-anchor the row at (or before) the current clock")
	_ = context.Background
}
