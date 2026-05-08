package extmsg

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestHTTPAdapterPublishSetsCSRFHeader pins that HTTPAdapter.Publish sets
// X-GC-Request on the outbound request. When an adapter registers a
// callback URL pointing at gc's own /svc/<service>/publish proxy
// (proxy_process mode — the standard slack-pack registration shape), gc's
// CSRF gate at internal/api/handler_services.go:serviceRequestAllowed
// requires the header on private-service-proxy mutations and 403s the
// request without it. Without the header, every Publish call silently
// returns FailureKind=auth and the message never reaches the actual
// adapter. See gastownhall/gascity#1817.
func TestHTTPAdapterPublishSetsCSRFHeader(t *testing.T) {
	t.Parallel()

	var gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/publish") {
			t.Errorf("expected /publish suffix, got %s", r.URL.Path)
		}
		gotHeader = r.Header.Get("X-GC-Request")
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wirePublishReceipt{
			MessageID: "ts-100",
			Conversation: ConversationRef{
				Provider:       "slack",
				ConversationID: "C123",
				Kind:           ConversationRoom,
			},
			Delivered: true,
		})
	}))
	defer server.Close()

	adapter := NewHTTPAdapter("test", server.URL, AdapterCapabilities{})
	receipt, err := adapter.Publish(context.Background(), PublishRequest{
		Conversation: ConversationRef{
			Provider:       "slack",
			ConversationID: "C123",
			Kind:           ConversationRoom,
		},
		Text: "hello",
	})
	if err != nil {
		t.Fatalf("Publish: %v", err)
	}
	if !receipt.Delivered {
		t.Fatalf("expected Delivered=true, got receipt=%+v", receipt)
	}
	if gotHeader == "" {
		t.Fatalf("expected X-GC-Request header on outbound request; got empty. " +
			"This header is required by gc's /svc-proxy CSRF gate when the " +
			"adapter callback URL points at a gc-internal proxy.")
	}
}

// TestHTTPAdapterEnsureChildConversationSetsCSRFHeader pins the same
// header on the sibling /child-conversation callback. Same /svc-proxy
// CSRF reasoning applies — without the header, child-conversation
// requests against a /svc-proxy callback URL also 403 silently.
func TestHTTPAdapterEnsureChildConversationSetsCSRFHeader(t *testing.T) {
	t.Parallel()

	var gotHeader string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/child-conversation") {
			t.Errorf("expected /child-conversation suffix, got %s", r.URL.Path)
		}
		gotHeader = r.Header.Get("X-GC-Request")
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ConversationRef{
			Provider:             "slack",
			ConversationID:       "C123-thread",
			Kind:                 ConversationThread,
			ParentConversationID: "C123",
		})
	}))
	defer server.Close()

	adapter := NewHTTPAdapter("test", server.URL, AdapterCapabilities{})
	parent := ConversationRef{
		Provider:       "slack",
		ConversationID: "C123",
		Kind:           ConversationRoom,
	}
	if _, err := adapter.EnsureChildConversation(context.Background(), parent, "test-label"); err != nil {
		t.Fatalf("EnsureChildConversation: %v", err)
	}
	if gotHeader == "" {
		t.Fatalf("expected X-GC-Request header on outbound child-conversation request; got empty.")
	}
}
