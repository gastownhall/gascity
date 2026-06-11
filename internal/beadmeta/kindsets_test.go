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
		KindTally, KindDrain, KindScopeCheck, KindWorkflowFinalize,
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
//     the control kinds minus {fanout, tally}. The fanout/tally exclusion is
//     INTENTIONAL (commit 2531b9440): those kinds are engine-minted from
//     [steps.on_complete] / [steps.tally] authoring surfaces, which formula
//     validation catches via struct-field checks, so hand-written metadata
//     coverage is not needed for them.
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
		if k == KindFanout || k == KindTally {
			continue
		}
		derived = append(derived, k)
	}
	slices.Sort(derived)
	got := slices.Clone(GraphContractMetadataKinds)
	slices.Sort(got)
	if !slices.Equal(got, derived) {
		t.Errorf("GraphContractMetadataKinds = %v\nwant StructuralGraphKinds ∪ (ControlKinds \\ {fanout, tally}) = %v", got, derived)
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
