package extmsg

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// stateBumpFaultStore injects failures into the transcript state's metadata
// compare-and-set, like a bd write whose commit landed before the connection
// dropped. failCAS counts the remaining CAS calls to fail.
type stateBumpFaultStore struct {
	beads.Store
	inner   *beads.MemStore
	failCAS atomic.Int32
}

func (s *stateBumpFaultStore) CompareAndSetMetadataKey(id, key, expected, next string) (bool, error) {
	if n := s.failCAS.Load(); n > 0 {
		s.failCAS.Add(-1)
		return false, errors.New("invalid connection")
	}
	return s.inner.CompareAndSetMetadataKey(id, key, expected, next)
}

// TestTranscriptAppendSequenceUniqueAfterStateBumpFailure covers a lost
// next_sequence advancement: the entry is stored but the head is never
// bumped, and the next append used to re-issue the same sequence. A
// transient CAS failure recovers inside the append; a persistent one stays a
// retryable error, and the next append after the fault clears publishes the
// original entry and allocates above it without duplication or overwrite.
func TestTranscriptAppendSequenceUniqueAfterStateBumpFailure(t *testing.T) {
	freezeTestClock(t)

	appendMsg := func(svc TranscriptService, id string) error {
		_, err := svc.Append(context.Background(), AppendTranscriptInput{
			Caller:            testAdapterCaller(),
			Conversation:      testConversationRef(),
			Kind:              TranscriptMessageInbound,
			Provenance:        TranscriptProvenanceLive,
			ProviderMessageID: id,
			Text:              id,
			CreatedAt:         testNow(),
		})
		return err
	}

	t.Run("transient failure recovers within the append", func(t *testing.T) {
		mem := beads.NewMemStore()
		store := &stateBumpFaultStore{Store: mem, inner: mem}
		svc := NewServices(beadstest.Mail(store), beadstest.Sessions(store)).Transcript

		store.failCAS.Store(1)
		if err := appendMsg(svc, "msg-1"); err != nil {
			t.Fatalf("Append(msg-1) under transient bump failure: %v", err)
		}
		entries, err := store.List(beads.ListQuery{Label: labelTranscriptBase})
		if err != nil {
			t.Fatalf("List transcript entries: %v", err)
		}
		if len(entries) != 1 || entries[0].Metadata["sequence"] != "1" {
			t.Fatalf("after transient bump failure: %d entries, want one at sequence 1", len(entries))
		}
		if err := appendMsg(svc, "msg-2"); err != nil {
			t.Fatalf("Append(msg-2): %v", err)
		}
		assertUniqueSequences(t, store, map[string]string{"msg-1": "1", "msg-2": "2"})
		state, err := svc.State(context.Background(), testAdapterCaller(), testConversationRef())
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if state == nil || state.NextSequence != 3 {
			t.Fatalf("state.NextSequence = %+v, want 3", state)
		}
	})

	t.Run("persistent failure stays retryable and later append publishes the original", func(t *testing.T) {
		mem := beads.NewMemStore()
		store := &stateBumpFaultStore{Store: mem, inner: mem}
		svc := NewServices(beadstest.Mail(store), beadstest.Sessions(store)).Transcript

		// More than the bounded publish attempts inside one append.
		store.failCAS.Store(100)
		err := appendMsg(svc, "msg-1")
		if err == nil {
			t.Fatal("Append(msg-1) under persistent bump failure succeeded, want a retryable error")
		}

		// The entry committed before publication failed: it stays stored at
		// sequence 1 with the head still at 1, and no duplicate exists.
		entries, err := store.List(beads.ListQuery{Label: labelTranscriptBase})
		if err != nil {
			t.Fatalf("List transcript entries: %v", err)
		}
		if len(entries) != 1 {
			t.Fatalf("entries after failed publication = %d, want 1 (no duplicate)", len(entries))
		}

		store.failCAS.Store(0)
		if err := appendMsg(svc, "msg-2"); err != nil {
			t.Fatalf("Append(msg-2) after fault cleared: %v", err)
		}
		assertUniqueSequences(t, store, map[string]string{"msg-1": "1", "msg-2": "2"})
		state, err := svc.State(context.Background(), testAdapterCaller(), testConversationRef())
		if err != nil {
			t.Fatalf("State: %v", err)
		}
		if state == nil || state.NextSequence != 3 {
			t.Fatalf("state.NextSequence = %+v, want 3", state)
		}
	})
}

// assertUniqueSequences checks that the stored transcript entries carry
// exactly the wanted provider-message -> sequence mapping with no
// duplicates.
func assertUniqueSequences(t *testing.T, store beads.Store, want map[string]string) {
	t.Helper()
	entries, err := store.List(beads.ListQuery{Label: labelTranscriptBase})
	if err != nil {
		t.Fatalf("List transcript entries: %v", err)
	}
	bySeq := map[string][]string{}
	for _, b := range entries {
		id, seq := b.Metadata["provider_message_id"], b.Metadata["sequence"]
		bySeq[seq] = append(bySeq[seq], id)
		if wantSeq, ok := want[id]; !ok || wantSeq != seq {
			t.Errorf("entry %q has sequence %q, want %q", id, seq, wantSeq)
		}
	}
	for seq, ids := range bySeq {
		if len(ids) > 1 {
			t.Errorf("duplicate transcript sequence #%s=%v", seq, ids)
		}
	}
	if len(bySeq) != len(want) {
		t.Errorf("stored %d distinct sequences, want %d", len(bySeq), len(want))
	}
}
