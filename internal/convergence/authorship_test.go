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

// Every convergence.* string constant declared in events.go is
// controller-authored, so a new one cannot default to worker-authored
// unnoticed. Only constant declarations count, so an unrelated string literal
// in the file cannot skew the tally.
func TestEveryConvergenceEventTypeIsControllerAuthored(t *testing.T) {
	file, err := parser.ParseFile(token.NewFileSet(), "events.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing events.go: %v", err)
	}
	var declared int
	for _, decl := range file.Decls {
		gen, ok := decl.(*ast.GenDecl)
		if !ok || gen.Tok != token.CONST {
			continue
		}
		for _, spec := range gen.Specs {
			for _, value := range spec.(*ast.ValueSpec).Values {
				lit, ok := value.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					continue
				}
				v, err := strconv.Unquote(lit.Value)
				if err != nil || !strings.HasPrefix(v, "convergence.") {
					continue
				}
				declared++
				if a, ok := events.RegisteredAuthorship(v); !ok || a != events.AuthorController {
					t.Errorf("%s: authorship %q (registered %v), want controller", v, a, ok)
				}
			}
		}
	}
	if declared != len(eventTypes) {
		t.Fatalf("events.go declares %d convergence types, eventTypes lists %d", declared, len(eventTypes))
	}
}
