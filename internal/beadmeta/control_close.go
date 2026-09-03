package beadmeta

import "strings"

// SelfClosingControlKinds lists the control kinds that close a graph node OTHER
// than the control bead itself, where that node is identified by the compiler
// and so can carry a declared dependency edge back onto the control. Those are
// the kinds that can mint the "blocked by my own closer" cycle ControlClosesNode
// detects, and the only ones the compiler can rule out statically.
//
// Behavior owners (internal/dispatch/runtime.go):
//
//   - KindScopeCheck — processScopeCheck resolves the scope body named by the
//     control's gc.scope_ref and closes it (closeScopeAsPassed / abortScope).
//   - KindWorkflowFinalize — processWorkflowFinalize closes the workflow root
//     named by the control's gc.root_bead_id.
//
// Other control kinds are NOT limited to closing themselves: a retry-eval closes
// the logical bead named by its gc.logical_bead_id, and a ralph iteration
// control closes its own logical bead, in both cases while the logical bead
// holds a blocks edge onto the control. Those pairs are safe only because
// internal/dispatch/retry.go closes the control before the logical bead in every
// terminal branch — safety by ordering, enforced at runtime by the strict-close
// test double rather than statically here. They are excluded from this set
// because the edge is minted at dispatch time from runtime metadata, not
// declared by the compiler, so ControlClosesNode cannot see it.
//
// TestControlClosesNodeOnlyForSelfClosingKinds pins this set against
// ControlClosesNode, so adding a control kind that closes a compiler-identified
// foreign node forces the question here instead of silently minting a new cycle.
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
