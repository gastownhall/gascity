package scripts_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"testing"
)

// TestHerdrXDGSocketPathTestsSkipOnNonLinuxPlatforms guards a coupled fix
// for ga-1qp5qo: os.UserConfigDir() ignores XDG_CONFIG_HOME on darwin and
// always resolves under "$HOME/Library/Application Support", so
// TestSocketPathHonorsXDGConfigHomeOverHome and
// TestSocketPathFallsBackToHomeConfigWhenXDGUnset must skip on non-Linux
// platforms rather than assert an XDG/.config-rooted path unconditionally.
// They pass today only because macOS CI never actually runs them; restoring
// macOS coverage without this guard turns mac red. Cherry-picked from
// 7a8fb35f441f7e45283b2558e2089f2d9c416ddb (closed PR #5435).
func TestHerdrXDGSocketPathTestsSkipOnNonLinuxPlatforms(t *testing.T) {
	repoRoot := repoRoot(t)
	path := filepath.Join(repoRoot, "internal", "runtime", "herdr", "client_test.go")

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}

	const guard = "skipUnlessXDGPlatform"
	wantCallers := map[string]bool{
		"TestSocketPathHonorsXDGConfigHomeOverHome":       false,
		"TestSocketPathFallsBackToHomeConfigWhenXDGUnset": false,
	}

	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok {
			continue
		}
		if _, tracked := wantCallers[fn.Name.Name]; !tracked || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == guard {
				wantCallers[fn.Name.Name] = true
			}
			return true
		})
	}

	for name, called := range wantCallers {
		if !called {
			t.Errorf("%s must call %s before asserting an XDG/.config-rooted socket path, since os.UserConfigDir() ignores XDG_CONFIG_HOME on darwin (cherry-pick 7a8fb35f441f7e45283b2558e2089f2d9c416ddb)", name, guard)
		}
	}
}
