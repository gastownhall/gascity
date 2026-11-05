package extmsg

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/testutil"
)

type fileRoundTripper func(*http.Request) (*http.Response, error)

func (f fileRoundTripper) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestHTTPAdapterPublishFileWireAndFailures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   PublishFailureKind
	}{
		{"delivered", 200, `{"file_id":"F123","delivered":true,"conversation":{"provider":"slack","account_id":"T1","conversation_id":"C1","kind":"room"}}`, ""},
		{"disabled", 503, "file upload disabled", PublishFailureTransient},
		{"forbidden", 403, "outside allowed root", PublishFailureAuth},
		{"old-adapter", 404, "not found", PublishFailureNotFound},
		{"rate-limit", 429, "rate limited", PublishFailureRateLimited},
		{"invalid", 400, "invalid file", PublishFailurePermanent},
		{"malformed", 200, "not json", PublishFailureTransient},
		{"missing-id", 200, `{"delivered":true}`, PublishFailureTransient},
		{"empty-receipt", 200, `{}`, PublishFailureTransient},
		{"null-receipt", 200, `null`, PublishFailureTransient},
		{"transport-error", 0, "", PublishFailureTransient},
		{"provider-error", 200, `{"delivered":false,"failure_kind":"auth","error":"missing_scope"}`, PublishFailureAuth},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ref := ConversationRef{Provider: "slack", AccountID: "T1", ConversationID: "C1", Kind: ConversationRoom}
			req := PublishFileRequest{SessionID: "session-1", Conversation: ref, FilePath: "/staged/report.txt", Filename: "result.txt", InitialComment: "results", Title: "Run", ReplyToMessageID: "thread-1", IdempotencyKey: "key-1", Metadata: map[string]string{"run": "1"}}
			adapter := NewHTTPAdapter("file-test", "http://adapter/svc/slack", AdapterCapabilities{})
			adapter.client.Transport = fileRoundTripper(func(r *http.Request) (*http.Response, error) {
				if tc.status == 0 {
					return nil, io.ErrUnexpectedEOF
				}
				if r.URL.Path != "/svc/slack/publish-file" || r.Method != http.MethodPost || r.Header.Get(csrfHeaderName) != "true" {
					t.Fatalf("wrong callback: %s %s headers=%v", r.Method, r.URL, r.Header)
				}
				var got PublishFileRequest
				if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
					t.Fatal(err)
				}
				if got.SessionID != req.SessionID || got.FilePath != req.FilePath || got.Filename != req.Filename || got.InitialComment != req.InitialComment || got.Title != req.Title || got.ReplyToMessageID != req.ReplyToMessageID || got.IdempotencyKey != req.IdempotencyKey || got.Metadata["run"] != "1" {
					t.Fatalf("wire lost fields: %+v", got)
				}
				return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
			})
			receipt, err := adapter.PublishFile(context.Background(), req)
			if err != nil {
				t.Fatal(err)
			}
			if receipt.FailureKind != tc.want || receipt.Delivered != (tc.name == "delivered") {
				t.Fatalf("receipt=%+v want failure=%q", receipt, tc.want)
			}
			if tc.name == "delivered" && receipt.FileID != "F123" {
				t.Fatalf("file ID lost: %+v", receipt)
			}
			if tc.name == "provider-error" && receipt.Error != "missing_scope" {
				t.Fatalf("provider error lost: %+v", receipt)
			}
		})
	}
}

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

	// Pass observations from the handler goroutine to the test
	// goroutine via a buffered channel — receiving on the channel
	// happens-before the test's assertions, satisfying the Go memory
	// model. A bare shared variable would race.
	gotHeader := make(chan string, 1)
	server := httptest.NewServer(testutil.IgnoreForeignLoopbackProbes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/publish") {
			t.Errorf("expected /publish suffix, got %s", r.URL.Path)
		}
		gotHeader <- r.Header.Get(csrfHeaderName)
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
	})))
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
	select {
	case h := <-gotHeader:
		if h != "true" {
			t.Fatalf("expected %s=%q on outbound request, got %q. "+
				"This header is required by gc's /svc-proxy CSRF gate when the "+
				"adapter callback URL points at a gc-internal proxy.",
				csrfHeaderName, "true", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server handler was never invoked; request did not reach the callback URL")
	}
}

// TestHTTPAdapterEnsureChildConversationSetsCSRFHeader pins the same
// header on the sibling /child-conversation callback. Same /svc-proxy
// CSRF reasoning applies — without the header, child-conversation
// requests against a /svc-proxy callback URL also 403 silently.
func TestHTTPAdapterEnsureChildConversationSetsCSRFHeader(t *testing.T) {
	t.Parallel()

	gotHeader := make(chan string, 1)
	server := httptest.NewServer(testutil.IgnoreForeignLoopbackProbes(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("expected POST, got %s", r.Method)
		}
		if !strings.HasSuffix(r.URL.Path, "/child-conversation") {
			t.Errorf("expected /child-conversation suffix, got %s", r.URL.Path)
		}
		gotHeader <- r.Header.Get(csrfHeaderName)
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(ConversationRef{
			Provider:             "slack",
			ConversationID:       "C123-thread",
			Kind:                 ConversationThread,
			ParentConversationID: "C123",
		})
	})))
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
	select {
	case h := <-gotHeader:
		if h != "true" {
			t.Fatalf("expected %s=%q on outbound child-conversation request, got %q.",
				csrfHeaderName, "true", h)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("server handler was never invoked; request did not reach the callback URL")
	}
}
