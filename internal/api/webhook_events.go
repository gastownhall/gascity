package api

import "github.com/gastownhall/gascity/internal/events"

// Webhook rejection reason enum. These are the stable strings carried on
// WebhookRejectedPayload.Reason so operators can alert/aggregate on a rejection
// class without parsing free text. The evented classes are the ones PAST the
// rate limiter — the auth decisions (source_denied, bearer_failed, verify_failed,
// operator_fault) and the dispatch/payload outcomes (bad_body, body_too_large,
// bad_payload, match_error, dispatch_refused, dispatch_unavailable,
// dispatch_error) — which are bounded and diagnostically useful.
//
// Notes on the deliberately NON-evented paths:
//   - An unresolved route (unknown webhook name), a visibility-perimeter/read-only
//     denial (webhookRequestAllowed), a rate-limit 429, and a non-POST method are
//     all cheap, unauthenticated, attacker-fully-controlled rejects that run at or
//     before the limiter. Eventing them would be an event-log-flood amplification
//     vector and a name-existence oracle (and would violate R2's "never confirm
//     which hooks exist"), so the receiver rejects them silently — there is no
//     reason string for them.
//   - no-match is classified as webhook.received (an accepted, authentic 2xx
//     delivery that no rule wanted), NOT as a rejection — so there is no
//     no_match reason.
const (
	reasonSourceDenied        = "source_denied"
	reasonBearerFailed        = "bearer_failed"
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
