package extmsg

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/events"
)

// addressedTestResolver maps addressed-session selectors to session IDs and
// reports every selector it was asked to resolve.
func addressedTestResolver(mapping map[string]string, asked *[]string) func(context.Context, string) (string, error) {
	return func(_ context.Context, selector string) (string, error) {
		*asked = append(*asked, selector)
		return mapping[selector], nil
	}
}

func addressedTestMessage(ref ConversationRef, addressed string) ExternalInboundMessage {
	msg := defaultRouteTestMessage(ref)
	msg.ProviderMessageID = "msg-1"
	msg.AddressedSession = addressed
	return msg
}

// inboundEventPayloads returns the extmsg.inbound payloads among events, with
// the subject each was recorded under.
func inboundEventPayloads(t *testing.T, captured []capturedEvent) ([]string, []InboundEventPayload) {
	t.Helper()
	var subjects []string
	var payloads []InboundEventPayload
	for _, ev := range captured {
		if ev.Type != events.ExtMsgInbound {
			continue
		}
		payload, ok := ev.Payload.(InboundEventPayload)
		if !ok {
			t.Fatalf("extmsg.inbound payload = %#v, want InboundEventPayload", ev.Payload)
		}
		subjects = append(subjects, ev.Subject)
		payloads = append(payloads, payload)
	}
	return subjects, payloads
}

// An unbound, ungrouped conversation with no default route is unroutable, yet
// an inbound its adapter addressed to a session is routed to that session: it
// is recorded in the transcript and announced by an extmsg.inbound event that
// names the session, and no binding is created for it.
func TestHandleInboundNormalizedAddressedSessionRoutesUnboundConversation(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	ref := testConversationRef()

	var asked []string
	var captured []capturedEvent
	deps := InboundDeps{
		Services:                fabric,
		ResolveAddressedSession: addressedTestResolver(map[string]string{"myrig/asker": "sess-w"}, &asked),
		EmitEvent: func(eventType, subject string, payload events.Payload) {
			captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
		},
	}
	result, err := HandleInboundNormalized(context.Background(), deps, addressedTestMessage(ref, "myrig/asker"))
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if len(asked) != 1 || asked[0] != "myrig/asker" {
		t.Fatalf("resolver asked %q, want exactly [myrig/asker]", asked)
	}
	if result.AddressedSessionID != "sess-w" {
		t.Fatalf("AddressedSessionID = %q, want sess-w", result.AddressedSessionID)
	}
	if result.TargetSessionID != "" || result.TargetAgentName != "" {
		t.Fatalf("result target = %q/%q, want no binding or route target", result.TargetSessionID, result.TargetAgentName)
	}
	if result.TranscriptEntry == nil {
		t.Fatal("TranscriptEntry = nil, want the addressed inbound recorded")
	}
	subjects, payloads := inboundEventPayloads(t, captured)
	if len(payloads) != 1 || subjects[0] != "sess-w" || payloads[0].AddressedSession != "sess-w" {
		t.Fatalf("inbound events = %q %#v, want one with subject and addressed_session sess-w", subjects, payloads)
	}
	binding, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if binding != nil {
		t.Fatalf("binding = %#v, want none: an addressed session reads the message, it does not own the conversation", binding)
	}
}

// The addressed session adds a recipient; it never displaces the binding,
// whose session stays the routed target and the event's subject.
func TestHandleInboundNormalizedAddressedSessionKeepsBoundTarget(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	ref := testConversationRef()
	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	var asked []string
	var captured []capturedEvent
	deps := InboundDeps{
		Services:                fabric,
		ResolveAddressedSession: addressedTestResolver(map[string]string{"sess-w": "sess-w"}, &asked),
		EmitEvent: func(eventType, subject string, payload events.Payload) {
			captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
		},
	}
	result, err := HandleInboundNormalized(context.Background(), deps, addressedTestMessage(ref, "sess-w"))
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetSessionID != "sess-a" || result.AddressedSessionID != "sess-w" {
		t.Fatalf("result = target %q addressed %q, want target sess-a addressed sess-w", result.TargetSessionID, result.AddressedSessionID)
	}
	subjects, payloads := inboundEventPayloads(t, captured)
	if len(payloads) != 1 || subjects[0] != "sess-a" || payloads[0].TargetSession != "sess-a" || payloads[0].AddressedSession != "sess-w" {
		t.Fatalf("inbound events = %q %#v, want one for sess-a addressing sess-w", subjects, payloads)
	}
}

// The default route still binds an unbound conversation to its agent when the
// inbound is addressed: the address is resolved after routing and adds the
// addressed session beside the default agent.
func TestHandleInboundNormalizedAddressedSessionLeavesDefaultRouteInPlace(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	ref := testConversationRef()

	var asked []string
	deps := InboundDeps{
		Services:                    fabric,
		DefaultAgentForConversation: func(ConversationRef) string { return "myrig/frontdesk" },
		ResolveAddressedSession:     addressedTestResolver(map[string]string{"sess-w": "sess-w"}, &asked),
	}
	result, err := HandleInboundNormalized(context.Background(), deps, addressedTestMessage(ref, "sess-w"))
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetAgentName != "myrig/frontdesk" || result.AddressedSessionID != "sess-w" {
		t.Fatalf("result = agent %q addressed %q, want agent myrig/frontdesk addressed sess-w", result.TargetAgentName, result.AddressedSessionID)
	}
	binding, err := fabric.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if binding == nil || binding.AgentName != "myrig/frontdesk" {
		t.Fatalf("binding = %#v, want the default route's agent binding", binding)
	}
}

// In a conversation that has a group, the group decides who hears the
// message, whether the group or a binding routes it: the address is never
// resolved, so it cannot reach a session outside the group.
func TestHandleInboundNormalizedAddressedSessionUnusedInGroupedConversation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		bindSession string
		bindAgent   string
		wantSession string
		wantAgent   string
	}{
		{name: "group routes it", wantSession: "sess-a"},
		{name: "session binding routes it", bindSession: "sess-b", wantSession: "sess-b"},
		{name: "agent binding routes it", bindAgent: "myrig/frontdesk", wantAgent: "myrig/frontdesk"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freezeTestClock(t)
			store := beads.NewMemStore()
			fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
			ref := testConversationRef()
			group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
				RootConversation: ref,
				Mode:             GroupModeLauncher,
				DefaultHandle:    "alpha",
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
			if tc.bindSession != "" || tc.bindAgent != "" {
				if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
					Conversation: ref,
					SessionID:    tc.bindSession,
					AgentName:    tc.bindAgent,
					Now:          testNow(),
				}); err != nil {
					t.Fatalf("Bind: %v", err)
				}
			}

			var asked []string
			var captured []capturedEvent
			deps := InboundDeps{
				Services:                fabric,
				ResolveAddressedSession: addressedTestResolver(map[string]string{"sess-w": "sess-w"}, &asked),
				EmitEvent: func(eventType, subject string, payload events.Payload) {
					captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
				},
			}
			result, err := HandleInboundNormalized(context.Background(), deps, addressedTestMessage(ref, "sess-w"))
			if err != nil {
				t.Fatalf("HandleInboundNormalized: %v", err)
			}
			if len(asked) != 0 {
				t.Fatalf("resolver asked %q in a grouped conversation, want no resolution", asked)
			}
			if result.AddressedSessionID != "" || result.TargetSessionID != tc.wantSession || result.TargetAgentName != tc.wantAgent {
				t.Fatalf("result = target %q/%q addressed %q, want target %q/%q and no addressed session", result.TargetSessionID, result.TargetAgentName, result.AddressedSessionID, tc.wantSession, tc.wantAgent)
			}
			_, payloads := inboundEventPayloads(t, captured)
			if len(payloads) != 1 || payloads[0].AddressedSession != "" {
				t.Fatalf("inbound events = %#v, want one that names no addressed session", payloads)
			}
		})
	}
}

// A group lookup that fails for a bound conversation fails an addressed
// inbound before anything is recorded: whether a group excludes the address
// is unknown, so it is neither resolved nor dropped.
func TestHandleInboundNormalizedAddressedSessionGroupLookupErrorFailsBoundInbound(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	ref := testConversationRef()
	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-b",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	lookupErr := errors.New("groups store unavailable")
	fabric.Groups = failingGroupLookup{GroupService: fabric.Groups, err: lookupErr}

	var asked []string
	var captured []capturedEvent
	deps := InboundDeps{
		Services:                fabric,
		ResolveAddressedSession: addressedTestResolver(map[string]string{"sess-w": "sess-w"}, &asked),
		EmitEvent: func(eventType, subject string, payload events.Payload) {
			captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
		},
	}
	_, err := HandleInboundNormalized(context.Background(), deps, addressedTestMessage(ref, "sess-w"))
	if !errors.Is(err, lookupErr) {
		t.Fatalf("HandleInboundNormalized error = %v, want it to wrap %v", err, lookupErr)
	}
	if len(asked) != 0 {
		t.Fatalf("resolver asked %q with the group unknown, want no resolution", asked)
	}
	entries, listErr := fabric.Transcript.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
	})
	if listErr != nil {
		t.Fatalf("Transcript.List: %v", listErr)
	}
	if len(entries) != 0 || len(captured) != 0 {
		t.Fatalf("transcript=%#v events=%#v, want nothing recorded for a failed inbound", entries, captured)
	}
}

// failingGroupLookup fails every group lookup by conversation with err.
type failingGroupLookup struct {
	GroupService
	err error
}

func (f failingGroupLookup) FindByConversation(context.Context, Caller, ConversationRef) (*ConversationGroupRecord, error) {
	return nil, f.err
}

// ExplicitTarget is a sender-controlled hint: it is never resolved as an
// address, so an unbound, ungrouped inbound that names a session only there
// stays unrouted and unrecorded.
func TestHandleInboundNormalizedExplicitTargetIsNeverAnAddress(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	ref := testConversationRef()

	var asked []string
	var captured []capturedEvent
	deps := InboundDeps{
		Services:                fabric,
		ResolveAddressedSession: addressedTestResolver(map[string]string{"sess-w": "sess-w"}, &asked),
		EmitEvent: func(eventType, subject string, payload events.Payload) {
			captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
		},
	}
	msg := addressedTestMessage(ref, "")
	msg.ExplicitTarget = "sess-w"
	result, err := HandleInboundNormalized(context.Background(), deps, msg)
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if len(asked) != 0 {
		t.Fatalf("resolver asked %q for an explicit target, want no resolution", asked)
	}
	if result.routed() || result.TranscriptEntry != nil || len(captured) != 0 {
		t.Fatalf("result = %#v events=%#v, want an unrouted, unrecorded inbound", result, captured)
	}
}

// An address that names no session (unknown, or closed) addresses no one, so
// an otherwise unroutable inbound stays unrouted and unrecorded.
func TestHandleInboundNormalizedAddressedSessionNamingNoSessionStaysUnrouted(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	ref := testConversationRef()

	var asked []string
	var captured []capturedEvent
	deps := InboundDeps{
		Services:                fabric,
		ResolveAddressedSession: addressedTestResolver(map[string]string{}, &asked),
		EmitEvent: func(eventType, subject string, payload events.Payload) {
			captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
		},
	}
	result, err := HandleInboundNormalized(context.Background(), deps, addressedTestMessage(ref, "nobody"))
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if len(asked) != 1 {
		t.Fatalf("resolver asked %q, want [nobody]", asked)
	}
	if result.routed() || result.TranscriptEntry != nil || len(captured) != 0 {
		t.Fatalf("result = %#v events=%#v, want an unrouted, unrecorded inbound", result, captured)
	}
}

// A resolver failure fails the inbound before anything is recorded, keeping
// its error class: invalid input stays invalid input (a permanent 4xx at the
// API), anything else stays retryable.
func TestHandleInboundNormalizedAddressedSessionResolverErrorFailsInbound(t *testing.T) {
	for _, tc := range []struct {
		name        string
		err         error
		wantInvalid bool
	}{
		{name: "ambiguous address", err: fmt.Errorf("%w: %q matches 2 sessions", ErrInvalidInput, "dup"), wantInvalid: true},
		{name: "store fault", err: errors.New("sessions store unavailable")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			freezeTestClock(t)
			store := beads.NewMemStore()
			fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
			ref := testConversationRef()

			var captured []capturedEvent
			deps := InboundDeps{
				Services: fabric,
				ResolveAddressedSession: func(context.Context, string) (string, error) {
					return "", tc.err
				},
				EmitEvent: func(eventType, subject string, payload events.Payload) {
					captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
				},
			}
			_, err := HandleInboundNormalized(context.Background(), deps, addressedTestMessage(ref, "dup"))
			if !errors.Is(err, tc.err) {
				t.Fatalf("HandleInboundNormalized error = %v, want it to wrap %v", err, tc.err)
			}
			if got := errors.Is(err, ErrInvalidInput); got != tc.wantInvalid {
				t.Fatalf("errors.Is(err, ErrInvalidInput) = %v, want %v", got, tc.wantInvalid)
			}
			entries, listErr := fabric.Transcript.List(context.Background(), ListTranscriptInput{
				Caller:       testControllerCaller(),
				Conversation: ref,
			})
			if listErr != nil {
				t.Fatalf("Transcript.List: %v", listErr)
			}
			if len(entries) != 0 || len(captured) != 0 {
				t.Fatalf("transcript=%#v events=%#v, want nothing recorded for a failed inbound", entries, captured)
			}
		})
	}
}
