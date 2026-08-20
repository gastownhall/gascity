package events

import "encoding/json"

// ControlStalledPayload is the typed payload for control.stalled events. It
// carries everything an operator needs to name the deadlock without reading a
// trace log: which control bead gave up, in which workflow and store, what the
// store kept refusing, and for how long.
//
// FirstSeen is the persisted budget anchor (RFC3339), not the emission time —
// the emission time is already the envelope's Ts, and the gap between them is
// the whole diagnostic.
type ControlStalledPayload struct {
	BeadID     string `json:"bead_id"`
	Kind       string `json:"kind,omitempty"`
	RootBeadID string `json:"root_bead_id,omitempty"`
	StorePath  string `json:"store_path,omitempty"`
	// ErrorClass names the tier that ran out of budget ("semantic").
	ErrorClass string `json:"error_class"`
	FirstSeen  string `json:"first_seen"`
	Attempts   int    `json:"attempts"`
	// Error is the store's refusal, truncated at the record site.
	Error string `json:"error"`
	// OrderName is the scoped order this run belongs to, when the workflow root
	// carries an order-run label. Empty for a workflow nothing scheduled.
	OrderName string `json:"order_name,omitempty"`
}

// IsEventPayload marks ControlStalledPayload as an events.Payload variant.
func (ControlStalledPayload) IsEventPayload() {}

// ControlStalledPayloadJSON builds the JSON wire form for attachment to an
// Event.Payload field.
func ControlStalledPayloadJSON(p ControlStalledPayload) json.RawMessage {
	b, _ := json.Marshal(p) //nolint:errcheck // a struct of scalars cannot fail to marshal
	return b
}

func init() {
	RegisterPayload(ControlStalled, ControlStalledPayload{})
}
