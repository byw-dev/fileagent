package queue

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPrefilterVerdict_RacyNeedsCtime — the mutation detector for "racy
// window drops the ctime check" (coordination rework-1 M3). A writer can
// forge mtime backwards (touch -t / os.Chtimes) but can NEVER forge ctime:
// after a same-tick rewrite with the mtime rolled back BEFORE the anchor,
// only the ctime racy check can catch the change. The construction:
//
//   - recorded == current on every field (all equal → past the mismatch
//     check)
//   - current.MtimeNs far BEFORE anchor−G (forged backwards)
//   - current.CtimeNs inside [anchor−G, ∞) (the real rewrite's ctime)
//
// One-sided rule (mtime OR ctime ≥ anchor−G) → Undecidable → hash. A
// mutated rule that only checks mtime → Same → bare skip (red).
func TestPrefilterVerdict_RacyNeedsCtime(t *testing.T) {
	g := RacyGranularityNs
	anchor := int64(1_000_000_000_000) // snapshot moment
	stamps := FileIdentity{
		MtimeNs: anchor - g - g, // forged back: two granularities before anchor
		CtimeNs: anchor - g/2,   // real rewrite: inside the racy window
		Dev:     1, Ino: 2, Size: 4,
	}
	rec := stamps // ledger copied the (forged-mtime) identity verbatim
	cur := stamps

	assert.Equal(t, VerdictUndecidable, PrefilterVerdict(rec, cur, anchor),
		"an mtime-rolled-back same-tick rewrite must stay racy via its ctime (the only unforgeable witness)")
}

// TestPrefilterVerdict_RacyNeedsMtime — the symmetric half: a stamp far
// before the anchor on CTIME but at/after anchor−G on mtime must also stay
// racy (both checks are required; removing either is a fail-open mutation).
func TestPrefilterVerdict_RacyNeedsMtime(t *testing.T) {
	g := RacyGranularityNs
	anchor := int64(1_000_000_000_000)
	stamps := FileIdentity{
		MtimeNs: anchor - g/2,   // rewritten just inside the window
		CtimeNs: anchor - g - g, // ctime somehow stale (coarse-FS tick collision)
		Dev:     1, Ino: 2, Size: 4,
	}
	assert.Equal(t, VerdictUndecidable, PrefilterVerdict(stamps, stamps, anchor),
		"an mtime inside the racy window must stay racy even when ctime is far before the anchor")
}
