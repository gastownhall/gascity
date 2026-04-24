// Package convergence provides field name constants and helpers for the
// convergence loop primitive's metadata namespace.
package convergence

import (
	"strconv"
	"strings"
	"time"
)

// Metadata field name constants for the convergence.* namespace.
const (
	FieldState             = "convergence.state"
	FieldIteration         = "convergence.iteration"
	FieldMaxIterations     = "convergence.max_iterations"
	FieldFormula           = "convergence.formula"
	FieldTarget            = "convergence.target"
	FieldGateMode          = "convergence.gate_mode"
	FieldGateCondition     = "convergence.gate_condition"
	FieldGateTimeout       = "convergence.gate_timeout"
	FieldGateTimeoutAction = "convergence.gate_timeout_action"
	FieldActiveWisp        = "convergence.active_wisp"
	FieldLastProcessedWisp = "convergence.last_processed_wisp"
	FieldAgentVerdict      = "convergence.agent_verdict"
	FieldAgentVerdictWisp  = "convergence.agent_verdict_wisp"
	FieldGateOutcome       = "convergence.gate_outcome"
	FieldGateExitCode      = "convergence.gate_exit_code"
	FieldGateOutcomeWisp   = "convergence.gate_outcome_wisp"
	FieldGateRetryCount    = "convergence.gate_retry_count"
	FieldTerminalReason    = "convergence.terminal_reason"
	FieldTerminalActor     = "convergence.terminal_actor"
	FieldWaitingReason     = "convergence.waiting_reason"
	FieldRetrySource       = "convergence.retry_source"
	FieldCityPath          = "convergence.city_path"
	FieldEvaluatePrompt    = "convergence.evaluate_prompt"
	FieldGateStdout        = "convergence.gate_stdout"
	FieldGateStderr        = "convergence.gate_stderr"
	FieldGateDurationMs    = "convergence.gate_duration_ms"
	FieldGateTruncated     = "convergence.gate_truncated"
	FieldPendingNextWisp   = "convergence.pending_next_wisp"
)

// VarPrefix is the metadata key prefix for template variables.
const VarPrefix = "var."

// State values for convergence.state.
const (
	StateCreating      = "creating" // set immediately after bead creation; reconciler terminates partial creations
	StateActive        = "active"
	StateWaitingManual = "waiting_manual"
	StateTerminated    = "terminated"
)

// GateMode values for convergence.gate_mode.
const (
	GateModeManual    = "manual"
	GateModeCondition = "condition"
	GateModeHybrid    = "hybrid"
)

// GateTimeoutAction values.
const (
	TimeoutActionIterate   = "iterate"
	TimeoutActionRetry     = "retry"
	TimeoutActionManual    = "manual"
	TimeoutActionTerminate = "terminate"
)

// TerminalReason values for convergence.terminal_reason.
const (
	TerminalApproved        = "approved"
	TerminalNoConvergence   = "no_convergence"
	TerminalStopped         = "stopped"
	TerminalPartialCreation = "partial_creation"
)

// GateOutcome values for convergence.gate_outcome.
const (
	GatePass    = "pass"
	GateFail    = "fail"
	GateTimeout = "timeout"
	GateError   = "error"
)

// WaitingReason values for convergence.waiting_reason.
const (
	WaitManual            = "manual"
	WaitHybridNoCondition = "hybrid_no_condition"
	WaitTimeout           = "timeout"
	WaitSlingFailure      = "sling_failure"
)

// Verdict values (normalized).
const (
	VerdictApprove          = "approve"
	VerdictApproveWithRisks = "approve-with-risks"
	VerdictBlock            = "block"
)

// pastTenseMap maps common past-tense agent verdict strings to their
// canonical present-tense form.
var pastTenseMap = map[string]string{
	"approved":            VerdictApprove,
	"blocked":             VerdictBlock,
	"approve-with-risk":   VerdictApproveWithRisks,
	"approved-with-risks": VerdictApproveWithRisks,
	"approved-with-risk":  VerdictApproveWithRisks,
}

// NormalizeVerdict normalizes a raw agent verdict string:
// lowercase, trim whitespace, past-tense mapping.
// Unknown values map to "block".
func NormalizeVerdict(raw string) string {
	v := strings.ToLower(strings.TrimSpace(raw))
	if v == "" {
		return VerdictBlock
	}
	if mapped, ok := pastTenseMap[v]; ok {
		return mapped
	}
	switch v {
	case VerdictApprove, VerdictApproveWithRisks, VerdictBlock:
		return v
	default:
		return VerdictBlock
	}
}

// EncodeInt encodes an integer as a decimal string for metadata storage.
func EncodeInt(n int) string {
	return strconv.Itoa(n)
}

// DecodeInt decodes a metadata string to an integer.
// Returns 0, false if the string is empty or not a valid integer.
func DecodeInt(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0, false
	}
	return n, true
}

// EncodeDuration encodes a duration as a Go duration string.
func EncodeDuration(d time.Duration) string {
	return d.String()
}

// DecodeDuration decodes a metadata string to a duration.
// Returns 0, false if the string is empty or not a valid duration.
func DecodeDuration(s string) (time.Duration, bool) {
	if s == "" {
		return 0, false
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, false
	}
	return d, true
}

// MetadataPresent checks if a key exists in a metadata map.
// Returns the value and whether the key was present (distinguishing
// absent from empty string).
func MetadataPresent(meta map[string]string, key string) (string, bool) {
	v, ok := meta[key]
	return v, ok
}
