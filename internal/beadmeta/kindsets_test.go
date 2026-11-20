package beadmeta

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// TestControlKindsExact pins the control-kind vocabulary. The behavior owner is
// the ProcessControl switch in internal/dispatch/runtime.go: adding a kind there
// without adding it here (or vice versa) must fail this test, so the two are
// updated together.
func TestControlKindsExact(t *testing.T) {
	want := []string{
		KindRetry, KindRalph, KindCheck, KindRetryEval, KindFanout,
		KindDrain, KindScopeCheck, KindWorkflowFinalize,
	}
	if !slices.Equal(ControlKinds, want) {
		t.Errorf("ControlKinds = %v, want %v", ControlKinds, want)
	}
	for _, k := range want {
		if !IsControlKind(k) {
			t.Errorf("IsControlKind(%q) = false, want true", k)
		}
	}
	for _, k := range []string{KindWorkflow, KindScope, KindSpec, KindWisp, KindTask, KindRun, KindRetryRun, KindCleanup, "", "nonsense"} {
		if IsControlKind(k) {
			t.Errorf("IsControlKind(%q) = true, want false", k)
		}
	}
}

// TestKindSetRelationships pins the structural relationships between the kind
// sets so membership drift between predicates becomes a test failure instead of
// folklore:
//
//   - control kinds, structural graph kinds, and topology kinds are disjoint
//     vocabulary regions (a kind is dispatched, or structures the graph, or
//     anchors topology — never two of those), except KindScope which is both a
//     structural node and a topology anchor;
//   - the graph-contract metadata trigger is exactly the structural kinds plus
//     the control kinds minus {fanout}. The fanout exclusion is INTENTIONAL
//     (commit 2531b9440): that kind is engine-minted from the
//     [steps.on_complete] authoring surface, which formula validation catches
//     via struct-field checks, so hand-written metadata coverage is not
//     needed for it;
//   - the engine-minted-only kinds are exactly the control kinds excluded from
//     the graph-contract metadata trigger, so together the two sets cover every
//     control kind: hand-writing a control kind in step metadata either demands
//     the graph contract or is rejected outright (ga-cjg11s).
func TestKindSetRelationships(t *testing.T) {
	if dup := firstDuplicate(ControlKinds); dup != "" {
		t.Errorf("ControlKinds contains duplicate %q", dup)
	}
	if dup := firstDuplicate(StructuralGraphKinds); dup != "" {
		t.Errorf("StructuralGraphKinds contains duplicate %q", dup)
	}
	if dup := firstDuplicate(WorkflowTopologyKinds); dup != "" {
		t.Errorf("WorkflowTopologyKinds contains duplicate %q", dup)
	}
	if dup := firstDuplicate(GraphContractMetadataKinds); dup != "" {
		t.Errorf("GraphContractMetadataKinds contains duplicate %q", dup)
	}

	for _, k := range ControlKinds {
		if slices.Contains(StructuralGraphKinds, k) {
			t.Errorf("%q is in both ControlKinds and StructuralGraphKinds", k)
		}
		if slices.Contains(WorkflowTopologyKinds, k) {
			t.Errorf("%q is in both ControlKinds and WorkflowTopologyKinds", k)
		}
	}
	for _, k := range StructuralGraphKinds {
		if slices.Contains(WorkflowTopologyKinds, k) && k != KindScope {
			t.Errorf("%q is in both StructuralGraphKinds and WorkflowTopologyKinds (only KindScope may be)", k)
		}
	}

	var derived []string
	derived = append(derived, StructuralGraphKinds...)
	for _, k := range ControlKinds {
		if k == KindFanout {
			continue
		}
		derived = append(derived, k)
	}
	slices.Sort(derived)
	got := slices.Clone(GraphContractMetadataKinds)
	slices.Sort(got)
	if !slices.Equal(got, derived) {
		t.Errorf("GraphContractMetadataKinds = %v\nwant StructuralGraphKinds ∪ (ControlKinds \\ {fanout}) = %v", got, derived)
	}
}

// TestScopeCheckExemptKindsComposition pins ScopeCheckExemptKinds to its
// declared composition — (ControlKinds \ {retry, ralph, retry-eval}) ∪
// {scope, spec} — so a new control kind forces an explicit decision about
// scope-check pairing instead of silently drifting one of the injection
// predicates (the drift this set was introduced to end; see ga-e154xo).
func TestScopeCheckExemptKindsComposition(t *testing.T) {
	if dup := firstDuplicate(ScopeCheckExemptKinds); dup != "" {
		t.Errorf("ScopeCheckExemptKinds contains duplicate %q", dup)
	}

	var derived []string
	for _, k := range ControlKinds {
		switch k {
		case KindRetry, KindRalph, KindRetryEval:
			continue
		}
		derived = append(derived, k)
	}
	derived = append(derived, KindScope, KindSpec)
	slices.Sort(derived)
	got := slices.Clone(ScopeCheckExemptKinds)
	slices.Sort(got)
	if !slices.Equal(got, derived) {
		t.Errorf("ScopeCheckExemptKinds = %v\nwant (ControlKinds \\ {retry, ralph, retry-eval}) ∪ {scope, spec} = %v", got, derived)
	}

	for _, k := range ScopeCheckExemptKinds {
		if !IsScopeCheckExemptKind(k) {
			t.Errorf("IsScopeCheckExemptKind(%q) = false, want true", k)
		}
	}
	for _, k := range []string{KindRetry, KindRalph, KindRetryEval, KindTask, KindCleanup, KindRun, KindRetryRun, KindWorkflow, KindWisp, "", "nonsense"} {
		if IsScopeCheckExemptKind(k) {
			t.Errorf("IsScopeCheckExemptKind(%q) = true, want false", k)
		}
	}

	if dup := firstDuplicate(EngineMintedOnlyKinds); dup != "" {
		t.Errorf("EngineMintedOnlyKinds contains duplicate %q", dup)
	}
	var mintedOnly []string
	for _, k := range ControlKinds {
		if !slices.Contains(GraphContractMetadataKinds, k) {
			mintedOnly = append(mintedOnly, k)
		}
	}
	slices.Sort(mintedOnly)
	gotMinted := slices.Clone(EngineMintedOnlyKinds)
	slices.Sort(gotMinted)
	if !slices.Equal(gotMinted, mintedOnly) {
		t.Errorf("EngineMintedOnlyKinds = %v\nwant ControlKinds \\ GraphContractMetadataKinds = %v", gotMinted, mintedOnly)
	}
	for _, k := range EngineMintedOnlyKinds {
		if slices.Contains(StructuralGraphKinds, k) {
			t.Errorf("%q is in both EngineMintedOnlyKinds and StructuralGraphKinds", k)
		}
		if slices.Contains(WorkflowTopologyKinds, k) {
			t.Errorf("%q is in both EngineMintedOnlyKinds and WorkflowTopologyKinds", k)
		}
	}
}

// TestEngineKindsComposition pins EngineKinds to its declared composition —
// ControlKinds ∪ StructuralGraphKinds ∪ WorkflowTopologyKinds ∪ {wisp, task,
// closed} — so the set stays exactly the gc.kind vocabulary the engine
// writes. A reader asking "is this the engine's own bead?" (the work-record
// close gate, internal/workrecord) must answer no for anything else: an
// operator-stamped kind (work, review, bug) is operator vocabulary with no
// engine meaning.
func TestEngineKindsComposition(t *testing.T) {
	if dup := firstDuplicate(EngineKinds); dup != "" {
		t.Errorf("EngineKinds contains duplicate %q", dup)
	}

	var derived []string
	derived = append(derived, ControlKinds...)
	derived = append(derived, StructuralGraphKinds...)
	derived = append(derived, WorkflowTopologyKinds...)
	derived = append(derived, KindWisp, KindTask, KindClosed)
	slices.Sort(derived)
	derived = slices.Compact(derived) // KindScope is in two of the unioned sets
	got := slices.Clone(EngineKinds)
	slices.Sort(got)
	if !slices.Equal(got, derived) {
		t.Errorf("EngineKinds = %v\nwant ControlKinds ∪ StructuralGraphKinds ∪ WorkflowTopologyKinds ∪ {wisp, task, closed} = %v", got, derived)
	}

	for _, k := range EngineKinds {
		if !IsEngineKind(k) {
			t.Errorf("IsEngineKind(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"", "work", "review", "bug", " step", "Task"} {
		if IsEngineKind(k) {
			t.Errorf("IsEngineKind(%q) = true, want false", k)
		}
	}

	// Drift guard: every Kind* constant this package declares (the *MetadataKey
	// key names aside) is a value the engine writes, so it must be in
	// EngineKinds. A future engine kind then cannot silently fall outside the
	// set — an engine-minted bead would newly answer "not the engine's" to
	// every IsEngineKind reader.
	for _, val := range declaredKindConstants(t) {
		if !IsEngineKind(val) {
			t.Errorf("declared kind constant %q is not in EngineKinds", val)
		}
	}
}

// declaredKindConstants parses this package's non-test Go files and returns
// the string value of every constant named Kind* that is not a *MetadataKey
// key name — the same source-scan shape guard_test.go uses, scoped to the
// package that declares the vocabulary.
func declaredKindConstants(t *testing.T) []string {
	t.Helper()
	pkgDir := filepath.Join(repoRoot(t), "internal", "beadmeta")
	entries, err := os.ReadDir(pkgDir)
	if err != nil {
		t.Fatalf("reading %s: %v", pkgDir, err)
	}
	fset := token.NewFileSet()
	var values []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, perr := parser.ParseFile(fset, filepath.Join(pkgDir, name), nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", name, perr)
		}
		for _, decl := range file.Decls {
			gen, ok := decl.(*ast.GenDecl)
			if !ok || gen.Tok != token.CONST {
				continue
			}
			for _, spec := range gen.Specs {
				vs, ok := spec.(*ast.ValueSpec)
				if !ok || len(vs.Values) == 0 {
					continue
				}
				for _, nameIdent := range vs.Names {
					if !strings.HasPrefix(nameIdent.Name, "Kind") || strings.HasSuffix(nameIdent.Name, "MetadataKey") {
						continue
					}
					lit, ok := vs.Values[0].(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: constant %s has no string literal value; the drift guard needs one per Kind* declaration", name, nameIdent.Name)
					}
					val, uerr := strconv.Unquote(lit.Value)
					if uerr != nil {
						t.Fatalf("%s: unquoting %s: %v", name, nameIdent.Name, uerr)
					}
					values = append(values, val)
				}
			}
		}
	}
	if len(values) == 0 {
		t.Fatal("the kind-constant scan found no Kind* constants; it is no longer guarding anything")
	}
	return values
}

func firstDuplicate(set []string) string {
	seen := make(map[string]struct{}, len(set))
	for _, k := range set {
		if _, ok := seen[k]; ok {
			return k
		}
		seen[k] = struct{}{}
	}
	return ""
}
