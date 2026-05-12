package scheduler

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestResolvePath(t *testing.T) {
	fixed := time.Date(2024, 3, 5, 9, 7, 0, 0, time.UTC)
	cases := []struct {
		tmpl string
		want string
	}{
		// Legacy technical names
		{"/data/{yyyy}/{mm}/{dd}", "/data/2024/03/05"},
		{"/logs/{yy}{mm}{dd}", "/logs/240305"},
		{"/out/{HH}/{MM}", "/out/09/07"},
		// Friendly aliases (same values, user-facing variable names)
		{"/data/{year}/{month}/{day}", "/data/2024/03/05"},
		{"/out/{hour}/{minute}", "/out/09/07"},
		// No substitution
		{"/flat/path", "/flat/path"},
		{"no-vars", "no-vars"},
	}
	for _, tc := range cases {
		got := ResolvePath(tc.tmpl, fixed)
		assert.Equal(t, tc.want, got, "template=%q", tc.tmpl)
	}
}

func TestResolvePathWithFile(t *testing.T) {
	fixed := time.Date(2024, 3, 5, 9, 7, 0, 0, time.UTC)
	cases := []struct {
		tmpl     string
		filename string
		want     string
	}{
		{"/{year}/{month}/{filename}", "data.csv", "/2024/03/data.csv"},
		{"/{yyyy}/{mm}/{filename}", "log.txt", "/2024/03/log.txt"},
		// No {filename} in template — placeholder left unchanged when filename=""
		{"/{year}/{month}", "", "/2024/03"},
		// {filename} with empty filename — left unchanged
		{"/{year}/{filename}", "", "/2024/{filename}"},
	}
	for _, tc := range cases {
		got := ResolvePathWithFile(tc.tmpl, fixed, tc.filename)
		assert.Equal(t, tc.want, got, "template=%q filename=%q", tc.tmpl, tc.filename)
	}
}

func TestScheduler_AddAndRemoveRule(t *testing.T) {
	s := New(zap.NewNop())
	s.Start()
	defer s.Stop()

	var fired atomic.Int32

	rule := CollectionRule{
		RuleID:   "r1",
		Name:     "Test",
		CronExpr: "@every 1s", // fire every second
		Enabled:  true,
	}
	err := s.AddRule(rule, func(_ string) { fired.Add(1) })
	require.NoError(t, err)

	// Wait for at least one firing.
	require.Eventually(t, func() bool { return fired.Load() >= 1 }, 3*time.Second, 100*time.Millisecond)

	s.RemoveRule("r1")
	countBefore := fired.Load()
	time.Sleep(1500 * time.Millisecond)
	// After removal, count should not increase significantly.
	assert.LessOrEqual(t, fired.Load()-countBefore, int32(1))
}

func TestScheduler_RunOnceOnStart(t *testing.T) {
	s := New(zap.NewNop())

	var called atomic.Int32
	rule := CollectionRule{
		RuleID:         "r2",
		Enabled:        true,
		RunOnceOnStart: true,
		CronExpr:       "",
	}
	err := s.AddRule(rule, func(_ string) { called.Add(1) })
	require.NoError(t, err)
	assert.Equal(t, int32(1), called.Load())
}

func TestScheduler_RunOnceOnStart_WithCron(t *testing.T) {
	s := New(zap.NewNop())
	s.Start()
	defer s.Stop()

	var called atomic.Int32
	rule := CollectionRule{
		RuleID:         "r3",
		Enabled:        true,
		RunOnceOnStart: true,
		CronExpr:       "@yearly",
	}
	err := s.AddRule(rule, func(_ string) { called.Add(1) })
	require.NoError(t, err)
	// Should be called exactly once immediately, not by cron.
	time.Sleep(100 * time.Millisecond)
	assert.Equal(t, int32(1), called.Load())
}

func TestScheduler_InvalidCronExpr(t *testing.T) {
	s := New(zap.NewNop())
	rule := CollectionRule{
		RuleID:   "bad",
		Enabled:  true,
		CronExpr: "not-a-cron",
	}
	err := s.AddRule(rule, func(_ string) {})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid cron expr")
}

func TestScheduler_DisabledRule(t *testing.T) {
	s := New(zap.NewNop())
	var called atomic.Int32
	rule := CollectionRule{
		RuleID:         "disabled",
		Enabled:        false,
		RunOnceOnStart: true,
	}
	err := s.AddRule(rule, func(_ string) { called.Add(1) })
	require.NoError(t, err)
	assert.Equal(t, int32(0), called.Load())
}

func TestScheduler_RemoveNonExistentRule(t *testing.T) {
	s := New(zap.NewNop())
	// Should be a no-op, not panic.
	s.RemoveRule("nonexistent")
}

func TestScheduler_PathTemplateResolution(t *testing.T) {
	s := New(zap.NewNop())
	var gotPath string
	rule := CollectionRule{
		RuleID:         "r4",
		Enabled:        true,
		RunOnceOnStart: true,
		BasePath:       "/data/{yyyy}/{mm}",
		CronExpr:       "",
	}
	err := s.AddRule(rule, func(p string) { gotPath = p })
	require.NoError(t, err)

	now := time.Now().UTC()
	expected := ResolvePath("/data/{yyyy}/{mm}", now)
	assert.Equal(t, expected, gotPath)
}
