package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/extmsg"
	"github.com/gastownhall/gascity/internal/session"
)

func TestHandleExtMsgInboundDefaultRouteBindsAndColdWakes(t *testing.T) {
	fs, srv, services, ref := newExtMsgAgentBindingFixture(t)
	fs.cfg.ExtMsg = config.ExtMsgConfig{
		DefaultRoutes: []config.ExtMsgDefaultRoute{
			{Provider: "discord", Agent: "myrig/worker"},
		},
	}

	if _, err := session.ResolveSessionID(fs.cityBeadStore, "myrig/worker"); err == nil {
		t.Fatal("named agent should not have a session before the inbound message")
	}

	rec := postExtMsg(t, fs, srv, "/extmsg/inbound", map[string]any{
		"message": map[string]any{
			"provider_message_id": "msg-default-1",
			"conversation":        conversationBody(ref),
			"actor":               map[string]any{"id": "user-1", "display_name": "User One", "is_bot": false},
			"text":                "hello, is anyone responsible for this chat?",
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

	binding, err := services.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if binding == nil || binding.AgentName != "myrig/worker" {
		t.Fatalf("binding = %#v, want sticky agent binding myrig/worker", binding)
	}

	// The notify fan-out cold-wakes the routed agent's named session.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if id, err := session.ResolveSessionID(fs.cityBeadStore, "myrig/worker"); err == nil && id != "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for default-routed inbound to cold-wake myrig/worker")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestHandleExtMsgInboundDefaultRouteUnknownAgentStaysUnbound(t *testing.T) {
	fs, srv, services, ref := newExtMsgAgentBindingFixture(t)
	fs.cfg.ExtMsg = config.ExtMsgConfig{
		DefaultRoutes: []config.ExtMsgDefaultRoute{
			{Provider: "discord", Agent: "myrig/ghost"},
		},
	}

	rec := postExtMsg(t, fs, srv, "/extmsg/inbound", map[string]any{
		"message": map[string]any{
			"provider_message_id": "msg-default-2",
			"conversation":        conversationBody(ref),
			"actor":               map[string]any{"id": "user-1", "display_name": "User One", "is_bot": false},
			"text":                "hello?",
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
	if result.TargetSessionID != "" || result.TargetAgentName != "" {
		t.Fatalf("result routed (%q/%q), want unrouted on unresolvable route agent", result.TargetSessionID, result.TargetAgentName)
	}
	binding, err := services.Bindings.ResolveByConversation(context.Background(), ref)
	if err != nil {
		t.Fatalf("ResolveByConversation: %v", err)
	}
	if binding != nil {
		t.Fatalf("binding = %#v, want none for unresolvable route agent", binding)
	}
}
