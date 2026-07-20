package extmsg

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// An inbound message that resolves to no binding, no group route, and no
// default route is accepted (200) and intentionally not delivered — but the
// drop must be observable, not silent. HandleInboundNormalized emits
// events.ExtMsgInboundDropped so operators can see accepted-then-vanished
// messages in the event log (hq-ar4: Slack messages 200-accepted by
// extmsg/inbound disappeared without any trace).
func TestHandleInboundNormalizedUnroutedEmitsInboundDropped(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	var captured []capturedEvent
	deps := InboundDeps{
		Services: fabric,
		EmitEvent: func(eventType, subject string, payload events.Payload) {
			captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
		},
	}
	result, err := HandleInboundNormalized(context.Background(), deps, ExternalInboundMessage{
		Conversation:   ref,
		Actor:          ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:           "hello, anyone there?",
		ExplicitTarget: "mayor",
		ReceivedAt:     testNow(),
	})
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetSessionID != "" || result.TargetAgentName != "" {
		t.Fatalf("result routed (%q/%q), want unrouted", result.TargetSessionID, result.TargetAgentName)
	}
	if len(captured) != 1 || captured[0].Type != events.ExtMsgInboundDropped {
		t.Fatalf("events = %#v, want one %s event", captured, events.ExtMsgInboundDropped)
	}
	payload, ok := captured[0].Payload.(InboundDroppedEventPayload)
	if !ok {
		t.Fatalf("payload = %#v, want InboundDroppedEventPayload", captured[0].Payload)
	}
	if payload.Provider != ref.Provider || payload.ConversationID != ref.ConversationID {
		t.Fatalf("payload conversation = %q/%q, want %q/%q", payload.Provider, payload.ConversationID, ref.Provider, ref.ConversationID)
	}
	if payload.Actor != "User One" {
		t.Fatalf("payload.Actor = %q, want User One", payload.Actor)
	}
	if payload.ExplicitTarget != "mayor" {
		t.Fatalf("payload.ExplicitTarget = %q, want mayor", payload.ExplicitTarget)
	}
}

// A routed inbound must NOT emit the dropped event — only the regular
// extmsg.inbound event.
func TestHandleInboundNormalizedRoutedDoesNotEmitInboundDropped(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()
	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	var captured []capturedEvent
	deps := InboundDeps{
		Services: fabric,
		EmitEvent: func(eventType, subject string, payload events.Payload) {
			captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
		},
	}
	result, err := HandleInboundNormalized(context.Background(), deps, ExternalInboundMessage{
		Conversation: ref,
		Actor:        ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:         "hello",
		ReceivedAt:   testNow(),
	})
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetSessionID != "sess-a" {
		t.Fatalf("TargetSessionID = %q, want sess-a", result.TargetSessionID)
	}
	for _, evt := range captured {
		if evt.Type == events.ExtMsgInboundDropped {
			t.Fatalf("routed inbound emitted %s: %#v", events.ExtMsgInboundDropped, evt)
		}
	}
}
