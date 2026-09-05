package scripts_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Rule ast:returns-store-list. See residency_boundary_test.go for the contract
// this rule is one quarter of.

// TestResidencyResolverBoundary is the check grep cannot make: any non-test,
// non-allowlisted function in cmd/gc or internal/api whose SIGNATURE hands back
// a raw []beads.Store or map[string]beads.Store must be in the baseline or
// carry the marker.
//
// It closes the grep half's residual hole — deleting one baselined call and
// adding a different one of the same pattern in the same file keeps the counts
// level, but a new store-list constructor changes a signature.
//
// Set RESIDENCY_BASELINE_EMIT=1 to print the rows instead of checking them —
// the shrink workflow, same as the shell guard's --emit-baseline.
func TestResidencyResolverBoundary(t *testing.T) {
	root := repoRoot(t)
	found, err := scanStoreListSignatures(root, residencyScanDirs, residencyAllowlist)
	if err != nil {
		t.Fatalf("scanning for store-list signatures: %v", err)
	}
	if len(found) == 0 {
		t.Fatal("the AST scan found no store-list signature at all; the guard is evaluating nothing")
	}
	if os.Getenv("RESIDENCY_BASELINE_EMIT") == "1" {
		for _, key := range sortedKeys(found) {
			fmt.Printf("%s\t%d\n", key, found[key])
		}
		t.Skip("emitted AST baseline rows")
	}

	baseline, err := readResidencyBaseline(residencyBaselinePath(t), func(p string) bool { return p == residencyASTPattern })
	if err != nil {
		t.Fatalf("reading the baseline: %v", err)
	}
	if len(baseline) == 0 {
		t.Fatal("the baseline pins no AST row; the ratchet has no denominator")
	}
	assertRatchet(t, found, baseline)
}

// TestResidencyResolverBoundaryControls falsifies the AST guard with real
// on-disk files.
func TestResidencyResolverBoundaryControls(t *testing.T) {
	root := t.TempDir()
	writeResidencyFixture(t, root, "cmd/gc/pinned.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

func pinned() []beads.Store { return nil }
`)
	base := map[string]int{"cmd/gc/pinned.go\tpinned\t" + residencyASTPattern: 1}
	scan := func(t *testing.T, baseline map[string]int) []string {
		t.Helper()
		found, err := scanStoreListSignatures(root, residencyScanDirs, nil)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return ratchetViolations(found, baseline)
	}

	t.Run("baselined signature passes", func(t *testing.T) {
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("a baselined signature was reported: %v", v)
		}
	})

	t.Run("new signature fails", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

func eleventh() map[string]beads.Store { return nil }
`)
		v := scan(t, base)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "eleventh.go") {
			t.Fatalf("the AST guard accepted a NEW store-list signature: %v", v)
		}
	})

	t.Run("marker suppresses", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

// eleventh enumerates by definition.
// `+residencyAllowMarker+` migration tooling
func eleventh() map[string]beads.Store { return nil }
`)
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("the marker did not suppress the signature: %v", v)
		}
	})

	t.Run("a non-store list is not a signature", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", "package main\n\nfunc eleventh() []string { return nil }\n")
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("an unrelated slice return was reported: %v", v)
		}
	})

	// The two spellings that change the result type's TEXT without changing what
	// the caller is handed. Both were live holes: cmd_wait.go's dependency store
	// set used the first one and this rule had never seen it.
	t.Run("a local type name for the list is still the list", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

type storeSet []beads.Store

func eleventh() storeSet { return nil }
`)
		v := scan(t, base)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "eleventh") {
			t.Fatalf("a one-line type alias hid the store list from the signature rule: %v", v)
		}
	})

	t.Run("an alias declared in another package is not resolved", func(t *testing.T) {
		// The counterpart that must stay silent. The alias pass is scoped to the
		// declaring directory, so this states the rule's honest edge rather than
		// implying a cross-package resolution it does not do.
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", "package main\n\nfunc eleventh() otherpkg.StoreSet { return nil }\n")
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("a qualified type this scanner cannot resolve was reported: %v", v)
		}
	})

	t.Run("a package var holding the constructor is a signature", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

var eleventh = func() []beads.Store { return nil }
`)
		v := scan(t, base)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "eleventh") {
			t.Fatalf("a func literal on a package var is not a FuncDecl, and slipped the rule: %v", v)
		}
	})

	t.Run("a package var declared with the func type is a signature", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

var eleventh func() map[string]beads.Store
`)
		v := scan(t, base)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "eleventh") {
			t.Fatalf("a func-typed package var slipped the rule: %v", v)
		}
	})

	t.Run("a plain store var is not a signature", func(t *testing.T) {
		writeResidencyFixture(t, root, "cmd/gc/eleventh.go", `package main

import "github.com/gastownhall/gascity/internal/beads"

var eleventh []beads.Store
`)
		if v := scan(t, base); len(v) != 0 {
			t.Fatalf("a variable that IS a store list, rather than a function handing one out, was reported: %v", v)
		}
	})

	t.Run("a subpackage is not a hiding place", func(t *testing.T) {
		writeResidencyFixture(t, root, "internal/api/dashboardbff/nested.go", `package dashboardbff

import "github.com/gastownhall/gascity/internal/beads"

func nested() []beads.Store { return nil }
`)
		defer func() {
			if err := os.Remove(filepath.Join(root, "internal/api/dashboardbff/nested.go")); err != nil {
				t.Fatalf("remove: %v", err)
			}
		}()
		v := scan(t, base)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "dashboardbff/nested.go") {
			t.Fatalf("the AST guard accepted a store-list signature one directory down: %v", v)
		}
	})

	t.Run("stale baseline forces a shrink", func(t *testing.T) {
		if err := os.Remove(filepath.Join(root, "cmd/gc/eleventh.go")); err != nil {
			t.Fatalf("remove: %v", err)
		}
		stale := map[string]int{
			"cmd/gc/pinned.go\tpinned\t" + residencyASTPattern: 1,
			"cmd/gc/gone.go\tgone\t" + residencyASTPattern:     1,
		}
		v := scan(t, stale)
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), "gone.go") {
			t.Fatalf("the AST guard accepted a baseline entry the tree no longer reaches: %v", v)
		}
	})
}

// scanStoreListSignatures counts, per file, the non-test functions whose
// results include a raw []beads.Store or map[string]beads.Store.
//
// The rule reads the SPELLING of the result type, which two legal spellings can
// change without changing what is handed over. `type storeList []beads.Store`
// and then `func f() storeList` is a one-line evasion of a rule about handing
// callers a raw store list; so is hanging the same signature off a package
// `var` as a func literal, which is not a FuncDecl and so never reached the
// result loop at all. Both are closed below — the alias pass first, then the
// count pass — because a rule this cheap to sidestep is not a rule.
func scanStoreListSignatures(root string, dirs []string, allowlist map[string]bool) (map[string]int, error) {
	tree, err := residencyParseTree(root, dirs)
	if err != nil {
		return nil, err
	}
	return countStoreListSignatures(tree, allowlist), nil
}

// countStoreListSignatures is the rule itself, over a tree someone else walked.
func countStoreListSignatures(tree residencyTree, allowlist map[string]bool) map[string]int {
	// Aliases are collected from EVERY file, including allowlisted ones. The
	// allowlist exempts a file from being COUNTED; letting it also hide a type
	// declaration would make it the one place to mint an invisible spelling for
	// the rest of the package.
	aliases := map[string]map[string]bool{}
	for _, pf := range tree.files {
		for _, decl := range pf.file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.TYPE {
				continue
			}
			for _, spec := range gen.Specs {
				ts, ok := spec.(*ast.TypeSpec)
				if !ok || ts.Name == nil || !isRawStoreList(ts.Type, nil) {
					continue
				}
				if aliases[pf.dir] == nil {
					aliases[pf.dir] = map[string]bool{}
				}
				aliases[pf.dir][ts.Name.Name] = true
			}
		}
	}

	found := map[string]int{}
	for _, pf := range tree.files {
		if allowlist[pf.rel] {
			continue
		}
		local := aliases[pf.dir]
		for _, decl := range pf.file.Decls {
			switch typed := decl.(type) {
			case *ast.FuncDecl:
				if typed.Doc != nil && strings.Contains(typed.Doc.Text(), residencyAllowMarker) {
					continue
				}
				if residencyFuncTypeYieldsStoreList(typed.Type, local) {
					found[pf.rel+"\t"+typed.Name.Name+"\t"+residencyASTPattern]++
				}
			case *ast.GenDecl:
				if typed.Tok != token.VAR || (typed.Doc != nil && strings.Contains(typed.Doc.Text(), residencyAllowMarker)) {
					continue
				}
				for _, spec := range typed.Specs {
					vs, ok := spec.(*ast.ValueSpec)
					if !ok || (vs.Doc != nil && strings.Contains(vs.Doc.Text(), residencyAllowMarker)) {
						continue
					}
					for i, name := range vs.Names {
						if residencyValueSpecYieldsStoreList(vs, i, local) {
							found[pf.rel+"\t"+name.Name+"\t"+residencyASTPattern]++
						}
					}
				}
			}
		}
	}
	return found
}

// residencyFuncTypeYieldsStoreList reports whether any result of ft is a raw
// store list, under the locally-declared aliases for its package.
func residencyFuncTypeYieldsStoreList(ft *ast.FuncType, aliases map[string]bool) bool {
	if ft == nil || ft.Results == nil {
		return false
	}
	for _, result := range ft.Results.List {
		if isRawStoreList(result.Type, aliases) {
			return true
		}
	}
	return false
}

// residencyValueSpecYieldsStoreList reports whether the i'th name of vs is a
// func value handing back a raw store list — `var f func() []beads.Store` by
// declared type, or `var f = func() []beads.Store { ... }` by literal.
func residencyValueSpecYieldsStoreList(vs *ast.ValueSpec, i int, aliases map[string]bool) bool {
	if ft, ok := vs.Type.(*ast.FuncType); ok && residencyFuncTypeYieldsStoreList(ft, aliases) {
		return true
	}
	if i >= len(vs.Values) {
		return false
	}
	lit, ok := residencyUnparen(vs.Values[i]).(*ast.FuncLit)
	return ok && residencyFuncTypeYieldsStoreList(lit.Type, aliases)
}

// isRawStoreList reports whether expr is []beads.Store, map[string]beads.Store
// or []storeref.Leg — the shapes that hand a caller a probe list it will read
// itself — or a local type name declared as one of those.
//
// The first two carry no leg order and no error policy at all. []storeref.Leg
// is the subtler one: a consumer that enumerated a plan through EachLeg and
// then RETURNS the bare legs has stripped the policy back off and handed the
// next caller the same unpoliced list, one function further from the resolver.
// The grep half ratchets the EachLeg call; this ratchets the shape it can be
// laundered into.
//
// aliases is nil when deciding whether a type DECLARATION is itself a store
// list, which is what stops `type a b; type b []beads.Store` from resolving
// through a chain the rule never claimed to follow. One hop is what a
// one-line evasion costs; a chain is a deliberate act with a paper trail.
func isRawStoreList(expr ast.Expr, aliases map[string]bool) bool {
	switch typed := expr.(type) {
	case *ast.ArrayType:
		return typed.Len == nil && (isBeadsStore(typed.Elt) || isStorerefLeg(typed.Elt))
	case *ast.MapType:
		key, ok := typed.Key.(*ast.Ident)
		return ok && key.Name == "string" && (isBeadsStore(typed.Value) || isStorerefLeg(typed.Value))
	case *ast.Ident:
		return aliases[typed.Name]
	default:
		return false
	}
}
