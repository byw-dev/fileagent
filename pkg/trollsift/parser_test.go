package trollsift

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// --- IsTrollsiftPattern ---

func TestIsTrollsiftPattern(t *testing.T) {
	assert.True(t, IsTrollsiftPattern("{agent_name}/file.csv"))
	assert.True(t, IsTrollsiftPattern("{a}"))
	assert.False(t, IsTrollsiftPattern("static/file.csv"))
	assert.False(t, IsTrollsiftPattern(""))
	assert.False(t, IsTrollsiftPattern("*.csv"))
}

// --- Globify (G-1 ~ G-9) ---

func TestGlobify(t *testing.T) {
	cases := []struct {
		id      string
		pattern string
		want    string
	}{
		{"G-1", `{agent_name}/{time:yyyy/MM/dd}/{filename}`, `*/????/??/??/*`},
		{"G-2", `{device}/{date:yyyy/MM/dd}/{seq:d}/{record_id:s}.csv`, `*/????/??/??/*/*.csv`},
		{"G-3", `{device}/{date:yyyy/MM/dd/HHmmss}.csv`, `*/????/??/??/??????.csv`},
		{"G-4", `{device:3s}/data.csv`, `???/data.csv`},
		{"G-5", `data/{date:yyyyMMdd}.csv`, `data/????????.csv`},
		{"G-6", `{device}/{date:yyyy/MM/dd}/report-{seq:d}.csv`, `*/????/??/??/report-*.csv`},
		{"G-7", `static/file.csv`, `static/file.csv`},
		{"G-8", `{n:2d}/{m:4s}/{date:yy-MM}.log`, `??/????/??-??.log`},
		{"G-9", `{device}/{seq:05d}.csv`, `*/?????.csv`},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			p, err := New(tc.pattern)
			require.NoError(t, err, "New should not error")
			assert.Equal(t, tc.want, p.Globify())
		})
	}
}

// --- Parse / Compose basic (PC-1 ~ PC-4) ---

func TestParseCompose_Basic(t *testing.T) {
	pattern := `{agent_name}/{date:yyyy/MM/dd}/{filename}`
	p, err := New(pattern)
	require.NoError(t, err)

	// PC-1: Parse
	t.Run("PC-1 Parse", func(t *testing.T) {
		vals, err := p.Parse("prod-agent/2024/03/15/data.csv")
		require.NoError(t, err)

		require.True(t, vals["agent_name"].IsStr)
		assert.Equal(t, "prod-agent", vals["agent_name"].Str)
		assert.Equal(t, "prod-agent", vals["agent_name"].Raw)

		require.True(t, vals["date"].IsTime)
		wantTime := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)
		assert.Equal(t, wantTime, vals["date"].Time)
		assert.Equal(t, "2024/03/15", vals["date"].Raw)

		require.True(t, vals["filename"].IsStr)
		assert.Equal(t, "data.csv", vals["filename"].Str)
		assert.Equal(t, "data.csv", vals["filename"].Raw)
	})

	t0 := time.Date(2024, 3, 15, 0, 0, 0, 0, time.UTC)

	// PC-2: Compose all fields
	t.Run("PC-2 Compose full", func(t *testing.T) {
		out, err := p.Compose(map[string]Value{
			"agent_name": S("a"),
			"date":       T(t0),
			"filename":   S("f.csv"),
		}, false)
		require.NoError(t, err)
		assert.Equal(t, "a/2024/03/15/f.csv", out)
	})

	// PC-3: allowPartial=true, filename missing
	t.Run("PC-3 Compose partial", func(t *testing.T) {
		out, err := p.Compose(map[string]Value{
			"agent_name": S("a"),
			"date":       T(t0),
		}, true)
		require.NoError(t, err)
		assert.Equal(t, "a/2024/03/15/{filename}", out)
	})

	// PC-4: allowPartial=false, filename missing → error
	t.Run("PC-4 Compose missing strict", func(t *testing.T) {
		_, err := p.Compose(map[string]Value{
			"agent_name": S("a"),
			"date":       T(t0),
		}, false)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "filename")
	})
}

// --- Timezone Parse (TZ-1 ~ TZ-3) ---

func TestTimezoneParse(t *testing.T) {
	cases := []struct {
		id      string
		pattern string
		input   string
		wantUTC time.Time
	}{
		{
			"TZ-1",
			`{device}/{date:yyyy/MM/dd/HHmmss|tz=Asia/Shanghai}.csv`,
			"sensor1/2024/01/01/120000.csv",
			time.Date(2024, 1, 1, 4, 0, 0, 0, time.UTC), // CST 12:00 → UTC 04:00
		},
		{
			"TZ-2",
			`{device}/{date:yyyy-MM-dd|tz=America/New_York}.csv`,
			"sensor1/2024-01-01.csv",
			time.Date(2024, 1, 1, 5, 0, 0, 0, time.UTC), // EST 00:00 → UTC 05:00
		},
		{
			"TZ-3",
			`{device}/{date:yyyy/MM/dd/HHmmss|tz=UTC}.csv`,
			"sensor1/2024/01/01/120000.csv",
			time.Date(2024, 1, 1, 12, 0, 0, 0, time.UTC),
		},
	}

	for _, tc := range cases {
		t.Run(tc.id, func(t *testing.T) {
			p, err := New(tc.pattern)
			require.NoError(t, err)

			vals, err := p.Parse(tc.input)
			require.NoError(t, err)

			require.True(t, vals["date"].IsTime)
			assert.Equal(t, tc.wantUTC, vals["date"].Time)
		})
	}
}

// --- Cross-timezone Compose ---

func TestCrossTimezoneCompose(t *testing.T) {
	parserA, err := New(`{device}/{date:yyyy/MM/dd/HHmmss|tz=Asia/Shanghai}.csv`)
	require.NoError(t, err)
	parserB, err := New(`{device}/{date:yyyy/MM/dd/HHmmss|tz=Asia/Tokyo}.csv`)
	require.NoError(t, err)

	// Parse with Shanghai → UTC internally
	valsA, err := parserA.Parse("sensor1/2024/01/01/120000.csv")
	require.NoError(t, err)
	// valsA["date"] = 2024-01-01 04:00:00 UTC

	// Compose back with A's parser (UTC 04:00 → CST 12:00 → unchanged)
	outA, err := parserA.Compose(valsA, false)
	require.NoError(t, err)
	assert.Equal(t, "sensor1/2024/01/01/120000.csv", outA)

	// Compose with B's parser (UTC 04:00 → JST 13:00)
	outB, err := parserB.Compose(valsA, false)
	require.NoError(t, err)
	assert.Equal(t, "sensor1/2024/01/01/130000.csv", outB)
}

// --- Zero-padded integer (ZP-1 ~ ZP-6) ---

func TestZeroPaddedInt(t *testing.T) {
	p, err := New(`{device}/{seq:05d}.csv`)
	require.NoError(t, err)

	// ZP-1: Globify
	t.Run("ZP-1 Globify", func(t *testing.T) {
		assert.Equal(t, "*/?????.csv", p.Globify())
	})

	// ZP-2: Parse "00001" → 1
	t.Run("ZP-2 Parse 00001", func(t *testing.T) {
		vals, err := p.Parse("sensor1/00001.csv")
		require.NoError(t, err)
		require.True(t, vals["seq"].IsInt)
		assert.Equal(t, 1, vals["seq"].Int)
		assert.Equal(t, "00001", vals["seq"].Raw)
		assert.Equal(t, "sensor1", vals["device"].Str)
	})

	// ZP-3: Parse "99999" → 99999
	t.Run("ZP-3 Parse 99999", func(t *testing.T) {
		vals, err := p.Parse("sensor1/99999.csv")
		require.NoError(t, err)
		require.True(t, vals["seq"].IsInt)
		assert.Equal(t, 99999, vals["seq"].Int)
		assert.Equal(t, "99999", vals["seq"].Raw)
	})

	// ZP-4: Compose I(1) → "00001"
	t.Run("ZP-4 Compose 1", func(t *testing.T) {
		out, err := p.Compose(map[string]Value{
			"device": S("sensor1"),
			"seq":    I(1),
		}, false)
		require.NoError(t, err)
		assert.Equal(t, "sensor1/00001.csv", out)
	})

	// ZP-5: Compose I(99999) → "99999"
	t.Run("ZP-5 Compose 99999", func(t *testing.T) {
		out, err := p.Compose(map[string]Value{
			"device": S("sensor1"),
			"seq":    I(99999),
		}, false)
		require.NoError(t, err)
		assert.Equal(t, "sensor1/99999.csv", out)
	})

	// ZP-6: Parse non-numeric → error
	t.Run("ZP-6 Parse non-numeric", func(t *testing.T) {
		_, err := p.Parse("sensor1/abc.csv")
		assert.Error(t, err)
	})
}

// --- Error / edge cases (E-1 ~ E-5) ---

func TestErrors(t *testing.T) {
	// E-1: unbalanced '{'
	t.Run("E-1 unbalanced open brace", func(t *testing.T) {
		_, err := New("{foo")
		assert.Error(t, err)
	})

	// E-2: empty field name
	t.Run("E-2 empty field name", func(t *testing.T) {
		_, err := New("{}")
		assert.Error(t, err)
	})

	// E-3: |tz= with empty value
	t.Run("E-3 empty tz value", func(t *testing.T) {
		_, err := New(`{t:yyyy-MM-dd|tz=}`)
		assert.Error(t, err)
	})

	// E-4: invalid IANA timezone (implementation may error at New or Parse/Compose)
	t.Run("E-4 invalid timezone", func(t *testing.T) {
		_, err := New(`{t:yyyy-MM-dd|tz=Invalid/Zone}`)
		// Spec allows either immediate or deferred error; we validate at New time.
		assert.Error(t, err)
	})

	// Unbalanced closing brace
	t.Run("unbalanced closing brace", func(t *testing.T) {
		_, err := New("foo}bar")
		assert.Error(t, err)
	})
}

// E-5: InjectContext does not overwrite existing keys
func TestInjectContext_NoOverwrite(t *testing.T) {
	original := S("existing")
	vals := map[string]Value{
		"agent_name": original,
	}

	ctx := AgentContext{AgentName: "new-name", AgentID: "abc-123"}
	result := InjectContext(ctx, vals)

	// agent_name must not be replaced
	assert.Equal(t, "existing", result["agent_name"].Str)
	// agent_id should be injected
	assert.Equal(t, "abc-123", result["agent_id"].Str)
}

func TestInjectContext_NilMap(t *testing.T) {
	ctx := AgentContext{AgentName: "agent1", AgentID: "id1"}
	result := InjectContext(ctx, nil)
	assert.Equal(t, "agent1", result["agent_name"].Str)
	assert.Equal(t, "id1", result["agent_id"].Str)
}

// --- Fixed-width string ---

func TestFixedWidthString(t *testing.T) {
	p, err := New(`{device:3s}/data.csv`)
	require.NoError(t, err)

	assert.Equal(t, "???/data.csv", p.Globify())

	vals, err := p.Parse("ABC/data.csv")
	require.NoError(t, err)
	assert.Equal(t, "ABC", vals["device"].Str)

	// Wrong width should not match
	_, err = p.Parse("AB/data.csv")
	assert.Error(t, err)
}

// --- Fixed-width integer (non-zero-pad) ---

func TestFixedWidthInt(t *testing.T) {
	p, err := New(`{n:2d}/{m:4s}/{date:yy-MM}.log`)
	require.NoError(t, err)

	assert.Equal(t, "??/????/??-??.log", p.Globify())

	vals, err := p.Parse("42/abcd/24-03.log")
	require.NoError(t, err)
	assert.Equal(t, 42, vals["n"].Int)
	assert.Equal(t, "abcd", vals["m"].Str)

	wantTime := time.Date(2024, 3, 1, 0, 0, 0, 0, time.UTC)
	assert.Equal(t, wantTime, vals["date"].Time)
}

// --- Pure-literal pattern (G-7) ---

func TestLiteralPattern(t *testing.T) {
	p, err := New("static/file.csv")
	require.NoError(t, err)

	assert.Equal(t, "static/file.csv", p.Globify())

	vals, err := p.Parse("static/file.csv")
	require.NoError(t, err)
	assert.Empty(t, vals)

	out, err := p.Compose(nil, false)
	require.NoError(t, err)
	assert.Equal(t, "static/file.csv", out)
}

// --- Compose partial preserves typed-field placeholders ---

func TestCompose_PartialTypedField(t *testing.T) {
	p, err := New(`{device}/{seq:05d}.csv`)
	require.NoError(t, err)

	out, err := p.Compose(map[string]Value{
		"device": S("s1"),
	}, true)
	require.NoError(t, err)
	// seq spec should be preserved in the placeholder
	assert.Equal(t, "s1/{seq:05d}.csv", out)
}

// --- IsTrollsiftPattern used as gate for Globify on plain glob ---

func TestGlobifyPlainGlob(t *testing.T) {
	// A plain glob like "*.csv" does not contain '{', so IsTrollsiftPattern returns false.
	assert.False(t, IsTrollsiftPattern("**/*.csv"))

	// Calling New on a plain pattern is still valid (treated as literal).
	p, err := New("**/*.csv")
	require.NoError(t, err)
	// Globify of a pure literal is itself.
	assert.Equal(t, "**/*.csv", p.Globify())
}

// --- fieldSpecString branches via Compose partial ---

func TestCompose_PartialAllFieldTypes(t *testing.T) {
	// kindTime (no tz) partial → fieldSpecString returns ldml only
	p, err := New(`{device}/{date:yyyy-MM-dd}/{filename}`)
	require.NoError(t, err)
	out, err := p.Compose(map[string]Value{"device": S("x")}, true)
	require.NoError(t, err)
	assert.Equal(t, "x/{date:yyyy-MM-dd}/{filename}", out)

	// kindTime (with tz) partial → fieldSpecString returns ldml|tz=...
	p2, err := New(`{device}/{date:yyyy-MM-dd|tz=Asia/Shanghai}/{filename}`)
	require.NoError(t, err)
	out2, err := p2.Compose(map[string]Value{"device": S("x")}, true)
	require.NoError(t, err)
	assert.Equal(t, "x/{date:yyyy-MM-dd|tz=Asia/Shanghai}/{filename}", out2)

	// kindInt variable (no width) partial → fieldSpecString returns "d"
	p3, err := New(`{device}/{seq:d}.csv`)
	require.NoError(t, err)
	out3, err := p3.Compose(map[string]Value{"device": S("x")}, true)
	require.NoError(t, err)
	assert.Equal(t, "x/{seq:d}.csv", out3)

	// kindInt fixed (no zero-pad) partial → fieldSpecString returns "2d"
	p4, err := New(`{device}/{seq:2d}.csv`)
	require.NoError(t, err)
	out4, err := p4.Compose(map[string]Value{"device": S("x")}, true)
	require.NoError(t, err)
	assert.Equal(t, "x/{seq:2d}.csv", out4)

	// kindStr fixed-width partial → fieldSpecString returns "3s"
	p5, err := New(`{device:3s}/data.csv`)
	require.NoError(t, err)
	out5, err := p5.Compose(nil, true)
	require.NoError(t, err)
	assert.Equal(t, "{device:3s}/data.csv", out5)
}

// --- formatValue wrong-type errors ---

func TestFormatValue_TypeMismatch(t *testing.T) {
	// Passing an int value to a string field should error.
	pStr, err := New(`{name}`)
	require.NoError(t, err)
	_, err = pStr.Compose(map[string]Value{"name": I(42)}, false)
	assert.Error(t, err)

	// Passing a string value to an int field should error.
	pInt, err := New(`{seq:d}`)
	require.NoError(t, err)
	_, err = pInt.Compose(map[string]Value{"seq": S("hello")}, false)
	assert.Error(t, err)

	// Passing a string value to a time field should error.
	pTime, err := New(`{date:yyyy-MM-dd}`)
	require.NoError(t, err)
	_, err = pTime.Compose(map[string]Value{"date": S("2024-01-01")}, false)
	assert.Error(t, err)
}

// --- Raw field (R-1 ~ R-3): verify Raw contains original matched substring ---

func TestParseRaw(t *testing.T) {
	// R-1: time field Raw holds original date string
	t.Run("R-1 time Raw", func(t *testing.T) {
		p, err := New(`{device}/{d_dt:yyyy-MM-dd}.csv`)
		require.NoError(t, err)
		vals, err := p.Parse("sensor1/2025-10-30.csv")
		require.NoError(t, err)
		require.True(t, vals["d_dt"].IsTime)
		assert.Equal(t, "2025-10-30", vals["d_dt"].Raw)
	})

	// R-2: int field Raw holds original digit string (with zero-padding)
	t.Run("R-2 int Raw", func(t *testing.T) {
		p, err := New(`{device}/{seq:d}.csv`)
		require.NoError(t, err)
		vals, err := p.Parse("sensor1/1.csv")
		require.NoError(t, err)
		require.True(t, vals["seq"].IsInt)
		assert.Equal(t, "1", vals["seq"].Raw)
	})

	// R-3: time field with tz=Asia/Shanghai Raw holds original path substring
	t.Run("R-3 time tz Raw", func(t *testing.T) {
		p, err := New(`{device}/{date:yyyy-MM-dd|tz=Asia/Shanghai}.csv`)
		require.NoError(t, err)
		vals, err := p.Parse("sensor1/2025-10-30.csv")
		require.NoError(t, err)
		require.True(t, vals["date"].IsTime)
		assert.Equal(t, "2025-10-30", vals["date"].Raw)
	})
}


// --- nil location path in ldml helpers ---

func TestLDMLHelpers_NilLoc(t *testing.T) {
now := time.Date(2024, 6, 15, 10, 30, 0, 0, time.UTC)

// parseTimeField with nil loc should default to UTC
got, err := parseTimeField("yyyy-MM-dd", "2024-06-15", nil)
require.NoError(t, err)
assert.Equal(t, time.Date(2024, 6, 15, 0, 0, 0, 0, time.UTC), got)

// formatTimeField with nil loc should default to UTC
s := formatTimeField(now, "yyyy-MM-dd", nil)
assert.Equal(t, "2024-06-15", s)
}
