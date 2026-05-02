package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func TestHandleAgentCreate(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"name":"coder","provider":"claude"}`
	req := newPostRequest(cityURL(fs, "/agents"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}

	// Verify agent was added.
	found := false
	for _, a := range fs.cfg.Agents {
		if a.Name == "coder" && a.Provider == "claude" {
			found = true
		}
	}
	if !found {
		t.Error("agent 'coder' not found in config after create")
	}
}

// agentVisibilityFakeState wraps fakeMutatorState with an
// AgentVisibilityWaiter implementation so the handler-side wiring can be
// exercised without spinning up the real controller.
type agentVisibilityFakeState struct {
	*fakeMutatorState
	waitCalled atomic.Bool
	waitName   atomic.Value // string
	waitErr    error
}

func (s *agentVisibilityFakeState) WaitForAgentVisibility(_ context.Context, qualifiedName string) error {
	s.waitCalled.Store(true)
	s.waitName.Store(qualifiedName)
	return s.waitErr
}

// TestHandleAgentCreate_InvokesVisibilityWaiter verifies that POST /agents
// calls WaitForAgentVisibility with the qualified name on success. This is
// the read-after-write guarantee that prevents a follow-up POST /sling from
// 404ing on the freshly created target.
func TestHandleAgentCreate_InvokesVisibilityWaiter(t *testing.T) {
	fs := &agentVisibilityFakeState{fakeMutatorState: newFakeMutatorState(t)}
	h := newTestCityHandler(t, fs)

	body := `{"name":"coder","dir":"myrig","provider":"claude"}`
	req := newPostRequest(cityURL(fs, "/agents"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusCreated {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusCreated, w.Body.String())
	}
	if !fs.waitCalled.Load() {
		t.Fatal("WaitForAgentVisibility was not called")
	}
	if got, _ := fs.waitName.Load().(string); got != "myrig/coder" {
		t.Errorf("WaitForAgentVisibility called with %q, want %q", got, "myrig/coder")
	}
}

// TestHandleAgentCreate_VisibilityWaiterErrorSurfacesAs500 ensures that a
// wait failure (e.g., context deadline) does not silently 201 — the caller
// must know the agent isn't yet reachable through findAgent.
func TestHandleAgentCreate_VisibilityWaiterErrorSurfacesAs500(t *testing.T) {
	fs := &agentVisibilityFakeState{
		fakeMutatorState: newFakeMutatorState(t),
		waitErr:          errors.New("simulated visibility wait failure"),
	}
	h := newTestCityHandler(t, fs)

	body := `{"name":"coder","dir":"myrig","provider":"claude"}`
	req := newPostRequest(cityURL(fs, "/agents"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusInternalServerError, w.Body.String())
	}
}

func TestHandleAgentCreate_MissingName(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"provider":"claude"}`
	req := newPostRequest(cityURL(fs, "/agents"), strings.NewReader(body))
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusUnprocessableEntity)
	}
}

func TestHandleAgentUpdate(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"provider":"gemini"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/agent/myrig/worker"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify provider was updated.
	for _, a := range fs.cfg.Agents {
		if a.Name == "worker" && a.Dir == "myrig" {
			if a.Provider != "gemini" {
				t.Errorf("provider = %q, want %q", a.Provider, "gemini")
			}
			return
		}
	}
	t.Error("agent 'myrig/worker' not found after update")
}

func TestHandleAgentUpdate_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"provider":"gemini"}`
	req := httptest.NewRequest("PATCH", cityURL(fs, "/agent/nonexistent"), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleAgentDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/agent/myrig/worker"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}

	// Verify agent was removed.
	for _, a := range fs.cfg.Agents {
		if a.Name == "worker" && a.Dir == "myrig" {
			t.Error("agent 'myrig/worker' still exists after delete")
		}
	}
}

func TestHandleAgentDelete_NotFound(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/agent/nonexistent"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNotFound)
	}
}

func TestHandleCityPatch_Suspend(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	body := `{"suspended": true}`
	req := httptest.NewRequest("PATCH", cityURL(fs, ""), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if !fs.cfg.Workspace.Suspended {
		t.Error("expected workspace to be suspended")
	}
}

func TestHandleCityPatch_Resume(t *testing.T) {
	fs := newFakeMutatorState(t)
	fs.cfg.Workspace.Suspended = true
	h := newTestCityHandler(t, fs)

	body := `{"suspended": false}`
	req := httptest.NewRequest("PATCH", cityURL(fs, ""), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d; body = %s", w.Code, http.StatusOK, w.Body.String())
	}
	if fs.cfg.Workspace.Suspended {
		t.Error("expected workspace to not be suspended")
	}
}

func TestCSRF_BlocksDeleteWithoutHeader(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandler(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/agent/myrig/worker"), nil)
	// No X-GC-Request header.
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
	// Phase 3 Fix 3d: humaCSRFMiddleware emits RFC 9457 Problem Details.
	// The detail field carries a "csrf:" prefix for semantic matching.
	var problem struct {
		Status int    `json:"status"`
		Title  string `json:"title"`
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(w.Body).Decode(&problem); err != nil {
		t.Fatalf("decode error: %v", err)
	}
	if problem.Status != http.StatusForbidden {
		t.Errorf("problem.status = %d, want %d", problem.Status, http.StatusForbidden)
	}
	if !strings.Contains(problem.Detail, "csrf") {
		t.Errorf("problem.detail = %q, want it to contain %q", problem.Detail, "csrf")
	}
}

func TestReadOnly_BlocksPatch(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandlerReadOnly(t, fs)

	body := `{"suspended": true}`
	req := httptest.NewRequest("PATCH", cityURL(fs, ""), strings.NewReader(body))
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}

func TestReadOnly_BlocksDelete(t *testing.T) {
	fs := newFakeMutatorState(t)
	h := newTestCityHandlerReadOnly(t, fs)

	req := httptest.NewRequest("DELETE", cityURL(fs, "/agent/myrig/worker"), nil)
	req.Header.Set("X-GC-Request", "true")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, req)

	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusForbidden)
	}
}
