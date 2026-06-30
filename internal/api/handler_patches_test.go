package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// --- Agent patch tests ---

func TestHandleAgentPatchList(t *testing.T) {
	fs := newFakeState(t)
	suspended := true
	fs.cfg.Patches.Agents = []config.AgentPatch{
		{Dir: "rig1", Name: "worker", Suspended: &suspended},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/agents"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	var resp listResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
}

func TestHandleAgentPatchList_Empty(t *testing.T) {
	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/agents"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp listResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 0 {
		t.Errorf("total = %d, want 0", resp.Total)
	}
}

func TestHandleAgentPatchGet(t *testing.T) {
	fs := newFakeState(t)
	suspended := true
	fs.cfg.Patches.Agents = []config.AgentPatch{
		{Dir: "rig1", Name: "worker", Suspended: &suspended},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/agent/rig1/worker"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
}

func TestHandleAgentPatchGet_NotFound(t *testing.T) {
	fs := newFakeState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/agent/rig1/nonexistent"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleAgentPatchSet(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"dir":"rig1","name":"worker","tmux_alias":"worker--{{.CityName}}","suspended":true}`
	req := httptest.NewRequest("PUT", cityURL(fs, "/patches/agents"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if len(fs.cfg.Patches.Agents) != 1 {
		t.Fatalf("patches.agent count = %d, want 1", len(fs.cfg.Patches.Agents))
	}
	if fs.cfg.Patches.Agents[0].Name != "worker" {
		t.Errorf("name = %q, want %q", fs.cfg.Patches.Agents[0].Name, "worker")
	}
	if fs.cfg.Patches.Agents[0].TmuxAlias == nil || *fs.cfg.Patches.Agents[0].TmuxAlias != "worker--{{.CityName}}" {
		t.Errorf("tmux alias = %v, want %q", fs.cfg.Patches.Agents[0].TmuxAlias, "worker--{{.CityName}}")
	}
}

// TestHandleAgentPatchSet_Provider verifies PUT /patches/agents wires the
// provider override through to the stored patch. This is the load-bearing
// "override a pack-imported agent's provider" path: config.AgentPatch supports
// Provider, but the HTTP input previously dropped it, making the PUT a silent
// no-op for provider. The stored patch must carry the provider, and a GET must
// surface it.
func TestHandleAgentPatchSet_Provider(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"dir":"rig1","name":"worker","provider":"claude-max"}`
	req := httptest.NewRequest("PUT", cityURL(fs, "/patches/agents"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if len(fs.cfg.Patches.Agents) != 1 {
		t.Fatalf("patches.agent count = %d, want 1", len(fs.cfg.Patches.Agents))
	}
	got := fs.cfg.Patches.Agents[0].Provider
	if got == nil {
		t.Fatal("patch Provider = nil; PUT dropped the provider override")
	}
	if *got != "claude-max" {
		t.Errorf("patch Provider = %q, want %q", *got, "claude-max")
	}

	// And it must round-trip through GET /patches/agent/{dir}/{base}.
	getReq := httptest.NewRequest("GET", cityURL(fs, "/patches/agent/rig1/worker"), nil)
	getW := httptest.NewRecorder()
	h.ServeHTTP(getW, getReq)
	if getW.Code != http.StatusOK {
		t.Fatalf("GET status = %d, want %d; body = %s", getW.Code, http.StatusOK, getW.Body.String())
	}
	var patch config.AgentPatch
	if err := json.NewDecoder(getW.Body).Decode(&patch); err != nil {
		t.Fatalf("decode patch: %v", err)
	}
	if patch.Provider == nil || *patch.Provider != "claude-max" {
		t.Errorf("GET patch Provider = %v, want %q", patch.Provider, "claude-max")
	}
}

func TestHandleAgentPatchSet_MissingName(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"dir":"rig1","suspended":true}`
	req := httptest.NewRequest("PUT", cityURL(fs, "/patches/agents"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusBadRequest)
	}
}

func TestHandleAgentPatchDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	suspended := true
	fs.cfg.Patches.Agents = []config.AgentPatch{
		{Dir: "rig1", Name: "worker", Suspended: &suspended},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/patches/agent/rig1/worker"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if len(fs.cfg.Patches.Agents) != 0 {
		t.Errorf("patches.agent count = %d, want 0", len(fs.cfg.Patches.Agents))
	}
}

func TestHandleAgentPatchDelete_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/patches/agent/nonexistent"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

// --- Rig patch tests ---

func TestHandleRigPatchList(t *testing.T) {
	fs := newFakeState(t)
	suspended := true
	fs.cfg.Patches.Rigs = []config.RigPatch{
		{Name: "myrig", Suspended: &suspended},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/rigs"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp listResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
}

func TestHandleRigPatchSet(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"name":"myrig","default_branch":"develop","suspended":true}`
	req := httptest.NewRequest("PUT", cityURL(fs, "/patches/rigs"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if len(fs.cfg.Patches.Rigs) != 1 {
		t.Fatalf("patches.rigs count = %d, want 1", len(fs.cfg.Patches.Rigs))
	}
	if fs.cfg.Patches.Rigs[0].DefaultBranch == nil || *fs.cfg.Patches.Rigs[0].DefaultBranch != "develop" {
		t.Fatalf("DefaultBranch = %v, want develop", fs.cfg.Patches.Rigs[0].DefaultBranch)
	}
}

func TestHandleRigPatchDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	suspended := true
	fs.cfg.Patches.Rigs = []config.RigPatch{
		{Name: "myrig", Suspended: &suspended},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/patches/rig/myrig"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if len(fs.cfg.Patches.Rigs) != 0 {
		t.Errorf("patches.rigs count = %d, want 0", len(fs.cfg.Patches.Rigs))
	}
}

// --- Provider patch tests ---

func TestHandleProviderPatchList(t *testing.T) {
	fs := newFakeState(t)
	cmd := "new-cmd"
	fs.cfg.Patches.Providers = []config.ProviderPatch{
		{Name: "claude", Command: &cmd},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("GET", cityURL(fs, "/patches/providers"), nil)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	var resp listResponse
	json.NewDecoder(w.Body).Decode(&resp) //nolint:errcheck
	if resp.Total != 1 {
		t.Errorf("total = %d, want 1", resp.Total)
	}
}

func TestHandleProviderPatchSet(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"name":"claude","command":"my-claude","acp_command":"my-claude-acp","acp_args":["serve","--stdio"],"accept_startup_dialogs":true}`
	req := httptest.NewRequest("PUT", cityURL(fs, "/patches/providers"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	if len(fs.cfg.Patches.Providers) != 1 {
		t.Fatalf("patches.providers count = %d, want 1", len(fs.cfg.Patches.Providers))
	}
	if got := fs.cfg.Patches.Providers[0].ACPCommand; got == nil || *got != "my-claude-acp" {
		t.Fatalf("ACPCommand = %v, want %q", got, "my-claude-acp")
	}
	if got := fs.cfg.Patches.Providers[0].ACPArgs; len(got) != 2 || got[0] != "serve" || got[1] != "--stdio" {
		t.Fatalf("ACPArgs = %#v, want [\"serve\" \"--stdio\"]", got)
	}
	if got := fs.cfg.Patches.Providers[0].AcceptStartupDialogs; got == nil || !*got {
		t.Fatalf("AcceptStartupDialogs = %v, want true", got)
	}
}

func TestHandleProviderPatchDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	cmd := "my-claude"
	fs.cfg.Patches.Providers = []config.ProviderPatch{
		{Name: "claude", Command: &cmd},
	}
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/patches/provider/claude"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusOK)
	}

	if len(fs.cfg.Patches.Providers) != 0 {
		t.Errorf("patches.providers count = %d, want 0", len(fs.cfg.Patches.Providers))
	}
}

func TestHandleProviderPatchDelete_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/patches/provider/nonexistent"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}
