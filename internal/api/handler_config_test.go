package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestHandleConfigGet(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Workspace.Provider = "claude"
	fs.cfg.Agents[0].MinActiveSessions = intPtr(0)
	fs.cfg.Agents[0].MaxActiveSessions = intPtr(3)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"custom": {DisplayName: "Custom", Command: "custom-cli"},
	}
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/config", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp configResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck

	if resp.Workspace.Name != "test-city" {
		t.Errorf("workspace.name = %q, want %q", resp.Workspace.Name, "test-city")
	}
	if resp.Workspace.Provider != "claude" {
		t.Errorf("workspace.provider = %q, want %q", resp.Workspace.Provider, "claude")
	}
	if len(resp.Agents) != 1 {
		t.Errorf("agents count = %d, want 1", len(resp.Agents))
	}
	if !resp.Agents[0].IsPool {
		t.Error("expected config agent to expose is_pool=true")
	}
	if len(resp.Rigs) != 1 {
		t.Errorf("rigs count = %d, want 1", len(resp.Rigs))
	}
	if _, ok := resp.Providers["custom"]; !ok {
		t.Error("expected 'custom' in providers")
	}
}

func TestHandleConfigGet_NoPatches(t *testing.T) {
	fs := newFakeState(t)
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/config", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	// Patches should be omitted when empty.
	var raw map[string]any
	json.NewDecoder(w.Body).Decode(&raw) //nolint:errcheck
	if _, ok := raw["patches"]; ok {
		t.Error("expected patches to be omitted when empty")
	}
}

func TestHandleConfigGet_WithPatches(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Patches.Agents = []config.AgentPatch{
		{Dir: "rig1", Name: "worker"},
	}
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/config", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp configResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Patches == nil {
		t.Fatal("expected patches to be present")
	}
	if resp.Patches.AgentCount != 1 {
		t.Errorf("patches.agent_count = %d, want 1", resp.Patches.AgentCount)
	}
}

func TestHandleConfigExplain(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Agents[0].MinActiveSessions = intPtr(0)
	fs.cfg.Agents[0].MaxActiveSessions = intPtr(3)
	fs.cfg.Providers = map[string]config.ProviderSpec{
		"claude": {DisplayName: "My Claude", Command: "my-claude"},
	}
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/config/explain", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck

	// Check agents have origin annotations.
	agents, ok := resp["agents"].([]any)
	if !ok {
		t.Fatal("expected agents array")
	}
	if len(agents) == 0 {
		t.Fatal("expected at least one agent")
	}
	agent0 := agents[0].(map[string]any)
	if agent0["origin"] != "inline" {
		t.Errorf("agent origin = %q, want %q", agent0["origin"], "inline")
	}
	if agent0["is_pool"] != true {
		t.Errorf("agent is_pool = %#v, want true", agent0["is_pool"])
	}

	// Check providers have origin annotations.
	providers, ok := resp["providers"].(map[string]any)
	if !ok {
		t.Fatal("expected providers map")
	}
	claude := providers["claude"].(map[string]any)
	if claude["origin"] != "builtin+city" {
		t.Errorf("claude origin = %q, want %q", claude["origin"], "builtin+city")
	}
	// A builtin-only provider should have origin "builtin".
	codex := providers["codex"].(map[string]any)
	if codex["origin"] != "builtin" {
		t.Errorf("codex origin = %q, want %q", codex["origin"], "builtin")
	}
}

func TestHandleConfigValidate_Valid(t *testing.T) {
	fs := newFakeState(t)
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/config/validate", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp["valid"] != true {
		t.Error("expected valid=true for well-formed config")
	}
}

func TestHandleConfigValidate_WithWarnings(t *testing.T) {
	fs := newFakeState(t)
	// Agent references a nonexistent provider — should produce a warning.
	fs.cfg.Agents[0].Provider = "nonexistent-provider"
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/config/validate", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck

	// Config is still valid (warnings are non-fatal).
	if resp["valid"] != true {
		t.Error("expected valid=true (warnings are non-fatal)")
	}

	warnings, ok := resp["warnings"].([]any)
	if !ok || len(warnings) == 0 {
		t.Error("expected at least one warning for unknown provider")
	}
}

func TestHandleConfigValidate_InvalidServiceRuntimeSupport(t *testing.T) {
	fs := newFakeState(t)
	fs.cfg.Services = []config.Service{{
		Name:     "review-intake",
		Workflow: config.ServiceWorkflowConfig{Contract: "missing.contract"},
	}}
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/config/validate", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp struct {
		Valid  bool     `json:"valid"`
		Errors []string `json:"errors"`
	}
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode validate response: %v", err)
	}
	if resp.Valid {
		t.Fatal("expected valid=false for unsupported service runtime")
	}
	if len(resp.Errors) == 0 || !strings.Contains(resp.Errors[0], `unsupported workflow contract`) {
		t.Fatalf("errors = %#v, want unsupported workflow contract", resp.Errors)
	}
}

func TestHandleConfigExplain_PackDerivedAgent(t *testing.T) {
	fs := newFakeState(t)
	// Simulate pack-derived agent: present in expanded config (cfg) but
	// absent from raw config. The explain handler uses RawConfigProvider
	// for accurate provenance detection.
	fs.rawCfg = &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		// No agents in raw — worker comes from pack expansion.
		Rigs: []config.Rig{
			{Name: "myrig", Path: "/tmp/myrig"},
		},
	}
	srv := New(fs)

	req := httptest.NewRequest("GET", "/v0/config/explain", nil)
	w := httptest.NewRecorder()
	srv.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp map[string]any
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	agents := resp["agents"].([]any)
	agent0 := agents[0].(map[string]any)
	if agent0["origin"] != "pack-derived" {
		t.Errorf("agent origin = %q, want %q", agent0["origin"], "pack-derived")
	}
}
