package beadmeta

import "slices"

// Named subsets of the gc.kind vocabulary. The sets are DATA: which kinds a
// dispatcher executes, which trigger the graph contract, and what to do per
// kind remain decisions owned by the dispatch/formula/graphroute packages.
//
// The AUTHORITATIVE set is ControlKinds — "kinds the control dispatcher can
// execute" — whose behavior owner is the ProcessControl switch in
// internal/dispatch/runtime.go (exactly one case per member; unknown kinds
// hard-error). Every other control-routing predicate is, by intent, equal to
// ControlKinds; two predicates currently lag it and carry explicit, documented
// exclusions at their definition sites pending behavior-reviewed routing
// fixes:
//
//   - graphroute.IsControlDispatcherKind excludes KindTally (PR #1194 added
//     tally to compile and ProcessControl but never wired routing, so tally
//     beads are currently routed to workers — tracked as a routing bug).
//   - dispatch.isAttemptControlKind excludes KindTally and KindDrain (frozen
//     2026-04-14 snapshot of the then-complete control set; later kinds were
//     never added).
//
// Three persisted kind values sit outside every set below: KindWisp (wisp
// molecule roots), KindClosed (closed-marker beads), and KindTask (written on
// simple attempt roots by internal/dispatch/control.go). gc.original_kind
// (OriginalKindMetadataKey) also persists values from this vocabulary with no
// current Go reader.
const (
	// KindTask is written on simple attempt roots that are plain work, not
	// control infrastructure.
	KindTask = "task"

	// KindClosed marks beads recording a closed/terminal state.
	KindClosed = "closed"
)

// ControlKinds lists the kinds the control dispatcher executes. The
// ProcessControl switch in internal/dispatch/runtime.go is the behavior owner
// and has exactly one case per member; TestControlKindsExact and the dispatch
// package's coverage test keep the two in lockstep.
var ControlKinds = []string{
	KindRetry,
	KindRalph,
	KindCheck,
	KindRetryEval,
	KindFanout,
	KindTally,
	KindDrain,
	KindScopeCheck,
	KindWorkflowFinalize,
}

// IsControlKind reports whether kind is a member of ControlKinds.
func IsControlKind(kind string) bool {
	return slices.Contains(ControlKinds, kind)
}

// StructuralGraphKinds lists graph-node kinds that structure a compiled
// workflow but are never dispatched as control beads — the ProcessControl
// switch hard-errors on them by design. KindRun and KindRetryRun are v1-era
// attempt kinds kept readable for persisted-bead compatibility (v2 attempt
// beads keep their original kind and carry gc.attempt instead; see commit
// c176a999e).
var StructuralGraphKinds = []string{
	KindScope,
	KindCleanup,
	KindRun,
	KindRetryRun,
}

// WorkflowTopologyKinds lists kinds that anchor workflow topology (root
// workflow, scope latch, formula spec). Routing never lands on these; agents
// must never claim them. graphroute.IsWorkflowTopologyKind derives from this
// set.
var WorkflowTopologyKinds = []string{
	KindWorkflow,
	KindScope,
	KindSpec,
}

// GraphContractMetadataKinds lists the gc.kind values that, when HAND-WRITTEN
// in step metadata, imply graph.v2 semantics and therefore trigger the formula
// compiler requirement (formula.metadataRequiresGraphContract derives from
// this set). It is exactly StructuralGraphKinds ∪ (ControlKinds \ {fanout,
// tally}): the fanout/tally exclusion is intentional — those kinds are
// engine-minted from [steps.on_complete] / [steps.tally], which formula
// validation catches via struct-field checks (commit 2531b9440), so metadata
// coverage is unnecessary for them. KindDrain appears in both detection paths
// (struct field and metadata) as belt-and-suspenders from PR #2784.
// TestKindSetRelationships pins this composition.
var GraphContractMetadataKinds = []string{
	KindScope,
	KindCleanup,
	KindScopeCheck,
	KindWorkflowFinalize,
	KindRetry,
	KindRetryRun,
	KindRetryEval,
	KindRalph,
	KindRun,
	KindCheck,
	KindDrain,
}
