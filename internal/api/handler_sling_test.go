package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// newSlingTestServer creates a test server with a fake runner that captures
// commands without executing real shell processes.
func newSlingTestServer(t *testing.T) (*Server, *fakeMutatorState) {
	t.Helper()
	state := newFakeMutatorState(t)
	state.cfg.Rigs[0].Prefix = "gc" // match MemStore's auto-generated prefix
	srv := New(state)
	srv.SlingRunnerFunc = func(_ string, _ string, _ map[string]string) (string, error) {
		return "", nil // no-op runner
	}
	return srv, state
}

func TestSlingWithBead(t *testing.T) {
	srv, state := newSlingTestServer(t)
	store := state.stores["myrig"]
	b, err := store.Create(beads.Bead{Title: "test task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"target":"myrig/worker","bead":"` + b.ID + `"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "slung" {
		t.Fatalf("status = %q, want %q", resp["status"], "slung")
	}
	if resp["mode"] != "direct" {
		t.Fatalf("mode = %q, want %q", resp["mode"], "direct")
	}
}

func TestSlingMissingTarget(t *testing.T) {
	srv, _ := newSlingTestServer(t)
	body := `{"bead":"abc"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSlingTargetNotFound(t *testing.T) {
	srv, _ := newSlingTestServer(t)
	body := `{"target":"nonexistent","bead":"abc"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSlingMissingBeadAndFormula(t *testing.T) {
	srv, _ := newSlingTestServer(t)
	body := `{"target":"myrig/worker"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSlingBeadAndFormulaMutuallyExclusive(t *testing.T) {
	srv, _ := newSlingTestServer(t)
	body := `{"target":"myrig/worker","bead":"abc","formula":"xyz"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSlingRejectsVarsWithoutFormula(t *testing.T) {
	srv, _ := newSlingTestServer(t)
	body := `{"target":"myrig/worker","bead":"BD-42","vars":{"issue":"BD-42"}}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSlingRejectsScopeWithoutFormula(t *testing.T) {
	srv, _ := newSlingTestServer(t)
	body := `{"target":"myrig/worker","bead":"BD-42","scope_kind":"city","scope_ref":"test-city"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSlingRejectsPartialScope(t *testing.T) {
	srv, _ := newSlingTestServer(t)
	body := `{"target":"myrig/worker","formula":"mol-review","scope_kind":"city"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSlingPoolTarget(t *testing.T) {
	srv, state := newSlingTestServer(t)
	state.cfg.Agents = []config.Agent{
		{
			Name:              "polecat",
			Dir:               "myrig",
			MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(3),
		},
	}
	store := state.stores["myrig"]
	b, err := store.Create(beads.Bead{Title: "test task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"target":"myrig/polecat","bead":"` + b.ID + `"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}

	var resp map[string]string
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp["status"] != "slung" {
		t.Fatalf("status = %q, want slung", resp["status"])
	}
}

// TestQualifySlingTarget covers the scope-aware target qualification logic:
// when a UI dispatch arrives with scope_kind=rig and a bare-name target,
// the helper rewrites the target to "<scope_ref>/<name>" — but only when
// the rig-scoped agent actually exists, so city-scoped fallbacks keep
// working.
func TestQualifySlingTarget(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", MaxActiveSessions: intPtr(1)},
			{Name: "mayor", MaxActiveSessions: intPtr(1)}, // city-scoped
		},
	}

	cases := []struct {
		name      string
		target    string
		scopeKind string
		scopeRef  string
		want      string
	}{
		{"rig_bare_qualifies", "worker", "rig", "myrig", "myrig/worker"},
		{"rig_bare_no_match_unchanged", "worker", "rig", "otherrig", "worker"},
		{"rig_qualified_unchanged", "myrig/worker", "rig", "myrig", "myrig/worker"},
		{"city_bare_unchanged", "worker", "city", "test-city", "worker"},
		{"no_scope_unchanged", "worker", "", "", "worker"},
		{"rig_but_no_ref_unchanged", "worker", "rig", "", "worker"},
		{"rig_city_scoped_fallthrough", "mayor", "rig", "myrig", "mayor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := qualifySlingTarget(cfg, tc.target, tc.scopeKind, tc.scopeRef)
			if got != tc.want {
				t.Errorf("qualifySlingTarget(%q, %q, %q) = %q, want %q", tc.target, tc.scopeKind, tc.scopeRef, got, tc.want)
			}
		})
	}
}

// TestApiAgentResolverHonorsRigContext verifies that the API-side agent
// resolver does the same rig-contextual bare-name match the CLI does —
// required so formula child steps with bare assignees resolve to the
// correct rig when the top-level target is rig-qualified.
func TestApiAgentResolverHonorsRigContext(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", MaxActiveSessions: intPtr(1)},
			{Name: "worker", Dir: "otherrig", MaxActiveSessions: intPtr(1)},
		},
	}
	resolver := apiAgentResolver{}

	// Bare name + rig context prefers the rig-scoped agent.
	a, ok := resolver.ResolveAgent(cfg, "worker", "myrig")
	if !ok {
		t.Fatal("expected to resolve worker with rig context")
	}
	if a.QualifiedName() != "myrig/worker" {
		t.Errorf("got %q, want myrig/worker", a.QualifiedName())
	}

	// Bare name + different rig context resolves to that rig.
	a, ok = resolver.ResolveAgent(cfg, "worker", "otherrig")
	if !ok {
		t.Fatal("expected to resolve worker with otherrig context")
	}
	if a.QualifiedName() != "otherrig/worker" {
		t.Errorf("got %q, want otherrig/worker", a.QualifiedName())
	}

	// Qualified name is never re-qualified.
	a, ok = resolver.ResolveAgent(cfg, "myrig/worker", "otherrig")
	if !ok {
		t.Fatal("expected to resolve qualified name")
	}
	if a.QualifiedName() != "myrig/worker" {
		t.Errorf("got %q, want myrig/worker (rigContext must not override qualified name)", a.QualifiedName())
	}

	// No rig context: fall back to plain findAgent behavior (bare name
	// without context and no city-scoped match → not found).
	if _, ok := resolver.ResolveAgent(cfg, "worker", ""); ok {
		t.Error("expected bare name with no rig context + no city-scoped agent to fail")
	}
}

// TestSlingRigScopeRejectsUnknownBareTarget is the end-to-end sibling:
// a bare target that can't be rig-qualified must still 404 (not silently
// route to a wrong agent).
func TestSlingRigScopeRejectsUnknownBareTarget(t *testing.T) {
	srv, _ := newSlingTestServer(t)
	// No agent named "ghost" in any scope.
	body := `{"target":"ghost","bead":"abc","scope_kind":"rig","scope_ref":"myrig"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404; body = %s", rec.Code, rec.Body.String())
	}
}
