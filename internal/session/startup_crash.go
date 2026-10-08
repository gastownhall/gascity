package session

import (
	"encoding/json"

	"github.com/gastownhall/gascity/internal/events"
)

// startupCrashPayload is the JSON shape of api.SessionLifecyclePayload for
// session.crashed. Duplicated here so session does not import the API
// projection layer.
type startupCrashPayload struct {
	SessionID string `json:"session_id"`
	Template  string `json:"template,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

func startupCrashPayloadJSON(sessionID, template string) json.RawMessage {
	b, _ := json.Marshal(startupCrashPayload{
		SessionID: sessionID,
		Template:  template,
		Reason:    string(SleepReasonStartupCrash),
	})
	return b
}

// recordStartupCrash emits events.SessionCrashed for a terminal provider
// death during Start. persistReason stamps sleep_reason=startup-crash on
// the surviving bead (wake/resume paths). Create rollback closes the bead,
// so persistReason is false there.
func (m *Manager) recordStartupCrash(id, sessName, template string, startErr error, persistReason bool) {
	if persistReason && id != "" && m.store != nil {
		_ = m.store.SetMetadata(id, "sleep_reason", string(SleepReasonStartupCrash))
	}
	if m.rec == nil {
		return
	}
	msg := ""
	if startErr != nil {
		msg = startErr.Error()
	}
	m.rec.Record(events.Event{
		Type:      events.SessionCrashed,
		Actor:     "gc",
		Subject:   sessName,
		SessionID: id,
		Message:   msg,
		Payload:   startupCrashPayloadJSON(id, template),
	})
}
