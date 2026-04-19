package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestHandleCityGet(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Workspace.Provider = "claude"
	fs.cfg.Workspace.SessionTemplate = "{{.City}}--{{.Agent}}"
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, ""), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp cityGetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if resp.Name != "test-city" {
		t.Errorf("name = %q, want %q", resp.Name, "test-city")
	}
	if resp.Suspended {
		t.Error("expected suspended=false")
	}
	if resp.Provider != "claude" {
		t.Errorf("provider = %q, want %q", resp.Provider, "claude")
	}
	if resp.SessionTemplate != "{{.City}}--{{.Agent}}" {
		t.Errorf("session_template = %q, want %q", resp.SessionTemplate, "{{.City}}--{{.Agent}}")
	}
	if resp.AgentCount != 1 {
		t.Errorf("agent_count = %d, want 1", resp.AgentCount)
	}
	if resp.RigCount != 1 {
		t.Errorf("rig_count = %d, want 1", resp.RigCount)
	}
}

func TestHandleCityGet_Suspended(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Workspace.Suspended = true
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, ""), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp cityGetResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !resp.Suspended {
		t.Error("expected suspended=true")
	}
}
