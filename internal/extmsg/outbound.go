package extmsg

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/events"
)

// MetadataKeySourceSessionID was the reserved metadata key used to
// propagate the originating gc session id across the publish wire before
// PublishRequest gained a native SessionID field (gc-kvt). It is no
// longer written by gc; it is retained here only because some adapter
// binaries still fall back to it when PublishRequest.SessionID is empty
// (e.g. an old gc binary publishing through a newer adapter). New code
// must populate PublishRequest.SessionID directly.
const MetadataKeySourceSessionID = "source_session_id"

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
	DeliveryContext *DeliveryContextRecord        `json:"DeliveryContext,omitempty"`
	TranscriptEntry *ConversationTranscriptRecord `json:"TranscriptEntry,omitempty"`
}

// OutboundDeps bundles the dependencies for outbound processing.
//
// ResolveSessionSelector resolves a session selector — a configured agent
// identity, session name, alias, or concrete session bead ID — to the
// concrete ID of a live session. The API layer supplies its session target
// resolution; extmsg stays free of session-layer imports. It is required
// only to publish on agent-bound conversations.
type OutboundDeps struct {
	Services               Services
	Registry               *AdapterRegistry
	EmitEvent              func(eventType, subject string, payload events.Payload)
	ResolveSessionSelector func(ctx context.Context, selector string) (string, error)
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
	return handleOutbound(ctx, deps, caller, req, func(adapter TransportAdapter) (*PublishReceipt, error) {
		return adapter.Publish(ctx, PublishRequest{ //nolint:staticcheck // S1016: copy API inputs explicitly so future internal fields cannot leak to the adapter wire.
			SessionID: req.SessionID, Conversation: req.Conversation, Text: req.Text,
			ReplyToMessageID: req.ReplyToMessageID, IdempotencyKey: req.IdempotencyKey, Metadata: req.Metadata,
		})
	})
}

// PublishFileRequest identifies a file on the adapter's filesystem. The adapter
// owns file access and allowed-root enforcement; gc never opens FilePath.
// IdempotencyKey is forwarded; deduplication depends on the adapter's contract.
type PublishFileRequest struct {
	SessionID        string            `json:"session_id" minLength:"1"`
	Conversation     ConversationRef   `json:"conversation"`
	FilePath         string            `json:"file_path" minLength:"1"`
	Filename         string            `json:"filename,omitempty"`
	InitialComment   string            `json:"initial_comment,omitempty"`
	ReplyToMessageID string            `json:"reply_to_message_id,omitempty"`
	Title            string            `json:"title,omitempty"`
	IdempotencyKey   string            `json:"idempotency_key,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

// TranscriptText describes a file publish without disclosing its local path.
func (r PublishFileRequest) TranscriptText() string {
	name := r.Filename
	if name == "" {
		name = filepath.Base(r.FilePath)
	}
	text := "[file: " + name + "]"
	if r.InitialComment != "" {
		text = r.InitialComment + "\n" + text
	}
	return text
}

// PublishFileReceipt records the adapter's file-upload result. FileID is the
// provider's canonical file reference, not a chat message or thread timestamp.
type PublishFileReceipt struct {
	FileID       string
	Conversation ConversationRef
	Delivered    bool
	FailureKind  PublishFailureKind
	Error        string `json:"Error,omitempty"`
}

// FileTransportAdapter is an optional extension for adapters that upload files.
type FileTransportAdapter interface {
	PublishFile(ctx context.Context, req PublishFileRequest) (*PublishFileReceipt, error)
}

// OutboundFileResult captures a file receipt and the outbound records it created.
type OutboundFileResult struct {
	Receipt         PublishFileReceipt
	DeliveryContext *DeliveryContextRecord        `json:"DeliveryContext,omitempty"`
	TranscriptEntry *ConversationTranscriptRecord `json:"TranscriptEntry,omitempty"`
}

// HandleOutboundFile uses the message outbound authorization and recording
// pipeline for a file upload. Only confirmed delivery creates outbound records.
func HandleOutboundFile(ctx context.Context, deps OutboundDeps, caller Caller, req PublishFileRequest) (*OutboundFileResult, error) {
	if strings.TrimSpace(req.SessionID) == "" || strings.TrimSpace(req.FilePath) == "" {
		return nil, fmt.Errorf("session_id and file_path are required: %w", ErrInvalidInput)
	}
	metadata := mapsClone(req.Metadata)
	name := req.Filename
	if name == "" {
		name = filepath.Base(req.FilePath)
	}
	metadata["filename"] = name
	var fileReceipt *PublishFileReceipt
	result, err := handleOutbound(ctx, deps, caller, OutboundRequest{
		SessionID: req.SessionID, Conversation: req.Conversation, Text: req.TranscriptText(),
		ReplyToMessageID: req.ReplyToMessageID, IdempotencyKey: req.IdempotencyKey, Metadata: metadata,
	}, func(adapter TransportAdapter) (*PublishReceipt, error) {
		files, ok := adapter.(FileTransportAdapter)
		if !ok {
			return nil, ErrAdapterUnsupported
		}
		var err error
		fileReceipt, err = files.PublishFile(ctx, req)
		if err != nil {
			return nil, err
		}
		if fileReceipt == nil || (fileReceipt.Delivered && strings.TrimSpace(fileReceipt.FileID) == "") {
			return nil, fmt.Errorf("adapter returned no file receipt or delivered file ID: %w", ErrInvariantViolation)
		}
		if fileReceipt.Delivered {
			metadata["file_id"] = fileReceipt.FileID
		}
		return &PublishReceipt{
			MessageID: fileReceipt.FileID, Conversation: fileReceipt.Conversation,
			Delivered: fileReceipt.Delivered, FailureKind: fileReceipt.FailureKind,
		}, nil
	})
	if err != nil {
		return nil, err
	}
	return &OutboundFileResult{Receipt: *fileReceipt, DeliveryContext: result.DeliveryContext, TranscriptEntry: result.TranscriptEntry}, nil
}

func handleOutbound(ctx context.Context, deps OutboundDeps, caller Caller, req OutboundRequest, publish func(TransportAdapter) (*PublishReceipt, error)) (*OutboundResult, error) {
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
	agentBound := binding != nil && binding.AgentName != ""
	switch {
	case agentBound:
		// Agent-bound conversations defer session identity to delivery
		// time: the publish is authorized when the caller's session is
		// the one the bound agent currently resolves to.
		if deps.ResolveSessionSelector == nil {
			return nil, fmt.Errorf("conversation %s/%s is bound to agent %s but no session selector resolution is available",
				req.Conversation.Provider, req.Conversation.ConversationID, binding.AgentName)
		}
		agentSessionID, err := deps.ResolveSessionSelector(ctx, binding.AgentName)
		if err != nil {
			return nil, fmt.Errorf("resolving bound agent %q session: %w", binding.AgentName, err)
		}
		if req.SessionID != "" {
			callerSessionID, err := deps.ResolveSessionSelector(ctx, req.SessionID)
			if err != nil {
				return nil, fmt.Errorf("resolving publishing session %q: %w", req.SessionID, err)
			}
			if callerSessionID != agentSessionID {
				return nil, fmt.Errorf("session %q does not own binding for conversation %s/%s (bound to agent %s, currently session %s)",
					req.SessionID, req.Conversation.Provider, req.Conversation.ConversationID, binding.AgentName, agentSessionID)
			}
		}
		publishingSession = agentSessionID
		bindingGeneration = binding.BindingGeneration
	case binding != nil:
		if req.SessionID != "" && binding.SessionID != req.SessionID {
			// Cross-wire: the caller is trying to post into a channel owned
			// by another session. Surface it as a structured warning before
			// rejecting, so the otherwise-silent misroute is observable
			// (RCA gc-5aie6). The error contract below is unchanged.
			if deps.EmitEvent != nil {
				deps.EmitEvent(events.ExtMsgOutboundChannelMismatch, req.SessionID, OutboundChannelMismatchPayload{
					Provider:       req.Conversation.Provider,
					ConversationID: req.Conversation.ConversationID,
					PostingSession: req.SessionID,
					OwnerSession:   binding.SessionID,
				})
			}
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

	// Step 4: Publish only after authorization, using the requested transport.
	receipt, err := publish(adapter)
	if err != nil {
		return nil, fmt.Errorf("adapter publish: %w", err)
	}

	result := &OutboundResult{Receipt: *receipt}

	// If the publish was not delivered, return the receipt without recording.
	if !receipt.Delivered {
		return result, nil
	}

	// Step 5: Record delivery context (session-binding path only).
	//
	// Delivery context tracks per-binding publish state and requires a
	// non-zero BindingGeneration tied to an active binding — neither
	// applies on the group fallback path. Recording is intentionally
	// skipped there; transcript append below still runs and remains the
	// authoritative outbound record for group flows. The agent-binding
	// path is skipped for the same reason: the delivery service
	// revalidates ownership against the binding's session ID, which an
	// agent binding does not pin, so the transcript is the authoritative
	// outbound record there too.
	now := time.Now()
	if binding != nil && !agentBound {
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
