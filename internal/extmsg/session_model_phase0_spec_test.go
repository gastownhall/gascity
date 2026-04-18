package extmsg

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// Phase 0 spec coverage from engdocs/design/session-model-unification.md:
// - External Bindings

func TestPhase0Bindings_HandleInboundNormalizedTargetsExactBoundSession(t *testing.T) {
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

	result, err := HandleInboundNormalized(context.Background(), InboundDeps{Services: fabric}, ExternalInboundMessage{
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
}

func TestPhase0Bindings_HandleInboundNormalizedWithoutBindingLeavesTargetEmpty(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	result, err := HandleInboundNormalized(context.Background(), InboundDeps{Services: fabric}, ExternalInboundMessage{
		Conversation: ref,
		Actor:        ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:         "hello",
		ReceivedAt:   testNow(),
	})
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result.TargetSessionID != "" {
		t.Fatalf("TargetSessionID = %q, want empty without binding", result.TargetSessionID)
	}
}
