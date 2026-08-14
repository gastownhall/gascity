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

func init() {
	RegisterPayload(ExecutionWorkAssociated, NoPayload{})
	RegisterPayload(ExecutionStepDefined, NoPayload{})
	RegisterPayload(ExecutionStepStarted, NoPayload{})
	RegisterPayload(ExecutionStepCompleted, NoPayload{})
	RegisterPayload(ExecutionClaimWindowExpired, ExecutionClaimWindowExpiredPayload{})
}
