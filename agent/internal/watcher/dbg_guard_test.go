package watcher

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestNoDirectPrintInProductionCode — guard against re-introducing
// fmt.Print*/builtin print calls in non-test production code. The rework-5
// re-check found fmt.Printf("DBG …") calls left in watcher.go: they printed
// to stdout on every scan, violating CLAUDE.md's "no fmt.Println / print() in
// business code — use the logging library" rule.
//
// WHY go/parser rather than a grep-based test: a grep also matches the words
// inside comments and string literals, which would police documentation
// wording and produce false positives; a parsed AST sees only REAL call
// expressions, so the rule can be stated exactly ("a call to fmt.Print,
// fmt.Printf, fmt.Println, or the builtin print/println") with no exemptions
// to maintain. fmt.Sprintf-style formatting that RETURNS a string is not an
// output call and stays allowed.
//
// Scope: every *.go file of the agent module EXCEPT *_test.go files — tests
// may print freely.
func TestNoDirectPrintInProductionCode(t *testing.T) {
	_, thisFile, _, _ := runtime.Caller(0)
	// internal/watcher/dbg_guard_test.go → agent module root
	agentRoot := filepath.Dir(filepath.Dir(filepath.Dir(thisFile)))

	banned := map[string]bool{
		"Print": true, "Printf": true, "Println": true,
	}
	var offenders []string
	err := filepath.WalkDir(agentRoot, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if name != "." && (strings.HasPrefix(name, ".") || name == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, src, 0)
		if err != nil {
			return err
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fun := call.Fun.(type) {
			case *ast.Ident:
				// The pkg-less builtins.
				if fun.Name == "print" || fun.Name == "println" {
					offenders = append(offenders, rel(agentRoot, path)+":"+
						strconv.Itoa(fset.Position(call.Pos()).Line)+": builtin "+fun.Name)
				}
			case *ast.SelectorExpr:
				pkg, ok := fun.X.(*ast.Ident)
				if !ok || pkg.Name != "fmt" || !banned[fun.Sel.Name] {
					return true
				}
				offenders = append(offenders, rel(agentRoot, path)+":"+
					strconv.Itoa(fset.Position(call.Pos()).Line)+": fmt."+fun.Sel.Name)
			}
			return true
		})
		return nil
	})
	require.NoError(t, err)
	require.Empty(t, offenders,
		"production code must not print to stdout directly (CLAUDE.md: use the logging library); found:\n%s",
		strings.Join(offenders, "\n"))
}

// rel renders path relative to agentRoot for readable failure messages.
func rel(root, path string) string {
	r, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return r
}
