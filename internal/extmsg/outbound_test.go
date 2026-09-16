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
	if adapter.publishs[0].SessionID != "sess-bound" {
		t.Fatalf("adapter publish SessionID = %q, want sess-bound", adapter.publishs[0].SessionID)
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

func TestHandleOutbound_ThreadsTranscriptSequenceToPublish(t *testing.T) {
	freezeTestClock(t)
	// Sequence threading is scoped to the llm-client (in-process SSE) path;
	// out-of-process adapters publish first so provider_message_id is available.
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()
	ref.Provider = ProviderLLMClient
	adapter := newStubAdapter("stub", ref)
	reg := NewAdapterRegistry()
	reg.Register(AdapterKey{Provider: ref.Provider, AccountID: ref.AccountID}, adapter)
	captured := make([]capturedEvent, 0)
	emit := func(eventType, subject string, payload events.Payload) {
		captured = append(captured, capturedEvent{Type: eventType, Subject: subject, Payload: payload})
	}
	deps := OutboundDeps{Services: fabric, Registry: reg, EmitEvent: emit}

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-bound",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	// Two replies on the same conversation: the live publish must carry the
	// monotonic transcript sequence assigned to each (1, then 2), not the
	// hardcoded 0 that made every reconnecting client replay the whole
	// transcript via Last-Event-ID: 0.
	for i, text := range []string{"first", "second"} {
		result, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
			SessionID:    "sess-bound",
			Conversation: ref,
			Text:         text,
		})
		if err != nil {
			t.Fatalf("HandleOutbound[%d]: %v", i, err)
		}
		wantSeq := int64(i + 1)
		if result.TranscriptEntry == nil {
			t.Fatalf("HandleOutbound[%d]: TranscriptEntry = nil, want appended", i)
		}
		if result.TranscriptEntry.Sequence != wantSeq {
			t.Fatalf("transcript sequence = %d, want %d", result.TranscriptEntry.Sequence, wantSeq)
		}
		if got := adapter.publishs[i].Sequence; got != wantSeq {
			t.Fatalf("published PublishRequest.Sequence = %d, want %d (must equal transcript sequence)", got, wantSeq)
		}
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
	// The rejection is no longer silent: a structured mismatch warning fires
	// so the cross-wire is observable (RCA gc-5aie6). No outbound event,
	// because nothing was published.
	if len(*captured) != 1 {
		t.Fatalf("captured events = %d, want 1 (mismatch warning) on rejection", len(*captured))
	}
	ev := (*captured)[0]
	if ev.Type != events.ExtMsgOutboundChannelMismatch {
		t.Fatalf("event type = %q, want %q", ev.Type, events.ExtMsgOutboundChannelMismatch)
	}
	if ev.Subject != "sess-other" {
		t.Fatalf("event subject = %q, want sess-other (the posting session)", ev.Subject)
	}
	payload, ok := ev.Payload.(OutboundChannelMismatchPayload)
	if !ok {
		t.Fatalf("event payload type = %T, want OutboundChannelMismatchPayload", ev.Payload)
	}
	if payload.PostingSession != "sess-other" {
		t.Fatalf("payload.PostingSession = %q, want sess-other", payload.PostingSession)
	}
	if payload.OwnerSession != "sess-owner" {
		t.Fatalf("payload.OwnerSession = %q, want sess-owner", payload.OwnerSession)
	}
	if payload.ConversationID != ref.ConversationID {
		t.Fatalf("payload.ConversationID = %q, want %q", payload.ConversationID, ref.ConversationID)
	}
}

// TestHandleOutbound_OwnBindingNoMismatchEvent confirms the mismatch warning
// is scoped to genuine cross-wires: a session publishing into the channel it
// owns emits only the normal outbound event, never a mismatch warning.
func TestHandleOutbound_OwnBindingNoMismatchEvent(t *testing.T) {
	freezeTestClock(t)
	fabric, _, captured, deps := newOutboundTestRig(t)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-owner",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	if _, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-owner",
		Conversation: ref,
		Text:         "hello",
	}); err != nil {
		t.Fatalf("HandleOutbound: %v", err)
	}
	for _, ev := range *captured {
		if ev.Type == events.ExtMsgOutboundChannelMismatch {
			t.Fatalf("unexpected mismatch warning on own-channel publish: %#v", ev)
		}
	}
}

// TestHandleOutbound_NonLLMClientRecordsProviderMessageID verifies that for
// out-of-process adapters (non-llm-client) the outbound transcript entry
// captures the receipt's provider_message_id, enabling inbound-echo dedup.
func TestHandleOutbound_NonLLMClientRecordsProviderMessageID(t *testing.T) {
	freezeTestClock(t)
	fabric, adapter, _, deps := newOutboundTestRig(t)
	ref := testConversationRef() // "discord" — not ProviderLLMClient

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-bound",
		Now:          testNow(),
	}); err != nil {
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
	if result.TranscriptEntry == nil {
		t.Fatalf("TranscriptEntry = nil, want appended")
	}
	if got, want := result.TranscriptEntry.ProviderMessageID, adapter.receipt.MessageID; got != want {
		t.Fatalf("ProviderMessageID = %q, want %q (receipt.MessageID)", got, want)
	}
}

// TestHandleOutbound_NonLLMClientPublishErrorLeavesNoEntry verifies that for
// out-of-process adapters a Publish error leaves no phantom outbound transcript
// entry (transcript is written after publish, only on success).
func TestHandleOutbound_NonLLMClientPublishErrorLeavesNoEntry(t *testing.T) {
	freezeTestClock(t)
	fabric, adapter, _, deps := newOutboundTestRig(t)
	ref := testConversationRef() // "discord" — not ProviderLLMClient
	adapter.err = errors.New("upstream unavailable")

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-bound",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	_, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		SessionID:    "sess-bound",
		Conversation: ref,
		Text:         "hello",
	})
	if err == nil {
		t.Fatalf("HandleOutbound error = nil, want publish error")
	}

	entries, listErr := fabric.Transcript.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
	})
	if listErr != nil {
		t.Fatalf("Transcript.List: %v", listErr)
	}
	if len(entries) != 0 {
		t.Fatalf("transcript entries = %d, want 0 (no entry on publish error)", len(entries))
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
