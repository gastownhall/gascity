package tmuxtest

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// TestEveryKillServerCallIsBounded pins the rule tmuxGuardCommandTimeout
// exists to enforce: no file in this package may issue "tmux ... kill-server"
// through an unbounded exec.Command. Every such call must go through
// killTmuxServerAtSocket or killTestSocketServer, which wrap it in a
// context.WithTimeout.
//
// kill-server is the one tmux verb this package aims at servers it already
// believes are unhealthy -- the orphan sweep targets that population by
// construction, and the Guard's teardown runs after a test has failed. An
// unbounded call to a wedged-but-accepting peer hangs the whole package until
// the outer go-test timeout instead of failing at the offending test, and
// every call site here passes io.Discard, so it hangs with no output.
//
// This is a source-level check because the defect it guards is a call shape,
// not a reachable behavior: the branch only fires against a server wedged in a
// way no fixture can produce on demand. It scans the package the way
// internal/testpolicy/resourcecensus scans the repository.
func TestEveryKillServerCallIsBounded(t *testing.T) {
	for _, file := range packageGoFiles(t) {
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", file, err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok || !isExecCommandCall(call) || !callArgsContain(call, "kill-server") {
				return true
			}
			t.Errorf("%s: unbounded exec.Command kill-server call; route it through killTmuxServerAtSocket (guard.go) so a wedged server cannot hang the package",
				fset.Position(call.Pos()))
			return true
		})
	}
}

// packageGoFiles returns every .go file in this package's directory. The test
// binary's working directory is the package directory, so "." is that package.
func packageGoFiles(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("reading package dir: %v", err)
	}
	var files []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".go") {
			files = append(files, filepath.Join(".", e.Name()))
		}
	}
	if len(files) == 0 {
		t.Fatal("no .go files found in package dir; scan would be vacuous")
	}
	return files
}

// isExecCommandCall reports whether call is exec.Command(...) -- the
// unbounded constructor. exec.CommandContext carries a deadline and is the
// shape this test wants callers to use.
func isExecCommandCall(call *ast.CallExpr) bool {
	sel, ok := call.Fun.(*ast.SelectorExpr)
	if !ok || sel.Sel.Name != "Command" {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	return ok && pkg.Name == "exec"
}

// callArgsContain reports whether any literal string argument of call equals
// want. It matches only literals, so a PID or socket path built at runtime is
// never mistaken for a tmux verb.
func callArgsContain(call *ast.CallExpr, want string) bool {
	for _, arg := range call.Args {
		lit, ok := arg.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			continue
		}
		got, err := strconv.Unquote(lit.Value)
		if err == nil && got == want {
			return true
		}
	}
	return false
}
