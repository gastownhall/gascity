package events

// SessionProgramStatusShadowPayload is the typed payload for
// session.program_status_shadow: one shadow-mode decision, today's screen
// reading beside what the program's own OSC 7501 record would decide
// (design ga-f4ovjm.7 §9.2). It holds closed tokens and numbers only, never
// the program's msg/title text or any prompt text, which is what lets the
// records age out with the events log's own rotation.
//
// The point and verdict enums mirror internal/runtime's ProgramStatusPoint and
// ProgramVerdict tokens (cmd/gc TestShadowPayloadEnumsMatchRuntimeTokens).
type SessionProgramStatusShadowPayload struct {
	// SessionID is the session's bead id, empty when the decision point knew
	// only its runtime name (the envelope's subject).
	SessionID string `json:"session_id,omitempty" doc:"The session's bead id, when the decision point knows it."`
	// Family is the session's provider family (its resolved built-in
	// ancestor), config data and never a role.
	Family string `json:"family,omitempty" doc:"The session's provider family (resolved built-in ancestor)."`
	// Point is the decision point the comparison was taken at.
	Point string `json:"point" enum:"wait_for_idle,typing_gate,submit_confirm,escape_resend,readiness,startup_ladder,interrupt_settle,pending,wait_idle_preread,execution_backstop,claim_nudge_hold,work_ladder,proved_idle,queued_delivery,drain_reminder,observation" doc:"The decision point."`
	// Scrape is today's reading, the one that decided.
	Scrape string `json:"scrape" enum:"idle,busy,indeterminate,pending,confirmed,unconfirmed,ready,not_ready,settled,unsettled,hold,no_hold,resend,no_resend,none" doc:"Today's screen reading, which decided."`
	// Program is what the record-side rule would decide.
	Program string `json:"program" enum:"idle,busy,indeterminate,pending,confirmed,unconfirmed,ready,not_ready,settled,unsettled,hold,no_hold,resend,no_resend,none" doc:"What the program-status record would decide."`
	// Agree reports Scrape == Program.
	Agree bool `json:"agree" doc:"Whether the two readings agree."`
	// Validity is the record reading's validity; only valid readings count
	// toward agreement. unread is a reading taken while the capture gate
	// failed.
	Validity string `json:"validity" enum:"unread,valid,not_armed,silent,cleared,reader_lost,exited" doc:"The record reading's validity."`
	// State is the record's root state, set only for a valid reading.
	State string `json:"state,omitempty" enum:"idle,working,done,blocked,error" doc:"The record's root state (valid readings only)."`
	// Kind qualifies a blocked State.
	Kind string `json:"kind,omitempty" enum:"permission,question,auth" doc:"The blocked state's kind."`
	// HoldLive reports whether a hold the record creates still counts
	// (within the ten-minute hold bound).
	HoldLive bool `json:"hold_live" doc:"Whether a hold derived from the record is still within its bound."`
	// RecordAgeMs is how long the root has been in State, in milliseconds.
	RecordAgeMs int64 `json:"record_age_ms" doc:"How long the record's root has been in its state, in milliseconds."`
	// LatencyMs is the program signal's time minus the scrape signal's, at
	// timed points only; negative means the record was earlier.
	LatencyMs int64 `json:"latency_ms,omitempty" doc:"Program signal minus scrape signal, in milliseconds (timed points only)."`
	// Timed reports that LatencyMs was measured.
	Timed bool `json:"timed,omitempty" doc:"Whether latency_ms was measured."`
	// ReaderVerified reports that the process table confirmed the capture
	// reader.
	ReaderVerified bool `json:"reader_verified" doc:"Whether the process table confirmed the capture reader."`
	// Dropped counts the comparisons this process dropped, under queue
	// pressure, since its last recorded one.
	Dropped int `json:"dropped,omitempty" doc:"Comparisons this process dropped since its last recorded one."`
}

// IsEventPayload marks SessionProgramStatusShadowPayload as an events.Payload variant.
func (SessionProgramStatusShadowPayload) IsEventPayload() {}

func init() {
	RegisterPayload(SessionProgramStatusShadow, SessionProgramStatusShadowPayload{})
}
