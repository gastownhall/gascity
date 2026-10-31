package v2purity

import (
	"go/ast"
	"go/token"
	"go/types"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// seal reports each value of a sealed type (a proof only a fresh read
// mints) built, and each field of one written, outside its minting file:
// a composite literal (an elided one included), a conversion, new, a
// non-pointer var, and an assignment (a range clause's included),
// increment or address-of of a field of a sealed value, reached directly
// or through an embedding struct, aliases resolved. Test files build them
// freely.
func (c Config) seal(pass *analysis.Pass) {
	for _, f := range pass.Files {
		file := filepath.Base(pass.Fset.Position(f.Pos()).Filename)
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		hit := func(n ast.Node, t types.Type, what string) {
			if name := c.sealedName(pass, t); name != "" && c.Sealed[name] != file {
				pass.Reportf(n.Pos(), "%s of sealed %s outside %s", what, name, c.Sealed[name])
			}
		}
		ast.Inspect(f, func(n ast.Node) bool {
			switch n := n.(type) {
			case *ast.CompositeLit:
				hit(n, pass.TypesInfo.TypeOf(n), "literal")
			case *ast.CallExpr:
				if tv, ok := pass.TypesInfo.Types[n.Fun]; ok && tv.IsType() {
					hit(n, tv.Type, "conversion")
				} else if id, ok := ast.Unparen(n.Fun).(*ast.Ident); ok && len(n.Args) == 1 {
					if b, ok := pass.TypesInfo.Uses[id].(*types.Builtin); ok && b.Name() == "new" {
						hit(n, pass.TypesInfo.TypeOf(n.Args[0]), "new")
					}
				}
			case *ast.ValueSpec:
				for _, id := range n.Names {
					if obj := pass.TypesInfo.Defs[id]; obj != nil {
						if _, ptr := types.Unalias(obj.Type()).(*types.Pointer); !ptr { // a nil pointer builds nothing
							hit(id, obj.Type(), "var")
						}
					}
				}
			case *ast.AssignStmt:
				for _, lhs := range n.Lhs {
					c.fieldWrite(pass, lhs, hit)
				}
			case *ast.RangeStmt:
				if n.Tok == token.ASSIGN {
					c.fieldWrite(pass, n.Key, hit)
					c.fieldWrite(pass, n.Value, hit)
				}
			case *ast.IncDecStmt:
				c.fieldWrite(pass, n.X, hit)
			case *ast.UnaryExpr:
				if n.Op == token.AND {
					c.fieldWrite(pass, n.X, hit)
				}
			}
			return true
		})
	}
}

// fieldWrite reports e, a selector of a field of a sealed value, written or
// addressed: the value selected from, or any sealed struct the selection
// passes through on its way to a promoted field.
func (c Config) fieldWrite(pass *analysis.Pass, e ast.Expr, hit func(ast.Node, types.Type, string)) {
	sel, ok := ast.Unparen(e).(*ast.SelectorExpr)
	if !ok {
		return
	}
	s := pass.TypesInfo.Selections[sel]
	if s == nil || s.Kind() != types.FieldVal {
		return
	}
	t := s.Recv()
	for _, i := range s.Index() {
		hit(sel, t, "field write")
		if p, ok := types.Unalias(t).(*types.Pointer); ok {
			t = p.Elem()
		}
		st, ok := t.Underlying().(*types.Struct)
		if !ok {
			return
		}
		t = st.Field(i).Type()
	}
}

// sealedName is the sealed type t is, a pointer to, or an alias of, in the
// configured package, or "".
func (c Config) sealedName(pass *analysis.Pass, t types.Type) string {
	if t == nil {
		return ""
	}
	if p, ok := types.Unalias(t).(*types.Pointer); ok {
		t = p.Elem()
	}
	named, ok := types.Unalias(t).(*types.Named)
	if !ok || named.Obj().Pkg() != pass.Pkg {
		return ""
	}
	if _, sealed := c.Sealed[named.Obj().Name()]; sealed {
		return named.Obj().Name()
	}
	return ""
}
