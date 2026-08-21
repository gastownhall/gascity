package api

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/extmsg"
)

// newExtMsgSubscribeFixture creates a test fixture with extmsg services,
// adapter registry, and a registered client token.
// Returns (fs, srv, clientID, rawToken, convRef).
func newExtMsgSubscribeFixture(t *testing.T) (*fakeState, *Server, string, string, extmsg.ConversationRef) {
	t.Helper()
	fs := newSessionFakeState(t)
	srv := New(fs)
	services := extmsg.NewServices(fs.cityBeadStore)
	fs.extmsgSvc = &services
	fs.adapterReg = extmsg.NewAdapterRegistry()

	result, err := extmsg.RegisterClient(context.Background(), fs.cityBeadStore, extmsg.RegisterClientInput{
		AllowNoCredential: true,
	})
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	convID := "test-conv-1"
	convRef := extmsg.ConversationRef{
		ScopeID:        result.ClientID,
		Provider:       extmsg.ProviderLLMClient,
		AccountID:      result.ClientID,
		ConversationID: convID,
		Kind:           extmsg.ConversationDM,
	}
	return fs, srv, result.ClientID, result.Token, convRef
}

func subscribeURL(fs *fakeState, clientID, convID string) string {
	return cityURL(fs, "/extmsg/clients/"+clientID+"/conversations/"+convID+"/subscribe")
}

func TestSubscribeHandler_MissingTokenReturns401(t *testing.T) {
	fs, srv, clientID, _, _ := newExtMsgSubscribeFixture(t)
	h := newTestCityHandlerWith(t, fs, srv)

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSubscribeHandler_InvalidTokenReturns401(t *testing.T) {
	fs, srv, clientID, _, _ := newExtMsgSubscribeFixture(t)
	h := newTestCityHandlerWith(t, fs, srv)

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil)
	req.Header.Set("X-GC-Client-Token", "not-a-real-token")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSubscribeHandler_AccountMismatchReturns403(t *testing.T) {
	fs, srv, _, token, _ := newExtMsgSubscribeFixture(t)
	h := newTestCityHandlerWith(t, fs, srv)

	req := httptest.NewRequest("GET", subscribeURL(fs, "wrong-client-id", "test-conv-1"), nil)
	req.Header.Set("X-GC-Client-Token", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSubscribeHandler_ServicesUnavailableReturns503(t *testing.T) {
	fs, srv, clientID, token, _ := newExtMsgSubscribeFixture(t)
	fs.extmsgSvc = nil
	h := newTestCityHandlerWith(t, fs, srv)

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil)
	req.Header.Set("X-GC-Client-Token", token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503; body: %s", rec.Code, rec.Body.String())
	}
}

func TestSubscribeHandler_NoBindingStreamsHeartbeats(t *testing.T) {
	fs, srv, clientID, token, _ := newExtMsgSubscribeFixture(t)
	fs.cfg.ExtMsg.ConnectedClients.HeartbeatInterval = "100ms"
	h := newTestCityHandlerWith(t, fs, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil).WithContext(ctx)
	req.Header.Set("X-GC-Client-Token", token)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	<-done

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body: %s", rec.Code, rec.Body.String())
	}
	if ct := rec.Header().Get("Content-Type"); ct != "text/event-stream" {
		t.Errorf("Content-Type = %q, want \"text/event-stream\"", ct)
	}
	if !strings.Contains(rec.Body.String(), "event: heartbeat") {
		t.Errorf("stream missing heartbeat event; body: %s", rec.Body.String())
	}
}

func TestSubscribeHandler_ValidTokenStreamsMessages(t *testing.T) {
	fs, srv, clientID, token, convRef := newExtMsgSubscribeFixture(t)
	fs.cfg.ExtMsg.ConnectedClients.HeartbeatInterval = "10s"
	h := newTestCityHandlerWith(t, fs, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil).WithContext(ctx)
	req.Header.Set("X-GC-Client-Token", token)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Wait for the LLMClientAdapter to register in the shared AdapterRegistry.
	adapterKey := extmsg.AdapterKey{Provider: extmsg.ProviderLLMClient, AccountID: clientID}
	deadline := time.Now().Add(time.Second)
	var adapter extmsg.TransportAdapter
	for time.Now().Before(deadline) {
		adapter = fs.adapterReg.Lookup(adapterKey)
		if adapter != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	if adapter == nil {
		t.Fatal("LLMClientAdapter not registered after 1s")
	}

	_, err := adapter.Publish(context.Background(), extmsg.PublishRequest{
		SessionID:    "test-session",
		Conversation: convRef,
		Text:         "hello from session",
	})
	if err != nil {
		t.Fatalf("adapter.Publish: %v", err)
	}

	// Give the stream loop time to pick up the event, then cancel.
	time.Sleep(50 * time.Millisecond)
	cancel()
	<-done

	body := rec.Body.String()
	// The SSE framework omits "event: message" for the default event type;
	// check the JSON payload field instead.
	if !strings.Contains(body, `"event":"message"`) {
		t.Errorf("stream missing message event payload; body: %s", body)
	}
	if !strings.Contains(body, "hello from session") {
		t.Errorf("stream missing message text; body: %s", body)
	}
}

func TestSubscribeHandler_ForbiddenSessionReturns403(t *testing.T) {
	fs, srv, _, _, _ := newExtMsgSubscribeFixture(t)

	// Register a distinct client with AllowedSessions=["session-A"]. A
	// non-empty credential prevents idempotent dedup with the anonymous
	// client already registered by the fixture.
	result, err := extmsg.RegisterClient(context.Background(), fs.cityBeadStore, extmsg.RegisterClientInput{
		Credential:      "test-cred-forbidden",
		AllowedSessions: []string{"session-A"},
	})
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	convRef := extmsg.ConversationRef{
		ScopeID:        result.ClientID,
		Provider:       extmsg.ProviderLLMClient,
		AccountID:      result.ClientID,
		ConversationID: "test-conv-forbidden",
		Kind:           extmsg.ConversationDM,
	}

	// Bind the conversation to "session-B", which is NOT in AllowedSessions.
	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	if _, err := fs.extmsgSvc.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: convRef,
		SessionID:    "session-B",
		Now:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}

	h := newTestCityHandlerWith(t, fs, srv)
	req := httptest.NewRequest("GET", subscribeURL(fs, result.ClientID, "test-conv-forbidden"), nil)
	req.Header.Set("X-GC-Client-Token", result.Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403; body: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "session_forbidden") {
		t.Errorf("body missing session_forbidden; body: %s", rec.Body.String())
	}
}

// onceForbiddenBindings wraps a BindingService so that the first
// ResolveByConversation call returns nil (no binding — simulating the precheck
// window) and all subsequent calls return a pre-configured forbidden binding
// (simulating a race-created binding that appears after the precheck).
type onceForbiddenBindings struct {
	extmsg.BindingService
	mu        sync.Mutex
	firstDone bool
	forbidden *extmsg.SessionBindingRecord
}

func (b *onceForbiddenBindings) ResolveByConversation(_ context.Context, _ extmsg.ConversationRef) (*extmsg.SessionBindingRecord, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if !b.firstDone {
		b.firstDone = true
		return nil, nil
	}
	return b.forbidden, nil
}

// TestSubscribeHandler_ForbiddenSessionAtStreamTimeDisconnects validates the
// second enforcement line: when no binding exists at precheck but a binding to
// a forbidden session appears at stream-time (TOCTOU window), the stream
// disconnects silently without delivering any SSE events.
func TestSubscribeHandler_ForbiddenSessionAtStreamTimeDisconnects(t *testing.T) {
	fs, srv, _, _, _ := newExtMsgSubscribeFixture(t)

	result, err := extmsg.RegisterClient(context.Background(), fs.cityBeadStore, extmsg.RegisterClientInput{
		Credential:      "test-cred-toctou",
		AllowedSessions: []string{"session-A"},
	})
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	// Inject proxy: precheck sees no binding (first call → nil); stream sees
	// a binding to "session-B" which is not in AllowedSessions.
	fs.extmsgSvc.Bindings = &onceForbiddenBindings{
		BindingService: fs.extmsgSvc.Bindings,
		forbidden:      &extmsg.SessionBindingRecord{SessionID: "session-B"},
	}

	h := newTestCityHandlerWith(t, fs, srv)
	req := httptest.NewRequest("GET", subscribeURL(fs, result.ClientID, "test-conv-toctou"), nil)
	req.Header.Set("X-GC-Client-Token", result.Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Errorf("status = %d, want 200 (stream opened then closed); body: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()
	if strings.Contains(body, "event:") || strings.Contains(body, "data:") {
		t.Errorf("stream delivered SSE events but should have disconnected silently; body: %s", body)
	}
}

func TestSubscribeHandler_AdapterUnregisteredOnDisconnect(t *testing.T) {
	fs, srv, clientID, token, _ := newExtMsgSubscribeFixture(t)
	fs.cfg.ExtMsg.ConnectedClients.HeartbeatInterval = "10s"
	h := newTestCityHandlerWith(t, fs, srv)

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, "test-conv-1"), nil).WithContext(ctx)
	req.Header.Set("X-GC-Client-Token", token)
	rec := httptest.NewRecorder()

	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()

	// Wait for adapter to appear.
	adapterKey := extmsg.AdapterKey{Provider: extmsg.ProviderLLMClient, AccountID: clientID}
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if fs.adapterReg.Lookup(adapterKey) != nil {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Cancel the request (simulate client disconnect).
	cancel()
	<-done

	// Adapter must be unregistered after disconnect.
	if fs.adapterReg.Lookup(adapterKey) != nil {
		t.Error("LLMClientAdapter still registered after disconnect")
	}
}

// TestSubscribeHandler_LastEventIDReplaysOnlyMissedMessages validates that a
// reconnect with Last-Event-ID backfills only transcript entries after the
// cursor — the behavior that was broken while live message events hardcoded
// sequence 0 (every reconnect sent Last-Event-ID: 0 and replayed everything).
func TestSubscribeHandler_LastEventIDReplaysOnlyMissedMessages(t *testing.T) {
	fs, srv, clientID, token, convRef := newExtMsgSubscribeFixture(t)
	fs.cfg.ExtMsg.ConnectedClients.HeartbeatInterval = "10s"

	caller := extmsg.Caller{Kind: extmsg.CallerController, ID: "test"}
	// A binding makes the stream run EnsureMembership + backfill replay.
	if _, err := fs.extmsgSvc.Bindings.Bind(context.Background(), caller, extmsg.BindInput{
		Conversation: convRef,
		SessionID:    "sess-1",
		Now:          time.Now().UTC(),
	}); err != nil {
		t.Fatalf("Bind: %v", err)
	}
	// Three outbound entries → sequences 1, 2, 3.
	for _, txt := range []string{"m1", "m2", "m3"} {
		if _, err := fs.extmsgSvc.Transcript.Append(context.Background(), extmsg.AppendTranscriptInput{
			Caller:       caller,
			Conversation: convRef,
			Kind:         extmsg.TranscriptMessageOutbound,
			Provenance:   extmsg.TranscriptProvenanceLive,
			Text:         txt,
			CreatedAt:    time.Now().UTC(),
		}); err != nil {
			t.Fatalf("Append %s: %v", txt, err)
		}
	}

	h := newTestCityHandlerWith(t, fs, srv)
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	req := httptest.NewRequest("GET", subscribeURL(fs, clientID, convRef.ConversationID), nil).WithContext(ctx)
	req.Header.Set("X-GC-Client-Token", token)
	req.Header.Set("Last-Event-ID", "2") // client last saw sequence 2

	rec := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		h.ServeHTTP(rec, req)
		close(done)
	}()
	<-done

	body := rec.Body.String()
	if !strings.Contains(body, "m3") {
		t.Errorf("backfill missing m3 (the only message after the cursor); body: %s", body)
	}
	if strings.Contains(body, "m1") || strings.Contains(body, "m2") {
		t.Errorf("backfill replayed already-seen messages (m1/m2); body: %s", body)
	}
}

// erroringBindings forces ResolveByConversation to fail, simulating a transient
// binding-store fault.
type erroringBindings struct {
	extmsg.BindingService
}

func (erroringBindings) ResolveByConversation(_ context.Context, _ extmsg.ConversationRef) (*extmsg.SessionBindingRecord, error) {
	return nil, errors.New("simulated binding store fault")
}

// TestSubscribeHandler_BindingStoreErrorFailsClosed validates that a binding
// lookup error during the precheck fails closed (503) instead of bypassing the
// token's allowed_sessions list.
func TestSubscribeHandler_BindingStoreErrorFailsClosed(t *testing.T) {
	fs, srv, _, _, _ := newExtMsgSubscribeFixture(t)

	// A client WITH an allow-list, so the precheck consults the binding store.
	result, err := extmsg.RegisterClient(context.Background(), fs.cityBeadStore, extmsg.RegisterClientInput{
		Credential:      "cred-failclosed",
		AllowedSessions: []string{"session-A"},
	})
	if err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}
	fs.extmsgSvc.Bindings = erroringBindings{BindingService: fs.extmsgSvc.Bindings}

	h := newTestCityHandlerWith(t, fs, srv)
	req := httptest.NewRequest("GET", subscribeURL(fs, result.ClientID, "conv-failclosed"), nil)
	req.Header.Set("X-GC-Client-Token", result.Token)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503 (fail closed on binding store error); body: %s", rec.Code, rec.Body.String())
	}
}
