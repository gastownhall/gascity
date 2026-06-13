package beadmeta

// Value vocabulary for engine-minted structural metadata keys. These are DATA
// declarations only: which kinds a dispatcher accepts, which kinds trigger the
// graph contract, and what an outcome means remain decisions owned by the
// dispatch/formula/delivery packages. Routing keys (gc.routed_to,
// gc.run_target, gc.execution_routed_to) deliberately have no value vocabulary
// here — their values are config-supplied agent identities, and enumerating
// them would hardcode role names (forbidden by the ZERO-hardcoded-roles rule).

// Values of KindMetadataKey ("gc.kind"). Several predicates over this
// vocabulary coexist with different membership; the named subsets, their
// relationships, and the authoritative set are declared in kindsets.go
// (ControlKinds is authoritative; every routing predicate derives from it).
const (
	// Control-bead kinds processed by the control dispatcher.
	KindRetry            = "retry"
	KindRalph            = "ralph"
	KindCheck            = "check"
	KindRetryEval        = "retry-eval"
	KindFanout           = "fanout"
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

	// KindWisp marks the root bead of a root-only wisp molecule.
	KindWisp = "wisp"

	// KindSpec marks a generated step-spec sidecar bead carrying a serialized
	// step definition rather than executable work.
	KindSpec = "spec"
)

// Values of OutcomeMetadataKey ("gc.outcome").
const (
	OutcomePass    = "pass"
	OutcomeFail    = "fail"
	OutcomeSkipped = "skipped"

	// OutcomeMissingRoot records a control bead closed because its workflow
	// root vanished from the store (see closeOrphanedControl in
	// internal/dispatch/runtime.go).
	OutcomeMissingRoot = "missing_root"
)

// Values of WorkOutcomeMetadataKey ("gc.work_outcome"), the typed work-record
// close disposition (ADR-0009). Deliberately disjoint from the control-plane
// OutcomeMetadataKey vocabulary above so the two never collide on one key. Only
// WorkOutcomeShipped carries an artifact (a commit on the work branch); the
// "shipped requires a reachable commit" rule is owned by the close gate in
// cmd/gc, not declared here.
const (
	WorkOutcomeShipped   = "shipped"
	WorkOutcomeNoOp      = "no-op"
	WorkOutcomeBlocked   = "blocked"
	WorkOutcomeAbandoned = "abandoned"
)

// Failure-class vocabulary, shared by FailureClassMetadataKey
// ("gc.failure_class") and its sibling keys LastFailureClassMetadataKey
// ("gc.last_failure_class") and ControllerErrorClassMetadataKey
// ("gc.controller_error_class") — all three classify a failure as retryable or
// not using the same value strings.
const (
	FailureClassTransient = "transient"
	FailureClassHard      = "hard"
)

// FormulaContractGraphV2 is the value of FormulaContractMetadataKey
// ("gc.formula_contract") marking a workflow compiled under the graph.v2
// contract — the single most-branched-on metadata value in the engine.
const FormulaContractGraphV2 = "graph.v2"

// Values of ScopeRoleMetadataKey ("gc.scope_role"): the role a bead plays
// inside a scope. The scope reconciler in internal/dispatch/runtime.go branches
// on exact agreement between writers (dispatch, formula, cmd/gc) and readers.
const (
	ScopeRoleBody     = "body"
	ScopeRoleMember   = "member"
	ScopeRoleControl  = "control"
	ScopeRoleTeardown = "teardown"
)

// Values of DrainStateMetadataKey ("gc.drain_state"): the drain control
// state machine. The empty string is a valid initial state (treated as
// pending); do not add validation that rejects "".
const (
	DrainStatePending    = "pending"
	DrainStateExpanding  = "expanding"
	DrainStateExpanded   = "expanded"
	DrainStateCompleting = "completing"
	DrainStateSucceeded  = "succeeded"
	DrainStateFailed     = "failed"
)

// Spawn-state vocabulary shared by RetryStateMetadataKey ("gc.retry_state")
// and FanoutStateMetadataKey ("gc.fanout_state"): both keys run the identical
// crash-resume state machine. The empty string is a valid initial state.
const (
	SpawnStateSpawning = "spawning"
	SpawnStateSpawned  = "spawned"
)

// Disposition vocabulary for FinalDispositionMetadataKey
// ("gc.final_disposition"). DispositionHardFail and DispositionSoftFail are
// also the legal values of OnExhaustedMetadataKey ("gc.on_exhausted"), which
// selects the disposition applied when retries are exhausted. DispositionPass
// shares its string with OutcomePass but is a distinct domain.
const (
	DispositionPass              = "pass"
	DispositionHardFail          = "hard_fail"
	DispositionSoftFail          = "soft_fail"
	DispositionControllerError   = "controller_error"
	DispositionOrphanedWorkflow  = "orphaned_workflow"
	DispositionControlQuarantine = "control_quarantined"
)

// Values of FanoutModeMetadataKey ("gc.fanout_mode").
const (
	FanoutModeParallel   = "parallel"
	FanoutModeSequential = "sequential"
)

// Values of DrainContextMetadataKey ("gc.drain_context").
const (
	DrainContextSeparate = "separate"
	DrainContextShared   = "shared"
)

// Values of DrainMemberAccessMetadataKey ("gc.drain_member_access").
const (
	DrainMemberAccessRead      = "read"
	DrainMemberAccessExclusive = "exclusive"
)

// Values of DrainOnItemFailureMetadataKey ("gc.drain_on_item_failure").
const (
	DrainOnItemFailureSkipRemaining = "skip_remaining"
	DrainOnItemFailureContinue      = "continue"
)

// CheckModeExec is the sole value of CheckModeMetadataKey ("gc.check_mode");
// formula validation and dispatch enforce it independently, so the constant
// ties the contract together.
const CheckModeExec = "exec"

// Values of ScopeKindMetadataKey ("gc.scope_kind").
const (
	ScopeKindCity = "city"
	ScopeKindRig  = "rig"
)
