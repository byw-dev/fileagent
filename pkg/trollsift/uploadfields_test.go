package trollsift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

// IC-BUG-50: submit_time is the declared reserved word for the moment the file
// was submitted for upload; time is its deprecated alias.
func TestInjectSubmitTime_InjectsWhenAbsent(t *testing.T) {
	now := time.Date(2026, 9, 13, 8, 0, 0, 0, time.UTC)

	fields := InjectSubmitTime(map[string]Value{}, now)
	st, ok := fields["submit_time"]
	assert.True(t, ok, "submit_time must be injected")
	assert.True(t, st.IsTime)
	assert.Equal(t, now, st.Time)

	// Deprecated alias keeps legacy {time} templates rendering.
	tv, ok := fields["time"]
	assert.True(t, ok, "deprecated time alias must be injected")
	assert.Equal(t, now, tv.Time)
}

// Parse-first priority, same rule as agent_name/agent_id in InjectContext
// (TestInjectContext_NoOverwrite): a field parsed out of path_pattern must
// never be overwritten by the injection.
func TestInjectSubmitTime_ParsedFieldWins(t *testing.T) {
	now := time.Date(2026, 9, 13, 8, 0, 0, 0, time.UTC)
	parsed := time.Date(2010, 10, 12, 8, 0, 0, 0, time.UTC)

	fields := InjectSubmitTime(map[string]Value{
		"submit_time": T(parsed),
		"time":        T(parsed),
	}, now)

	assert.Equal(t, parsed, fields["submit_time"].Time, "parsed submit_time must win")
	assert.Equal(t, parsed, fields["time"].Time, "parsed time (legacy alias) must win")
}

// A field that merely shares a prefix with the reserved word (time_zone) is a
// normal parsed field and is never touched.
func TestInjectSubmitTime_TimeZonePrefixNotClobbered(t *testing.T) {
	now := time.Date(2026, 9, 13, 8, 0, 0, 0, time.UTC)

	fields := InjectSubmitTime(map[string]Value{
		"time_zone": S("UTC+8"),
	}, now)

	assert.Equal(t, "UTC+8", fields["time_zone"].Str)
}

// The deprecated {time} token is detected with a word boundary: {time_zone}
// is NOT a use of the reserved word.
func TestUsesDeprecatedTimeField(t *testing.T) {
	assert.True(t, UsesDeprecatedTimeField("{time:yyyy/MM/dd}/{filename}"))
	assert.True(t, UsesDeprecatedTimeField("/{agent_name}/{time}/{filename}"))
	assert.False(t, UsesDeprecatedTimeField("/{agent_name}/{time_zone}/{filename}"))
	assert.False(t, UsesDeprecatedTimeField("/{submit_time:yyyy/MM/dd}/{filename}"))
	assert.False(t, UsesDeprecatedTimeField("/{filename}"))
	assert.False(t, UsesDeprecatedTimeField(""))
}