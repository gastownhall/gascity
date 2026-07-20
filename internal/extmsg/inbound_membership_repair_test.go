package extmsg

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// These tests pin the hq-ar4 recurrence: a conversation whose binding is
// active but whose transcript membership record has been lost (closed by a
// cleanup race) must have the membership repaired by the next inbound.
// Delivery fans out over memberships, so without the repair every inbound is
// accepted, transcribed, and evented — then notified to nobody, forever.

func TestHandleInboundNormalizedRepairsMissingAgentBindingMembership(t *testing.T) {
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

	// Simulate the production state: the binding-owned membership vanishes
	// while the binding stays active.
	if err := fabric.Transcript.RemoveMembership(context.Background(), RemoveMembershipInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "rig-a/helper",
		Owner:        MembershipOwnerBinding,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("RemoveMembership: %v", err)
	}
	members, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 0 {
		t.Fatalf("memberships after removal = %#v, want none", members)
	}

	deps := InboundDeps{Services: fabric}
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

	members, err = fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships after inbound: %v", err)
	}
	if len(members) != 1 || members[0].SessionID != "rig-a/helper" {
		t.Fatalf("memberships after inbound = %#v, want one keyed rig-a/helper (membership repaired)", members)
	}
}

func TestHandleInboundNormalizedRepairsMissingSessionBindingMembership(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind(session): %v", err)
	}
	if err := fabric.Transcript.RemoveMembership(context.Background(), RemoveMembershipInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-a",
		Owner:        MembershipOwnerBinding,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("RemoveMembership: %v", err)
	}

	deps := InboundDeps{Services: fabric}
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

	members, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships after inbound: %v", err)
	}
	if len(members) != 1 || members[0].SessionID != "sess-a" {
		t.Fatalf("memberships after inbound = %#v, want one keyed sess-a (membership repaired)", members)
	}
}

// An intact membership must pass through the repair as a no-op: same single
// membership record, no owner or policy churn.
func TestHandleInboundNormalizedLeavesIntactMembershipAlone(t *testing.T) {
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
	before, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil || len(before) != 1 {
		t.Fatalf("ListMemberships before = %#v (%v), want one", before, err)
	}

	deps := InboundDeps{Services: fabric}
	if _, err := HandleInboundNormalized(context.Background(), deps, ExternalInboundMessage{
		Conversation: ref,
		Actor:        ExternalActor{ID: "user-1", DisplayName: "User One"},
		Text:         "hello",
		ReceivedAt:   testNow(),
	}); err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}

	after, err := fabric.Transcript.ListMemberships(context.Background(), testControllerCaller(), ref)
	if err != nil {
		t.Fatalf("ListMemberships after: %v", err)
	}
	if len(after) != 1 || after[0].ID != before[0].ID {
		t.Fatalf("memberships after inbound = %#v, want the original record %s untouched", after, before[0].ID)
	}
}
