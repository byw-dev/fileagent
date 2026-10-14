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
//
// ── Scan boundary (stated honestly) ──────────────────────────────────────
//
//   - scanned: every .go file under the controlplane module root — including
//     _test.go files, build-tagged and generated files, and testdata/
//     directories (business code can explicitly import testdata packages, so
//     they are compilation-reachable);
//   - skipped: the .git/ and vendor/ subtrees (not our source), and any
//     directory carrying its own go.mod — a different Go module whose imports
//     are that module's boundary, not ours (controlplane's go.mod has no
//     replace pointing into its own subdirectories, so no reachable code is
//     hidden behind this skip);
//   - refused, fail-closed (the test goes RED): any symlink met on the walk
//     (directory or file — the scanner does not follow links while the
//     compiler follows imported ones), any file that cannot be parsed, and
//     any import literal that does not decode. A green run therefore means
//     "every scanned file was readable and both rules ran over its imports",
//     never "the unreadable parts were skipped".

import (
	"fmt"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
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

// violationsForFile parses one .go file (imports only) and returns its
// boundary violations. It is the single file-level entry point shared by the
// real-tree scan and the regression tests, so the literal decoding and the
// fail-closed behavior below cannot drift apart from the scan.
//
// Fail-closed by contract: a file that cannot be parsed, or an import literal
// that does not decode, is returned AS a violation. A green scan must mean
// "every file was readable and every rule ran", never "the unreadable parts
// were skipped" — skipping parse failures is how a violating file would
// launder itself green (rework P2-1/P2-2).
func violationsForFile(moduleRoot, path, modulePath string) []string {
	rel, err := filepath.Rel(moduleRoot, path)
	if err != nil {
		return []string{path + ": not under the module root: " + err.Error()}
	}
	relSlash := filepath.ToSlash(rel)
	fset := token.NewFileSet()
	f, perr := parser.ParseFile(fset, path, nil, parser.ImportsOnly)
	if perr != nil {
		return []string{relSlash + ": UNREADABLE (parse error: " + perr.Error() + ") — fail-closed, the boundary scan must not skip files"}
	}
	var imports []string
	var problems []string
	for _, imp := range f.Imports {
		// imp.Path.Value is the RAW Go literal INCLUDING its quotes/backquotes.
		// Trimming quote characters does not decode a literal: a raw literal
		// (backquotes) keeps its delimiters and an escaped literal keeps its
		// escapes, and both are compilable ways to smuggle a forbidden path
		// past the rules (rework P2-1). strconv.Unquote is the decoder Go
		// itself uses; a literal that will not decode is a violation, not a
		// skip.
		p, uerr := strconv.Unquote(imp.Path.Value)
		if uerr != nil {
			problems = append(problems, relSlash+": import literal "+imp.Path.Value+" does not decode ("+uerr.Error()+") — fail-closed")
			continue
		}
		imports = append(imports, p)
	}
	return append(problems, classifyFile(relSlash, imports, modulePath)...)
}

// scanSkipDirs are the only subtrees the boundary scan excludes: `.git` is
// not source, and `vendor/` is third-party code that is not OUR source tree —
// the D-037 rules govern what we write, not what we vendor. Everything else
// under the module root is scanned, INCLUDING testdata: `go list ./...` does
// not enumerate testdata packages, but business code can still import them
// explicitly, which compiles the forbidden dependency right into the module
// (the review's skip_testdata mutation proved this compiles). Scanning them
// closes that hole at zero cost (parse-only).
var scanSkipDirs = map[string]bool{".git": true, "vendor": true}

// scanTree walks every .go file under the module rooted at root and returns
// the boundary violations found (root is the module root in production and a
// synthetic tree in the regression tests below).
//
// A non-nil error means the scan itself refused to complete — a filesystem
// failure or a deliberate fail-closed refusal (a symlink on the walk, see the
// header). Callers must fail the test on it; an error is never "clean".
func scanTree(root, modulePath string) ([]string, error) {
	var problems []string
	scanned := 0
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		// Symlinks (directory or file) are refused fail-closed: WalkDir does
		// not follow them, but the compiler DOES follow explicitly imported
		// symlinked directories — a violating package behind a link would
		// compile while never being scanned (review R2). Following instead
		// would need cycle detection and cross-module logic the scanner does
		// not have; refusing keeps "green" meaningful.
		if d.Type()&fs.ModeSymlink != 0 {
			rel, rerr := filepath.Rel(root, path)
			if rerr != nil {
				rel = path
			}
			return fmt.Errorf("%s: boundary scan does not follow symlinks; replace it with a real directory/file (fail-closed)", filepath.ToSlash(rel))
		}
		if d.IsDir() {
			if scanSkipDirs[d.Name()] {
				return filepath.SkipDir
			}
			// A nested go.mod marks the root of a DIFFERENT Go module: the
			// toolchain does not treat its sources as part of this module
			// (`go list ./...` skips them; importing them needs a replace or
			// workspace entry), so its imports are that module's boundary,
			// not ours. Verified: controlplane's only replace points OUTSIDE
			// the module (../pkg/trollsift); if a replace ever points into a
			// controlplane subdirectory, this blanket skip must be revisited.
			if _, gerr := os.Lstat(filepath.Join(path, "go.mod")); gerr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".go") {
			return nil
		}
		scanned++
		problems = append(problems, violationsForFile(root, path, modulePath)...)
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	if scanned == 0 {
		return nil, fmt.Errorf("walk %s: scanned 0 Go files — the boundary scan is broken, not clean", root)
	}
	return problems, nil
}

// TestImportBoundary is the scan: every .go file under the controlplane module
// is parsed (imports only) and checked against both rules.
func TestImportBoundary(t *testing.T) {
	root := moduleRoot(t)
	modulePath := modulePathOf(t, root)

	problems, err := scanTree(root, modulePath)
	if err != nil {
		t.Fatalf("boundary scan refused: %v", err)
	}
	for _, p := range problems {
		t.Error(p)
	}
}

// TestImportBoundary_LiteralDecoding pins the P2-1 rework: import paths must
// be DECODED as Go string literals before the rules run. Raw literals
// (backquotes) and escaped literals (e.g. `seaweed\x66s`) are all legal,
// compilable Go — a scanner that only strips quote characters lets both
// smuggle a forbidden path past rules A and B. The synthetic module lives in
// a temp dir OUTSIDE the real tree so the fixture itself is never scanned by
// TestImportBoundary.
func TestImportBoundary_LiteralDecoding(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/b3rw1\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatalf("write synthetic go.mod: %v", err)
	}
	modulePath := modulePathOf(t, root)
	const forbidden = "github.com/byw-dev/fileagent/controlplane/internal/storage/seaweedfs/filerpb"
	files := map[string]string{
		// Raw string literal: the trailing backquote defeats suffix matching
		// and the leading one defeats prefix matching when quotes are merely
		// trimmed.
		"raw_literal.go": "package p\n\nimport _ `" + forbidden + "`\n",
		// Escaped string literal: `\x66` is a legal escape for 'f'.
		"escaped_literal.go": "package p\n\nimport _ \"" + strings.Replace(forbidden, "seaweedfs", `seaweed\x66s`, 1) + "\"\n",
	}
	for name, src := range files {
		if err := os.WriteFile(filepath.Join(root, name), []byte(src), 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
	}
	for name := range files {
		v := violationsForFile(root, filepath.Join(root, name), modulePath)
		if len(v) == 0 {
			t.Errorf("%s: raw/escaped import literal not flagged — the scanner must decode literals before matching rules (P2-1)", name)
			continue
		}
		if msg := strings.Join(v, "\n"); !strings.Contains(msg, "filerpb") {
			t.Errorf("%s: violations do not mention the decoded path:\n%s", name, msg)
		}
	}
}

// TestImportBoundary_NestedModuleSkipped pins the R2 rework: a directory
// under the module root that carries its own go.mod is a DIFFERENT Go module.
// The Go toolchain does not treat its sources as part of the controlplane
// module, so its imports are that module's boundary, not ours — the scan must
// skip the whole subtree instead of misreporting its files as CP violations.
// (Safe today: controlplane's go.mod has no replace pointing into its own
// subdirectories; if one ever appears, revisit — see the header.)
func TestImportBoundary_NestedModuleSkipped(t *testing.T) {
	root := t.TempDir()
	write := func(rel, content string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(root, rel)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, rel), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/rw2root\n\ngo 1.22\n")
	write("own/ok.go", "package own\n\nimport _ \"fmt\"\n")
	// An independent nested module with a rule-A violating import.
	write("review_nested/go.mod", "module example.com/review-nested\n\ngo 1.22\n")
	write("review_nested/fixture.go", "package nested\n\nimport _ \"github.com/seaweedfs/seaweedfs-client-go/filer\"\n")

	problems, err := scanTree(root, modulePathOf(t, root))
	if err != nil {
		t.Fatalf("boundary scan refused: %v", err)
	}
	for _, p := range problems {
		t.Errorf("nested-module file must not be reported as a controlplane violation: %s", p)
	}
}

// TestImportBoundary_SymlinkRefused pins the R2 rework: the scanner does not
// follow symlinks while the compiler DOES follow explicitly imported symlinked
// directories — a violating package hidden behind a link would compile but
// never be scanned. The scan therefore refuses (fail-closed) on ANY symlink
// it meets, directory or file, instead of silently walking past it.
func TestImportBoundary_SymlinkRefused(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte("module example.com/rw2root\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "own.go"), []byte("package root\n\nimport _ \"fmt\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A real directory outside the walked tree, plus a dir symlink into the
	// tree and a .go file symlink at top level.
	outside := t.TempDir()
	if err := os.WriteFile(filepath.Join(outside, "linked.go"), []byte("package linked\n\nimport _ \"fmt\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "review_link")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "linked.go"), filepath.Join(root, "linked_file.go")); err != nil {
		t.Fatal(err)
	}

	_, err := scanTree(root, modulePathOf(t, root))
	if err == nil {
		t.Fatal("scan over a symlink returned no error — symlinks must be refused fail-closed, not walked past")
	}
	msg := err.Error()
	if !strings.Contains(msg, "symlink") || !(strings.Contains(msg, "review_link") || strings.Contains(msg, "linked_file.go")) {
		// Walk order hits one of the two links first; the message must name
		// the path it refused and say why.
		t.Errorf("refusal message must name a symlink path and the reason, got: %s", msg)
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
