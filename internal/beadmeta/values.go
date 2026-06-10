package beadmeta

// Value vocabulary for engine-minted structural metadata keys. These are DATA
// declarations only: which kinds a dispatcher accepts, which kinds trigger the
// graph contract, and what an outcome means remain decisions owned by the
// dispatch/formula/delivery packages. Routing keys (gc.routed_to,
// gc.run_target, gc.execution_routed_to) deliberately have no value vocabulary
// here — their values are config-supplied agent identities, and enumerating
// them would hardcode role names (forbidden by the ZERO-hardcoded-roles rule).

// Values of KindMetadataKey ("gc.kind"). Three predicates over this vocabulary
// coexist with deliberately different membership: the control-dispatch switch
// (internal/dispatch/runtime.go), the attempt-routing predicate
// (internal/dispatch/control.go isAttemptControlKind), and the graph-contract
// trigger (internal/formula/types.go metadataRequiresGraphContract). Each
// builds its own set from these constants; this block does not define which
// set is "right".
const (
	// Control-bead kinds processed by the control dispatcher.
	KindRetry            = "retry"
	KindRalph            = "ralph"
	KindCheck            = "check"
	KindRetryEval        = "retry-eval"
	KindFanout           = "fanout"
	KindTally            = "tally"
	KindDrain            = "drain"
	KindScopeCheck       = "scope-check"
	KindWorkflowFinalize = "workflow-finalize"

	// Structural graph-node kinds: compiled into graphs, never dispatched as
	// control beads (the dispatch switch hard-errors on them).
	KindScope    = "scope"
	KindCleanup  = "cleanup"
	KindRun      = "run"
	KindRetryRun = "retry-run"

	// KindWorkflow marks a workflow root bead.
	KindWorkflow = "workflow"
)

// Values of OutcomeMetadataKey ("gc.outcome").
const (
	OutcomePass    = "pass"
	OutcomeFail    = "fail"
	OutcomeSkipped = "skipped"
)

// Values of FailureClassMetadataKey ("gc.failure_class").
const (
	FailureClassTransient = "transient"
	FailureClassHard      = "hard"
)
