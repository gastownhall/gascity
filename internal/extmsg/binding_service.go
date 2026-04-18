package extmsg

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

const defaultTouchDebounce = 30 * time.Second

type bindingLockEntry struct {
	mu   sync.Mutex
	refs int
}

type bindingLockPool struct {
	mu    sync.Mutex
	locks map[string]*bindingLockEntry
}

var sharedBindingLockPools sync.Map

type bindingCleaner interface {
	ClearForConversation(ctx context.Context, sessionID string, ref ConversationRef) error
}

type bindingMembershipEnsurer interface {
	EnsureMembership(ctx context.Context, input EnsureMembershipInput) (ConversationMembershipRecord, error)
	RemoveMembership(ctx context.Context, input RemoveMembershipInput) error
	ensureMembershipLocked(input EnsureMembershipInput) (ConversationMembershipRecord, error)
	removeMembershipLocked(input RemoveMembershipInput) error
}

type bindingService struct {
	store         beads.Store
	delivery      bindingCleaner
	transcript    bindingMembershipEnsurer
	touchDebounce time.Duration
	locks         *bindingLockPool
}

// BindingServiceOption configures a binding service instance.
type BindingServiceOption func(*bindingService)

// WithBindingTouchDebounce sets the minimum interval between touch updates.
func WithBindingTouchDebounce(d time.Duration) BindingServiceOption {
	return func(s *bindingService) {
		if d > 0 {
			s.touchDebounce = d
		}
	}
}

func newBindingService(store beads.Store, delivery bindingCleaner, transcript bindingMembershipEnsurer, locks *bindingLockPool, opts ...BindingServiceOption) BindingService {
	svc := &bindingService{
		store:         store,
		touchDebounce: defaultTouchDebounce,
		locks:         locks,
	}
	if delivery != nil {
		svc.delivery = delivery
	}
	if transcript != nil {
		svc.transcript = transcript
	}
	for _, opt := range opts {
		if opt != nil {
			opt(svc)
		}
	}
	return svc
}

func (s *bindingService) Bind(ctx context.Context, caller Caller, input BindInput) (SessionBindingRecord, error) {
	if err := checkContext(ctx); err != nil {
		return SessionBindingRecord{}, err
	}
	ref, err := validateConversationRef(input.Conversation)
	if err != nil {
		return SessionBindingRecord{}, err
	}
	if err := authorizeMutation(caller, ref); err != nil {
		return SessionBindingRecord{}, err
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return SessionBindingRecord{}, fmt.Errorf("%w: session_id required", ErrInvalidInput)
	}
	now := zeroNow(input.Now)

	var out SessionBindingRecord
	err = withBindingLock(s.locks, ref, func() error {
		if err := checkContext(ctx); err != nil {
			return err
		}
		history, err := s.listBindingsForConversation(ref)
		if err != nil {
			return err
		}
		active, err := s.activeBinding(ctx, history, now)
		if err != nil {
			return err
		}
		if active != nil {
			if active.SessionID != sessionID {
				return fmt.Errorf("%w: conversation already bound to %s", ErrBindingConflict, active.SessionID)
			}
			if err := s.updateBindingMetadata(*active, input.Metadata, input.ExpiresAt, now); err != nil {
				return err
			}
			updated, err := s.getBinding(active.ID)
			if err != nil {
				return err
			}
			out = updated
			if s.transcript != nil {
				if _, err := s.transcript.ensureMembershipLocked(EnsureMembershipInput{
					Caller:         caller,
					Conversation:   ref,
					SessionID:      sessionID,
					BackfillPolicy: MembershipBackfillSinceJoin,
					Owner:          MembershipOwnerBinding,
					Now:            now,
				}); err != nil {
					return wrapTranscriptSyncError("ensure transcript membership after bind", err)
				}
			}
			return nil
		}
		nextGeneration := nextBindingGeneration(history)
		b, err := s.store.Create(beads.Bead{
			Title:  conversationTitle(ref),
			Type:   "task",
			Labels: []string{"gc:extmsg-binding", labelBindingBase, bindingConversationLabel(ref), bindingSessionLabel(sessionID)},
			Metadata: encodeMetadataFields(input.Metadata, map[string]string{
				"schema_version":         strconv.Itoa(schemaVersion),
				"scope_id":               ref.ScopeID,
				"provider":               ref.Provider,
				"account_id":             ref.AccountID,
				"conversation_id":        ref.ConversationID,
				"parent_conversation_id": ref.ParentConversationID,
				"conversation_kind":      string(ref.Kind),
				"session_id":             sessionID,
				"binding_generation":     strconv.FormatInt(nextGeneration, 10),
				"bound_at":               formatTime(now),
				"expires_at":             formatTimePtr(input.ExpiresAt),
				"last_touched_at":        formatTime(now),
				"created_by_kind":        string(normalizeCaller(caller).Kind),
				"created_by_id":          normalizeCaller(caller).ID,
			}),
		})
		if err != nil {
			return fmt.Errorf("create external binding: %w", err)
		}
		out, err = decodeBindingBead(b)
		if err != nil {
			return err
		}
		if s.transcript != nil {
			if _, err := s.transcript.ensureMembershipLocked(EnsureMembershipInput{
				Caller:         caller,
				Conversation:   ref,
				SessionID:      sessionID,
				BackfillPolicy: MembershipBackfillSinceJoin,
				Owner:          MembershipOwnerBinding,
				Now:            now,
			}); err != nil {
				return wrapTranscriptSyncError("ensure transcript membership after bind", err)
			}
		}
		return nil
	})
	if err != nil {
		return SessionBindingRecord{}, err
	}
	return out, nil
}

func (s *bindingService) ResolveByConversation(ctx context.Context, ref ConversationRef) (*SessionBindingRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	ref, err := validateConversationRef(ref)
	if err != nil {
		return nil, err
	}
	return resolveActiveBinding(ctx, s.locks, s.store, s.delivery, s.transcript, ref, timeNow())
}

func (s *bindingService) ListBySession(ctx context.Context, sessionID string) ([]SessionBindingRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	items, err := s.store.List(beads.ListQuery{Label: bindingSessionLabel(sessionID)})
	if err != nil {
		return nil, fmt.Errorf("list bindings by session label: %w", err)
	}
	seen := make(map[string]bool, len(items))
	out := make([]SessionBindingRecord, 0, len(items))
	for _, item := range items {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if !hasLabel(item, "gc:extmsg-binding") {
			continue
		}
		record, err := decodeBindingBead(item)
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
		active, err := resolveActiveBinding(ctx, s.locks, s.store, s.delivery, s.transcript, record.Conversation, timeNow())
		if err != nil {
			return nil, err
		}
		if active != nil && active.SessionID == sessionID {
			out = append(out, *active)
		}
	}
	return out, nil
}

func (s *bindingService) Touch(ctx context.Context, caller Caller, bindingID string, now time.Time) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	bindingID = strings.TrimSpace(bindingID)
	if bindingID == "" {
		return nil
	}
	item, err := s.store.Get(bindingID)
	if err != nil {
		return fmt.Errorf("get binding %s: %w", bindingID, err)
	}
	record, err := decodeBindingBead(item)
	if err != nil {
		return err
	}
	if err := authorizeMutation(caller, record.Conversation); err != nil {
		return err
	}
	if record.Status != BindingActive {
		return nil
	}
	now = zeroNow(now)
	lastTouched, err := parseTime(item.Metadata, "last_touched_at")
	if err != nil {
		return err
	}
	if !lastTouched.IsZero() && now.Sub(lastTouched) < s.touchDebounce {
		return nil
	}
	return s.store.SetMetadata(bindingID, "last_touched_at", formatTime(now))
}

func (s *bindingService) Unbind(ctx context.Context, caller Caller, input UnbindInput) ([]SessionBindingRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	now := zeroNow(input.Now)
	sessionID := strings.TrimSpace(input.SessionID)
	if input.Conversation == nil && sessionID == "" {
		return nil, fmt.Errorf("%w: conversation or session_id required", ErrInvalidInput)
	}

	var seeds []SessionBindingRecord
	if input.Conversation != nil {
		ref, err := validateConversationRef(*input.Conversation)
		if err != nil {
			return nil, err
		}
		if err := authorizeMutation(caller, ref); err != nil {
			return nil, err
		}
		history, err := s.listBindingsForConversation(ref)
		if err != nil {
			return nil, err
		}
		for _, record := range history {
			if record.Status != BindingActive {
				continue
			}
			if sessionID != "" && record.SessionID != sessionID {
				continue
			}
			seeds = append(seeds, record)
		}
	} else {
		items, err := s.store.List(beads.ListQuery{Label: bindingSessionLabel(sessionID)})
		if err != nil {
			return nil, fmt.Errorf("list bindings by session label: %w", err)
		}
		for _, item := range items {
			if !hasLabel(item, "gc:extmsg-binding") {
				continue
			}
			record, err := decodeBindingBead(item)
			if err != nil {
				return nil, err
			}
			if record.Status != BindingActive || record.SessionID != sessionID {
				continue
			}
			if err := authorizeMutation(caller, record.Conversation); err != nil {
				return nil, err
			}
			seeds = append(seeds, record)
		}
	}
	if len(seeds) == 0 {
		return nil, nil
	}
	sortConversationRefs(seeds)

	closed := make([]SessionBindingRecord, 0, len(seeds))
	for _, seed := range seeds {
		err := withBindingLock(s.locks, seed.Conversation, func() error {
			if err := checkContext(ctx); err != nil {
				return err
			}
			history, err := s.listBindingsForConversation(seed.Conversation)
			if err != nil {
				return err
			}
			active, err := s.activeBinding(ctx, history, now)
			if err != nil {
				return err
			}
			if active == nil {
				return nil
			}
			if sessionID != "" && active.SessionID != sessionID {
				return nil
			}
			if err := s.store.SetMetadata(active.ID, "last_touched_at", formatTime(now)); err != nil {
				return fmt.Errorf("update binding %s metadata: %w", active.ID, err)
			}
			if s.transcript != nil {
				if err := s.transcript.removeMembershipLocked(RemoveMembershipInput{
					Caller:       caller,
					Conversation: active.Conversation,
					SessionID:    active.SessionID,
					Owner:        MembershipOwnerBinding,
					Now:          now,
				}); err != nil {
					return wrapTranscriptSyncError("remove transcript membership after unbind", err)
				}
			}
			if err := s.store.Close(active.ID); err != nil {
				if s.transcript != nil {
					_, _ = s.transcript.ensureMembershipLocked(EnsureMembershipInput{
						Caller:         caller,
						Conversation:   active.Conversation,
						SessionID:      active.SessionID,
						BackfillPolicy: MembershipBackfillSinceJoin,
						Owner:          MembershipOwnerBinding,
						Now:            now,
					})
				}
				return fmt.Errorf("close binding %s: %w", active.ID, err)
			}
			active.Status = BindingEnded
			if active.Metadata == nil {
				active.Metadata = make(map[string]string)
			}
			active.Metadata["last_touched_at"] = formatTime(now)
			closed = append(closed, *active)
			if s.delivery != nil {
				if err := s.delivery.ClearForConversation(ctx, active.SessionID, active.Conversation); err != nil {
					return err
				}
			}
			return nil
		})
		if err != nil {
			return closed, err
		}
	}
	return closed, nil
}

// ReassignSessionBindings moves active bindings from one session bead ID to
// another during canonical session repair.
func ReassignSessionBindings(ctx context.Context, store beads.Store, oldSessionID, newSessionID string, now time.Time) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if store == nil {
		return nil
	}
	oldSessionID = strings.TrimSpace(oldSessionID)
	newSessionID = strings.TrimSpace(newSessionID)
	if oldSessionID == "" || newSessionID == "" || oldSessionID == newSessionID {
		return nil
	}
	items, err := store.List(beads.ListQuery{Label: bindingSessionLabel(oldSessionID)})
	if err != nil {
		return fmt.Errorf("list bindings by retired session label: %w", err)
	}
	locks := sharedBindingLockPool(store)
	transcript := newTranscriptService(store, locks)
	delivery := deliveryCleaner{store: store, locks: locks}
	caller := Caller{Kind: CallerController, ID: "session-retirement"}
	now = zeroNow(now)
	for _, item := range items {
		if err := checkContext(ctx); err != nil {
			return err
		}
		if !hasLabel(item, labelBindingBase) || item.Status == "closed" {
			continue
		}
		seed, err := decodeBindingBead(item)
		if err != nil {
			return fmt.Errorf("decode binding %s: %w", item.ID, err)
		}
		if seed.Status != BindingActive || seed.SessionID != oldSessionID {
			continue
		}
		if err := withBindingLock(locks, seed.Conversation, func() error {
			latest, err := store.Get(seed.ID)
			if err != nil {
				return fmt.Errorf("get binding %s: %w", seed.ID, err)
			}
			if !hasLabel(latest, labelBindingBase) || latest.Status == "closed" {
				return nil
			}
			record, err := decodeBindingBead(latest)
			if err != nil {
				return fmt.Errorf("decode binding %s: %w", latest.ID, err)
			}
			if record.Status != BindingActive || record.SessionID != oldSessionID {
				return nil
			}
			hasTargetBinding, err := activeBindingExistsForSession(store, record.Conversation, record.ID, newSessionID)
			if err != nil {
				return err
			}
			if hasTargetBinding {
				if err := transcript.removeMembershipLocked(RemoveMembershipInput{
					Caller:       caller,
					Conversation: record.Conversation,
					SessionID:    oldSessionID,
					Owner:        MembershipOwnerBinding,
					Now:          now,
				}); err != nil {
					return wrapTranscriptSyncError("remove transcript membership after duplicate binding repair", err)
				}
				if err := delivery.ClearForConversation(ctx, oldSessionID, record.Conversation); err != nil {
					return err
				}
				if err := store.Close(record.ID); err != nil {
					return fmt.Errorf("close duplicate binding %s during session reassignment: %w", record.ID, err)
				}
				return nil
			}
			if _, err := transcript.ensureMembershipLocked(EnsureMembershipInput{
				Caller:         caller,
				Conversation:   record.Conversation,
				SessionID:      newSessionID,
				BackfillPolicy: MembershipBackfillSinceJoin,
				Owner:          MembershipOwnerBinding,
				Now:            now,
			}); err != nil {
				return wrapTranscriptSyncError("ensure transcript membership after binding reassignment", err)
			}
			labelsToAdd, labelsToRemove := recordLabels(latest.Labels, []string{bindingSessionLabel(oldSessionID)}, []string{bindingSessionLabel(newSessionID)})
			if err := store.Update(record.ID, beads.UpdateOpts{
				Labels:       labelsToAdd,
				RemoveLabels: labelsToRemove,
				Metadata: map[string]string{
					"session_id":      newSessionID,
					"last_touched_at": formatTime(now),
				},
			}); err != nil {
				return fmt.Errorf("reassign binding %s from session %s to %s: %w", record.ID, oldSessionID, newSessionID, err)
			}
			if err := transcript.removeMembershipLocked(RemoveMembershipInput{
				Caller:       caller,
				Conversation: record.Conversation,
				SessionID:    oldSessionID,
				Owner:        MembershipOwnerBinding,
				Now:          now,
			}); err != nil {
				return wrapTranscriptSyncError("remove transcript membership after binding reassignment", err)
			}
			if err := delivery.ClearForConversation(ctx, oldSessionID, record.Conversation); err != nil {
				return err
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

// CloseSessionBindings terminates active bindings for a retired session bead ID.
func CloseSessionBindings(ctx context.Context, store beads.Store, sessionID string, now time.Time) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	if store == nil {
		return nil
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	_, err := NewServices(store).Bindings.Unbind(ctx, Caller{Kind: CallerController, ID: "session-retirement"}, UnbindInput{
		SessionID: sessionID,
		Now:       now,
	})
	return err
}

func activeBindingExistsForSession(store beads.Store, ref ConversationRef, currentID, sessionID string) (bool, error) {
	items, err := store.List(beads.ListQuery{
		Label:         bindingConversationLabel(ref),
		IncludeClosed: true,
	})
	if err != nil {
		return false, fmt.Errorf("list bindings by conversation label: %w", err)
	}
	for _, item := range items {
		if item.ID == currentID || !hasLabel(item, labelBindingBase) || item.Status == "closed" {
			continue
		}
		record, err := decodeBindingBead(item)
		if err != nil {
			return false, err
		}
		if record.Status == BindingActive && sameConversationRef(record.Conversation, ref) && record.SessionID == sessionID {
			return true, nil
		}
	}
	return false, nil
}

func (s *bindingService) listBindingsForConversation(ref ConversationRef) ([]SessionBindingRecord, error) {
	items, err := s.store.List(beads.ListQuery{
		Label:         bindingConversationLabel(ref),
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list bindings by conversation label: %w", err)
	}
	out := make([]SessionBindingRecord, 0, len(items))
	for _, item := range items {
		if !hasLabel(item, "gc:extmsg-binding") {
			continue
		}
		record, err := decodeBindingBead(item)
		if err != nil {
			return nil, err
		}
		if !sameConversationRef(record.Conversation, ref) {
			continue
		}
		out = append(out, record)
	}
	return out, nil
}

func (s *bindingService) activeBinding(ctx context.Context, history []SessionBindingRecord, now time.Time) (*SessionBindingRecord, error) {
	return selectActiveBinding(ctx, history, now, func(record SessionBindingRecord) error {
		if s.transcript != nil {
			if err := s.transcript.removeMembershipLocked(RemoveMembershipInput{
				Caller:       Caller{Kind: CallerController, ID: "binding-expiry"},
				Conversation: record.Conversation,
				SessionID:    record.SessionID,
				Owner:        MembershipOwnerBinding,
				Now:          now,
			}); err != nil {
				return wrapTranscriptSyncError("remove transcript membership after binding expiry", err)
			}
		}
		if s.delivery != nil {
			if err := s.delivery.ClearForConversation(ctx, record.SessionID, record.Conversation); err != nil {
				return err
			}
		}
		if err := s.store.Close(record.ID); err != nil {
			if s.transcript != nil {
				_, _ = s.transcript.ensureMembershipLocked(EnsureMembershipInput{
					Caller:         Caller{Kind: CallerController, ID: "binding-expiry"},
					Conversation:   record.Conversation,
					SessionID:      record.SessionID,
					BackfillPolicy: MembershipBackfillSinceJoin,
					Owner:          MembershipOwnerBinding,
					Now:            now,
				})
			}
			return fmt.Errorf("close expired binding %s: %w", record.ID, err)
		}
		return nil
	})
}

func (s *bindingService) updateBindingMetadata(record SessionBindingRecord, meta map[string]string, expiresAt *time.Time, now time.Time) error {
	fields := map[string]string{
		"expires_at":      formatTimePtr(expiresAt),
		"last_touched_at": formatTime(now),
	}
	if record.ExpiresAt != nil && expiresAt == nil {
		fields["expires_at"] = ""
	}
	kvs := encodeMetadataFields(meta, fields)
	if len(kvs) == 0 {
		return nil
	}
	return s.store.SetMetadataBatch(record.ID, kvs)
}

func (s *bindingService) getBinding(id string) (SessionBindingRecord, error) {
	item, err := s.store.Get(id)
	if err != nil {
		return SessionBindingRecord{}, fmt.Errorf("get binding %s: %w", id, err)
	}
	return decodeBindingBead(item)
}

func decodeBindingBead(b beads.Bead) (SessionBindingRecord, error) {
	ref, err := conversationRefFromMetadata(b.Metadata)
	if err != nil {
		return SessionBindingRecord{}, err
	}
	boundAt, err := parseTime(b.Metadata, "bound_at")
	if err != nil {
		return SessionBindingRecord{}, err
	}
	expiresAtRaw, err := parseTime(b.Metadata, "expires_at")
	if err != nil {
		return SessionBindingRecord{}, err
	}
	var expiresAt *time.Time
	if !expiresAtRaw.IsZero() {
		expiresAt = &expiresAtRaw
	}
	return SessionBindingRecord{
		ID:                b.ID,
		SchemaVersion:     parseInt(b.Metadata, "schema_version"),
		Conversation:      ref,
		SessionID:         strings.TrimSpace(b.Metadata["session_id"]),
		Status:            recordStatus(b),
		BoundAt:           boundAt,
		ExpiresAt:         expiresAt,
		BindingGeneration: parseInt64(b.Metadata, "binding_generation"),
		Metadata:          decodePrefixedMetadata(b.Metadata),
	}, nil
}

func nextBindingGeneration(records []SessionBindingRecord) int64 {
	var maxGeneration int64
	for _, record := range records {
		if record.BindingGeneration > maxGeneration {
			maxGeneration = record.BindingGeneration
		}
	}
	return maxGeneration + 1
}

func bindingExpired(record SessionBindingRecord, now time.Time) bool {
	return record.ExpiresAt != nil && !record.ExpiresAt.After(now)
}

func resolveActiveBinding(ctx context.Context, locks *bindingLockPool, store beads.Store, delivery bindingCleaner, transcript bindingMembershipEnsurer, ref ConversationRef, now time.Time) (*SessionBindingRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	var out *SessionBindingRecord
	err := withBindingLock(locks, ref, func() error {
		var err error
		out, err = resolveActiveBindingLocked(ctx, store, delivery, transcript, ref, now)
		return err
	})
	return out, err
}

func resolveActiveBindingLocked(ctx context.Context, store beads.Store, delivery bindingCleaner, transcript bindingMembershipEnsurer, ref ConversationRef, now time.Time) (*SessionBindingRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	items, err := store.List(beads.ListQuery{
		Label:         bindingConversationLabel(ref),
		IncludeClosed: true,
	})
	if err != nil {
		return nil, fmt.Errorf("list bindings by conversation label: %w", err)
	}
	history := make([]SessionBindingRecord, 0, len(items))
	for _, item := range items {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if !hasLabel(item, "gc:extmsg-binding") {
			continue
		}
		record, err := decodeBindingBead(item)
		if err != nil {
			return nil, err
		}
		if !sameConversationRef(record.Conversation, ref) {
			continue
		}
		history = append(history, record)
	}
	return selectActiveBinding(ctx, history, now, func(record SessionBindingRecord) error {
		if transcript != nil {
			if err := transcript.removeMembershipLocked(RemoveMembershipInput{
				Caller:       Caller{Kind: CallerController, ID: "binding-expiry"},
				Conversation: record.Conversation,
				SessionID:    record.SessionID,
				Owner:        MembershipOwnerBinding,
				Now:          now,
			}); err != nil {
				return wrapTranscriptSyncError("remove transcript membership after binding expiry", err)
			}
		}
		if delivery != nil {
			if err := delivery.ClearForConversation(ctx, record.SessionID, record.Conversation); err != nil {
				return err
			}
		}
		if err := store.Close(record.ID); err != nil {
			if transcript != nil {
				_, _ = transcript.ensureMembershipLocked(EnsureMembershipInput{
					Caller:         Caller{Kind: CallerController, ID: "binding-expiry"},
					Conversation:   record.Conversation,
					SessionID:      record.SessionID,
					BackfillPolicy: MembershipBackfillSinceJoin,
					Owner:          MembershipOwnerBinding,
					Now:            now,
				})
			}
			return fmt.Errorf("close expired binding %s: %w", record.ID, err)
		}
		return nil
	})
}

func selectActiveBinding(ctx context.Context, history []SessionBindingRecord, now time.Time, expire func(SessionBindingRecord) error) (*SessionBindingRecord, error) {
	var active *SessionBindingRecord
	for _, record := range history {
		if err := checkContext(ctx); err != nil {
			return nil, err
		}
		if record.Status != BindingActive {
			continue
		}
		if bindingExpired(record, now) {
			if err := expire(record); err != nil {
				return nil, err
			}
			continue
		}
		if active != nil {
			return nil, fmt.Errorf("%w: multiple active bindings for %s", ErrInvariantViolation, conversationLockKey(record.Conversation))
		}
		rec := record
		active = &rec
	}
	return active, nil
}

func withBindingLock(pool *bindingLockPool, ref ConversationRef, fn func() error) error {
	return withLockKey(pool, conversationLockKey(ref), fn)
}

func withLockKey(pool *bindingLockPool, key string, fn func() error) error {
	lock := pool.acquire(key)
	defer pool.release(key, lock)
	return fn()
}

func newBindingLockPool() *bindingLockPool {
	return &bindingLockPool{locks: map[string]*bindingLockEntry{}}
}

func sharedBindingLockPool(store beads.Store) *bindingLockPool {
	key := bindingLockPoolKey(store)
	if existing, ok := sharedBindingLockPools.Load(key); ok {
		return existing.(*bindingLockPool)
	}
	created := newBindingLockPool()
	actual, _ := sharedBindingLockPools.LoadOrStore(key, created)
	return actual.(*bindingLockPool)
}

func bindingLockPoolKey(store beads.Store) string {
	if store == nil {
		return "<nil>"
	}
	value := reflect.ValueOf(store)
	switch value.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan, reflect.UnsafePointer:
		return fmt.Sprintf("%T:%x", store, value.Pointer())
	default:
		return fmt.Sprintf("%T:%v", store, store)
	}
}

func (p *bindingLockPool) acquire(key string) *bindingLockEntry {
	p.mu.Lock()
	lock := p.locks[key]
	if lock == nil {
		lock = &bindingLockEntry{}
		p.locks[key] = lock
	}
	lock.refs++
	p.mu.Unlock()

	lock.mu.Lock()
	return lock
}

func (p *bindingLockPool) release(key string, lock *bindingLockEntry) {
	lock.mu.Unlock()

	p.mu.Lock()
	lock.refs--
	if lock.refs == 0 {
		delete(p.locks, key)
	}
	p.mu.Unlock()
}

func conversationRefFromMetadata(meta map[string]string) (ConversationRef, error) {
	return validateConversationRef(ConversationRef{
		ScopeID:              meta["scope_id"],
		Provider:             meta["provider"],
		AccountID:            meta["account_id"],
		ConversationID:       meta["conversation_id"],
		ParentConversationID: meta["parent_conversation_id"],
		Kind:                 ConversationKind(meta["conversation_kind"]),
	})
}

func recordLabels(oldLabels []string, remove []string, add []string) ([]string, []string) {
	desired := make(map[string]bool, len(add))
	for _, label := range add {
		label = strings.TrimSpace(label)
		if label == "" {
			continue
		}
		desired[label] = true
	}
	updatedAdd := make([]string, 0, len(add))
	for _, label := range add {
		if label == "" || slices.Contains(oldLabels, label) {
			continue
		}
		updatedAdd = append(updatedAdd, label)
	}
	updatedRemove := make([]string, 0, len(remove))
	for _, label := range remove {
		label = strings.TrimSpace(label)
		if label == "" || !slices.Contains(oldLabels, label) || desired[label] {
			continue
		}
		updatedRemove = append(updatedRemove, label)
	}
	return updatedAdd, updatedRemove
}
