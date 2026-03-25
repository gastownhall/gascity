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

	var receipt PublishReceipt
	if err := json.Unmarshal(respBody, &receipt); err != nil {
		// Malformed 2xx body — cannot confirm delivery.
		return &PublishReceipt{
			Conversation: req.Conversation,
			Delivered:    false,
			FailureKind:  PublishFailureTransient,
		}, nil
	}
	return &receipt, nil
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
