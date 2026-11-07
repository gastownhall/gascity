package extmsg

import (
	"errors"
	"fmt"
	"strconv"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// transcriptAppendAttempts bounds how many sequences one Append tries
	// while other writers keep occupying the frontier.
	transcriptAppendAttempts = 8
	// transcriptFrontierAttempts bounds the reads and writes that advance
	// next_sequence past one stored entry.
	transcriptFrontierAttempts = 8
)

// errTranscriptContended reports a bounded retry loop that ran out of
// attempts without a store error to blame.
var errTranscriptContended = errors.New("transcript sequence contended")

// transcriptEntryID is the pinned bead ID of the entry at sequence: the
// transcript state bead's ID, which its store minted, plus the sequence. A
// store that honors pinned IDs refuses a second entry at one sequence.
func transcriptEntryID(stateID string, sequence int64) string {
	return stateID + "-" + strconv.FormatInt(sequence, 10)
}

// readTranscriptStateLive reads the transcript state bead from the store
// itself, past any cache, and returns it with its decoded record.
func (s *transcriptService) readTranscriptStateLive(stateID string) (beads.Bead, ConversationTranscriptStateRecord, error) {
	b, err := beads.LiveFor(s.store).Get(stateID)
	if err != nil {
		return beads.Bead{}, ConversationTranscriptStateRecord{}, fmt.Errorf("read transcript state %s: %w", stateID, err)
	}
	record, err := decodeTranscriptStateBead(b)
	if err != nil {
		return beads.Bead{}, ConversationTranscriptStateRecord{}, fmt.Errorf("decode transcript state %s: %w", stateID, err)
	}
	return b, record, nil
}

// decodeTranscriptOccupant decodes the bead found at the pinned ID of
// sequence, and refuses it unless it is that sequence's entry of ref.
func decodeTranscriptOccupant(b beads.Bead, ref ConversationRef, sequence int64) (ConversationTranscriptRecord, error) {
	if !hasLabel(b, "gc:extmsg-transcript") {
		return ConversationTranscriptRecord{}, fmt.Errorf("%w: pinned transcript entry ID %s holds a bead that is not a transcript entry", ErrInvariantViolation, b.ID)
	}
	record, err := decodeTranscriptBead(b)
	if err != nil {
		return ConversationTranscriptRecord{}, fmt.Errorf("%w: decode pinned transcript entry %s: %w", ErrInvariantViolation, b.ID, err)
	}
	if !sameConversationRef(record.Conversation, ref) || record.Sequence != sequence {
		return ConversationTranscriptRecord{}, fmt.Errorf("%w: pinned transcript entry %s holds %s sequence %d, want %s sequence %d",
			ErrInvariantViolation, b.ID, conversationLockKey(record.Conversation), record.Sequence, conversationLockKey(ref), sequence)
	}
	return record, nil
}

// coverSequenceLocked advances the transcript's next_sequence past sequence,
// an entry the store holds, so readers and read cursors can reach it. It
// never lowers next_sequence on a store with metadata CAS: it reads the live
// value, stops when that value already covers sequence, and otherwise swaps
// it for sequence+1 only while it is still the value it read, re-reading
// after a lost swap or an ambiguous write error.
//
// A store without metadata CAS gets the same loop with a plain write, which
// the binding lock serializes only within this process: a writer elsewhere
// can still overwrite a higher value with a lower one.
func (s *transcriptService) coverSequenceLocked(stateID string, sequence int64) error {
	writer, useCAS := beads.As[beads.MetadataCASWriter](s.store)
	target := strconv.FormatInt(sequence+1, 10)
	lastErr := errTranscriptContended
	for range transcriptFrontierAttempts {
		stateBead, state, err := s.readTranscriptStateLive(stateID)
		if err != nil {
			return err
		}
		if state.NextSequence > sequence {
			return s.ensureEarliestAvailableLocked(state)
		}
		if useCAS {
			swapped, err := writer.CompareAndSetMetadataKey(stateID, "next_sequence", stateBead.Metadata["next_sequence"], target)
			switch {
			case errors.Is(err, beads.ErrConditionalWriteUnsupported):
				useCAS = false
			case err != nil:
				lastErr = err
			case swapped:
				return s.ensureEarliestAvailableLocked(state)
			}
			continue
		}
		if err := s.store.SetMetadataBatch(stateID, map[string]string{"next_sequence": target}); err != nil {
			lastErr = err
			continue
		}
		return s.ensureEarliestAvailableLocked(state)
	}
	return fmt.Errorf("advance transcript %s next_sequence past %d: %w", stateID, sequence, lastErr)
}

// ensureEarliestAvailableLocked marks sequence 1 as the earliest available
// entry of a transcript state that does not record one yet.
func (s *transcriptService) ensureEarliestAvailableLocked(state ConversationTranscriptStateRecord) error {
	if state.EarliestAvailableSequence > 0 {
		return nil
	}
	if err := s.store.SetMetadataBatch(state.ID, map[string]string{"earliest_available_sequence": "1"}); err != nil {
		return fmt.Errorf("update transcript state %s earliest sequence: %w", state.ID, err)
	}
	return nil
}
