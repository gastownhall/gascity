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

// OrderFailingPayload is the typed payload for order.failing events: which
// scoped order keeps failing, how many consecutive failures, since when, and
// the latest failure's message.
//
// FirstFailed is the streak anchor (RFC3339), the failure that opened the
// current run, and stays fixed for every alert of one streak, so a consumer
// can use it to tell one failure episode from the next. FailingForMS is the
// gap from it to the failure that alerted. LastError is that failure's
// order.failed message, already bounded and redacted at its emit site.
type OrderFailingPayload struct {
	OrderName    string `json:"order_name"`
	Consecutive  int    `json:"consecutive"`
	FirstFailed  string `json:"first_failed"`
	FailingForMS int64  `json:"failing_for_ms"`
	LastError    string `json:"last_error"`
}

// IsEventPayload marks OrderFailingPayload as an events.Payload variant.
func (OrderFailingPayload) IsEventPayload() {}

// OrderFailingPayloadJSON builds the JSON wire form for attachment to an
// Event.Payload field.
func OrderFailingPayloadJSON(p OrderFailingPayload) json.RawMessage {
	b, _ := json.Marshal(p) //nolint:errcheck // a struct of scalars cannot fail to marshal
	return b
}

// OrderRecoveredPayload is the typed payload for order.recovered events: the
// order whose alerted failure streak a success ended, how many consecutive
// failures it counted, and its anchor. FirstFailed (RFC3339) equals the
// first_failed of every order.failing the streak raised, so a consumer can
// match the recovery to its alerts. FailedForMS is the gap from it to the
// success.
type OrderRecoveredPayload struct {
	OrderName   string `json:"order_name"`
	Consecutive int    `json:"consecutive"`
	FirstFailed string `json:"first_failed"`
	FailedForMS int64  `json:"failed_for_ms"`
}

// IsEventPayload marks OrderRecoveredPayload as an events.Payload variant.
func (OrderRecoveredPayload) IsEventPayload() {}

// OrderRecoveredPayloadJSON builds the JSON wire form for attachment to an
// Event.Payload field.
func OrderRecoveredPayloadJSON(p OrderRecoveredPayload) json.RawMessage {
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

// OrderInapplicablePayload is the typed payload for order.inapplicable
// events: which order the dispatcher will not run, for which bead scope
// ("city" or "rig:<name>"), the backend that scope uses, and the backends the
// order requires.
type OrderInapplicablePayload struct {
	OrderName       string   `json:"order_name"`
	Scope           string   `json:"scope"`
	Backend         string   `json:"backend"`
	RequiresBackend []string `json:"requires_backend"`
}

// IsEventPayload marks OrderInapplicablePayload as an events.Payload variant.
func (OrderInapplicablePayload) IsEventPayload() {}

// OrderInapplicablePayloadJSON builds the JSON wire form for attachment to an
// Event.Payload field.
func OrderInapplicablePayloadJSON(p OrderInapplicablePayload) json.RawMessage {
	if p.RequiresBackend == nil {
		p.RequiresBackend = []string{}
	}
	b, _ := json.Marshal(p) //nolint:errcheck // a struct of strings and a string slice cannot fail to marshal
	return b
}

func init() {
	RegisterPayload(OrderSuppressed, OrderSuppressedPayload{})
	RegisterPayload(OrderFailing, OrderFailingPayload{})
	RegisterPayload(OrderRecovered, OrderRecoveredPayload{})
	RegisterPayload(OrderSkipped, OrderSkippedPayload{})
	RegisterPayload(OrderInapplicable, OrderInapplicablePayload{})
}
