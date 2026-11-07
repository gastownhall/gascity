package extmsg

import (
	"context"
	"strconv"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// lockPoolContentionStore models a pin-refusing, CAS-capable store shared by
// two writers whose in-process lock pools cannot see each other. It gates the
// first transcript-entry create until the test releases it, so a second
// writer completes a whole append at that sequence before the first one's
// create returns. It forwards the metadata CAS itself (the way production
// wrappers do) and records every next_sequence value written, so the test can
// pin that the durable frontier never decreases.
type lockPoolContentionStore struct {
	beads.Store
	cas beads.MetadataCASWriter

	mu       sync.Mutex
	armed    bool
	gated    bool
	release  chan struct{}
	arrived  chan struct{}
	frontier []int64
}

func newLockPoolContentionStore(inner *beads.MemStore) *lockPoolContentionStore {
	cas, ok := beads.MetadataCASWriterFor(inner)
	if !ok {
		panic("inner store must offer the metadata CAS")
	}
	return &lockPoolContentionStore{
		Store:   inner,
		cas:     cas,
		release: make(chan struct{}),
		arrived: make(chan struct{}),
	}
}

// gateFirstTranscriptCreate arms the gate; the first create of a transcript
// entry (with or without a pinned id, so the base behavior hooks it too)
// blocks until release.
func (s *lockPoolContentionStore) gateFirstTranscriptCreate() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.armed = true
}

func (s *lockPoolContentionStore) Create(b beads.Bead) (beads.Bead, error) {
	s.mu.Lock()
	gate := s.armed && !s.gated && hasLabel(b, "gc:extmsg-transcript")
	if gate {
		s.gated = true
	}
	s.mu.Unlock()
	if gate {
		close(s.arrived)
		<-s.release
	}
	return s.Store.Create(b)
}

func (s *lockPoolContentionStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if key == "next_sequence" {
		if n, err := strconv.ParseInt(next, 10, 64); err == nil {
			s.mu.Lock()
			s.frontier = append(s.frontier, n)
			s.mu.Unlock()
		}
	}
	return s.cas.CompareAndSetMetadataKey(id, key, expected, next)
}

func (s *lockPoolContentionStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if raw, ok := kvs["next_sequence"]; ok {
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			s.mu.Lock()
			s.frontier = append(s.frontier, n)
			s.mu.Unlock()
		}
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func (s *lockPoolContentionStore) frontierWrites() []int64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]int64(nil), s.frontier...)
}

// TestTranscriptAppendSequenceUniqueAcrossLockPools: two services over one
// store but separate lock pools (two processes, or the Services generation a
// config reload builds) that interleave at the same sequence must still store
// entries with distinct sequences, and the durable frontier must never
// decrease. The store refuses a pinned entry ID and offers metadata CAS.
func TestTranscriptAppendSequenceUniqueAcrossLockPools(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	store := newLockPoolContentionStore(mem)
	poolA := newBindingLockPool()
	poolB := newBindingLockPool()
	if poolA == poolB {
		t.Fatal("test needs two distinct lock pools")
	}
	svcA := newTranscriptService(beadstest.Mail(store), poolA)
	svcB := newTranscriptService(beadstest.Mail(store), poolB)
	ref := testConversationRef()
	ctx := context.Background()

	// Initialize the shared transcript state without consuming a sequence.
	if _, err := svcA.EnsureMembership(ctx, EnsureMembershipInput{
		Caller:       testControllerCaller(),
		Conversation: ref,
		SessionID:    "sess-pool",
	}); err != nil {
		t.Fatalf("EnsureMembership: %v", err)
	}
	state, err := svcA.State(ctx, testControllerCaller(), ref)
	if err != nil || state == nil {
		t.Fatalf("State: %v, %v", state, err)
	}
	if state.NextSequence != 1 {
		t.Fatalf("fresh transcript NextSequence = %d, want 1", state.NextSequence)
	}
	store.gateFirstTranscriptCreate()

	type appendResult struct {
		entry ConversationTranscriptRecord
		err   error
	}
	aDone := make(chan appendResult, 1)
	go func() {
		entry, err := svcA.Append(ctx, AppendTranscriptInput{
			Caller:            testAdapterCaller(),
			Conversation:      ref,
			Kind:              TranscriptMessageInbound,
			Provenance:        TranscriptProvenanceLive,
			ProviderMessageID: "msg-a",
			Text:              "from pool A",
			CreatedAt:         testNow(),
		})
		aDone <- appendResult{entry: entry, err: err}
	}()
	<-store.arrived

	// While A's create sits inside the store, B completes a whole append at
	// the same sequence from its own pool.
	bEntry, err := svcB.Append(ctx, AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: "msg-b",
		Text:              "from pool B",
		CreatedAt:         testNow(),
	})
	if err != nil {
		t.Fatalf("Append B: %v", err)
	}
	close(store.release)
	aRes := <-aDone
	if aRes.err != nil {
		t.Fatalf("Append A: %v", aRes.err)
	}

	if aRes.entry.Sequence == bEntry.Sequence {
		t.Fatalf("both pools stored sequence %d", bEntry.Sequence)
	}
	got := map[int64]string{aRes.entry.Sequence: "msg-a", bEntry.Sequence: "msg-b"}
	if len(got) != 2 {
		t.Fatalf("sequences %d and %d are not distinct", aRes.entry.Sequence, bEntry.Sequence)
	}

	entries, err := mem.List(beads.ListQuery{Label: "gc:extmsg-transcript"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("stored %d transcript entries, want 2", len(entries))
	}
	seen := map[int64]string{}
	for _, b := range entries {
		seq := parseInt64(b.Metadata, "sequence")
		if id, dup := seen[seq]; dup {
			t.Fatalf("duplicate transcript sequence #%d = [%s %s]", seq, id, b.Metadata["provider_message_id"])
		}
		seen[seq] = b.Metadata["provider_message_id"]
	}

	writes := store.frontierWrites()
	previous := int64(0)
	for _, w := range writes {
		if w < previous {
			t.Fatalf("durable frontier decreased: %v", writes)
		}
		previous = w
	}
	final, err := svcA.State(ctx, testControllerCaller(), ref)
	if err != nil || final == nil {
		t.Fatalf("final State: %v, %v", final, err)
	}
	if final.NextSequence != 3 {
		t.Fatalf("final NextSequence = %d, want 3", final.NextSequence)
	}
}
