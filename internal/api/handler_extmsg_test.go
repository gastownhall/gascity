package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/extmsg"
)

// testTransportAdapter implements extmsg.TransportAdapter for handler tests.
type testTransportAdapter struct {
	name string
}

func (a *testTransportAdapter) Name() string { return a.name }
func (a *testTransportAdapter) Capabilities() extmsg.AdapterCapabilities {
	return extmsg.AdapterCapabilities{}
}

func (a *testTransportAdapter) VerifyAndNormalizeInbound(_ context.Context, p extmsg.InboundPayload) (*extmsg.ExternalInboundMessage, error) {
	return &extmsg.ExternalInboundMessage{
		ProviderMessageID: "msg-from-adapter",
		Conversation: extmsg.ConversationRef{
			ScopeID:        "scope",
			Provider:       "test-provider",
			AccountID:      "test-account",
			ConversationID: "chan-1",
			Kind:           extmsg.ConversationRoom,
		},
		Actor:      extmsg.ExternalActor{ID: "user-1", DisplayName: "Alice"},
		Text:       string(p.Body),
		ReceivedAt: time.Now(),
	}, nil
}

func (a *testTransportAdapter) Publish(_ context.Context, req extmsg.PublishRequest) (*extmsg.PublishReceipt, error) {
	return &extmsg.PublishReceipt{
		MessageID:    "out-001",
		Conversation: req.Conversation,
		Delivered:    true,
	}, nil
}

func (a *testTransportAdapter) EnsureChildConversation(_ context.Context, _ extmsg.ConversationRef, _ string) (*extmsg.ConversationRef, error) {
	return nil, extmsg.ErrAdapterUnsupported
}

func newExtMsgFakeState(t *testing.T) *fakeState {
	t.Helper()
	fs := newFakeState(t)
	store := beads.NewMemStore()
	svc := extmsg.NewServices(store)
	fs.extmsgSvc = &svc
	fs.adapterReg = extmsg.NewAdapterRegistry()
	fs.cityBeadStore = store
	return fs
}

func TestHandleExtMsgAdapterLifecycle(t *testing.T) {
	fs := newExtMsgFakeState(t)
	srv := New(fs)

	// Register adapter.
	body := `{"provider":"test-provider","account_id":"test-account","name":"my-adapter","callback_url":"http://localhost:9999"}`
	req := newPostRequest("/v0/extmsg/adapters", bytes.NewBufferString(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// List adapters.
	req = httptest.NewRequest("GET", "/v0/extmsg/adapters", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var listResp struct {
		Items []struct {
			Provider  string `json:"provider"`
			AccountID string `json:"account_id"`
			Name      string `json:"name"`
		} `json:"items"`
	}
	if err := json.NewDecoder(w.Body).Decode(&listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Items) != 1 {
		t.Fatalf("expected 1 adapter, got %d", len(listResp.Items))
	}
	if listResp.Items[0].Name != "my-adapter" {
		t.Fatalf("expected my-adapter, got %s", listResp.Items[0].Name)
	}

	// Unregister adapter.
	body = `{"provider":"test-provider","account_id":"test-account"}`
	req = httptest.NewRequest("DELETE", "/v0/extmsg/adapters", bytes.NewBufferString(body))
	req.Header.Set("X-GC-Request", "true")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Verify empty.
	req = httptest.NewRequest("GET", "/v0/extmsg/adapters", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)
	if err := json.NewDecoder(w.Body).Decode(&listResp); err != nil {
		t.Fatal(err)
	}
	if len(listResp.Items) != 0 {
		t.Fatalf("expected 0 adapters after unregister, got %d", len(listResp.Items))
	}
}

func TestHandleExtMsgBindingLifecycle(t *testing.T) {
	fs := newExtMsgFakeState(t)
	srv := New(fs)

	conv := extmsg.ConversationRef{
		ScopeID:        "scope",
		Provider:       "discord",
		AccountID:      "bot-1",
		ConversationID: "chan-42",
		Kind:           extmsg.ConversationRoom,
	}

	// Bind.
	bindBody, _ := json.Marshal(map[string]any{
		"conversation": conv,
		"session_id":   "session-alpha",
	})
	req := newPostRequest("/v0/extmsg/bindings", bytes.NewBuffer(bindBody))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("bind: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// List bindings by session.
	req = httptest.NewRequest("GET", "/v0/extmsg/bindings?session_id=session-alpha", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var listResp struct {
		Items []extmsg.SessionBindingRecord `json:"items"`
		Total int                           `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&listResp); err != nil {
		t.Fatal(err)
	}
	if listResp.Total != 1 {
		t.Fatalf("expected 1 binding, got %d", listResp.Total)
	}

	// Unbind.
	unbindBody, _ := json.Marshal(map[string]any{
		"conversation": conv,
		"session_id":   "session-alpha",
	})
	req = httptest.NewRequest("DELETE", "/v0/extmsg/bindings", bytes.NewBuffer(unbindBody))
	req.Header.Set("X-GC-Request", "true")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("unbind: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleExtMsgInboundNormalized(t *testing.T) {
	fs := newExtMsgFakeState(t)
	srv := New(fs)

	conv := extmsg.ConversationRef{
		ScopeID:        "scope",
		Provider:       "discord",
		AccountID:      "bot-1",
		ConversationID: "chan-42",
		Kind:           extmsg.ConversationRoom,
	}

	// Create a binding first.
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	_, err := fs.extmsgSvc.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: conv,
		SessionID:    "worker",
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Send normalized inbound.
	msg := extmsg.ExternalInboundMessage{
		ProviderMessageID: "msg-001",
		Conversation:      conv,
		Actor:             extmsg.ExternalActor{ID: "user-1", DisplayName: "Alice"},
		Text:              "hello from discord",
		ReceivedAt:        time.Now(),
	}
	body, _ := json.Marshal(map[string]any{"message": msg})
	req := newPostRequest("/v0/extmsg/inbound", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("inbound: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	respBody, _ := io.ReadAll(w.Body)
	var result extmsg.InboundResult
	if err := json.Unmarshal(respBody, &result); err != nil {
		t.Fatal(err)
	}
	if result.TargetSessionID != "worker" {
		t.Fatalf("expected worker, got %s", result.TargetSessionID)
	}
}

func TestHandleExtMsgInboundRawPayload(t *testing.T) {
	fs := newExtMsgFakeState(t)
	srv := New(fs)

	conv := extmsg.ConversationRef{
		ScopeID:        "scope",
		Provider:       "test-provider",
		AccountID:      "test-account",
		ConversationID: "chan-1",
		Kind:           extmsg.ConversationRoom,
	}

	// Register a real adapter.
	fs.adapterReg.Register(
		extmsg.AdapterKey{Provider: "test-provider", AccountID: "test-account"},
		&testTransportAdapter{name: "test"},
	)

	// Bind.
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	_, err := fs.extmsgSvc.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: conv,
		SessionID:    "worker",
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Send raw payload.
	body, _ := json.Marshal(map[string]any{
		"provider":   "test-provider",
		"account_id": "test-account",
		"payload":    []byte("hello raw"),
	})
	req := newPostRequest("/v0/extmsg/inbound", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("inbound raw: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleExtMsgOutbound(t *testing.T) {
	fs := newExtMsgFakeState(t)
	srv := New(fs)

	conv := extmsg.ConversationRef{
		ScopeID:        "scope",
		Provider:       "test-provider",
		AccountID:      "test-account",
		ConversationID: "chan-1",
		Kind:           extmsg.ConversationRoom,
	}

	// Register adapter and bind.
	fs.adapterReg.Register(
		extmsg.AdapterKey{Provider: "test-provider", AccountID: "test-account"},
		&testTransportAdapter{name: "test"},
	)
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	_, err := fs.extmsgSvc.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: conv,
		SessionID:    "worker",
		Now:          time.Now(),
	})
	if err != nil {
		t.Fatalf("bind: %v", err)
	}

	// Publish outbound.
	body, _ := json.Marshal(map[string]any{
		"session_id":   "worker",
		"conversation": conv,
		"text":         "hello from gc",
	})
	req := newPostRequest("/v0/extmsg/outbound", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("outbound: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleExtMsgGroupAndParticipants(t *testing.T) {
	fs := newExtMsgFakeState(t)
	srv := New(fs)

	// Ensure group.
	body, _ := json.Marshal(map[string]any{
		"root_conversation": extmsg.ConversationRef{
			ScopeID:        "scope",
			Provider:       "discord",
			AccountID:      "bot-1",
			ConversationID: "chan-99",
			Kind:           extmsg.ConversationRoom,
		},
		"mode":           "launcher",
		"default_handle": "worker",
	})
	req := newPostRequest("/v0/extmsg/groups", bytes.NewBuffer(body))
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("ensure group: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	var group extmsg.ConversationGroupRecord
	if err := json.NewDecoder(w.Body).Decode(&group); err != nil {
		t.Fatal(err)
	}

	// Upsert participant.
	body, _ = json.Marshal(map[string]any{
		"group_id":   group.ID,
		"handle":     "worker",
		"session_id": "session-beta",
		"public":     true,
	})
	req = newPostRequest("/v0/extmsg/groups/participants", bytes.NewBuffer(body))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("upsert participant: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	// Remove participant.
	body, _ = json.Marshal(map[string]any{
		"group_id": group.ID,
		"handle":   "worker",
	})
	req = httptest.NewRequest("DELETE", "/v0/extmsg/groups/participants", bytes.NewBuffer(body))
	req.Header.Set("X-GC-Request", "true")
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("remove participant: expected 200, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleExtMsgGroupLookup(t *testing.T) {
	fs := newExtMsgFakeState(t)
	srv := New(fs)

	// GET before group exists should return 404.
	req := httptest.NewRequest("GET", "/v0/extmsg/groups?scope_id=scope&provider=discord&account_id=bot-1&conversation_id=chan-99&kind=room", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("expected 404 for missing group, got %d: %s", w.Code, w.Body.String())
	}

	// Create group via POST.
	body, _ := json.Marshal(map[string]any{
		"root_conversation": extmsg.ConversationRef{
			ScopeID:        "scope",
			Provider:       "discord",
			AccountID:      "bot-1",
			ConversationID: "chan-99",
			Kind:           extmsg.ConversationRoom,
		},
		"mode":           "launcher",
		"default_handle": "worker",
	})
	req = newPostRequest("/v0/extmsg/groups", bytes.NewBuffer(body))
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("ensure group: expected 201, got %d: %s", w.Code, w.Body.String())
	}

	// GET after creation should return the group.
	req = httptest.NewRequest("GET", "/v0/extmsg/groups?scope_id=scope&provider=discord&account_id=bot-1&conversation_id=chan-99&kind=room", nil)
	w = httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("expected 200 for existing group, got %d: %s", w.Code, w.Body.String())
	}
}

func TestHandleExtMsgTranscript(t *testing.T) {
	fs := newExtMsgFakeState(t)
	srv := New(fs)

	conv := extmsg.ConversationRef{
		ScopeID:        "scope",
		Provider:       "discord",
		AccountID:      "bot-1",
		ConversationID: "chan-42",
		Kind:           extmsg.ConversationRoom,
	}

	// Append a transcript entry directly for testing.
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	_, err := fs.extmsgSvc.Transcript.Append(context.Background(), extmsg.AppendTranscriptInput{
		Caller:            caller,
		Conversation:      conv,
		Kind:              extmsg.TranscriptMessageInbound,
		Provenance:        extmsg.TranscriptProvenanceLive,
		ProviderMessageID: "msg-001",
		Actor:             extmsg.ExternalActor{ID: "u1", DisplayName: "Alice"},
		Text:              "hello",
		CreatedAt:         time.Now(),
	})
	if err != nil {
		t.Fatalf("append: %v", err)
	}

	// List transcript.
	req := httptest.NewRequest("GET", "/v0/extmsg/transcript?scope_id=scope&provider=discord&account_id=bot-1&conversation_id=chan-42&kind=room", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("list transcript: expected 200, got %d: %s", w.Code, w.Body.String())
	}

	var listResp struct {
		Items []extmsg.ConversationTranscriptRecord `json:"items"`
		Total int                                   `json:"total"`
	}
	if err := json.NewDecoder(w.Body).Decode(&listResp); err != nil {
		t.Fatal(err)
	}
	if listResp.Total != 1 {
		t.Fatalf("expected 1 entry, got %d", listResp.Total)
	}
}

func TestHandleExtMsgServicesUnavailable(t *testing.T) {
	fs := newFakeState(t)
	// Don't set extmsgSvc or adapterReg — they stay nil.
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/extmsg/bindings?session_id=x", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("expected 503, got %d", w.Code)
	}
}
