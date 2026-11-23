package extmsg

import (
	"context"
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
