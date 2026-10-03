package extmsg

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestListPublishableBySessionFindsRoomBoundSeatByNameAndLiveID is the ga-98z
// contract: bind-room registers group participants (often under a seat name
// like gascity/quartz) without a 1:1 extmsg binding. Publish discovery
// (GET /extmsg/bindings) must still return that room for both the seat name
// and the live session bead ID.
func TestListPublishableBySessionFindsRoomBoundSeatByNameAndLiveID(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	seatName := "gascity/quartz"
	liveID := makeSessionBead(t, store, seatName)
	ref := ConversationRef{
		ScopeID:        "boomfartville",
		Provider:       "slack",
		AccountID:      "T0TESTWS",
		ConversationID: "C0BQED3AQKG",
		Kind:           ConversationRoom,
	}

	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "gascity-quartz",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "gascity-quartz",
		SessionID: seatName,
		Public:    true,
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	exact, err := fabric.Bindings.ListBySession(context.Background(), seatName)
	if err != nil {
		t.Fatalf("ListBySession(seat): %v", err)
	}
	if len(exact) != 0 {
		t.Fatalf("ListBySession(seat) = %#v, want empty (group participants are not 1:1 bindings)", exact)
	}
	exactLive, err := fabric.Bindings.ListBySession(context.Background(), liveID)
	if err != nil {
		t.Fatalf("ListBySession(live): %v", err)
	}
	if len(exactLive) != 0 {
		t.Fatalf("ListBySession(live) = %#v, want empty", exactLive)
	}

	byName, err := fabric.Bindings.ListPublishableBySession(context.Background(), seatName)
	if err != nil {
		t.Fatalf("ListPublishableBySession(seat): %v", err)
	}
	if len(byName) != 1 {
		t.Fatalf("ListPublishableBySession(seat) len = %d, want 1; got %#v", len(byName), byName)
	}
	if byName[0].Conversation != ref {
		t.Fatalf("ListPublishableBySession(seat) conversation = %#v, want %#v", byName[0].Conversation, ref)
	}
	if byName[0].Status != BindingActive {
		t.Fatalf("ListPublishableBySession(seat) status = %q, want %q", byName[0].Status, BindingActive)
	}

	byLive, err := fabric.Bindings.ListPublishableBySession(context.Background(), liveID)
	if err != nil {
		t.Fatalf("ListPublishableBySession(live): %v", err)
	}
	if len(byLive) != 1 {
		t.Fatalf("ListPublishableBySession(live) len = %d, want 1; got %#v", len(byLive), byLive)
	}
	if byLive[0].Conversation != ref {
		t.Fatalf("ListPublishableBySession(live) conversation = %#v, want %#v", byLive[0].Conversation, ref)
	}
}

func TestListPublishableBySessionPrefersRealBindingOverGroupSynthetic(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	ref := testConversationRef()

	binding, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    "sess-a",
		Now:          testNow(),
	})
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
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
		SessionID: "sess-a",
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	got, err := fabric.Bindings.ListPublishableBySession(context.Background(), "sess-a")
	if err != nil {
		t.Fatalf("ListPublishableBySession: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListPublishableBySession len = %d, want 1 (no duplicate synthetic)", len(got))
	}
	if got[0].ID != binding.ID {
		t.Fatalf("ListPublishableBySession ID = %s, want real binding %s", got[0].ID, binding.ID)
	}
}

func TestListPublishableBySessionFindsOneToOneBindingBySessionName(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	seatName := "gascity/quartz"
	liveID := makeSessionBead(t, store, seatName)
	ref := testConversationRef()

	if _, err := fabric.Bindings.Bind(context.Background(), testControllerCaller(), BindInput{
		Conversation: ref,
		SessionID:    liveID,
		Now:          testNow(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	got, err := fabric.Bindings.ListPublishableBySession(context.Background(), seatName)
	if err != nil {
		t.Fatalf("ListPublishableBySession(seat): %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("ListPublishableBySession(seat) len = %d, want 1; got %#v", len(got), got)
	}
	if got[0].Conversation != ref {
		t.Fatalf("conversation = %#v, want %#v", got[0].Conversation, ref)
	}
}
