package extmsg

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// coverFaultStore stores entries but fails every next_sequence advance — CAS
// and batched write alike — while its flag is set, modeling a fault that
// clears between deliveries. It forwards the metadata CAS itself so the
// frontier path stays on the CAS arm, the way production stores run.
type coverFaultStore struct {
	beads.Store
	cas beads.MetadataCASWriter

	mu      sync.Mutex
	fail    bool
	creates int
}

func newCoverFaultStore(inner *beads.MemStore) *coverFaultStore {
	cas, ok := beads.MetadataCASWriterFor(inner)
	if !ok {
		panic("inner store must offer the metadata CAS")
	}
	return &coverFaultStore{Store: inner, cas: cas}
}

func (s *coverFaultStore) setFailAdvance(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

func (s *coverFaultStore) failing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fail
}

func (s *coverFaultStore) transcriptCreates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates
}

func (s *coverFaultStore) Create(b beads.Bead) (beads.Bead, error) {
	if hasLabel(b, "gc:extmsg-transcript") {
		s.mu.Lock()
		s.creates++
		s.mu.Unlock()
	}
	return s.Store.Create(b)
}

func (s *coverFaultStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if key == "next_sequence" && s.failing() {
		return false, errors.New("i/o timeout")
	}
	return s.cas.CompareAndSetMetadataKey(id, key, expected, next)
}

func (s *coverFaultStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs["next_sequence"]; ok && s.failing() {
		return errors.New("i/o timeout")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// TestTranscriptAppendReplayReturnsStoredEntryAndAdvancesFrontier: when an
// entry was stored but its frontier advance failed, redelivering the same
// provider message must return the stored entry without creating another one,
// and must publish it by advancing the frontier past it.
func TestTranscriptAppendReplayReturnsStoredEntryAndAdvancesFrontier(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	store := newCoverFaultStore(mem)
	svc := newTranscriptService(beadstest.Mail(store), newBindingLockPool())
	ref := testConversationRef()
	ctx := context.Background()

	store.setFailAdvance(true)
	first, err := svc.Append(ctx, AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-r",
		Text:              "stored but unpublished",
		CreatedAt:         testNow(),
	})
	if err == nil {
		t.Fatalf("first Append should fail while the advance faults; got entry %d", first.Sequence)
	}
	if store.transcriptCreates() != 1 {
		t.Fatalf("first Append stored %d entries, want 1", store.transcriptCreates())
	}

	// The fault clears; the adapter redelivers the same provider message.
	store.setFailAdvance(false)
	second, err := svc.Append(ctx, AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-r",
		Text:              "stored but unpublished",
		CreatedAt:         testNow(),
	})
	if err != nil {
		t.Fatalf("replay Append: %v", err)
	}
	if second.Sequence != 1 {
		t.Fatalf("replay returned sequence %d, want the stored sequence 1", second.Sequence)
	}
	if store.transcriptCreates() != 1 {
		t.Fatalf("replay created a second entry; transcript creates = %d", store.transcriptCreates())
	}

	state, err := svc.State(ctx, testControllerCaller(), ref)
	if err != nil || state == nil {
		t.Fatalf("State: %v, %v", state, err)
	}
	if state.NextSequence != 2 {
		t.Fatalf("NextSequence = %d after replay, want 2 (the stored entry is published)", state.NextSequence)
	}
	if second.ID != fmt.Sprintf("%s-1", state.ID) {
		t.Fatalf("replay entry id = %q, want %q", second.ID, fmt.Sprintf("%s-1", state.ID))
	}

	// The replayed entry is now visible to readers.
	listed, err := svc.List(ctx, ListTranscriptInput{Caller: testControllerCaller(), Conversation: ref})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(listed) != 1 || listed[0].Sequence != 1 {
		t.Fatalf("List after replay = %+v, want the published sequence 1", listed)
	}
}
