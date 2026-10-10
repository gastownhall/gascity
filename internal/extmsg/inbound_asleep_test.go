package extmsg

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestInboundToAsleepRoomBoundSeatPersistsTranscript is the ga-tvs contract.
// bind-room attaches a Slack room to a named seat. When that seat is asleep,
// the inbound must still land as a transcript entry the seat can replay later
// — by its stable name and by its live session bead ID. Dropping the message
// because nothing is running makes the ping unrecoverable.
func TestInboundToAsleepRoomBoundSeatPersistsTranscript(t *testing.T) {
	freezeTestClock(t)
	store := beads.NewMemStore()
	fabric := NewServices(store)
	seatName := "gascity/quartz"
	liveID := makeSessionBead(t, store, seatName)
	if err := store.SetMetadata(liveID, "state", string(session.StateAsleep)); err != nil {
		t.Fatalf("mark session asleep: %v", err)
	}
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

	const text = "overseer ping while quartz is asleep"
	result, err := HandleInboundNormalized(context.Background(), InboundDeps{Services: fabric}, ExternalInboundMessage{
		ProviderMessageID: "1700000000.000100",
		Conversation:      ref,
		Actor:             ExternalActor{ID: "UOVERSEER", DisplayName: "overseer", IsBot: false},
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
