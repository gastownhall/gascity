package extmsg

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	transcriptBucketSize      = int64(64)
	defaultTranscriptPageSize = 100
	maxTranscriptPageSize     = 500
)

type transcriptService struct {
	store beads.Store
	locks *bindingLockPool
}

func newTranscriptService(store beads.Store, locks *bindingLockPool) *transcriptService {
	return &transcriptService{store: store, locks: locks}
}

func (s *transcriptService) Append(ctx context.Context, input AppendTranscriptInput) (ConversationTranscriptRecord, error) {
	if err := checkContext(ctx); err != nil {
		return ConversationTranscriptRecord{}, err
	}
	ref, err := validateConversationRef(input.Conversation)
	if err != nil {
		return ConversationTranscriptRecord{}, err
	}
	if err := authorizeMutation(input.Caller, ref); err != nil {
		return ConversationTranscriptRecord{}, err
	}
	kind := TranscriptMessageKind(strings.ToLower(strings.TrimSpace(string(input.Kind))))
	switch kind {
	case TranscriptMessageInbound, TranscriptMessageOutbound:
	default:
		return ConversationTranscriptRecord{}, fmt.Errorf("%w: invalid transcript kind %q", ErrInvalidInput, input.Kind)
	}
	provenance := TranscriptProvenance(strings.ToLower(strings.TrimSpace(string(input.Provenance))))
	switch provenance {
	case "":
		provenance = TranscriptProvenanceLive
	case TranscriptProvenanceLive, TranscriptProvenanceHydrated:
	default:
		return ConversationTranscriptRecord{}, fmt.Errorf("%w: invalid provenance %q", ErrInvalidInput, input.Provenance)
	}
	createdAt := zeroNow(input.CreatedAt)
	providerMessageID := strings.TrimSpace(input.ProviderMessageID)
	text := strings.TrimSpace(input.Text)
	var out ConversationTranscriptRecord
	err = withBindingLock(s.locks, ref, func() error {
		state, err := s.ensureStateLocked(ref)
		if err != nil {
			return err
		}
		if provenance == TranscriptProvenanceHydrated {
			if state.HydrationStatus != HydrationPending {
				return fmt.Errorf("%w: conversation is not pending hydration", ErrInvalidInput)
			}
		} else if state.HydrationStatus == HydrationPending {
			return ErrHydrationPending
		}
		if providerMessageID != "" {
			existing, err := s.findTranscriptByProviderMessageLocked(ref, providerMessageID)
			if err != nil {
				return err
			}
			if existing != nil {
				out = *existing
				return nil
			}
		}
		actorJSON, err := marshalOptionalJSON(input.Actor)
		if err != nil {
			return err
		}
		attachmentsJSON, err := marshalOptionalJSON(input.Attachments)
		if err != nil {
			return err
		}
		sequence := state.NextSequence
		fields := encodeMetadataFields(input.Metadata, map[string]string{
			"schema_version":         strconv.Itoa(schemaVersion),
			"scope_id":               ref.ScopeID,
			"provider":               ref.Provider,
			"account_id":             ref.AccountID,
			"conversation_id":        ref.ConversationID,
			"parent_conversation_id": ref.ParentConversationID,
			"conversation_kind":      string(ref.Kind),
			"sequence":               strconv.FormatInt(sequence, 10),
			"kind":                   string(kind),
			"provenance":             string(provenance),
			"provider_message_id":    providerMessageID,
			"explicit_target":        normalizeHandle(input.ExplicitTarget),
			"reply_to_message_id":    strings.TrimSpace(input.ReplyToMessageID),
			"source_session_id":      strings.TrimSpace(input.SourceSessionID),
			"created_at":             formatTime(createdAt),
			"actor_json":             actorJSON,
			"attachments_json":       attachmentsJSON,
		})
		labels := []string{
			labelTranscriptBase,
			transcriptConversationLabel(ref),
			transcriptBucketLabel(ref, transcriptBucket(sequence)),
		}
		if providerMessageID != "" {
			labels = append(labels, transcriptProviderMessageLabel(ref, providerMessageID))
		}
		created, err := s.store.Create(beads.Bead{
			Title:       fmt.Sprintf("%s#%d", conversationTitle(ref), sequence),
			Type:        "task",
			Description: text,
			Labels:      append([]string{"gc:extmsg-transcript"}, labels...),
			Metadata:    fields,
		})
		if err != nil {
			return fmt.Errorf("create transcript entry: %w", err)
		}
		next := sequence + 1
		updates := map[string]string{
			"next_sequence": strconv.FormatInt(next, 10),
		}
		if state.EarliestAvailableSequence <= 0 {
			updates["earliest_available_sequence"] = "1"
		}
		if err := s.store.SetMetadataBatch(state.ID, updates); err != nil {
			return fmt.Errorf("update transcript state: %w", err)
		}
		out, err = decodeTranscriptBead(created)
		return err
	})
	return out, err
}

func (s *transcriptService) List(ctx context.Context, input ListTranscriptInput) ([]ConversationTranscriptRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := requireControllerCaller(input.Caller); err != nil {
		return nil, err
	}
	ref, err := validateConversationRef(input.Conversation)
	if err != nil {
		return nil, err
	}
	after := input.AfterSequence
	if after < 0 {
		after = 0
	}
	limit := clampTranscriptLimit(input.Limit)
	var out []ConversationTranscriptRecord
	err = withBindingLock(s.locks, ref, func() error {
		state, err := s.findStateLocked(ref)
		if err != nil {
			return err
		}
		if state == nil || state.NextSequence <= 1 {
			out = nil
			return nil
		}
		out, err = s.listTranscriptLocked(ref, after, limit)
		return err
	})
	return out, err
}

func (s *transcriptService) EnsureMembership(ctx context.Context, input EnsureMembershipInput) (ConversationMembershipRecord, error) {
	if err := checkContext(ctx); err != nil {
		return ConversationMembershipRecord{}, err
	}
	if err := requireControllerCaller(input.Caller); err != nil {
		return ConversationMembershipRecord{}, err
	}
	ref, err := validateConversationRef(input.Conversation)
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return ConversationMembershipRecord{}, fmt.Errorf("%w: session_id required", ErrInvalidInput)
	}
	policy, err := normalizeBackfillPolicy(input.BackfillPolicy)
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	owner, err := normalizeMembershipOwner(input.Owner)
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	now := zeroNow(input.Now)
	var out ConversationMembershipRecord
	err = withBindingLock(s.locks, ref, func() error {
		var lockedErr error
		out, lockedErr = s.ensureMembershipLocked(EnsureMembershipInput{
			Caller:         input.Caller,
			Conversation:   ref,
			SessionID:      sessionID,
			BackfillPolicy: policy,
			Owner:          owner,
			Metadata:       input.Metadata,
			Now:            now,
		})
		return lockedErr
	})
	return out, err
}

func (s *transcriptService) UpdateMembership(ctx context.Context, input UpdateMembershipInput) (ConversationMembershipRecord, error) {
	if err := checkContext(ctx); err != nil {
		return ConversationMembershipRecord{}, err
	}
	if err := requireControllerCaller(input.Caller); err != nil {
		return ConversationMembershipRecord{}, err
	}
	ref, err := validateConversationRef(input.Conversation)
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return ConversationMembershipRecord{}, fmt.Errorf("%w: session_id required", ErrInvalidInput)
	}
	policy, err := normalizeBackfillPolicy(input.BackfillPolicy)
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	var out ConversationMembershipRecord
	err = withBindingLock(s.locks, ref, func() error {
		existing, err := s.findActiveMembershipLocked(ref, sessionID)
		if err != nil {
			return err
		}
		if existing == nil {
			return ErrMembershipNotFound
		}
		owners, _ := addMembershipOwner(existing.Owners, MembershipOwnerManual)
		fields := encodeMetadataFields(input.Metadata, map[string]string{
			"membership_backfill_policy": string(effectiveMembershipBackfillPolicy(owners, policy)),
			"manual_backfill_policy":     string(policy),
			"membership_owner_kinds":     encodeMembershipOwners(owners),
		})
		if err := s.store.SetMetadataBatch(existing.ID, fields); err != nil {
			return fmt.Errorf("update membership metadata: %w", err)
		}
		updated, err := s.store.Get(existing.ID)
		if err != nil {
			return fmt.Errorf("get membership %s: %w", existing.ID, err)
		}
		out, err = decodeMembershipBead(updated)
		return err
	})
	return out, err
}

func (s *transcriptService) RemoveMembership(ctx context.Context, input RemoveMembershipInput) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := requireControllerCaller(input.Caller); err != nil {
		return err
	}
	ref, err := validateConversationRef(input.Conversation)
	if err != nil {
		return err
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return fmt.Errorf("%w: session_id required", ErrInvalidInput)
	}
	owner, err := normalizeMembershipOwner(input.Owner)
	if err != nil {
		return err
	}
	now := zeroNow(input.Now)
	return withBindingLock(s.locks, ref, func() error {
		return s.removeMembershipLocked(RemoveMembershipInput{
			Caller:       input.Caller,
			Conversation: ref,
			SessionID:    sessionID,
			Owner:        owner,
			Now:          now,
		})
	})
}

func (s *transcriptService) ensureMembershipLocked(input EnsureMembershipInput) (ConversationMembershipRecord, error) {
	ref, err := validateConversationRef(input.Conversation)
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return ConversationMembershipRecord{}, fmt.Errorf("%w: session_id required", ErrInvalidInput)
	}
	policy, err := normalizeBackfillPolicy(input.BackfillPolicy)
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	owner, err := normalizeMembershipOwner(input.Owner)
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	now := zeroNow(input.Now)
	state, err := s.ensureStateLocked(ref)
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	existing, err := s.findActiveMembershipLocked(ref, sessionID)
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	if existing != nil {
		owners, changed := addMembershipOwner(existing.Owners, owner)
		manualPolicy := existing.ManualBackfill
		if owner == MembershipOwnerManual {
			manualPolicy = policy
		}
		effectivePolicy := effectiveMembershipBackfillPolicy(owners, manualPolicy)
		if !changed && effectivePolicy == existing.BackfillPolicy {
			return *existing, nil
		}
		fields := map[string]string{}
		if changed {
			fields["membership_owner_kinds"] = encodeMembershipOwners(owners)
		}
		if owner == MembershipOwnerManual && manualPolicy != existing.ManualBackfill {
			fields["manual_backfill_policy"] = string(manualPolicy)
		}
		if effectivePolicy != existing.BackfillPolicy {
			fields["membership_backfill_policy"] = string(effectivePolicy)
		}
		if err := s.store.SetMetadataBatch(existing.ID, fields); err != nil {
			return ConversationMembershipRecord{}, fmt.Errorf("update membership owners: %w", err)
		}
		updated, err := s.store.Get(existing.ID)
		if err != nil {
			return ConversationMembershipRecord{}, fmt.Errorf("get membership %s: %w", existing.ID, err)
		}
		return decodeMembershipBead(updated)
	}
	joinedSequence := state.NextSequence - 1
	if joinedSequence < 0 {
		joinedSequence = 0
	}
	fields := encodeMetadataFields(input.Metadata, map[string]string{
		"schema_version":             strconv.Itoa(schemaVersion),
		"scope_id":                   ref.ScopeID,
		"provider":                   ref.Provider,
		"account_id":                 ref.AccountID,
		"conversation_id":            ref.ConversationID,
		"parent_conversation_id":     ref.ParentConversationID,
		"conversation_kind":          string(ref.Kind),
		"session_id":                 sessionID,
		"joined_at":                  formatTime(now),
		"joined_sequence":            strconv.FormatInt(joinedSequence, 10),
		"last_read_sequence":         "0",
		"membership_backfill_policy": string(effectiveMembershipBackfillPolicy([]MembershipOwner{owner}, policy)),
		"manual_backfill_policy":     manualBackfillMetadataValue(owner, policy),
		"membership_owner_kinds":     encodeMembershipOwners([]MembershipOwner{owner}),
	})
	created, err := s.store.Create(beads.Bead{
		Title:    sessionID + " -> " + conversationTitle(ref),
		Type:     "task",
		Labels:   []string{"gc:extmsg-membership", labelMembershipBase, membershipConversationLabel(ref), membershipExactLabel(ref, sessionID), membershipSessionLabel(sessionID)},
		Metadata: fields,
	})
	if err != nil {
		return ConversationMembershipRecord{}, fmt.Errorf("create membership: %w", err)
	}
	return decodeMembershipBead(created)
}

func (s *transcriptService) removeMembershipLocked(input RemoveMembershipInput) error {
	ref, err := validateConversationRef(input.Conversation)
	if err != nil {
		return err
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return fmt.Errorf("%w: session_id required", ErrInvalidInput)
	}
	owner, err := normalizeMembershipOwner(input.Owner)
	if err != nil {
		return err
	}
	now := zeroNow(input.Now)
	existing, err := s.findActiveMembershipLocked(ref, sessionID)
	if err != nil {
		return err
	}
	if existing == nil {
		return nil
	}
	owners := existing.Owners
	if len(owners) == 0 {
		owners = []MembershipOwner{owner}
	}
	nextOwners, changed := removeMembershipOwner(owners, owner)
	if !changed {
		return nil
	}
	if len(nextOwners) > 0 {
		fields := map[string]string{
			"membership_owner_kinds": encodeMembershipOwners(nextOwners),
		}
		nextManualPolicy := existing.ManualBackfill
		if owner == MembershipOwnerManual {
			nextManualPolicy = ""
			fields["manual_backfill_policy"] = ""
		}
		nextPolicy := effectiveMembershipBackfillPolicy(nextOwners, nextManualPolicy)
		if nextPolicy != existing.BackfillPolicy {
			fields["membership_backfill_policy"] = string(nextPolicy)
		}
		if err := s.store.SetMetadataBatch(existing.ID, fields); err != nil {
			return fmt.Errorf("update membership owners: %w", err)
		}
		return nil
	}
	if err := s.store.SetMetadata(existing.ID, "closed_at", formatTime(now)); err != nil {
		return fmt.Errorf("set membership closed_at: %w", err)
	}
	if err := s.store.Close(existing.ID); err != nil {
		return fmt.Errorf("close membership %s: %w", existing.ID, err)
	}
	return nil
}

func (s *transcriptService) ListMemberships(ctx context.Context, caller Caller, ref ConversationRef) ([]ConversationMembershipRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := requireControllerCaller(caller); err != nil {
		return nil, err
	}
	ref, err := validateConversationRef(ref)
	if err != nil {
		return nil, err
	}
	items, err := s.store.List(beads.ListQuery{Label: membershipConversationLabel(ref)})
	if err != nil {
		return nil, fmt.Errorf("list memberships by conversation label: %w", err)
	}
	out := make([]ConversationMembershipRecord, 0, len(items))
	seen := make(map[string]ConversationMembershipRecord)
	for _, item := range items {
		if !hasLabel(item, "gc:extmsg-membership") || item.Status == "closed" {
			continue
		}
		record, err := decodeMembershipBead(item)
		if err != nil {
			return nil, err
		}
		if !sameConversationRef(record.Conversation, ref) {
			continue
		}
		if existing, ok := seen[record.SessionID]; ok {
			return nil, fmt.Errorf("%w: duplicate memberships for session %s (%s, %s)", ErrInvariantViolation, record.SessionID, existing.ID, record.ID)
		}
		seen[record.SessionID] = record
		out = append(out, record)
	}
	slices.SortFunc(out, func(a, b ConversationMembershipRecord) int {
		return strings.Compare(a.SessionID, b.SessionID)
	})
	return out, nil
}

func (s *transcriptService) ListConversationsBySession(ctx context.Context, caller Caller, sessionID string) ([]ConversationMembershipRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := requireControllerCaller(caller); err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	items, err := s.store.List(beads.ListQuery{Label: membershipSessionLabel(sessionID)})
	if err != nil {
		return nil, fmt.Errorf("list memberships by session label: %w", err)
	}
	out := make([]ConversationMembershipRecord, 0, len(items))
	seen := make(map[string]bool)
	for _, item := range items {
		if !hasLabel(item, "gc:extmsg-membership") || item.Status == "closed" {
			continue
		}
		record, err := decodeMembershipBead(item)
		if err != nil {
			return nil, err
		}
		if record.SessionID != sessionID {
			continue
		}
		key := conversationLockKey(record.Conversation)
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, record)
	}
	sortConversationRefsFromMemberships(out)
	return out, nil
}

func (s *transcriptService) ListBackfill(ctx context.Context, input ListBackfillInput) ([]ConversationTranscriptRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	if err := requireControllerCaller(input.Caller); err != nil {
		return nil, err
	}
	ref, err := validateConversationRef(input.Conversation)
	if err != nil {
		return nil, err
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session_id required", ErrInvalidInput)
	}
	limit := clampTranscriptLimit(input.Limit)
	var out []ConversationTranscriptRecord
	err = withBindingLock(s.locks, ref, func() error {
		state, err := s.ensureStateLocked(ref)
		if err != nil {
			return err
		}
		if state.HydrationStatus == HydrationPending {
			return ErrHydrationPending
		}
		membership, err := s.findActiveMembershipLocked(ref, sessionID)
		if err != nil {
			return err
		}
		if membership == nil {
			return ErrMembershipNotFound
		}
		after := membership.LastReadSequence
		if after == 0 && membership.BackfillPolicy == MembershipBackfillSinceJoin {
			after = membership.JoinedSequence
		}
		out, err = s.listTranscriptLocked(ref, after, limit)
		return err
	})
	return out, err
}

func (s *transcriptService) Ack(ctx context.Context, input AckMembershipInput) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if err := requireControllerCaller(input.Caller); err != nil {
		return err
	}
	ref, err := validateConversationRef(input.Conversation)
	if err != nil {
		return err
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return fmt.Errorf("%w: session_id required", ErrInvalidInput)
	}
	if input.Sequence < 0 {
		return fmt.Errorf("%w: sequence must be non-negative", ErrInvalidInput)
	}
	return withBindingLock(s.locks, ref, func() error {
		state, err := s.ensureStateLocked(ref)
		if err != nil {
			return err
		}
		maxSequence := state.NextSequence - 1
		if input.Sequence > maxSequence {
			return fmt.Errorf("%w: ack sequence %d exceeds head %d", ErrInvalidInput, input.Sequence, maxSequence)
		}
		membership, err := s.findActiveMembershipLocked(ref, sessionID)
		if err != nil {
			return err
		}
		if membership == nil {
			return ErrMembershipNotFound
		}
		if input.Sequence <= membership.LastReadSequence {
			return nil
		}
		return s.store.SetMetadata(membership.ID, "last_read_sequence", strconv.FormatInt(input.Sequence, 10))
	})
}

func (s *transcriptService) BeginHydration(ctx context.Context, caller Caller, ref ConversationRef, metadata map[string]string) (ConversationTranscriptStateRecord, error) {
	if err := checkContext(ctx); err != nil {
		return ConversationTranscriptStateRecord{}, err
	}
	ref, err := validateConversationRef(ref)
	if err != nil {
		return ConversationTranscriptStateRecord{}, err
	}
	if err := authorizeMutation(caller, ref); err != nil {
		return ConversationTranscriptStateRecord{}, err
	}
	var out ConversationTranscriptStateRecord
	err = withBindingLock(s.locks, ref, func() error {
		state, err := s.ensureStateLocked(ref)
		if err != nil {
			return err
		}
		if state.NextSequence > 1 && state.HydrationStatus != HydrationPending {
			return fmt.Errorf("%w: cannot begin hydration after live traffic", ErrInvalidInput)
		}
		fields := encodeMetadataFields(metadata, map[string]string{
			"hydration_status": string(HydrationPending),
		})
		if err := s.store.SetMetadataBatch(state.ID, fields); err != nil {
			return fmt.Errorf("update hydration state: %w", err)
		}
		updated, err := s.store.Get(state.ID)
		if err != nil {
			return fmt.Errorf("get state %s: %w", state.ID, err)
		}
		out, err = decodeTranscriptStateBead(updated)
		return err
	})
	return out, err
}

func (s *transcriptService) CompleteHydration(ctx context.Context, caller Caller, ref ConversationRef) (ConversationTranscriptStateRecord, error) {
	return s.updateHydrationState(ctx, caller, ref, HydrationComplete, nil)
}

func (s *transcriptService) MarkHydrationFailed(ctx context.Context, caller Caller, ref ConversationRef, metadata map[string]string) (ConversationTranscriptStateRecord, error) {
	return s.updateHydrationState(ctx, caller, ref, HydrationFailed, metadata)
}

func (s *transcriptService) State(ctx context.Context, caller Caller, ref ConversationRef) (*ConversationTranscriptStateRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	ref, err := validateConversationRef(ref)
	if err != nil {
		return nil, err
	}
	if err := authorizeMutation(caller, ref); err != nil {
		return nil, err
	}
	var out *ConversationTranscriptStateRecord
	err = withBindingLock(s.locks, ref, func() error {
		state, err := s.findStateLocked(ref)
		if err != nil {
			return err
		}
		if state != nil {
			rec := *state
			out = &rec
		}
		return nil
	})
	return out, err
}

func (s *transcriptService) updateHydrationState(ctx context.Context, caller Caller, ref ConversationRef, status HydrationStatus, metadata map[string]string) (ConversationTranscriptStateRecord, error) {
	if err := checkContext(ctx); err != nil {
		return ConversationTranscriptStateRecord{}, err
	}
	switch status {
	case HydrationComplete, HydrationFailed:
	default:
		return ConversationTranscriptStateRecord{}, fmt.Errorf("%w: invalid hydration transition %q", ErrInvalidInput, status)
	}
	ref, err := validateConversationRef(ref)
	if err != nil {
		return ConversationTranscriptStateRecord{}, err
	}
	if err := authorizeMutation(caller, ref); err != nil {
		return ConversationTranscriptStateRecord{}, err
	}
	var out ConversationTranscriptStateRecord
	err = withBindingLock(s.locks, ref, func() error {
		state, err := s.ensureStateLocked(ref)
		if err != nil {
			return err
		}
		if state.HydrationStatus != HydrationPending {
			return fmt.Errorf("%w: hydration transition requires pending status", ErrInvalidInput)
		}
		fields := encodeMetadataFields(metadata, map[string]string{
			"hydration_status": string(status),
		})
		if err := s.store.SetMetadataBatch(state.ID, fields); err != nil {
			return fmt.Errorf("update hydration state: %w", err)
		}
		updated, err := s.store.Get(state.ID)
		if err != nil {
			return fmt.Errorf("get state %s: %w", state.ID, err)
		}
		out, err = decodeTranscriptStateBead(updated)
		return err
	})
	return out, err
}

func (s *transcriptService) ensureStateLocked(ref ConversationRef) (ConversationTranscriptStateRecord, error) {
	state, err := s.findStateLocked(ref)
	if err != nil {
		return ConversationTranscriptStateRecord{}, err
	}
	if state != nil {
		return *state, nil
	}
	fields := map[string]string{
		"schema_version":              strconv.Itoa(schemaVersion),
		"scope_id":                    ref.ScopeID,
		"provider":                    ref.Provider,
		"account_id":                  ref.AccountID,
		"conversation_id":             ref.ConversationID,
		"parent_conversation_id":      ref.ParentConversationID,
		"conversation_kind":           string(ref.Kind),
		"next_sequence":               "1",
		"earliest_available_sequence": "1",
		"hydration_status":            string(HydrationLiveOnly),
		"max_retained_entries":        "0",
	}
	created, err := s.store.Create(beads.Bead{
		Title:    conversationTitle(ref) + "/state",
		Type:     "task",
		Labels:   []string{"gc:extmsg-transcript-state", labelTranscriptStateBase, transcriptStateLabel(ref)},
		Metadata: fields,
	})
	if err != nil {
		return ConversationTranscriptStateRecord{}, fmt.Errorf("create transcript state: %w", err)
	}
	return decodeTranscriptStateBead(created)
}

func (s *transcriptService) findStateLocked(ref ConversationRef) (*ConversationTranscriptStateRecord, error) {
	items, err := s.store.List(beads.ListQuery{Label: transcriptStateLabel(ref)})
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

func (s *transcriptService) findTranscriptByProviderMessageLocked(ref ConversationRef, providerMessageID string) (*ConversationTranscriptRecord, error) {
	items, err := s.store.List(beads.ListQuery{Label: transcriptProviderMessageLabel(ref, providerMessageID)})
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

func (s *transcriptService) findActiveMembershipLocked(ref ConversationRef, sessionID string) (*ConversationMembershipRecord, error) {
	items, err := s.store.List(beads.ListQuery{Label: membershipExactLabel(ref, sessionID)})
	if err != nil {
		return nil, fmt.Errorf("list membership by exact label: %w", err)
	}
	var out *ConversationMembershipRecord
	for _, item := range items {
		if !hasLabel(item, "gc:extmsg-membership") || item.Status == "closed" {
			continue
		}
		record, err := decodeMembershipBead(item)
		if err != nil {
			return nil, err
		}
		if !sameConversationRef(record.Conversation, ref) || record.SessionID != sessionID {
			continue
		}
		if out != nil {
			return nil, fmt.Errorf("%w: duplicate memberships for session %s", ErrInvariantViolation, sessionID)
		}
		rec := record
		out = &rec
	}
	return out, nil
}

func (s *transcriptService) listTranscriptLocked(ref ConversationRef, after int64, limit int) ([]ConversationTranscriptRecord, error) {
	state, err := s.ensureStateLocked(ref)
	if err != nil {
		return nil, err
	}
	if limit <= 0 {
		return nil, nil
	}
	startSeq := after + 1
	if startSeq < state.EarliestAvailableSequence {
		startSeq = state.EarliestAvailableSequence
	}
	endSeq := state.NextSequence - 1
	if startSeq > endSeq {
		return nil, nil
	}
	startBucket := transcriptBucket(startSeq)
	endBucket := transcriptBucket(endSeq)
	records := make([]ConversationTranscriptRecord, 0, limit)
	for bucket := startBucket; bucket <= endBucket && len(records) < limit; bucket++ {
		items, err := s.store.List(beads.ListQuery{Label: transcriptBucketLabel(ref, bucket)})
		if err != nil {
			return nil, fmt.Errorf("list transcript bucket %d: %w", bucket, err)
		}
		bucketRecords := make([]ConversationTranscriptRecord, 0, len(items))
		for _, item := range items {
			if !hasLabel(item, "gc:extmsg-transcript") || item.Status == "closed" {
				continue
			}
			record, err := decodeTranscriptBead(item)
			if err != nil {
				return nil, err
			}
			if !sameConversationRef(record.Conversation, ref) || record.Sequence <= after {
				continue
			}
			bucketRecords = append(bucketRecords, record)
		}
		slices.SortFunc(bucketRecords, func(a, b ConversationTranscriptRecord) int {
			switch {
			case a.Sequence < b.Sequence:
				return -1
			case a.Sequence > b.Sequence:
				return 1
			default:
				return strings.Compare(a.ID, b.ID)
			}
		})
		for _, record := range bucketRecords {
			if len(records) >= limit {
				break
			}
			records = append(records, record)
		}
	}
	return records, nil
}

func decodeTranscriptBead(b beads.Bead) (ConversationTranscriptRecord, error) {
	ref, err := conversationRefFromMetadata(b.Metadata)
	if err != nil {
		return ConversationTranscriptRecord{}, err
	}
	createdAt, err := parseTime(b.Metadata, "created_at")
	if err != nil {
		return ConversationTranscriptRecord{}, err
	}
	actor, err := decodeOptionalActor(b.Metadata["actor_json"])
	if err != nil {
		return ConversationTranscriptRecord{}, err
	}
	attachments, err := decodeOptionalAttachments(b.Metadata["attachments_json"])
	if err != nil {
		return ConversationTranscriptRecord{}, err
	}
	return ConversationTranscriptRecord{
		ID:                b.ID,
		SchemaVersion:     parseInt(b.Metadata, "schema_version"),
		Conversation:      ref,
		Sequence:          parseInt64(b.Metadata, "sequence"),
		Kind:              TranscriptMessageKind(strings.TrimSpace(b.Metadata["kind"])),
		Provenance:        TranscriptProvenance(strings.TrimSpace(b.Metadata["provenance"])),
		ProviderMessageID: strings.TrimSpace(b.Metadata["provider_message_id"]),
		Actor:             actor,
		Text:              b.Description,
		ExplicitTarget:    normalizeHandle(b.Metadata["explicit_target"]),
		ReplyToMessageID:  strings.TrimSpace(b.Metadata["reply_to_message_id"]),
		Attachments:       attachments,
		SourceSessionID:   strings.TrimSpace(b.Metadata["source_session_id"]),
		CreatedAt:         createdAt,
		Metadata:          decodePrefixedMetadata(b.Metadata),
	}, nil
}

func decodeMembershipBead(b beads.Bead) (ConversationMembershipRecord, error) {
	ref, err := conversationRefFromMetadata(b.Metadata)
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	joinedAt, err := parseTime(b.Metadata, "joined_at")
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	owners, err := decodeMembershipOwners(b.Metadata["membership_owner_kinds"])
	if err != nil {
		return ConversationMembershipRecord{}, err
	}
	return ConversationMembershipRecord{
		ID:               b.ID,
		SchemaVersion:    parseInt(b.Metadata, "schema_version"),
		Conversation:     ref,
		SessionID:        strings.TrimSpace(b.Metadata["session_id"]),
		JoinedAt:         joinedAt,
		JoinedSequence:   parseInt64(b.Metadata, "joined_sequence"),
		LastReadSequence: parseInt64(b.Metadata, "last_read_sequence"),
		BackfillPolicy:   MembershipBackfillPolicy(strings.TrimSpace(b.Metadata["membership_backfill_policy"])),
		ManualBackfill:   MembershipBackfillPolicy(strings.TrimSpace(b.Metadata["manual_backfill_policy"])),
		Owners:           owners,
		Metadata:         decodePrefixedMetadata(b.Metadata),
	}, nil
}

func decodeTranscriptStateBead(b beads.Bead) (ConversationTranscriptStateRecord, error) {
	ref, err := conversationRefFromMetadata(b.Metadata)
	if err != nil {
		return ConversationTranscriptStateRecord{}, err
	}
	return ConversationTranscriptStateRecord{
		ID:                        b.ID,
		SchemaVersion:             parseInt(b.Metadata, "schema_version"),
		Conversation:              ref,
		NextSequence:              parseInt64(b.Metadata, "next_sequence"),
		EarliestAvailableSequence: parseInt64(b.Metadata, "earliest_available_sequence"),
		HydrationStatus:           HydrationStatus(strings.TrimSpace(b.Metadata["hydration_status"])),
		OldestHydratedMessageID:   strings.TrimSpace(b.Metadata["oldest_hydrated_message_id"]),
		MaxRetainedEntries:        parseInt(b.Metadata, "max_retained_entries"),
		Metadata:                  decodePrefixedMetadata(b.Metadata),
	}, nil
}

func normalizeBackfillPolicy(policy MembershipBackfillPolicy) (MembershipBackfillPolicy, error) {
	switch MembershipBackfillPolicy(strings.ToLower(strings.TrimSpace(string(policy)))) {
	case "", MembershipBackfillAll:
		return MembershipBackfillAll, nil
	case MembershipBackfillSinceJoin:
		return MembershipBackfillSinceJoin, nil
	default:
		return "", fmt.Errorf("%w: invalid backfill policy %q", ErrInvalidInput, policy)
	}
}

func normalizeMembershipOwner(owner MembershipOwner) (MembershipOwner, error) {
	switch MembershipOwner(strings.ToLower(strings.TrimSpace(string(owner)))) {
	case "", MembershipOwnerManual:
		return MembershipOwnerManual, nil
	case MembershipOwnerBinding:
		return MembershipOwnerBinding, nil
	case MembershipOwnerGroup:
		return MembershipOwnerGroup, nil
	default:
		return "", fmt.Errorf("%w: invalid membership owner %q", ErrInvalidInput, owner)
	}
}

func decodeMembershipOwners(raw string) ([]MembershipOwner, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	parts := strings.Split(raw, ",")
	owners := make([]MembershipOwner, 0, len(parts))
	seen := make(map[MembershipOwner]struct{}, len(parts))
	for _, part := range parts {
		owner, err := normalizeMembershipOwner(MembershipOwner(part))
		if err != nil {
			return nil, err
		}
		if _, ok := seen[owner]; ok {
			continue
		}
		seen[owner] = struct{}{}
		owners = append(owners, owner)
	}
	slices.SortFunc(owners, func(a, b MembershipOwner) int {
		return strings.Compare(string(a), string(b))
	})
	return owners, nil
}

func encodeMembershipOwners(owners []MembershipOwner) string {
	normalized := make([]MembershipOwner, 0, len(owners))
	seen := make(map[MembershipOwner]struct{}, len(owners))
	for _, owner := range owners {
		normalizedOwner, err := normalizeMembershipOwner(owner)
		if err != nil {
			continue
		}
		if _, ok := seen[normalizedOwner]; ok {
			continue
		}
		seen[normalizedOwner] = struct{}{}
		normalized = append(normalized, normalizedOwner)
	}
	slices.SortFunc(normalized, func(a, b MembershipOwner) int {
		return strings.Compare(string(a), string(b))
	})
	parts := make([]string, 0, len(normalized))
	for _, owner := range normalized {
		parts = append(parts, string(owner))
	}
	return strings.Join(parts, ",")
}

func addMembershipOwner(owners []MembershipOwner, owner MembershipOwner) ([]MembershipOwner, bool) {
	normalizedOwner, err := normalizeMembershipOwner(owner)
	if err != nil {
		return owners, false
	}
	normalized := make([]MembershipOwner, 0, len(owners)+1)
	seen := false
	for _, existing := range owners {
		current, err := normalizeMembershipOwner(existing)
		if err != nil {
			continue
		}
		normalized = append(normalized, current)
		if current == normalizedOwner {
			seen = true
		}
	}
	if seen {
		return uniqueMembershipOwners(normalized), false
	}
	normalized = append(normalized, normalizedOwner)
	return uniqueMembershipOwners(normalized), true
}

func removeMembershipOwner(owners []MembershipOwner, owner MembershipOwner) ([]MembershipOwner, bool) {
	normalizedOwner, err := normalizeMembershipOwner(owner)
	if err != nil {
		return owners, false
	}
	normalized := make([]MembershipOwner, 0, len(owners))
	removed := false
	for _, existing := range owners {
		current, err := normalizeMembershipOwner(existing)
		if err != nil {
			continue
		}
		if current == normalizedOwner {
			removed = true
			continue
		}
		normalized = append(normalized, current)
	}
	return uniqueMembershipOwners(normalized), removed
}

func uniqueMembershipOwners(owners []MembershipOwner) []MembershipOwner {
	if len(owners) == 0 {
		return nil
	}
	seen := make(map[MembershipOwner]struct{}, len(owners))
	normalized := make([]MembershipOwner, 0, len(owners))
	for _, owner := range owners {
		current, err := normalizeMembershipOwner(owner)
		if err != nil {
			continue
		}
		if _, ok := seen[current]; ok {
			continue
		}
		seen[current] = struct{}{}
		normalized = append(normalized, current)
	}
	slices.SortFunc(normalized, func(a, b MembershipOwner) int {
		return strings.Compare(string(a), string(b))
	})
	return normalized
}

func effectiveMembershipBackfillPolicy(owners []MembershipOwner, manualPolicy MembershipBackfillPolicy) MembershipBackfillPolicy {
	normalizedOwners := uniqueMembershipOwners(owners)
	for _, owner := range normalizedOwners {
		if owner == MembershipOwnerGroup {
			return MembershipBackfillAll
		}
	}
	for _, owner := range normalizedOwners {
		if owner == MembershipOwnerManual {
			policy, err := normalizeBackfillPolicy(manualPolicy)
			if err == nil {
				return policy
			}
			return MembershipBackfillAll
		}
	}
	for _, owner := range normalizedOwners {
		if owner == MembershipOwnerBinding {
			return MembershipBackfillSinceJoin
		}
	}
	policy, err := normalizeBackfillPolicy(manualPolicy)
	if err == nil {
		return policy
	}
	return MembershipBackfillAll
}

func manualBackfillMetadataValue(owner MembershipOwner, policy MembershipBackfillPolicy) string {
	if owner != MembershipOwnerManual {
		return ""
	}
	return string(policy)
}

func requireControllerCaller(caller Caller) error {
	if normalizeCaller(caller).Kind != CallerController {
		return ErrUnauthorized
	}
	return nil
}

func clampTranscriptLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultTranscriptPageSize
	case limit > maxTranscriptPageSize:
		return maxTranscriptPageSize
	default:
		return limit
	}
}

func transcriptBucket(sequence int64) int64 {
	if sequence <= 0 {
		return 0
	}
	return (sequence - 1) / transcriptBucketSize
}

func marshalOptionalJSON(v any) (string, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return "", fmt.Errorf("marshal json: %w", err)
	}
	if string(data) == "null" || string(data) == "[]" || string(data) == "{}" {
		return "", nil
	}
	return string(data), nil
}

func decodeOptionalActor(raw string) (ExternalActor, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ExternalActor{}, nil
	}
	var actor ExternalActor
	if err := json.Unmarshal([]byte(raw), &actor); err != nil {
		return ExternalActor{}, fmt.Errorf("decode actor_json: %w", err)
	}
	return actor, nil
}

func decodeOptionalAttachments(raw string) ([]ExternalAttachment, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}
	var attachments []ExternalAttachment
	if err := json.Unmarshal([]byte(raw), &attachments); err != nil {
		return nil, fmt.Errorf("decode attachments_json: %w", err)
	}
	return attachments, nil
}

func sortConversationRefsFromMemberships(memberships []ConversationMembershipRecord) {
	slices.SortFunc(memberships, func(a, b ConversationMembershipRecord) int {
		ka := conversationLockKey(a.Conversation)
		kb := conversationLockKey(b.Conversation)
		switch {
		case ka < kb:
			return -1
		case ka > kb:
			return 1
		default:
			return strings.Compare(a.ID, b.ID)
		}
	})
}
