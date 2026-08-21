package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/api/genclient"
	"github.com/gastownhall/gascity/internal/extmsg"
)

func TestCmdExtMsgBindRejectsInvalidTargetFlags(t *testing.T) {
	tests := []struct {
		name      string
		agentName string
		sessionID string
		replace   bool
		wantMsg   string
	}{
		{name: "bind requires a target", wantMsg: "gc extmsg bind: --agent or --session is required"},
		{name: "handoff requires a target", replace: true, wantMsg: "gc extmsg handoff: --agent or --session is required"},
		{name: "bind target mutually exclusive", agentName: "myrig/a", sessionID: "sess-1", wantMsg: "gc extmsg bind: --agent and --session are mutually exclusive"},
		{name: "handoff target mutually exclusive", agentName: "myrig/a", sessionID: "sess-1", replace: true, wantMsg: "gc extmsg handoff: --agent and --session are mutually exclusive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			// These validations short-circuit before the command reaches the
			// city API, so an unconfigured city does not affect the result.
			code := cmdExtMsgBind(extMsgConversationFlags{}, tt.agentName, tt.sessionID, tt.replace, false, &stdout, &stderr)
			if code != 1 {
				t.Fatalf("cmdExtMsgBind code = %d, want 1", code)
			}
			if got := stderr.String(); !strings.Contains(got, tt.wantMsg) {
				t.Fatalf("stderr = %q, want it to contain %q", got, tt.wantMsg)
			}
			if stdout.Len() != 0 {
				t.Fatalf("stdout = %q, want empty on validation failure", stdout.String())
			}
		})
	}
}

func TestConversationRefValidatesRequiredFlags(t *testing.T) {
	// scope-id is set so conversationRef does not need to load city config.
	if _, err := (&extMsgConversationFlags{scopeID: "alpha", conversationID: "123"}).conversationRef("/nonexistent"); err == nil || !strings.Contains(err.Error(), "--provider is required") {
		t.Fatalf("conversationRef(no provider) err = %v, want --provider is required", err)
	}
	if _, err := (&extMsgConversationFlags{scopeID: "alpha", provider: "telegram"}).conversationRef("/nonexistent"); err == nil || !strings.Contains(err.Error(), "--conversation-id is required") {
		t.Fatalf("conversationRef(no conversation-id) err = %v, want --conversation-id is required", err)
	}

	ref, err := (&extMsgConversationFlags{
		scopeID:        "alpha",
		provider:       "telegram",
		accountID:      "default",
		conversationID: "7113355",
		kind:           "dm",
	}).conversationRef("/nonexistent")
	if err != nil {
		t.Fatalf("conversationRef(valid): %v", err)
	}
	want := extmsg.ConversationRef{
		ScopeID:        "alpha",
		Provider:       "telegram",
		AccountID:      "default",
		ConversationID: "7113355",
		Kind:           extmsg.ConversationDM,
	}
	if ref != want {
		t.Fatalf("conversationRef = %+v, want %+v", ref, want)
	}
}

func TestConversationRefIfSetIsNilWhenUnset(t *testing.T) {
	got, err := (&extMsgConversationFlags{}).conversationRefIfSet("/nonexistent")
	if err != nil {
		t.Fatalf("conversationRefIfSet(unset): %v", err)
	}
	if got != nil {
		t.Fatalf("conversationRefIfSet(unset) = %+v, want nil so unbind filters by session/agent", got)
	}

	ref, err := (&extMsgConversationFlags{scopeID: "alpha", provider: "telegram", conversationID: "123"}).conversationRefIfSet("/nonexistent")
	if err != nil {
		t.Fatalf("conversationRefIfSet(set): %v", err)
	}
	if ref == nil || ref.Provider != "telegram" || ref.ConversationID != "123" {
		t.Fatalf("conversationRefIfSet(set) = %+v, want a populated ref", ref)
	}
}

func TestPrintExtMsgBindingOutputs(t *testing.T) {
	record := extmsg.SessionBindingRecord{
		ID:                "bind-1",
		Conversation:      extmsg.ConversationRef{Provider: "telegram", ConversationID: "7113355"},
		AgentName:         "myrig/specialist",
		Status:            extmsg.BindingActive,
		BindingGeneration: 3,
	}

	var jsonOut bytes.Buffer
	if code := printExtMsgBinding(&jsonOut, true, record, "handed off"); code != 0 {
		t.Fatalf("printExtMsgBinding(json) code = %d, want 0", code)
	}
	var decoded extmsg.SessionBindingRecord
	if err := json.Unmarshal(jsonOut.Bytes(), &decoded); err != nil {
		t.Fatalf("decode json output: %v", err)
	}
	if decoded.ID != "bind-1" || decoded.AgentName != "myrig/specialist" || decoded.BindingGeneration != 3 {
		t.Fatalf("decoded json = %+v, want the binding record", decoded)
	}

	var humanOut bytes.Buffer
	if code := printExtMsgBinding(&humanOut, false, record, "handed off"); code != 0 {
		t.Fatalf("printExtMsgBinding(human) code = %d, want 0", code)
	}
	human := humanOut.String()
	for _, want := range []string{"handed off", "telegram/7113355", "agent myrig/specialist", "binding bind-1", "generation 3"} {
		if !strings.Contains(human, want) {
			t.Fatalf("human output = %q, want it to contain %q", human, want)
		}
	}
}

// fakeConv is a ConversationRef used across extmsg reply tests.
var fakeConv = genclient.ConversationRef{
	ScopeId:        "scope-1",
	Provider:       "llm-client",
	AccountId:      "client-abc",
	ConversationId: "conv-123",
	Kind:           "direct",
}

// deliveredExtmsgHandler returns a handler that serves a successful delivered
// OutboundResult for POST …/extmsg/outbound.
func deliveredExtmsgHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/extmsg/outbound") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Receipt": map[string]any{
				"Delivered":   true,
				"FailureKind": "",
				"MessageID":   "msg-1",
				"Conversation": map[string]any{
					"scope_id":        "scope-1",
					"provider":        "llm-client",
					"account_id":      "client-abc",
					"conversation_id": "conv-123",
					"kind":            "direct",
				},
				"Metadata":   map[string]any{},
				"RetryAfter": 0,
			},
			"DeliveryContext": map[string]any{},
			"TranscriptEntry": map[string]any{
				"Actor":             map[string]any{},
				"Attachments":       nil,
				"Conversation":      map[string]any{"kind": "direct"},
				"CreatedAt":         "2026-06-21T00:00:00Z",
				"ExplicitTarget":    "",
				"ID":                "tr-1",
				"Kind":              "outbound",
				"Metadata":          map[string]any{},
				"Provenance":        "live",
				"ProviderMessageID": "",
				"ReplyToMessageID":  "",
				"SchemaVersion":     1,
				"Sequence":          int64(42),
				"SourceSessionID":   "sess-1",
				"Text":              "hello",
			},
		})
	})
}

// noSubscriberExtmsgHandler returns a handler that serves Delivered=false.
func noSubscriberExtmsgHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/extmsg/outbound") {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Receipt": map[string]any{
				"Delivered":   false,
				"FailureKind": "not_found",
				"Conversation": map[string]any{
					"scope_id":        "scope-1",
					"provider":        "llm-client",
					"account_id":      "client-abc",
					"conversation_id": "conv-123",
					"kind":            "direct",
				},
				"Metadata":   map[string]any{},
				"RetryAfter": 0,
			},
			"DeliveryContext": map[string]any{},
			"TranscriptEntry": map[string]any{
				"Actor":             map[string]any{},
				"Attachments":       nil,
				"Conversation":      map[string]any{"kind": "direct"},
				"CreatedAt":         "2026-06-21T00:00:00Z",
				"ExplicitTarget":    "",
				"ID":                "tr-2",
				"Kind":              "outbound",
				"Metadata":          map[string]any{},
				"Provenance":        "live",
				"ProviderMessageID": "",
				"ReplyToMessageID":  "",
				"SchemaVersion":     1,
				"Sequence":          int64(7),
				"SourceSessionID":   "sess-1",
				"Text":              "hello",
			},
		})
	})
}

// errorExtmsgHandler returns a handler that responds with 500.
func errorExtmsgHandler(t *testing.T) http.Handler {
	t.Helper()
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/problem+json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"status": 500,
			"title":  "Internal Server Error",
			"detail": "outbound: internal error",
		})
	})
}

// makeEnvelopeJSON returns a JSON-encoded externalOriginEnvelope for conv.
func makeEnvelopeJSON(conv genclient.ConversationRef) string {
	b, _ := json.Marshal(externalOriginEnvelope{Conversation: conv})
	return string(b)
}

// TestExtmsgReplyFromContextRef verifies that runExtmsgReply reads the
// ConversationRef from GC_EXTERNAL_ORIGIN when refJSON is empty.
func TestExtmsgReplyFromContextRef(t *testing.T) {
	t.Setenv("GC_EXTERNAL_ORIGIN", makeEnvelopeJSON(fakeConv))
	t.Setenv("GC_SESSION_ID", "sess-1")

	srv := httptest.NewServer(deliveredExtmsgHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := runExtmsgReply(c, "" /* refJSON */, false, nil, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runExtmsgReply = %d, want 0; stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "delivered conv-123 seq:42") {
		t.Errorf("stdout = %q, want to contain \"delivered conv-123 seq:42\"", stdout.String())
	}
}

// TestExtmsgReplyWithExplicitRef verifies that --ref overrides GC_EXTERNAL_ORIGIN.
func TestExtmsgReplyWithExplicitRef(t *testing.T) {
	// GC_EXTERNAL_ORIGIN contains a different conversation — explicit --ref should win.
	otherConv := genclient.ConversationRef{
		Provider: "other", AccountId: "other-acc", ConversationId: "other-conv", Kind: "direct",
	}
	t.Setenv("GC_EXTERNAL_ORIGIN", makeEnvelopeJSON(otherConv))
	t.Setenv("GC_SESSION_ID", "sess-1")

	explicitRef, _ := json.Marshal(fakeConv)

	// The server records what conversation_id was in the request body.
	var gotConvID string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/extmsg/outbound") {
			http.NotFound(w, r)
			return
		}
		var body struct {
			Conversation *genclient.ConversationRef `json:"conversation"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		if body.Conversation != nil {
			gotConvID = body.Conversation.ConversationId
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"Receipt": map[string]any{
				"Delivered": true,
				"Conversation": map[string]any{
					"conversation_id": body.Conversation.ConversationId,
					"kind":            "direct",
				},
				"Metadata":   map[string]any{},
				"RetryAfter": 0,
			},
			"DeliveryContext": map[string]any{},
			"TranscriptEntry": map[string]any{
				"Actor": map[string]any{}, "Attachments": nil,
				"Conversation": map[string]any{"kind": "direct"},
				"CreatedAt":    "2026-06-21T00:00:00Z",
				"Sequence":     int64(5), "Kind": "outbound", "Provenance": "live",
				"Metadata": map[string]any{},
			},
		})
	}))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := runExtmsgReply(c, string(explicitRef), false, nil, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runExtmsgReply = %d, want 0; stderr: %s", code, stderr.String())
	}
	if gotConvID != "conv-123" {
		t.Errorf("request used conversation_id %q, want %q (explicit --ref should override context)", gotConvID, "conv-123")
	}
}

// TestExtmsgReplyMissingRefFails verifies that missing --ref and missing
// GC_EXTERNAL_ORIGIN produces a non-zero exit with a clear error message.
func TestExtmsgReplyMissingRefFails(t *testing.T) {
	t.Setenv("GC_EXTERNAL_ORIGIN", "")
	t.Setenv("GC_SESSION_ID", "sess-1")

	srv := httptest.NewServer(deliveredExtmsgHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := runExtmsgReply(c, "" /* refJSON */, false, nil, false, &stdout, &stderr)
	if code == 0 {
		t.Fatal("runExtmsgReply = 0, want non-zero (no ref, no env)")
	}
	if !strings.Contains(stderr.String(), envExternalOrigin) {
		t.Errorf("stderr = %q, want mention of %s", stderr.String(), envExternalOrigin)
	}
}

// TestExtmsgReplyNonZeroExitOnOutboundError verifies that API errors produce
// a non-zero exit code.
func TestExtmsgReplyNonZeroExitOnOutboundError(t *testing.T) {
	t.Setenv("GC_EXTERNAL_ORIGIN", makeEnvelopeJSON(fakeConv))
	t.Setenv("GC_SESSION_ID", "sess-1")

	srv := httptest.NewServer(errorExtmsgHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := runExtmsgReply(c, "", false, nil, false, &stdout, &stderr)
	if code == 0 {
		t.Fatal("runExtmsgReply = 0, want non-zero on API error")
	}
	if !strings.Contains(stderr.String(), "error") {
		t.Errorf("stderr = %q, want error mention", stderr.String())
	}
}

// TestExtmsgReplyNoSubscriberExitsZero verifies that Delivered=false exits 0
// (designer requirement I1: no-subscriber is not an error).
func TestExtmsgReplyNoSubscriberExitsZero(t *testing.T) {
	t.Setenv("GC_EXTERNAL_ORIGIN", makeEnvelopeJSON(fakeConv))
	t.Setenv("GC_SESSION_ID", "sess-1")

	srv := httptest.NewServer(noSubscriberExtmsgHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := runExtmsgReply(c, "", false, nil, false, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runExtmsgReply = %d, want 0 (no-subscriber is not an error); stderr: %s", code, stderr.String())
	}
	if !strings.Contains(stdout.String(), "no-subscriber") {
		t.Errorf("stdout = %q, want to contain \"no-subscriber\"", stdout.String())
	}
}

// TestExtmsgReplyJSONOutput verifies --json output shape.
func TestExtmsgReplyJSONOutput(t *testing.T) {
	t.Setenv("GC_EXTERNAL_ORIGIN", makeEnvelopeJSON(fakeConv))
	t.Setenv("GC_SESSION_ID", "sess-1")

	srv := httptest.NewServer(deliveredExtmsgHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := runExtmsgReply(c, "", false, nil, true /* jsonOutput */, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("runExtmsgReply = %d, want 0; stderr: %s", code, stderr.String())
	}

	var result extmsgReplyJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("invalid JSON output: %v\nraw: %s", err, stdout.String())
	}
	if !result.Delivered {
		t.Errorf("delivered = false, want true")
	}
	if result.ConversationID != "conv-123" {
		t.Errorf("conversation_id = %q, want conv-123", result.ConversationID)
	}
	if result.Sequence != 42 {
		t.Errorf("sequence = %d, want 42", result.Sequence)
	}
}

// TestExtmsgReplyStdinMutualExclusion verifies that passing both --stdin and
// a positional argument exits non-zero.
func TestExtmsgReplyStdinMutualExclusion(t *testing.T) {
	t.Setenv("GC_EXTERNAL_ORIGIN", makeEnvelopeJSON(fakeConv))
	t.Setenv("GC_SESSION_ID", "sess-1")

	srv := httptest.NewServer(deliveredExtmsgHandler(t))
	defer srv.Close()
	c := api.NewCityScopedClient(srv.URL, "test-city")

	var stdout, stderr bytes.Buffer
	code := runExtmsgReply(c, "", true /* fromStdin */, []string{"extra arg"}, false, &stdout, &stderr)
	if code == 0 {
		t.Fatal("runExtmsgReply = 0, want non-zero (stdin + positional arg conflict)")
	}
	if !strings.Contains(stderr.String(), "mutually exclusive") {
		t.Errorf("stderr = %q, want mutually exclusive message", stderr.String())
	}
}
