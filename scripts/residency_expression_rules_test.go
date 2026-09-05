package scripts_test

import (
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"regexp"
	"strings"
	"testing"
)

// Rules ast:vocabulary-alias, ast:plan-leg-store-chain and
// ast:uncounted-call-spelling: the evasions a line-oriented census cannot see.
//
// All three read expressions rather than declarations, and all three watch the
// grep rule's own vocabulary — DERIVED from the shared pattern file, never
// restated — so a row added there extends both censuses on the day it lands.
//
// See residency_boundary_test.go for the contract these rules are one quarter
// of.

// The two evasions that are invisible to a line-oriented census by
// construction:
//
// (a) FUNCTION-VALUE ALIASING. Every call-shaped grep row demands a literal `(`
//     after the name, so `f := routes.storeFor` followed by `f(c)` names
//     nothing the census knows: the derivation is fetched under one name and
//     performed under another. `(routes.storeFor)(c)` evades the same row for
//     the same reason — the character after the name is `)`.
//
// (d) LINE-SPLIT FIELD ACCESS. `c:plan-leg-store-access` is anchored to one
//     line, and gofmt preserves a chain broken across three, so `pl.` / `Leg.` /
//     `Store` walks past it.
//
// Both belong here rather than in the pattern file because grep is the wrong
// instrument for each: a value-position row would have to match a name NOT
// followed by `(`, which fires inside strings and, worse, cannot tell a
// reference from a declaration — and the tree has both (`rigBeadStores` is a
// parameter name, `BeadStores()` an interface method). The AST knows the
// difference and is layout-blind.

// residencyGuardedNames is the identifier vocabulary the alias rule watches,
// DERIVED from the shared pattern file rather than restated here.
//
// A second hand-kept list of names would be this guard's own bug class one
// level up — the pattern file's header says as much about the two grep halves —
// and it would fail in a specific way: a vocabulary row added to the file would
// be aliasable from the day it landed, because only the census that counts
// CALLS would have learned the name.
type residencyGuardedNames struct {
	bare         map[string]bool // BeadStores, cliSoleClassBinding, ...
	selector     map[string]bool // the selector half of a receiver-blind row like `\.storeFor\(`
	qualified    map[string]bool // "storeref.EachLeg", "routes.storeFor"
	qualifiedSel map[string]bool // "EachLeg" — the qualified rows' selector halves
	unreduced    []string        // call-shaped rows that did not reduce to an identifier
}

func (n residencyGuardedNames) empty() bool {
	return len(n.bare) == 0 && len(n.selector) == 0 && len(n.qualified) == 0
}

var (
	residencyOptionalGroupRe = regexp.MustCompile(`\(([A-Za-z0-9_]+)\)\?`)
	residencyIdentRe         = regexp.MustCompile(`^[A-Za-z0-9_]+$`)
)

// residencyDeriveGuardedNames reduces the call-shaped pattern rows to the
// identifiers whose call they count.
//
// A row that is NOT call-shaped is skipped deliberately, not silently: a
// `[]beads.Store{` literal and a `.Leg.Store` field access have no name to
// alias, and `d:NudgeQueueIDPrefix` already matches a bare mention because it
// has no call parenthesis in the first place. A row that IS call-shaped but does
// not reduce to an identifier is recorded in unreduced, because dropping it
// would be the fail-open this rule exists to prevent.
//
// CALL-SHAPEDNESS IS DECIDED BY CONTAINING `\(`, NOT BY ENDING IN IT. The two
// tests differ on rows nobody has written yet, which is exactly when it matters:
// `openLegacyStores\([^)]` or `ReservedPrefixFor\(ctx` are plainly call rows the
// grep census would count, and under a suffix test they would fall out of the
// alias vocabulary with no record — the pattern file's promise that a new row
// extends both censuses would quietly stop being true. Under a containment test
// they reach the reduction, fail it, and land in unreduced, where
// TestResidencyGuardedNameDerivation refuses them until the shape is handled.
func residencyDeriveGuardedNames(patterns []residencyPattern) residencyGuardedNames {
	names := residencyGuardedNames{
		bare:         map[string]bool{},
		selector:     map[string]bool{},
		qualified:    map[string]bool{},
		qualifiedSel: map[string]bool{},
	}
	for _, p := range patterns {
		expr := strings.TrimPrefix(p.regex.String(), `(^|[^A-Za-z0-9_])`)
		if !strings.Contains(expr, `\(`) {
			continue
		}
		switch {
		case strings.HasSuffix(expr, `\(\)`):
			expr = strings.TrimSuffix(expr, `\(\)`)
		case strings.HasSuffix(expr, `\(`):
			expr = strings.TrimSuffix(expr, `\(`)
		default:
			names.unreduced = append(names.unreduced, p.name)
			continue
		}
		for _, spelling := range residencyExpandOptional(expr) {
			spelling = strings.ReplaceAll(spelling, `\.`, ".")
			pkg, sel, qualified := strings.Cut(spelling, ".")
			switch {
			case qualified && pkg == "":
				// `\.storeFor\(` — matched on ANY receiver, so the alias rule
				// watches the selector wherever it is fetched from too.
				if residencyIdentRe.MatchString(sel) {
					names.selector[sel] = true
					continue
				}
			case qualified:
				if residencyIdentRe.MatchString(pkg) && residencyIdentRe.MatchString(sel) {
					names.qualified[spelling] = true
					names.qualifiedSel[sel] = true
					continue
				}
			default:
				if residencyIdentRe.MatchString(spelling) {
					names.bare[spelling] = true
					continue
				}
			}
			names.unreduced = append(names.unreduced, p.name)
		}
	}
	return names
}

// residencyExpandOptional spells out every alternative of an `(X)?` group, so
// `cliSoleClassBinding(Store)?` guards both spellings the grep row guards.
func residencyExpandOptional(expr string) []string {
	out := []string{expr}
	for {
		grew := false
		var next []string
		for _, candidate := range out {
			m := residencyOptionalGroupRe.FindStringSubmatchIndex(candidate)
			if m == nil {
				next = append(next, candidate)
				continue
			}
			grew = true
			next = append(next,
				candidate[:m[0]]+candidate[m[2]:m[3]]+candidate[m[1]:],
				candidate[:m[0]]+candidate[m[1]:])
		}
		out = next
		if !grew {
			return out
		}
	}
}

// residencyMarkerLines is the escape hatch for expression rules. The signature
// rule reads a function's DOC comment, which is the right granularity for a
// whole declaration; an expression needs the line it sits on, the same way the
// grep half reads it. A doc-comment marker therefore does not suppress an
// expression inside the function — suppressing every site in a body from one
// line above it is not a reviewable exemption.
func residencyMarkerLines(data []byte) map[int]bool {
	out := map[int]bool{}
	for i, line := range strings.Split(string(data), "\n") {
		if strings.Contains(line, residencyAllowMarker) {
			out[i+1] = true
		}
	}
	return out
}

// residencyMarked reads ONE line: the one the decisive token sits on — the
// selector's `.Sel` for a selector, the identifier itself for an identifier.
//
// Scanning the node's whole Pos..End span instead would hand a multi-line
// expression an exemption its author never asked for. In
//
//	s := resolveBinding(
//		cityPath, // residency:allow reviewed for the hit on THIS line
//	).Leg.Store
//
// the marker was written and reviewed for the argument line, and a span check
// would silently extend it over the `.Leg.Store` chain three lines later. A
// marker is a reviewed exemption for a site; it has to name one line the way
// the grep half does, or it stops being reviewable.
func residencyMarked(fset *token.FileSet, markers map[int]bool, pos token.Pos) bool {
	return len(markers) != 0 && markers[fset.Position(pos).Line]
}

// residencyIsLegStoreChain reports whether sel is the `.Leg.Store` chain,
// however it is laid out across lines or parenthesised.
//
// The parentheses matter as much as the line breaks: gofmt does not strip a
// redundant `(pl.Leg).Store`, so it survives review looking like ordinary code
// while the text rule — which spells the chain `\.Leg\.Store` — reads `.Leg)`
// and sees nothing. Unwrapping is one loop; leaving it out would have made the
// docs beside this rule false on the day they were written.
func residencyIsLegStoreChain(sel *ast.SelectorExpr) bool {
	if sel.Sel == nil || sel.Sel.Name != "Store" {
		return false
	}
	inner, ok := residencyUnparen(sel.X).(*ast.SelectorExpr)
	return ok && inner.Sel != nil && inner.Sel.Name == "Leg"
}

// residencyGrepCouldNotCount reports whether sel is a CALL to guarded
// vocabulary written in a spelling the text census provably cannot match.
//
// Calls are normally grep's business and this rule stands down for them. Three
// spellings take that away, and every one of them survives gofmt exactly as
// written:
//
//	routes.          storeref.          import sr ".../storeref"
//		storeFor(c)      EachLeg(p, fn)     sr.EachLeg(p, fn)
//
//	(routes).storeFor(c)
//
// The first two split the chain at the dot, so no single line holds the dot,
// the name and the parenthesis that every b/c row requires together. The third
// keeps them on one line but spells the package half something the row does not
// know — `storeref\.EachLeg\(` cannot match `sr.EachLeg(`. The fourth writes
// every token on one line under the name the row does know, and still cannot be
// matched: `routes\.storeFor\(` needs the qualifier and the dot ADJACENT, and
// the `)` of `(routes)` sits between them. In all four the call happens, the
// resolver is bypassed, and both halves stay silent.
//
// The receiver is therefore classified WITHOUT being unparenthesised first.
// This function models what grep sees, and grep does not unparen anything —
// normalising here would credit the text census with a match it cannot make and
// stand the rule down on the one spelling that needs it. residencyUnparen
// belongs to the rules that match structure, not to this one.
//
// This is the line-split evasion the bead names, applied to the call families
// rather than to `.Leg.Store`, plus the import-rename and parenthesised-receiver
// twins it shares a fix with. It needs no type resolution: the question is not
// what the receiver IS, it is whether the text census had the three tokens on
// one line under the spelling it was given.
func residencyGrepCouldNotCount(fset *token.FileSet, sel *ast.SelectorExpr, names residencyGuardedNames) bool {
	if sel.Sel == nil {
		return false
	}
	x, qualifiedByIdent := sel.X.(*ast.Ident)
	switch {
	case qualifiedByIdent && names.qualified[x.Name+"."+sel.Sel.Name]:
		// Spelled the way the row expects — grep counts it if it is on one line.
	case names.selector[sel.Sel.Name]:
		// A receiver-blind row: `\.storeFor\(` needs only the dot and the name.
	case names.qualifiedSel[sel.Sel.Name]:
		// A qualified row reached through some OTHER qualifier — a renamed
		// import, or a variable holding the package's own seam. No row can match
		// this line however it is laid out, so the split test below is moot.
		return true
	default:
		return false
	}
	return fset.Position(sel.X.End()).Line != fset.Position(sel.Sel.Pos()).Line
}

func residencyUnparen(expr ast.Expr) ast.Expr {
	for {
		paren, ok := expr.(*ast.ParenExpr)
		if !ok {
			return expr
		}
		expr = paren.X
	}
}

// scanResidencyASTExpressions counts, per (path, enclosing function, pattern),
// the alias and leg-chain sites in the tree the grep half scans.
//
// It covers all of residencyScanDirs, as every rule here now does: this one
// COMPLEMENTS grep on grep's own subject — the vocabulary — so scanning less
// than grep would leave the alias reachable exactly where the vocabulary is
// still counted.
//
// THE HONEST RESIDUAL: an access through an intermediate variable —
// `l := pl.Leg; use(l.Store)`, or `r := routes; r.storeFor(c)` — is invisible
// to an untyped AST, the same class of residual as the grep half's
// swap-within-one-function. Closing it needs go/types, and full type-checking
// cmd/gc on every commit costs more than the hole is worth.
func scanResidencyASTExpressions(root string, dirs []string, allowlist map[string]bool, names residencyGuardedNames) (map[string]int, error) {
	tree, err := residencyParseTree(root, dirs)
	if err != nil {
		return nil, err
	}
	return countResidencyASTExpressions(tree, allowlist, names), nil
}

// countResidencyASTExpressions is the rule itself, over a tree someone else
// walked.
func countResidencyASTExpressions(tree residencyTree, allowlist map[string]bool, names residencyGuardedNames) map[string]int {
	found := map[string]int{}
	for _, pf := range tree.files {
		if allowlist[pf.rel] {
			continue
		}
		countResidencyDeclExpressions(tree.fset, pf.file, pf.rel, residencyMarkerLines(pf.data), names, found)
	}
	return found
}

// countResidencyDeclExpressions attributes hits to the enclosing top-level
// declaration, the same key the other two rules use.
//
// It is name-based, not type-resolved, so a local or parameter SPELLED like a
// guarded accessor is counted wherever it is read. That is deliberate rather
// than tolerated: a variable named rigBeadStores reads at its use sites exactly
// like a call to the enumerator it shadows, which is the confusion this whole
// vocabulary exists to make visible. The two such parameters in cmd/gc — six
// occurrences across the reaper and the agent-home sweep — were renamed rather
// than marked. Resolving them properly needs go/types on a
// package that does not compile in isolation here, and the cost of the name
// rule is one rename; the cost of go/types is a guard slow enough to be
// skipped.
func countResidencyDeclExpressions(fset *token.FileSet, file *ast.File, rel string, markers map[int]bool, names residencyGuardedNames, found map[string]int) {
	for _, decl := range file.Decls {
		fn := residencyFileScope
		if fd, ok := decl.(*ast.FuncDecl); ok && fd.Name != nil {
			fn = fd.Name.Name
		}

		// A call's Fun is grep's business, not this rule's — but only when it is
		// the Fun DIRECTLY. `(routes.storeFor)(c)` puts a ParenExpr there, which
		// is what makes the grep row miss it, so the selector inside stays
		// countable. Sel idents and declaration names are excluded so a
		// selector is counted once, as a selector, and never as its own
		// declaration.
		callFuns := map[ast.Node]bool{}
		selIdents := map[*ast.Ident]bool{}
		declIdents := map[*ast.Ident]bool{}
		ast.Inspect(decl, func(n ast.Node) bool {
			switch typed := n.(type) {
			case *ast.CallExpr:
				callFuns[typed.Fun] = true
			case *ast.SelectorExpr:
				if typed.Sel != nil {
					selIdents[typed.Sel] = true
				}
			case *ast.FuncDecl:
				if typed.Name != nil {
					declIdents[typed.Name] = true
				}
			case *ast.Field:
				for _, fieldName := range typed.Names {
					declIdents[fieldName] = true
				}
			}
			return true
		})

		count := func(pattern string) { found[rel+"\t"+fn+"\t"+pattern]++ }
		ast.Inspect(decl, func(n ast.Node) bool {
			switch typed := n.(type) {
			case *ast.SelectorExpr:
				if residencyMarked(fset, markers, typed.Sel.Pos()) {
					return true
				}
				if residencyIsLegStoreChain(typed) {
					count(residencyLegChainPattern)
					return true
				}
				if callFuns[ast.Node(typed)] {
					if residencyGrepCouldNotCount(fset, typed, names) {
						count(residencyUncountedCall)
					}
					return true
				}
				if x, ok := typed.X.(*ast.Ident); ok && names.qualified[x.Name+"."+typed.Sel.Name] {
					count(residencyAliasPattern)
					return true
				}
				if names.selector[typed.Sel.Name] || names.bare[typed.Sel.Name] || names.qualifiedSel[typed.Sel.Name] {
					count(residencyAliasPattern)
				}
			case *ast.Ident:
				if selIdents[typed] || declIdents[typed] || callFuns[ast.Node(typed)] {
					return true
				}
				if names.bare[typed.Name] && !residencyMarked(fset, markers, typed.Pos()) {
					count(residencyAliasPattern)
				}
			}
			return true
		})
	}
}

// TestResidencyGuardedNameDerivation pins the reduction from pattern rows to
// guarded identifiers.
//
// Without it a regex edit could empty the alias rule silently — the rule would
// still run, still find nothing, and still pass, which is the failure mode the
// derivation was chosen to avoid in the first place.
func TestResidencyGuardedNameDerivation(t *testing.T) {
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	if len(names.unreduced) != 0 {
		t.Fatalf("call-shaped pattern rows did not reduce to an identifier, so the alias rule does not guard them: %v", names.unreduced)
	}
	if names.empty() {
		t.Fatal("the derivation produced no guarded name; the alias rule is evaluating nothing")
	}
	for _, want := range []string{"BeadStores", "cliSoleClassBinding"} {
		if !names.bare[want] {
			t.Errorf("%q is grep vocabulary but not alias vocabulary", want)
		}
	}
	for _, want := range []string{"storeref.EachLeg", "storeref.ClassCandidates", "routes.storeFor"} {
		if !names.qualified[want] {
			t.Errorf("%q is grep vocabulary but not alias vocabulary", want)
		}
	}
	// The selector half of every qualified row is guarded on its own, because the
	// grep row names one package qualifier and an import can be renamed. storeFor
	// is the case that pays for itself today: the tree reaches it through
	// cs.storageRoutes, through cliStorageRoutes(cityPath), and through
	// graphStores, none of which the `routes.` row can match.
	for _, want := range []string{"EachLeg", "ClassCandidates", "storeFor"} {
		if !names.qualifiedSel[want] {
			t.Errorf("%q is not guarded on its own, so a renamed import spells the same call past both halves", want)
		}
	}

	t.Run("an unhandled call shape lands in unreduced rather than vanishing", func(t *testing.T) {
		// The fail-open this classification exists to prevent: a plainly
		// call-shaped row the reduction has no case for. It must be REPORTED,
		// not skipped like the deliberately non-call rows beside it.
		synthetic := []residencyPattern{
			{name: "x:mid-call", regex: regexp.MustCompile(`(^|[^A-Za-z0-9_])openLegacyStores\([^)]`)},
			{name: "x:no-call", regex: regexp.MustCompile(`\[\]beads\.Store\{`)},
		}
		got := residencyDeriveGuardedNames(synthetic)
		if len(got.unreduced) != 1 || got.unreduced[0] != "x:mid-call" {
			t.Fatalf("unreduced = %v, want exactly [x:mid-call]: a call-shaped row must be refused, a non-call row skipped", got.unreduced)
		}
		if !got.empty() {
			t.Error("the unhandled row was also reduced to a name; the classification is reporting and guarding the same row")
		}
	})
}

// TestResidencyUncountedCallControls falsifies ast:uncounted-call-spelling —
// the rule for calls the grep census cannot see.
//
// Every grep row for a method or qualified call keys on a dot, a name and an
// open parenthesis appearing TOGETHER ON ONE LINE. Three legal spellings break
// that up, and gofmt preserves all of them: writing the dot at the end of one
// line and the name at the start of the next, renaming the import so the
// qualifier in the row never appears, and parenthesising the receiver so the
// qualifier and the dot stop being adjacent. None is exotic — gofmt produces
// the first itself on a long chain — and the alias rule deliberately stands
// down on a call's Fun, so before this rule all three walked past both halves
// of the guard.
func TestResidencyUncountedCallControls(t *testing.T) {
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	count := func(t *testing.T, body string) int {
		t.Helper()
		root := t.TempDir()
		writeResidencyFixture(t, root, "cmd/gc/calls.go", body)
		found, err := scanResidencyASTExpressions(root, residencyScanDirs, nil, names)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return found["cmd/gc/calls.go\tuse\t"+residencyUncountedCall]
	}

	fires := []struct{ label, body string }{
		{
			"a receiver-blind selector split across the dot",
			"package main\n\nfunc use(routes cliStorageRoutes, c coordclass.Class) {\n\t_, _ = routes.\n\t\tstoreFor(c)\n}\n",
		},
		{
			"a qualified call split across the dot",
			"package main\n\nfunc use(p storeref.ResolvedPlan, fn func()) {\n\tstoreref.\n\t\tEachLeg(p, fn)\n}\n",
		},
		{
			"a renamed import spelling a qualified call",
			"package main\n\nfunc use(p storeref.ResolvedPlan, fn func()) {\n\tsr.EachLeg(p, fn)\n}\n",
		},
		{
			"a parenthesized receiver",
			"package main\n\nfunc use(routes cliStorageRoutes, c coordclass.Class) {\n\t_, _ = (routes).storeFor(c)\n}\n",
		},
	}
	for _, tc := range fires {
		if got := count(t, tc.body); got != 1 {
			t.Errorf("%s: counted %d, want 1 — the call is invisible to the grep census and now to this rule too", tc.label, got)
		}
	}

	silent := []struct{ label, body string }{
		{
			"the same receiver-blind call on one line",
			"package main\n\nfunc use(routes cliStorageRoutes, c coordclass.Class) {\n\t_, _ = routes.storeFor(c)\n}\n",
		},
		{
			"the same qualified call on one line",
			"package main\n\nfunc use(p storeref.ResolvedPlan, fn func()) {\n\tstoreref.EachLeg(p, fn)\n}\n",
		},
		{
			"a split call on a name that is not vocabulary",
			"package main\n\nfunc use(routes cliStorageRoutes) {\n\t_ = routes.\n\t\tstoreForPoolAssignment()\n}\n",
		},
		{
			"a marked split call",
			"package main\n\nfunc use(routes cliStorageRoutes, c coordclass.Class) {\n\t_, _ = routes.\n\t\tstoreFor(c) // " + residencyAllowMarker + " tested escape hatch\n}\n",
		},
	}
	for _, tc := range silent {
		if got := count(t, tc.body); got != 0 {
			t.Errorf("%s: counted %d, want 0 — the grep census already sees this, so counting it here double-books the site", tc.label, got)
		}
	}
}

// TestResidencyMarkerIsLineScoped pins that `// residency:allow` exempts the
// LINE it sits on and nothing else.
//
// The marker used to be tested against the enclosing declaration's whole span,
// which silenced every site in a function because one line in it was annotated.
// An escape hatch that wide is not an escape hatch, it is an off switch, and it
// would be reached for exactly when a function holds one reviewed exception and
// several unreviewed ones.
func TestResidencyMarkerIsLineScoped(t *testing.T) {
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	root := t.TempDir()
	writeResidencyFixture(t, root, "cmd/gc/scope.go",
		"package main\n\nfunc use(pl storeref.PlanLeg, other storeref.PlanLeg) {\n"+
			"\t_ = pl.Leg.Store // "+residencyAllowMarker+" reviewed\n"+
			"\t_ = other.Leg.Store\n}\n")
	found, err := scanResidencyASTExpressions(root, residencyScanDirs, nil, names)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if got := found["cmd/gc/scope.go\tuse\t"+residencyLegChainPattern]; got != 1 {
		t.Fatalf("counted %d chains, want 1: the marked line must be exempt and the unmarked one beside it must not", got)
	}
}

// TestResidencyASTExpressionRatchet is the CI-visible half of the two
// expression rules: no new alias, no new leg chain, no stale pin.
//
// Set RESIDENCY_BASELINE_EMIT=1 to print the rows instead of checking them.
func TestResidencyASTExpressionRatchet(t *testing.T) {
	root := repoRoot(t)
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	if names.empty() {
		t.Fatal("the derivation produced no guarded name; the alias rule is evaluating nothing")
	}
	found, err := scanResidencyASTExpressions(root, residencyScanDirs, residencyAllowlist, names)
	if err != nil {
		t.Fatalf("scanning for aliased vocabulary: %v", err)
	}
	if os.Getenv("RESIDENCY_BASELINE_EMIT") == "1" {
		for _, key := range sortedKeys(found) {
			fmt.Printf("%s\t%d\n", key, found[key])
		}
		t.Skip("emitted AST expression baseline rows")
	}

	// All three patterns this scan emits, or a row it CAN produce has no
	// baseline to be compared against: the emitter would print it, the file
	// would hold it, and the ratchet would still read the census as growth
	// from zero.
	baseline, err := readResidencyBaseline(residencyBaselinePath(t), func(p string) bool {
		return p == residencyAliasPattern || p == residencyLegChainPattern || p == residencyUncountedCall
	})
	if err != nil {
		t.Fatalf("reading the baseline: %v", err)
	}
	// A ZERO census is the GOAL for every rule here, so unlike the grep and
	// signature ratchets this one must not fail closed on an empty baseline. The
	// denominator that proves it is evaluating something is names.empty() above —
	// the derived vocabulary — not the row count. Pinning the row count instead
	// would book a guaranteed deadlock: the expression baseline holds one row, and
	// retiring it is the migration this whole lane exists to drive, so success
	// would turn CI red.
	assertRatchet(t, found, baseline)
}

// TestResidencyVocabularyAliasControls falsifies the alias rule on real files:
// four ways to fetch the vocabulary without calling it must fire, and five
// shapes that merely look like it must not.
func TestResidencyVocabularyAliasControls(t *testing.T) {
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	scan := func(t *testing.T, root string) map[string]int {
		t.Helper()
		found, err := scanResidencyASTExpressions(root, residencyScanDirs, nil, names)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return found
	}
	fires := func(t *testing.T, label, body string) {
		t.Helper()
		root := t.TempDir()
		writeResidencyFixture(t, root, "cmd/gc/alias.go", body)
		if found := scan(t, root); found["cmd/gc/alias.go\tevade\t"+residencyAliasPattern] == 0 {
			t.Errorf("%s: the alias rule did not fire; the vocabulary was fetched under one name and called under another", label)
		}
	}
	silent := func(t *testing.T, label, body string) {
		t.Helper()
		root := t.TempDir()
		writeResidencyFixture(t, root, "cmd/gc/quiet.go", body)
		for key, n := range scan(t, root) {
			if strings.Contains(key, residencyAliasPattern) && n > 0 {
				t.Errorf("%s: the alias rule fired on a shape that is not an alias (%s)", label, strings.ReplaceAll(key, "\t", " "))
			}
		}
	}

	fires(t, "a method value", "package main\n\nfunc evade(routes cliStorageRoutes, c coordclass.Class) {\n\tf := routes.storeFor\n\t_, _ = f(c)\n}\n")
	fires(t, "a package-level function value", "package main\n\nfunc evade() {\n\tg := BeadStores\n\t_ = g()\n}\n")
	fires(t, "a qualified function value", "package main\n\nfunc evade(c coordclass.Class) {\n\th := storeref.ClassCandidates\n\t_ = h(c)\n}\n")
	fires(t, "a parenthesized call target", "package main\n\nfunc evade(routes cliStorageRoutes, c coordclass.Class) {\n\t_, _ = (routes.storeFor)(c)\n}\n")

	silent(t, "a plain call", "package main\n\nfunc quiet(routes cliStorageRoutes, c coordclass.Class) {\n\t_, _ = routes.storeFor(c)\n}\n")
	silent(t, "the accessor's own declaration", "package main\n\nfunc (r cliStorageRoutes) storeFor(c coordclass.Class) (beads.Store, bool) {\n\treturn nil, false\n}\n")
	silent(t, "an interface method", "package main\n\ntype apiState interface {\n\tBeadStores() map[string]beads.Store\n}\n")
	silent(t, "a distinct identifier", "package main\n\nfunc quiet() {\n\t_ = storeForPoolAssignment\n}\n")
	silent(t, "a marked line", "package main\n\nfunc quiet(routes cliStorageRoutes) {\n\tf := routes.storeFor // "+residencyAllowMarker+" tested escape hatch\n\t_ = f\n}\n")

	t.Run("a new alias is a ratchet violation", func(t *testing.T) {
		root := t.TempDir()
		writeResidencyFixture(t, root, "cmd/gc/alias.go", "package main\n\nfunc evade() {\n\tg := BeadStores\n\t_ = g()\n}\n")
		v := ratchetViolations(scan(t, root), map[string]int{})
		if len(v) == 0 || !strings.Contains(strings.Join(v, " "), residencyAliasPattern) {
			t.Fatalf("an unpinned alias was not reported as growth: %v", v)
		}
	})
}

// TestResidencyPlanLegChainControls falsifies the leg-chain rule. The split
// form is the one the grep row cannot see; the single-line form proves this
// rule subsumes its shape rather than merely complementing it.
func TestResidencyPlanLegChainControls(t *testing.T) {
	names := residencyDeriveGuardedNames(loadResidencyPatterns(t, residencyScriptsDir(t)))
	scan := func(t *testing.T, body string) int {
		t.Helper()
		root := t.TempDir()
		writeResidencyFixture(t, root, "cmd/gc/legs.go", body)
		found, err := scanResidencyASTExpressions(root, residencyScanDirs, nil, names)
		if err != nil {
			t.Fatalf("scan: %v", err)
		}
		return found["cmd/gc/legs.go\tuse\t"+residencyLegChainPattern]
	}

	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\ts := pl.\n\t\tLeg.\n\t\tStore\n\t_ = s\n}\n"); got != 1 {
		t.Errorf("the line-split chain the grep row cannot see was counted %d times, want 1", got)
	}
	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\t_ = pl.Leg.Store\n}\n"); got != 1 {
		t.Errorf("the single-line chain was counted %d times, want 1", got)
	}
	// The parenthesized form is the third spelling and the one that defeats BOTH
	// halves as written: gofmt preserves the parens, the grep row reads `.Leg)`
	// and stops, and an AST rule that required the chain's inner node to be a
	// selector directly walked past the ParenExpr. Nested parens are legal Go and
	// gofmt keeps those too, so the unwrap has to be a loop.
	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\t_ = (pl.Leg).Store\n}\n"); got != 1 {
		t.Errorf("the parenthesized chain was counted %d times, want 1", got)
	}
	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\t_ = ((pl.Leg)).Store\n}\n"); got != 1 {
		t.Errorf("the doubly-parenthesized chain was counted %d times, want 1", got)
	}
	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\t_ = pl.Leg.StoreDir\n}\n"); got != 0 {
		t.Errorf("a different field on the same leg was counted %d times, want 0", got)
	}
	if got := scan(t, "package main\n\nfunc use(pl storeref.ResolvedPlan) {\n\t_ = pl.Legs.Store\n}\n"); got != 0 {
		t.Errorf("a different field holding the chain's outer name was counted %d times, want 0", got)
	}
	if got := scan(t, "package main\n\nfunc use(pl storeref.PlanLeg) {\n\t_ = pl.Leg.Store // "+residencyAllowMarker+" tested escape hatch\n}\n"); got != 0 {
		t.Errorf("the marker did not suppress the chain: counted %d times", got)
	}
	if got := scan(t, "package main\n\nfunc use() {}\n"); got != 0 {
		t.Errorf("a clean file was counted %d times", got)
	}
}
