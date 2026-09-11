package extmsg

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// InboundResult captures the outcome of processing an inbound message.
// TargetSessionID is set when the conversation is bound to a concrete
// session; TargetAgentName is set when it is bound to a configured agent
// identity, whose live session the delivery layer resolves (cold-waking one
// when none is live).
type InboundResult struct {
	Message         ExternalInboundMessage
	Binding         *SessionBindingRecord
	GroupRoute      *GroupRouteDecision
	TranscriptEntry *ConversationTranscriptRecord
	TargetSessionID string
	TargetAgentName string
}

// targetSubject is the event subject for the routed target: the concrete
// session when one is bound, the agent identity otherwise.
func (r *InboundResult) targetSubject() string {
	if r.TargetSessionID != "" {
		return r.TargetSessionID
	}
	return r.TargetAgentName
}

// routed reports whether the message resolved to any delivery target.
func (r *InboundResult) routed() bool {
	return r.TargetSessionID != "" || r.TargetAgentName != ""
}

// InboundDeps bundles the dependencies for inbound processing.
// The caller (HTTP handler) assembles deps from api.State, keeping
// the orchestrator independent of the State interface.
//
// DefaultAgentForConversation returns the configured default agent
// identity for an inbound conversation that has no binding and belongs to
// no group ("" = no route configured). The API layer supplies the config
// lookup; extmsg stays config-free. When it names an agent, the pipeline
// binds the conversation to that agent (a sticky agent-name binding) and
// routes the message to it. A grouped conversation is never default-routed,
// even when its own routing finds no target, so the sticky binding cannot
// shadow the room.
type InboundDeps struct {
	Services                    Services
	Registry                    *AdapterRegistry
	EmitEvent                   func(eventType, subject string, payload events.Payload)
	DefaultAgentForConversation func(ref ConversationRef) string
}

// applyDefaultRoute binds an unrouted, ungrouped conversation to its
// configured default agent and marks the result as routed to it. The caller
// restricts this to conversations with no group so the sticky binding cannot
// shadow a room's own routing. A concurrent bind races benignly: on conflict
// the freshly active binding wins, whatever its kind.
func applyDefaultRoute(ctx context.Context, deps InboundDeps, result *InboundResult, ref ConversationRef, now time.Time) error {
	if deps.DefaultAgentForConversation == nil {
		return nil
	}
	agentName := deps.DefaultAgentForConversation(ref)
	if agentName == "" {
		return nil
	}
	caller := Caller{Kind: CallerController, ID: "extmsg-default-route"}
	binding, err := deps.Services.Bindings.Bind(ctx, caller, BindInput{
		Conversation: ref,
		AgentName:    agentName,
		Now:          now,
	})
	if err != nil {
		if !errors.Is(err, ErrBindingConflict) {
			return fmt.Errorf("binding default-route agent %q: %w", agentName, err)
		}
		active, rerr := deps.Services.Bindings.ResolveByConversation(ctx, ref)
		if rerr != nil {
			return fmt.Errorf("resolving binding after default-route conflict: %w", rerr)
		}
		if active == nil {
			return nil
		}
		result.Binding = active
		result.TargetSessionID = active.SessionID
		result.TargetAgentName = active.AgentName
		return nil
	}
	result.Binding = &binding
	result.TargetAgentName = binding.AgentName
	return nil
}

// resolveInboundTarget resolves the delivery target for an inbound message and
// records it on result: an existing binding first, then a group route, then the
// configured default route. result is left unrouted when nothing matches. Both
// inbound entry points share this helper so the binding -> group -> default
// precedence stays identical across the raw and pre-normalized paths.
//
// The default route applies only to conversations that belong to no group. A
// grouped conversation whose message finds no routable participant stays
// unrouted rather than being rebound to the default agent — a durable binding
// there would resolve before group routing and shadow the room on every later
// message.
func resolveInboundTarget(ctx context.Context, deps InboundDeps, result *InboundResult, msg ExternalInboundMessage, now time.Time) error {
	binding, err := deps.Services.Bindings.ResolveByConversation(ctx, msg.Conversation)
	if err != nil {
		return fmt.Errorf("resolving binding: %w", err)
	}
	if binding != nil {
		result.Binding = binding
		result.TargetSessionID = binding.SessionID
		result.TargetAgentName = binding.AgentName
	}

	// No binding — try group routing.
	grouped := false
	if !result.routed() {
		route, err := deps.Services.Groups.ResolveInbound(ctx, msg)
		if err != nil {
			if !errors.Is(err, ErrGroupNotFound) && !errors.Is(err, ErrGroupRouteNotFound) {
				return fmt.Errorf("resolving group route: %w", err)
			}
		} else {
			result.GroupRoute = route
			result.TargetSessionID = route.TargetSessionID
			grouped = route.Match != GroupRouteNoGroup
		}
	}

	// Still no target — try the configured default route, but only for an
	// ungrouped conversation. A grouped conversation that missed routing keeps
	// the prior behavior of staying unrouted.
	if !result.routed() && !grouped {
		if err := applyDefaultRoute(ctx, deps, result, msg.Conversation, now); err != nil {
			return err
		}
	}
	return nil
}

// HandleInbound processes a raw inbound payload through the full pipeline:
//  1. Look up adapter by key.
//  2. Verify and normalize the payload.
//  3. Resolve binding for the conversation.
//  4. If no binding, try group routing, then the configured default route.
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

	// Steps 3-4: Resolve the delivery target — existing binding, else group
	// route, else the configured default route.
	if err := resolveInboundTarget(ctx, deps, result, *msg, msg.ReceivedAt); err != nil {
		return nil, err
	}
	if !result.routed() {
		// No binding, no group route, no default route — empty target.
		return result, nil
	}

	// Step 5: Append to transcript.
	if result.routed() {
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
		deps.EmitEvent(events.ExtMsgInbound, result.targetSubject(), InboundEventPayload{
			Provider:       msg.Conversation.Provider,
			ConversationID: msg.Conversation.ConversationID,
			Actor:          msg.Actor.DisplayName,
			TargetSession:  result.TargetSessionID,
			TargetAgent:    result.TargetAgentName,
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

	// Steps 1-2: Resolve the delivery target — existing binding, else group
	// route, else the configured default route.
	if err := resolveInboundTarget(ctx, deps, result, msg, now); err != nil {
		return nil, err
	}
	if !result.routed() {
		// No binding, no group route, no default route — empty target.
		return result, nil
	}

	// Step 3: Append to transcript.
	if result.routed() {
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
		deps.EmitEvent(events.ExtMsgInbound, result.targetSubject(), InboundEventPayload{
			Provider:       msg.Conversation.Provider,
			ConversationID: msg.Conversation.ConversationID,
			Actor:          msg.Actor.DisplayName,
			TargetSession:  result.TargetSessionID,
			TargetAgent:    result.TargetAgentName,
		})
	}

	return result, nil
}
