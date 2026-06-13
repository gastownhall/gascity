package beadmeta

import "slices"

// Named subsets of the gc.kind vocabulary. The sets are DATA: which kinds a
// dispatcher executes, which trigger the graph contract, and what to do per
// kind remain decisions owned by the dispatch/formula/graphroute packages.
//
// The AUTHORITATIVE set is ControlKinds — "kinds the control dispatcher can
// execute" — whose behavior owner is the ProcessControl switch in
// internal/dispatch/runtime.go (exactly one case per member; unknown kinds
// hard-error). Every control-routing predicate is exactly equal to
// ControlKinds (graphroute.IsControlDispatcherKind and
// dispatch.isAttemptControlKind both derive from IsControlKind, and each has
// a lockstep test pinning the equality).
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
	KindDrain,
	KindScopeCheck,
	KindWorkflowFinalize,
}

// IsControlKind reports whether kind is a member of ControlKinds.
func IsControlKind(kind string) bool {
	return slices.Contains(ControlKinds, kind)
}

// ScopeCheckExemptKinds lists the gc.kind values that never receive a paired
// scope-check control, even when the step carries gc.scope_ref. It is exactly
// (ControlKinds \ {KindRetry, KindRalph, KindRetryEval}) ∪ {KindScope,
// KindSpec}; TestScopeCheckExemptKindsComposition pins the composition.
//
// Rationale per member: KindScope is the scope latch itself; KindSpec marks
// frozen step-spec sidecars (bookkeeping, not work); the remaining members are
// control kinds whose terminal scope reconciliation is owned by the control
// runtime (fanout reconciles its enclosing scope on close, scope-check IS the
// reconciler, workflow-finalize runs at root level, and check beads are closed
// by their owning ralph control, which reconciles). KindRetry and KindRalph
// stay non-exempt on purpose: their controls pair with scope-checks in
// addition to their own close-time reconciliation (NDI redundancy), and the
// scope-check's isRetryAttemptSubject branch depends on that pairing.
//
// Consumers (the judgment of WHEN to inject stays at each site):
//
//   - formula.needsScopeCheck — compile-path injection (internal/formula/graph.go)
//   - formula.recipeStepNeedsScopeCheck — dynamic-fragment injection
//     (internal/formula/fragment.go)
//   - dispatch.attemptRecipeStepNeedsScopeCheck — attempt-recipe injection
//     (internal/dispatch/control.go)
//   - formula.markRalphBodyOutputSinks — ralph body output-sink marking
//     (internal/formula/ralph.go) additionally exempts KindRalph at its
//     definition site; control beads are never worker-executed, so none of
//     these kinds can honor gc.output_json_required.
//
// Drain controls reconcile their enclosing scope on terminal close
// (reconcileClosedDrainScope in internal/dispatch/drain.go), matching the
// fanout/retry/ralph close-time behavior, so exemption from scope-check
// pairing never strands a scope latch.
var ScopeCheckExemptKinds = []string{
	KindScope,
	KindScopeCheck,
	KindWorkflowFinalize,
	KindFanout,
	KindCheck,
	KindDrain,
	KindSpec,
}

// IsScopeCheckExemptKind reports whether kind is a member of
// ScopeCheckExemptKinds.
func IsScopeCheckExemptKind(kind string) bool {
	return slices.Contains(ScopeCheckExemptKinds, kind)
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
// this set). It is exactly StructuralGraphKinds ∪ (ControlKinds \ {fanout}):
// the fanout exclusion is intentional — that kind is engine-minted from
// [steps.on_complete], which formula validation catches via struct-field
// checks (commit 2531b9440), so it is covered by EngineMintedOnlyKinds
// instead (hand-writing it is a validation error, not a contract trigger).
// KindDrain appears in both detection paths (struct field and metadata) as
// belt-and-suspenders from PR #2784. TestKindSetRelationships pins this
// composition.
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

// EngineMintedOnlyKinds lists the gc.kind values that only the formula
// compiler may mint: fanout control beads are expanded from the
// [steps.on_complete] authoring surface (formula ApplyGraphControls), and no
// hand-authoring surface exists for them. Hand-writing these values in step
// metadata is rejected by formula validation (the behavior owner is
// Formula.Validate via validateEngineMintedKindMetadata, ga-cjg11s) —
// otherwise the bead would pass validation and the legacy routing path would
// stamp it onto a worker instead of the control dispatcher. It is exactly
// ControlKinds \ GraphContractMetadataKinds; TestKindSetRelationships pins
// this composition.
var EngineMintedOnlyKinds = []string{
	KindFanout,
}
