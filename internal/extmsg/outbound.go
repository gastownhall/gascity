package extmsg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// OutboundRequest specifies what to publish to an external conversation.
type OutboundRequest struct {
	SessionID        string
	Conversation     ConversationRef
	Text             string
	ReplyToMessageID string
	IdempotencyKey   string
	Metadata         map[string]string
}

// OutboundResult captures the outcome of a publish operation.
type OutboundResult struct {
	Receipt         PublishReceipt
	DeliveryContext *DeliveryContextRecord
	TranscriptEntry *ConversationTranscriptRecord
}

// OutboundDeps bundles the dependencies for outbound processing.
type OutboundDeps struct {
	Services  Services
	Registry  *AdapterRegistry
	EmitEvent func(eventType, subject string, payload events.Payload)
}

// HandleOutbound publishes a message from a session to an external conversation.
//
// Pipeline:
//  1. Resolve active binding for the conversation.
//  2. If a binding exists, verify the caller session owns it. If no binding
//     exists but the caller passed a SessionID, fall back to group routing:
//     the publish is authorized when the SessionID is a participant of the
//     group bound to the conversation (mirrors the inbound group fallback).
//  3. Look up adapter by conversation ref.
//  4. Call adapter.Publish.
//  5. Record delivery context.
//  6. Append outbound entry to transcript.
//  7. Emit event for the caller to fan out peer notifications.
//
// On the group-fallback path the publishing session is req.SessionID and
// BindingGeneration is zero — the group authorization model has no
// monotonic generation concept. Downstream consumers in the producer path
// do not compare generations against zero today.
func HandleOutbound(ctx context.Context, deps OutboundDeps, caller Caller, req OutboundRequest) (*OutboundResult, error) {
	if deps.Registry == nil {
		return nil, errors.New("adapter registry is nil")
	}

	// Step 1: Resolve binding.
	binding, err := deps.Services.Bindings.ResolveByConversation(ctx, req.Conversation)
	if err != nil {
		return nil, fmt.Errorf("resolving binding: %w", err)
	}

	// Step 2: Authorize the publish.
	//
	// publishingSession is the session we credit for the publish (delivery
	// context owner + event subject). On the binding path this is the
	// binding's session; on the group fallback path it is the caller's
	// session. bindingGeneration is non-zero only on the binding path.
	var publishingSession string
	var bindingGeneration int64
	switch {
	case binding != nil:
		if req.SessionID != "" && binding.SessionID != req.SessionID {
			return nil, fmt.Errorf("session %q does not own binding for conversation %s/%s (bound to %s)",
				req.SessionID, req.Conversation.Provider, req.Conversation.ConversationID, binding.SessionID)
		}
		publishingSession = binding.SessionID
		bindingGeneration = binding.BindingGeneration
	case req.SessionID == "":
		// No binding and no caller session — preserve the historical error
		// string so external callers that pattern-match it stay green.
		return nil, fmt.Errorf("no active binding for conversation %s/%s",
			req.Conversation.Provider, req.Conversation.ConversationID)
	default:
		decision, err := deps.Services.Groups.ResolveOutbound(ctx, req.Conversation, req.SessionID)
		if err != nil {
			return nil, fmt.Errorf("resolving group route: %w", err)
		}
		if decision == nil || decision.Match != GroupRouteParticipantMatch {
			return nil, fmt.Errorf("no active binding for conversation %s/%s",
				req.Conversation.Provider, req.Conversation.ConversationID)
		}
		publishingSession = req.SessionID
	}

	// Step 3: Look up adapter.
	adapter := deps.Registry.LookupByConversation(req.Conversation)
	if adapter == nil {
		return nil, fmt.Errorf("no adapter for %s/%s", req.Conversation.Provider, req.Conversation.AccountID)
	}

	// Step 4: Publish.
	receipt, err := adapter.Publish(ctx, PublishRequest{
		Conversation:     req.Conversation,
		Text:             req.Text,
		ReplyToMessageID: req.ReplyToMessageID,
		IdempotencyKey:   req.IdempotencyKey,
		Metadata:         req.Metadata,
	})
	if err != nil {
		return nil, fmt.Errorf("adapter publish: %w", err)
	}

	result := &OutboundResult{Receipt: *receipt}

	// If the publish was not delivered, return the receipt without recording.
	if !receipt.Delivered {
		return result, nil
	}

	// Step 5: Record delivery context (binding path only).
	//
	// Delivery context tracks per-binding publish state and requires a
	// non-zero BindingGeneration tied to an active binding — neither
	// applies on the group fallback path. Recording is intentionally
	// skipped there; transcript append below still runs and remains the
	// authoritative outbound record for group flows.
	now := time.Now()
	if binding != nil {
		dc := DeliveryContextRecord{
			SessionID:         publishingSession,
			Conversation:      req.Conversation,
			BindingGeneration: bindingGeneration,
			LastPublishedAt:   now,
			LastMessageID:     receipt.MessageID,
			SourceSessionID:   req.SessionID,
			Metadata:          req.Metadata,
		}
		if err := deps.Services.Delivery.Record(ctx, caller, dc); err != nil {
			// Delivery context recording is important but not fatal.
			// The message was already published.
			result.DeliveryContext = nil
		} else {
			result.DeliveryContext = &dc
		}
	}

	// Step 6: Append outbound transcript entry.
	entry, err := deps.Services.Transcript.Append(ctx, AppendTranscriptInput{
		Caller:            caller,
		Conversation:      req.Conversation,
		Kind:              TranscriptMessageOutbound,
		Provenance:        TranscriptProvenanceLive,
		ProviderMessageID: receipt.MessageID,
		Text:              req.Text,
		SourceSessionID:   req.SessionID,
		CreatedAt:         now,
		Metadata:          req.Metadata,
	})
	// Transcript append is non-fatal (whether hydration-pending or otherwise);
	// the message was already published. If it failed, the entry was not written.
	if err == nil {
		result.TranscriptEntry = &entry
	}

	// Step 7: Emit event.
	// Wake and peer fanout are handled by the caller. The event subject is
	// the publishing session — identical to binding.SessionID on the
	// binding path (Step 2 enforces equality with req.SessionID), and the
	// caller's session on the group fallback path.
	if deps.EmitEvent != nil {
		deps.EmitEvent(events.ExtMsgOutbound, publishingSession, OutboundEventPayload{
			Provider:       req.Conversation.Provider,
			ConversationID: req.Conversation.ConversationID,
			Session:        req.SessionID,
			MessageID:      receipt.MessageID,
		})
	}

	return result, nil
}
