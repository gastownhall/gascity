package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// Regression for ga-79peco: a config swap (an API config mutation, or a
// reload) does not advance the event index, so the agent list cached under an
// unchanged index kept serving the previous config's agents until the TTL
// expired: a just-created agent was missing from GET /agents for up to 2s.
func TestHandleAgentListCacheMissesAfterConfigSwap(t *testing.T) {
	state := newFakeState(t)
	h := newTestCityHandler(t, state)
	list := func() string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, cityURL(state, "/agents"), nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("agents = %d, want 200; body = %s", rec.Code, rec.Body.String())
		}
		return rec.Body.String()
	}
	if body := list(); strings.Contains(body, `"myrig/helper"`) {
		t.Fatalf("helper listed before it exists: %s", body)
	}

	next := *state.cfg
	next.Agents = append(append([]config.Agent(nil), state.cfg.Agents...), config.Agent{Name: "helper", Dir: "myrig", Provider: "test-agent", MaxActiveSessions: intPtr(1)})
	state.cfg = &next

	if body := list(); !strings.Contains(body, `"myrig/helper"`) {
		t.Fatalf("agent list served the pre-swap config (no helper) at an unchanged event index: %s", body)
	}
}
