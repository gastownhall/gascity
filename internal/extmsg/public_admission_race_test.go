package extmsg

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// publicAdmissionReadInterleaving commits a real competing admission exactly
// after the loser checked both receipt keys and before it reads the transcript.
// All rows still land in the same guarded MemStore transaction.
type publicAdmissionReadInterleaving struct {
	*beads.MemStore
	transcriptLabel      string
	beforeTranscriptRead func() error
	once                 sync.Once
	raceErr              error
}

func (s *publicAdmissionReadInterleaving) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Label == s.transcriptLabel {
		s.once.Do(func() { s.raceErr = s.beforeTranscriptRead() })
		if s.raceErr != nil {
			return nil, s.raceErr
		}
	}
	return s.MemStore.List(q)
}

func TestPublicAdmissionRecoversReceiptCommittedBeforeTranscriptLookup(t *testing.T) {
	for _, kind := range []string{"identical key", "same provider message new key", "changed payload", "changed targets", "changed payload new key"} {
		t.Run(kind, func(t *testing.T) {
			mem := publicTestStore()
			winnerInput, loserInput := publicTestInput(), publicTestInput()
			changed := false
			switch kind {
			case "same provider message new key":
				loserInput.RequestKey = "another-transport-key"
			case "changed payload":
				loserInput.Message.Text = "different payload"
				changed = true
			case "changed targets":
				loserInput.Recipients[0].SessionID = "a-different-session"
				changed = true
			case "changed payload new key":
				loserInput.RequestKey = "another-transport-key"
				loserInput.Message.Text = "different payload"
				changed = true
			}
			var winner PublicReceipt
			raced := &publicAdmissionReadInterleaving{
				MemStore: mem, transcriptLabel: transcriptProviderMessageLabel(loserInput.Message.Conversation, loserInput.Message.ProviderMessageID),
			}
			raced.beforeTranscriptRead = func() error {
				var err error
				winner, err = publicTestAdmission(t, mem).Admit(context.Background(), winnerInput)
				return err
			}
			got, err := publicTestAdmission(t, raced).Admit(context.Background(), loserInput)
			if winner.ID == "" {
				t.Fatalf("fixture never committed the competing admission: %v", err)
			}
			if changed {
				if !errors.Is(err, ErrMessageConflict) {
					t.Fatalf("changed replay was not refused: receipt=%+v err=%v", got, err)
				}
			} else if err != nil || got.ID != winner.ID || got.TranscriptID != winner.TranscriptID || got.PayloadDigest != winner.PayloadDigest {
				t.Fatalf("identical replay failed to recover committed receipt: got=%+v winner=%+v err=%v", got, winner, err)
			}
			records, err := NewServices(mem).Transcript.List(context.Background(), ListTranscriptInput{
				Caller: testControllerCaller(), Conversation: winnerInput.Message.Conversation,
			})
			if err != nil || len(records) != 1 || records[0].ID != winner.TranscriptID || records[0].Text != winnerInput.Message.Text {
				t.Fatalf("race changed public history: records=%+v err=%v", records, err)
			}
			pending, err := publicTestAdmission(t, mem).Pending(context.Background(), 100)
			if err != nil || len(pending) != len(winnerInput.Recipients) {
				t.Fatalf("race created extra delivery intents: pending=%+v err=%v", pending, err)
			}
		})
	}
}

func TestPublicAdmissionStillRefusesLegacyTranscriptWithoutReceiptAfterLookupRace(t *testing.T) {
	mem := publicTestStore()
	input := publicTestInput()
	raced := &publicAdmissionReadInterleaving{
		MemStore: mem, transcriptLabel: transcriptProviderMessageLabel(input.Message.Conversation, input.Message.ProviderMessageID),
	}
	var legacy ConversationTranscriptRecord
	raced.beforeTranscriptRead = func() error {
		var err error
		legacy, err = NewServices(mem).Transcript.Append(context.Background(), AppendTranscriptInput{
			Caller: testControllerCaller(), Conversation: input.Message.Conversation, Kind: TranscriptMessageInbound,
			ProviderMessageID: input.Message.ProviderMessageID, Actor: input.Message.Actor, Text: input.Message.Text,
		})
		return err
	}
	got, err := publicTestAdmission(t, raced).Admit(context.Background(), input)
	if !errors.Is(err, ErrMessageConflict) {
		t.Fatalf("legacy delivery was silently adopted/replayed: got=%+v err=%v", got, err)
	}
	if legacy.ID == "" {
		t.Fatal("fixture did not append the legacy record")
	}
	receipt, found, err := publicTestAdmission(t, mem).Recover(context.Background(), input)
	if err != nil || found {
		t.Fatalf("legacy conflict minted admission: receipt=%+v found=%t err=%v", receipt, found, err)
	}
	pending, err := publicTestAdmission(t, mem).Pending(context.Background(), 100)
	if err != nil || len(pending) != 0 {
		t.Fatalf("legacy conflict minted deliveries: pending=%+v err=%v", pending, err)
	}
}
