package extmsg

import (
	"context"
	"errors"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// ambiguousCreateStore commits the first explicit-ID create to the backing
// store and then answers it with an error anyway — the "create committed but
// the caller saw a failure" case a dropped connection produces.
type ambiguousCreateStore struct {
	beads.Store
	armed atomic.Bool
}

func (s *ambiguousCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	if hasLabel(b, "gc:extmsg-transcript") && s.armed.CompareAndSwap(true, false) {
		if _, err := s.Store.Create(b); err != nil {
			return beads.Bead{}, err
		}
		return beads.Bead{}, errors.New("invalid connection")
	}
	return s.Store.Create(b)
}

// TestTranscriptAppendRecoversAmbiguousCreateWithoutProviderMessageID pins the
// hardest recovery case: the entry committed, the create answered an error,
// and the message has no provider message ID, so only this Append's evidence
// can prove the stored entry is ours. Append must read the entry back,
// publish it, and hand the caller the stored entry; the next message must get
// a fresh sequence.
func TestTranscriptAppendRecoversAmbiguousCreateWithoutProviderMessageID(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	store := &ambiguousCreateStore{Store: mem}
	store.armed.Store(true)
	svc := newTranscriptService(beadstest.Mail(store), newBindingLockPool())
	ref := testConversationRef()
	ctx := context.Background()

	entry, err := svc.Append(ctx, AppendTranscriptInput{
		Caller:       testAdapterCaller(),
		Conversation: ref,
		Kind:         TranscriptMessageInbound,
		Provenance:   TranscriptProvenanceLive,
		Text:         "no provider message id",
		CreatedAt:    testNow(),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if entry.Sequence != 1 {
		t.Fatalf("recovered entry sequence = %d, want 1", entry.Sequence)
	}
	if entry.Text != "no provider message id" {
		t.Fatalf("recovered entry text = %q", entry.Text)
	}

	state, err := svc.State(ctx, testControllerCaller(), ref)
	if err != nil || state == nil {
		t.Fatalf("State: %v, %v", state, err)
	}
	if entry.ID != fmt.Sprintf("%s-1", state.ID) {
		t.Fatalf("recovered entry id = %q, want the pinned id %q", entry.ID, fmt.Sprintf("%s-1", state.ID))
	}

	entries, err := mem.List(beads.ListQuery{Label: "gc:extmsg-transcript"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stored %d transcript entries, want 1", len(entries))
	}

	// The frontier was published past the recovered entry, so the next
	// message must not reuse sequence 1.
	next, err := svc.Append(ctx, AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-next",
		Text:              "second message",
		CreatedAt:         testNow(),
	})
	if err != nil {
		t.Fatalf("Append after recovery: %v", err)
	}
	if next.Sequence != 2 {
		t.Fatalf("next entry sequence = %d, want 2", next.Sequence)
	}
}

// TestTranscriptAppendRecoversAmbiguousCreateWithProviderMessageID is the
// sibling case: when the stored entry carries the same provider message ID,
// Append recovers it the same way and the replay is visible exactly once.
func TestTranscriptAppendRecoversAmbiguousCreateWithProviderMessageID(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	store := &ambiguousCreateStore{Store: mem}
	store.armed.Store(true)
	svc := newTranscriptService(beadstest.Mail(store), newBindingLockPool())
	ref := testConversationRef()
	ctx := context.Background()

	entry, err := svc.Append(ctx, AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-ambiguous",
		Text:              "ambiguous create",
		CreatedAt:         testNow(),
	})
	if err != nil {
		t.Fatalf("Append: %v", err)
	}
	if entry.Sequence != 1 {
		t.Fatalf("recovered entry sequence = %d, want 1", entry.Sequence)
	}
	if entry.ProviderMessageID != "msg-ambiguous" {
		t.Fatalf("recovered entry provider message id = %q", entry.ProviderMessageID)
	}

	entries, err := mem.List(beads.ListQuery{Label: "gc:extmsg-transcript"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("stored %d transcript entries, want 1", len(entries))
	}

	state, err := svc.State(ctx, testControllerCaller(), ref)
	if err != nil || state == nil {
		t.Fatalf("State: %v, %v", state, err)
	}
	if state.NextSequence != 2 {
		t.Fatalf("NextSequence = %d, want 2 after recovered append", state.NextSequence)
	}
	next, err := svc.Append(ctx, AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-after",
		Text:              "after ambiguous create",
		CreatedAt:         testNow(),
	})
	if err != nil {
		t.Fatalf("Append after recovery: %v", err)
	}
	if next.Sequence != 2 {
		t.Fatalf("next entry sequence = %d, want 2", next.Sequence)
	}
}
