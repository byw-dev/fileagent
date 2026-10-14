package seaweedfs

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const minute = int64(time.Minute)

func TestResumeFrom(t *testing.T) {
	declared := int64(1_760_000_000_000_000_000) // 2026-10-09T12:26:40Z
	assert.Equal(t, declared-minute, ResumeFrom(declared, 60*time.Second), "default 60s margin")
	assert.Equal(t, declared, ResumeFrom(declared, 0))
	assert.Equal(t, declared-2*minute, ResumeFrom(declared, 2*time.Minute))
}

func TestResumeFromMarginExceedsDeclared(t *testing.T) {
	// M > declared: clamped at zero, never subscribe "in the future".
	assert.Equal(t, int64(0), ResumeFrom(30*minute, 60*time.Minute))
	assert.Equal(t, int64(0), ResumeFrom(60*minute, 60*time.Minute))
}

func TestResumeFromZeroCursor(t *testing.T) {
	assert.Equal(t, int64(0), ResumeFrom(0, 60*time.Second))
}

func TestResumeFromNegativeMargin(t *testing.T) {
	// A negative margin would start after the declared value and lose events;
	// it is clamped to zero.
	assert.Equal(t, int64(1_000), ResumeFrom(1_000, -time.Second))
}

func TestGapWindow(t *testing.T) {
	c := int64(1_000_000_000_000)
	h := int64(1_000_000_000_000 + 5*minute)

	from, to := GapWindow(c, h, 60*time.Second)
	assert.Equal(t, c-minute, from)
	assert.Equal(t, h, to)

	// Reversed arguments: the window must not flip when the filer clock was
	// rolled back between break and reconnect.
	from, to = GapWindow(h, c, 60*time.Second)
	assert.Equal(t, c-minute, from)
	assert.Equal(t, h, to)
}

func TestGapWindowEqualEnds(t *testing.T) {
	from, to := GapWindow(42, 42, 60*time.Second)
	assert.Equal(t, int64(42)-minute, from)
	assert.Equal(t, int64(42), to)
}

func TestGapWindowZeroMargin(t *testing.T) {
	from, to := GapWindow(10, 20, 0)
	assert.Equal(t, int64(10), from)
	assert.Equal(t, int64(20), to)
}

func TestGapWindowMarginExceedsLowEnd(t *testing.T) {
	// The formula is literal: from may go negative, which for mtime bounds
	// simply means "no lower bound". Callers that need non-negative bounds
	// clamp themselves.
	from, to := GapWindow(30*minute, 60*minute, 60*time.Minute)
	assert.Equal(t, int64(-30*minute), from)
	assert.Equal(t, 60*minute, to)
}

func TestGapWindowNegativeMargin(t *testing.T) {
	from, to := GapWindow(10, 20, -time.Second)
	assert.Equal(t, int64(10), from)
	assert.Equal(t, int64(20), to)
}

// Log names below use the format verified against SeaweedFS 4.48 source:
// weed/filer/filer_notify.go logFlushFunc —
// fmt.Sprintf("%s/%04d-%02d-%02d/%02d-%02d.%08x", SystemLogDir, ...UTC...)

func TestCursorBeforeEarliestLogFile(t *testing.T) {
	// 2026-10-15T14:23:00Z in ns (epoch 1792074180).
	nameTs := int64(1_792_074_180_000_000_000)

	before, err := CursorBeforeEarliestLog(nameTs-1, "2026-10-15/14-23.0123abcd")
	require.NoError(t, err)
	assert.True(t, before, "cursor below the earliest minute is out of range")

	before, err = CursorBeforeEarliestLog(nameTs, "2026-10-15/14-23.0123abcd")
	require.NoError(t, err)
	assert.False(t, before, "cursor at the file's own minute is in range (events at ts == name minute live in this file)")

	before, err = CursorBeforeEarliestLog(nameTs+1, "2026-10-15/14-23.0123abcd")
	require.NoError(t, err)
	assert.False(t, before)
}

func TestCursorBeforeEarliestLogDayOnly(t *testing.T) {
	midnight := int64(1_792_022_400_000_000_000) // 2026-10-15T00:00:00Z (epoch 1792022400)

	before, err := CursorBeforeEarliestLog(midnight-1, "2026-10-15")
	require.NoError(t, err)
	assert.True(t, before)

	before, err = CursorBeforeEarliestLog(midnight, "2026-10-15")
	require.NoError(t, err)
	assert.False(t, before)

	before, err = CursorBeforeEarliestLog(midnight+time.Hour.Nanoseconds(), "2026-10-15")
	require.NoError(t, err)
	assert.False(t, before)
}

func TestCursorBeforeEarliestLogNameTolerance(t *testing.T) {
	nameTs := int64(1_792_074_180_000_000_000) // 2026-10-15T14:23:00Z (epoch 1792074180)

	// Full path below SystemLogDir, leading/trailing slashes.
	for _, name := range []string{
		"/topics/.system/log/2026-10-15/14-23.0123abcd",
		"2026-10-15/14-23.0123abcd/",
		"/2026-10-15/14-23.0123abcd",
	} {
		before, err := CursorBeforeEarliestLog(nameTs-1, name)
		require.NoError(t, err, name)
		assert.True(t, before, name)
	}

	// Filer-id suffix variations (canonical is %08x, tolerate other lengths
	// and case), and absence of the suffix.
	for _, name := range []string{
		"2026-10-15/14-23",
		"2026-10-15/14-23.deadbeef",
		"2026-10-15/14-23.DEADBEEF",
		"2026-10-15/14-23.1",
	} {
		before, err := CursorBeforeEarliestLog(nameTs-1, name)
		require.NoError(t, err, name)
		assert.True(t, before, name)
	}
}

func TestCursorBeforeEarliestLogZeroCursor(t *testing.T) {
	before, err := CursorBeforeEarliestLog(0, "2026-10-15/14-23.0123abcd")
	require.NoError(t, err)
	assert.True(t, before, "zero cursor predates any log file")
}

func TestCursorBeforeEarliestLogErrors(t *testing.T) {
	for _, name := range []string{
		"",
		"   ",
		"foo",
		"2026-1-01",                 // unpadded
		"2026-13-01",                // month 13
		"2026-02-30",                // Feb 30 normalizes to March 2
		"2026-00-10",                // month 0
		"2026-01-00",                // day 0
		"2026-01-01/24-00",          // hour 24
		"2026-01-01/23-60",          // minute 60
		"2026-01-01/2359",           // missing dash
		"2026-01-01/2-05",           // unpadded hour
		"2026-01-01/23-59.",         // trailing dot, empty id
		"2026-01-01/23-59.xyz",      // non-hex filer id
		"2026-01-01/23-59.0a/extra", // unexpected extra component
	} {
		_, err := CursorBeforeEarliestLog(1, name)
		require.Error(t, err, "name %q must be rejected", name)
	}
}
