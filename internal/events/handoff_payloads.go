package events

// SessionHandoffStagedPayload is the typed payload for session.handoff_staged
// events. SessionKey identifies the handing-off session; MessageID is the
// staged mail.Message's bead id.
type SessionHandoffStagedPayload struct {
	SessionKey string `json:"session_key"`
	MessageID  string `json:"message_id"`
}

// IsEventPayload marks SessionHandoffStagedPayload as an events.Payload variant.
func (SessionHandoffStagedPayload) IsEventPayload() {}

// SessionHandoffRestartAcceptedPayload is the typed payload for
// session.handoff_restart_accepted events.
type SessionHandoffRestartAcceptedPayload struct {
	SessionKey string `json:"session_key"`
	MessageID  string `json:"message_id"`
}

// IsEventPayload marks SessionHandoffRestartAcceptedPayload as an events.Payload variant.
func (SessionHandoffRestartAcceptedPayload) IsEventPayload() {}

// SessionHandoffSuccessorStartedPayload is the typed payload for
// session.handoff_successor_started events.
type SessionHandoffSuccessorStartedPayload struct {
	SessionKey string `json:"session_key"`
	MessageID  string `json:"message_id"`
}

// IsEventPayload marks SessionHandoffSuccessorStartedPayload as an events.Payload variant.
func (SessionHandoffSuccessorStartedPayload) IsEventPayload() {}

// SessionHandoffReleasedPayload is the typed payload for
// session.handoff_released events.
type SessionHandoffReleasedPayload struct {
	SessionKey string `json:"session_key"`
	MessageID  string `json:"message_id"`
}

// IsEventPayload marks SessionHandoffReleasedPayload as an events.Payload variant.
func (SessionHandoffReleasedPayload) IsEventPayload() {}

// SessionHandoffFailedPayload is the typed payload for session.handoff_failed
// events. Reason carries a short human-readable cause (a persist error's
// message, or a fixed release-timeout description).
type SessionHandoffFailedPayload struct {
	SessionKey string `json:"session_key"`
	Reason     string `json:"reason"`
}

// IsEventPayload marks SessionHandoffFailedPayload as an events.Payload variant.
func (SessionHandoffFailedPayload) IsEventPayload() {}

func init() {
	RegisterPayload(SessionHandoffStaged, SessionHandoffStagedPayload{})
	RegisterPayload(SessionHandoffRestartAccepted, SessionHandoffRestartAcceptedPayload{})
	RegisterPayload(SessionHandoffSuccessorStarted, SessionHandoffSuccessorStartedPayload{})
	RegisterPayload(SessionHandoffReleased, SessionHandoffReleasedPayload{})
	RegisterPayload(SessionHandoffFailed, SessionHandoffFailedPayload{})
}
