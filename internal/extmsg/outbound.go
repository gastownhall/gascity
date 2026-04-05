package extmsg

import (
	"context"
	"errors"
	"fmt"
	"time"
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
	EmitEvent func(eventType, subject string, payload map[string]any)
}

// HandleOutbound publishes a message from a session to an external conversation.
//
// Pipeline:
//  1. Resolve active binding for the conversation.
//  2. Verify the binding session matches the caller.
//  3. Look up adapter by conversation ref.
//  4. Call adapter.Publish.
//  5. Record delivery context.
//  6. Append outbound entry to transcript.
//  7. Notify peer conversation members (best-effort nudge).
//  8. Emit event.
func HandleOutbound(ctx context.Context, deps OutboundDeps, caller Caller, req OutboundRequest) (*OutboundResult, error) {
	if deps.Registry == nil {
		return nil, errors.New("adapter registry is nil")
	}

	// Step 1: Resolve binding.
	binding, err := deps.Services.Bindings.ResolveByConversation(ctx, req.Conversation)
	if err != nil {
		return nil, fmt.Errorf("resolving binding: %w", err)
	}
	if binding == nil {
		return nil, fmt.Errorf("no active binding for conversation %s/%s",
			req.Conversation.Provider, req.Conversation.ConversationID)
	}

	// Step 2: Verify the caller session owns the binding.
	if req.SessionID != "" && binding.SessionID != req.SessionID {
		return nil, fmt.Errorf("session %q does not own binding for conversation %s/%s (bound to %s)",
			req.SessionID, req.Conversation.Provider, req.Conversation.ConversationID, binding.SessionID)
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

	// Step 5: Record delivery context.
	now := time.Now()
	dc := DeliveryContextRecord{
		SessionID:         binding.SessionID,
		Conversation:      req.Conversation,
		BindingGeneration: binding.BindingGeneration,
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
	// Wake is handled by the caller (HTTP handler calls state.Poke()).
	// Peer sessions discover new entries via gc transcript check --inject.
	if deps.EmitEvent != nil {
		deps.EmitEvent("extmsg.outbound", binding.SessionID, map[string]any{
			"provider":        req.Conversation.Provider,
			"conversation_id": req.Conversation.ConversationID,
			"session":         req.SessionID,
			"message_id":      receipt.MessageID,
		})
	}

	return result, nil
}
