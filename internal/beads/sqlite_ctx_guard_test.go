package beads

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
)

// sqliteCtxGuardAllowed names the functions of the non-test sqlite*.go files
// that may call context.Background() (or its sibling context.TODO()), keyed by
// the enclosing declaration's name; each value says why. Every other function
// takes the operation context its caller derived, so no statement can run
// without the store's deadline.
var sqliteCtxGuardAllowed = map[string]string{
	"OpenSQLiteStore":           "construction-time statements run before the store exists, so there is no operation to bound",
	"opContext":                 "the one helper that derives an operation context; a nil parent becomes Background there",
	"startRetentionSweeper":     "the sweeper's own root cancel context, which CloseStore cancels through retentionStop",
	"SQLiteSequenceHighWaterAt": "an immutable read of a database file this store does not hold open; not a store operation",
}

// sqliteCtxGuardBannedMethods are the ctx-less database/sql calls: each has a
// Context twin that takes the operation context, and a statement run without
// it is not interrupted when the transaction's context is done.
var sqliteCtxGuardBannedMethods = map[string]bool{
	"Exec": true, "Query": true, "QueryRow": true, "Prepare": true, "Begin": true,
}

// sqliteCtxViolations parses src as one file and reports each banned call
// whose enclosing function is not in allowed, as "file:line: call in func".
// It walks the syntax tree, so a banned name inside a comment or a string
// neither trips nor satisfies it.
func sqliteCtxViolations(t *testing.T, filename string, src []byte, allowed map[string]string) []string {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, filename, src, 0)
	if err != nil {
		t.Fatalf("parsing %s: %v", filename, err)
	}
	var out []string
	report := func(pos token.Pos, call, enclosing string) {
		if _, ok := allowed[enclosing]; ok {
			return
		}
		out = append(out, fset.Position(pos).String()+": "+call+" in "+enclosing)
	}
	for _, decl := range file.Decls {
		enclosing := "<package scope>"
		if fn, ok := decl.(*ast.FuncDecl); ok {
			enclosing = fn.Name.Name
		}
		ast.Inspect(decl, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "context" && (sel.Sel.Name == "Background" || sel.Sel.Name == "TODO") {
				report(call.Pos(), "context."+sel.Sel.Name+"()", enclosing)
			} else if sqliteCtxGuardBannedMethods[sel.Sel.Name] {
				report(call.Pos(), "ctx-less ."+sel.Sel.Name+"()", enclosing)
			}
			return true
		})
	}
	return out
}

// sqliteCtxGuardPackageDir resolves internal/beads. Under bazel the
// runtime.Caller path is runfiles-relative, so an explicit root wins.
func sqliteCtxGuardPackageDir(t *testing.T) string {
	t.Helper()
	if root := bazeltest.OverrideRoot(); root != "" {
		return filepath.Join(root, "internal", "beads")
	}
	_, filename, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot resolve the internal/beads directory")
	}
	return filepath.Dir(filename)
}

// TestSQLiteOperationPathsCarryTheOperationContext is the G-4 guard: no
// non-test sqlite*.go function outside the allow-list calls
// context.Background() or a ctx-less Exec, Query, QueryRow, Prepare or Begin,
// so a new statement cannot slip past the store's operation deadline.
func TestSQLiteOperationPathsCarryTheOperationContext(t *testing.T) {
	dir := sqliteCtxGuardPackageDir(t)
	paths, err := filepath.Glob(filepath.Join(dir, "sqlite*.go"))
	if err != nil {
		t.Fatalf("globbing %s: %v", dir, err)
	}
	var sources []string
	for _, p := range paths {
		if !strings.HasSuffix(p, "_test.go") {
			sources = append(sources, p)
		}
	}
	if len(sources) == 0 {
		t.Fatalf("no sqlite*.go sources under %s: the guard would pass without scanning anything", dir)
	}
	var violations []string
	for _, p := range sources {
		src, err := os.ReadFile(p)
		if err != nil {
			t.Fatalf("reading %s: %v", p, err)
		}
		violations = append(violations, sqliteCtxViolations(t, filepath.Base(p), src, sqliteCtxGuardAllowed)...)
	}
	sort.Strings(violations)
	if len(violations) > 0 {
		t.Fatalf("operation paths that bypass the operation context (derive one with opContext and use the Context variant):\n  %s",
			strings.Join(violations, "\n  "))
	}
}

// TestSQLiteCtxGuardFlagsBannedCalls is the guard's negative self-test: a
// source holding each banned call outside the allow-list is flagged, one
// inside it is not, and a banned name in a comment or a string is ignored.
func TestSQLiteCtxGuardFlagsBannedCalls(t *testing.T) {
	const src = `package beads

import "context"

// context.Background() and tx.Exec(...) in a comment are not calls.
const note = "context.Background() tx.Exec(x)"

func (s *SQLiteStore) Leaky() {
	ctx := context.Background()
	_ = ctx
	s.db.Exec("DELETE FROM beads")
	s.db.Query("SELECT 1")
	s.db.QueryRow("SELECT 1")
	s.db.Prepare("SELECT 1")
	s.db.Begin()
	_ = func() { context.TODO() }
}

func (s *SQLiteStore) Bounded() {
	s.db.ExecContext(ctx, "DELETE FROM beads")
}

func opContext() { _ = context.Background() }

var packageScope = context.Background()
`
	got := sqliteCtxViolations(t, "fake.go", []byte(src), map[string]string{"opContext": "test"})
	wantCalls := []string{
		"context.Background() in Leaky",
		"ctx-less .Exec() in Leaky",
		"ctx-less .Query() in Leaky",
		"ctx-less .QueryRow() in Leaky",
		"ctx-less .Prepare() in Leaky",
		"ctx-less .Begin() in Leaky",
		"context.TODO() in Leaky",
		"context.Background() in <package scope>",
	}
	if len(got) != len(wantCalls) {
		t.Fatalf("flagged %d calls, want %d:\n  %s", len(got), len(wantCalls), strings.Join(got, "\n  "))
	}
	joined := strings.Join(got, "\n")
	for _, want := range wantCalls {
		if !strings.Contains(joined, want) {
			t.Errorf("guard did not flag %q; got:\n  %s", want, strings.Join(got, "\n  "))
		}
	}
	for _, line := range got {
		if strings.Contains(line, "Bounded") || strings.Contains(line, "in opContext") {
			t.Errorf("guard flagged an allowed or context-carrying call: %s", line)
		}
	}
}
