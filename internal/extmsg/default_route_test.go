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

func TestHandleInboundNormalizedDefaultRouteSkipsGroupedConversation(t *testing.T) {
	freezeTestClock(t)

	cases := []struct {
		// name describes the grouped routing miss being exercised.
		name string
		// defaultHandle seeds the group's default participant; empty means the
		// group has no routable default at all.
		defaultHandle string
		// message builds the inbound that misses group routing.
		message func(ref ConversationRef) ExternalInboundMessage
	}{
		{
			// A group exists with a routable default, but the message explicitly
			// targets a handle that is not a participant.
			name:          "explicit target miss",
			defaultHandle: "alpha",
			message: func(ref ConversationRef) ExternalInboundMessage {
				msg := defaultRouteTestMessage(ref)
				msg.ExplicitTarget = "ghost"
				return msg
			},
		},
		{
			// A group exists but has no default or last-addressed handle, so an
			// untargeted message routes to no participant.
			name:          "no routable default handle",
			defaultHandle: "",
			message:       defaultRouteTestMessage,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			fabric := NewServices(store)
			ref := testConversationRef()

			group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
				RootConversation: ref,
				Mode:             GroupModeLauncher,
				DefaultHandle:    tc.defaultHandle,
			})
			if err != nil {
				t.Fatalf("EnsureGroup: %v", err)
			}
			if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
				GroupID:   group.ID,
				Handle:    "alpha",
				SessionID: "sess-a",
			}); err != nil {
				t.Fatalf("UpsertParticipant(alpha): %v", err)
			}

			deps := InboundDeps{
				Services: fabric,
				DefaultAgentForConversation: func(ConversationRef) string {
					return "myrig/frontdesk"
				},
			}
			result, err := HandleInboundNormalized(context.Background(), deps, tc.message(ref))
			if err != nil {
				t.Fatalf("HandleInboundNormalized: %v", err)
			}

			// The conversation is grouped, so a routing miss leaves it unrouted
			// rather than handing it to the default agent.
			if result.TargetSessionID != "" || result.TargetAgentName != "" {
				t.Fatalf("result routed (%q/%q), want unrouted grouped miss", result.TargetSessionID, result.TargetAgentName)
			}
			if result.GroupRoute == nil || result.GroupRoute.Match != GroupRouteNoMatch {
				t.Fatalf("GroupRoute = %#v, want grouped no-match", result.GroupRoute)
			}

			// No sticky default-agent binding may exist: one would resolve before
			// group routing on every later message and permanently bypass the room.
			binding, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
			if err != nil {
				t.Fatalf("ResolveByConversation: %v", err)
			}
			if binding != nil {
				t.Fatalf("binding = %#v, want none (grouped conversation must not be default-routed)", binding)
			}
		})
	}
}

func TestGroupServiceResolveInboundDistinguishesNoGroupFromNoMatch(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewGroupService(store)
	ref := testConversationRef()

	// No group for the conversation: the distinct "no group" outcome that makes
	// the conversation eligible for a default route.
	noGroup, err := svc.ResolveInbound(context.Background(), ExternalInboundMessage{Conversation: ref})
	if err != nil {
		t.Fatalf("ResolveInbound(no group): %v", err)
	}
	if noGroup.Match != GroupRouteNoGroup {
		t.Fatalf("ResolveInbound(no group).Match = %q, want %q", noGroup.Match, GroupRouteNoGroup)
	}

	// A group exists but yields no routable target: a grouped no-match, which
	// stays distinct from "no group" so the default route cannot hijack the room.
	group, err := svc.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := svc.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant(alpha): %v", err)
	}
	noMatch, err := svc.ResolveInbound(context.Background(), ExternalInboundMessage{
		Conversation:   ref,
		ExplicitTarget: "ghost",
	})
	if err != nil {
		t.Fatalf("ResolveInbound(grouped miss): %v", err)
	}
	if noMatch.Match != GroupRouteNoMatch {
		t.Fatalf("ResolveInbound(grouped miss).Match = %q, want %q", noMatch.Match, GroupRouteNoMatch)
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
