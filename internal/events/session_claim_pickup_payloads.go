package events

// SessionClaimPickupExhaustedPayload is the typed payload for
// session.claim_pickup_exhausted. It names the session bead and runtime
// session, the open assigned bead the seat never picked up, and how many
// re-nudges the controller's named-seat claim-pickup backstop spent before
// giving up, so an operator can tell a seat that never noticed its work apart
// from a slow one. Attempts is the delivered-nudge count at exhaustion, not a
// retry budget the consumer should act on.
type SessionClaimPickupExhaustedPayload struct {
	SessionID   string `json:"session_id"`
	SessionName string `json:"session_name"`
	BeadID      string `json:"bead_id"`
	Attempts    int    `json:"attempts"`
}

// IsEventPayload marks SessionClaimPickupExhaustedPayload as an events.Payload variant.
func (SessionClaimPickupExhaustedPayload) IsEventPayload() {}

func init() {
	RegisterPayload(SessionClaimPickupExhausted, SessionClaimPickupExhaustedPayload{})
}
