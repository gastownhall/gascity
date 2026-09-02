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

// ControlDispatcherScopeGapPayload is the typed payload for
// control.dispatcher_scope_gap events. It names one scope that owns open
// control work but configures no control-dispatcher, and counts the rows the
// reconciler suppressed from the tick's demand snapshot because of it.
//
// SuppressedCount is the whole point: the pre-existing stderr diagnostic names
// one bead per scope, so an operator reading it cannot tell a single stuck row
// from a hundred. The count is per build, not cumulative — the gap is a
// standing config condition, so consecutive emissions re-state it rather than
// adding to it.
type ControlDispatcherScopeGapPayload struct {
	// ScopeLabel is the operator-facing name of the gap, identical to the text
	// the stderr diagnostic prints: the leg the rows were collected through
	// plus the scope that owns them.
	ScopeLabel string `json:"scope_label"`
	// RigContext is the owning rig's name; empty means the city owns the rows.
	RigContext string `json:"rig_context,omitempty"`
	// StoreRef is the canonical ref of the leg the rows were collected through.
	// On a relocated city that is the class binding, which serves every scope
	// and owns none, so it is reported alongside RigContext rather than as the
	// owner.
	StoreRef string `json:"store_ref,omitempty"`
	// SuppressedCount is how many control rows this scope had suppressed from
	// the demand snapshot in the build that emitted the event.
	SuppressedCount int `json:"suppressed_count"`
	// SampleBeadID is one suppressed row, so an operator can start from a
	// concrete bead. Which row it is is not stable across builds: the repair
	// sweep starts at a rotating offset.
	SampleBeadID string `json:"sample_bead_id,omitempty"`
}

// IsEventPayload marks ControlDispatcherScopeGapPayload as an events.Payload variant.
func (ControlDispatcherScopeGapPayload) IsEventPayload() {}

// ControlDispatcherScopeGapPayloadJSON builds the JSON wire form for attachment
// to an Event.Payload field.
func ControlDispatcherScopeGapPayloadJSON(p ControlDispatcherScopeGapPayload) json.RawMessage {
	b, _ := json.Marshal(p) //nolint:errcheck // a struct of scalars cannot fail to marshal
	return b
}

func init() {
	RegisterPayload(ControlStalled, ControlStalledPayload{})
	RegisterPayload(ControlDispatcherScopeGap, ControlDispatcherScopeGapPayload{})
}
