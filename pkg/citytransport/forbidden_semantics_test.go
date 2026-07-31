package citytransport

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// CITY-SHELL-2: the transport shell must contain no policy decision, no
// source-order decision, no checkpoint advancement, and no backfill mapping.
// The shell is the layer that can most easily grow one of those by accident —
// it is the only layer that touches both the mapped offers and the network — so
// the prohibition is enforced mechanically against this package's own source
// rather than by review.
//
// The scanner is deliberately identifier-based rather than comment-based, and
// it runs over the AST so a word appearing in a doc comment (this file's own
// prose, for one) cannot trip it.

var forbiddenIdents = map[string]string{
	"Checkpoint":  "checkpoint advancement belongs to pkg/citysource",
	"checkpoint":  "checkpoint advancement belongs to pkg/citysource",
	"Watermark":   "watermark state belongs to pkg/citysource",
	"HighWater":   "high-water state belongs to pkg/citysource",
	"highWater":   "high-water state belongs to pkg/citysource",
	"Backfill":    "backfill mapping belongs to pkg/citysource",
	"backfill":    "backfill mapping belongs to pkg/citysource",
	"Cursor":      "cursor state belongs to pkg/citysource",
	"cursor":      "cursor state belongs to pkg/citysource",
	"IsAllowed":   "the type allowlist is a policy decision",
	"allowedType": "the type allowlist is a policy decision",
	"emitContent": "the content opt-in is a signed-policy decision",
	"EmitContent": "the content opt-in is a signed-policy decision",
}

// forbiddenImports would each drag a decision into the shell. sort in
// particular: any ordering this package performed would be a source-order
// decision, and the offers it is handed are already in source order.
var forbiddenImports = []string{
	"sort",
	"github.com/gastownhall/gascity/pkg/citysource",
	"github.com/gastownhall/gascity/pkg/eventexport",
	"github.com/gastownhall/gascity/internal/events",
	"github.com/gastownhall/gascity/internal/eventfeed",
}

func TestShellContainsNoDecisionSemantics(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		// Scan the shipped shell only. Test files legitimately name the
		// forbidden concepts in order to assert their absence.
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	if len(pkgs) == 0 {
		t.Fatal("scanner found no non-test source; it would pass vacuously")
	}

	scanned := 0
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			scanned++
			base := filepath.Base(name)

			for _, imp := range file.Imports {
				path := strings.Trim(imp.Path.Value, `"`)
				for _, bad := range forbiddenImports {
					if path == bad {
						t.Errorf("%s imports %q: the shell must make no decision that import enables", base, bad)
					}
				}
			}

			// A wire DTO may NAME a forbidden concept — the server reports its
			// own watermark as ACK_STALE evidence, and the shell has to carry
			// it. What the shell may not do is hold or act on one. So field
			// names inside the wire DTOs are exempt; every other occurrence,
			// including any local variable or any read of such a field, is not.
			exempt := wireDTOFieldIdents(file)

			ast.Inspect(file, func(n ast.Node) bool {
				id, ok := n.(*ast.Ident)
				if !ok {
					return true
				}
				if exempt[id] {
					return true
				}
				if why, bad := forbiddenIdents[id.Name]; bad {
					t.Errorf("%s references %q at %s: %s",
						base, id.Name, fset.Position(id.Pos()), why)
				}
				return true
			})
		}
	}
	if scanned < 3 {
		t.Fatalf("scanned only %d files; expected the whole shell", scanned)
	}
}

// wireDTOSchema is the closed set of types that describe bytes on the wire.
var wireDTOSchema = map[string]bool{
	"Offer": true, "Upload": true, "Result": true, "Ack": true, "Problem": true,
}

// wireDTOFieldIdents returns the field-name identifiers declared inside the
// wire DTO types, which are exempt from the identifier ban.
func wireDTOFieldIdents(file *ast.File) map[*ast.Ident]bool {
	exempt := map[*ast.Ident]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		ts, ok := n.(*ast.TypeSpec)
		if !ok || !wireDTOSchema[ts.Name.Name] {
			return true
		}
		st, ok := ts.Type.(*ast.StructType)
		if !ok {
			return true
		}
		for _, f := range st.Fields.List {
			for _, name := range f.Names {
				exempt[name] = true
			}
		}
		return true
	})
	return exempt
}

// The shell must not decide content: nothing in it may assign Title or Formula.
// It carries them because the mapper already decided, under a signed policy.
func TestShellNeverAssignsContentFields(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}
	for _, pkg := range pkgs {
		for name, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok {
					return true
				}
				for _, lhs := range assign.Lhs {
					sel, ok := lhs.(*ast.SelectorExpr)
					if !ok {
						continue
					}
					if sel.Sel.Name == "Title" || sel.Sel.Name == "Formula" {
						t.Errorf("%s:%s assigns %s: content is the mapper's signed decision, not the shell's",
							filepath.Base(name), fset.Position(assign.Pos()), sel.Sel.Name)
					}
				}
				return true
			})
		}
	}
}
