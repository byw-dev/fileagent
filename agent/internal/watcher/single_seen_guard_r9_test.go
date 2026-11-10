package watcher

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestReviewR6_SingleSeenMapInProduction — rework-9 blocker 2's guard:
// `make(map[string]seenEntry)` must appear in production code EXACTLY ONCE —
// in Start, where the ONE lifecycle seen map is created and published to the
// dirty recheck, the initial scan, the fsnotify loop and the polling loop.
// The pre-fix bug had Start build a second map (recheckSeen) while :469/:927
// built their own — three maps meant a recheck-delivered version was
// invisible to the normal path's suppression verdicts (the returned-A-lost
// scenario). A regression that introduces another `make(map[string]seenEntry)`
// fails here.
func TestReviewR6_SingleSeenMapInProduction(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	agentRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	watcherDir := filepath.Join(agentRoot, "internal", "watcher")

	occurrences := 0
	locations := []string{}
	entries, err := os.ReadDir(watcherDir)
	require.NoError(t, err)
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(filepath.Join(watcherDir, name))
		require.NoError(t, err)
		text := string(src)
		// Count real creation sites: exempt runPolling's documented
		// test-only fallback line (its preceding comment contains
		// "test-only fallback").
		lines := strings.Split(text, "\n")
		for i, line := range lines {
			if !strings.Contains(line, "make(map[string]seenEntry)") {
				continue
			}
			context := strings.Join(lines[max(0, i-6):i+1], "\n")
			if strings.Contains(context, "test-only fallback") {
				continue
			}
			occurrences++
			locations = append(locations, name+":"+itoa(i+1))
		}
	}
	require.Equal(t, 1, occurrences,
		"the lifecycle seen map must be created exactly once outside runPolling's documented test-only fallback; found %d in %v",
		occurrences, locations)
	// The one site must be Start's publish in watcher.go.
	require.Len(t, locations, 1)
	require.True(t, strings.HasPrefix(locations[0], "watcher.go:"),
		"the one seen-map creation must live in watcher.go (Start), got %v", locations)
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		return "-" + string(b)
	}
	return string(b)
}
