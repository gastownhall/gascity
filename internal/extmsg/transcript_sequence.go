package extmsg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

// Transcript entry allocation.
//
// Every entry is created at a pinned id <state-id>-<sequence>, where the state
// bead and the entry share the store's id namespace. Allocation is
// create-first: the entry is stored before the state's next_sequence frontier
// moves, so a fault between the two leaves a stored-but-unpublished entry —
// never a published-but-missing one. Three properties make that safe:
//
//   - The allocation sequence is taken from a live bucket scan, not from the
//     frontier alone: the next sequence is the first at or above the frontier
//     whose bucket holds no stored entry of this conversation. A writer whose
//     frontier advance failed does not hand its sequence to the next writer,
//     even across lock pools or processes.
//   - A create that answers an error is classified by reading the pin back:
//     absent, ours (recover and publish it), another writer's entry (cover it
//     and take the next sequence), or foreign data (an invariant violation).
//     "Ours" is proven by this Append's random token, persisted with the
//     entry, or by a matching nonblank provider message ID. The binding lock
//     serializes local writers; the pinned ids make the create itself the
//     fence against every other writer.
//   - The frontier only ever advances to one past a stored entry, via the
//     metadata CAS when the store offers it and a covering write under the
//     binding lock otherwise. It never decreases.

const (
	// transcriptAppendMaxAttempts bounds one Append's allocation attempts:
	// each attempt handles one occupied pin (another writer's entry stored at
	// the sequence we wanted).
	transcriptAppendMaxAttempts = 8
	// transcriptCoverMaxAttempts bounds the frontier cover's retries against
	// a racing writer or an ambiguous compare-and-set error.
	transcriptCoverMaxAttempts = 8
	// transcriptScanMaxPasses bounds the allocation bucket scan: one pass per
	// sequence the scan has to skip.
	transcriptScanMaxPasses = 32
	// transcriptRecoverMaxEntries bounds the CAS-less roll-forward: how many
	// contiguous occupied pins one completed covering write may recover over.
	transcriptRecoverMaxEntries = 64

	// transcriptAppendTokenKey is the metadata key every appended entry
	// carries this Append's random token under. A create that committed but
	// answered an error is identified as this Append's entry by matching it.
	transcriptAppendTokenKey = "append_token"
)

// newTranscriptAppendToken mints the per-Append ownership token.
func newTranscriptAppendToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate transcript append token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// transcriptPinnedEntryID is the id a transcript entry for sequence is created
// at: the state bead's own id namespace, so a store that honors pinned ids
// keeps the entry exactly readable by sequence.
func transcriptPinnedEntryID(stateID string, sequence int64) string {
	return fmt.Sprintf("%s-%d", stateID, sequence)
}

// transcriptPinsHonored reports whether entryID is a pin of stateID: only
// then is reading a pin a reliable probe of a stored entry (a store that
// minted its own id over the pin stores the entry elsewhere in the
// namespace).
func transcriptPinsHonored(entryID, stateID string) bool {
	return len(entryID) > len(stateID)+1 && entryID[:len(stateID)] == stateID && entryID[len(stateID)] == '-'
}

// live reads this service's store behind any cache the store chain carries, so
// allocation and recovery decide on stored state, not a possibly-stale view.
func (s *transcriptService) live() beads.LiveReader {
	return beads.LiveFor(s.store)
}

// findStateLiveLocked is findStateLocked reading the store live.
func (s *transcriptService) findStateLiveLocked(ref ConversationRef) (*ConversationTranscriptStateRecord, error) {
	items, err := s.live().List(beads.ListQuery{Label: transcriptStateLabel(ref)})
	if err != nil {
		return nil, fmt.Errorf("list transcript state: %w", err)
	}
	var out *ConversationTranscriptStateRecord
	for _, item := range items {
		if !hasLabel(item, "gc:extmsg-transcript-state") || item.Status == "closed" {
			continue
		}
		record, err := decodeTranscriptStateBead(item)
		if err != nil {
			return nil, err
		}
		if !sameConversationRef(record.Conversation, ref) {
			continue
		}
		if out != nil {
			return nil, fmt.Errorf("%w: multiple transcript states for %s", ErrInvariantViolation, conversationLockKey(ref))
		}
		rec := record
		out = &rec
	}
	return out, nil
}

// findTranscriptByProviderMessageLive is findTranscriptByProviderMessageLocked
// reading the store live.
func (s *transcriptService) findTranscriptByProviderMessageLive(ref ConversationRef, providerMessageID string) (*ConversationTranscriptRecord, error) {
	items, err := s.live().List(beads.ListQuery{Label: transcriptProviderMessageLabel(ref, providerMessageID)})
	if err != nil {
		return nil, fmt.Errorf("list transcript by provider message label: %w", err)
	}
	var out *ConversationTranscriptRecord
	for _, item := range items {
		if !hasLabel(item, "gc:extmsg-transcript") || item.Status == "closed" {
			continue
		}
		record, err := decodeTranscriptBead(item)
		if err != nil {
			return nil, err
		}
		if !sameConversationRef(record.Conversation, ref) || record.ProviderMessageID != providerMessageID {
			continue
		}
		if out != nil {
			return nil, fmt.Errorf("%w: duplicate transcript provider message %q", ErrInvariantViolation, providerMessageID)
		}
		rec := record
		out = &rec
	}
	return out, nil
}

// liveBucketMaxSequence returns the highest stored entry sequence of this
// conversation in bucket, 0 when the bucket holds none.
func (s *transcriptService) liveBucketMaxSequence(ref ConversationRef, bucket int64) (int64, error) {
	items, err := s.live().List(beads.ListQuery{Label: transcriptBucketLabel(ref, bucket)})
	if err != nil {
		return 0, fmt.Errorf("list transcript bucket %d: %w", bucket, err)
	}
	var highest int64
	for _, item := range items {
		if !hasLabel(item, "gc:extmsg-transcript") || item.Status == "closed" {
			continue
		}
		record, err := decodeTranscriptBead(item)
		if err != nil {
			return 0, err
		}
		if !sameConversationRef(record.Conversation, ref) {
			continue
		}
		if record.Sequence > highest {
			highest = record.Sequence
		}
	}
	return highest, nil
}

// nextAllocatableSequenceLive returns the first sequence at or above floor
// that no stored entry of this conversation occupies. The frontier is the
// floor, but stored entries beyond it (a writer whose advance failed) push the
// allocation above themselves, so a sequence is never handed to two messages.
func (s *transcriptService) nextAllocatableSequenceLive(ref ConversationRef, floor int64) (int64, error) {
	sequence := floor
	if sequence < 1 {
		sequence = 1
	}
	for range transcriptScanMaxPasses {
		highest, err := s.liveBucketMaxSequence(ref, transcriptBucket(sequence))
		if err != nil {
			return 0, err
		}
		if highest < sequence {
			return sequence, nil
		}
		sequence = highest + 1
	}
	return 0, fmt.Errorf("scan transcript entries for %s above sequence %d: too many stored entries", conversationLockKey(ref), floor)
}

// coverTranscriptFrontierLocked advances the state's next_sequence past
// storedSeq — never lowering it — and initializes earliest_available_sequence
// when the state predates it. The advance goes through the metadata CAS when
// the store offers it, so a racing writer's frontier survives; otherwise the
// binding lock serializes local writers and a covering write is exact, and
// when the store honors pins the write is followed by a bounded roll-forward
// over the contiguous occupied pins earlier failed advances left behind. The
// CAS-conflict and CAS-error cases re-read the live state: a frontier already
// past the target resolves them, and the remaining budget bounds the retry.
func (s *transcriptService) coverTranscriptFrontierLocked(ctx context.Context, ref ConversationRef, stateID string, storedSeq int64, pinsHonored bool) error {
	target := storedSeq + 1
	var lastErr error
	for range transcriptCoverMaxAttempts {
		if err := checkContext(ctx); err != nil {
			return err
		}
		state, err := s.findStateLiveLocked(ref)
		if err != nil {
			return err
		}
		if state == nil {
			return fmt.Errorf("%w: transcript state for %s disappeared", ErrInvariantViolation, conversationLockKey(ref))
		}
		if state.ID != stateID {
			return fmt.Errorf("%w: transcript state for %s replaced %s during append", ErrInvariantViolation, conversationLockKey(ref), stateID)
		}
		if state.NextSequence >= target {
			return nil
		}
		if state.EarliestAvailableSequence <= 0 {
			if err := s.store.SetMetadataBatch(state.ID, map[string]string{"earliest_available_sequence": "1"}); err != nil {
				return fmt.Errorf("update transcript state: %w", err)
			}
		}
		if writer, ok := beads.As[beads.MetadataCASWriter](s.store); ok {
			swapped, err := writer.CompareAndSetMetadataKey(state.ID, "next_sequence",
				strconv.FormatInt(state.NextSequence, 10), strconv.FormatInt(target, 10))
			if err == nil {
				if swapped {
					return nil
				}
				continue // a racing writer moved the frontier; the live re-read classifies
			}
			if !errors.Is(err, beads.ErrConditionalWriteUnsupported) {
				lastErr = fmt.Errorf("compare-and-set next_sequence: %w", err)
				continue // ambiguous: the live re-read decides whether we are covered
			}
			// The writer refused the fence at call time; fall through to the
			// locked covering write.
		}
		if err := s.store.SetMetadataBatch(state.ID, map[string]string{
			"next_sequence": strconv.FormatInt(target, 10),
		}); err != nil {
			return fmt.Errorf("update transcript state: %w", err)
		}
		if pinsHonored {
			return s.rollForwardOverPinsLocked(ref, stateID, target)
		}
		return nil
	}
	if lastErr != nil {
		return fmt.Errorf("advance transcript sequence past %d for %s: %w", storedSeq, conversationLockKey(ref), lastErr)
	}
	return fmt.Errorf("advance transcript sequence past %d for %s: too many attempts", storedSeq, conversationLockKey(ref))
}

// rollForwardOverPinsLocked completes a covering write on a store without
// metadata CAS: earlier writers may have stored entries at pins beyond the
// just-written frontier while their own advances failed, so probe the pins
// forward and advance over every contiguous occupied one. A sequence is never
// advanced over unless its pin reads back a stored bead — absence stops the
// walk. The probe budget bounds it; exhausting it persists what was verified
// and reports the recovery incomplete.
func (s *transcriptService) rollForwardOverPinsLocked(ref ConversationRef, stateID string, from int64) error {
	live := s.live()
	frontier := from
	complete := false
	for range transcriptRecoverMaxEntries {
		_, err := live.Get(transcriptPinnedEntryID(stateID, frontier))
		if errors.Is(err, beads.ErrNotFound) {
			complete = true
			break
		}
		if err != nil {
			return fmt.Errorf("recover transcript frontier for %s at sequence %d: %w", conversationLockKey(ref), frontier, err)
		}
		frontier++
	}
	if frontier > from {
		if err := s.store.SetMetadataBatch(stateID, map[string]string{
			"next_sequence": strconv.FormatInt(frontier, 10),
		}); err != nil {
			return fmt.Errorf("update transcript state: %w", err)
		}
	}
	if !complete {
		return fmt.Errorf("recover transcript frontier for %s above sequence %d: too many contiguous stored entries", conversationLockKey(ref), from-1)
	}
	return nil
}

// transcriptPinState classifies a create that answered an error, by reading
// the pinned id back.
type transcriptPinState int

const (
	// transcriptPinAbsent: nothing is stored at the pin; the create error is
	// the whole failure.
	transcriptPinAbsent transcriptPinState = iota
	// transcriptPinVerifyFailed: the read-back itself failed; the create
	// error is reported alongside it.
	transcriptPinVerifyFailed
	// transcriptPinOurs: the pin holds this append's entry (it committed);
	// recover by publishing it.
	transcriptPinOurs
	// transcriptPinAnother: the pin holds another writer's valid entry;
	// publish past it and take the next sequence.
	transcriptPinAnother
	// transcriptPinUnidentifiable: the pin holds an entry this append cannot
	// prove ownership of; report the create error rather than double-store.
	transcriptPinUnidentifiable
	// transcriptPinForeign: the pin holds data that is not an entry of this
	// conversation at this sequence; an invariant violation.
	transcriptPinForeign
)

// classifyPinnedCreateLocked reads the pin for sequence back and classifies
// what the refused create left there. Ownership evidence, strongest first:
// this Append's token, then a matching nonblank provider message ID.
func (s *transcriptService) classifyPinnedCreateLocked(ref ConversationRef, stateID string, sequence int64, providerMessageID, appendToken string) (transcriptPinState, *ConversationTranscriptRecord, error) {
	pin := transcriptPinnedEntryID(stateID, sequence)
	b, err := s.live().Get(pin)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return transcriptPinAbsent, nil, nil
		}
		return transcriptPinVerifyFailed, nil, fmt.Errorf("verify pinned transcript entry %s: %w", pin, err)
	}
	record, decodeErr := decodeTranscriptBead(b)
	if decodeErr != nil || !sameConversationRef(record.Conversation, ref) || record.Sequence != sequence {
		return transcriptPinForeign, nil, fmt.Errorf("%w: pinned transcript entry %s holds %q", ErrInvariantViolation, pin, b.Title)
	}
	if appendToken != "" && strings.TrimSpace(b.Metadata[transcriptAppendTokenKey]) == appendToken {
		rec := record
		return transcriptPinOurs, &rec, nil
	}
	if providerMessageID != "" {
		if record.ProviderMessageID == providerMessageID {
			rec := record
			return transcriptPinOurs, &rec, nil
		}
		// A different provider message id cannot be this append's entry.
		return transcriptPinAnother, nil, nil
	}
	// The stored entry carries neither this Append's token nor a comparable
	// provider message ID, so ownership cannot be proven: surface the create
	// error rather than store this message twice.
	return transcriptPinUnidentifiable, nil, nil
}
