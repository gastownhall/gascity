package extmsg

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
)

// ambiguousCreateStore commits the first transcript-entry Create and then
// reports a transport error, like a bd write whose commit landed before the
// connection dropped.
type ambiguousCreateStore struct {
	beads.Store
	failed bool
}

func (s *ambiguousCreateStore) Create(b beads.Bead) (beads.Bead, error) {
	created, err := s.Store.Create(b)
	if err != nil {
		return created, err
	}
	if !s.failed && slices.Contains(b.Labels, labelTranscriptBase) {
		s.failed = true
		return beads.Bead{}, errors.New("invalid connection")
	}
	return created, nil
}

func TestTranscriptAppendSequenceUniqueAfterAmbiguousCreate(t *testing.T) {
	freezeTestClock(t)
	store := &ambiguousCreateStore{Store: beads.NewMemStore()}
	svc := NewServices(beadstest.Mail(store), beadstest.Sessions(store)).Transcript
	ref := testConversationRef()

	appendMsg := func(id string) error {
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
	if err := appendMsg("msg-1"); err == nil {
		t.Fatal("Append(msg-1) succeeded, want the injected ambiguous create error")
	}
	if err := appendMsg("msg-2"); err != nil {
		t.Fatalf("Append(msg-2): %v", err)
	}

	entries, err := store.List(beads.ListQuery{Label: labelTranscriptBase})
	if err != nil {
		t.Fatalf("List transcript entries: %v", err)
	}
	seqs := map[string]string{}
	bySeq := map[string][]string{}
	for _, b := range entries {
		id, seq := b.Metadata["provider_message_id"], b.Metadata["sequence"]
		seqs[id] = seq
		bySeq[seq] = append(bySeq[seq], id)
	}
	for seq, ids := range bySeq {
		if len(ids) > 1 {
			t.Errorf("duplicate transcript sequence #%s=%v (all entries: %v)", seq, ids, seqs)
		}
	}
}
