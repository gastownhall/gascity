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
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
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
	waitCalled             atomic.Bool
	waitName               atomic.Value // string
	waitErr                error
	waitUntilContextDone   bool
	publishAgentDuringWait bool
	pendingAgent           *config.Agent
}

func (s *agentVisibilityFakeState) CreateAgent(a config.Agent) error {
	if !s.publishAgentDuringWait {
		return s.fakeMutatorState.CreateAgent(a)
	}
	pending := a
	s.pendingAgent = &pending
	return nil
}

func (s *agentVisibilityFakeState) WaitForAgentVisibility(ctx context.Context, qualifiedName string) error {
	s.waitCalled.Store(true)
	s.waitName.Store(qualifiedName)
	if s.waitUntilContextDone {
		<-ctx.Done()
		return ctx.Err()
	}
	if s.waitErr != nil {
		return s.waitErr
	}
	if s.publishAgentDuringWait && s.pendingAgent != nil {
		s.cfg.Agents = append(s.cfg.Agents, *s.pendingAgent)
		s.pendingAgent = nil
	}
	return nil
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

// TestHandleAgentCreate_MakesImmediateSlingTargetVisible proves the handler
// sequence that regressed in the live contract: once POST /agents returns 201,
// a POST /sling against the same freshly-created target resolves through the
// handler's current Config snapshot.
func TestHandleAgentCreate_MakesImmediateSlingTargetVisible(t *testing.T) {
	fs := &agentVisibilityFakeState{
		fakeMutatorState:       newFakeMutatorState(t),
		publishAgentDuringWait: true,
	}
	fs.cfg.Rigs[0].Prefix = "gc"
	srv := New(fs)
	srv.SlingRunnerFunc = func(_ string, _ string, _ map[string]string) (string, error) {
		return "", nil
	}
	h := newTestCityHandlerWith(t, fs, srv)

	b, err := fs.stores["myrig"].Create(beads.Bead{Title: "route me", Type: "task"})
	if err != nil {
		t.Fatalf("create bead: %v", err)
	}

	createReq := newPostRequest(cityURL(fs, "/agents"), strings.NewReader(
		`{"name":"coder","dir":"myrig","provider":"test-agent"}`,
	))
	createRec := httptest.NewRecorder()
	h.ServeHTTP(createRec, createReq)
	if createRec.Code != http.StatusCreated {
		t.Fatalf("create status = %d, want %d; body = %s", createRec.Code, http.StatusCreated, createRec.Body.String())
	}

	slingBody := `{"target":"myrig/coder","bead":"` + b.ID + `"}`
	slingRec := httptest.NewRecorder()
	h.ServeHTTP(slingRec, newPostRequest(cityURL(fs, "/sling"), strings.NewReader(slingBody)))
	if slingRec.Code != http.StatusOK {
		t.Fatalf("sling status = %d, want %d; body = %s", slingRec.Code, http.StatusOK, slingRec.Body.String())
	}
}

func TestHandleAgentCreate_VisibilityWaiterTimeoutIsBounded(t *testing.T) {
	fs := &agentVisibilityFakeState{
		fakeMutatorState:     newFakeMutatorState(t),
		waitUntilContextDone: true,
	}
	srv := New(fs)
	srv.agentVisibilityWaitTimeout = 10 * time.Millisecond
	h := newTestCityHandlerWith(t, fs, srv)

	req := newPostRequest(cityURL(fs, "/agents"), strings.NewReader(
		`{"name":"coder","dir":"myrig","provider":"test-agent"}`,
	))
	rec := httptest.NewRecorder()
	start := time.Now()
	h.ServeHTTP(rec, req)

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("handler returned after %s, want bounded visibility timeout", elapsed)
	}
	if rec.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusGatewayTimeout, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if strings.Contains(rec.Body.String(), context.DeadlineExceeded.Error()) {
		t.Fatalf("response leaked raw context error: %s", rec.Body.String())
	}
}

func TestHandleAgentCreate_VisibilityWaiterCancelIsServiceUnavailable(t *testing.T) {
	fs := &agentVisibilityFakeState{
		fakeMutatorState: newFakeMutatorState(t),
		waitErr:          context.Canceled,
	}
	h := newTestCityHandler(t, fs)

	req := newPostRequest(cityURL(fs, "/agents"), strings.NewReader(
		`{"name":"coder","dir":"myrig","provider":"test-agent"}`,
	))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d; body = %s", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
	if got := rec.Header().Get("Retry-After"); got != "1" {
		t.Fatalf("Retry-After = %q, want 1", got)
	}
	if strings.Contains(rec.Body.String(), context.Canceled.Error()) {
		t.Fatalf("response leaked raw context error: %s", rec.Body.String())
	}
}

// TestHandleAgentCreate_VisibilityWaiterErrorSurfacesAs500 ensures that a
// projection failure does not silently 201 — the caller must know the agent
// isn't yet reachable through findAgent.
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
	if strings.Contains(w.Body.String(), "simulated visibility wait failure") {
		t.Fatalf("response leaked raw waiter error: %s", w.Body.String())
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
