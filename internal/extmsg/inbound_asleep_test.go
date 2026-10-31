package extmsg

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestInboundToAsleepRoomBoundSeatPersistsTranscript pins the asleep-seat contract.
// bind-room attaches a Slack room to a named seat. When that seat is asleep,
// the inbound must still land as a transcript entry the seat can replay later
// — by its stable name and by its live session bead ID. Dropping the message
// because nothing is running makes the ping unrecoverable.
func TestInboundToAsleepRoomBoundSeatPersistsTranscript(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	seatName := "example-rig/seat-1"
	liveID := makeSessionBead(t, store, seatName)
	if err := store.SetMetadata(liveID, "state", string(session.StateAsleep)); err != nil {
		t.Fatalf("mark session asleep: %v", err)
	}
	ref := ConversationRef{
		ScopeID:        "city-1",
		Provider:       "slack",
		AccountID:      "acct-1",
		ConversationID: "room-1",
		Kind:           ConversationRoom,
	}
	group, err := fabric.Groups.EnsureGroup(context.Background(), testControllerCaller(), EnsureGroupInput{
		RootConversation: ref,
		Mode:             GroupModeLauncher,
		DefaultHandle:    "example-rig-seat-1",
	})
	if err != nil {
		t.Fatalf("EnsureGroup: %v", err)
	}
	if _, err := fabric.Groups.UpsertParticipant(context.Background(), testControllerCaller(), UpsertParticipantInput{
		GroupID:   group.ID,
		Handle:    "example-rig-seat-1",
		SessionID: seatName,
		Public:    true,
	}); err != nil {
		t.Fatalf("UpsertParticipant: %v", err)
	}

	const text = "ping while the bound seat is asleep"
	result, err := HandleInboundNormalized(context.Background(), InboundDeps{Services: fabric}, ExternalInboundMessage{
		ProviderMessageID: "1700000000.000100",
		Conversation:      ref,
		Actor:             ExternalActor{ID: "user-1", DisplayName: "User One", IsBot: false},
		Text:              text,
		ReceivedAt:        testNow(),
	})
	if err != nil {
		t.Fatalf("HandleInboundNormalized: %v", err)
	}
	if result == nil || result.TranscriptEntry == nil {
		t.Fatalf("inbound to asleep seat produced no transcript entry: %#v", result)
	}
	if result.TranscriptEntry.Text != text {
		t.Fatalf("transcript text = %q, want %q", result.TranscriptEntry.Text, text)
	}

	entries, err := fabric.Transcript.List(context.Background(), ListTranscriptInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		Limit:        20,
	})
	if err != nil {
		t.Fatalf("List transcript: %v", err)
	}
	if len(entries) != 1 || entries[0].Text != text {
		t.Fatalf("stored transcript = %#v, want one entry %q", entries, text)
	}

	for _, selector := range []string{seatName, liveID} {
		backfill, err := fabric.Transcript.ListBackfill(context.Background(), ListBackfillInput{
			Caller:       testControllerCaller(),
			Conversation: ref,
			SessionID:    selector,
			Limit:        20,
		})
		if err != nil {
			t.Fatalf("ListBackfill(%s): %v", selector, err)
		}
		if len(backfill) != 1 || backfill[0].Text != text {
			t.Fatalf("ListBackfill(%s) = %#v, want the inbound so a later session can replay it", selector, backfill)
		}
	}
}
