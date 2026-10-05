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
//
// The Blocker fields name WHAT to act on. The streak fields answer "how long has
// this order been held back"; they cannot answer "held back by what", and that
// is the question an operator has to answer before anything changes — the gate
// reopens when the blocking bead closes, so its id is the whole remedy. They are
// resolved by a separate best-effort read at emission time (never per tick), so
// they are omitempty: a lookup that fails or races the blocker closing must
// leave the event exactly as informative as it was before these existed, never
// suppress it.
//
// BlockerAgeMS is the blocker's own age at emission, which is NOT SuppressedForMS:
// the streak starts at the first tick on which the order was DUE and refused,
// while the blocker has usually been open since the previous run poured it. For
// a 4h cron whose wisp was never worked, the blocker is ~4h older than the
// streak. Both are carried because the gap between them is itself diagnostic.
type OrderSuppressedPayload struct {
	OrderName       string `json:"order_name"`
	Consecutive     int    `json:"consecutive"`
	FirstSuppressed string `json:"first_suppressed"`
	SuppressedForMS int64  `json:"suppressed_for_ms"`
	BlockerID       string `json:"blocker_id,omitempty"`
	BlockerKind     string `json:"blocker_kind,omitempty"`
	BlockerTitle    string `json:"blocker_title,omitempty"`
	BlockerAgeMS    int64  `json:"blocker_age_ms,omitempty"`
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
