// Package beadmeta is the single named home for Gas City's engine-owned
// bead-metadata key vocabulary — the "gc." namespace written into and read
// from bead.Metadata across the workflow engine, role workers, CLI, and API.
//
// Before beadmeta, these keys were ~126 raw string literals scattered across
// ~70 files with no central declaration: the real interface between modules was
// folklore. This package makes the seam named and compiler-checked. It imports
// only the standard library, so every workflow package can import it without
// risk of an import cycle, mirroring internal/events.
//
// Scope: this package owns engine-touched bead-metadata KEY NAMES only. It
// deliberately excludes (each a separate owner): gc.* event-type names
// (internal/events.KnownEventTypes), telemetry metric names (internal/telemetry),
// JSON schema-version contract strings (e.g. gc.dolt.cleanup.v1), and the
// t3bridge UI thread-metadata namespace (internal/runtime/t3bridge). Pack- and
// prompt-private keys that no Go file reads stay open-world by design and are
// intentionally NOT declared here (see KnownMetadataPrefixes for the dynamic
// gc.var.<name> case).
//
// A const block (not a runtime registry) is the right mechanism: bead.Metadata
// is map[string]string with no runtime decode/codegen consumer, so the
// events.RegisterPayload reflect-based machinery would add init-ordering cost for
// no benefit. Drift is enforced statically by the AST guard in guard_test.go.
//
// Cross-repo note: bd (the beads backend) stores these keys as opaque JSON in
// its generic issues.metadata column and never interprets them — the key
// names are 100% gascity-minted vocabulary, so changing bd's schema does not
// require touching this package and vice versa. See
// engdocs/design/beads-dolt-contract-redesign.md for the storage contract.
package beadmeta

// Namespace is the reserved prefix for every engine-minted bead-metadata key.
// Runtime guards that reserve the namespace (e.g. rejecting caller-supplied
// "gc."-prefixed keys) compare against this single source of truth.
const Namespace = "gc."

// Engine-owned bead-metadata keys. Each constant is the exact string written to
// or read from bead.Metadata by at least one non-test Go file under internal/ or
// cmd/. Keep this block sorted by identifier; the Go compiler rejects duplicate
// identifiers, giving us a free compile-time uniqueness guarantee.
const (
	AttemptLogMetadataKey                = "gc.attempt_log"
	AttemptMetadataKey                   = "gc.attempt"
	BondMetadataKey                      = "gc.bond"
	BondVarsMetadataKey                  = "gc.bond_vars"
	BrainParentSIDMetadataKey            = "gc.brain_parent_sid"
	CheckInfraRetryMetadataKey           = "gc.check_infra_retry"
	CheckModeMetadataKey                 = "gc.check_mode"
	CheckPathMetadataKey                 = "gc.check_path"
	CheckTimeoutMetadataKey              = "gc.check_timeout"
	CityPathMetadataKey                  = "gc.city_path"
	ClosedByAttemptMetadataKey           = "gc.closed_by_attempt"
	ContinuationGroupMetadataKey         = "gc.continuation_group"
	ControlDispatcherFallbackMetadataKey = "gc.control_dispatcher_fallback"
	ControlEpochMetadataKey              = "gc.control_epoch"
	ControlForMetadataKey                = "gc.control_for"
	ControlQuarantineReasonMetadataKey   = "gc.control_quarantine_reason"
	ControlQuarantinedAtMetadataKey      = "gc.control_quarantined_at"
	ControlQuarantinedMetadataKey        = "gc.control_quarantined"
	ControllerErrorClassMetadataKey      = "gc.controller_error_class"
	ControllerErrorMetadataKey           = "gc.controller_error"
	ControllerRetryableMetadataKey       = "gc.controller_retryable"
	CurrentRunIDMetadataKey              = "gc.current_run_id"
	CwdMetadataKey                       = "gc.cwd"
	// ActiveWorkBeadMetadataKey is the session bead's current-pointer to the STEP it
	// is executing — the work bead's bare gc.step_id (NOT its namespaced bead id),
	// stamped at the claim hook and read at the usage record site to populate
	// usage.Fact.StepID. Empty when the current work has no formula step (ad-hoc /
	// manual), matching the events plane. See engdocs/design/active-work-bead-v0.md.
	ActiveWorkBeadMetadataKey            = "gc.active_work_bead"
	DeferredAssigneeMetadataKey          = "gc.deferred_assignee"
	DeferredExecutionRoutedToMetadataKey = "gc.deferred_execution_routed_to"
	DeferredRoutedToMetadataKey          = "gc.deferred_routed_to"
	DeferredTypeMetadataKey              = "gc.deferred_type"
	DetachedMetadataKey                  = "gc.detached"
	DrainContextMetadataKey              = "gc.drain_context"
	DrainContinuationGroupMetadataKey    = "gc.drain_continuation_group"
	DrainControlIDMetadataKey            = "gc.drain_control_id"
	DrainCountMetadataKey                = "gc.drain_count"
	DrainFormulaMetadataKey              = "gc.drain_formula"
	DrainIndexMetadataKey                = "gc.drain_index"
	DrainItemSingleLaneMetadataKey       = "gc.drain_item_single_lane"
	DrainManifestMetadataKey             = "gc.drain_manifest.v1"
	DrainMaxUnitsMetadataKey             = "gc.drain_max_units"
	DrainMemberAccessMetadataKey         = "gc.drain_member_access"
	DrainMemberIDMetadataKey             = "gc.drain_member_id"
	DrainMemberUnresolvedMetadataKey     = "gc.drain_member_unresolved"
	DrainOnItemFailureMetadataKey        = "gc.drain_on_item_failure"
	DrainParentConvoyIDMetadataKey       = "gc.drain_parent_convoy_id"
	DrainStateMetadataKey                = "gc.drain_state"
	DrainUnitKeyMetadataKey              = "gc.drain_unit_key"
	DurationMsMetadataKey                = "gc.duration_ms"
	DynamicFragmentMetadataKey           = "gc.dynamic_fragment"
	ExclusiveDrainReservationMetadataKey = "gc.exclusive_drain_reservation"
	ExecutionRigContextMetadataKey       = "gc.execution_rig_context"
	ExecutionRoutedToMetadataKey         = "gc.execution_routed_to"
	ExitCodeMetadataKey                  = "gc.exit_code"
	FailedAttemptMetadataKey             = "gc.failed_attempt"
	FailureClassMetadataKey              = "gc.failure_class"
	FailureOwnerMetadataKey              = "gc.failure_owner"
	FailureReasonMetadataKey             = "gc.failure_reason"
	FailureSubjectMetadataKey            = "gc.failure_subject"
	FanoutModeMetadataKey                = "gc.fanout_mode"
	FanoutStateMetadataKey               = "gc.fanout_state"
	FinalDispositionMetadataKey          = "gc.final_disposition"
	ForEachMetadataKey                   = "gc.for_each"
	FormulaMetadataKey                   = "gc.formula"
	FormulaContractMetadataKey           = "gc.formula_contract"
	FormulaHashMetadataKey               = "gc.formula_hash"
	FormulaNameMetadataKey               = "gc.formula_name"
	FormulaSourceMetadataKey             = "gc.formula_source"
	GCExemptMetadataKey                  = "gc.gc_exempt"
	Graphv2RootKeyMetadataKey            = "gc.graphv2_root_key"
	IdempotencyKeyMetadataKey            = "gc.idempotency_key"
	InputConvoyIDMetadataKey             = "gc.input_convoy_id"
	InstantiatingMetadataKey             = "gc.instantiating"
	IterationMetadataKey                 = "gc.iteration"
	ItemRootKeyMetadataKey               = "gc.item_root_key"
	KindMetadataKey                      = "gc.kind"
	LastFailureClassMetadataKey          = "gc.last_failure_class"
	LastFinalizeErrorMetadataKey         = "gc.last_finalize_error"
	LastHeartbeatAtMetadataKey           = "gc.last_heartbeat_at"
	LogicalBeadIDMetadataKey             = "gc.logical_bead_id"
	MaxAttemptsMetadataKey               = "gc.max_attempts"
	MissingRootBeadIDMetadataKey         = "gc.missing_root_bead_id"
	ModelMetadataKey                     = "gc.model"
	NextAttemptMetadataKey               = "gc.next_attempt"
	OnExhaustedMetadataKey               = "gc.on_exhausted"
	OnFailMetadataKey                    = "gc.on_fail"
	OriginalKindMetadataKey              = "gc.original_kind"
	OutcomeBeadIDMetadataKey             = "gc.outcome_bead_id"
	OutcomeMetadataKey                   = "gc.outcome"
	OutputJSONMetadataKey                = "gc.output_json"
	OutputJSONRequiredMetadataKey        = "gc.output_json_required"
	ParentBeadIDMetadataKey              = "gc.parent_bead_id"
	ParentConvoyIDMetadataKey            = "gc.parent_convoy_id"
	PartialFragmentMetadataKey           = "gc.partial_fragment"
	PartialRetryMetadataKey              = "gc.partial_retry"
	PackMetadataKey                      = "gc.pack"
	PackRootMetadataKey                  = "gc.pack_root"
	PackWorkspaceMetadataKey             = "gc.pack_workspace"
	PerDispatchModelMetadataKey          = "gc.per_dispatch_model"
	RalphStepIDMetadataKey               = "gc.ralph_step_id"
	ReasoningMetadataKey                 = "gc.reasoning"
	RequiredArtifactMetadataKey          = "gc.required_artifact"
	RequiredArtifactsMetadataKey         = "gc.required_artifacts"
	RetryCountMetadataKey                = "gc.retry_count"
	RetryFromMetadataKey                 = "gc.retry_from"
	RetrySessionRecycledMetadataKey      = "gc.retry_session_recycled"
	RetryStateMetadataKey                = "gc.retry_state"
	RigRootMetadataKey                   = "gc.rig_root"
	RootBeadIDMetadataKey                = "gc.root_bead_id"
	RootStoreRefMetadataKey              = "gc.root_store_ref"
	RoutedToMetadataKey                  = "gc.routed_to"
	RunTargetMetadataKey                 = "gc.run_target"
	RuntimeVarsMetadataKey               = "gc.graphv2_vars.v1"
	ScopeKindMetadataKey                 = "gc.scope_kind"
	ScopeNameMetadataKey                 = "gc.scope_name"
	ScopeRefMetadataKey                  = "gc.scope_ref"
	ScopeRoleMetadataKey                 = "gc.scope_role"
	SessionAffinityMetadataKey           = "gc.session_affinity"
	SessionIDMetadataKey                 = "gc.session_id"
	// SessionIDCamelMetadataKey is the camelCase variant some bead writers stamp
	// alongside the snake_case SessionIDMetadataKey; both are read when resolving a
	// bead's session link.
	SessionIDCamelMetadataKey = "gc.sessionId"
	SessionNameMetadataKey    = "gc.session_name"
	// SessionNameCamelMetadataKey is the camelCase variant of SessionNameMetadataKey,
	// mirroring SessionIDCamelMetadataKey.
	SessionNameCamelMetadataKey    = "gc.sessionName"
	SourceBeadIDMetadataKey        = "gc.source_bead_id"
	SourceStepSpecMetadataKey      = "gc.source_step_spec"
	SourceStoreRefMetadataKey      = "gc.source_store_ref"
	SpawnedCountMetadataKey        = "gc.spawned_count"
	SpecForMetadataKey             = "gc.spec_for"
	SpecForRefMetadataKey          = "gc.spec_for_ref"
	StderrMetadataKey              = "gc.stderr"
	StdoutMetadataKey              = "gc.stdout"
	StepIDMetadataKey              = "gc.step_id"
	StepRefMetadataKey             = "gc.step_ref"
	StepTimeoutMetadataKey         = "gc.step_timeout"
	SyntheticKindMetadataKey       = "gc.synthetic_kind"
	SyntheticMetadataKey           = "gc.synthetic"
	TemplateMetadataKey            = "gc.template"
	TerminalMetadataKey            = "gc.terminal"
	TriggerBeadIDMetadataKey       = "gc.trigger_bead_id"
	TriggerBeadStoreRefMetadataKey = "gc.trigger_bead_store_ref"
	TruncatedMetadataKey           = "gc.truncated"
	WorkBranchMetadataKey          = "gc.work_branch"
	WorkCommitMetadataKey          = "gc.work_commit"
	WorkDirMetadataKey             = "gc.work_dir"
	WorkOutcomeMetadataKey         = "gc.work_outcome"
	WorkVerificationMetadataKey    = "gc.work_verification"
	WorkflowIDMetadataKey          = "gc.workflow_id"
)

// Work-record metadata keys (ADR-0009). These bind a work bead to its claim
// and its outcome so observability/eval can answer "what work was done, by
// whom, with what artifact, to what end":
//
//   - WorkBranchMetadataKey ("gc.work_branch") — the git branch the claiming
//     worker is on; the durable handle from the bead to its work. Stamped at
//     claim time alongside WorkDirMetadataKey and read by the close gate.
//   - WorkOutcomeMetadataKey ("gc.work_outcome") — the typed close disposition,
//     one of "shipped" | "no-op" | "blocked" | "abandoned". Deliberately NOT
//     OutcomeMetadataKey ("gc.outcome"): that key is the control-plane step
//     result ("pass"/"fail"/"skipped") read by internal/dispatch, a disjoint
//     vocabulary that must not be overloaded.
//   - WorkCommitMetadataKey ("gc.work_commit") — the commit SHA that satisfied
//     the bead; required when the outcome is "shipped" and validated reachable
//     on WorkBranchMetadataKey by the close gate. Named in the gc.work_* family
//     (not a bare "gc.commit") to avoid collision with future commit concepts.
//   - WorkVerificationMetadataKey ("gc.work_verification") — the verification
//     record (gate result, "manual", or a link) backing a shipped outcome.
//
// The set of valid WorkOutcomeMetadataKey values and the "shipped requires a
// commit on the branch" rule live with the close gate in cmd/gc.

// FormulaVarPrefix is the dynamic key prefix under which formula-supplied
// variables are written as gc.var.<name>. The suffix is open-world (a
// user-authored variable name), so it is declared as a prefix, not enumerated.
const FormulaVarPrefix = Namespace + "var."

// Directory keys: a deliberate non-"gc."-prefixed sibling family on bead
// metadata, declared here so the vocabulary has one home. Their read/write
// fallback semantics (canonical-then-legacy) live with their owner in
// internal/beads/contract/metadata.go, which aliases these constants. They are
// not in KnownMetadataKeys because the drift guard's key-shape rule only
// covers the gc. namespace. Note these are distinct from the gc.-prefixed
// WorkDirMetadataKey ("gc.work_dir") above.
const (
	// WorkerDirMetadataKey records the agent process working directory on
	// session beads.
	WorkerDirMetadataKey = "worker_dir"

	// ArtifactDirMetadataKey records the work artifact directory on task and
	// molecule beads.
	ArtifactDirMetadataKey = "artifact_dir"

	// LegacyWorkDirMetadataKey is the deprecated key that overloaded both
	// meanings; reads still fall back to it on session beads.
	LegacyWorkDirMetadataKey = "work_dir"
)

// Dispatch metadata keys: a non-"gc."-prefixed family that sling writes onto
// work and source beads to wire molecules together and record the merge
// strategy. They predate the gc. namespace convention and their on-store
// strings are load-bearing (the run-chain resolver in runid.go and the graph
// dispatch readers key on them), so they are declared here — like the
// directory keys above — to give the vocabulary one home without changing any
// wire value. They are intentionally NOT in KnownMetadataKeys, whose drift
// guard only covers the gc. namespace.
const (
	// MoleculeIDMetadataKey links a poured/wisp work bead to its molecule root.
	MoleculeIDMetadataKey = "molecule_id"

	// MoleculeFailedMetadataKey marks the beads of a partially-instantiated
	// molecule as failed (value "true"). Written best-effort by
	// internal/molecule markFailed on instantiation error paths; read by
	// dispatch/sling/cmd/gc to skip or close failed roots.
	MoleculeFailedMetadataKey = "molecule_failed"

	// MergeStrategyMetadataKey records the merge strategy chosen for a slung bead.
	MergeStrategyMetadataKey = "merge_strategy"
)

// OptionMetadataPrefix is the dynamic non-"gc."-prefixed key prefix under
// which provider option choices are stored as opt_<OptionsSchema key> (e.g.
// opt_model, opt_effort) on session and work beads. The suffix is open-world
// (a pack-authored OptionsSchema key), so it is declared as a prefix, not
// enumerated — the non-gc sibling of FormulaVarPrefix. Like the directory
// keys above, it is not in KnownMetadataPrefixes because the drift guard's
// key-shape rule only covers the gc. namespace.
const OptionMetadataPrefix = "opt_"

// KnownMetadataKeys lists every engine-owned bead-metadata key this package
// declares. The guard test asserts every gc.* metadata literal used in non-test
// Go resolves to a member of this slice (or a KnownMetadataPrefixes entry).
var KnownMetadataKeys = []string{
	AttemptLogMetadataKey,
	AttemptMetadataKey,
	BondMetadataKey,
	BondVarsMetadataKey,
	BrainParentSIDMetadataKey,
	CheckInfraRetryMetadataKey,
	CheckModeMetadataKey,
	CheckPathMetadataKey,
	CheckTimeoutMetadataKey,
	CityPathMetadataKey,
	ClosedByAttemptMetadataKey,
	ContinuationGroupMetadataKey,
	ControlEpochMetadataKey,
	ControlForMetadataKey,
	ControlQuarantineReasonMetadataKey,
	ControlQuarantinedAtMetadataKey,
	ControlQuarantinedMetadataKey,
	ControllerErrorClassMetadataKey,
	ControllerErrorMetadataKey,
	ControllerRetryableMetadataKey,
	CurrentRunIDMetadataKey,
	ActiveWorkBeadMetadataKey,
	CwdMetadataKey,
	DeferredAssigneeMetadataKey,
	DeferredExecutionRoutedToMetadataKey,
	DeferredRoutedToMetadataKey,
	DeferredTypeMetadataKey,
	DetachedMetadataKey,
	DrainContextMetadataKey,
	DrainContinuationGroupMetadataKey,
	DrainControlIDMetadataKey,
	DrainCountMetadataKey,
	DrainFormulaMetadataKey,
	DrainIndexMetadataKey,
	DrainItemSingleLaneMetadataKey,
	DrainManifestMetadataKey,
	DrainMaxUnitsMetadataKey,
	DrainMemberAccessMetadataKey,
	DrainMemberIDMetadataKey,
	DrainMemberUnresolvedMetadataKey,
	DrainOnItemFailureMetadataKey,
	DrainParentConvoyIDMetadataKey,
	DrainStateMetadataKey,
	DrainUnitKeyMetadataKey,
	DurationMsMetadataKey,
	DynamicFragmentMetadataKey,
	ExclusiveDrainReservationMetadataKey,
	ExecutionRigContextMetadataKey,
	ExecutionRoutedToMetadataKey,
	ExitCodeMetadataKey,
	FailedAttemptMetadataKey,
	FailureClassMetadataKey,
	FailureOwnerMetadataKey,
	FailureReasonMetadataKey,
	FailureSubjectMetadataKey,
	FanoutModeMetadataKey,
	FanoutStateMetadataKey,
	FinalDispositionMetadataKey,
	ForEachMetadataKey,
	FormulaMetadataKey,
	FormulaContractMetadataKey,
	FormulaHashMetadataKey,
	FormulaNameMetadataKey,
	FormulaSourceMetadataKey,
	GCExemptMetadataKey,
	Graphv2RootKeyMetadataKey,
	IdempotencyKeyMetadataKey,
	InputConvoyIDMetadataKey,
	InstantiatingMetadataKey,
	IterationMetadataKey,
	ItemRootKeyMetadataKey,
	KindMetadataKey,
	LastFailureClassMetadataKey,
	LastFinalizeErrorMetadataKey,
	LastHeartbeatAtMetadataKey,
	LogicalBeadIDMetadataKey,
	MaxAttemptsMetadataKey,
	MissingRootBeadIDMetadataKey,
	ModelMetadataKey,
	NextAttemptMetadataKey,
	OnExhaustedMetadataKey,
	OnFailMetadataKey,
	OriginalKindMetadataKey,
	OutcomeBeadIDMetadataKey,
	OutcomeMetadataKey,
	OutputJSONMetadataKey,
	OutputJSONRequiredMetadataKey,
	ParentBeadIDMetadataKey,
	ParentConvoyIDMetadataKey,
	PartialFragmentMetadataKey,
	PartialRetryMetadataKey,
	PackMetadataKey,
	PackRootMetadataKey,
	PackWorkspaceMetadataKey,
	PerDispatchModelMetadataKey,
	RalphStepIDMetadataKey,
	ReasoningMetadataKey,
	RequiredArtifactMetadataKey,
	RequiredArtifactsMetadataKey,
	RetryCountMetadataKey,
	RetryFromMetadataKey,
	RetrySessionRecycledMetadataKey,
	RetryStateMetadataKey,
	RigRootMetadataKey,
	RootBeadIDMetadataKey,
	RootStoreRefMetadataKey,
	RoutedToMetadataKey,
	RunTargetMetadataKey,
	RuntimeVarsMetadataKey,
	ScopeKindMetadataKey,
	ScopeNameMetadataKey,
	ScopeRefMetadataKey,
	ScopeRoleMetadataKey,
	SessionAffinityMetadataKey,
	SessionIDMetadataKey,
	SessionIDCamelMetadataKey,
	SessionNameMetadataKey,
	SessionNameCamelMetadataKey,
	SourceBeadIDMetadataKey,
	SourceStepSpecMetadataKey,
	SourceStoreRefMetadataKey,
	SpawnedCountMetadataKey,
	SpecForMetadataKey,
	SpecForRefMetadataKey,
	StderrMetadataKey,
	StdoutMetadataKey,
	StepIDMetadataKey,
	StepRefMetadataKey,
	StepTimeoutMetadataKey,
	SyntheticKindMetadataKey,
	SyntheticMetadataKey,
	TemplateMetadataKey,
	TerminalMetadataKey,
	TriggerBeadIDMetadataKey,
	TriggerBeadStoreRefMetadataKey,
	TruncatedMetadataKey,
	WorkBranchMetadataKey,
	WorkCommitMetadataKey,
	WorkDirMetadataKey,
	WorkOutcomeMetadataKey,
	WorkVerificationMetadataKey,
	WorkflowIDMetadataKey,
}

// KnownMetadataPrefixes lists declared open-world key prefixes. A literal that
// begins with one of these is considered declared even though its full key is
// not enumerable.
var KnownMetadataPrefixes = []string{
	FormulaVarPrefix,
}

// SessionAffinityMetadataKeys are the metadata keys that pin a work bead to a
// particular live session through continuation-group routing. They must be
// cleared together whenever work is rerouted off its original session without a
// preserved assignee (retry-to-pool, reopen-source, orphan/closed/retired-session
// release); otherwise a later claim re-vacuums the bead onto an unrelated
// session via the stale group. Both cmd/gc and internal/dispatch consume this
// single list so a new affinity key cannot silently fix one clear path while
// leaving another stale.
//
// Of these keys, ContinuationGroupMetadataKey is the active routing vector: the
// hook claim path reads it to vacuum open, unassigned sibling work onto the
// claiming session. SessionAffinityMetadataKey is currently an advisory marker —
// it is written (e.g. internal/dispatch/drain.go) but no Go routing path reads
// it yet, so it is cleared alongside the group for hygiene and future-proofing
// rather than because it gates routing today.
var SessionAffinityMetadataKeys = []string{
	SessionAffinityMetadataKey,
	ContinuationGroupMetadataKey,
}
