package extmsg

import (
	"context"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// delayedPublishStore parks the first transcript-state head bump, so a
// writer's entry is stored at its slot before the head advances — the
// delayed writer whose later entry a member's backfill cursor must not
// skip.
type delayedPublishStore struct {
	beads.Store
	parked  chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *delayedPublishStore) SetMetadataBatch(id string, metadata map[string]string) error {
	if _, ok := metadata["next_sequence"]; ok {
		s.once.Do(func() {
			s.parked <- struct{}{}
			<-s.release
		})
	}
	return s.Store.SetMetadataBatch(id, metadata)
}

// TestTranscriptBackfillCursorDoesNotSkipLateEntry covers two caching layers
// over one store with independent primed caches: a member reads and acks the
// head while a delayed writer's entry sits unpublished, and the next
// backfill must return that writer's later entry. The acknowledgment skipped
// no unseen lower entry, and a stale cached head must never hide a published
// sequence.
func TestTranscriptBackfillCursorDoesNotSkipLateEntry(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	writerBacking := beads.NewCachingStoreForTest(mem, nil)
	memberBacking := beads.NewCachingStoreForTest(mem, nil)
	writerCache := beadstest.Mail(writerBacking)
	memberCache := beadstest.Mail(memberBacking)
	svcW := NewServices(writerCache, beadstest.Sessions(writerCache)).Transcript
	svcM := NewServices(memberCache, beadstest.Sessions(memberCache)).Transcript
	// The seed handle has no cache, so the membership and state beads it
	// creates reach the member's cache only through the member's prime —
	// prime-loaded rows reflect the published head, not a stale local write.
	svcSeed := NewServices(beadstest.Mail(mem), beadstest.Sessions(mem)).Transcript
	ref := testConversationRef()
	controller := Caller{Kind: CallerController, ID: "controller-1"}

	appendMsg := func(svc TranscriptService, id string) error {
		_, err := svc.Append(context.Background(), AppendTranscriptInput{
			Caller:            testAdapterCaller(),
			Conversation:      ref,
			Kind:              TranscriptMessageInbound,
			Provenance:        TranscriptProvenanceLive,
			ProviderMessageID: id,
			Text:              id,
			CreatedAt:         testNow(),
		})
		return err
	}

	if _, err := svcSeed.EnsureMembership(context.Background(), EnsureMembershipInput{
		Caller:       controller,
		Conversation: ref,
		SessionID:    "sess-m",
	}); err != nil {
		t.Fatalf("EnsureMembership: %v", err)
	}

	// Two published entries; the member primes its cache and reads and acks
	// the head at sequence 2. Its cached state bead says the head is 3 until
	// something refreshes it.
	for _, id := range []string{"msg-0", "msg-a"} {
		if err := appendMsg(svcW, id); err != nil {
			t.Fatalf("Append(%s): %v", id, err)
		}
	}
	// Full prime: the member's cache is live-serving, so its state bead still
	// says the head is 3 when the delayed writer publishes sequence 3.
	if err := memberBacking.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	backfill, err := svcM.ListBackfill(context.Background(), ListBackfillInput{
		Caller:       controller,
		Conversation: ref,
		SessionID:    "sess-m",
	})
	if err != nil {
		t.Fatalf("ListBackfill: %v", err)
	}
	if len(backfill) != 2 {
		t.Fatalf("initial backfill = %d entries, want 2", len(backfill))
	}
	if err := svcM.Ack(context.Background(), AckMembershipInput{
		Caller:       controller,
		Conversation: ref,
		SessionID:    "sess-m",
		Sequence:     2,
	}); err != nil {
		t.Fatalf("Ack(2): %v", err)
	}

	// The delayed writer stores its entry at slot 3 while the head is still
	// 3 (published through 2), parked inside its publication.
	delayed := &delayedPublishStore{Store: writerCache, parked: make(chan struct{}), release: make(chan struct{})}
	svcB := NewServices(beadstest.Mail(delayed), beadstest.Sessions(delayed)).Transcript
	errCh := make(chan error, 1)
	go func() { errCh <- appendMsg(svcB, "msg-b") }()
	<-delayed.parked

	// The member reads again before the publication lands: nothing new is
	// published, and the acknowledgment of sequence 2 skipped nothing.
	early, err := svcM.ListBackfill(context.Background(), ListBackfillInput{
		Caller:       controller,
		Conversation: ref,
		SessionID:    "sess-m",
	})
	if err != nil {
		t.Fatalf("ListBackfill before publication: %v", err)
	}
	if len(early) != 0 {
		t.Fatalf("backfill before publication = %d entries, want 0", len(early))
	}

	// The publication lands; the member's next backfill must return the
	// late entry, even though its cached head still says 3.
	close(delayed.release)
	if err := <-errCh; err != nil {
		t.Fatalf("Append(msg-b) after publication resumed: %v", err)
	}
	late, err := svcM.ListBackfill(context.Background(), ListBackfillInput{
		Caller:       controller,
		Conversation: ref,
		SessionID:    "sess-m",
	})
	if err != nil {
		t.Fatalf("ListBackfill after publication: %v", err)
	}
	if len(late) != 1 || late[0].ProviderMessageID != "msg-b" || late[0].Sequence != 3 {
		t.Fatalf("backfill after publication = %+v, want msg-b at sequence 3", late)
	}
	if err := svcM.Ack(context.Background(), AckMembershipInput{
		Caller:       controller,
		Conversation: ref,
		SessionID:    "sess-m",
		Sequence:     3,
	}); err != nil {
		t.Fatalf("Ack(3): %v", err)
	}
	assertUniqueSequences(t, mem, map[string]string{"msg-0": "1", "msg-a": "2", "msg-b": "3"})
}
