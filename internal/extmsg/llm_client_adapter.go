package extmsg

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// ProviderLLMClient is the provider name for connected-client SSE streams.
const ProviderLLMClient = "llm-client"

// LLMClientAdapter implements TransportAdapter for in-process SSE delivery.
// It routes published messages to a SubscriberRegistry keyed by the
// conversation ref. It does not support child conversations or attachments.
type LLMClientAdapter struct {
	accountID string
	registry  *SubscriberRegistry
}

// NewLLMClientAdapter creates an adapter backed by the given registry.
// accountID is the controller-assigned client_id (bead ID of the token bead).
func NewLLMClientAdapter(accountID string, registry *SubscriberRegistry) *LLMClientAdapter {
	return &LLMClientAdapter{
		accountID: strings.TrimSpace(accountID),
		registry:  registry,
	}
}

// Name returns the adapter's display name.
func (a *LLMClientAdapter) Name() string {
	return fmt.Sprintf("%s/%s", ProviderLLMClient, a.accountID)
}

// Capabilities returns the capabilities of this adapter.
func (a *LLMClientAdapter) Capabilities() AdapterCapabilities {
	return AdapterCapabilities{
		SupportsChildConversations: false,
		SupportsAttachments:        false,
	}
}

// VerifyAndNormalizeInbound is not used by the connected-client path; LLM
// clients only subscribe to outbound SSE events.
func (a *LLMClientAdapter) VerifyAndNormalizeInbound(_ context.Context, _ InboundPayload) (*ExternalInboundMessage, error) {
	return nil, fmt.Errorf("%w: llm-client adapter does not accept inbound payloads", ErrAdapterUnsupported)
}

// Publish converts the request to an SSEMessageEvent and delivers it via
// the SubscriberRegistry. If there is no current subscriber, Delivered is
// false and the receipt is returned without error.
func (a *LLMClientAdapter) Publish(_ context.Context, req PublishRequest) (*PublishReceipt, error) {
	event := NewSSEMessageEvent(
		req.Text,
		req.SessionID,
		req.Conversation,
		req.Sequence,
		time.Now().UTC(),
	)

	r := a.registry.Publish(req.Conversation, event)
	receipt := &PublishReceipt{
		Conversation: req.Conversation,
		Delivered:    r.Delivered,
	}
	if !r.Delivered {
		receipt.FailureKind = PublishFailureNotFound
	}
	return receipt, nil
}

// EnsureChildConversation is not supported by the connected-client adapter.
func (a *LLMClientAdapter) EnsureChildConversation(_ context.Context, _ ConversationRef, _ string) (*ConversationRef, error) {
	return nil, errors.New("llm-client adapter does not support child conversations")
}
