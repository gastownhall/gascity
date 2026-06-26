package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

func newExtMsgAgentBindingFixture(t *testing.T) (*fakeState, *Server, *extmsg.Services, extmsg.ConversationRef) {
	t.Helper()
	fs := newSessionFakeState(t)
	srv := New(fs)
	services := extmsg.NewServices(fs.cityBeadStore)
	fs.extmsgSvc = &services
	registry := extmsg.NewAdapterRegistry()
	registry.Register(extmsg.AdapterKey{Provider: "discord", AccountID: "acct-1"}, &testExtMsgAdapter{})
	fs.adapterReg = registry
	ref := extmsg.ConversationRef{
		ScopeID:        "guild-1",
		Provider:       "discord",
		AccountID:      "acct-1",
		ConversationID: "thread-agent",
		Kind:           extmsg.ConversationThread,
	}
	return fs, srv, &services, ref
}

func conversationBody(ref extmsg.ConversationRef) map[string]any {
	return map[string]any{
		"scope_id":        ref.ScopeID,
		"provider":        ref.Provider,
		"account_id":      ref.AccountID,
		"conversation_id": ref.ConversationID,
		"kind":            ref.Kind,
	}
}

func postExtMsg(t *testing.T, fs *fakeState, srv *Server, path string, body map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("Marshal(body): %v", err)
	}
	req := newPostRequest(cityURL(fs, path), strings.NewReader(string(raw)))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)
	return rec
}

func TestHandleExtMsgBindAgentNamePersistsConfiguredIdentity(t *testing.T) {
	fs, srv, services, ref := newExtMsgAgentBindingFixture(t)

	rec := postExtMsg(t, fs, srv, "/extmsg/bind", map[string]any{
		"agent_name":   "myrig/worker",
		"conversation": conversationBody(ref),
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}

	binding, err := services.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if binding == nil || binding.AgentName != "myrig/worker" || binding.SessionID != "" {
		t.Fatalf("binding = %#v, want agent binding myrig/worker", binding)
	}

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	members, err := services.Transcript.ListMemberships(context.Background(), caller, ref)
	if err != nil {
		t.Fatalf("ListMemberships: %v", err)
	}
	if len(members) != 1 || members[0].SessionID != "myrig/worker" {
		t.Fatalf("memberships = %#v, want one keyed myrig/worker", members)
	}
}

func TestHandleExtMsgBindRejectsAmbiguousOrMissingTarget(t *testing.T) {
	fs, srv, _, ref := newExtMsgAgentBindingFixture(t)

	rec := postExtMsg(t, fs, srv, "/extmsg/bind", map[string]any{
		"conversation": conversationBody(ref),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status(neither) = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	rec = postExtMsg(t, fs, srv, "/extmsg/bind", map[string]any{
		"session_id":   "sess-1",
		"agent_name":   "myrig/worker",
		"conversation": conversationBody(ref),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status(both) = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestHandleExtMsgBindRejectsUnknownAgentName(t *testing.T) {
	fs, srv, _, ref := newExtMsgAgentBindingFixture(t)

	rec := postExtMsg(t, fs, srv, "/extmsg/bind", map[string]any{
		"agent_name":   "myrig/ghost",
		"conversation": conversationBody(ref),
	})
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestHandleExtMsgInboundAgentBoundColdWakesNamedSession(t *testing.T) {
	fs, srv, services, ref := newExtMsgAgentBindingFixture(t)

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	if _, err := services.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: ref,
		AgentName:    "myrig/worker",
		Now:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}
	if _, err := session.ResolveSessionID(fs.cityBeadStore, "myrig/worker"); err == nil {
		t.Fatal("named agent should not have a session before the inbound message")
	}

	rec := postExtMsg(t, fs, srv, "/extmsg/inbound", map[string]any{
		"message": map[string]any{
			"provider_message_id": "msg-1",
			"conversation":        conversationBody(ref),
			"actor":               map[string]any{"id": "user-1", "display_name": "User One", "is_bot": false},
			"text":                "anyone there?",
			"received_at":         time.Now().UTC().Format(time.RFC3339),
		},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var result extmsg.InboundResult
	if err := json.NewDecoder(rec.Body).Decode(&result); err != nil {
		t.Fatalf("decode inbound result: %v", err)
	}
	if result.TargetAgentName != "myrig/worker" {
		t.Fatalf("TargetAgentName = %q, want myrig/worker", result.TargetAgentName)
	}

	// The notify fan-out materializes the bound agent's named session —
	// the cold-wake. It runs in a background goroutine, so poll briefly.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if id, err := session.ResolveSessionID(fs.cityBeadStore, "myrig/worker"); err == nil && id != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for agent-bound inbound to cold-wake myrig/worker")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandleExtMsgOutboundAgentBoundAuthorizesResolvedAgentSession(t *testing.T) {
	fs, srv, services, ref := newExtMsgAgentBindingFixture(t)

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	if _, err := services.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: ref,
		AgentName:    "myrig/worker",
		Now:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Bind(agent): %v", err)
	}

	agentSessionID, err := srv.resolveSessionIDMaterializingNamed(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("materialize myrig/worker: %v", err)
	}
	imposter := createTestSession(t, fs.cityBeadStore, fs.sp, "Imposter")

	rec := postExtMsg(t, fs, srv, "/extmsg/outbound", map[string]any{
		"session_id":   imposter.ID,
		"conversation": conversationBody(ref),
		"text":         "not my conversation",
	})
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status(imposter) = %d, want %d; body: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}

	rec = postExtMsg(t, fs, srv, "/extmsg/outbound", map[string]any{
		"session_id":   agentSessionID,
		"conversation": conversationBody(ref),
		"text":         "reply from the bound agent",
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("status(agent) = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
}
