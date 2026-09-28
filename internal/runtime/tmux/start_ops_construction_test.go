package tmux

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestProductionStartOpsCarryRuntimeDir is the behavioral half of the
// dr-6siig HIGH 2 regression: the start-ops a Provider hands to its session
// entry points must carry the Provider's own runtime dir, because that field
// is the only thing that makes the start-crash and startup-nudge-unconfirmed
// diagnostics reachable.
func TestProductionStartOpsCarryRuntimeDir(t *testing.T) {
	dir := t.TempDir()
	p := NewProviderWithConfig(Config{RuntimeDir: dir})

	ops := p.startOps(runtime.Config{}, true)
	if ops.runtimeDir != dir {
		t.Fatalf("startOps runtimeDir = %q, want %q; diagnostic capture is disabled on this path", ops.runtimeDir, dir)
	}

	// And the dir it composes has to be the one gc doctor scans. Asserting
	// the field alone would still pass if the writers joined their own
	// literal, which is exactly what HIGH 1 was.
	got := citylayout.SessionDiagnosticsDirForRuntimeDir(ops.runtimeDir)
	if want := filepath.Join(dir, "sessions"); got != want {
		t.Fatalf("session diagnostics dir = %q, want %q", got, want)
	}
}

// TestNewTmuxStartOpsHasOneProductionCallSite is the structural half. The
// defect was not a wrong value, it was three hand-written argument lists where
// two omitted the runtime dir and nothing noticed: Relaunch and RunLive passed
// "" and so suppressed an unconfirmed startup nudge and then recorded it
// nowhere. A test that only checks behavior on the paths it happens to know
// about cannot catch a FOURTH call site added later with the same omission.
// This one can: it enumerates the package's non-test sources from the AST and
// requires that (*Provider).startOps is the sole production constructor.
func TestNewTmuxStartOpsHasOneProductionCallSite(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}

	fset := token.NewFileSet()
	type site struct{ file, fn string }
	var sites []site
	scanned := 0

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		scanned++

		for _, decl := range f.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok {
				continue
			}
			ast.Inspect(fn, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				id, ok := call.Fun.(*ast.Ident)
				if !ok || id.Name != "newTmuxStartOps" {
					return true
				}
				sites = append(sites, site{file: name, fn: fn.Name.Name})
				return true
			})
		}
	}

	// A zero-file scan would report a clean result for the wrong reason.
	if scanned == 0 {
		t.Fatal("scanned no non-test Go files; the guard measured nothing")
	}
	if len(sites) != 1 {
		t.Fatalf("newTmuxStartOps has %d production call site(s), want exactly 1 (in startOps): %+v\n"+
			"Every production start-ops must come from (*Provider).startOps so the runtime dir cannot be omitted; "+
			"see dr-6siig HIGH 2, where two of three hand-written call sites passed \"\".", len(sites), sites)
	}
	if sites[0].fn != "startOps" {
		t.Fatalf("newTmuxStartOps is called from %s (%s), want startOps", sites[0].fn, sites[0].file)
	}
}
