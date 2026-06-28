package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/extmsg"
)

func TestClientBindExtMsgConversationHandoff(t *testing.T) {
	var gotBody struct {
		Conversation extmsg.ConversationRef `json:"conversation"`
		SessionID    string                 `json:"session_id"`
		AgentName    string                 `json:"agent_name"`
		Replace      bool                   `json:"replace"`
	}
	var gotHeader string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/city/alpha/extmsg/bind" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		gotHeader = r.Header.Get("X-GC-Request")
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode bind body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(extmsg.SessionBindingRecord{ //nolint:errcheck
			ID:                "bind-1",
			Conversation:      gotBody.Conversation,
			AgentName:         gotBody.AgentName,
			Status:            extmsg.BindingActive,
			BoundAt:           time.Now().UTC(),
			BindingGeneration: 2,
		})
	}))
	defer ts.Close()

	ref := extmsg.ConversationRef{
		ScopeID:        "alpha",
		Provider:       "telegram",
		AccountID:      "default",
		ConversationID: "7113355",
		Kind:           extmsg.ConversationDM,
	}
	c := NewCityScopedClient(ts.URL, "alpha")
	record, err := c.BindExtMsgConversation(ExtMsgBindSpec{
		Conversation: ref,
		AgentName:    "myrig/specialist",
		Replace:      true,
	})
	if err != nil {
		t.Fatalf("BindExtMsgConversation: %v", err)
	}
	if gotHeader != "true" {
		t.Fatalf("X-GC-Request = %q, want true", gotHeader)
	}
	if gotBody.AgentName != "myrig/specialist" || !gotBody.Replace || gotBody.SessionID != "" {
		t.Fatalf("bind body = %+v, want agent handoff", gotBody)
	}
	if gotBody.Conversation != ref {
		t.Fatalf("conversation = %+v, want %+v", gotBody.Conversation, ref)
	}
	if record.ID != "bind-1" || record.AgentName != "myrig/specialist" || record.BindingGeneration != 2 {
		t.Fatalf("record = %+v, want decoded binding", record)
	}
}

func TestClientBindExtMsgConversationPreservesSessionName(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/city/alpha/extmsg/bind" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		var body struct {
			Conversation extmsg.ConversationRef `json:"conversation"`
			SessionID    string                 `json:"session_id"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode bind body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(extmsg.SessionBindingRecord{ //nolint:errcheck
			ID:                "bind-1",
			Conversation:      body.Conversation,
			SessionID:         body.SessionID,
			SessionName:       "myrig/worker-1",
			Status:            extmsg.BindingActive,
			BoundAt:           time.Now().UTC(),
			BindingGeneration: 1,
		})
	}))
	defer ts.Close()

	ref := extmsg.ConversationRef{
		ScopeID:        "alpha",
		Provider:       "telegram",
		AccountID:      "default",
		ConversationID: "7113355",
		Kind:           extmsg.ConversationDM,
	}
	c := NewCityScopedClient(ts.URL, "alpha")
	record, err := c.BindExtMsgConversation(ExtMsgBindSpec{
		Conversation: ref,
		SessionID:    "sess-123",
	})
	if err != nil {
		t.Fatalf("BindExtMsgConversation: %v", err)
	}
	// SessionName is the respawn-stable identity; the wire->domain decode must
	// carry it through, not silently drop it.
	if record.SessionName != "myrig/worker-1" {
		t.Fatalf("record.SessionName = %q, want myrig/worker-1", record.SessionName)
	}
	if record.SessionID != "sess-123" {
		t.Fatalf("record.SessionID = %q, want sess-123", record.SessionID)
	}
}

func TestClientUnbindExtMsgConversationByAgent(t *testing.T) {
	var gotBody struct {
		Conversation *extmsg.ConversationRef `json:"conversation"`
		SessionID    string                  `json:"session_id"`
		AgentName    string                  `json:"agent_name"`
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/v0/city/alpha/extmsg/unbind" {
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.String())
		}
		if err := json.NewDecoder(r.Body).Decode(&gotBody); err != nil {
			t.Fatalf("decode unbind body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{ //nolint:errcheck
			"unbound": []extmsg.SessionBindingRecord{{ID: "bind-9", AgentName: gotBody.AgentName, Status: extmsg.BindingEnded}},
		})
	}))
	defer ts.Close()

	c := NewCityScopedClient(ts.URL, "alpha")
	unbound, err := c.UnbindExtMsgConversation(nil, "", "myrig/frontdesk")
	if err != nil {
		t.Fatalf("UnbindExtMsgConversation: %v", err)
	}
	if gotBody.Conversation != nil || gotBody.AgentName != "myrig/frontdesk" || gotBody.SessionID != "" {
		t.Fatalf("unbind body = %+v, want agent-only filter", gotBody)
	}
	if len(unbound) != 1 || unbound[0].ID != "bind-9" || unbound[0].Status != extmsg.BindingEnded {
		t.Fatalf("unbound = %+v, want one ended binding", unbound)
	}
}
