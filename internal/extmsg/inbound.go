package extmsg

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// InboundResult captures the outcome of processing an inbound message.
type InboundResult struct {
	Message         ExternalInboundMessage
	Binding         *SessionBindingRecord
	GroupRoute      *GroupRouteDecision
	TranscriptEntry *ConversationTranscriptRecord
	TargetSessionID string
}

// InboundDeps bundles the dependencies for inbound processing.
// The caller (HTTP handler) assembles deps from api.State, keeping
// the orchestrator independent of the State interface.
type InboundDeps struct {
	Services  Services
	Registry  *AdapterRegistry
	EmitEvent func(eventType, subject string, payload map[string]any)
}

// HandleInbound processes a raw inbound payload through the full pipeline:
//  1. Look up adapter by key.
//  2. Verify and normalize the payload.
//  3. Resolve binding for the conversation.
//  4. If no binding, try group routing.
//  5. Append to transcript.
//  6. Nudge all conversation members (not just the target).
//  7. Emit event.
func HandleInbound(ctx context.Context, deps InboundDeps, key AdapterKey, payload InboundPayload) (*InboundResult, error) {
	if deps.Registry == nil {
		return nil, errors.New("adapter registry is nil")
	}

	// Step 1: Look up adapter.
	adapter := deps.Registry.Lookup(key)
	if adapter == nil {
		return nil, fmt.Errorf("no adapter registered for %s/%s", key.Provider, key.AccountID)
	}

	// Step 2: Verify and normalize.
	msg, err := adapter.VerifyAndNormalizeInbound(ctx, payload)
	if err != nil {
		return nil, fmt.Errorf("adapter verification failed: %w", err)
	}
	if msg == nil {
		return nil, errors.New("adapter returned nil message without error")
	}

	result := &InboundResult{Message: *msg}

	// Step 3: Resolve binding.
	binding, err := deps.Services.Bindings.ResolveByConversation(ctx, msg.Conversation)
	if err != nil {
		return nil, fmt.Errorf("resolving binding: %w", err)
	}

	if binding != nil {
		result.Binding = binding
		result.TargetSessionID = binding.SessionID
	}

	// Step 4: If no binding, try group routing.
	if result.TargetSessionID == "" {
		route, err := deps.Services.Groups.ResolveInbound(ctx, *msg)
		if err != nil {
			if !errors.Is(err, ErrGroupNotFound) && !errors.Is(err, ErrGroupRouteNotFound) {
				return nil, fmt.Errorf("resolving group route: %w", err)
			}
			// No binding and no group route — return result with empty target.
			return result, nil
		}
		result.GroupRoute = route
		result.TargetSessionID = route.TargetSessionID
	}

	// Step 5: Append to transcript.
	if result.TargetSessionID != "" {
		caller := Caller{
			Kind:      CallerAdapter,
			ID:        adapter.Name(),
			Provider:  key.Provider,
			AccountID: key.AccountID,
		}
		entry, err := deps.Services.Transcript.Append(ctx, AppendTranscriptInput{
			Caller:            caller,
			Conversation:      msg.Conversation,
			Kind:              TranscriptMessageInbound,
			Provenance:        TranscriptProvenanceLive,
			ProviderMessageID: msg.ProviderMessageID,
			Actor:             msg.Actor,
			Text:              msg.Text,
			ExplicitTarget:    msg.ExplicitTarget,
			ReplyToMessageID:  msg.ReplyToMessageID,
			Attachments:       msg.Attachments,
			CreatedAt:         msg.ReceivedAt,
		})
		if err != nil {
			if !errors.Is(err, ErrHydrationPending) {
				return nil, fmt.Errorf("appending transcript: %w", err)
			}
			// Hydration pending — transcript entry was not written.
		} else {
			result.TranscriptEntry = &entry
		}
	}

	// Step 6: Emit event.
	// Wake is handled by the caller (HTTP handler calls state.Poke()).
	// Sessions discover unread entries via gc transcript check --inject.
	if deps.EmitEvent != nil {
		deps.EmitEvent("extmsg.inbound", result.TargetSessionID, map[string]any{
			"provider":        msg.Conversation.Provider,
			"conversation_id": msg.Conversation.ConversationID,
			"actor":           msg.Actor.DisplayName,
			"target_session":  result.TargetSessionID,
		})
	}

	return result, nil
}

// HandleInboundNormalized processes a pre-normalized inbound message (used by
// out-of-process adapters that verify and normalize on their side before
// posting to the API).
func HandleInboundNormalized(ctx context.Context, deps InboundDeps, msg ExternalInboundMessage) (*InboundResult, error) {
	result := &InboundResult{Message: msg}

	now := msg.ReceivedAt
	if now.IsZero() {
		now = time.Now()
	}

	// Step 1: Resolve binding.
	binding, err := deps.Services.Bindings.ResolveByConversation(ctx, msg.Conversation)
	if err != nil {
		return nil, fmt.Errorf("resolving binding: %w", err)
	}

	if binding != nil {
		result.Binding = binding
		result.TargetSessionID = binding.SessionID
	}

	// Step 2: If no binding, try group routing.
	if result.TargetSessionID == "" {
		route, err := deps.Services.Groups.ResolveInbound(ctx, msg)
		if err != nil {
			if !errors.Is(err, ErrGroupNotFound) && !errors.Is(err, ErrGroupRouteNotFound) {
				return nil, fmt.Errorf("resolving group route: %w", err)
			}
			return result, nil
		}
		result.GroupRoute = route
		result.TargetSessionID = route.TargetSessionID
	}

	// Step 3: Append to transcript.
	if result.TargetSessionID != "" {
		caller := Caller{Kind: CallerController, ID: "inbound-normalized"}
		entry, err := deps.Services.Transcript.Append(ctx, AppendTranscriptInput{
			Caller:            caller,
			Conversation:      msg.Conversation,
			Kind:              TranscriptMessageInbound,
			Provenance:        TranscriptProvenanceLive,
			ProviderMessageID: msg.ProviderMessageID,
			Actor:             msg.Actor,
			Text:              msg.Text,
			ExplicitTarget:    msg.ExplicitTarget,
			ReplyToMessageID:  msg.ReplyToMessageID,
			Attachments:       msg.Attachments,
			CreatedAt:         now,
		})
		if err != nil {
			if !errors.Is(err, ErrHydrationPending) {
				return nil, fmt.Errorf("appending transcript: %w", err)
			}
			// Hydration pending — transcript entry was not written.
		} else {
			result.TranscriptEntry = &entry
		}
	}

	// Step 4: Emit event.
	if deps.EmitEvent != nil {
		deps.EmitEvent("extmsg.inbound", result.TargetSessionID, map[string]any{
			"provider":        msg.Conversation.Provider,
			"conversation_id": msg.Conversation.ConversationID,
			"actor":           msg.Actor.DisplayName,
			"target_session":  result.TargetSessionID,
		})
	}

	return result, nil
}
