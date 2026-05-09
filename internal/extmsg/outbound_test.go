package extmsg

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// stubAdapter is a TransportAdapter that records Publish calls and returns a
// fixed receipt. It exists only for outbound_test.go; widen as needed.
type stubAdapter struct {
	mu       sync.Mutex
	name     string
	receipt  PublishReceipt
	publishs []PublishRequest
	err      error
}

func newStubAdapter(name string, ref ConversationRef) *stubAdapter {
	return &stubAdapter{
		name: name,
		receipt: PublishReceipt{
			MessageID:    "ext-msg-1",
			Conversation: ref,
			Delivered:    true,
		},
	}
}

func (a *stubAdapter) Name() string { return a.name }
func (a *stubAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{}
}

func (a *stubAdapter) VerifyAndNormalizeInbound(_ context.Context, _ InboundPayload) (*ExternalInboundMessage, error) {
	return nil, errors.New("stubAdapter does not implement inbound")
}

func (a *stubAdapter) Publish(_ context.Context, req PublishRequest) (*PublishReceipt, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.err != nil {
		return nil, a.err
	}
	a.publishs = append(a.publishs, req)
	receipt := a.receipt
	receipt.Conversation = req.Conversation
	return &receipt, nil
}

func (a *stubAdapter) EnsureChildConversation(_ context.Context, _ ConversationRef, _ string) (*ConversationRef, error) {
	return nil, errors.New("stubAdapter does not implement child conversations")
}

type capturedEvent struct {
	Type    string
	Subject string
	Payload events.Payload
}

func newOutboundTestRig(t *testing.T) (Services, *stubAdapter, *[]capturedEvent, OutboundDeps) {
	t.Helper()
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()
	adapter := newStubAdapter("stub", ref)
	reg := NewAdapterRegistry()
	reg.Register(AdapterKey{Provider: ref.Provider, AccountID: ref.AccountID}, adapter)
	captured := make([]capturedEvent, 0)
	emit := func(eventType, subject string, payload events.Payload) {
		captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
	}
	deps := OutboundDeps{Services: fabric, Registry: reg, EmitEvent: emit}
	return fabric, adapter, &captured, deps
}

func TestHandleOutbound_BindingPathUnchanged(t *testing.T) {
	freezeTestClock(t)
	fabric, adapter, captured, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	binding, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-bound",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}

	result, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-bound",
		Conversation: ref,
		Text:         "hello",
	})
	if err != nil {
		t.Fatalf("HandleOutbound: %v", err)
	}
	if !result.Receipt.Delivered {
		t.Fatalf("Receipt.Delivered = false, want true")
	}
	if len(adapter.publishs) != 1 {
		t.Fatalf("adapter publishes = %d, want 1", len(adapter.publishs))
	}
	if result.DeliveryContext == nil {
		t.Fatalf("DeliveryContext = nil, want recorded")
	}
	if result.DeliveryContext.SessionID != "sess-bound" {
		t.Fatalf("DeliveryContext.SessionID = %q, want sess-bound", result.DeliveryContext.SessionID)
	}
	if result.DeliveryContext.BindingGeneration != binding.BindingGeneration {
		t.Fatalf("DeliveryContext.BindingGeneration = %d, want %d",
			result.DeliveryContext.BindingGeneration, binding.BindingGeneration)
	}
	if len(*captured) != 1 {
		t.Fatalf("captured events = %d, want 1", len(*captured))
	}
	if (*captured)[0].Type != events.ExtMsgOutbound {
		t.Fatalf("event type = %q, want %q", (*captured)[0].Type, events.ExtMsgOutbound)
	}
	if (*captured)[0].Subject != "sess-bound" {
		t.Fatalf("event subject = %q, want sess-bound", (*captured)[0].Subject)
	}
}

func TestHandleOutbound_RoomBoundParticipantPublishes(t *testing.T) {
	freezeTestClock(t)
	fabric, adapter, _, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-room",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	result, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-room",
		Conversation: ref,
		Text:         "hello",
	})
	if err != nil {
		t.Fatalf("HandleOutbound: %v", err)
	}
	if !result.Receipt.Delivered {
		t.Fatalf("Receipt.Delivered = false, want true")
	}
	if len(adapter.publishs) != 1 {
		t.Fatalf("adapter publishes = %d, want 1", len(adapter.publishs))
	}
	// Delivery context is binding-only; group fallback path skips it.
	if result.DeliveryContext != nil {
		t.Fatalf("DeliveryContext = %#v, want nil on group path", result.DeliveryContext)
	}
	// Transcript append remains the authoritative outbound record on the group path.
	if result.TranscriptEntry == nil {
		t.Fatalf("TranscriptEntry = nil, want appended")
	}
	if result.TranscriptEntry.SourceSessionID != "sess-room" {
		t.Fatalf("TranscriptEntry.SourceSessionID = %q, want sess-room", result.TranscriptEntry.SourceSessionID)
	}
}

func TestHandleOutbound_RoomBoundNonParticipantRejected(t *testing.T) {
	freezeTestClock(t)
	fabric, adapter, captured, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-room",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	_, err = HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-stranger",
		Conversation: ref,
		Text:         "hello",
	})
	if err == nil {
		t.Fatalf("HandleOutbound(non-participant) error = nil, want rejection")
	}
	// Preserve historical error contract used by API tier (422) and external callers.
	if !strings.Contains(err.Error(), "no active binding for conversation") {
		t.Fatalf("HandleOutbound(non-participant) error = %v, want 'no active binding for conversation' substring", err)
	}
	if len(adapter.publishs) != 0 {
		t.Fatalf("adapter publishes = %d, want 0 on rejection", len(adapter.publishs))
	}
	if len(*captured) != 0 {
		t.Fatalf("captured events = %d, want 0 on rejection", len(*captured))
	}
}

func TestHandleOutbound_NoBindingNoGroupReturnsActiveBindingError(t *testing.T) {
	freezeTestClock(t)
	_, adapter, captured, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	_, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-x",
		Conversation: ref,
		Text:         "hello",
	})
	if err == nil {
		t.Fatalf("HandleOutbound error = nil, want 'no active binding'")
	}
	if !strings.Contains(err.Error(), "no active binding for conversation") {
		t.Fatalf("HandleOutbound error = %v, want 'no active binding for conversation' substring", err)
	}
	if len(adapter.publishs) != 0 {
		t.Fatalf("adapter publishes = %d, want 0", len(adapter.publishs))
	}
	if len(*captured) != 0 {
		t.Fatalf("captured events = %d, want 0", len(*captured))
	}
}

func TestHandleOutbound_NoBindingEmptySessionReturnsActiveBindingError(t *testing.T) {
	freezeTestClock(t)
	_, adapter, captured, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	_, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "",
		Conversation: ref,
		Text:         "hello",
	})
	if err == nil {
		t.Fatalf("HandleOutbound error = nil, want 'no active binding'")
	}
	if !strings.Contains(err.Error(), "no active binding for conversation") {
		t.Fatalf("HandleOutbound error = %v, want 'no active binding for conversation' substring", err)
	}
	if len(adapter.publishs) != 0 {
		t.Fatalf("adapter publishes = %d, want 0", len(adapter.publishs))
	}
	if len(*captured) != 0 {
		t.Fatalf("captured events = %d, want 0", len(*captured))
	}
}

func TestHandleOutbound_GroupRouteEmitsEventOnPublishingSession(t *testing.T) {
	freezeTestClock(t)
	fabric, _, captured, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "alpha",
		SessionID: "sess-room",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	if _, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-room",
		Conversation: ref,
		Text:         "hello",
	}); err != nil {
		t.Fatalf("HandleOutbound: %v", err)
	}
	if len(*captured) != 1 {
		t.Fatalf("captured events = %d, want 1", len(*captured))
	}
	if (*captured)[0].Type != events.ExtMsgOutbound {
		t.Fatalf("event type = %q, want %q", (*captured)[0].Type, events.ExtMsgOutbound)
	}
	// Subject is the publishing session (the participant), not a binding session.
	if (*captured)[0].Subject != "sess-room" {
		t.Fatalf("event subject = %q, want sess-room (publishing session)", (*captured)[0].Subject)
	}
}

func TestHandleOutbound_BindingMismatchRejected(t *testing.T) {
	freezeTestClock(t)
	fabric, adapter, captured, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-owner",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	_, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-other",
		Conversation: ref,
		Text:         "hello",
	})
	if err == nil {
		t.Fatalf("HandleOutbound(mismatched session) error = nil, want rejection")
	}
	if !strings.Contains(err.Error(), "does not own binding") {
		t.Fatalf("HandleOutbound(mismatched session) error = %v, want 'does not own binding' substring", err)
	}
	if len(adapter.publishs) != 0 {
		t.Fatalf("adapter publishes = %d, want 0 on rejection", len(adapter.publishs))
	}
	if len(*captured) != 0 {
		t.Fatalf("captured events = %d, want 0 on rejection", len(*captured))
	}
}

// --- ResolveOutbound unit tests ---

func TestResolveOutbound_NoGroup(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewGroupService(store)
	ref := testConversationRef()

	decision, err := svc.ResolveOutbound(context.Background(), ref, "sess-x")
	if err != nil {
		t.Fatalf("ResolveOutbound: %v", err)
	}
	if decision == nil {
		t.Fatal("ResolveOutbound = nil, want NoMatch decision")
	}
	if decision.Match != GroupRouteNoMatch {
		t.Fatalf("decision.Match = %q, want %q", decision.Match, GroupRouteNoMatch)
	}
}

func TestResolveOutbound_ParticipantMatch(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewGroupService(store)
	ref := testConversationRef()

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
		t.Fatalf("UpsertParticipant: %v", err)
	}

	decision, err := svc.ResolveOutbound(context.Background(), ref, "sess-a")
	if err != nil {
		t.Fatalf("ResolveOutbound: %v", err)
	}
	if decision == nil {
		t.Fatal("ResolveOutbound = nil, want match decision")
	}
	if decision.Match != GroupRouteParticipantMatch {
		t.Fatalf("decision.Match = %q, want %q", decision.Match, GroupRouteParticipantMatch)
	}
	if decision.GroupID != group.ID {
		t.Fatalf("decision.GroupID = %q, want %q", decision.GroupID, group.ID)
	}
	if decision.Participant.SessionID != "sess-a" {
		t.Fatalf("decision.Participant.SessionID = %q, want sess-a", decision.Participant.SessionID)
	}
	if decision.Participant.Handle != "alpha" {
		t.Fatalf("decision.Participant.Handle = %q, want alpha", decision.Participant.Handle)
	}
}

func TestResolveOutbound_ParticipantMiss(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewGroupService(store)
	ref := testConversationRef()

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
		t.Fatalf("UpsertParticipant: %v", err)
	}

	decision, err := svc.ResolveOutbound(context.Background(), ref, "sess-stranger")
	if err != nil {
		t.Fatalf("ResolveOutbound: %v", err)
	}
	if decision == nil {
		t.Fatal("ResolveOutbound = nil, want NoMatch decision")
	}
	if decision.Match != GroupRouteNoMatch {
		t.Fatalf("decision.Match = %q, want %q", decision.Match, GroupRouteNoMatch)
	}
	if decision.GroupID != group.ID {
		t.Fatalf("decision.GroupID = %q, want %q (group exists, only participant missing)", decision.GroupID, group.ID)
	}
}

func TestResolveOutbound_EmptySessionRejected(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	svc := NewGroupService(store)
	ref := testConversationRef()

	cases := []string{"", "   ", "\t"}
	for _, sessionID := range cases {
		t.Run("session_"+sessionID, func(t *testing.T) {
			_, err := svc.ResolveOutbound(context.Background(), ref, sessionID)
			if err == nil {
				t.Fatalf("ResolveOutbound(%q) error = nil, want ErrInvalidInput", sessionID)
			}
			if !errors.Is(err, ErrInvalidInput) {
				t.Fatalf("ResolveOutbound(%q) error = %v, want ErrInvalidInput", sessionID, err)
			}
		})
	}
}
