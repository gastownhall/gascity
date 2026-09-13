package convergence

import (
	"encoding/json"
	"fmt"
	"time"
)

// Convergence event type constants. These match the event_type discriminator
// values in the Event Contracts spec section.
const (
	EventCreated        = "convergence.created"
	EventIteration      = "convergence.iteration"
	EventTerminated     = "convergence.terminated"
	EventWaitingManual  = "convergence.waiting_manual"
	EventManualApprove  = "convergence.manual_approve"
	EventManualIterate  = "convergence.manual_iterate"
	EventManualStop     = "convergence.manual_stop"
	EventTriggerAdvance = "convergence.trigger_advance"
)

// Event delivery tiers.
const (
	// TierCritical events use at-least-once delivery (emitted before commit
	// point, re-emitted on replay). Iteration and Terminated events.
	TierCritical = "critical"

	// TierRecoverable events use best-effort with reconciliation.
	// Created, WaitingManual, and ManualIterate events.
	TierRecoverable = "recoverable"

	// TierBestEffort events are emitted after durable state changes but
	// not re-emitted on recovery. ManualApprove and ManualStop events.
	TierBestEffort = "best_effort"
)

// EventIDCreated returns the stable event ID for a ConvergenceCreated event.
func EventIDCreated(beadID string) string {
	return fmt.Sprintf("converge:%s:created", beadID)
}

// EventIDIteration returns the stable event ID for a ConvergenceIteration event.
// N is derived from the wisp's own idempotency key, not the global counter.
func EventIDIteration(beadID string, iteration int) string {
	return fmt.Sprintf("converge:%s:iter:%d:iteration", beadID, iteration)
}

// EventIDWaitingManual returns the stable event ID for a ConvergenceWaitingManual event.
func EventIDWaitingManual(beadID string, iteration int) string {
	return fmt.Sprintf("converge:%s:iter:%d:waiting_manual", beadID, iteration)
}

// EventIDTerminated returns the stable event ID for a ConvergenceTerminated event.
func EventIDTerminated(beadID string) string {
	return fmt.Sprintf("converge:%s:terminated", beadID)
}

// EventIDManualApprove returns the stable event ID for a ConvergenceManualApprove event.
func EventIDManualApprove(beadID string) string {
	return fmt.Sprintf("converge:%s:manual_approve", beadID)
}

// EventIDManualIterate returns the stable event ID for a ConvergenceManualIterate event.
// N is the iteration number of the NEW wisp being poured.
func EventIDManualIterate(beadID string, iteration int) string {
	return fmt.Sprintf("converge:%s:iter:%d:manual_iterate", beadID, iteration)
}

// EventIDManualStop returns the stable event ID for a ConvergenceManualStop event.
func EventIDManualStop(beadID string) string {
	return fmt.Sprintf("converge:%s:manual_stop", beadID)
}

// EventIDTriggerAdvance returns the stable event ID for a ConvergenceTriggerAdvance
// event. N is the iteration number of the NEW wisp poured when the trigger fired.
// It is deliberately distinct from EventIDIteration so the trigger-driven advance
// cannot collide with the per-wisp iteration event that the same iteration's wisp
// emits when it closes (both would otherwise derive converge:<bead>:iter:N:iteration).
func EventIDTriggerAdvance(beadID string, iteration int) string {
	return fmt.Sprintf("converge:%s:iter:%d:trigger_advance", beadID, iteration)
}

// CreatedPayload is the structured payload for ConvergenceCreated events.
type CreatedPayload struct {
	Formula       string  `json:"formula"`
	Target        string  `json:"target"`
	Rig           string  `json:"rig,omitempty"`
	GateMode      string  `json:"gate_mode"`
	MaxIterations int     `json:"max_iterations"`
	Title         string  `json:"title"`
	FirstWispID   string  `json:"first_wisp_id"`
	RetrySource   *string `json:"retry_source"` // null if not a retry
}

// GateResultPayload is the gate execution result included in iteration events.
type GateResultPayload struct {
	ExitCode   *int   `json:"exit_code"` // null for timeout/pre-exec error
	Stdout     string `json:"stdout"`
	Stderr     string `json:"stderr"`
	DurationMs int64  `json:"duration_ms"`
	Truncated  bool   `json:"truncated"`
}

// IterationPayload is the structured payload for ConvergenceIteration events.
type IterationPayload struct {
	Rig                  string             `json:"rig,omitempty"`
	Iteration            int                `json:"iteration"`
	WispID               string             `json:"wisp_id"`
	AgentVerdict         string             `json:"agent_verdict"`
	GateMode             string             `json:"gate_mode"`
	GateOutcome          *string            `json:"gate_outcome"` // null when no gate evaluated
	GateResult           *GateResultPayload `json:"gate_result"`  // null when no gate evaluated
	GateRetryCount       int                `json:"gate_retry_count"`
	Action               string             `json:"action"`         // iterate|approved|no_convergence|waiting_manual|stopped
	WaitingReason        *string            `json:"waiting_reason"` // present only for waiting_manual
	NextWispID           *string            `json:"next_wisp_id"`   // present only for iterate
	IterationDurationMs  int64              `json:"iteration_duration_ms"`
	CumulativeDurationMs int64              `json:"cumulative_duration_ms"`
	IterationTokens      *int64             `json:"iteration_tokens"`  // null if unavailable
	CumulativeTokens     *int64             `json:"cumulative_tokens"` // null if unavailable
}

// TerminatedPayload is the structured payload for ConvergenceTerminated events.
type TerminatedPayload struct {
	Rig                  string `json:"rig,omitempty"`
	TerminalReason       string `json:"terminal_reason"` // approved|no_convergence|stopped
	TotalIterations      int    `json:"total_iterations"`
	FinalStatus          string `json:"final_status"` // always "closed"
	Actor                string `json:"actor"`        // controller or operator:<username>
	CumulativeDurationMs int64  `json:"cumulative_duration_ms"`
}

// WaitingManualPayload is the structured payload for ConvergenceWaitingManual events.
type WaitingManualPayload struct {
	Rig                  string             `json:"rig,omitempty"`
	Iteration            int                `json:"iteration"`
	WispID               string             `json:"wisp_id"`
	AgentVerdict         string             `json:"agent_verdict"`
	GateMode             string             `json:"gate_mode"`
	GateOutcome          *string            `json:"gate_outcome"` // null for pure manual
	GateResult           *GateResultPayload `json:"gate_result"`  // null for pure manual
	Reason               string             `json:"reason"`       // manual|hybrid_no_condition|timeout|sling_failure
	IterationDurationMs  int64              `json:"iteration_duration_ms"`
	CumulativeDurationMs int64              `json:"cumulative_duration_ms"`
}

// ManualActionPayload is the structured payload for the state-transition
// events ConvergenceManualApprove, ConvergenceManualIterate, ConvergenceManualStop
// (operator-driven), and ConvergenceTriggerAdvance (controller-driven).
type ManualActionPayload struct {
	Rig        string  `json:"rig,omitempty"`
	Actor      string  `json:"actor"` // operator:<username>, or "controller" for trigger_advance
	PriorState string  `json:"prior_state"`
	NewState   string  `json:"new_state"`
	Iteration  int     `json:"iteration"`
	WispID     *string `json:"wisp_id"`      // null if none
	NextWispID *string `json:"next_wisp_id"` // null for approve/stop
}

// MarshalPayload marshals a payload struct to json.RawMessage.
func MarshalPayload(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return data
}

// EventEmitter abstracts event recording for the convergence handler.
// The controller implements this by wrapping events.Recorder.
type EventEmitter interface {
	Emit(eventType, eventID, beadID string, payload json.RawMessage, recovery bool)
}

// EmittedEvent holds all fields needed to emit a convergence event.
type EmittedEvent struct {
	Type     string
	EventID  string
	BeadID   string
	Payload  json.RawMessage
	Recovery bool
	Ts       time.Time
}

// NullableString returns a pointer to s, or nil if s is empty.
func NullableString(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// GateResultToPayload converts a GateResult to a GateResultPayload for events.
// Returns nil if the gate result has no meaningful content (manual mode).
func GateResultToPayload(r GateResult) *GateResultPayload {
	if r.Outcome == "" {
		return nil
	}
	return &GateResultPayload{
		ExitCode:   r.ExitCode,
		Stdout:     r.Stdout,
		Stderr:     r.Stderr,
		DurationMs: r.Duration.Milliseconds(),
		Truncated:  r.Truncated,
	}
}
