package beads

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"
)

// TestAllMembershipsCoversEveryDeclaredConstant keeps AllMemberships honest by
// parsing the declarations rather than trusting a second hand-written list.
// AllMemberships is what the wire-schema guard checks an OpenAPI enum against,
// so a rule that is declared but missing from it would leave that guard
// unable to tell a real vocabulary member from a typo.
func TestAllMembershipsCoversEveryDeclaredConstant(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "membership.go", nil, 0)
	if err != nil {
		t.Fatalf("parsing membership.go: %v", err)
	}

	declared := map[string]bool{}
	ast.Inspect(file, func(n ast.Node) bool {
		decl, ok := n.(*ast.GenDecl)
		if !ok || decl.Tok != token.CONST {
			return true
		}
		for _, spec := range decl.Specs {
			value, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			typeName, ok := value.Type.(*ast.Ident)
			if !ok || typeName.Name != "Membership" {
				continue
			}
			for _, name := range value.Names {
				declared[name.Name] = true
			}
		}
		return true
	})
	if len(declared) == 0 {
		t.Fatal("parsed no Membership constants from membership.go; the guard below would pass vacuously")
	}

	listed := map[string]bool{}
	for _, m := range AllMemberships() {
		listed[string(m)] = true
	}

	// Map declared identifier -> its value by evaluating the exported symbols.
	byIdent := map[string]Membership{
		"MembershipDirectRootID":                 MembershipDirectRootID,
		"MembershipDepReachable":                 MembershipDepReachable,
		"MembershipRootIDAndParentClosure":       MembershipRootIDAndParentClosure,
		"MembershipRootIDParentClosureAndConvoy": MembershipRootIDParentClosureAndConvoy,
	}
	for name := range declared {
		value, known := byIdent[name]
		if !known {
			t.Errorf("membership.go declares %s, which this test does not know about; add it to byIdent and to AllMemberships, or the vocabulary guard silently stops covering it", name)
			continue
		}
		if !listed[string(value)] {
			t.Errorf("AllMemberships omits %s (%q); a wire enum naming that rule would then look like a typo to the schema guard", name, value)
		}
	}
	for name := range byIdent {
		if !declared[name] {
			t.Errorf("this test references %s but membership.go no longer declares it", name)
		}
	}
	if got, want := len(AllMemberships()), len(declared); got != want {
		t.Errorf("AllMemberships returns %d rules, want %d declared constants (duplicates or omissions)", got, want)
	}
}
