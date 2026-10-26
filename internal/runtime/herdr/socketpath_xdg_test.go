package herdr

import (
	_ "embed"
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

//go:embed client.go
var clientSource string

// TestSocketPathResolvesXDGOnEveryPlatform pins the herdr socket root to
// herdr's own resolution ($XDG_CONFIG_HOME, else $HOME/.config) on every
// platform. os.UserConfigDir() only matches that on Linux; on darwin it
// ignores XDG_CONFIG_HOME and returns "$HOME/Library/Application Support",
// a directory herdr never binds a socket in, so every start times out
// (gastownhall/gascity#5770). The behavioral TestSocketPath* tests cannot
// observe that from a Linux host, so assert the resolver does not delegate
// to the platform-dependent stdlib call.
func TestSocketPathResolvesXDGOnEveryPlatform(t *testing.T) {
	f, err := parser.ParseFile(token.NewFileSet(), "client.go", clientSource, 0)
	if err != nil {
		t.Fatalf("parsing client.go: %v", err)
	}
	ast.Inspect(f, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "os" && sel.Sel.Name == "UserConfigDir" {
			t.Errorf("client.go resolves the herdr config root via os.UserConfigDir(), which on darwin ignores XDG_CONFIG_HOME and returns $HOME/Library/Application Support; herdr binds $XDG_CONFIG_HOME (else $HOME/.config)/herdr on every platform")
		}
		return true
	})
}
