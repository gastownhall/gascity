package events

import "encoding/json"

// NudgeDialogBlockedPayload is the typed payload for nudge.dialog_blocked
// events. It fires when the queued-nudge delivery gate defers because a
// dialog (not ordinary pane activity) owns the target session's input — the
// case the gate/confirm asymmetry used to render invisible: the gate saw the
// pane as idle-enough while a dialog silently absorbed every re-paste.
//
// BeadID is usually empty: the gate check runs at the session/target level,
// before any specific queued item is claimed, so no bead is naturally in
// scope yet.
type NudgeDialogBlockedPayload struct {
	SessionID  string `json:"session_id"`
	DialogKind string `json:"dialog_kind"`
	BeadID     string `json:"bead_id,omitempty"`
}

// IsEventPayload marks NudgeDialogBlockedPayload as an events.Payload variant.
func (NudgeDialogBlockedPayload) IsEventPayload() {}

// NudgeDialogBlockedPayloadJSON builds the JSON wire form for attachment to
// an Event.Payload field.
func NudgeDialogBlockedPayloadJSON(p NudgeDialogBlockedPayload) json.RawMessage {
	b, _ := json.Marshal(p) //nolint:errcheck // a struct of scalars cannot fail to marshal
	return b
}

func init() {
	RegisterPayload(NudgeDialogBlocked, NudgeDialogBlockedPayload{})
}
