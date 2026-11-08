package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/bazeltest"
	"github.com/gastownhall/gascity/internal/events"
)

const gcModulePath = "github.com/gastownhall/gascity"

// TestEverySDKEventTypeHasAnAuthorship is the module-wide authorship guard.
// internal/events checks the types it declares; this checks every type the
// SDK records anywhere: each events.Event{Type: ...} literal in non-test
// source whose Type is a string literal or a package-level string constant
// must name a type with a registered authorship. An unregistered type
// defaults to Worker, which a worker site may record, so a controller-only
// type declared outside events.go (breaker.state_changed, a future
// ops.recording_degraded) would otherwise be forgeable from a box unnoticed.
// It runs in cmd/gc because only the gc binary links every package that
// registers authorship from init. A Type set from a variable or a parameter
// (an emit helper) is not resolved; the constant the helper is called with
// is then covered only if some literal names it too.
func TestEverySDKEventTypeHasAnAuthorship(t *testing.T) {
	root := bazeltest.RepoRoot(t)
	r := &eventTypeResolver{root: root, consts: map[string]map[string]constDecl{}}
	uses := r.eventTypeUses(t)
	if len(uses) < 40 {
		t.Fatalf("resolved %d event types under %s; the scan is broken (are //:repo_go_srcs in the test's data?)", len(uses), root)
	}
	var missing []string
	for eventType, where := range uses {
		if _, ok := events.RegisteredAuthorship(eventType); !ok {
			missing = append(missing, eventType+" (recorded at "+where+")")
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing)
		t.Fatalf("event types the SDK records with no registered authorship; register each beside its declaration (events.RegisterAuthorship from init) after deciding its class (DES-BX D13):\n  %s", strings.Join(missing, "\n  "))
	}
}

type constDecl struct {
	expr ast.Expr
	file *ast.File
}

type eventTypeResolver struct {
	root   string
	consts map[string]map[string]constDecl // package dir -> const name -> declaration
}

// eventTypeUses maps every event type a non-test events.Event literal names
// to one place that names it.
func (r *eventTypeResolver) eventTypeUses(t *testing.T) map[string]string {
	t.Helper()
	uses := map[string]string{}
	err := filepath.WalkDir(r.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			name := d.Name()
			if path != r.root && (strings.HasPrefix(name, ".") || strings.HasPrefix(name, "bazel-") || name == "testdata" || name == "node_modules" || name == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		dir := filepath.Dir(path)
		eventsName := importName(file, gcModulePath+"/internal/events")
		inEvents := file.Name.Name == "events" && strings.HasSuffix(filepath.ToSlash(dir), "internal/events")
		if eventsName == "" && !inEvents {
			return nil
		}
		ast.Inspect(file, func(n ast.Node) bool {
			lit, ok := n.(*ast.CompositeLit)
			if !ok || !isEventType(lit.Type, eventsName, inEvents) {
				return true
			}
			for _, elt := range lit.Elts {
				kv, ok := elt.(*ast.KeyValueExpr)
				if !ok {
					continue
				}
				if key, ok := kv.Key.(*ast.Ident); !ok || key.Name != "Type" {
					continue
				}
				if v, ok := r.resolve(t, dir, file, kv.Value, 0); ok {
					rel, _ := filepath.Rel(r.root, path)
					uses[v] = rel
				}
			}
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("scanning %s: %v", r.root, err)
	}
	return uses
}

func isEventType(expr ast.Expr, eventsName string, inEvents bool) bool {
	switch e := expr.(type) {
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		return ok && eventsName != "" && x.Name == eventsName && e.Sel.Name == "Event"
	case *ast.Ident:
		return inEvents && e.Name == "Event"
	}
	return false
}

// importName returns the name file refers to path by, or "".
func importName(file *ast.File, path string) string {
	for _, spec := range file.Imports {
		p, _ := strconv.Unquote(spec.Path.Value)
		if p != path {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return filepath.Base(p)
	}
	return ""
}

// resolve returns the string an expression names when it is a string literal
// or a package-level constant of this module, following constants defined
// from other constants.
func (r *eventTypeResolver) resolve(t *testing.T, dir string, file *ast.File, expr ast.Expr, depth int) (string, bool) {
	t.Helper()
	if depth > 8 {
		return "", false
	}
	switch e := expr.(type) {
	case *ast.BasicLit:
		if e.Kind != token.STRING {
			return "", false
		}
		s, err := strconv.Unquote(e.Value)
		return s, err == nil
	case *ast.ParenExpr:
		return r.resolve(t, dir, file, e.X, depth+1)
	case *ast.CallExpr: // the conversion string(x)
		if fn, ok := e.Fun.(*ast.Ident); ok && fn.Name == "string" && len(e.Args) == 1 {
			return r.resolve(t, dir, file, e.Args[0], depth+1)
		}
	case *ast.Ident:
		if decl, ok := r.packageConsts(t, dir)[e.Name]; ok {
			return r.resolve(t, dir, decl.file, decl.expr, depth+1)
		}
	case *ast.SelectorExpr:
		x, ok := e.X.(*ast.Ident)
		if !ok {
			return "", false
		}
		for _, spec := range file.Imports {
			p, _ := strconv.Unquote(spec.Path.Value)
			name := filepath.Base(p)
			if spec.Name != nil {
				name = spec.Name.Name
			}
			if name != x.Name || !strings.HasPrefix(p, gcModulePath+"/") {
				continue
			}
			other := filepath.Join(r.root, filepath.FromSlash(strings.TrimPrefix(p, gcModulePath+"/")))
			if decl, ok := r.packageConsts(t, other)[e.Sel.Name]; ok {
				return r.resolve(t, other, decl.file, decl.expr, depth+1)
			}
		}
	}
	return "", false
}

// packageConsts returns the package-level constants declared in dir's
// non-test files.
func (r *eventTypeResolver) packageConsts(t *testing.T, dir string) map[string]constDecl {
	t.Helper()
	if got, ok := r.consts[dir]; ok {
		return got
	}
	out := map[string]constDecl{}
	r.consts[dir] = out
	entries, err := os.ReadDir(dir)
	if err != nil {
		return out
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(token.NewFileSet(), filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parsing %s: %v", filepath.Join(dir, name), err)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs := spec.(*ast.ValueSpec)
				for i, ident := range vs.Names {
					if i < len(vs.Values) {
						out[ident.Name] = constDecl{expr: vs.Values[i], file: file}
					}
				}
			}
		}
	}
	return out
}
