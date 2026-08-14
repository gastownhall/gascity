package events

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
	RegisterPayload(ExecutionStepDefined, NoPayload{})
	RegisterPayload(ExecutionStepStarted, NoPayload{})
	RegisterPayload(ExecutionStepCompleted, NoPayload{})
	RegisterPayload(ExecutionStepStalled, ExecutionStepStalledPayload{})
}
