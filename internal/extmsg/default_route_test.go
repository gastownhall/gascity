package extmsg

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func defaultRouteTestMessage(ref ConversationRef) ExternalInboundMessage {
	return ExternalInboundMessage{
		Conversation: ref,
		Actor:        ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:         "anyone home?",
		ReceivedAt:   testNow(),
	}
}

func TestHandleInboundNormalizedDefaultRouteBindsConfiguredAgent(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	deps := InboundDeps{
		Services: fabric,
		DefaultAgentForConversation: func(got ConversationRef) string {
			if !sameConversationRef(got, ref) {
				t.Fatalf("resolver called with %#v, want %#v", got, ref)
			}
			return "myrig/frontdesk"
		},
	}
	result, err := HandleInboundNormalized(context.Background(), deps, defaultRouteTestMessage(ref))
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetAgentName != "myrig/frontdesk" {
		t.Fatalf("TargetAgentName = %q, want myrig/frontdesk", result.TargetAgentName)
	}
	if result.Binding == nil || result.Binding.AgentName != "myrig/frontdesk" {
		t.Fatalf("Binding = %#v, want agent binding myrig/frontdesk", result.Binding)
	}
	if result.TranscriptEntry == nil {
		t.Fatalf("TranscriptEntry = nil, want inbound appended after default route")
	}

	// The default route is sticky: a durable agent binding now exists, so
	// the next inbound routes through it without consulting the resolver.
	binding, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if binding == nil || binding.AgentName != "myrig/frontdesk" {
		t.Fatalf("persisted binding = %#v, want agent binding", binding)
	}
	deps.DefaultAgentForConversation = func(ConversationRef) string {
		t.Fatal("resolver consulted for an already-bound conversation")
		return ""
	}
	again, err := HandleInboundNormalized(context.Background(), deps, defaultRouteTestMessage(ref))
	if err != nil {
		t.Fatalf("HandleInboundNormalized(second): %v", err)
	}
	if again.TargetAgentName != "myrig/frontdesk" {
		t.Fatalf("second TargetAgentName = %q, want myrig/frontdesk", again.TargetAgentName)
	}
}

func TestHandleInboundNormalizedDefaultRouteAbsentPreservesUnbound(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	// No resolver wired (config absent) — unrouted, no binding created.
	result, err := HandleInboundNormalized(context.Background(), InboundDeps{Services: fabric}, defaultRouteTestMessage(ref))
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetSessionID != "" || result.TargetAgentName != "" {
		t.Fatalf("result routed (%q/%q), want unrouted", result.TargetSessionID, result.TargetAgentName)
	}

	// Resolver wired but no route for this conversation — same.
	deps := InboundDeps{
		Services:                    fabric,
		DefaultAgentForConversation: func(ConversationRef) string { return "" },
	}
	result, err = HandleInboundNormalized(context.Background(), deps, defaultRouteTestMessage(ref))
	if err != nil {
		t.Fatalf("HandleInboundNormalized(empty route): %v", err)
	}
	if result.TargetSessionID != "" || result.TargetAgentName != "" {
		t.Fatalf("result routed (%q/%q), want unrouted", result.TargetSessionID, result.TargetAgentName)
	}
	binding, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if binding != nil {
		t.Fatalf("binding = %#v, want none", binding)
	}
}

func TestHandleInboundNormalizedDefaultRouteConflictAdoptsExistingBinding(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	// The resolver simulates a concurrent racer: by the time the default
	// route tries to bind, a session binding already exists. The pipeline
	// must adopt the active binding instead of failing the inbound.
	deps := InboundDeps{
		Services: fabric,
		DefaultAgentForConversation: func(ConversationRef) string {
			if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
				Conversation: ref,
				SessionID:    "sess-racer",
				Now:          testNow(),
			}); err != nil {
				t.Fatalf("racer Bind: %v", err)
			}
			return "myrig/frontdesk"
		},
	}
	result, err := HandleInboundNormalized(context.Background(), deps, defaultRouteTestMessage(ref))
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetSessionID != "sess-racer" || result.TargetAgentName != "" {
		t.Fatalf("result = %q/%q, want racer session binding adopted", result.TargetSessionID, result.TargetAgentName)
	}
}
