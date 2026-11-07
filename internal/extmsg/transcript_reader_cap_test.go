package extmsg

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// TestTranscriptListExcludesUncoveredEntryUntilPublication: an entry that was
// stored while its frontier advance failed shares its bucket with the last
// published entry, so listing must cap visibility at the frontier instead of
// leaking it. Once a replay publishes it, a member can read it and
// acknowledge it without skipping it.
func TestTranscriptListExcludesUncoveredEntryUntilPublication(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	store := newCoverFaultStore(mem)
	svc := newTranscriptService(beadstest.Mail(store), newBindingLockPool())
	ref := testConversationRef()
	ctx := context.Background()
	controller := testControllerCaller()

	if _, err := svc.EnsureMembership(ctx, EnsureMembershipInput{
		Caller:       controller,
		Conversation: ref,
		SessionID:    "sess-member",
	}); err != nil {
		t.Fatalf("EnsureMembership: %v", err)
	}

	first, err := svc.Append(ctx, AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-covered",
		Text:              "published",
		CreatedAt:         testNow(),
	})
	if err != nil {
		t.Fatalf("first Append: %v", err)
	}
	if first.Sequence != 1 {
		t.Fatalf("first entry sequence = %d, want 1", first.Sequence)
	}

	// The second entry stores but its advance fails: it sits in the same
	// bucket (entries 1..64) as the published sequence 1, unpublished.
	store.setFailAdvance(true)
	if _, err := svc.Append(ctx, AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-uncovered",
		Text:              "stored but unpublished",
		CreatedAt:         testNow(),
	}); err == nil {
		t.Fatal("second Append should fail while the advance faults")
	}

	listed, err := svc.List(ctx, ListTranscriptInput{Caller: controller, Conversation: ref})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Sequence != 1 {
		t.Fatalf("List = %+v, want only the published sequence 1", listed)
	}

	backfill, err := svc.ListBackfill(ctx, ListBackfillInput{Caller: controller, Conversation: ref, SessionID: "sess-member"})
	if err != nil {
		t.Fatalf("ListBackfill: %v", err)
	}
	if len(backfill) != 1 || backfill[0].Sequence != 1 {
		t.Fatalf("ListBackfill = %+v, want only the published sequence 1", backfill)
	}

	// A replay of the stored provider message publishes it...
	store.setFailAdvance(false)
	replayed, err := svc.Append(ctx, AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-uncovered",
		Text:              "stored but unpublished",
		CreatedAt:         testNow(),
	})
	if err != nil {
		t.Fatalf("replay Append: %v", err)
	}
	if replayed.Sequence != 2 {
		t.Fatalf("replayed entry sequence = %d, want 2", replayed.Sequence)
	}

	// ...so the member now reads it and can acknowledge it in order, without
	// skipping over sequence 2.
	afterBackfill, err := svc.ListBackfill(ctx, ListBackfillInput{Caller: controller, Conversation: ref, SessionID: "sess-member"})
	if err != nil {
		t.Fatalf("ListBackfill after publication: %v", err)
	}
	if len(afterBackfill) != 2 || afterBackfill[0].Sequence != 1 || afterBackfill[1].Sequence != 2 {
		t.Fatalf("ListBackfill after publication = %+v, want sequences [1 2]", afterBackfill)
	}
	if err := svc.Ack(ctx, AckMembershipInput{Caller: controller, Conversation: ref, SessionID: "sess-member", Sequence: 1}); err != nil {
		t.Fatalf("Ack(1): %v", err)
	}
	if err := svc.Ack(ctx, AckMembershipInput{Caller: controller, Conversation: ref, SessionID: "sess-member", Sequence: 2}); err != nil {
		t.Fatalf("Ack(2): %v", err)
	}
}
