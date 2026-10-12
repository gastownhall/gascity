package events

import "encoding/json"

// OrderSuppressedPayload is the typed payload for order.suppressed events. It
// carries everything needed to name a stalled order without a second query:
// which scoped order is being held back, how many consecutive dispatch checks
// its open-work gate has refused, and how long that has been going on.
//
// FirstSuppressed is the streak anchor (RFC3339) — the tick that opened the
// current run of refusals, not the emission time, which is already the
// envelope's Ts. SuppressedForMS is the gap between them, carried explicitly so
// a reader does not have to do date arithmetic to answer "how long".
type OrderSuppressedPayload struct {
	OrderName       string `json:"order_name"`
	Consecutive     int    `json:"consecutive"`
	FirstSuppressed string `json:"first_suppressed"`
	SuppressedForMS int64  `json:"suppressed_for_ms"`
}

// IsEventPayload marks OrderSuppressedPayload as an events.Payload variant.
func (OrderSuppressedPayload) IsEventPayload() {}

// OrderSuppressedPayloadJSON builds the JSON wire form for attachment to an
// Event.Payload field.
func OrderSuppressedPayloadJSON(p OrderSuppressedPayload) json.RawMessage {
	b, _ := json.Marshal(p) //nolint:errcheck // a struct of scalars cannot fail to marshal
	return b
}

// OrderSkippedPayload is the typed payload for order.skipped events: which
// order declared it, whether nothing ("skipped") or only some ("partial") of
// its work ran, and the scopes or steps that did not run.
type OrderSkippedPayload struct {
	OrderName string              `json:"order_name"`
	Outcome   string              `json:"outcome"`
	Reason    string              `json:"reason"`
	Scopes    []OrderSkippedScope `json:"scopes"`
}

// OrderSkippedScope is one scope or step an exec order reports as not run.
type OrderSkippedScope struct {
	Scope  string `json:"scope"`
	Reason string `json:"reason"`
}

// IsEventPayload marks OrderSkippedPayload as an events.Payload variant.
func (OrderSkippedPayload) IsEventPayload() {}

// OrderSkippedPayloadJSON builds the JSON wire form for attachment to an
// Event.Payload field.
func OrderSkippedPayloadJSON(p OrderSkippedPayload) json.RawMessage {
	if p.Scopes == nil {
		p.Scopes = []OrderSkippedScope{}
	}
	b, _ := json.Marshal(p) //nolint:errcheck // a struct of strings and string slices cannot fail to marshal
	return b
}

func init() {
	RegisterPayload(OrderSuppressed, OrderSuppressedPayload{})
	RegisterPayload(OrderSkipped, OrderSkippedPayload{})
}
