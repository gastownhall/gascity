package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
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

// TestSlingRigScopeE2EReachesFormulaValidation is the end-to-end
// regression guard for the target rewrite. A bare target with
// scope_kind=rig + a matching rig-scoped agent must make it past
// handleSling's agent lookup and hit the downstream "formula required
// when scope is set" validation — any regression in qualifySlingTarget
// or its invocation would 404 here instead of 400.
//
// This is the single observable boundary where we can prove the
// end-to-end /v0/sling → target rewrite wiring still works without
// dragging in real formula instantiation machinery.
func TestSlingRigScopeE2EReachesFormulaValidation(t *testing.T) {
	srv, _ := newSlingTestServer(t)
	// Bare "worker" must be qualified to "myrig/worker" by handleSling
	// before findAgent is called. If the rewrite is broken, findAgent
	// returns 404 for bare "worker". If it's working, the handler moves
	// on and trips the "formula required when scope is set" rule (400).
	body := `{"target":"worker","bead":"BD-42","scope_kind":"rig","scope_ref":"myrig"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest("/v0/sling", strings.NewReader(body)))
	if rec.Code == http.StatusNotFound {
		t.Fatalf("got 404 — qualifySlingTarget did not rewrite bare target; body = %s", rec.Body.String())
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (formula-required); body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "formula") {
		t.Errorf("body = %s; expected formula-required error (proves we got past agent lookup)", rec.Body.String())
	}
}

// TestApiVsAgentutilResolverParity locks in the current behavioral
// contract between apiAgentResolver and agentutil.ResolveAgent so that
// future drift between CLI and API resolution surfaces as a test
// failure rather than a silent regression (the exact class of bug
// that motivated this PR). Any case where the two resolvers disagree
// is either an intentional divergence (document it here) or a bug.
func TestApiVsAgentutilResolverParity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", MaxActiveSessions: intPtr(1)},
			{Name: "worker", Dir: "otherrig", MaxActiveSessions: intPtr(1)},
			{Name: "mayor", MaxActiveSessions: intPtr(1)}, // city-scoped, unique
		},
	}
	apiRes := apiAgentResolver{}

	cases := []struct {
		name       string
		input      string
		rigContext string
		wantFound  bool
		wantQName  string
		// mustAgree: both resolvers must give the same answer.
		// When false, this is an intentional divergence documented below.
		mustAgree bool
	}{
		{"qualified_name", "myrig/worker", "", true, "myrig/worker", true},
		{"qualified_name_with_rig_context", "myrig/worker", "otherrig", true, "myrig/worker", true},
		{"bare_name_with_rig_context", "worker", "myrig", true, "myrig/worker", true},
		{"bare_name_with_other_rig_context", "worker", "otherrig", true, "otherrig/worker", true},
		{"city_scoped_bare_name", "mayor", "", true, "mayor", true},
		{"city_scoped_bare_name_with_rig_context", "mayor", "myrig", true, "mayor", true},
		// Intentional divergence: API resolver deliberately omits
		// step-3 unambiguous-bare-name fallback, so a bare rig-scoped
		// name with no rig context does NOT resolve through the API.
		// agentutil.ResolveAgent with UseAmbientRig=false also skips
		// it; enabling it would require AllowPoolMembers=true and
		// would expose BindingName gaps. See apiAgentResolver doc.
		{"bare_name_no_context_ambiguous_rig_scoped", "worker", "", false, "", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiAgent, apiOK := apiRes.ResolveAgent(cfg, tc.input, tc.rigContext)
			if apiOK != tc.wantFound {
				t.Fatalf("apiAgentResolver found=%v, want %v", apiOK, tc.wantFound)
			}
			if apiOK && apiAgent.QualifiedName() != tc.wantQName {
				t.Fatalf("apiAgentResolver QualifiedName = %q, want %q", apiAgent.QualifiedName(), tc.wantQName)
			}
			if !tc.mustAgree {
				return
			}
			utilAgent, utilOK := agentutil.ResolveAgent(cfg, tc.input, agentutil.ResolveOpts{
				UseAmbientRig: tc.rigContext != "",
				RigContext:    tc.rigContext,
			})
			if apiOK != utilOK {
				t.Errorf("resolver disagreement: apiAgentResolver found=%v, agentutil.ResolveAgent found=%v", apiOK, utilOK)
			}
			if apiOK && utilOK && apiAgent.QualifiedName() != utilAgent.QualifiedName() {
				t.Errorf("resolver disagreement: api=%q, util=%q", apiAgent.QualifiedName(), utilAgent.QualifiedName())
			}
		})
	}
}
