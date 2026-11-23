package extmsg

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/events"
)

// TestHandleInboundNormalizedAmbiguousCreateKeepsSequencesDistinct covers the
// inbound pipeline over a transcript whose entry create commits and then
// reports a transport error: the bounded domain retry adopts the committed
// entry and publishes it, the delivery completes successfully with exactly
// one stored entry and one inbound event (the adapter's 2xx, not a lost
// message), and the next message allocates the sequence above it.
func TestHandleInboundNormalizedAmbiguousCreateKeepsSequencesDistinct(t *testing.T) {
	freezeTestClock(t)
	store := &ambiguousCreateStore{Store: beads.NewMemStore()}
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}

	inboundEvents := 0
	deps := InboundDeps{
		Services: fabric,
		EmitEvent: func(eventType, _ string, _ events.Payload) {
			if eventType == events.ExtMsgInbound {
				inboundEvents++
			}
		},
	}
	inbound := func(id, text string) (*InboundResult, error) {
		return HandleInboundNormalized(context.Background(), deps, ExternalInboundMessage{
			Conversation:      ref,
			ProviderMessageID: id,
			Actor:             ExternalActor{ID: "user-1", DisplayName: "User One"},
			Text:              text,
			ReceivedAt:        testNow(),
		})
	}

	// The first delivery's entry create commits and then errors: the retry
	// adopts and publishes that entry, and the request still completes.
	first, err := inbound("msg-1", "hello")
	if err != nil {
		t.Fatalf("HandleInboundNormalized(msg-1): %v", err)
	}
	if first == nil || first.TranscriptEntry == nil {
		t.Fatalf("msg-1 result = %+v, want a transcript entry", first)
	}
	if inboundEvents != 1 {
		t.Fatalf("inbound events after msg-1 = %d, want 1", inboundEvents)
	}

	// The next delivery allocates the sequence above it, distinct and ordered.
	second, err := inbound("msg-2", "again")
	if err != nil {
		t.Fatalf("HandleInboundNormalized(msg-2): %v", err)
	}
	if second == nil || second.TranscriptEntry == nil || second.TranscriptEntry.Sequence != 2 {
		t.Fatalf("msg-2 entry = %+v, want sequence 2", second)
	}
	if inboundEvents != 2 {
		t.Fatalf("inbound events after msg-2 = %d, want 2", inboundEvents)
	}
	assertUniqueSequences(t, store, map[string]string{"msg-1": "1", "msg-2": "2"})
}

// TestHandleOutboundTranscriptFaultRetriesRecordingWithoutResending covers
// the outbound recording over the same ambiguous create: the message is
// published to the adapter exactly once and the recording retry repairs the
// entry in the domain — the external send is never repeated.
func TestHandleOutboundTranscriptFaultRetriesRecordingWithoutResending(t *testing.T) {
	freezeTestClock(t)
	store := &ambiguousCreateStore{Store: beads.NewMemStore()}
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	ref := testConversationRef()
	adapter := newStubAdapter("stub", ref)
	reg := NewAdapterRegistry()
	reg.Register(AdapterKey{Provider: ref.Provider, AccountID: ref.AccountID}, adapter)
	deps := OutboundDeps{Services: fabric, Registry: reg}

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}
	deps.ResolveSessionSelector = agentTestResolver(map[string]string{"rig-a/helper": "sess-live-1"})

	result, err := HandleOutbound(context.Background(), deps, testControllerCaller(), OutboundRequest{
		Conversation: ref,
		Text:         "published once",
	})
	if err != nil {
		t.Fatalf("HandleOutbound: %v", err)
	}
	if len(adapter.publishs) != 1 {
		t.Fatalf("adapter publishes = %d, want exactly 1 (never resend)", len(adapter.publishs))
	}
	if result.TranscriptEntry == nil {
		t.Fatalf("TranscriptEntry = nil, want the recording retried after the ambiguous create")
	}
	assertUniqueSequences(t, store, map[string]string{"ext-msg-1": "1"})
}

// persistentBumpFaultStore fails every transcript-head advancement until the
// fault clears, like a store outage that outlasts the append's bounded
// recovery: the entry create commits, but nothing can publish it.
type persistentBumpFaultStore struct {
	beads.Store
	faulted bool
}

func (s *persistentBumpFaultStore) SetMetadataBatch(id string, metadata map[string]string) error {
	if s.faulted && metadata["next_sequence"] != "" {
		return errors.New("invalid connection")
	}
	return s.Store.SetMetadataBatch(id, metadata)
}

// TestHandleInboundPersistentFaultStaysRetryableWithoutNotification covers a
// publication fault that survives the append's bounded recovery and the
// inbound retry: the request fails with the contextual retryable fault before
// any event or notification, the committed entry stays stored, and once the
// fault clears a later retry publishes that entry without duplication.
func TestHandleInboundPersistentFaultStaysRetryableWithoutNotification(t *testing.T) {
	freezeTestClock(t)
	store := &persistentBumpFaultStore{Store: beads.NewMemStore(), faulted: true}
	fabric := NewServices(beadstest.Mail(store), beadstest.Sessions(store))
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		AgentName:    "rig-a/helper",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}

	inboundEvents := 0
	deps := InboundDeps{
		Services: fabric,
		EmitEvent: func(eventType, _ string, _ events.Payload) {
			if eventType == events.ExtMsgInbound {
				inboundEvents++
			}
		},
	}
	inbound := func(id, text string) (*InboundResult, error) {
		return HandleInboundNormalized(context.Background(), deps, ExternalInboundMessage{
			Conversation:      ref,
			ProviderMessageID: id,
			Actor:             ExternalActor{ID: "user-1", DisplayName: "User One"},
			Text:              text,
			ReceivedAt:        testNow(),
		})
	}

	// The fault outlasts the bounded recovery: the request fails with the
	// contextual retryable fault, before any event or notification.
	_, err := inbound("msg-1", "hello")
	if err == nil {
		t.Fatal("HandleInboundNormalized(msg-1) under a persistent publication fault succeeded, want a retryable error")
	}
	if !strings.Contains(err.Error(), "transcript append retryable fault") {
		t.Fatalf("error = %v, want the contextual transcript retryable fault", err)
	}
	if inboundEvents != 0 {
		t.Fatalf("inbound events = %d, want none while the entry is unpublished", inboundEvents)
	}

	// The entry committed before the publication stalled: fill-before-publish
	// leaves it stored, so a blind resend would mint a duplicate sequence.
	entries, err := store.List(beads.ListQuery{Label: labelTranscriptBase})
	if err != nil {
		t.Fatalf("List transcript entries: %v", err)
	}
	if len(entries) != 1 || entries[0].Metadata["provider_message_id"] != "msg-1" {
		t.Fatalf("stored entries = %+v, want the one committed msg-1 entry", entries)
	}

	// Once the fault clears, the retry publishes the stored entry: the same
	// provider message comes back exactly once, with no duplicate sequence.
	store.faulted = false
	second, err := inbound("msg-1", "hello")
	if err != nil {
		t.Fatalf("HandleInboundNormalized(msg-1) after the fault cleared: %v", err)
	}
	if second == nil || second.TranscriptEntry == nil || second.TranscriptEntry.Sequence != 1 {
		t.Fatalf("retried entry = %+v, want the stored msg-1 entry at sequence 1", second)
	}
	if inboundEvents != 1 {
		t.Fatalf("inbound events after retry = %d, want 1", inboundEvents)
	}
	assertUniqueSequences(t, store, map[string]string{"msg-1": "1"})
}
