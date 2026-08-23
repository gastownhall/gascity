package events

// ExecutionClaimWindowExpiredPayload is the typed payload for
// execution.claim_window_expired events. It is the fleet's own orphan report:
// InvocationAgeMS is how long the refusing `gc hook --claim` process had been
// running when it reached the mutation, and ParentAlive is false when that
// process has been reparented to init — the process-table signature of a
// provider tool call that was killed or abandoned while its claim command
// survived. BeadID is the candidate that was NOT claimed.
type ExecutionClaimWindowExpiredPayload struct {
	BeadID          string `json:"bead_id"`
	InvocationAgeMS int64  `json:"invocation_age_ms"`
	ParentAlive     bool   `json:"parent_alive"`
}

// IsEventPayload marks ExecutionClaimWindowExpiredPayload as an events.Payload variant.
func (ExecutionClaimWindowExpiredPayload) IsEventPayload() {}

// ExecutionStepStalledPayload is the typed payload for execution.step_stalled.
// It names the claim that was never executed and how many claim nudges the
// controller's execution backstop spent before giving up, so an operator (or a
// dashboard) can tell a stalled claim apart from a slow one without correlating
// three other event streams. Attempts is the delivered-nudge count at
// exhaustion, not a retry budget the consumer should act on.
type ExecutionStepStalledPayload struct {
	BeadID     string `json:"bead_id"`
	RootBeadID string `json:"root_bead_id,omitempty"`
	SessionID  string `json:"session_id"`
	Attempts   int    `json:"attempts"`
}

// IsEventPayload marks ExecutionStepStalledPayload as an events.Payload variant.
func (ExecutionStepStalledPayload) IsEventPayload() {}

func init() {
	RegisterPayload(ExecutionWorkAssociated, NoPayload{})
	RegisterPayload(ExecutionRunAnchored, NoPayload{})
	RegisterPayload(ExecutionStepDefined, NoPayload{})
	RegisterPayload(ExecutionStepStarted, NoPayload{})
	RegisterPayload(ExecutionStepCompleted, NoPayload{})
	RegisterPayload(ExecutionClaimWindowExpired, ExecutionClaimWindowExpiredPayload{})
	RegisterPayload(ExecutionStepStalled, ExecutionStepStalledPayload{})
}
