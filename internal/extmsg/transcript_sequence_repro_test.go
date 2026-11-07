package extmsg

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// failNextSequenceBumpStore stores every entry but fails the first
// next_sequence advance, as a store would on a dropped connection.
type failNextSequenceBumpStore struct {
	beads.Store
	failed bool
}

func (s *failNextSequenceBumpStore) SetMetadataBatch(id string, kvs map[string]string) error {
	if _, ok := kvs["next_sequence"]; ok && !s.failed {
		s.failed = true
		return errors.New("i/o timeout")
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// TestTranscriptAppendNeverReusesSequenceAfterFailedAdvance: an entry the store
// committed must keep its sequence even when the next_sequence advance fails,
// so the next message gets a fresh sequence.
func TestTranscriptAppendNeverReusesSequenceAfterFailedAdvance(t *testing.T) {
	freezeTestClock(t)
	mem := beads.NewMemStore()
	store := &failNextSequenceBumpStore{Store: mem}
	svc := NewServices(beadstest.Mail(store), beadstest.Sessions(store)).Transcript
	ref := testConversationRef()

	for _, id := range []string{"msg-1", "msg-2"} {
		_, _ = svc.Append(context.Background(), AppendTranscriptInput{
			Caller:            testAdapterCaller(),
			Conversation:      ref,
			Kind:              TranscriptMessageInbound,
			Provenance:        TranscriptProvenanceLive,
			ProviderMessageID: id,
			Text:              id,
			CreatedAt:         testNow(),
		})
	}

	entries, err := mem.List(beads.ListQuery{Label: "gc:extmsg-transcript"})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	bySeq := map[string][]string{}
	for _, b := range entries {
		seq := b.Metadata["sequence"]
		bySeq[seq] = append(bySeq[seq], b.Metadata["provider_message_id"])
	}
	for seq, ids := range bySeq {
		if len(ids) > 1 {
			t.Errorf("duplicate transcript sequence #%s=%v", seq, ids)
		}
	}
}
