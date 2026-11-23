package extmsg

import (
	"context"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// lockPoolStore separates one MemStore into independent binding-lock pools:
// each wrapper instance has its own PhysicalStoreKey, so two services over
// the same store never share a conversation lock. Its Create routes
// explicit-ID creates to the memory store's pinned create — the collision
// guard this change adds — and its metadata CAS delegates to the same
// underlying store, so both writers race on identical state.
type lockPoolStore struct {
	beads.Store
	inner   *beads.MemStore
	parked  chan struct{}
	release chan struct{}
	once    sync.Once
}

// pinnedCreator is the subset MemStore grows in this change. The assertion
// keeps this test compiling against the base, where both writers instead
// store a duplicate entry for the same sequence.
type pinnedCreator interface {
	CreatePinned(beads.Bead) (beads.Bead, error)
}

func (s *lockPoolStore) Create(b beads.Bead) (beads.Bead, error) {
	if s.parked != nil {
		s.once.Do(func() {
			s.parked <- struct{}{}
			<-s.release
		})
	}
	if b.ID != "" {
		if creator, ok := any(s.inner).(pinnedCreator); ok {
			return creator.CreatePinned(b)
		}
	}
	return s.inner.Create(b)
}

func (s *lockPoolStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	return s.inner.CompareAndSetMetadataKey(id, key, expected, next)
}

// TestTranscriptAppendSequenceUniqueAcrossLockPools covers two writers over
// one store whose binding lock pools are independent (separate processes, or
// in-process services over different handles): when their creates overlap
// deterministically at the same sequence slot, only one entry lands there,
// the loser allocates the next slot, and the counter never regresses.
func TestTranscriptAppendSequenceUniqueAcrossLockPools(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	writerA := &lockPoolStore{Store: mem, inner: mem}
	writerB := &lockPoolStore{
		Store:   mem,
		inner:   mem,
		parked:  make(chan struct{}),
		release: make(chan struct{}),
	}
	svcA := NewServices(beadstest.Mail(writerA), beadstest.Sessions(writerA)).Transcript
	svcB := NewServices(beadstest.Mail(writerB), beadstest.Sessions(writerB)).Transcript
	ref := testConversationRef()

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

	// Seed the state and one published entry so both writers race at slot 2.
	if err := appendMsg(svcA, "msg-0"); err != nil {
		t.Fatalf("Append(msg-0): %v", err)
	}

	// Writer B reads the head and parks inside its create at slot 2.
	errCh := make(chan error, 1)
	go func() { errCh <- appendMsg(svcB, "msg-b") }()
	<-writerB.parked

	// Writer A wins slot 2 while B is parked.
	if err := appendMsg(svcA, "msg-a"); err != nil {
		t.Fatalf("Append(msg-a): %v", err)
	}

	// B resumes: its pinned create at slot 2 collides, and it must allocate
	// above the published slot instead of storing a duplicate.
	close(writerB.release)
	if err := <-errCh; err != nil {
		t.Fatalf("Append(msg-b) after losing the slot race: %v", err)
	}

	assertUniqueSequences(t, mem, map[string]string{"msg-0": "1", "msg-a": "2", "msg-b": "3"})
	state, err := svcA.State(context.Background(), testAdapterCaller(), ref)
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	if state == nil || state.NextSequence != 4 {
		t.Fatalf("state.NextSequence = %+v, want 4", state)
	}
}
