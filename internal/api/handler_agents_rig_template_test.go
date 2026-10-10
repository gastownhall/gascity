package api

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/gastownhall/gascity/internal/config"
)

// Regression for ga-l24lrx: GET /agent/{dir}/{base} resolves a rig-qualified
// identity ("myrig/rigbot") through the generic scope="rig" template that
// serves every rig ("rigbot"), but the qualified edit routes looked the
// identity up verbatim and answered 404. The instance has no config of its
// own, so edits address the template (as the CLI does) and deleting the
// shared template through one rig's instance is refused.
func TestQualifiedAgentEditsAddressGenericRigScopedTemplate(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Agents = append(fs.cfg.Agents, config.Agent{Name: "rigbot", Scope: "rig", Provider: "test-agent"})
	h := newTestCityHandler(t, fs)

	do := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		var req *http.Request
		if body != "" {
			req = httptest.NewRequest(method, cityURL(fs, path), strings.NewReader(body))
		} else {
			req = httptest.NewRequest(method, cityURL(fs, path), nil)
		}
		req.Header.Set("X-GC-Request", "true")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}

	if w := do("GET", "/agent/myrig/rigbot", ""); w.Code != http.StatusOK {
		t.Fatalf("GET qualified instance = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if w := do("POST", "/agent/myrig/rigbot/suspend", ""); w.Code != http.StatusOK {
		t.Fatalf("suspend qualified instance = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if !fs.suspended["rigbot"] {
		t.Fatalf("suspend did not address the template; suspended = %v", fs.suspended)
	}
	if w := do("POST", "/agent/myrig/rigbot/resume", ""); w.Code != http.StatusOK {
		t.Fatalf("resume qualified instance = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if fs.suspended["rigbot"] {
		t.Fatal("resume did not address the template")
	}
	if w := do("PATCH", "/agent/myrig/rigbot", `{"suspended":true}`); w.Code != http.StatusOK {
		t.Fatalf("PATCH qualified instance = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	if a, ok := findAgent(fs.cfg, "rigbot"); !ok || !a.Suspended {
		t.Fatalf("PATCH did not update the template: %+v", a)
	}
	if w := do("DELETE", "/agent/myrig/rigbot", ""); w.Code != http.StatusConflict {
		t.Fatalf("DELETE qualified instance = %d, want 409; body = %s", w.Code, w.Body.String())
	}
	if _, ok := findAgent(fs.cfg, "rigbot"); !ok {
		t.Fatal("DELETE through a rig instance removed the shared template")
	}
}

// A [[patches.agent]] (or rig/provider) patch whose target is absent from the
// merged config is rolled back at reload; the client named a target that does
// not exist, so it is a 400, not a 500.
func TestMutationErrorMapsPatchTargetNotFoundToBadRequest(t *testing.T) {
	cfg := &config.City{}
	applyErr := config.ApplyPatches(cfg, config.Patches{Agents: []config.AgentPatch{{Dir: "myrig", Name: "rigbot"}}})
	if applyErr == nil {
		t.Fatal("ApplyPatches accepted a patch for a missing agent")
	}
	wrapped := fmt.Errorf("refreshing updated city config: %w", fmt.Errorf("applying patches: %w", applyErr))
	var se huma.StatusError
	if !errors.As(mutationError(wrapped), &se) || se.GetStatus() != http.StatusBadRequest {
		t.Fatalf("mutationError(%v) = %v, want 400", wrapped, mutationError(wrapped))
	}
}
