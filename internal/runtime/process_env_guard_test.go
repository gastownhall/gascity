package runtime

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// bareEnvironAllowed lists the runtime files that may read os.Environ()
// directly, each with the reason its read cannot leave a projection in a child
// that outlives it.
var bareEnvironAllowed = map[string]string{
	// RunSetupCommand has no callers (#5637), and this package must stay
	// stdlib-only, so it cannot reach execenv.
	"setupcommand.go": "unwired setup runner in the stdlib-only contract package",
	// mergeCurrentEnv fills this process's own identity into a process-table
	// scan of itself; it starts nothing.
	"proctable/scan_linux.go": "self identity merge for the process-table scan",
	// processSnapshotCommand's ps reads no BEADS_* or BD_* variable and exits
	// before the state-cache refresh returns. Waiting out a projection there
	// would stall every liveness read on a stale cache for a whole open.
	"tmux/state_cache.go": "one-shot ps process snapshot on the liveness path",
}

// TestRuntimeProvidersBuildChildEnvThroughExecenv keeps every runtime provider
// building a child's environment through execenv.Environ. A bare os.Environ()
// read can land inside a native beads store open, which projects its scope's
// BEADS_* variables (and withholds BD_*) process-wide for the whole open, and
// a session spawned from that read keeps the projection for its whole life.
func TestRuntimeProvidersBuildChildEnvThroughExecenv(t *testing.T) {
	runtimeDir := filepath.Join(moduleRootForGuard(t), "internal", "runtime")
	fset := token.NewFileSet()
	scanned := 0
	err := filepath.WalkDir(runtimeDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path != runtimeDir && (strings.HasPrefix(d.Name(), ".") || d.Name() == "testdata") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		rel, err := filepath.Rel(runtimeDir, path)
		if err != nil {
			return err
		}
		scanned++
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		osName := ""
		for _, imp := range file.Imports {
			if strings.Trim(imp.Path.Value, `"`) != "os" {
				continue
			}
			osName = "os"
			if imp.Name != nil {
				osName = imp.Name.Name
			}
		}
		if osName == "" {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok || sel.Sel.Name != "Environ" {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); !ok || pkg.Name != osName {
				return true
			}
			if _, allowed := bareEnvironAllowed[filepath.ToSlash(rel)]; !allowed {
				t.Errorf("%s: os.Environ() builds a child environment outside execenv.Environ; read it through execenv.Environ so an in-flight native beads store open cannot leak into the child", fset.Position(call.Pos()))
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// The scan must reach the provider subpackages, not just this one.
	if _, err := os.Stat(filepath.Join(runtimeDir, "subprocess", "subprocess.go")); err != nil || scanned == 0 {
		t.Fatalf("guard scanned %d files and cannot see the subprocess provider (%v); the test needs //:repo_go_srcs", scanned, err)
	}
}

// moduleRootForGuard walks up to the directory holding go.mod: the source tree
// under go test, the runfiles tree (//:go.mod, //:repo_go_srcs) under Bazel.
func moduleRootForGuard(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the test's working directory")
		}
		dir = parent
	}
}
