package convergence

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
)

// Every convergence.* constant declared in events.go is controller-authored,
// so a new one cannot default to worker-authored unnoticed.
func TestEveryConvergenceEventTypeIsControllerAuthored(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "events.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing events.go: %v", err)
	}
	var declared int
	ast.Inspect(file, func(n ast.Node) bool {
		lit, ok := n.(*ast.BasicLit)
		if !ok || lit.Kind != token.STRING {
			return true
		}
		v, err := strconv.Unquote(lit.Value)
		if err != nil || !strings.HasPrefix(v, "convergence.") {
			return true
		}
		declared++
		if a, ok := events.RegisteredAuthorship(v); !ok || a != events.AuthorController {
			t.Errorf("%s: authorship %q (registered %v), want controller", v, a, ok)
		}
		return true
	})
	if declared != len(eventTypes) {
		t.Fatalf("events.go declares %d convergence types, eventTypes lists %d", declared, len(eventTypes))
	}
}
