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

// nonCASRecoveryStore hides the metadata CAS entirely (the way a wrapping
// store that does not forward optional capabilities does) and fails every
// next_sequence advance while its flag is set. The transcript service must
// still converge on this store: allocation must not reuse stored sequences,
// and a completed stale-frontier write must roll the frontier forward over
// the contiguous pinned entries a bounded recovery can see.
type nonCASRecoveryStore struct {
	beads.Store

	mu      sync.Mutex
	fail    bool
	creates int
}

func (s *nonCASRecoveryStore) setFailAdvance(fail bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.fail = fail
}

func (s *nonCASRecoveryStore) failing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.fail
}

func (s *nonCASRecoveryStore) transcriptCreates() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.creates
}

func (s *nonCASRecoveryStore) Create(b beads.Bead) (beads.Bead, error) {
	if hasLabel(b, "gc:extmsg-transcript") {
		s.mu.Lock()
		s.creates++
		s.mu.Unlock()
	}
	return s.Store.Create(b)
}

func (s *nonCASRecoveryStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs["next_sequence"]; ok && s.failing() {
		return errors.New("i/o timeout")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

func appendLive(t *testing.T, svc *transcriptService, ref ConversationRef, pmid string) ConversationTranscriptRecord {
	t.Helper()
	entry, err := svc.Append(context.Background(), AppendTranscriptInput{
		Caller:            testAdapterCaller(),
		Conversation:      ref,
		Kind:              TranscriptMessageInbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: pmid,
		Text:              pmid,
		CreatedAt:         testNow(),
	})
	if err != nil {
		t.Fatalf("Append(%s): %v", pmid, err)
	}
	return entry
}

// TestTranscriptAppendRecoversStaleFrontierWithoutCAS: with no metadata CAS
// available, three entries store while every advance fails (so the durable
// frontier stays stale), then storage recovers and a replay completes a
// covering write. The bounded recovery must roll the frontier forward over
// the contiguous occupied pins — covering every stored entry with no
// duplicate sequences — and the next append must continue past them.
func TestTranscriptAppendRecoversStaleFrontierWithoutCAS(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	mem.HonorExplicitIDs = true
	store := &nonCASRecoveryStore{Store: mem}
	svc := newTranscriptService(beadstest.Mail(store), newBindingLockPool())
	ref := testConversationRef()
	ctx := context.Background()
	controller := testControllerCaller()

	store.setFailAdvance(true)
	for _, pmid := range []string{"msg-1", "msg-2", "msg-3"} {
		_, err := svc.Append(ctx, AppendTranscriptInput{
			Caller:            testAdapterCaller(),
			Conversation:      ref,
			Kind:              TranscriptMessageInbound,
			Provenance:        TranscriptProvenanceLive,
			ProviderMessageID: pmid,
			Text:              pmid,
			CreatedAt:         testNow(),
		})
		if err == nil {
			t.Fatalf("Append(%s) should fail while every advance faults", pmid)
		}
	}
	if store.transcriptCreates() != 3 {
		t.Fatalf("stored %d entries before recovery, want 3", store.transcriptCreates())
	}

	// Storage recovers; a redelivery of the first message completes the
	// covering write and triggers the recovery.
	store.setFailAdvance(false)
	replayed := appendLive(t, svc, ref, "msg-1")
	if replayed.Sequence != 1 {
		t.Fatalf("replay returned sequence %d, want 1", replayed.Sequence)
	}
	if store.transcriptCreates() != 3 {
		t.Fatalf("replay created another entry; transcript creates = %d", store.transcriptCreates())
	}

	state, err := svc.State(ctx, controller, ref)
	if err != nil || state == nil {
		t.Fatalf("State: %v, %v", state, err)
	}
	if state.NextSequence != 4 {
		t.Fatalf("NextSequence = %d after recovery, want 4 (frontier covers the three stored entries)", state.NextSequence)
	}

	entries, err := mem.List(beads.ListQuery{Label: "gc:extmsg-transcript"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	seen := map[int64]string{}
	for _, b := range entries {
		seq := parseInt64(b.Metadata, "sequence")
		if id, dup := seen[seq]; dup {
			t.Fatalf("duplicate transcript sequence #%d = [%s %s]", seq, id, b.Metadata["provider_message_id"])
		}
		seen[seq] = b.Metadata["provider_message_id"]
	}
	if len(seen) != 3 {
		t.Fatalf("stored %d distinct sequences, want 3", len(seen))
	}

	// The next message continues past the recovered entries, at the pinned id
	// for its sequence.
	next := appendLive(t, svc, ref, "msg-4")
	if next.Sequence != 4 {
		t.Fatalf("next entry sequence = %d, want 4", next.Sequence)
	}
	if next.ID != fmt.Sprintf("%s-4", state.ID) {
		t.Fatalf("next entry id = %q, want the pinned id %q", next.ID, fmt.Sprintf("%s-4", state.ID))
	}
}
