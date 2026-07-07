package api

import "github.com/gastownhall/gascity/internal/events"

// Webhook rejection reason enum. These are the stable strings carried on
// WebhookRejectedPayload.Reason so operators can alert/aggregate on a rejection
// class without parsing free text. The security-relevant classes the design and
// red-team call out (perimeter_denied, read_only, verify_failed, operator_fault,
// rate_limited, dispatch_refused) are here alongside the operational ones
// (method_not_allowed, body_too_large, bad_body, bad_payload, match_error,
// dispatch_unavailable/dispatch_error) that keep the receiver debuggable.
//
// Notes on two design decisions:
//   - An unresolved route (unknown webhook name) is intentionally NOT evented:
//     the route segment is chosen by an unauthenticated caller, so emitting there
//     would be an event-log-flood amplification vector and a name-existence oracle
//     (it would also violate R2's "never confirm which hooks exist"). The receiver
//     404s such probes silently, so there is no unknown_webhook reason.
//   - no-match is classified as webhook.received (an accepted, authentic 2xx
//     delivery that no rule wanted), NOT as a rejection — so there is no
//     no_match reason.
const (
	reasonMethodNotAllowed    = "method_not_allowed"
	reasonPerimeterDenied     = "perimeter_denied"
	reasonReadOnly            = "read_only"
	reasonRateLimited         = "rate_limited"
	reasonBodyTooLarge        = "body_too_large"
	reasonBadBody             = "bad_body"
	reasonOperatorFault       = "operator_fault"
	reasonVerifyFailed        = "verify_failed"
	reasonBadPayload          = "bad_payload"
	reasonMatchError          = "match_error"
	reasonDispatchRefused     = "dispatch_refused"
	reasonDispatchUnavailable = "dispatch_unavailable"
	reasonDispatchError       = "dispatch_error"
)

// WebhookEventSink receives the E8 observability events at the receiver's
// accept/reject decision points. It is the injection seam E3 stubbed on Server:
// production forwards to the city event bus (cityEventWebhookSink), and tests
// substitute a fake to assert exactly which events fire with which fields. The
// receiver only ever hands it the typed payloads, so an event can never carry an
// ad-hoc shape (Principle 7). The methods take the payload by value; there is no
// context parameter because the underlying events.Recorder.Record is itself
// context-free (matching EmitTypedEvent / the extmsg emitter).
type WebhookEventSink interface {
	Received(WebhookReceivedPayload)
	Rejected(WebhookRejectedPayload)
}

// cityEventWebhookSink is the production WebhookEventSink: it records the typed
// payload onto the per-city event bus, where it flows to gc orders feed and the
// typed /v0/city/{city}/events/stream projection alongside OrderFired. A nil
// recorder (events disabled) makes both methods no-ops.
type cityEventWebhookSink struct {
	rec events.Recorder
}

// Received records a webhook.received event.
func (s cityEventWebhookSink) Received(ev WebhookReceivedPayload) {
	if s.rec == nil {
		return
	}
	EmitTypedEvent(s.rec, events.WebhookReceived, ev.Webhook, ev)
}

// Rejected records a webhook.rejected event.
func (s cityEventWebhookSink) Rejected(ev WebhookRejectedPayload) {
	if s.rec == nil {
		return
	}
	EmitTypedEvent(s.rec, events.WebhookRejected, ev.Webhook, ev)
}

// webhookEventSink returns the configured sink, defaulting to the city event bus
// when no test override is injected. Resolving the recorder lazily (rather than
// caching it on Server) keeps the sink correct if EventProvider changes and lets
// the field stay a pure test seam.
func (s *Server) webhookEventSink() WebhookEventSink {
	if s.webhookEvents != nil {
		return s.webhookEvents
	}
	return cityEventWebhookSink{rec: s.state.EventProvider()}
}

// emitWebhookReceived records a webhook.received event through the active sink.
func (s *Server) emitWebhookReceived(ev WebhookReceivedPayload) {
	s.webhookEventSink().Received(ev)
}

// emitWebhookRejected records a webhook.rejected event through the active sink.
func (s *Server) emitWebhookRejected(ev WebhookRejectedPayload) {
	s.webhookEventSink().Rejected(ev)
}
