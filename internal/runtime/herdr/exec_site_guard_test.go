package herdr

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// execSiteAllowList names the only functions in this package's non-test source
// that may start a process with exec.Command or exec.CommandContext, keyed
// "file:function". The pre_start shell is not here: it runs through the
// runtime package's setup-command helper, not an exec call in this package. Everything else goes through the bounded runner, so a new
// herdr verb cannot reintroduce an exec with no deadline (ga-ikd8gj).
var execSiteAllowList = map[string]string{
	"client.go:runWithin":    "the bounded runner every herdr CLI verb goes through",
	"client.go:launchServer": "the detached session-server launch (released, never waited on)",
	"attach_other.go:Attach": "interactive `agent attach`: runs until the user detaches",
}

// AT-H10: no exec.Command or exec.CommandContext call outside the allow-list.
func TestExecSitesAreAllowListed(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]bool{}
	scanned := 0
	fset := token.NewFileSet()
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		scanned++
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				call, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				sel, ok := call.Fun.(*ast.SelectorExpr)
				if !ok {
					return true
				}
				pkg, ok := sel.X.(*ast.Ident)
				if ok && pkg.Name == "exec" && (sel.Sel.Name == "Command" || sel.Sel.Name == "CommandContext") {
					found[name+":"+fd.Name.Name] = true
				}
				return true
			})
		}
	}
	if scanned == 0 {
		t.Fatal("no non-test sources in the test's working directory: the scan would pass vacuously (BUILD.bazel data glob?)")
	}
	var bad []string
	for site := range found {
		if _, ok := execSiteAllowList[site]; !ok {
			bad = append(bad, site)
		}
	}
	sort.Strings(bad)
	if len(bad) > 0 {
		t.Fatalf("exec.Command/CommandContext outside the allow-list (route it through runWithin): %v", bad)
	}
	for site := range execSiteAllowList {
		if !found[site] {
			t.Errorf("allow-list entry %s no longer starts a process: remove it", site)
		}
	}
}
