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

type testExtMsgAdapter struct {
	publishCalls []extmsg.PublishRequest
}

func (a *testExtMsgAdapter) Name() string { return "test-extmsg-adapter" }

func (a *testExtMsgAdapter) Capabilities() extmsg.AdapterCapabilities {
	return extmsg.AdapterCapabilities{}
}

func (a *testExtMsgAdapter) VerifyAndNormalizeInbound(context.Context, extmsg.InboundPayload) (*extmsg.ExternalInboundMessage, error) {
	panic("unexpected VerifyAndNormalizeInbound call")
}

func (a *testExtMsgAdapter) Publish(_ context.Context, req extmsg.PublishRequest) (*extmsg.PublishReceipt, error) {
	a.publishCalls = append(a.publishCalls, req)
	return &extmsg.PublishReceipt{
		MessageID:    "discord-msg-1",
		Conversation: req.Conversation,
		Delivered:    true,
	}, nil
}

func (a *testExtMsgAdapter) EnsureChildConversation(context.Context, extmsg.ConversationRef, string) (*extmsg.ConversationRef, error) {
	panic("unexpected EnsureChildConversation call")
}

func TestHandleExtMsgOutboundNotifiesPeerMembersAndMaterializesNamedSessions(t *testing.T) {
	fs := newSessionFakeState(t)
	srv := New(fs)

	services := extmsg.NewServices(fs.cityBeadStore)
	fs.extmsgSvc = &services
	registry := extmsg.NewAdapterRegistry()
	adapter := &testExtMsgAdapter{}
	registry.Register(extmsg.AdapterKey{Provider: "discord", AccountID: "acct-1"}, adapter)
	fs.adapterReg = registry

	source := createTestSession(t, fs.cityBeadStore, fs.sp, "Publisher")
	ref := extmsg.ConversationRef{
		ScopeID:        "guild-1",
		Provider:       "discord",
		AccountID:      "acct-1",
		ConversationID: "thread-1",
		Kind:           extmsg.ConversationThread,
	}
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	now := time.Now().UTC()
	if _, err := services.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: ref,
		SessionID:    source.ID,
		Now:          now,
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	if _, err := services.Transcript.EnsureMembership(context.Background(), extmsg.EnsureMembershipInput{
		Caller:         caller,
		Conversation:   ref,
		SessionID:      "myrig/worker",
		BackfillPolicy: extmsg.MembershipBackfillSinceJoin,
		Owner:          extmsg.MembershipOwnerManual,
		Now:            now,
	}); err != nil {
		t.Fatalf("EnsureMembership(peer): %v", err)
	}
	if _, err := session.ResolveSessionID(fs.cityBeadStore, "myrig/worker"); err == nil {
		t.Fatal("named peer should not be materialized before outbound publish")
	}

	body, err := json.Marshal(map[string]any{
		"session_id": source.ID,
		"conversation": map[string]any{
			"scope_id":        ref.ScopeID,
			"provider":        ref.Provider,
			"account_id":      ref.AccountID,
			"conversation_id": ref.ConversationID,
			"kind":            ref.Kind,
		},
		"text": "hello peers",
	})
	if err != nil {
		t.Fatalf("Marshal(body): %v", err)
	}
	req := newPostRequest("/v0/extmsg/outbound", strings.NewReader(string(body)))
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	if len(adapter.publishCalls) != 1 {
		t.Fatalf("publish calls = %d, want 1", len(adapter.publishCalls))
	}
	if adapter.publishCalls[0].Text != "hello peers" {
		t.Fatalf("publish text = %q, want hello peers", adapter.publishCalls[0].Text)
	}

	peerID, err := session.ResolveSessionID(fs.cityBeadStore, "myrig/worker")
	if err != nil {
		t.Fatalf("ResolveSessionID(myrig/worker): %v", err)
	}
	peerBead, err := fs.cityBeadStore.Get(peerID)
	if err != nil {
		t.Fatalf("Get(peer): %v", err)
	}
	peerSessionName := peerBead.Metadata["session_name"]
	if peerSessionName == "" {
		t.Fatal("materialized peer session missing session_name")
	}
	if !fs.sp.IsRunning(peerSessionName) {
		t.Fatalf("peer session %q should be running after outbound publish", peerSessionName)
	}

	peerNudges := 0
	for _, call := range fs.sp.Calls {
		if call.Method != "Nudge" {
			continue
		}
		if call.Name == source.SessionName {
			t.Fatalf("source session should not receive peer publish nudge; calls=%#v", fs.sp.Calls)
		}
		if call.Name == peerSessionName && strings.Contains(call.Message, "hello peers") {
			peerNudges++
		}
	}
	if peerNudges != 1 {
		t.Fatalf("peer nudge count = %d, want 1; calls=%#v", peerNudges, fs.sp.Calls)
	}
}
