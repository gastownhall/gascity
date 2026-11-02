package events

// SessionStartupKickoffGivenUpPayload is the typed payload for
// session.startup_kickoff_given_up. It names the session bead, the bound work
// step its startup kickoff never advanced, and how many re-nudges the
// controller's named/direct startup backstop spent before giving up, so an
// operator can tell a session that never started its step apart from a slow
// one. Attempts is the delivered-nudge count at exhaustion, not a retry budget
// the consumer should act on.
type SessionStartupKickoffGivenUpPayload struct {
	SessionID   string `json:"session_id"`
	BoundStepID string `json:"bound_step_id"`
	Attempts    int    `json:"attempts"`
}

// IsEventPayload marks SessionStartupKickoffGivenUpPayload as an events.Payload variant.
func (SessionStartupKickoffGivenUpPayload) IsEventPayload() {}

func init() {
	RegisterPayload(SessionStartupKickoffGivenUp, SessionStartupKickoffGivenUpPayload{})
}
