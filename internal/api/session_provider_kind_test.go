package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestProviderSessionReadsBackAsProviderSession pins what a client reads for
// the provider session it created: the same kind its create reported, and no
// pool or agent_kind, since it is not a member of any agent's pool.
func TestProviderSessionReadsBackAsProviderSession(t *testing.T) {
	fs := newSessionFakeState(t)
	// As InjectImplicitAgents does for every configured provider: an implicit
	// agent named after the provider, with no session cap (so it is a pool).
	implicit := config.Agent{Name: "test-agent", Provider: "test-agent", Implicit: true}
	replaced := false
	for i := range fs.cfg.Agents {
		if fs.cfg.Agents[i].Name == "test-agent" && fs.cfg.Agents[i].Dir == "" {
			fs.cfg.Agents[i], replaced = implicit, true
		}
	}
	if !replaced {
		fs.cfg.Agents = append(fs.cfg.Agents, implicit)
	}
	h := newTestCityHandlerWith(t, fs, New(fs))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(fs, "/sessions"), strings.NewReader(`{"kind":"provider","name":"test-agent"}`)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("create status = %d; body: %s", rec.Code, rec.Body.String())
	}
	accepted := decodeAsyncAccepted(t, rec.Body)
	success, failure := waitForSessionCreateResult(t, fs.eventProv, accepted.RequestID)
	if success == nil {
		t.Fatalf("session create failed: %s: %s", failure.ErrorCode, failure.ErrorMessage)
	}
	created := success.Session
	if created.Kind != "provider" || created.AgentKind != "" || created.Pool != "" {
		t.Fatalf("create result kind=%q agent_kind=%q pool=%q, want provider with no agent kind or pool", created.Kind, created.AgentKind, created.Pool)
	}

	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(fs, "/session/")+created.ID, nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET status = %d; body: %s", rec.Code, rec.Body.String())
	}
	var got sessionResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.Kind != "provider" || got.AgentKind != "" || got.Pool != "" {
		t.Fatalf("GET kind=%q agent_kind=%q pool=%q, want provider with no agent kind or pool", got.Kind, got.AgentKind, got.Pool)
	}
}
