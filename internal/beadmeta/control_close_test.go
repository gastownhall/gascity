package beadmeta

import (
	"slices"
	"testing"
)

func TestControlClosesNodeScopeCheckClosesItsBody(t *testing.T) {
	t.Parallel()

	scopeCheck := map[string]string{
		KindMetadataKey:      KindScopeCheck,
		ScopeRefMetadataKey:  "body",
		ScopeRoleMetadataKey: ScopeRoleControl,
	}
	body := map[string]string{
		KindMetadataKey:      KindScope,
		ScopeRoleMetadataKey: ScopeRoleBody,
	}

	tests := []struct {
		name   string
		nodeID string
		meta   map[string]string
		want   bool
	}{
		{name: "step-local body id", nodeID: "body", meta: body, want: true},
		{name: "formula-namespaced body id", nodeID: "mol-x.body", meta: body, want: true},
		{name: "body identified by gc.step_ref", nodeID: "phys-1", meta: map[string]string{
			KindMetadataKey: KindScope, ScopeRoleMetadataKey: ScopeRoleBody, StepRefMetadataKey: "mol-x.body",
		}, want: true},
		{name: "a different scope's body", nodeID: "mol-x.other-body", meta: body, want: false},
		{name: "suffix without the separator", nodeID: "mol-x.notbody", meta: body, want: false},
		{name: "a scope member, not the body", nodeID: "body", meta: map[string]string{
			ScopeRefMetadataKey: "body", ScopeRoleMetadataKey: ScopeRoleMember,
		}, want: false},
		{name: "the scope-check itself", nodeID: "body", meta: scopeCheck, want: false},
		{name: "a scope latch with the teardown role", nodeID: "body", meta: map[string]string{
			KindMetadataKey: KindScope, ScopeRoleMetadataKey: ScopeRoleTeardown,
		}, want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := ControlClosesNode(scopeCheck, tc.nodeID, tc.meta); got != tc.want {
				t.Fatalf("ControlClosesNode(scope-check, %q) = %v, want %v", tc.nodeID, got, tc.want)
			}
		})
	}
}

func TestControlClosesNodeScopeCheckWithoutScopeRefClosesNothing(t *testing.T) {
	t.Parallel()

	scopeCheck := map[string]string{KindMetadataKey: KindScopeCheck}
	body := map[string]string{KindMetadataKey: KindScope, ScopeRoleMetadataKey: ScopeRoleBody}
	if ControlClosesNode(scopeCheck, "body", body) {
		t.Fatal("a scope-check with no gc.scope_ref must not claim to close any body")
	}
}

func TestControlClosesNodeWorkflowFinalizeClosesTheRoot(t *testing.T) {
	t.Parallel()

	finalize := map[string]string{KindMetadataKey: KindWorkflowFinalize}

	if !ControlClosesNode(finalize, "mol-x", map[string]string{KindMetadataKey: KindWorkflow}) {
		t.Fatal("workflow-finalize must be reported as the closer of the workflow root")
	}
	if ControlClosesNode(finalize, "mol-x.body", map[string]string{KindMetadataKey: KindScope, ScopeRoleMetadataKey: ScopeRoleBody}) {
		t.Fatal("workflow-finalize closes the root, not a scope body")
	}
	if ControlClosesNode(finalize, "mol-x.step", map[string]string{}) {
		t.Fatal("workflow-finalize closes the root, not a plain work step")
	}
}

// TestControlClosesNodeOnlyForSelfClosingKinds pins ControlClosesNode against
// SelfClosingControlKinds: every control kind that closes a foreign graph node
// must be declared in that set, so adding one forces the author to state it
// instead of silently minting a new close cycle.
func TestControlClosesNodeOnlyForSelfClosingKinds(t *testing.T) {
	t.Parallel()

	// Every graph node shape a compiled workflow can produce.
	nodes := []struct {
		id   string
		meta map[string]string
	}{
		{id: "mol-x", meta: map[string]string{KindMetadataKey: KindWorkflow}},
		{id: "mol-x.body", meta: map[string]string{KindMetadataKey: KindScope, ScopeRoleMetadataKey: ScopeRoleBody}},
		{id: "body", meta: map[string]string{KindMetadataKey: KindScope, ScopeRoleMetadataKey: ScopeRoleBody}},
		{id: "mol-x.member", meta: map[string]string{ScopeRefMetadataKey: "body", ScopeRoleMetadataKey: ScopeRoleMember}},
		{id: "mol-x.cleanup", meta: map[string]string{KindMetadataKey: KindCleanup, ScopeRefMetadataKey: "body", ScopeRoleMetadataKey: ScopeRoleTeardown}},
		{id: "mol-x.spec", meta: map[string]string{KindMetadataKey: KindSpec}},
		{id: "mol-x.step", meta: map[string]string{}},
	}

	for _, kind := range ControlKinds {
		control := map[string]string{
			KindMetadataKey:      kind,
			ScopeRefMetadataKey:  "body",
			ScopeRoleMetadataKey: ScopeRoleControl,
		}
		closesSomething := false
		for _, node := range nodes {
			if ControlClosesNode(control, node.id, node.meta) {
				closesSomething = true
			}
		}
		declared := IsSelfClosingControlKind(kind)
		if closesSomething != declared {
			t.Errorf("control kind %q closes a foreign node = %v, but IsSelfClosingControlKind = %v; declare it in SelfClosingControlKinds (and teach ControlClosesNode which node it closes) or prove it closes only itself",
				kind, closesSomething, declared)
		}
	}

	for _, kind := range SelfClosingControlKinds {
		if !slices.Contains(ControlKinds, kind) {
			t.Errorf("SelfClosingControlKinds member %q is not a control kind", kind)
		}
	}
}

func TestNodeIsScope(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		nodeID   string
		meta     map[string]string
		scopeRef string
		want     bool
	}{
		{name: "exact id", nodeID: "body", scopeRef: "body", want: true},
		{name: "namespaced id", nodeID: "mol-x.body", scopeRef: "body", want: true},
		{name: "fully namespaced ref", nodeID: "mol-x.body", scopeRef: "mol-x.body", want: true},
		{name: "step ref", nodeID: "phys-1", meta: map[string]string{StepRefMetadataKey: "mol-x.body"}, scopeRef: "body", want: true},
		{name: "step id", nodeID: "phys-1", meta: map[string]string{StepIDMetadataKey: "body"}, scopeRef: "body", want: true},
		{name: "empty scope ref", nodeID: "body", scopeRef: "", want: false},
		{name: "unrelated id", nodeID: "mol-x.other", scopeRef: "body", want: false},
		{name: "shared suffix without separator", nodeID: "mol-x.subbody", scopeRef: "body", want: false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := NodeIsScope(tc.nodeID, tc.meta, tc.scopeRef); got != tc.want {
				t.Fatalf("NodeIsScope(%q, %v, %q) = %v, want %v", tc.nodeID, tc.meta, tc.scopeRef, got, tc.want)
			}
		})
	}
}
