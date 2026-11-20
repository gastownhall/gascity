package extmsg

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// guardedTranscriptPrefix is deliberately capability-based: a guarded native
// writer must also identify the namespace in which it accepts pinned creates.
// A read-only native leaf behind a CLI writer cannot satisfy this path.
func (s *transcriptService) guardedTranscriptPrefix() (string, beads.GuardedBatcher, bool) {
	batcher, ok := beads.GuardedBatcherFor(s.store)
	if !ok {
		return "", nil, false
	}
	prefixer, ok := beads.As[beads.IDPrefixReporter](s.store)
	if !ok || strings.TrimSpace(prefixer.IDPrefix()) == "" {
		return "", nil, false
	}
	return prefixer.IDPrefix(), batcher, true
}

// appendGuarded shares the public admission sequence transaction for ordinary
// outbound/hydrated appends. Without this, a concurrent outbound Append could
// allocate the public admission's sequence and later overwrite next_sequence.
func (s *transcriptService) appendGuarded(ctx context.Context, input AppendTranscriptInput, ref ConversationRef, kind TranscriptMessageKind, provenance TranscriptProvenance, createdAt time.Time, providerID, text, prefix string, batcher beads.GuardedBatcher) (ConversationTranscriptRecord, error) {
	identity := struct {
		Conversation             ConversationRef
		ProviderMessageID, Nonce string
	}{Conversation: ref, ProviderMessageID: providerID}
	if providerID == "" {
		identity.Nonce = rand.Text()
	}
	transcriptID := prefix + "-xt-" + publicHash(identity)[:32]
	for attempt := 0; attempt < 16; attempt++ {
		if err := ctx.Err(); err != nil {
			return ConversationTranscriptRecord{}, err
		}
		if providerID != "" {
			existing, err := s.findTranscriptByProviderMessageLocked(ref, providerID)
			if err != nil {
				return ConversationTranscriptRecord{}, err
			}
			if existing != nil {
				if provenance == TranscriptProvenanceLive && (kind == TranscriptMessageInbound || existing.Kind == TranscriptMessageInbound) && !sameInboundTranscriptPayload(*existing, input, ref, kind) {
					return ConversationTranscriptRecord{}, ErrMessageConflict
				}
				return *existing, nil
			}
		}
		stateItem, sequence, err := s.planGuardedTranscriptState(ref, provenance, prefix)
		if err != nil {
			return ConversationTranscriptRecord{}, err
		}
		actor, err := marshalOptionalJSON(input.Actor)
		if err != nil {
			return ConversationTranscriptRecord{}, err
		}
		attachments, err := marshalOptionalJSON(input.Attachments)
		if err != nil {
			return ConversationTranscriptRecord{}, err
		}
		labels := []string{"gc:extmsg-transcript", labelTranscriptBase, transcriptConversationLabel(ref), transcriptBucketLabel(ref, transcriptBucket(sequence))}
		if providerID != "" {
			labels = append(labels, transcriptProviderMessageLabel(ref, providerID))
		}
		row := beads.Bead{ID: transcriptID, Title: fmt.Sprintf("%s#%d", conversationTitle(ref), sequence), Type: "task", Status: "open", Description: text, Labels: labels, Metadata: encodeMetadataFields(input.Metadata, map[string]string{
			"schema_version": strconv.Itoa(schemaVersion), "scope_id": ref.ScopeID, "provider": ref.Provider, "account_id": ref.AccountID, "conversation_id": ref.ConversationID, "parent_conversation_id": ref.ParentConversationID, "conversation_kind": string(ref.Kind), "sequence": strconv.FormatInt(sequence, 10), "kind": string(kind), "provenance": string(provenance), "provider_message_id": providerID, "explicit_target": normalizeHandle(input.ExplicitTarget), "reply_to_message_id": strings.TrimSpace(input.ReplyToMessageID), "source_session_id": strings.TrimSpace(input.SourceSessionID), "created_at": formatTime(createdAt), "actor_json": actor, "attachments_json": attachments,
		})}
		_, writeErr := batcher.ApplyGuarded(ctx, beads.GuardedBatch{Reason: "append transcript and advance sequence", Items: []beads.GuardedItem{stateItem, {Create: &row}}})
		// A lost write acknowledgement must not append again, including an
		// outbound entry without a provider ID. The local pinned ID survives retries.
		stored, readErr := s.store.Get(transcriptID)
		if readErr == nil {
			existing, err := decodeTranscriptBead(stored)
			if err != nil {
				return ConversationTranscriptRecord{}, err
			}
			if provenance == TranscriptProvenanceLive && (kind == TranscriptMessageInbound || existing.Kind == TranscriptMessageInbound) && !sameInboundTranscriptPayload(existing, input, ref, kind) {
				return ConversationTranscriptRecord{}, ErrMessageConflict
			}
			return existing, nil
		}
		if !errors.Is(readErr, beads.ErrNotFound) {
			return ConversationTranscriptRecord{}, readErr
		}
		if writeErr == nil {
			return ConversationTranscriptRecord{}, fmt.Errorf("%w: transcript commit missing on readback", ErrInvariantViolation)
		}
		if errors.Is(writeErr, beads.ErrAlreadyExists) || beads.IsPreconditionFailed(writeErr) {
			continue
		}
		return ConversationTranscriptRecord{}, writeErr
	}
	return ConversationTranscriptRecord{}, fmt.Errorf("transcript append contention: %w", beads.ErrStoreUnavailable)
}

// planGuardedTranscriptState reads the exact revision that reserves sequence.
// Its deterministic first-state ID is shared by public admission and ordinary
// native transcript writers, so independent processes cannot create two states.
func (s *transcriptService) planGuardedTranscriptState(ref ConversationRef, provenance TranscriptProvenance, prefix string) (beads.GuardedItem, int64, error) {
	state, err := s.findStateLocked(ref)
	if err != nil {
		return beads.GuardedItem{}, 0, err
	}
	if state == nil {
		if provenance == TranscriptProvenanceHydrated {
			return beads.GuardedItem{}, 0, fmt.Errorf("%w: conversation is not pending hydration", ErrInvalidInput)
		}
		return beads.GuardedItem{Create: &beads.Bead{ID: prefix + "-xs-" + publicHash(ref)[:32], Title: conversationTitle(ref) + "/state", Type: "task", Status: "open", Labels: []string{"gc:extmsg-transcript-state", labelTranscriptStateBase, transcriptStateLabel(ref)}, Metadata: map[string]string{
			"schema_version": strconv.Itoa(schemaVersion), "scope_id": ref.ScopeID, "provider": ref.Provider, "account_id": ref.AccountID, "conversation_id": ref.ConversationID, "parent_conversation_id": ref.ParentConversationID, "conversation_kind": string(ref.Kind), "next_sequence": "2", "earliest_available_sequence": "1", "hydration_status": string(HydrationLiveOnly), "max_retained_entries": "0",
		}}}, 1, nil
	}
	row, err := s.store.Get(state.ID)
	if err != nil {
		return beads.GuardedItem{}, 0, err
	}
	current, err := decodeTranscriptStateBead(row)
	if err != nil {
		return beads.GuardedItem{}, 0, err
	}
	if provenance == TranscriptProvenanceHydrated {
		if current.HydrationStatus != HydrationPending {
			return beads.GuardedItem{}, 0, fmt.Errorf("%w: conversation is not pending hydration", ErrInvalidInput)
		}
	} else if current.HydrationStatus == HydrationPending {
		return beads.GuardedItem{}, 0, ErrHydrationPending
	}
	fields := map[string]string{"next_sequence": strconv.FormatInt(current.NextSequence+1, 10)}
	if current.EarliestAvailableSequence <= 0 {
		fields["earliest_available_sequence"] = "1"
	}
	return beads.GuardedItem{Update: &beads.GuardedUpdate{ID: row.ID, ExpectedRevision: &row.Revision, Opts: beads.UpdateOpts{Metadata: fields}}}, current.NextSequence, nil
}
