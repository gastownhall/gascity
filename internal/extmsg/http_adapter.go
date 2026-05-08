package extmsg

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// csrfHeaderName mirrors internal/api/city_scope.go:csrfHeaderName.
// http_adapter's outbound requests must set it because adapters often
// register a callback URL pointing at gc's own /svc/<service>/publish
// proxy (proxy_process mode), which is gated by the same CSRF check
// gc's CLI client already passes. Defined locally to avoid an import
// cycle on internal/api.
const csrfHeaderName = "X-GC-Request"

// HTTPAdapter implements TransportAdapter by forwarding publish requests
// to an external HTTP service at callbackURL. Used for out-of-process
// adapters that register via the API.
type HTTPAdapter struct {
	name         string
	callbackURL  string
	capabilities AdapterCapabilities
	client       *http.Client
}

// NewHTTPAdapter creates an HTTPAdapter that forwards to callbackURL.
func NewHTTPAdapter(name, callbackURL string, caps AdapterCapabilities) *HTTPAdapter {
	return &HTTPAdapter{
		name:         name,
		callbackURL:  callbackURL,
		capabilities: caps,
		client: &http.Client{
			Timeout: 30 * time.Second,
		},
	}
}

// Name returns the adapter name.
func (a *HTTPAdapter) Name() string { return a.name }

// Capabilities returns the adapter capabilities.
func (a *HTTPAdapter) Capabilities() AdapterCapabilities { return a.capabilities }

// VerifyAndNormalizeInbound is not used for HTTP adapters — out-of-process
// adapters verify and normalize on their side before posting to the API.
func (a *HTTPAdapter) VerifyAndNormalizeInbound(_ context.Context, _ InboundPayload) (*ExternalInboundMessage, error) {
	return nil, fmt.Errorf("HTTP adapter %q does not support raw inbound verification: %w", a.name, ErrAdapterUnsupported)
}

// Publish forwards a publish request to the adapter's callback URL.
func (a *HTTPAdapter) Publish(ctx context.Context, req PublishRequest) (*PublishReceipt, error) {
	if a.callbackURL == "" {
		return &PublishReceipt{
			Conversation: req.Conversation,
			Delivered:    false,
			FailureKind:  PublishFailureUnsupported,
		}, nil
	}

	body, err := json.Marshal(req)
	if err != nil {
		return nil, fmt.Errorf("marshaling publish request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.callbackURL+"/publish", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	// See csrfHeaderName above for why this is required on outbound
	// callbacks. Harmless when callbackURL is an external HTTP listener.
	httpReq.Header.Set(csrfHeaderName, "true")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return &PublishReceipt{
			Conversation: req.Conversation,
			Delivered:    false,
			FailureKind:  PublishFailureTransient,
		}, nil
	}
	defer resp.Body.Close() //nolint:errcheck

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return &PublishReceipt{
			Conversation: req.Conversation,
			Delivered:    false,
			FailureKind:  PublishFailureTransient,
		}, nil
	}

	if resp.StatusCode >= 400 {
		kind := PublishFailureTransient
		switch {
		case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
			kind = PublishFailureAuth
		case resp.StatusCode == http.StatusNotFound:
			kind = PublishFailureNotFound
		case resp.StatusCode == http.StatusTooManyRequests:
			kind = PublishFailureRateLimited
		case resp.StatusCode >= 400 && resp.StatusCode < 500:
			kind = PublishFailurePermanent
		}
		return &PublishReceipt{
			Conversation: req.Conversation,
			Delivered:    false,
			FailureKind:  kind,
		}, nil
	}

	var wire wirePublishReceipt
	if err := json.Unmarshal(respBody, &wire); err != nil {
		// Malformed 2xx body — cannot confirm delivery.
		return &PublishReceipt{
			Conversation: req.Conversation,
			Delivered:    false,
			FailureKind:  PublishFailureTransient,
		}, nil
	}
	return wire.toPublishReceipt(), nil
}

// wirePublishReceipt mirrors PublishReceipt with the snake_case json tags
// adapters write on the /publish response body. PublishReceipt itself is
// intentionally untagged — it is exposed via the Huma API as
// OutboundResult.Receipt where PascalCase is the public contract — so we
// use this intermediate type at the wire boundary instead of changing
// PublishReceipt's serialization shape.
//
// Without this shim, json.Unmarshal into the untagged PublishReceipt
// silently zeroes MessageID, FailureKind, RetryAfter, and Metadata,
// because Go's case-insensitive field match does not bridge the
// underscore boundary (e.g. "message_id" does not match "MessageID").
type wirePublishReceipt struct {
	MessageID    string             `json:"message_id,omitempty"`
	Conversation ConversationRef    `json:"conversation"`
	Delivered    bool               `json:"delivered"`
	FailureKind  PublishFailureKind `json:"failure_kind,omitempty"`
	RetryAfter   time.Duration      `json:"retry_after,omitempty"`
	Metadata     map[string]string  `json:"metadata,omitempty"`
}

func (w wirePublishReceipt) toPublishReceipt() *PublishReceipt {
	return &PublishReceipt{
		MessageID:    w.MessageID,
		Conversation: w.Conversation,
		Delivered:    w.Delivered,
		FailureKind:  w.FailureKind,
		RetryAfter:   w.RetryAfter,
		Metadata:     w.Metadata,
	}
}

// EnsureChildConversation forwards a child conversation request to the
// adapter's callback URL.
func (a *HTTPAdapter) EnsureChildConversation(ctx context.Context, ref ConversationRef, label string) (*ConversationRef, error) {
	if a.callbackURL == "" {
		return nil, ErrAdapterUnsupported
	}

	body, err := json.Marshal(map[string]any{
		"conversation": ref,
		"label":        label,
	})
	if err != nil {
		return nil, fmt.Errorf("marshaling request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.callbackURL+"/child-conversation", bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("creating HTTP request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	httpReq.Header.Set(csrfHeaderName, "true")

	resp, err := a.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("HTTP request failed: %w", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		return nil, fmt.Errorf("adapter returned status %d", resp.StatusCode)
	}

	var childRef ConversationRef
	if err := json.NewDecoder(resp.Body).Decode(&childRef); err != nil {
		return nil, fmt.Errorf("decoding response: %w", err)
	}
	return &childRef, nil
}
