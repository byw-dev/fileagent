package queue

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestIC48_CandidateCallSitesRouteThroughDecideProcessed is an AST guard for
// the IC-BUG-48 wiring: the two candidate dedup call sites (main.go's
// submitFile, executor.go's processTask) must route through
// DecideProcessed — the legacy bare-tuple IsProcessed must NOT appear in
// either file's non-test production code. Without this guard a future
// refactor could quietly reintroduce the tuple-only shortcut and reopen the
// same-second/rebuild blind spots.
func TestIC48_CandidateCallSitesRouteThroughDecideProcessed(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	// internal/queue/…_test.go → agent module root
	agentRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))

	cases := []struct {
		file            string
		mustUse         string
		mustNotUse      string
		mustNotUseCount int // allowed occurrences of the forbidden selector (0 here)
	}{
		{
			file:            filepath.Join(agentRoot, "cmd", "agent", "main.go"),
			mustUse:         "DecideProcessed",
			mustNotUse:      "IsProcessed",
			mustNotUseCount: 0,
		},
		{
			file:            filepath.Join(agentRoot, "internal", "executor", "executor.go"),
			mustUse:         "DecideProcessed",
			mustNotUse:      "IsProcessed",
			mustNotUseCount: 0,
		},
		// reports.go must write the ledger through CompleteReportedIdentity
		// (the uploader's same-fd identity), not the identity-less shortcut.
		{
			file:            filepath.Join(agentRoot, "internal", "executor", "reports.go"),
			mustUse:         "CompleteReportedIdentity",
			mustNotUse:      "CompleteReported(",
			mustNotUseCount: 0,
		},
	}

	for _, tc := range cases {
		t.Run(filepath.Base(tc.file), func(t *testing.T) {
			src, err := os.ReadFile(tc.file)
			require.NoError(t, err)
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, tc.file, src, 0)
			require.NoError(t, err)

			uses := map[string]int{}
			ast.Inspect(f, func(n ast.Node) bool {
				sel, ok := n.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				uses[sel.Sel.Name]++
				return true
			})
			assert.GreaterOrEqual(t, uses[tc.mustUse], 1,
				"%s must route the candidate dedup through %s", tc.file, tc.mustUse)
			// Also enforce on raw text for the parenthesised-call form the
			// AST check above may miss via aliases.
			raw := string(src)
			if tc.mustNotUseCount == 0 {
				assert.NotContains(t, raw, tc.mustNotUse+"(",
					"%s must not call the bare-tuple shortcut %s", tc.file, tc.mustNotUse)
			}
		})
	}
}

// TestIC48_UploadFileCarriesIdentity pins the uploader-side half of the
// wiring: UploadFile must populate result.Identity from the same-fd stat and
// set IdentityComplete only when the post-read re-stat matched. AST guard on
// uploader.go (string-level; the behaviour tests live in the uploader
// package).
func TestIC48_UploadFileCarriesIdentity(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	agentRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))
	src, err := os.ReadFile(filepath.Join(agentRoot, "internal", "uploader", "uploader.go"))
	require.NoError(t, err)
	raw := string(src)
	for _, marker := range []string{
		"fileid.StatHandle(f)",          // pre-read same-fd snapshot
		"result.IdentityComplete = true", // post-read verification writes it
		"SnapAtNs: handleID.SnapAtNs",    // the racy anchor travels with the identity
	} {
		assert.True(t, strings.Contains(raw, marker),
			"uploader.go must contain %q (IC-BUG-48 same-fd snapshot wiring)", marker)
	}
}
