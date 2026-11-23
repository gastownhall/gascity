// Transcript sequence publication and recovery.
//
// A transcript sequence is allocated by advancing the state bead's
// next_sequence, and an entry belongs to the slot it names. The append path
// below inverts the old write order: it fills the slot first (creating the
// entry bead with a deterministic ID) and publishes afterwards with a
// metadata compare-and-set, so a fault between the two leaves the entry
// stored but unpublished instead of re-issuing the sequence. Three faults
// used to duplicate a sequence this way:
//
//   - a create whose commit landed before a transport error reported failure:
//     the next append re-issued the same sequence;
//   - a lost next_sequence bump: the next append re-issued the same sequence;
//   - two writers over independent lock pools: both appended at the same
//     lagging head.
//
// Every append walks the same bounded loop: read the live head, deliver a
// duplicate provider message if one is already stored (repairing its
// publication), publish-and-retry when the slot already holds an entry
// (another writer's, a committed-then-errored create, or a legacy minted
// entry from a pre-upgrade writer), and otherwise create at the slot and
// publish. Live reads go through the beads live handle so another writer's
// publication is always visible, with or without a caching layer.

package extmsg

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	// transcriptAppendSlotAttempts bounds the slot-allocation retries in one
	// append: each retry follows a live re-read of the head, so the loop only
	// spins while concurrent writers keep winning slots first.
	transcriptAppendSlotAttempts = 6

	// transcriptPublishAttempts bounds the metadata CAS retries for one
	// sequence publication. A lost race is confirmed by the next read, and
	// a transport error is retried because a committed CAS shows up as
	// already_next on re-read.
	transcriptPublishAttempts = 4

	// metadataKeyAppendToken is the per-append token that recognizes this
	// invocation's entry after a create whose commit was reported as an
	// error, for provider messages that carry no provider ID.
	metadataKeyAppendToken = "append_token"
)

// errTranscriptRetryable marks a transcript append fault the caller may
// safely retry: the create or publication did not confirm, but no duplicate
// was written and a later append recovers. Validation, invariant,
// hydration-pending, and context-cancellation errors are never wrapped in
// it.
var errTranscriptRetryable = errors.New("transcript append retryable fault")

// transcriptAppendDraft is the immutable per-call entry payload, built once
// before the slot loop so retries write byte-identical entries.
type transcriptAppendDraft struct {
	kind              TranscriptMessageKind
	provenance        TranscriptProvenance
	providerMessageID string
	explicitTarget    string
	replyToMessageID  string
	sourceSessionID   string
	text              string
	actorJSON         string
	attachmentsJSON   string
	createdAt         time.Time
	metadata          map[string]string
}

// newTranscriptAppendToken returns the private unprefixed token identifying
// this append invocation's entry.
func newTranscriptAppendToken() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generate append token: %w", err)
	}
	return hex.EncodeToString(buf), nil
}

// transcriptEntryID is the deterministic entry ID for a conversation slot:
// pinned by the store when the create honors explicit IDs, and otherwise
// still carried on the create so stores that do pin (SQLite, bd) get the
// collision protection for free.
func transcriptEntryID(stateID string, sequence int64) string {
	return stateID + "-" + strconv.FormatInt(sequence, 10) + "-entry"
}

// appendDraftLocked fills the next free sequence slot with the drafted entry
// and publishes it. stateID is the conversation's transcript state bead.
func (s *transcriptService) appendDraftLocked(ctx context.Context, ref ConversationRef, stateID string, draft transcriptAppendDraft, token string) (ConversationTranscriptRecord, error) {
	for attempt := 0; attempt < transcriptAppendSlotAttempts; attempt++ {
		if err := checkContext(ctx); err != nil {
			return ConversationTranscriptRecord{}, err
		}
		_, state, err := s.readStateBeadLive(stateID)
		if err != nil {
			return ConversationTranscriptRecord{}, err
		}
		slot := state.NextSequence
		if draft.providerMessageID != "" {
			existing, err := s.findTranscriptByProviderMessageLive(ref, draft.providerMessageID)
			if err != nil {
				return ConversationTranscriptRecord{}, err
			}
			if existing != nil {
				if err := s.publishSlotLive(ref, stateID, existing.Sequence); err != nil {
					return ConversationTranscriptRecord{}, err
				}
				return *existing, nil
			}
		}
		occupant, err := s.findSlotOccupantLive(ref, slot)
		if err != nil {
			return ConversationTranscriptRecord{}, err
		}
		if occupant != nil {
			if draft.providerMessageID != "" && occupant.ProviderMessageID == draft.providerMessageID {
				// A concurrent delivery of the same provider message won
				// this slot: the message is already stored exactly once.
				if err := s.publishSlotLive(ref, stateID, slot); err != nil {
					return ConversationTranscriptRecord{}, err
				}
				return *occupant, nil
			}
			// The slot is filled but unpublished: another writer's
			// committed-then-errored create, or a legacy minted entry left at
			// a lagging head by a pre-upgrade writer. Publish it, then retry
			// at the live head.
			if err := s.publishSlotLive(ref, stateID, slot); err != nil {
				return ConversationTranscriptRecord{}, err
			}
			continue
		}
		entry := s.buildEntryBead(ref, stateID, slot, draft, token)
		created, err := s.createEntry(entry)
		if err != nil {
			createErr := classifyTranscriptCreateError(err)
			outcome, recovered, rerr := s.recoverFailedCreate(ref, stateID, slot, draft, token)
			if rerr != nil {
				return ConversationTranscriptRecord{}, rerr
			}
			switch outcome {
			case transcriptCreateRecovered:
				return *recovered, nil
			case transcriptCreateLostSlot:
				// Another writer's entry took the slot and is now published:
				// retry at the live head.
				continue
			default:
				return ConversationTranscriptRecord{}, createErr
			}
		}
		if err := s.publishSlotLive(ref, stateID, slot); err != nil {
			return ConversationTranscriptRecord{}, err
		}
		return decodeTranscriptBead(created)
	}
	return ConversationTranscriptRecord{}, fmt.Errorf("%w: sequence slots exhausted for %s", errTranscriptRetryable, conversationLockKey(ref))
}

// buildEntryBead renders the drafted entry for slot. The bead carries the
// deterministic pinned ID and this invocation's append token.
func (s *transcriptService) buildEntryBead(ref ConversationRef, stateID string, sequence int64, draft transcriptAppendDraft, token string) beads.Bead {
	fields := encodeMetadataFields(draft.metadata, map[string]string{
		"schema_version":         strconv.Itoa(schemaVersion),
		"scope_id":               ref.ScopeID,
		"provider":               ref.Provider,
		"account_id":             ref.AccountID,
		"conversation_id":        ref.ConversationID,
		"parent_conversation_id": ref.ParentConversationID,
		"conversation_kind":      string(ref.Kind),
		"sequence":               strconv.FormatInt(sequence, 10),
		"kind":                   string(draft.kind),
		"provenance":             string(draft.provenance),
		"provider_message_id":    draft.providerMessageID,
		"explicit_target":        normalizeHandle(draft.explicitTarget),
		"reply_to_message_id":    strings.TrimSpace(draft.replyToMessageID),
		"source_session_id":      strings.TrimSpace(draft.sourceSessionID),
		"created_at":             formatTime(draft.createdAt),
		"actor_json":             draft.actorJSON,
		"attachments_json":       draft.attachmentsJSON,
		metadataKeyAppendToken:   token,
	})
	labels := []string{
		labelTranscriptBase,
		transcriptConversationLabel(ref),
		transcriptBucketLabel(ref, transcriptBucket(sequence)),
	}
	if draft.providerMessageID != "" {
		labels = append(labels, transcriptProviderMessageLabel(ref, draft.providerMessageID))
	}
	return beads.Bead{
		ID:          transcriptEntryID(stateID, sequence),
		Title:       fmt.Sprintf("%s#%d", conversationTitle(ref), sequence),
		Type:        "task",
		Description: draft.text,
		Labels:      append([]string{"gc:extmsg-transcript"}, labels...),
		Metadata:    fields,
	}
}

// transcriptCreateOutcome classifies the recovery of a failed entry create.
type transcriptCreateOutcome int

const (
	// transcriptCreatePending: the create did not commit (or the store
	// minted a different ID, which the next append's occupancy check
	// adopts); the caller may retry, and the create error is returned.
	transcriptCreatePending transcriptCreateOutcome = iota
	// transcriptCreateRecovered: this append's entry committed and is now
	// published; the stored record is the append's result.
	transcriptCreateRecovered
	// transcriptCreateLostSlot: another writer's entry committed at the
	// slot and is now published; the append retries at the live head.
	transcriptCreateLostSlot
)

// createEntry persists the drafted entry, pinning its deterministic ID when
// the store offers the capability and falling back to the ordinary create
// otherwise. The fallback preserves the pre-capability behavior for plain
// wrappers; a store that pins IDs in its ordinary create (SQLite, bd) pins
// through it too.
func (s *transcriptService) createEntry(entry beads.Bead) (beads.Bead, error) {
	if creator, ok := beads.PinnedIDCreatorFor(s.store); ok {
		return creator.CreatePinned(entry)
	}
	return s.store.Create(entry)
}

// recoverFailedCreate decides whether a failed entry create nevertheless
// committed, by reading the slot's pinned ID authoritatively. A confirmed
// absence retains the create error. A committed entry is published either
// way, and the append reports success when the stored entry is ours — it
// carries this append's token or provider message — or lost the slot when
// another writer's entry took it.
func (s *transcriptService) recoverFailedCreate(ref ConversationRef, stateID string, slot int64, draft transcriptAppendDraft, token string) (transcriptCreateOutcome, *ConversationTranscriptRecord, error) {
	pinned := transcriptEntryID(stateID, slot)
	committed, err := s.getLiveBead(pinned)
	if err != nil {
		return 0, nil, fmt.Errorf("read transcript entry %s after create error: %w: %w", pinned, err, errTranscriptRetryable)
	}
	if committed.ID == "" {
		return transcriptCreatePending, nil, nil
	}
	occupant, verr := validateSlotOccupant(ref, committed, slot)
	if verr != nil {
		return 0, nil, verr
	}
	if err := s.publishSlotLive(ref, stateID, slot); err != nil {
		return 0, nil, err
	}
	ours := committed.Metadata[metadataKeyAppendToken] == token ||
		(draft.providerMessageID != "" && occupant.ProviderMessageID == draft.providerMessageID)
	if ours {
		return transcriptCreateRecovered, occupant, nil
	}
	return transcriptCreateLostSlot, nil, nil
}

// classifyTranscriptCreateError wraps an entry-create failure for the
// caller. Transport-class faults are retryable; errors a retry cannot fix —
// validation, invariant, capability and namespace refusals, cancellation —
// surface unchanged.
func classifyTranscriptCreateError(err error) error {
	fixed := []error{
		ErrInvalidInput,
		ErrInvalidCaller,
		ErrUnauthorized,
		ErrInvariantViolation,
		ErrHydrationPending,
		beads.ErrPinnedIDOutsideNamespace,
		beads.ErrConditionalWriteUnsupported,
		context.Canceled,
		context.DeadlineExceeded,
	}
	for _, sentinel := range fixed {
		if errors.Is(err, sentinel) {
			return fmt.Errorf("create transcript entry: %w", err)
		}
	}
	return fmt.Errorf("create transcript entry: %w: %w", err, errTranscriptRetryable)
}

// publishSlotLive advances the conversation head past slot, which must hold
// a committed entry. The advance is a metadata compare-and-set from the
// exact stored value, so the counter never regresses and a lost race is
// confirmed by re-read rather than assumed. Stores without the CAS
// capability (plain wrappers, test doubles) fall back to a guarded
// unconditional write, preserving the pre-CAS behavior for them.
func (s *transcriptService) publishSlotLive(ref ConversationRef, stateID string, slot int64) error {
	var lastErr error
	for attempt := 0; attempt < transcriptPublishAttempts; attempt++ {
		stateBead, state, err := s.readStateBeadLive(stateID)
		if err != nil {
			return err
		}
		if state.NextSequence > slot {
			// Already published, possibly further than slot.
			return s.ensureEarliestFloorLive(stateID, state)
		}
		next := strconv.FormatInt(slot+1, 10)
		if _, ok := beads.MetadataCASWriterFor(s.store); ok {
			outcome, err := beads.ApplyMetadataCAS(s.store, stateID, "next_sequence", stateBead.Metadata["next_sequence"], next)
			switch {
			case err != nil:
				// A committed CAS shows up as head > slot on the next read.
				lastErr = err
				continue
			case outcome == beads.MetadataCASSwapped || outcome == beads.MetadataCASAlreadyNext:
				return s.ensureEarliestFloorLive(stateID, state)
			default:
				// Conflict: another writer advanced first; re-read confirms.
				continue
			}
		}
		updates := map[string]string{"next_sequence": next}
		if state.EarliestAvailableSequence <= 0 {
			updates["earliest_available_sequence"] = "1"
		}
		if err := s.store.SetMetadataBatch(stateID, updates); err != nil {
			return fmt.Errorf("update transcript state: %w: %w", err, errTranscriptRetryable)
		}
		return s.ensureEarliestFloorLive(stateID, state)
	}
	if lastErr != nil {
		return fmt.Errorf("%w: publish sequence %d for %s: %w", errTranscriptRetryable, slot, conversationLockKey(ref), lastErr)
	}
	return fmt.Errorf("%w: publish sequence %d for %s", errTranscriptRetryable, slot, conversationLockKey(ref))
}

// ensureEarliestFloorLive sets the legacy initial retention floor on state
// beads that predate it. States created by this code carry the floor from
// the start, so this is a no-op for them.
func (s *transcriptService) ensureEarliestFloorLive(stateID string, state ConversationTranscriptStateRecord) error {
	if state.EarliestAvailableSequence > 0 {
		return nil
	}
	if err := s.store.SetMetadataBatch(stateID, map[string]string{"earliest_available_sequence": "1"}); err != nil {
		return fmt.Errorf("set transcript retention floor: %w: %w", err, errTranscriptRetryable)
	}
	return nil
}

// readStateBeadLive reads the transcript state bead and its raw metadata
// through the live handle, so another writer's publication is visible with
// or without a caching layer.
func (s *transcriptService) readStateBeadLive(stateID string) (beads.Bead, ConversationTranscriptStateRecord, error) {
	bead, err := beads.LiveFor(s.store).Get(stateID)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, ConversationTranscriptStateRecord{}, fmt.Errorf("%w: transcript state %s missing", ErrInvariantViolation, stateID)
		}
		return beads.Bead{}, ConversationTranscriptStateRecord{}, fmt.Errorf("read transcript state %s: %w: %w", stateID, err, errTranscriptRetryable)
	}
	state, err := decodeTranscriptStateBead(bead)
	if err != nil {
		return beads.Bead{}, ConversationTranscriptStateRecord{}, err
	}
	return bead, state, nil
}

// getLiveBead reads one bead through the live handle. A missing bead returns
// the zero Bead and no error, so callers distinguish absence themselves.
func (s *transcriptService) getLiveBead(id string) (beads.Bead, error) {
	bead, err := beads.LiveFor(s.store).Get(id)
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			return beads.Bead{}, nil
		}
		return beads.Bead{}, err
	}
	return bead, nil
}

// findSlotOccupantLive returns the entry committed at slot, if any, reading
// the bucket through the live handle and including closed entries (a closed
// occupant still owns its slot; readers skip it by status). More than one
// occupant at a slot means the store accepted duplicate sequences.
func (s *transcriptService) findSlotOccupantLive(ref ConversationRef, slot int64) (*ConversationTranscriptRecord, error) {
	items, err := beads.LiveFor(s.store).List(beads.ListQuery{
		Label:         transcriptBucketLabel(ref, transcriptBucket(slot)),
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list transcript bucket at sequence %d: %w: %w", slot, err, errTranscriptRetryable)
	}
	var out *ConversationTranscriptRecord
	for _, item := range items {
		if !hasLabel(item, "gc:extmsg-transcript") {
			continue
		}
		record, err := decodeTranscriptBead(item)
		if err != nil {
			return nil, err
		}
		if !sameConversationRef(record.Conversation, ref) || record.Sequence != slot {
			continue
		}
		if out != nil {
			return nil, fmt.Errorf("%w: duplicate transcript entries at sequence %d for %s", ErrInvariantViolation, slot, conversationLockKey(ref))
		}
		rec := record
		out = &rec
	}
	return out, nil
}

// validateSlotOccupant checks that a bead found at slot is a well-formed
// transcript entry of this conversation, or reports an invariant violation.
func validateSlotOccupant(ref ConversationRef, b beads.Bead, slot int64) (*ConversationTranscriptRecord, error) {
	if !hasLabel(b, "gc:extmsg-transcript") || !hasLabel(b, labelTranscriptBase) {
		return nil, fmt.Errorf("%w: bead %s at transcript slot %d is not a transcript entry", ErrInvariantViolation, b.ID, slot)
	}
	record, err := decodeTranscriptBead(b)
	if err != nil {
		return nil, fmt.Errorf("%w: transcript entry %s at slot %d: %w", ErrInvariantViolation, b.ID, slot, err)
	}
	if !sameConversationRef(record.Conversation, ref) {
		return nil, fmt.Errorf("%w: transcript entry %s at slot %d belongs to another conversation", ErrInvariantViolation, b.ID, slot)
	}
	if record.Sequence != slot {
		return nil, fmt.Errorf("%w: transcript entry %s carries sequence %d at slot %d", ErrInvariantViolation, b.ID, record.Sequence, slot)
	}
	return &record, nil
}

// findTranscriptByProviderMessageLive is the live-read form of
// findTranscriptByProviderMessageLocked, so a provider message committed by
// another writer is found even through a caching layer.
func (s *transcriptService) findTranscriptByProviderMessageLive(ref ConversationRef, providerMessageID string) (*ConversationTranscriptRecord, error) {
	items, err := beads.LiveFor(s.store).List(beads.ListQuery{Label: transcriptProviderMessageLabel(ref, providerMessageID)})
	if err != nil {
		return nil, fmt.Errorf("list transcript by provider message label: %w: %w", err, errTranscriptRetryable)
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
