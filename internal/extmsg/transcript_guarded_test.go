package extmsg

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// A real native/SQLite writer advertises its pinned namespace. This fixture
// exercises the same optional capability over the transactional MemStore.
type publicNamespacedStore struct{ *beads.MemStore }

func (*publicNamespacedStore) IDPrefix() string { return "gc" }

func TestGuardedPublicAndOutboundAppendsShareSequenceReservation(t *testing.T) {
	mem := &publicNamespacedStore{MemStore: publicTestStore()}
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	for n := range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if n%2 == 0 {
				input := publicTestInput()
				input.RequestKey = fmt.Sprintf("key-%d", n)
				input.Message.ProviderMessageID = fmt.Sprintf("inbound-%d", n)
				_, err := publicTestAdmission(t, mem).Admit(context.Background(), input)
				errs <- err
			} else {
				_, err := NewServices(mem).Transcript.Append(context.Background(), AppendTranscriptInput{Caller: testControllerCaller(), Conversation: testConversationRef(), Kind: TranscriptMessageOutbound, Text: "native reply", ProviderMessageID: fmt.Sprintf("outbound-%d", n)})
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	rows, err := NewServices(mem).Transcript.List(context.Background(), ListTranscriptInput{Caller: testControllerCaller(), Conversation: testConversationRef()})
	if err != nil || len(rows) != 16 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
	seen := make(map[int64]bool)
	for _, row := range rows {
		if seen[row.Sequence] {
			t.Fatalf("duplicate sequence %d", row.Sequence)
		}
		seen[row.Sequence] = true
	}
	for n := int64(1); n <= 16; n++ {
		if !seen[n] {
			t.Fatalf("missing sequence %d", n)
		}
	}
}

func TestGuardedOutboundAppendDeduplicatesAndRetainsPayloadGuard(t *testing.T) {
	mem := &publicNamespacedStore{MemStore: publicTestStore()}
	input := AppendTranscriptInput{Caller: testControllerCaller(), Conversation: testConversationRef(), Kind: TranscriptMessageInbound, Text: "first", ProviderMessageID: "inbound-one"}
	first, err := NewServices(mem).Transcript.Append(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	retry, err := NewServices(mem).Transcript.Append(context.Background(), input)
	if err != nil || retry.ID != first.ID {
		t.Fatalf("retry=%v err=%v", retry, err)
	}
	input.Text = "changed"
	if _, err := NewServices(mem).Transcript.Append(context.Background(), input); err == nil {
		t.Fatal("changed inbound payload accepted")
	}
}

type guardedInboundCollision struct {
	*publicNamespacedStore
	once sync.Once
}

func (s *guardedInboundCollision) GuardedBatcherHandle() (beads.GuardedBatcher, bool) { return s, true }

func (s *guardedInboundCollision) ApplyGuarded(ctx context.Context, b beads.GuardedBatch) (beads.GuardedResult, error) {
	winner := false
	s.once.Do(func() {
		winner = true
		copyItems := append([]beads.GuardedItem(nil), b.Items...)
		for i, item := range copyItems {
			if item.Create != nil && hasLabel(*item.Create, "gc:extmsg-transcript") {
				row := *item.Create
				row.Description = "winner payload"
				copyItems[i].Create = &row
			}
		}
		_, err := s.MemStore.ApplyGuarded(ctx, beads.GuardedBatch{Reason: b.Reason, Items: copyItems})
		if err != nil {
			panic(err)
		}
	})
	if winner {
		return beads.GuardedResult{}, beads.ErrAlreadyExists
	}
	return s.MemStore.ApplyGuarded(ctx, b)
}

func TestGuardedInboundAppendRejectsConflictAfterLostCreateRace(t *testing.T) {
	mem := &guardedInboundCollision{publicNamespacedStore: &publicNamespacedStore{MemStore: publicTestStore()}}
	input := AppendTranscriptInput{Caller: testControllerCaller(), Conversation: testConversationRef(), Kind: TranscriptMessageInbound, Text: "loser payload", ProviderMessageID: "race-message"}
	_, err := NewServices(mem).Transcript.Append(context.Background(), input)
	if !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("conflicting readback accepted: %v", err)
	}
	rows, err := NewServices(mem).Transcript.List(context.Background(), ListTranscriptInput{Caller: testControllerCaller(), Conversation: testConversationRef()})
	if err != nil || len(rows) != 1 || rows[0].Text != "winner payload" {
		t.Fatalf("rows=%+v err=%v", rows, err)
	}
}
