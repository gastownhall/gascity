package beads

import "testing"

func TestBackendUpdatesFromOptsDoesNotSendMetadataPatchAsReplacement(t *testing.T) {
	updates := backendUpdatesFromOpts(UpdateOpts{
		Metadata: map[string]string{"gc.routed_to": "rig/runner"},
	})
	if _, ok := updates["metadata"]; ok {
		t.Fatalf("backendUpdatesFromOpts forwarded metadata patch as replacement: %#v", updates["metadata"])
	}
}

func TestBackendMergedMetadataPreservesExistingKeys(t *testing.T) {
	merged := mergeBackendMetadataPatch(
		map[string]string{
			"gc.kind":    "control",
			"gc.step_id": "build",
		},
		map[string]string{
			"gc.routed_to": "rig/runner",
		},
	)
	if merged["gc.kind"] != "control" {
		t.Fatalf("gc.kind = %q, want control; metadata=%#v", merged["gc.kind"], merged)
	}
	if merged["gc.step_id"] != "build" {
		t.Fatalf("gc.step_id = %q, want build; metadata=%#v", merged["gc.step_id"], merged)
	}
	if merged["gc.routed_to"] != "rig/runner" {
		t.Fatalf("gc.routed_to = %q, want rig/runner; metadata=%#v", merged["gc.routed_to"], merged)
	}
}

func TestBackendIssueFromBeadIncludesCreateTimeDependencies(t *testing.T) {
	got := backendIssueFromBead(Bead{
		ID:       "gc-child",
		Title:    "Child",
		ParentID: "gc-parent",
		Dependencies: []Dep{{
			DependsOnID: "gc-blocker",
			Type:        "blocks",
		}},
		Needs: []string{"validates:gc-check", "gc-default-blocker"},
	})

	want := []Dep{
		{IssueID: "gc-child", DependsOnID: "gc-blocker", Type: "blocks"},
		{IssueID: "gc-child", DependsOnID: "gc-parent", Type: "parent-child"},
		{IssueID: "gc-child", DependsOnID: "gc-check", Type: "validates"},
		{IssueID: "gc-child", DependsOnID: "gc-default-blocker", Type: "blocks"},
	}
	if len(got.Deps) != len(want) {
		t.Fatalf("deps len = %d, want %d: %#v", len(got.Deps), len(want), got.Deps)
	}
	for i := range want {
		if got.Deps[i] != want[i] {
			t.Fatalf("dep[%d] = %#v, want %#v", i, got.Deps[i], want[i])
		}
	}
}
