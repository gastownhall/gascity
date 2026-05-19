package main

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"path/filepath"
	"strings"
	"time"
)

const (
	sessionReconcilerTraceSchemaVersion = 1

	sessionReconcilerTraceRootDir    = "session-reconciler-trace"
	sessionReconcilerTraceArmsFile   = "arms.json"
	sessionReconcilerTraceHeadFile   = "head.json"
	sessionReconcilerTraceLockFile   = "trace.lock"
	sessionReconcilerTraceQuarantine = "quarantine"
	sessionReconcilerTraceSegments   = "segments"
)

type TraceRecordType string

const (
	TraceRecordCycleStart          TraceRecordType = "cycle_start"
	TraceRecordCycleInputSnapshot  TraceRecordType = "cycle_input_snapshot"
	TraceRecordBatchCommit         TraceRecordType = "batch_commit"
	TraceRecordConfigReload        TraceRecordType = "config_reload"
	TraceRecordTemplateTickSummary TraceRecordType = "template_tick_summary"
	TraceRecordTemplateConfig      TraceRecordType = "template_config_snapshot"
	TraceRecordSessionBaseline     TraceRecordType = "session_baseline"
	TraceRecordSessionResult       TraceRecordType = "session_result"
	TraceRecordDecision            TraceRecordType = "decision"
	TraceRecordOperation           TraceRecordType = "operation"
	TraceRecordMutation            TraceRecordType = "mutation"
	TraceRecordTraceControl        TraceRecordType = "trace_control"
	TraceRecordCycleResult         TraceRecordType = "cycle_result"
)

type TraceMode string

const (
	TraceModeBaseline TraceMode = "baseline"
	TraceModeDetail   TraceMode = "detail"
)

type TraceSource string

const (
	TraceSourceAlwaysOn          TraceSource = "always_on"
	TraceSourceManual            TraceSource = "manual"
	TraceSourceAuto              TraceSource = "auto"
	TraceSourceDerivedDependency TraceSource = "derived_dependency"
)

type TraceSiteCode string

const (
	TraceSiteUnknown                 TraceSiteCode = "unknown"
	TraceSiteScaleCheckExec          TraceSiteCode = "trace.scale_check_exec"
	TraceSiteCycleStart              TraceSiteCode = "cycle.start"
	TraceSiteCycleFinish             TraceSiteCode = "cycle.finish"
	TraceSiteConfigReload            TraceSiteCode = "config.reload"
	TraceSiteDesiredStateBuild       TraceSiteCode = "desired_state.build"
	TraceSitePoolDemandCompute       TraceSiteCode = "pool_desired.compute"
	TraceSitePoolAgentCap            TraceSiteCode = "reconciler.pool.agent_cap"
	TraceSitePoolRigCap              TraceSiteCode = "reconciler.pool.rig_cap"
	TraceSitePoolWorkspaceCap        TraceSiteCode = "reconciler.pool.workspace_cap"
	TraceSitePoolAccept              TraceSiteCode = "reconciler.pool.accept"
	TraceSitePoolMinFill             TraceSiteCode = "reconciler.pool.min_fill"
	TraceSitePoolInFlightReuse       TraceSiteCode = "reconciler.pool.inflight_reuse"
	TraceSiteReconcilerUnknownState  TraceSiteCode = "reconciler.session.skip_unknown_state"
	TraceSiteReconcilerOrphaned      TraceSiteCode = "reconciler.session.orphan_or_suspended"
	TraceSiteReconcilerCloseOrphan   TraceSiteCode = "reconciler.session.close_orphan"
	TraceSiteReconcilerPendingCreate TraceSiteCode = "reconciler.session.rollback_pending_create"
	TraceSiteReconcilerConfigDrift   TraceSiteCode = "reconciler.session.config_drift"
	TraceSiteReconcilerIdleDrain     TraceSiteCode = "reconciler.session.idle_drain"
	TraceSiteReconcilerIdleTimeout   TraceSiteCode = "reconciler.session.idle_timeout"
	TraceSiteReconcilerWakeDecision  TraceSiteCode = "reconciler.session.wake_decision"
	TraceSiteReconcilerDrainDecision TraceSiteCode = "reconciler.session.drain"
	TraceSiteDrainStale              TraceSiteCode = "reconciler.drain.stale"
	TraceSiteDrainComplete           TraceSiteCode = "reconciler.drain.complete"
	TraceSiteDrainCancel             TraceSiteCode = "reconciler.drain.cancel"
	TraceSiteDrainTimeout            TraceSiteCode = "reconciler.drain.timeout"
	TraceSiteMutationBeadMetadata    TraceSiteCode = "bead_metadata"
	TraceSiteMutationRuntimeMeta     TraceSiteCode = "runtime_meta"
	TraceSiteLifecycleStartRollback  TraceSiteCode = "reconciler.start.rollback_pending"
	TraceSiteLifecycleStartFailed    TraceSiteCode = "reconciler.start.failed"
	TraceSiteLifecycleStartRun       TraceSiteCode = "reconciler.start.execute"
	TraceSiteLifecycleStartPrepare   TraceSiteCode = "lifecycle.start.prepare"
	TraceSiteLifecycleStartExecute   TraceSiteCode = "lifecycle.start.execute"
	TraceSiteLifecycleStartCommit    TraceSiteCode = "lifecycle.start.commit"
	TraceSiteLifecycleDrainBegin     TraceSiteCode = "lifecycle.drain.begin"
	TraceSiteLifecycleDrainAdvance   TraceSiteCode = "lifecycle.drain.advance"
	TraceSiteTraceControl            TraceSiteCode = "trace.control"
)

type TraceReasonCode string

const (
	TraceReasonUnknown                TraceReasonCode = "unknown"
	TraceReasonNoDemand               TraceReasonCode = "no_demand"
	TraceReasonNoMatchingSession      TraceReasonCode = "no_matching_session"
	TraceReasonDependencyBlocked      TraceReasonCode = "blocked_on_dependencies"
	TraceReasonStorePartial           TraceReasonCode = "store_partial"
	TraceReasonConfigDrift            TraceReasonCode = "config_drift"
	TraceReasonIdle                   TraceReasonCode = "idle"
	TraceReasonPendingCreateRollback  TraceReasonCode = "pending_create_rollback"
	TraceReasonWakeFailureIncremented TraceReasonCode = "wake_failure_incremented"
	TraceReasonQuarantineEntered      TraceReasonCode = "quarantine_entered"
	TraceReasonUnknownStateSkipped    TraceReasonCode = "unknown_state_skipped"
	TraceReasonTemplateMissing        TraceReasonCode = "template_missing"
	TraceReasonNoEffectTemplateMatch  TraceReasonCode = "no_effective_template_match"
	TraceReasonAutoArmSuppressed      TraceReasonCode = "auto_arm_suppressed"
	TraceReasonRetained               TraceReasonCode = "retained"
	TraceReasonExpired                TraceReasonCode = "expired"
	TraceReasonAgentCap               TraceReasonCode = "agent_cap"
	TraceReasonRigCap                 TraceReasonCode = "rig_cap"
	TraceReasonWorkspaceCap           TraceReasonCode = "workspace_cap"
	TraceReasonCap                    TraceReasonCode = "cap"
	TraceReasonMinFill                TraceReasonCode = "min_fill"
	TraceReasonInFlightReuse          TraceReasonCode = "inflight_reuse"
	TraceReasonWake                   TraceReasonCode = "wake"
	TraceReasonIdleTimeout            TraceReasonCode = "idle_timeout"
	TraceReasonStaleGeneration        TraceReasonCode = "stale_generation"
	TraceReasonSuspended              TraceReasonCode = "suspended"
	TraceReasonOrphaned               TraceReasonCode = "orphaned"
	TraceReasonDrainTimeout           TraceReasonCode = "drain_timeout"
	TraceReasonStoreQueryPartial      TraceReasonCode = "store_query_partial"
	TraceReasonNoWakeReason           TraceReasonCode = "no_wake_reason"
)

type TraceOutcomeCode string

const (
	TraceOutcomeUnknown                 TraceOutcomeCode = "unknown"
	TraceOutcomeComplete                TraceOutcomeCode = "complete"
	TraceOutcomePartial                 TraceOutcomeCode = "partial"
	TraceOutcomeApplied                 TraceOutcomeCode = "applied"
	TraceOutcomeNoChange                TraceOutcomeCode = "no_change"
	TraceOutcomeFailed                  TraceOutcomeCode = "failed"
	TraceOutcomeSuccess                 TraceOutcomeCode = "success"
	TraceOutcomeDeferredByWakeBudget    TraceOutcomeCode = "deferred_by_wake_budget"
	TraceOutcomeSessionExists           TraceOutcomeCode = "session_exists"
	TraceOutcomeSessionExistsConverged  TraceOutcomeCode = "session_exists_converged"
	TraceOutcomeBlockedOnDependencies   TraceOutcomeCode = "blocked_on_dependencies"
	TraceOutcomeProviderError           TraceOutcomeCode = "provider_error"
	TraceOutcomePanicRecovered          TraceOutcomeCode = "panic_recovered"
	TraceOutcomeDeadlineExceeded        TraceOutcomeCode = "deadline_exceeded"
	TraceOutcomeCanceled                TraceOutcomeCode = "canceled"
	TraceOutcomeSlowStorageDegraded     TraceOutcomeCode = "slow_storage_degraded"
	TraceOutcomeLowSpaceDegraded        TraceOutcomeCode = "low_space_degraded"
	TraceOutcomePromotionPartialContext TraceOutcomeCode = "promotion_partial_context"
	TraceOutcomeAccepted                TraceOutcomeCode = "accepted"
	TraceOutcomeRejected                TraceOutcomeCode = "rejected"
	TraceOutcomeSkipped                 TraceOutcomeCode = "skipped"
	TraceOutcomeDrain                   TraceOutcomeCode = "drain"
	TraceOutcomeClosed                  TraceOutcomeCode = "closed"
	TraceOutcomeRollback                TraceOutcomeCode = "rollback"
	TraceOutcomeDeferredAttached        TraceOutcomeCode = "deferred_attached"
	TraceOutcomeDeferredActive          TraceOutcomeCode = "deferred_active"
	TraceOutcomeStop                    TraceOutcomeCode = "stop"
	TraceOutcomeStartCandidate          TraceOutcomeCode = "start_candidate"
	TraceOutcomeRetry                   TraceOutcomeCode = "retry"
	TraceOutcomeCancel                  TraceOutcomeCode = "cancel"
)

type TraceCompletionStatus string

const (
	TraceCompletionCompleted      TraceCompletionStatus = "completed"
	TraceCompletionTraceError     TraceCompletionStatus = "trace_error"
	TraceCompletionPanicRecovered TraceCompletionStatus = "panic_recovered"
	TraceCompletionAborted        TraceCompletionStatus = "aborted"
)

type TraceCompletenessStatus string

const (
	TraceCompletenessComplete                TraceCompletenessStatus = "complete"
	TraceCompletenessPartialLoss             TraceCompletenessStatus = "partial_loss"
	TraceCompletenessNotTraced               TraceCompletenessStatus = "not_traced"
	TraceCompletenessPromotionPartialContext TraceCompletenessStatus = "promotion_partial_context"
)

type TraceEvaluationStatus string

const (
	TraceEvaluationEligible          TraceEvaluationStatus = "eligible"
	TraceEvaluationDependencyBlocked TraceEvaluationStatus = "dependency_blocked"
	TraceEvaluationCapRejected       TraceEvaluationStatus = "cap_rejected"
	TraceEvaluationStorePartial      TraceEvaluationStatus = "store_partial"
	TraceEvaluationMissingTemplate   TraceEvaluationStatus = "missing_template"
	TraceEvaluationSkipped           TraceEvaluationStatus = "skipped"
)

type TraceDurabilityTier string

const (
	TraceDurabilityMetadata TraceDurabilityTier = "metadata"
	TraceDurabilityDurable  TraceDurabilityTier = "durable"
)

type TraceTickTrigger string

const (
	TraceTickTriggerPatrol         TraceTickTrigger = "patrol"
	TraceTickTriggerPoke           TraceTickTrigger = "poke"
	TraceTickTriggerStartup        TraceTickTrigger = "startup"
	TraceTickTriggerReloadFollowup TraceTickTrigger = "reload_followup"
	TraceTickTriggerControl        TraceTickTrigger = "control"
	TraceTickTriggerUnknown        TraceTickTrigger = "unknown"
)

type TraceArmScopeType string

const (
	TraceArmScopeTemplate TraceArmScopeType = "template"
)

type TraceArmSource string

const (
	TraceArmSourceManual TraceArmSource = "manual"
	TraceArmSourceAuto   TraceArmSource = "auto"
)

type TraceTextBlob struct {
	Value         string `json:"value"`
	OriginalBytes int    `json:"original_bytes"`
	StoredBytes   int    `json:"stored_bytes"`
	Truncated     bool   `json:"truncated"`
}

func NewTraceTextBlob(value string, maxBytes int) TraceTextBlob {
	b := []byte(value)
	blob := TraceTextBlob{
		Value:         value,
		OriginalBytes: len(b),
		StoredBytes:   len(b),
	}
	if maxBytes > 0 && len(b) > maxBytes {
		blob.Value = string(b[:maxBytes])
		blob.StoredBytes = maxBytes
		blob.Truncated = true
	}
	return blob
}

type SessionReconcilerTraceRecord struct {
	TraceSchemaVersion    int                     `json:"trace_schema_version"`
	Seq                   uint64                  `json:"seq"`
	TraceID               string                  `json:"trace_id"`
	TickID                string                  `json:"tick_id"`
	RecordID              string                  `json:"record_id"`
	ParentRecordID        string                  `json:"parent_record_id,omitempty"`
	CausedByRecordIDs     []string                `json:"caused_by_record_ids,omitempty"`
	RecordType            TraceRecordType         `json:"record_type"`
	TraceMode             TraceMode               `json:"trace_mode,omitempty"`
	TraceSource           TraceSource             `json:"trace_source,omitempty"`
	SiteCode              TraceSiteCode           `json:"site_code,omitempty"`
	Ts                    time.Time               `json:"ts"`
	CycleOffsetMS         int64                   `json:"cycle_offset_ms,omitempty"`
	CityPath              string                  `json:"city_path,omitempty"`
	ConfigRevision        string                  `json:"config_revision,omitempty"`
	Template              string                  `json:"template,omitempty"`
	SessionBeadID         string                  `json:"session_bead_id,omitempty"`
	SessionName           string                  `json:"session_name,omitempty"`
	Alias                 string                  `json:"alias,omitempty"`
	Provider              string                  `json:"provider,omitempty"`
	WorkDir               string                  `json:"work_dir,omitempty"`
	SessionKey            string                  `json:"session_key,omitempty"`
	OperationID           string                  `json:"operation_id,omitempty"`
	ControllerInstanceID  string                  `json:"controller_instance_id,omitempty"`
	ControllerPID         int                     `json:"controller_pid,omitempty"`
	ControllerStartedAt   *time.Time              `json:"controller_started_at,omitempty"`
	Host                  string                  `json:"host,omitempty"`
	TickTrigger           TraceTickTrigger        `json:"tick_trigger,omitempty"`
	TriggerDetail         string                  `json:"trigger_detail,omitempty"`
	GCVersion             string                  `json:"gc_version,omitempty"`
	GCCommit              string                  `json:"gc_commit,omitempty"`
	BuildDate             string                  `json:"build_date,omitempty"`
	VcsDirty              bool                    `json:"vcs_dirty,omitempty"`
	CodeFingerprint       string                  `json:"code_fingerprint,omitempty"`
	ReasonCode            TraceReasonCode         `json:"reason_code,omitempty"`
	OutcomeCode           TraceOutcomeCode        `json:"outcome_code,omitempty"`
	CompletionStatus      TraceCompletionStatus   `json:"completion_status,omitempty"`
	CompletenessStatus    TraceCompletenessStatus `json:"completeness_status,omitempty"`
	EvaluationStatus      TraceEvaluationStatus   `json:"evaluation_status,omitempty"`
	DurabilityTier        TraceDurabilityTier     `json:"durability_tier,omitempty"`
	DurationMS            int64                   `json:"duration_ms,omitempty"`
	RecordCount           int                     `json:"record_count,omitempty"`
	SeqStart              uint64                  `json:"seq_start,omitempty"`
	SeqEnd                uint64                  `json:"seq_end,omitempty"`
	FirstSeq              uint64                  `json:"first_seq,omitempty"`
	LastSeq               uint64                  `json:"last_seq,omitempty"`
	BatchCRC32            uint32                  `json:"batch_crc32,omitempty"`
	DroppedRecordCount    int                     `json:"dropped_record_count,omitempty"`
	DroppedBatchCount     int                     `json:"dropped_batch_count,omitempty"`
	DropReasonCounts      map[string]int          `json:"drop_reason_counts,omitempty"`
	ActiveTemplateCount   int                     `json:"active_template_count,omitempty"`
	DetailedTemplateCount int                     `json:"detailed_template_count,omitempty"`
	TemplatesTouched      []string                `json:"templates_touched,omitempty"`
	DecisionCounts        map[string]int          `json:"decision_counts,omitempty"`
	OperationCounts       map[string]int          `json:"operation_counts,omitempty"`
	MutationCounts        map[string]int          `json:"mutation_counts,omitempty"`
	ReasonCounts          map[string]int          `json:"reason_counts,omitempty"`
	OutcomeCounts         map[string]int          `json:"outcome_counts,omitempty"`
	AutoArmsTriggered     []string                `json:"auto_arms_triggered,omitempty"`
	DemandSummary         map[string]any          `json:"demand_summary,omitempty"`
	DependencyBlocked     bool                    `json:"dependency_blocked,omitempty"`
	MissingTemplate       bool                    `json:"missing_template,omitempty"`
	Fields                map[string]any          `json:"fields,omitempty"`
}

func newTraceRecord(kind TraceRecordType) SessionReconcilerTraceRecord {
	return SessionReconcilerTraceRecord{
		TraceSchemaVersion: sessionReconcilerTraceSchemaVersion,
		RecordType:         kind,
		Fields:             make(map[string]any),
	}
}

func (r *SessionReconcilerTraceRecord) ensureFields() {
	if r.Fields == nil {
		r.Fields = make(map[string]any)
	}
}

func (r SessionReconcilerTraceRecord) clone() SessionReconcilerTraceRecord {
	if len(r.CausedByRecordIDs) > 0 {
		r.CausedByRecordIDs = append([]string(nil), r.CausedByRecordIDs...)
	}
	if len(r.TemplatesTouched) > 0 {
		r.TemplatesTouched = append([]string(nil), r.TemplatesTouched...)
	}
	if len(r.AutoArmsTriggered) > 0 {
		r.AutoArmsTriggered = append([]string(nil), r.AutoArmsTriggered...)
	}
	if len(r.DropReasonCounts) > 0 {
		cp := make(map[string]int, len(r.DropReasonCounts))
		for k, v := range r.DropReasonCounts {
			cp[k] = v
		}
		r.DropReasonCounts = cp
	}
	if len(r.DecisionCounts) > 0 {
		cp := make(map[string]int, len(r.DecisionCounts))
		for k, v := range r.DecisionCounts {
			cp[k] = v
		}
		r.DecisionCounts = cp
	}
	if len(r.OperationCounts) > 0 {
		cp := make(map[string]int, len(r.OperationCounts))
		for k, v := range r.OperationCounts {
			cp[k] = v
		}
		r.OperationCounts = cp
	}
	if len(r.MutationCounts) > 0 {
		cp := make(map[string]int, len(r.MutationCounts))
		for k, v := range r.MutationCounts {
			cp[k] = v
		}
		r.MutationCounts = cp
	}
	if len(r.ReasonCounts) > 0 {
		cp := make(map[string]int, len(r.ReasonCounts))
		for k, v := range r.ReasonCounts {
			cp[k] = v
		}
		r.ReasonCounts = cp
	}
	if len(r.OutcomeCounts) > 0 {
		cp := make(map[string]int, len(r.OutcomeCounts))
		for k, v := range r.OutcomeCounts {
			cp[k] = v
		}
		r.OutcomeCounts = cp
	}
	if len(r.DemandSummary) > 0 {
		cp := make(map[string]any, len(r.DemandSummary))
		for k, v := range r.DemandSummary {
			cp[k] = v
		}
		r.DemandSummary = cp
	}
	if len(r.Fields) > 0 {
		cp := make(map[string]any, len(r.Fields))
		for k, v := range r.Fields {
			cp[k] = v
		}
		r.Fields = cp
	}
	return r
}

type TraceArm struct {
	ScopeType      TraceArmScopeType `json:"scope_type"`
	ScopeValue     string            `json:"scope_value"`
	Source         TraceArmSource    `json:"source"`
	Level          TraceMode         `json:"level"`
	ArmedAt        time.Time         `json:"armed_at"`
	ExpiresAt      time.Time         `json:"expires_at"`
	LastExtendedAt time.Time         `json:"last_extended_at"`
	TriggerReason  string            `json:"trigger_reason,omitempty"`
	ActorKind      string            `json:"actor_kind,omitempty"`
	ActorUser      string            `json:"actor_user,omitempty"`
	ActorHost      string            `json:"actor_host,omitempty"`
	ActorPID       int               `json:"actor_pid,omitempty"`
	CommandSummary string            `json:"command_summary,omitempty"`
	RequestedAt    *time.Time        `json:"requested_at,omitempty"`
	UpdatedAt      time.Time         `json:"updated_at"`
}

type TraceArmState struct {
	SchemaVersion int        `json:"schema_version"`
	UpdatedAt     time.Time  `json:"updated_at"`
	Arms          []TraceArm `json:"arms"`
}

func (s TraceArmState) normalized() TraceArmState {
	s.Arms = append([]TraceArm(nil), s.Arms...)
	sortTraceArms(s.Arms)
	return s
}

type TraceFilter struct {
	TraceID     string
	TickID      string
	Template    string
	SessionName string
	RecordType  TraceRecordType
	ReasonCode  TraceReasonCode
	OutcomeCode TraceOutcomeCode
	SiteCode    TraceSiteCode
	TraceMode   TraceMode
	TraceSource TraceSource
	Since       time.Time
	Until       time.Time
	SeqAfter    uint64
	SeqBefore   uint64
}

func matchesTraceFilter(r SessionReconcilerTraceRecord, f TraceFilter) bool {
	if f.TraceID != "" && r.TraceID != f.TraceID {
		return false
	}
	if f.TickID != "" && r.TickID != f.TickID {
		return false
	}
	if f.Template != "" && !traceTemplateMatches(r.Template, f.Template) {
		return false
	}
	if f.SessionName != "" && r.SessionName != f.SessionName {
		return false
	}
	if f.RecordType != "" && r.RecordType != f.RecordType {
		return false
	}
	if f.ReasonCode != "" && r.ReasonCode != f.ReasonCode {
		return false
	}
	if f.OutcomeCode != "" && r.OutcomeCode != f.OutcomeCode {
		return false
	}
	if f.SiteCode != "" && r.SiteCode != f.SiteCode {
		return false
	}
	if f.TraceMode != "" && r.TraceMode != f.TraceMode {
		return false
	}
	if f.TraceSource != "" && r.TraceSource != f.TraceSource {
		return false
	}
	if !f.Since.IsZero() && r.Ts.Before(f.Since) {
		return false
	}
	if !f.Until.IsZero() && r.Ts.After(f.Until) {
		return false
	}
	if f.SeqAfter > 0 && r.Seq <= f.SeqAfter {
		return false
	}
	if f.SeqBefore > 0 && r.Seq >= f.SeqBefore {
		return false
	}
	return true
}

func traceTemplateMatches(candidate, selector string) bool {
	candidate = strings.TrimSpace(candidate)
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return true
	}
	if candidate == selector {
		return true
	}
	return normalizedTraceTemplate(candidate) == normalizedTraceTemplate(selector)
}

func normalizedTraceTemplate(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	return strings.TrimSpace(filepath.Clean(v))
}

func normalizeTraceSiteCode(raw string) (TraceSiteCode, string) {
	raw = strings.TrimSpace(raw)
	switch raw {
	case "reconciler.session.unknown_state":
		return TraceSiteReconcilerUnknownState, ""
	case "reconciler.session.pending_create":
		return TraceSiteReconcilerPendingCreate, ""
	case "reconciler.session.wake":
		return TraceSiteReconcilerWakeDecision, ""
	case "reconciler.session.drain":
		return TraceSiteReconcilerDrainDecision, ""
	}
	switch TraceSiteCode(raw) {
	case TraceSiteUnknown,
		TraceSiteScaleCheckExec,
		TraceSiteCycleStart,
		TraceSiteCycleFinish,
		TraceSiteConfigReload,
		TraceSiteDesiredStateBuild,
		TraceSitePoolDemandCompute,
		TraceSitePoolAgentCap,
		TraceSitePoolRigCap,
		TraceSitePoolWorkspaceCap,
		TraceSitePoolAccept,
		TraceSitePoolMinFill,
		TraceSitePoolInFlightReuse,
		TraceSiteReconcilerUnknownState,
		TraceSiteReconcilerOrphaned,
		TraceSiteReconcilerCloseOrphan,
		TraceSiteReconcilerPendingCreate,
		TraceSiteReconcilerConfigDrift,
		TraceSiteReconcilerIdleDrain,
		TraceSiteReconcilerIdleTimeout,
		TraceSiteReconcilerWakeDecision,
		TraceSiteReconcilerDrainDecision,
		TraceSiteDrainStale,
		TraceSiteDrainComplete,
		TraceSiteDrainCancel,
		TraceSiteDrainTimeout,
		TraceSiteMutationBeadMetadata,
		TraceSiteMutationRuntimeMeta,
		TraceSiteLifecycleStartRollback,
		TraceSiteLifecycleStartFailed,
		TraceSiteLifecycleStartRun,
		TraceSiteLifecycleStartPrepare,
		TraceSiteLifecycleStartExecute,
		TraceSiteLifecycleStartCommit,
		TraceSiteLifecycleDrainBegin,
		TraceSiteLifecycleDrainAdvance,
		TraceSiteTraceControl:
		return TraceSiteCode(raw), ""
	default:
		return TraceSiteUnknown, raw
	}
}

func normalizeTraceReasonCode(raw string) (TraceReasonCode, string) {
	raw = strings.TrimSpace(raw)
	switch raw {
	case "config-drift":
		return TraceReasonConfigDrift, ""
	case "store-partial":
		return TraceReasonStorePartial, ""
	case "no-wake-reason":
		return TraceReasonNoWakeReason, ""
	}
	switch TraceReasonCode(raw) {
	case TraceReasonUnknown,
		TraceReasonNoDemand,
		TraceReasonNoMatchingSession,
		TraceReasonDependencyBlocked,
		TraceReasonStorePartial,
		TraceReasonConfigDrift,
		TraceReasonIdle,
		TraceReasonPendingCreateRollback,
		TraceReasonWakeFailureIncremented,
		TraceReasonQuarantineEntered,
		TraceReasonUnknownStateSkipped,
		TraceReasonTemplateMissing,
		TraceReasonNoEffectTemplateMatch,
		TraceReasonAutoArmSuppressed,
		TraceReasonRetained,
		TraceReasonExpired,
		TraceReasonAgentCap,
		TraceReasonRigCap,
		TraceReasonWorkspaceCap,
		TraceReasonCap,
		TraceReasonMinFill,
		TraceReasonInFlightReuse,
		TraceReasonWake,
		TraceReasonIdleTimeout,
		TraceReasonStaleGeneration,
		TraceReasonSuspended,
		TraceReasonOrphaned,
		TraceReasonDrainTimeout,
		TraceReasonStoreQueryPartial,
		TraceReasonNoWakeReason:
		return TraceReasonCode(raw), ""
	default:
		return TraceReasonUnknown, raw
	}
}

func normalizeTraceOutcomeCode(raw string) (TraceOutcomeCode, string) {
	switch TraceOutcomeCode(strings.TrimSpace(raw)) {
	case TraceOutcomeUnknown,
		TraceOutcomeComplete,
		TraceOutcomePartial,
		TraceOutcomeApplied,
		TraceOutcomeNoChange,
		TraceOutcomeFailed,
		TraceOutcomeSuccess,
		TraceOutcomeDeferredByWakeBudget,
		TraceOutcomeSessionExists,
		TraceOutcomeSessionExistsConverged,
		TraceOutcomeBlockedOnDependencies,
		TraceOutcomeProviderError,
		TraceOutcomePanicRecovered,
		TraceOutcomeDeadlineExceeded,
		TraceOutcomeCanceled,
		TraceOutcomeSlowStorageDegraded,
		TraceOutcomeLowSpaceDegraded,
		TraceOutcomePromotionPartialContext,
		TraceOutcomeAccepted,
		TraceOutcomeRejected,
		TraceOutcomeSkipped,
		TraceOutcomeDrain,
		TraceOutcomeClosed,
		TraceOutcomeRollback,
		TraceOutcomeDeferredAttached,
		TraceOutcomeDeferredActive,
		TraceOutcomeStop,
		TraceOutcomeStartCandidate,
		TraceOutcomeRetry,
		TraceOutcomeCancel:
		return TraceOutcomeCode(strings.TrimSpace(raw)), ""
	default:
		return TraceOutcomeUnknown, strings.TrimSpace(raw)
	}
}

func newTraceID(prefix string) string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return fmt.Sprintf("%s-%d", prefix, time.Now().UTC().UnixNano())
	}
	return fmt.Sprintf("%s-%s", prefix, hex.EncodeToString(buf[:]))
}

func stableTraceRecordID(traceID string, seq uint64, local int) string {
	return fmt.Sprintf("%s:%d:%d", traceID, seq, local)
}

func traceSegmentFileName(index int) string {
	return fmt.Sprintf("segment-%06d.jsonl", index)
}

func traceDayDir(base string, t time.Time) string {
	return filepath.Join(base, fmt.Sprintf("%04d", t.UTC().Year()), fmt.Sprintf("%02d", int(t.UTC().Month())), fmt.Sprintf("%02d", t.UTC().Day()))
}

func traceScopeKey(scopeType TraceArmScopeType, scopeValue string, source TraceArmSource) string {
	return string(scopeType) + "|" + scopeValue + "|" + string(source)
}

func traceCommandSummary(command string, selector string, forDuration string, all bool) string {
	parts := []string{command}
	if selector != "" {
		parts = append(parts, "template="+selector)
	}
	if forDuration != "" {
		parts = append(parts, "for="+forDuration)
	}
	if all {
		parts = append(parts, "all=true")
	}
	return strings.Join(parts, " ")
}

func traceRecordSummary(rec SessionReconcilerTraceRecord) string {
	parts := []string{
		string(rec.RecordType),
		string(rec.SiteCode),
		string(rec.ReasonCode),
		string(rec.OutcomeCode),
	}
	if rec.Template != "" {
		parts = append(parts, "template="+rec.Template)
	}
	if rec.SessionName != "" {
		parts = append(parts, "session="+rec.SessionName)
	}
	return strings.Join(parts, " ")
}

func traceRecentRecords(records []SessionReconcilerTraceRecord, limit int) []SessionReconcilerTraceRecord {
	if limit <= 0 || len(records) <= limit {
		return records
	}
	out := make([]SessionReconcilerTraceRecord, limit)
	copy(out, records[len(records)-limit:])
	return out
}

type traceRecordPayload map[string]any

type sessionReconcilerTraceCycleInfo struct {
	TickID               uint64
	TraceID              string
	TraceMode            string
	TraceSource          string
	CityPath             string
	ConfigRevision       string
	ControllerInstanceID string
	ControllerPID        int
	ControllerStartedAt  time.Time
	Host                 string
	GCVersion            string
	GCCommit             string
	BuildDate            string
	CodeFingerprint      string
	TickTrigger          string
	TriggerDetail        string
}
