package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The AC-8 census: the interrupted-close recognizer's vocabulary must cover
// every state this binary's own closes stamp. A close is two writes (the
// ClosePatch metadata, then the status flip); when the second write is lost,
// the row carries the close's state code while still open (#2085). The
// finisher can only retake such a close if interruptedCloseGate recognizes
// the code — so a close path stamping a code the recognizer does not know
// re-creates the bug. This census walks the non-test Go under cmd/gc and
// internal and fails on any close vocabulary that escapes the recognizer:
//
//  1. every ClosePatch call's state argument (literal, or a variable matched
//     through the checked-in carrier table below),
//  2. every closeWhy literal's reason, with its gate equal to the
//     recognizer's gate for that reason whenever the reason is a close code
//     (a known-state reason never reaches the finisher, so its gate is the
//     close's own business),
//  3. every SleepReason constant the pool-slot freeable admit lists accept,
//     and
//  4. archived and draining — the documented forward-compat states — are not
//     recognized.
const censusFinisherFile = "cmd/gc/session_reconciler_interrupted_close.go"

// interruptCloseCensusDirs are the trees whose non-test Go the census reads.
var interruptCloseCensusDirs = []string{"cmd/gc", "internal"}

// freeableSleepReasonValues resolves every session.SleepReason constant the
// pool-slot freeable admit lists reference. A constant referenced by a list
// but missing here fails the census (add its resolution); a resolved value
// that is neither a known state nor on the passthrough list fails too.
var freeableSleepReasonValues = map[string]string{
	"SleepReasonIdle":                  string(sessionpkg.SleepReasonIdle),
	"SleepReasonIdleTimeout":           string(sessionpkg.SleepReasonIdleTimeout),
	"SleepReasonCityStop":              string(sessionpkg.SleepReasonCityStop),
	"SleepReasonFailedCreate":          string(sessionpkg.SleepReasonFailedCreate),
	"SleepReasonRuntimeMissing":        string(sessionpkg.SleepReasonRuntimeMissing),
	"SleepReasonProviderTerminalError": string(sessionpkg.SleepReasonProviderTerminalError),
	"SleepReasonMaxSessionAge":         string(sessionpkg.SleepReasonMaxSessionAge),
	"SleepReasonConfigDrift":           string(sessionpkg.SleepReasonConfigDrift),
	"SleepReasonKilled":                string(sessionpkg.SleepReasonKilled),
}

// poolFreeableSleepValues is the value set a pool-freeable row's trimmed
// sleep_reason can carry (the admit lists' constants), plus the drained
// fallback the pool closes stamp when it is empty.
func poolFreeableSleepValues() []string {
	vals := make([]string, 0, len(freeableSleepReasonValues)+1)
	for _, v := range freeableSleepReasonValues {
		vals = append(vals, v)
	}
	return append(vals, "drained")
}

// closeGateIdents resolves a closeWhy gate field written as a gate constant.
var closeGateIdents = map[string]closeGate{
	"closeGated":     closeGated,
	"closeFreeSlot":  closeFreeSlot,
	"closeReleasing": closeReleasing,
	"closeRollback":  closeRollback,
}

// closeStateCarriers is the checked-in table for every non-literal that can
// carry a state into session.ClosePatch or a closeWhy reason, keyed by the
// rendering file:expression. A census hit whose expression is not in this
// table fails: the carrier's value set is the contract, and it must be
// maintained by hand. Entries filled at census time are set in
// interruptCloseCarrierTable below.
var closeStateCarriers = map[string][]string{
	// The close-orphan flow mints its reason two lines above the sites
	// (session_reconciler.go: "orphaned", overridden to "suspended").
	"session_reconciler.go:reason": {"orphaned", "suspended"},
	// The v2 drain close carries the drain decision's reason
	// (reconcile_arms_close.go; allocator_decide.go sets DrainReason).
	"reconcile_arms_close.go:DrainReason": {"orphaned", "suspended"},
	// Constant carriers. The selector qualifier is dropped when rendering,
	// so session.StateFailedCreate and sessionpkg.StateFailedCreate share a
	// key per file.
	"session_beads.go:StateFailedCreate":                  {"failed-create"},
	"session_name_lookup.go:StateFailedCreate":            {"failed-create"},
	"session_reconciler.go:StateFailedCreate":             {"failed-create"},
	"session_lifecycle_parallel.go:StateFailedCreate":     {"failed-create"},
	"session_beads.go:strandedRepairCloseReason":          {"stranded-repair"},
	"reconcile_effect_close.go:strandedRepairCloseReason": {"stranded-repair"},
}

// interruptCloseCarrierTable returns the carrier table with its
// census-computed entries filled: the legacy close's reason param carries the
// union of every closeWhy reason (the literals themselves are censused
// first), the two pool-sleep closes carry the freeable admit list, and the
// store's generic legacy close may carry any close code.
func interruptCloseCarrierTable(closeWhyReasons []string) map[string][]string {
	table := map[string][]string{}
	for k, v := range closeStateCarriers {
		table[k] = v
	}
	full := make([]string, 0, len(interruptedCloseCodeGates)+len(interruptedClosePassthroughCodes)+4)
	for code := range interruptedCloseCodeGates {
		full = append(full, code)
	}
	for code := range interruptedClosePassthroughCodes {
		full = append(full, code)
	}
	full = append(full, "drained", "failed-create", "orphaned", "suspended")
	table["session_beads.go:reason"] = closeWhyReasons
	table["session_reconciler.go:closeReason"] = poolFreeableSleepValues()
	table["reconcile_arms_close.go:cmp.Or"] = poolFreeableSleepValues()
	table["store.go:stateCode"] = full
	return table
}

// interruptCloseStateOK reports whether v is a state the recognizer
// vocabulary covers: a known state, or a code interruptedCloseGate knows.
func interruptCloseStateOK(v string) bool {
	return knownSessionStates[v] || interruptCloseGateFor(v) != 0
}

// interruptCloseGateFor is the recognizer's gate for a state code, 0 when the
// state is not one of this binary's close codes (known states included).
func interruptCloseGateFor(v string) closeGate {
	if g, ok := interruptedCloseCodeGates[v]; ok {
		return g
	}
	if interruptedClosePassthroughCodes[v] {
		return closeGated
	}
	return 0
}

// interruptCloseRender renders a carrier expression for the carrier table's
// file:expression keys: string() conversions unwrap, a selector keeps only
// its final identifier, and cmp.Or collapses to its call name.
func interruptCloseRender(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.BasicLit:
		if v, err := strconv.Unquote(e.Value); err == nil {
			return v
		}
		return e.Value
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	case *ast.CallExpr:
		if len(e.Args) == 1 {
			switch fun := e.Fun.(type) {
			case *ast.Ident:
				if fun.Name == "string" {
					return interruptCloseRender(e.Args[0])
				}
			case *ast.SelectorExpr:
				if fun.Sel.Name == "string" {
					return interruptCloseRender(e.Args[0])
				}
			}
		}
		if fun, ok := e.Fun.(*ast.SelectorExpr); ok && fun.Sel.Name == "Or" {
			return "cmp.Or"
		}
	}
	return "<unhandled>"
}

// TestInterruptedCloseVocabularyCensus is AC-8's guard. See the file comment
// for the four clauses.
func TestInterruptedCloseVocabularyCensus(t *testing.T) {
	root := repoRootForLint(t)

	type whySite struct {
		file         string
		reason, gate ast.Expr
	}
	type patchSite struct {
		file string
		arg  ast.Expr
	}
	var whys []whySite
	var patches []patchSite
	admitLists := 0
	var freeableSleeps []string

	fset := token.NewFileSet()
	for _, dir := range interruptCloseCensusDirs {
		err := filepath.WalkDir(filepath.Join(root, dir), func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(p, ".go") || strings.HasSuffix(p, "_test.go") {
				return nil
			}
			rel, rerr := filepath.Rel(root, p)
			if rerr != nil {
				return rerr
			}
			rel = filepath.ToSlash(rel)
			f, perr := parser.ParseFile(fset, p, nil, 0)
			if perr != nil {
				t.Fatalf("parse %s: %v", rel, perr)
			}
			// The finisher's own closeWhy applies the recognizer's verdict
			// by construction (its reason IS whatever the recognizer
			// accepted); censusing it against itself is vacuous.
			isFinisher := rel == censusFinisherFile
			ast.Inspect(f, func(n ast.Node) bool {
				switch node := n.(type) {
				case *ast.FuncDecl:
					if strings.HasPrefix(node.Name.Name, "isPoolSessionSlotFreeable") {
						admitLists++
						ast.Inspect(node.Body, func(m ast.Node) bool {
							if sel, ok := m.(*ast.SelectorExpr); ok && strings.HasPrefix(sel.Sel.Name, "SleepReason") {
								// Only package-qualified constants (session.SleepReasonIdle);
								// i.SleepReason is the Info field access, not a constant.
								if x, ok := sel.X.(*ast.Ident); ok && (x.Name == "session" || x.Name == "sessionpkg") {
									freeableSleeps = append(freeableSleeps, sel.Sel.Name)
								}
							}
							return true
						})
					}
				case *ast.CallExpr:
					if fun, ok := node.Fun.(*ast.SelectorExpr); ok && fun.Sel.Name == "ClosePatch" && len(node.Args) == 2 && !isFinisher {
						patches = append(patches, patchSite{file: rel, arg: node.Args[1]})
					}
				case *ast.CompositeLit:
					if id, ok := node.Type.(*ast.Ident); ok && id.Name == "closeWhy" && !isFinisher {
						site := whySite{file: rel}
						for _, elt := range node.Elts {
							kv, ok := elt.(*ast.KeyValueExpr)
							if !ok {
								continue
							}
							key, ok := kv.Key.(*ast.Ident)
							if !ok {
								continue
							}
							switch key.Name {
							case "reason":
								site.reason = kv.Value
							case "gate":
								site.gate = kv.Value
							}
						}
						whys = append(whys, site)
					}
				}
				return true
			})
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", dir, err)
		}
	}

	if len(whys) == 0 || len(patches) == 0 {
		t.Fatalf("census found %d closeWhy literals and %d ClosePatch calls — the walk is broken", len(whys), len(patches))
	}
	if admitLists == 0 {
		t.Fatal("census found no isPoolSessionSlotFreeable* admit list — the walk is broken")
	}

	// Resolve the closeWhy reasons first: the legacy close's reason param
	// carries their union. The pre-table carries every census-computed entry
	// except that union (nil), which no closeWhy reason needs — only the
	// legacy close's ClosePatch argument does, and it is checked below
	// against the final table.
	pre := interruptCloseCarrierTable(nil)
	closeWhyUnion := map[string]bool{}
	for _, site := range whys {
		if site.reason == nil {
			t.Fatalf("%s: closeWhy literal without a reason field", site.file)
		}
		key := filepath.Base(site.file) + ":" + interruptCloseRender(site.reason)
		var vals []string
		if tableVals, ok := pre[key]; ok {
			if tableVals == nil {
				t.Fatalf("census bug: carrier %q unresolved before the union exists", key)
			}
			vals = tableVals
		} else if lit, ok := site.reason.(*ast.BasicLit); ok {
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: closeWhy reason literal %q: %v", site.file, lit.Value, err)
			}
			vals = []string{v}
		} else {
			t.Fatalf("%s: closeWhy reason carrier %q is not in closeStateCarriers — add its value set",
				site.file, interruptCloseRender(site.reason))
		}
		for _, v := range vals {
			closeWhyUnion[v] = true
			if !interruptCloseStateOK(v) {
				t.Fatalf("%s: closeWhy reason %q is neither a known state nor recognized by interruptedCloseGate — a half-closed row in it would be held open forever (#2085)", site.file, v)
			}
		}
	}
	union := make([]string, 0, len(closeWhyUnion))
	for v := range closeWhyUnion {
		union = append(union, v)
	}

	// The dynamic table, now that the union exists.
	table := interruptCloseCarrierTable(union)
	resolve := func(t *testing.T, file string, expr ast.Expr) []string {
		t.Helper()
		if lit, ok := expr.(*ast.BasicLit); ok {
			v, err := strconv.Unquote(lit.Value)
			if err != nil {
				t.Fatalf("%s: state literal %q: %v", file, lit.Value, err)
			}
			return []string{v}
		}
		key := filepath.Base(file) + ":" + interruptCloseRender(expr)
		if vals, ok := table[key]; ok {
			return vals
		}
		t.Fatalf("%s: state carrier %q is not in the carrier table — add its value set", file, interruptCloseRender(expr))
		return nil
	}

	// Clause 2: gate equality — a close code's closeWhy gate must equal the
	// recognizer's gate, so a retaken close cascades exactly like the
	// original.
	for _, site := range whys {
		if site.gate == nil {
			continue
		}
		var gates []closeGate
		if ident, ok := site.gate.(*ast.Ident); ok {
			if g, ok := closeGateIdents[ident.Name]; ok {
				gates = []closeGate{g}
			}
		}
		for _, v := range resolve(t, site.file, site.reason) {
			want := interruptCloseGateFor(v)
			if want == 0 {
				continue // a known-state reason never reaches the finisher
			}
			if len(gates) != 1 || gates[0] != want {
				t.Fatalf("%s: closeWhy for %q must gate as the recognizer does (%s) so a retaken close cascades identically; got %v",
					site.file, v, interruptedCloseGateName(want), site.gate)
			}
		}
	}

	// Clause 1: every ClosePatch state argument is in vocabulary.
	for _, site := range patches {
		for _, v := range resolve(t, site.file, site.arg) {
			if !interruptCloseStateOK(v) {
				t.Fatalf("%s: ClosePatch state %q is neither a known state nor recognized by interruptedCloseGate — a half-closed row in it would be held open forever (#2085)", site.file, v)
			}
		}
	}

	// Clause 3: every pool-freeable SleepReason constant resolves to a known
	// state or a passthrough code.
	if len(freeableSleeps) == 0 {
		t.Fatal("the pool-slot freeable admit lists reference no SleepReason constants — the scan is broken")
	}
	for _, name := range freeableSleeps {
		v, ok := freeableSleepReasonValues[name]
		if !ok {
			t.Fatalf("SleepReason constant %q referenced by a pool-slot freeable admit list is not resolved in freeableSleepReasonValues — add its resolution", name)
		}
		if !knownSessionStates[v] && !interruptedClosePassthroughCodes[v] {
			t.Fatalf("pool-freeable sleep reason %q is neither a known state nor on the interrupted-close passthrough list — a half-closed pool row sleeping in it would be held open forever (#2085)", v)
		}
	}

	// Clause 4: the documented forward-compat states stay unrecognized.
	for _, s := range []string{"archived", "draining"} {
		if interruptedCloseCodeGates[s] != 0 || interruptedClosePassthroughCodes[s] {
			t.Fatalf("state %q must not be recognized as an interrupted close: it is the forward-compat skip's own vocabulary", s)
		}
		if g := interruptedCloseGate(sessionpkg.Info{MetadataState: s, SleepReason: s, PoolManaged: true}); g != 0 {
			t.Fatalf("interruptedCloseGate(%q) = %s, want unrecognized", s, interruptedCloseGateName(g))
		}
	}
}
