package extmsg

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

type groupService struct {
	store      beads.Store
	locks      *bindingLockPool
	transcript groupTranscriptSync
}

type groupTranscriptSync interface {
	EnsureMembership(ctx context.Context, input EnsureMembershipInput) (ConversationMembershipRecord, error)
	RemoveMembership(ctx context.Context, input RemoveMembershipInput) error
}

// NewGroupService creates a GroupService backed by the given bead store.
func NewGroupService(store beads.Store) GroupService {
	locks := sharedBindingLockPool(store)
	return newGroupService(store, locks, newTranscriptService(store, locks))
}

func newGroupService(store beads.Store, locks *bindingLockPool, transcript groupTranscriptSync) GroupService {
	return &groupService{store: store, locks: locks, transcript: transcript}
}

func groupTranscriptCaller() Caller {
	return Caller{Kind: CallerController, ID: "group-service"}
}

func (s *groupService) EnsureGroup(ctx context.Context, caller Caller, input EnsureGroupInput) (ConversationGroupRecord, error) {
	if err := checkContext(ctx); err != nil {
		return ConversationGroupRecord{}, err
	}
	ref, err := validateConversationRef(input.RootConversation)
	if err != nil {
		return ConversationGroupRecord{}, err
	}
	if err := authorizeMutation(caller, ref); err != nil {
		return ConversationGroupRecord{}, err
	}
	mode := GroupMode(strings.ToLower(strings.TrimSpace(string(input.Mode))))
	switch mode {
	case GroupModeLauncher:
	default:
		return ConversationGroupRecord{}, fmt.Errorf("%w: invalid group mode %q", ErrInvalidInput, input.Mode)
	}
	defaultHandle := normalizeHandle(input.DefaultHandle)
	lastHandle := normalizeHandle(input.LastAddressedHandle)
	title := conversationTitle(ref)
	fields := encodeMetadataFields(input.Metadata, map[string]string{
		"schema_version":                      strconv.Itoa(schemaVersion),
		"scope_id":                            ref.ScopeID,
		"provider":                            ref.Provider,
		"account_id":                          ref.AccountID,
		"conversation_id":                     ref.ConversationID,
		"parent_conversation_id":              ref.ParentConversationID,
		"conversation_kind":                   string(ref.Kind),
		"mode":                                string(mode),
		"default_handle":                      defaultHandle,
		"last_addressed_handle":               lastHandle,
		"fanout_enabled":                      strconv.FormatBool(input.FanoutPolicy.Enabled),
		"fanout_allow_untargeted":             strconv.FormatBool(input.FanoutPolicy.AllowUntargetedPublication),
		"fanout_max_peer_triggered_publishes": strconv.Itoa(input.FanoutPolicy.MaxPeerTriggeredPublishes),
		"fanout_max_total_peer_deliveries":    strconv.Itoa(input.FanoutPolicy.MaxTotalPeerDeliveries),
	})
	if lastHandle == "" {
		delete(fields, "last_addressed_handle")
	}
	var out ConversationGroupRecord
	err = withLockKey(s.locks, groupRootLabel(ref), func() error {
		items, err := s.store.List(beads.ListQuery{Label: groupRootLabel(ref)})
		if err != nil {
			return fmt.Errorf("list groups by root label: %w", err)
		}
		for _, item := range items {
			if err := checkContext(ctx); err != nil {
				return err
			}
			if !hasLabel(item, "gc:extmsg-group") || item.Status == "closed" {
				continue
			}
			record, err := decodeGroupBead(item)
			if err != nil {
				return err
			}
			if !sameConversationRef(record.RootConversation, ref) {
				continue
			}
			if err := s.store.Update(item.ID, beads.UpdateOpts{Title: &title}); err != nil {
				return fmt.Errorf("update group title: %w", err)
			}
			if err := s.store.SetMetadataBatch(item.ID, fields); err != nil {
				return fmt.Errorf("update group metadata: %w", err)
			}
			updated, err := s.store.Get(item.ID)
			if err != nil {
				return fmt.Errorf("get group %s: %w", item.ID, err)
			}
			out, err = decodeGroupBead(updated)
			return err
		}
		created, err := s.store.Create(beads.Bead{
			Title:    title,
			Type:     "task",
			Labels:   []string{"gc:extmsg-group", labelGroupBase, groupRootLabel(ref)},
			Metadata: fields,
		})
		if err != nil {
			return fmt.Errorf("create group: %w", err)
		}
		out, err = decodeGroupBead(created)
		return err
	})
	return out, err
}

func (s *groupService) UpsertParticipant(ctx context.Context, caller Caller, input UpsertParticipantInput) (ConversationGroupParticipant, error) {
	if err := checkContext(ctx); err != nil {
		return ConversationGroupParticipant{}, err
	}
	groupID := strings.TrimSpace(input.GroupID)
	if groupID == "" {
		return ConversationGroupParticipant{}, fmt.Errorf("%w: group_id required", ErrInvalidInput)
	}
	handle, err := validateHandle(input.Handle)
	if err != nil {
		return ConversationGroupParticipant{}, err
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return ConversationGroupParticipant{}, fmt.Errorf("%w: session_id required", ErrInvalidInput)
	}
	group, err := s.getGroupByID(groupID)
	if err != nil {
		return ConversationGroupParticipant{}, err
	}
	if err := authorizeMutation(caller, group.RootConversation); err != nil {
		return ConversationGroupParticipant{}, err
	}
	title := groupID + "/" + handle
	fields := encodeMetadataFields(input.Metadata, map[string]string{
		"schema_version": strconv.Itoa(schemaVersion),
		"group_id":       groupID,
		"handle":         handle,
		"session_id":     sessionID,
		"public":         strconv.FormatBool(input.Public),
	})
	var out ConversationGroupParticipant
	err = withLockKey(s.locks, groupParticipantsMutationLock(groupID), func() error {
		items, err := s.store.List(beads.ListQuery{
			Label:         groupParticipantLabel(groupID),
			IncludeClosed: true,
		})
		if err != nil {
			return fmt.Errorf("list group participants: %w", err)
		}
		for _, item := range items {
			if err := checkContext(ctx); err != nil {
				return err
			}
			if !hasLabel(item, "gc:extmsg-participant") || item.Status == "closed" {
				continue
			}
			record, err := decodeParticipantBead(item)
			if err != nil {
				return err
			}
			if record.Handle != handle {
				continue
			}
			pendingCleanup := pendingCleanupSessionIDsFromMetadata(item.Metadata)
			if record.SessionID != "" && record.SessionID != sessionID {
				pendingCleanup = append(pendingCleanup, record.SessionID)
			}
			pendingCleanup = removeSessionID(pendingCleanup, sessionID)
			updateFields := mapsClone(fields)
			updateFields["previous_session_id_pending_cleanup"] = encodePendingCleanupSessionIDs(pendingCleanup)
			labelsToAdd, labelsToRemove := recordLabels(item.Labels, []string{groupParticipantSessionLabel(record.SessionID)}, []string{groupParticipantSessionLabel(sessionID)})
			if err := s.store.Update(item.ID, beads.UpdateOpts{
				Title:        &title,
				Labels:       labelsToAdd,
				RemoveLabels: labelsToRemove,
			}); err != nil {
				return fmt.Errorf("update group participant: %w", err)
			}
			if err := s.store.SetMetadataBatch(item.ID, updateFields); err != nil {
				return fmt.Errorf("update participant metadata: %w", err)
			}
			updated, err := s.store.Get(item.ID)
			if err != nil {
				return fmt.Errorf("get participant %s: %w", item.ID, err)
			}
			out, err = decodeParticipantBead(updated)
			if err != nil {
				return err
			}
			if s.transcript == nil {
				return nil
			}
			_, err = s.transcript.EnsureMembership(ctx, EnsureMembershipInput{
				Caller:         groupTranscriptCaller(),
				Conversation:   group.RootConversation,
				SessionID:      sessionID,
				BackfillPolicy: MembershipBackfillAll,
				Owner:          MembershipOwnerGroup,
				Now:            timeNow(),
			})
			if err != nil {
				return wrapTranscriptSyncError("ensure transcript membership after participant upsert", err)
			}
			if len(pendingCleanup) == 0 {
				return nil
			}
			activeSessions, err := s.activeParticipantSessionCounts(ctx, groupID)
			if err != nil {
				return err
			}
			remainingCleanup := make([]string, 0, len(pendingCleanup))
			var cleanupErr error
			for _, cleanupSessionID := range pendingCleanup {
				if activeSessions[cleanupSessionID] > 0 {
					continue
				}
				err = s.transcript.RemoveMembership(ctx, RemoveMembershipInput{
					Caller:       groupTranscriptCaller(),
					Conversation: group.RootConversation,
					SessionID:    cleanupSessionID,
					Owner:        MembershipOwnerGroup,
					Now:          timeNow(),
				})
				if err == nil || errors.Is(err, ErrMembershipNotFound) {
					continue
				}
				cleanupErr = err
				remainingCleanup = append(remainingCleanup, cleanupSessionID)
			}
			if err := s.setParticipantPendingCleanup(item.ID, remainingCleanup); err != nil {
				return err
			}
			if len(remainingCleanup) > 0 {
				return wrapTranscriptSyncError("remove transcript membership after participant reassignment", cleanupErr)
			}
			return nil
		}
		created, err := s.store.Create(beads.Bead{
			Title:    title,
			Type:     "task",
			Labels:   []string{"gc:extmsg-participant", labelGroupParticipantBase, groupParticipantLabel(groupID), groupParticipantSessionLabel(sessionID)},
			Metadata: fields,
		})
		if err != nil {
			return fmt.Errorf("create group participant: %w", err)
		}
		out, err = decodeParticipantBead(created)
		if err != nil {
			return err
		}
		if s.transcript == nil {
			return nil
		}
		_, err = s.transcript.EnsureMembership(ctx, EnsureMembershipInput{
			Caller:         groupTranscriptCaller(),
			Conversation:   group.RootConversation,
			SessionID:      sessionID,
			BackfillPolicy: MembershipBackfillAll,
			Owner:          MembershipOwnerGroup,
			Now:            timeNow(),
		})
		if err != nil {
			return wrapTranscriptSyncError("ensure transcript membership after participant upsert", err)
		}
		return nil
	})
	if err != nil {
		return ConversationGroupParticipant{}, err
	}
	return out, nil
}

func (s *groupService) RemoveParticipant(ctx context.Context, caller Caller, input RemoveParticipantInput) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	groupID := strings.TrimSpace(input.GroupID)
	if groupID == "" {
		return fmt.Errorf("%w: group_id required", ErrInvalidInput)
	}
	handle, err := validateHandle(input.Handle)
	if err != nil {
		return err
	}
	group, err := s.getGroupByID(groupID)
	if err != nil {
		return err
	}
	if err := authorizeMutation(caller, group.RootConversation); err != nil {
		return err
	}
	var sessionIDs []string
	var found bool
	err = withLockKey(s.locks, groupParticipantsMutationLock(groupID), func() error {
		items, err := s.store.List(beads.ListQuery{
			Label:         groupParticipantLabel(groupID),
			IncludeClosed: true,
		})
		if err != nil {
			return fmt.Errorf("list group participants: %w", err)
		}
		seenSessionIDs := make(map[string]struct{})
		for _, item := range items {
			if !hasLabel(item, "gc:extmsg-participant") {
				continue
			}
			record, err := decodeParticipantBead(item)
			if err != nil {
				return err
			}
			if record.Handle != handle {
				continue
			}
			found = true
			if record.SessionID != "" {
				if _, ok := seenSessionIDs[record.SessionID]; !ok {
					seenSessionIDs[record.SessionID] = struct{}{}
					sessionIDs = append(sessionIDs, record.SessionID)
				}
			}
			for _, pendingSessionID := range pendingCleanupSessionIDsFromMetadata(item.Metadata) {
				if pendingSessionID == "" {
					continue
				}
				if _, ok := seenSessionIDs[pendingSessionID]; ok {
					continue
				}
				seenSessionIDs[pendingSessionID] = struct{}{}
				sessionIDs = append(sessionIDs, pendingSessionID)
			}
			if item.Status == "closed" {
				continue
			}
			if err := s.store.Close(item.ID); err != nil {
				return fmt.Errorf("close participant %s: %w", item.ID, err)
			}
		}
		if s.transcript == nil {
			return nil
		}
		activeSessions, err := s.activeParticipantSessionCounts(ctx, groupID)
		if err != nil {
			return err
		}
		for _, sessionID := range sessionIDs {
			if activeSessions[sessionID] > 0 {
				continue
			}
			err := s.transcript.RemoveMembership(ctx, RemoveMembershipInput{
				Caller:       groupTranscriptCaller(),
				Conversation: group.RootConversation,
				SessionID:    sessionID,
				Owner:        MembershipOwnerGroup,
				Now:          timeNow(),
			})
			if err == nil || errors.Is(err, ErrMembershipNotFound) {
				continue
			}
			return wrapTranscriptSyncError("remove transcript membership after participant removal", err)
		}
		return nil
	})
	if err != nil {
		return err
	}
	if !found {
		return ErrGroupRouteNotFound
	}
	return nil
}

func (s *groupService) ResolveInbound(ctx context.Context, event ExternalInboundMessage) (*GroupRouteDecision, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	ref, err := validateConversationRef(event.Conversation)
	if err != nil {
		return nil, err
	}
	group, err := s.findGroupByRoot(ref)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return &GroupRouteDecision{Match: GroupRouteNoMatch}, nil
	}
	participants, err := s.listParticipants(group.ID)
	if err != nil {
		return nil, err
	}
	byHandle := make(map[string]ConversationGroupParticipant, len(participants))
	for _, participant := range participants {
		byHandle[participant.Handle] = participant
	}
	if explicit := normalizeHandle(event.ExplicitTarget); explicit != "" {
		target, ok := byHandle[explicit]
		if !ok {
			return &GroupRouteDecision{Match: GroupRouteNoMatch}, nil
		}
		return &GroupRouteDecision{
			Match:           GroupRouteExplicitTarget,
			TargetSessionID: target.SessionID,
			UpdateCursor:    true,
		}, nil
	}
	if target, ok := byHandle[group.LastAddressedHandle]; ok {
		return &GroupRouteDecision{
			Match:           GroupRouteLastAddressed,
			TargetSessionID: target.SessionID,
		}, nil
	}
	if target, ok := byHandle[group.DefaultHandle]; ok {
		return &GroupRouteDecision{
			Match:           GroupRouteDefault,
			TargetSessionID: target.SessionID,
		}, nil
	}
	return &GroupRouteDecision{Match: GroupRouteNoMatch}, nil
}

// ResolveOutbound authorizes an outbound publish from sessionID against the
// conversation's group. Unlike ResolveInbound (which routes a message to a
// participant by handle), ResolveOutbound checks whether sessionID is itself
// a participant of the group bound to ref. This mirrors the authorization
// boundary established by bind-room: any session that is a group participant
// is authorized to publish on behalf of the conversation.
//
// Returns a decision with Match == GroupRouteParticipantMatch and the matching
// participant when sessionID is a participant. Returns Match == GroupRouteNoMatch
// when no group is bound or sessionID is not a participant.
func (s *groupService) ResolveOutbound(ctx context.Context, ref ConversationRef, sessionID string) (*GroupOutboundDecision, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	validatedRef, err := validateConversationRef(ref)
	if err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, fmt.Errorf("%w: session_id required", ErrInvalidInput)
	}
	group, err := s.findGroupByRoot(validatedRef)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return &GroupOutboundDecision{Match: GroupRouteNoMatch}, nil
	}
	participants, err := s.listParticipants(group.ID)
	if err != nil {
		return nil, err
	}
	for _, participant := range participants {
		if participant.SessionID == sessionID {
			return &GroupOutboundDecision{
				Match:       GroupRouteParticipantMatch,
				GroupID:     group.ID,
				Participant: participant,
			}, nil
		}
	}
	return &GroupOutboundDecision{Match: GroupRouteNoMatch, GroupID: group.ID}, nil
}

func (s *groupService) UpdateCursor(ctx context.Context, caller Caller, input UpdateCursorInput) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	ref, err := validateConversationRef(input.RootConversation)
	if err != nil {
		return err
	}
	if err := authorizeMutation(caller, ref); err != nil {
		return err
	}
	handle := normalizeHandle(input.Handle)
	group, err := s.findGroupByRoot(ref)
	if err != nil {
		return err
	}
	if group == nil {
		return ErrGroupNotFound
	}
	if handle == "" {
		return s.store.SetMetadata(group.ID, "last_addressed_handle", "")
	}
	participants, err := s.listParticipants(group.ID)
	if err != nil {
		return err
	}
	found := false
	for _, participant := range participants {
		if participant.Handle == handle {
			found = true
			break
		}
	}
	if !found {
		return ErrGroupRouteNotFound
	}
	return s.store.SetMetadata(group.ID, "last_addressed_handle", handle)
}

// FindByConversation looks up an existing group by its root conversation.
func (s *groupService) FindByConversation(_ context.Context, _ Caller, ref ConversationRef) (*ConversationGroupRecord, error) {
	group, err := s.findGroupByRoot(ref)
	if err != nil {
		return nil, err
	}
	if group == nil {
		return nil, ErrGroupNotFound
	}
	return group, nil
}

func (s *groupService) findGroupByRoot(ref ConversationRef) (*ConversationGroupRecord, error) {
	items, err := s.store.List(beads.ListQuery{Label: groupRootLabel(ref)})
	if err != nil {
		return nil, fmt.Errorf("list groups by root label: %w", err)
	}
	var out *ConversationGroupRecord
	for _, item := range items {
		if !hasLabel(item, "gc:extmsg-group") || item.Status == "closed" {
			continue
		}
		record, err := decodeGroupBead(item)
		if err != nil {
			return nil, err
		}
		if !sameConversationRef(record.RootConversation, ref) {
			continue
		}
		if out != nil {
			return nil, fmt.Errorf("%w: multiple groups for %s", ErrInvariantViolation, conversationLockKey(ref))
		}
		rec := record
		out = &rec
	}
	return out, nil
}

func (s *groupService) getGroupByID(groupID string) (ConversationGroupRecord, error) {
	item, err := s.store.Get(groupID)
	if err != nil {
		return ConversationGroupRecord{}, fmt.Errorf("get group %s: %w", groupID, err)
	}
	if !hasLabel(item, "gc:extmsg-group") || item.Status == "closed" {
		return ConversationGroupRecord{}, ErrGroupNotFound
	}
	return decodeGroupBead(item)
}

func (s *groupService) listParticipants(groupID string) ([]ConversationGroupParticipant, error) {
	items, err := s.store.List(beads.ListQuery{Label: groupParticipantLabel(groupID)})
	if err != nil {
		return nil, fmt.Errorf("list group participants: %w", err)
	}
	out := make([]ConversationGroupParticipant, 0, len(items))
	seen := make(map[string]ConversationGroupParticipant)
	for _, item := range items {
		if !hasLabel(item, "gc:extmsg-participant") || item.Status == "closed" {
			continue
		}
		record, err := decodeParticipantBead(item)
		if err != nil {
			return nil, err
		}
		if existing, ok := seen[record.Handle]; ok {
			return nil, fmt.Errorf("%w: duplicate participants for handle %s (%s, %s)", ErrInvariantViolation, record.Handle, existing.ID, record.ID)
		}
		seen[record.Handle] = record
		out = append(out, record)
	}
	return out, nil
}

func (s *groupService) activeParticipantSessionCounts(ctx context.Context, groupID string) (map[string]int, error) {
	items, err := s.store.List(beads.ListQuery{Label: groupParticipantLabel(groupID)})
	if err != nil {
		return nil, fmt.Errorf("list group participants: %w", err)
	}
	counts := make(map[string]int)
	for _, item := range items {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if !hasLabel(item, "gc:extmsg-participant") || item.Status == "closed" {
			continue
		}
		record, err := decodeParticipantBead(item)
		if err != nil {
			return nil, err
		}
		if record.SessionID == "" {
			continue
		}
		counts[record.SessionID]++
	}
	return counts, nil
}

func (s *groupService) setParticipantPendingCleanup(participantID string, sessionIDs []string) error {
	if err := s.store.SetMetadata(participantID, "previous_session_id_pending_cleanup", encodePendingCleanupSessionIDs(sessionIDs)); err != nil {
		return fmt.Errorf("set participant pending cleanup: %w", err)
	}
	return nil
}

func groupParticipantsMutationLock(groupID string) string {
	return groupParticipantLabel(groupID) + ":mutation"
}

func mapsClone(src map[string]string) map[string]string {
	if len(src) == 0 {
		return map[string]string{}
	}
	dst := make(map[string]string, len(src))
	for key, value := range src {
		dst[key] = value
	}
	return dst
}

func pendingCleanupSessionIDsFromMetadata(metadata map[string]string) []string {
	raw := strings.TrimSpace(metadata["previous_session_id_pending_cleanup"])
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	seen := make(map[string]struct{}, len(parts))
	for _, part := range parts {
		sessionID := strings.TrimSpace(part)
		if sessionID == "" {
			continue
		}
		if _, ok := seen[sessionID]; ok {
			continue
		}
		seen[sessionID] = struct{}{}
		out = append(out, sessionID)
	}
	slices.Sort(out)
	return out
}

func encodePendingCleanupSessionIDs(sessionIDs []string) string {
	if len(sessionIDs) == 0 {
		return ""
	}
	normalized := make([]string, 0, len(sessionIDs))
	seen := make(map[string]struct{}, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" {
			continue
		}
		if _, ok := seen[sessionID]; ok {
			continue
		}
		seen[sessionID] = struct{}{}
		normalized = append(normalized, sessionID)
	}
	slices.Sort(normalized)
	return strings.Join(normalized, ",")
}

func removeSessionID(sessionIDs []string, target string) []string {
	target = strings.TrimSpace(target)
	if target == "" {
		return pendingCleanupSessionIDsFromMetadata(map[string]string{"previous_session_id_pending_cleanup": encodePendingCleanupSessionIDs(sessionIDs)})
	}
	out := make([]string, 0, len(sessionIDs))
	for _, sessionID := range sessionIDs {
		sessionID = strings.TrimSpace(sessionID)
		if sessionID == "" || sessionID == target {
			continue
		}
		out = append(out, sessionID)
	}
	return pendingCleanupSessionIDsFromMetadata(map[string]string{"previous_session_id_pending_cleanup": encodePendingCleanupSessionIDs(out)})
}

func decodeGroupBead(b beads.Bead) (ConversationGroupRecord, error) {
	ref, err := conversationRefFromMetadata(b.Metadata)
	if err != nil {
		return ConversationGroupRecord{}, err
	}
	return ConversationGroupRecord{
		ID:                  b.ID,
		SchemaVersion:       parseInt(b.Metadata, "schema_version"),
		RootConversation:    ref,
		Mode:                GroupMode(strings.TrimSpace(b.Metadata["mode"])),
		DefaultHandle:       normalizeHandle(b.Metadata["default_handle"]),
		LastAddressedHandle: normalizeHandle(b.Metadata["last_addressed_handle"]),
		FanoutPolicy: FanoutPolicy{
			Enabled:                    parseBool(b.Metadata, "fanout_enabled"),
			AllowUntargetedPublication: parseBool(b.Metadata, "fanout_allow_untargeted"),
			MaxPeerTriggeredPublishes:  parseInt(b.Metadata, "fanout_max_peer_triggered_publishes"),
			MaxTotalPeerDeliveries:     parseInt(b.Metadata, "fanout_max_total_peer_deliveries"),
		},
		Metadata: decodePrefixedMetadata(b.Metadata),
	}, nil
}

//nolint:unparam // error return reserved for future decoding failures
func decodeParticipantBead(b beads.Bead) (ConversationGroupParticipant, error) {
	return ConversationGroupParticipant{
		ID:        b.ID,
		GroupID:   strings.TrimSpace(b.Metadata["group_id"]),
		Handle:    normalizeHandle(b.Metadata["handle"]),
		SessionID: strings.TrimSpace(b.Metadata["session_id"]),
		Public:    parseBool(b.Metadata, "public"),
		Metadata:  decodePrefixedMetadata(b.Metadata),
	}, nil
}
