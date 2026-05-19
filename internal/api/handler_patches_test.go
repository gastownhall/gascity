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

	body := `{"dir":"rig1","name":"worker","suspended":true}`
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

	body := `{"name":"myrig","suspended":true}`
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

	body := `{"name":"claude","command":"my-claude","acp_command":"my-claude-acp","acp_args":["serve","--stdio"]}`
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
