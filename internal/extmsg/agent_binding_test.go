package extmsg

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

func TestBindingServiceBindAgentNameCreatesAgentBinding(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	svc := fabric.Bindings
	ref := testConversationRef()

	first, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}
	if first.AgentName != "rig-a/helper" {
		t.Fatalf("AgentName = %q, want rig-a/helper", first.AgentName)
	}
	if first.SessionID != "" {
		t.Fatalf("SessionID = %q, want empty for agent binding", first.SessionID)
	}
	if first.BindingGeneration != 1 {
		t.Fatalf("BindingGeneration = %d, want 1", first.BindingGeneration)
	}

	// Membership is keyed by the agent name so the notify path resolves it
	// as a session selector (materializing a session when none is live).
	members, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 1 || members[0].SessionID != "rig-a/helper" {
		t.Fatalf("memberships = %#v, want one keyed rig-a/helper", members)
	}

	// Idempotent re-bind of the same agent keeps the record.
	second, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Bind(idempotent agent): %v", err)
	}
	if second.ID != first.ID || second.BindingGeneration != first.BindingGeneration {
		t.Fatalf("idempotent agent bind changed record: got %s/%d want %s/%d",
			second.ID, second.BindingGeneration, first.ID, first.BindingGeneration)
	}

	// A different agent conflicts.
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/other",
		Now:          testNow(),
	}); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("Bind(other agent) error = %v, want ErrBindingConflict", err)
	}

	// A session bind conflicts with the active agent binding.
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); !errors.Is(err, ErrBindingConflict) {
		t.Fatalf("Bind(session over agent) error = %v, want ErrBindingConflict", err)
	}

	got, err := svc.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if got == nil || got.AgentName != "rig-a/helper" || got.SessionID != "" {
		t.Fatalf("ResolveByConversation = %#v, want agent binding rig-a/helper", got)
	}
}

func TestBindingServiceBindRequiresExactlyOneTarget(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	ref := testConversationRef()

	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		Now:          testNow(),
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Bind(neither) error = %v, want ErrInvalidInput", err)
	}

	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); !errors.Is(err, ErrInvalidInput) {
		t.Fatalf("Bind(both) error = %v, want ErrInvalidInput", err)
	}
}

func TestBindingServiceAgentBindingSurvivesSessionRetirement(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	svc := fabric.Bindings
	agentRef := testConversationRef()
	sessionRef := testConversationRef()
	sessionRef.ConversationID = "thread-2"

	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: agentRef,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: sessionRef,
		SessionID:    "sess-live-1",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(session): %v", err)
	}

	// Retire the concrete session that has been serving the agent. The
	// session-keyed binding dies with it; the agent binding and its
	// membership survive — conversation continuity does not depend on
	// session continuity.
	if err := CloseSessionBindings(context.Background(), store, "sess-live-1", testNow().Add(time.Minute)); err != nil {
		t.Fatalf("CloseSessionBindings: %v", err)
	}

	gone, err := svc.ResolveByConversation(context.Background(), sessionRef)
	if err != nil {
		t.Fatalf("ResolveByConversation(session): %v", err)
	}
	if gone != nil {
		t.Fatalf("session binding survived retirement: %#v", gone)
	}

	kept, err := svc.ResolveByConversation(context.Background(), agentRef)
	if err != nil {
		t.Fatalf("ResolveByConversation(agent): %v", err)
	}
	if kept == nil || kept.AgentName != "rig-a/helper" {
		t.Fatalf("agent binding = %#v, want active rig-a/helper", kept)
	}
	members, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), agentRef)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 1 || members[0].SessionID != "rig-a/helper" {
		t.Fatalf("agent membership = %#v, want one keyed rig-a/helper", members)
	}
}

func TestBindingServiceUnbindAgentBindingByConversation(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	svc := fabric.Bindings
	ref := testConversationRef()

	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}

	closed, err := svc.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		Conversation: &ref,
		Now:          testNow().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Unbind: %v", err)
	}
	if len(closed) != 1 || closed[0].AgentName != "rig-a/helper" || closed[0].Status != BindingEnded {
		t.Fatalf("Unbind closed = %#v, want ended agent binding", closed)
	}

	members, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("memberships after unbind = %#v, want none", members)
	}
}

func TestBindingServiceUnbindByAgentName(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewServices(store).Bindings
	agentRef := testConversationRef()
	otherRef := testConversationRef()
	otherRef.ConversationID = "thread-2"

	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: agentRef,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}
	if _, err := svc.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: otherRef,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(session): %v", err)
	}

	closed, err := svc.Unbind(context.Background(), testControllerCaller(), UnbindInput{
		AgentName: "rig-a/helper",
		Now:       testNow().Add(time.Minute),
	})
	if err != nil {
		t.Fatalf("Unbind(agent name): %v", err)
	}
	if len(closed) != 1 || closed[0].AgentName != "rig-a/helper" {
		t.Fatalf("Unbind closed = %#v, want the agent binding only", closed)
	}

	still, err := svc.ResolveByConversation(context.Background(), otherRef)
	if err != nil {
		t.Fatalf("ResolveByConversation(other): %v", err)
	}
	if still == nil || still.SessionID != "sess-a" {
		t.Fatalf("session binding = %#v, want untouched sess-a", still)
	}
}

func TestHandleInboundNormalizedAgentBindingRoutesAgentTarget(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
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
	if result.TargetAgentName != "rig-a/helper" {
		t.Fatalf("TargetAgentName = %q, want rig-a/helper", result.TargetAgentName)
	}
	if result.TargetSessionID != "" {
		t.Fatalf("TargetSessionID = %q, want empty for agent binding", result.TargetSessionID)
	}
	if result.TranscriptEntry == nil {
		t.Fatalf("TranscriptEntry = nil, want inbound appended for agent-bound conversation")
	}
	if len(captured) != 1 || captured[0].Subject != "rig-a/helper" {
		t.Fatalf("events = %#v, want one inbound event with agent subject", captured)
	}
	payload, ok := captured[0].Payload.(InboundEventPayload)
	if !ok || payload.TargetAgent != "rig-a/helper" {
		t.Fatalf("payload = %#v, want TargetAgent rig-a/helper", captured[0].Payload)
	}
}

// agentTestResolver maps session selectors (agent names or concrete IDs) to
// concrete session IDs the way the API layer's session resolution does.
func agentTestResolver(mapping map[string]string) func(context.Context, string) (string, error) {
	return func(_ context.Context, selector string) (string, error) {
		if id, ok := mapping[selector]; ok {
			return id, nil
		}
		return "", fmt.Errorf("session not found: %q", selector)
	}
}

func TestHandleOutboundAgentBoundResolverAuthorizesMatchingSession(t *testing.T) {
	freezeTestClock(t)
	fabric, adapter, captured, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}
	deps.ResolveSessionSelector = agentTestResolver(map[string]string{
		"rig-a/helper": "sess-live-1",
		"sess-live-1":  "sess-live-1",
	})

	result, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-live-1",
		Conversation: ref,
		Text:         "reply from the woken session",
	})
	if err != nil {
		t.Fatalf("HandleOutbound: %v", err)
	}
	if !result.Receipt.Delivered {
		t.Fatalf("Receipt.Delivered = false, want true")
	}
	if len(adapter.publishs) != 1 {
		t.Fatalf("publishes = %d, want 1", len(adapter.publishs))
	}
	// Delivery-context recording is skipped on the agent path (like the
	// group-fallback path) — transcript remains the outbound record.
	if result.DeliveryContext != nil {
		t.Fatalf("DeliveryContext = %#v, want nil on agent path", result.DeliveryContext)
	}
	if result.TranscriptEntry == nil {
		t.Fatalf("TranscriptEntry = nil, want outbound appended")
	}
	var outboundEvents []capturedEvent
	for _, ev := range *captured {
		if ev.Type == "extmsg.outbound" {
			outboundEvents = append(outboundEvents, ev)
		}
	}
	if len(outboundEvents) != 1 || outboundEvents[0].Subject != "sess-live-1" {
		t.Fatalf("outbound events = %#v, want one with subject sess-live-1", outboundEvents)
	}
}

func TestHandleOutboundAgentBoundRejectsMismatchedSession(t *testing.T) {
	freezeTestClock(t)
	fabric, adapter, _, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}
	deps.ResolveSessionSelector = agentTestResolver(map[string]string{
		"rig-a/helper": "sess-live-1",
		"sess-other":   "sess-other",
	})

	if _, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-other",
		Conversation: ref,
		Text:         "imposter",
	}); err == nil {
		t.Fatalf("HandleOutbound(mismatch) = nil error, want rejection")
	}
	if len(adapter.publishs) != 0 {
		t.Fatalf("publishes = %d, want 0 after rejection", len(adapter.publishs))
	}
}

func TestHandleOutboundAgentBoundNoLiveSessionFails(t *testing.T) {
	freezeTestClock(t)
	fabric, adapter, _, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}
	deps.ResolveSessionSelector = agentTestResolver(map[string]string{
		"sess-other": "sess-other",
	})

	if _, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-other",
		Conversation: ref,
		Text:         "no live agent session",
	}); err == nil {
		t.Fatalf("HandleOutbound(no live agent session) = nil error, want failure")
	}
	if len(adapter.publishs) != 0 {
		t.Fatalf("publishes = %d, want 0", len(adapter.publishs))
	}
}

func TestHandleOutboundAgentBoundWithoutResolverFails(t *testing.T) {
	freezeTestClock(t)
	fabric, adapter, _, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}
	deps.ResolveSessionSelector = nil

	if _, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-live-1",
		Conversation: ref,
		Text:         "no resolver wired",
	}); err == nil {
		t.Fatalf("HandleOutbound(no resolver) = nil error, want failure")
	}
	if len(adapter.publishs) != 0 {
		t.Fatalf("publishes = %d, want 0", len(adapter.publishs))
	}
}
