package extmsg

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/gastownhall/gascity/internal/beads"
)

type deliveryContextService struct {
	store      beads.Store
	locks      *bindingLockPool
	transcript bindingMembershipEnsurer
}

type deliveryCleaner struct {
	store beads.Store
	locks *bindingLockPool
}

func newDeliveryContextService(store beads.Store, locks *bindingLockPool, transcript bindingMembershipEnsurer) DeliveryContextService {
	return &deliveryContextService{store: store, locks: locks, transcript: transcript}
}

func (s *deliveryContextService) Record(ctx context.Context, caller Caller, input DeliveryContextRecord) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	ref, err := validateConversationRef(input.Conversation)
	if err != nil {
		return err
	}
	if err := authorizeMutation(caller, ref); err != nil {
		return err
	}
	sessionID := strings.TrimSpace(input.SessionID)
	if sessionID == "" {
		return fmt.Errorf("%w: session_id required", ErrInvalidInput)
	}
	if input.BindingGeneration <= 0 {
		return fmt.Errorf("%w: binding_generation required", ErrInvalidInput)
	}
	label := deliveryRouteLabel(ref, sessionID)
	title := sessionID + " -> " + conversationTitle(ref)
	fields := encodeMetadataFields(input.Metadata, map[string]string{
		"schema_version":         strconv.Itoa(schemaVersion),
		"session_id":             sessionID,
		"scope_id":               ref.ScopeID,
		"provider":               ref.Provider,
		"account_id":             ref.AccountID,
		"conversation_id":        ref.ConversationID,
		"parent_conversation_id": ref.ParentConversationID,
		"conversation_kind":      string(ref.Kind),
		"binding_generation":     strconv.FormatInt(input.BindingGeneration, 10),
		"last_published_at":      formatTime(input.LastPublishedAt),
		"last_message_id":        strings.TrimSpace(input.LastMessageID),
		"source_session_id":      strings.TrimSpace(input.SourceSessionID),
	})
	return withBindingLock(s.locks, ref, func() error {
		activeBinding, err := resolveActiveBindingLocked(ctx, s.store, deliveryCleaner{s.store, s.locks}, s.transcript, ref, timeNow())
		if err != nil {
			return err
		}
		if activeBinding == nil || activeBinding.SessionID != sessionID || activeBinding.BindingGeneration != input.BindingGeneration {
			return ErrBindingMismatch
		}
		return withLockKey(s.locks, label, func() error {
			items, err := s.store.List(beads.ListQuery{Label: label})
			if err != nil {
				return fmt.Errorf("list delivery contexts: %w", err)
			}
			for _, item := range items {
				if err := checkContext(ctx); err != nil {
					return err
				}
				if !hasLabel(item, "gc:extmsg-delivery") || item.Status == "closed" {
					continue
				}
				record, err := decodeDeliveryBead(item)
				if err != nil {
					return err
				}
				if !sameConversationRef(record.Conversation, ref) || record.SessionID != sessionID {
					continue
				}
				if err := s.store.Update(item.ID, beads.UpdateOpts{Title: &title}); err != nil {
					return fmt.Errorf("update delivery title: %w", err)
				}
				if err := s.store.SetMetadataBatch(item.ID, fields); err != nil {
					return fmt.Errorf("update delivery metadata: %w", err)
				}
				return nil
			}
			_, err = s.store.Create(beads.Bead{
				Title:    title,
				Type:     "task",
				Labels:   []string{"gc:extmsg-delivery", labelDeliveryBase, label, deliverySessionLabel(sessionID)},
				Metadata: fields,
			})
			if err != nil {
				return fmt.Errorf("create delivery context: %w", err)
			}
			return nil
		})
	})
}

func (s *deliveryContextService) Resolve(ctx context.Context, sessionID string, ref ConversationRef) (*DeliveryContextRecord, error) {
	if err := checkContext(ctx); err != nil {
		return nil, err
	}
	ref, err := validateConversationRef(ref)
	if err != nil {
		return nil, err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil, nil
	}
	label := deliveryRouteLabel(ref, sessionID)
	var out *DeliveryContextRecord
	err = withBindingLock(s.locks, ref, func() error {
		activeBinding, err := resolveActiveBindingLocked(ctx, s.store, deliveryCleaner{s.store, s.locks}, s.transcript, ref, timeNow())
		if err != nil {
			return err
		}
		return withLockKey(s.locks, label, func() error {
			items, err := s.store.List(beads.ListQuery{Label: label})
			if err != nil {
				return fmt.Errorf("list delivery contexts: %w", err)
			}
			for _, item := range items {
				if err := checkContext(ctx); err != nil {
					return err
				}
				if !hasLabel(item, "gc:extmsg-delivery") || item.Status == "closed" {
					continue
				}
				record, err := decodeDeliveryBead(item)
				if err != nil {
					return err
				}
				if !sameConversationRef(record.Conversation, ref) || record.SessionID != sessionID {
					continue
				}
				if activeBinding != nil &&
					activeBinding.SessionID == sessionID &&
					activeBinding.BindingGeneration == record.BindingGeneration {
					if out == nil {
						rec := record
						out = &rec
						continue
					}
					if err := s.store.Close(item.ID); err != nil {
						return fmt.Errorf("close duplicate delivery context %s: %w", item.ID, err)
					}
					continue
				}
				if err := s.store.Close(item.ID); err != nil {
					return fmt.Errorf("close stale delivery context %s: %w", item.ID, err)
				}
			}
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *deliveryContextService) ClearForConversation(ctx context.Context, sessionID string, ref ConversationRef) error {
	return deliveryCleaner{s.store, s.locks}.ClearForConversation(ctx, sessionID, ref)
}

func decodeDeliveryBead(b beads.Bead) (DeliveryContextRecord, error) {
	ref, err := conversationRefFromMetadata(b.Metadata)
	if err != nil {
		return DeliveryContextRecord{}, err
	}
	lastPublishedAt, err := parseTime(b.Metadata, "last_published_at")
	if err != nil {
		return DeliveryContextRecord{}, err
	}
	return DeliveryContextRecord{
		ID:                b.ID,
		SchemaVersion:     parseInt(b.Metadata, "schema_version"),
		SessionID:         strings.TrimSpace(b.Metadata["session_id"]),
		Conversation:      ref,
		BindingGeneration: parseInt64(b.Metadata, "binding_generation"),
		LastPublishedAt:   lastPublishedAt,
		LastMessageID:     strings.TrimSpace(b.Metadata["last_message_id"]),
		SourceSessionID:   strings.TrimSpace(b.Metadata["source_session_id"]),
		Metadata:          decodePrefixedMetadata(b.Metadata),
	}, nil
}

func (c deliveryCleaner) ClearForConversation(ctx context.Context, sessionID string, ref ConversationRef) error {
	if err := checkContext(ctx); err != nil {
		return err
	}
	ref, err := validateConversationRef(ref)
	if err != nil {
		return err
	}
	sessionID = strings.TrimSpace(sessionID)
	if sessionID == "" {
		return nil
	}
	label := deliveryRouteLabel(ref, sessionID)
	return withLockKey(c.locks, label, func() error {
		items, err := c.store.List(beads.ListQuery{Label: label})
		if err != nil {
			return fmt.Errorf("list delivery contexts: %w", err)
		}
		for _, item := range items {
			if err := checkContext(ctx); err != nil {
				return err
			}
			if !hasLabel(item, "gc:extmsg-delivery") || item.Status == "closed" {
				continue
			}
			record, err := decodeDeliveryBead(item)
			if err != nil {
				return err
			}
			if !sameConversationRef(record.Conversation, ref) || record.SessionID != sessionID {
				continue
			}
			if err := c.store.Close(item.ID); err != nil {
				return fmt.Errorf("close delivery context %s: %w", item.ID, err)
			}
		}
		return nil
	})
}
