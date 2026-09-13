package trollsift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// Cross-alias matrix (review P1-A): time and submit_time are aliases, not two
// independent fields. When exactly one side was parsed out of path_pattern,
// the parsed value must be mirrored to the missing alias — otherwise migrating
// a rule from {time} to {submit_time} (exactly what the deprecation hint
// advises) would silently change the object key from the data date to the
// upload instant. When both sides were parsed, each keeps its own value: the
// template composes only the fields it references, and force-mirroring two
// independent parse results would fabricate a value neither asked for.
// When neither was parsed, both get the submit instant.
func TestInjectSubmitTime_AliasMatrix(t *testing.T) {
	at := time.Date(2026, 9, 13, 8, 0, 0, 0, time.UTC)
	dataDate := time.Date(2010, 10, 12, 8, 0, 0, 0, time.UTC)

	t.Run("parsed time only: mirrored to submit_time", func(t *testing.T) {
		fields := InjectSubmitTime(map[string]Value{"time": T(dataDate)}, at)
		assert.Equal(t, dataDate, fields["submit_time"].Time,
			"parsed time must be mirrored to the missing submit_time alias")
		assert.Equal(t, dataDate, fields["time"].Time)
	})

	t.Run("parsed submit_time only: mirrored to time", func(t *testing.T) {
		fields := InjectSubmitTime(map[string]Value{"submit_time": T(dataDate)}, at)
		assert.Equal(t, dataDate, fields["time"].Time,
			"parsed submit_time must be mirrored to the missing time alias")
		assert.Equal(t, dataDate, fields["submit_time"].Time)
	})

	t.Run("both parsed: each keeps its own value, no cross-overwrite", func(t *testing.T) {
		other := time.Date(1999, 1, 2, 3, 0, 0, 0, time.UTC)
		fields := InjectSubmitTime(map[string]Value{
			"time":        T(dataDate),
			"submit_time": T(other),
		}, at)
		assert.Equal(t, dataDate, fields["time"].Time)
		assert.Equal(t, other, fields["submit_time"].Time,
			"two independently parsed values must not be mirrored onto each other")
	})

	t.Run("neither parsed: both get the submit instant", func(t *testing.T) {
		fields := InjectSubmitTime(map[string]Value{}, at)
		assert.Equal(t, at, fields["time"].Time)
		assert.Equal(t, at, fields["submit_time"].Time)
	})
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

// Bare reserved time fields ({submit_time} / {time} without LDML) cannot
// compose: Go types a format-less field as string while the injected value is
// a time (review P1-B, measured: "expects a string value"). The bare form is
// therefore forbidden and CP rule create/update must warn on it.
func TestUsesBareReservedTimeField(t *testing.T) {
	assert.True(t, UsesBareReservedTimeField("{submit_time}/{filename}"))
	assert.True(t, UsesBareReservedTimeField("/{agent_name}/{time}/{filename}"))
	assert.False(t, UsesBareReservedTimeField("{submit_time:yyyy/MM/dd}/{filename}"))
	assert.False(t, UsesBareReservedTimeField("{time:yyyy|tz=Asia/Shanghai}/{filename}"))
	assert.False(t, UsesBareReservedTimeField("/{agent_name}/{time_zone}/{filename}"))
	assert.False(t, UsesBareReservedTimeField("/{filename}"))
	assert.False(t, UsesBareReservedTimeField(""))
}

// The bare form really does fail to compose with the injected time value —
// pinning the failure that motivates the bare-form ban.
func TestBareReservedTimeField_CannotCompose(t *testing.T) {
	now := time.Date(2026, 9, 13, 8, 0, 0, 0, time.UTC)
	fields := InjectSubmitTime(map[string]Value{}, now)
	p, err := New("{submit_time}/{filename}")
	require.NoError(t, err)
	_, err = p.Compose(fields, false)
	assert.Error(t, err, "bare {submit_time} must not silently compose")
}