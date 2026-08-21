package beadmeta

import "strings"

// SelfClosingControlKinds lists the control kinds whose completion closes a
// graph node OTHER than the control bead itself. Every other member of
// ControlKinds closes only itself, so only these kinds can create the
// "blocked by my own closer" cycle that ControlClosesNode detects.
//
// Behavior owners (internal/dispatch/runtime.go):
//
//   - KindScopeCheck — processScopeCheck resolves the scope body named by the
//     control's gc.scope_ref and closes it (closeScopeAsPassed / abortScope).
//   - KindWorkflowFinalize — processWorkflowFinalize closes the workflow root
//     named by the control's gc.root_bead_id.
//
// TestControlClosesNodeOnlyForSelfClosingKinds pins this set against
// ControlClosesNode, so adding a control kind that closes a foreign node
// forces the question here instead of silently minting a new cycle.
var SelfClosingControlKinds = []string{
	KindScopeCheck,
	KindWorkflowFinalize,
}

// IsSelfClosingControlKind reports whether kind is a member of
// SelfClosingControlKinds.
func IsSelfClosingControlKind(kind string) bool {
	for _, candidate := range SelfClosingControlKinds {
		if candidate == kind {
			return true
		}
	}
	return false
}

// ControlClosesNode reports whether completing the control node described by
// controlMeta closes the graph node identified by nodeID/nodeMeta.
//
// This is the predicate behind one structural invariant: a node must never
// carry a readiness-blocking dependency on a control that closes it. Such an
// edge is a permanent deadlock — the store refuses to close a blocked issue
// ("cannot close blocked issue"), and the only bead that could clear the
// blocker is the one being refused (ga-a6zy9).
//
// Callers pass compiler-level nodes (formula steps or recipe steps), so the
// resolution is by declared identity rather than by store lookup.
func ControlClosesNode(controlMeta map[string]string, nodeID string, nodeMeta map[string]string) bool {
	switch controlMeta[KindMetadataKey] {
	case KindScopeCheck:
		if nodeMeta[KindMetadataKey] != KindScope || nodeMeta[ScopeRoleMetadataKey] != ScopeRoleBody {
			return false
		}
		return NodeIsScope(nodeID, nodeMeta, controlMeta[ScopeRefMetadataKey])
	case KindWorkflowFinalize:
		return nodeMeta[KindMetadataKey] == KindWorkflow
	default:
		return false
	}
}

// NodeIsScope reports whether the compiler node identified by nodeID/nodeMeta
// is the scope named by scopeRef. Authored scope refs are step-local
// ("body") while compiled node IDs are formula-namespaced
// ("mol-x.body"), so a namespaced suffix match counts — mirroring the
// runtime resolution in dispatch.matchesScopeRef.
func NodeIsScope(nodeID string, nodeMeta map[string]string, scopeRef string) bool {
	if scopeRef == "" {
		return false
	}
	for _, candidate := range []string{nodeID, nodeMeta[StepRefMetadataKey], nodeMeta[StepIDMetadataKey]} {
		if candidate == "" {
			continue
		}
		if candidate == scopeRef || strings.HasSuffix(candidate, "."+scopeRef) {
			return true
		}
	}
	return false
}
