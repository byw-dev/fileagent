package storageevent

// boundary_test.go — import-boundary enforcement for the storage event source
// (D-037 rule 2, B3-PR1).
//
// D-037: only the SeaweedFS adapter may import SeaweedFS clients; index and
// business code only know the storageevent interfaces. Two rules guard that:
//
//	Rule A — imports under github.com/seaweedfs/ or of any path ending in
//	  /internal/storage/seaweedfs/filerpb may appear ONLY inside
//	  controlplane/internal/storage/seaweedfs/ (not even cmd/, which is
//	  wiring, not an adapter).
//	Rule B — imports of the wrapper package
//	  <module>/internal/storage/seaweedfs (or any of its subpackages) may
//	  appear only inside that tree and under controlplane/cmd/.
//
// The seaweedfs packages do not exist yet (they arrive with PR3a/PR3b). The
// test must still pass today and must catch the first violating import the
// moment one is written — go/parser only looks at syntax, so it fires on
// imports of packages that do not exist. Effectiveness is proven by mutation:
// temporarily add a violating import to a handler file and watch this test go
// red (see the B3-PR1 report for the recorded runs).

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seaweedfsPkg is the wrapper import path prefix (module path + the reserved
// directory) and seaweedfsDir its on-disk tree, both relative to the
// controlplane module root.
const (
	seaweedfsDir = "internal/storage/seaweedfs"
	cmdDir       = "cmd"
)

// moduleRoot walks up from the test's working directory (the package dir) to
// the closest directory containing go.mod — the controlplane module root.
func moduleRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod found above the package directory")
		}
		dir = parent
	}
}

// modulePathOf reads the module path from go.mod (not `go list -m`, which in
// go.work mode prints every workspace module).
func modulePathOf(t *testing.T, root string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.TrimSpace(strings.TrimPrefix(line, "module "))
		}
	}
	t.Fatal("go.mod has no module directive")
	return ""
}

// ruleAViolated reports whether import path p is a Rule A subject.
func ruleAViolated(p string) bool {
	return strings.HasPrefix(p, "github.com/seaweedfs/") ||
		strings.HasSuffix(p, "/internal/storage/seaweedfs/filerpb")
}

// ruleBViolated reports whether import path p is a Rule B subject (the
// seaweedfs wrapper package or any subpackage of it).
func ruleBViolated(p, modulePath string) bool {
	wrapper := modulePath + "/" + seaweedfsDir
	return p == wrapper || strings.HasPrefix(p, wrapper+"/")
}

// classifyFile evaluates both rules for one file. relSlash is the file's path
// relative to the module root with forward slashes; imports are the parsed
// import paths. The returned slice holds human-readable violation lines.
func classifyFile(relSlash string, imports []string, modulePath string) []string {
	inTree := strings.HasPrefix(relSlash, seaweedfsDir+"/")
	inCmd := strings.HasPrefix(relSlash, cmdDir+"/")
	var problems []string
	for _, p := range imports {
		// Rule A subjects inside the seaweedfs tree are the adapter itself.
		if ruleAViolated(p) && !inTree {
			problems = append(problems,
				"rule A: "+relSlash+" imports "+p+
					" (only "+seaweedfsDir+"/ may import SeaweedFS clients)")
		}
		// Rule B subjects may additionally be imported from cmd/ (wiring).
		if ruleBViolated(p, modulePath) && !inTree && !inCmd {
			problems = append(problems,
				"rule B: "+relSlash+" imports "+p+
					" (only "+seaweedfsDir+"/ and "+cmdDir+"/ may import the adapter)")
		}
	}
	return problems
}

// TestImportBoundary is the scan: every .go file under the controlplane module
// is parsed (imports only) and checked against both rules.
func TestImportBoundary(t *testing.T) {
	root := moduleRoot(t)
	modulePath := modulePathOf(t, root)

	skipDirs := map[string]bool{".git": true, "vendor": true, "testdata": true, "node_modules": true, "bin": true}
	var problems []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if skipDirs[d.Name()] && path != root {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
		if perr != nil {
			// A file that does not parse cannot be compiled into the module,
			// so it cannot violate the real (compiler-enforced) boundary.
			t.Logf("skipping unparseable file %s: %v", path, perr)
			return nil
		}
		scanned++
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			return rerr
		}
		var imports []string
		for _, imp := range f.Imports {
			imports = append(imports, strings.Trim(imp.Path.Value, `"`))
		}
		problems = append(problems, classifyFile(filepath.ToSlash(rel), imports, modulePath)...)
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	if scanned == 0 {
		t.Fatal("scanned 0 Go files — the boundary scan is broken, not clean")
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// TestClassifyFile pins the rule logic on synthetic samples so the rules are
// tested independently of the current repository contents (which today
// contain no seaweedfs import at all).
func TestClassifyFile(t *testing.T) {
	const modulePath = "github.com/byw-dev/fileagent/controlplane"
	tests := []struct {
		name    string
		rel     string
		imports []string
		want    int // number of expected violation lines
	}{
		{"handler importing upstream client", "internal/api/handler/events.go",
			[]string{"github.com/seaweedfs/seaweedfs-client-go/filer"}, 1},
		{"handler importing filerpb by suffix", "internal/api/handler/events.go",
			[]string{"example.com/fork/internal/storage/seaweedfs/filerpb"}, 1},
		{"handler importing the wrapper", "internal/api/handler/events.go",
			[]string{modulePath + "/internal/storage/seaweedfs"}, 1},
		{"handler importing a wrapper subpackage", "internal/indexer/indexer.go",
			[]string{modulePath + "/internal/storage/seaweedfs/iam"}, 1},
		{"cmd wiring the wrapper is allowed", "cmd/server/main.go",
			[]string{modulePath + "/internal/storage/seaweedfs"}, 0},
		{"cmd importing the upstream client is NOT allowed", "cmd/server/main.go",
			[]string{"github.com/seaweedfs/seaweedfs-client-go/filer"}, 1},
		{"adapter importing the upstream client", "internal/storage/seaweedfs/adapter.go",
			[]string{"github.com/seaweedfs/seaweedfs-client-go/filer"}, 0},
		{"adapter importing its own sibling", "internal/storage/seaweedfs/adapter.go",
			[]string{modulePath + "/internal/storage/seaweedfs/filerpb"}, 0},
		{"unrelated stdlib import", "internal/api/router.go",
			[]string{"go/parser", "github.com/byw-dev/fileagent/controlplane/internal/indexer"}, 0},
		{"prefix lookalike does not match", "internal/api/handler/events.go",
			[]string{"github.com/seaweedfs-extra/foo", modulePath + "/internal/storage/seaweedfsx"}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyFile(tt.rel, tt.imports, modulePath)
			if len(got) != tt.want {
				t.Errorf("classifyFile(%s, %v) = %d problems (%v), want %d",
					tt.rel, tt.imports, len(got), got, tt.want)
			}
		})
	}
}
