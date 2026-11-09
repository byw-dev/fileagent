package queue

import (
	"testing"

	"github.com/stretchr/testify/assert"
)

// TestPrefilterVerdict_Table pins the verdict truth table at unit level
// (IC-BUG-48): unknown fields never compare equal, known-field mismatches are
// "changed", the racy window is symmetric around the record time, and a fully
// known match outside the window is "same".
func TestPrefilterVerdict_Table(t *testing.T) {
	known := FileIdentity{MtimeNs: 1_000_000_000_000, CtimeNs: 1_000_000_000_000, Dev: 1, Ino: 2, Size: 4}
	g := RacyGranularityNs
	cases := []struct {
		name       string
		rec, cur   FileIdentity
		recordAtNs int64
		want       Verdict
	}{
		{"rec unknown mtime", FileIdentity{}, known, 1, VerdictUndecidable},
		{"rec legacy zero identity", FileIdentity{Size: 4}, known, 1, VerdictUndecidable},
		{"cur unknown ctime", known, FileIdentity{MtimeNs: known.MtimeNs, CtimeNs: 0, Dev: 1, Ino: 2, Size: 4}, 1, VerdictUndecidable},
		{"mtime differs", known, FileIdentity{MtimeNs: known.MtimeNs + 1, CtimeNs: known.CtimeNs, Dev: 1, Ino: 2, Size: 4}, 1, VerdictChanged},
		{"ctime differs (rewrite)", known, FileIdentity{MtimeNs: known.MtimeNs, CtimeNs: known.CtimeNs + 1, Dev: 1, Ino: 2, Size: 4}, 1, VerdictChanged},
		{"inode differs (rebuild)", known, FileIdentity{MtimeNs: known.MtimeNs, CtimeNs: known.CtimeNs, Dev: 1, Ino: 3, Size: 4}, 1, VerdictChanged},
		{"dev differs", known, FileIdentity{MtimeNs: known.MtimeNs, CtimeNs: known.CtimeNs, Dev: 9, Ino: 2, Size: 4}, 1, VerdictChanged},
		{"size differs", known, FileIdentity{MtimeNs: known.MtimeNs, CtimeNs: known.CtimeNs, Dev: 1, Ino: 2, Size: 5}, 1, VerdictChanged},
		{"all equal, record time unknown", known, known, 0, VerdictUndecidable},
		{"all equal, inside racy window (just recorded)", known, known, known.MtimeNs + g/2, VerdictUndecidable},
		{"all equal, inside racy window (record just before stamp)", known, known, known.MtimeNs + 1, VerdictUndecidable},
		{"all equal, outside window (long after)", known, known, known.MtimeNs + g + g, VerdictSame},
		{"all equal, outside window (long before)", known, known, known.MtimeNs - g - g, VerdictSame},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, PrefilterVerdict(tc.rec, tc.cur, tc.recordAtNs))
		})
	}
}

// TestPrefilterVerdict_FiletimeZeroStaysZero pins the "zero means unknown"
// contract end to end: a zero anchor or zero stamps can never produce Same.
func TestPrefilterVerdict_FiletimeZeroStaysZero(t *testing.T) {
	known := FileIdentity{MtimeNs: 5, CtimeNs: 5, Dev: 1, Ino: 1, Size: 1}
	assert.Equal(t, VerdictUndecidable, PrefilterVerdict(FileIdentity{}, known, 1))
	assert.Equal(t, VerdictUndecidable, PrefilterVerdict(known, FileIdentity{}, 1))
	assert.Equal(t, VerdictUndecidable, PrefilterVerdict(known, known, 0))
}
