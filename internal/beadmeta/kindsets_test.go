package beadmeta

import (
	"slices"
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
// closed} — and pins that an operator's own classification is not a member.
// The work-record close gate (internal/workrecord.Gated) exempts exactly the
// members, so a kind missing here would put engine beads under the gate, and
// an extra member would let an operator's beads skip it.
func TestEngineKindsComposition(t *testing.T) {
	if dup := firstDuplicate(EngineKinds); dup != "" {
		t.Errorf("EngineKinds contains duplicate %q", dup)
	}

	var derived []string
	for _, set := range [][]string{ControlKinds, StructuralGraphKinds, WorkflowTopologyKinds, {KindWisp, KindTask, KindClosed}} {
		for _, k := range set {
			if !slices.Contains(derived, k) {
				derived = append(derived, k)
			}
		}
	}
	slices.Sort(derived)
	got := slices.Clone(EngineKinds)
	slices.Sort(got)
	if !slices.Equal(got, derived) {
		t.Errorf("EngineKinds = %v\nwant ControlKinds ∪ StructuralGraphKinds ∪ WorkflowTopologyKinds ∪ {wisp, task, closed} = %v", got, derived)
	}

	// Every declared gc.kind constant is the engine's own, including any that
	// sits outside the named sets above.
	for _, k := range []string{
		KindRetry, KindRalph, KindCheck, KindRetryEval, KindFanout, KindDrain,
		KindScopeCheck, KindWorkflowFinalize, KindScope, KindCleanup, KindRun,
		KindRetryRun, KindWorkflow, KindWisp, KindSpec, KindTask, KindClosed,
	} {
		if !IsEngineKind(k) {
			t.Errorf("IsEngineKind(%q) = false, want true", k)
		}
	}
	for _, k := range []string{"", "work", "review", "bug", "step", "Workflow", " task"} {
		if IsEngineKind(k) {
			t.Errorf("IsEngineKind(%q) = true, want false", k)
		}
	}
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
