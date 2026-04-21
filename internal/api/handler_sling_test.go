package api

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
)

// newSlingTestServer creates a test handler wrapping a Server that has a
// fake runner injected (captures commands without executing real shell
// processes).
func newSlingTestServer(t *testing.T) (http.Handler, *fakeMutatorState) {
	t.Helper()
	state := newFakeMutatorState(t)
	state.cfg.Rigs[0].Prefix = "gc" // match MemStore's auto-generated prefix
	srv := New(state)
	srv.SlingRunnerFunc = func(_ string, _ string, _ map[string]string) (string, error) {
		return "", nil // no-op runner
	}
	return newTestCityHandlerWith(t, state, srv), state
}

func TestNewSyncsFormulaV2FeatureFlags(t *testing.T) {
	state := newFakeMutatorState(t)
	state.cfg.Daemon.FormulaV2 = true

	prevFormulaV2 := formula.IsFormulaV2Enabled()
	prevGraphApply := molecule.IsGraphApplyEnabled()
	formula.SetFormulaV2Enabled(false)
	molecule.SetGraphApplyEnabled(false)
	t.Cleanup(func() {
		formula.SetFormulaV2Enabled(prevFormulaV2)
		molecule.SetGraphApplyEnabled(prevGraphApply)
	})

	_ = New(state)

	if !formula.IsFormulaV2Enabled() {
		t.Fatal("formula.IsFormulaV2Enabled() = false, want true")
	}
	if !molecule.IsGraphApplyEnabled() {
		t.Fatal("molecule.IsGraphApplyEnabled() = false, want true")
	}
}

func TestSlingWithBead(t *testing.T) {
	h, state := newSlingTestServer(t)
	store := state.stores["myrig"]
	b, err := store.Create(beads.Bead{Title: "test task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"target":"myrig/worker","bead":"` + b.ID + `"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))

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

func TestSlingLogsMalformedCustomSlingQueryWarning(t *testing.T) {
	srv, state := newSlingTestServer(t)
	for i := range state.cfg.Agents {
		if state.cfg.Agents[i].QualifiedName() == "myrig/worker" {
			state.cfg.Agents[i].SlingQuery = "bd update {} --set-metadata gc.routed_to={{.Rig"
			break
		}
	}

	var stderr bytes.Buffer
	oldStderr := apiSlingStderr
	apiSlingStderr = func() io.Writer { return &stderr }
	t.Cleanup(func() { apiSlingStderr = oldStderr })

	store := state.stores["myrig"]
	b, err := store.Create(beads.Bead{Title: "test task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"target":"myrig/worker","bead":"` + b.ID + `"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(stderr.String(), "sling_query") {
		t.Fatalf("stderr missing field name: %q", stderr.String())
	}
	if strings.Contains(stderr.String(), "gc.routed_to={{.Rig") {
		t.Fatalf("stderr should redact raw template, got %q", stderr.String())
	}
}

func TestSlingMissingTarget(t *testing.T) {
	h, state := newSlingTestServer(t)
	_ = state
	body := `{"bead":"abc"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	// target is now marked required:true + minLength:1 in the spec, so
	// Huma rejects at the validator (422 Unprocessable Entity) before
	// the handler's explicit "target is required" 400 can fire. Either
	// status communicates "missing required field" unambiguously.
	if rec.Code != http.StatusBadRequest && rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 400 or 422 for missing target", rec.Code)
	}
}

func TestSlingTargetNotFound(t *testing.T) {
	h, state := newSlingTestServer(t)
	_ = state
	body := `{"target":"nonexistent","bead":"abc"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestSlingMissingBeadAndFormula(t *testing.T) {
	h, state := newSlingTestServer(t)
	_ = state
	body := `{"target":"myrig/worker"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSlingBeadAndFormulaMutuallyExclusive(t *testing.T) {
	h, state := newSlingTestServer(t)
	_ = state
	body := `{"target":"myrig/worker","bead":"abc","formula":"xyz"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
}

func TestSlingRejectsVarsWithoutFormula(t *testing.T) {
	h, state := newSlingTestServer(t)
	_ = state
	body := `{"target":"myrig/worker","bead":"BD-42","vars":{"issue":"BD-42"}}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSlingRejectsScopeWithoutFormula(t *testing.T) {
	h, state := newSlingTestServer(t)
	_ = state
	body := `{"target":"myrig/worker","bead":"BD-42","scope_kind":"city","scope_ref":"test-city"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSlingRejectsPartialScope(t *testing.T) {
	h, state := newSlingTestServer(t)
	_ = state
	body := `{"target":"myrig/worker","formula":"mol-review","scope_kind":"city"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
}

func TestSlingPoolTarget(t *testing.T) {
	h, state := newSlingTestServer(t)
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
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))

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

func TestSlingConflictReturns409ForExistingLiveWorkflow(t *testing.T) {
	// The Huma migration moved sling to /v0/city/{cityName}/sling and
	// replaced the old plain-JSON `{code, message, source_bead_id, ...}`
	// error body with RFC 9457 Problem Details. The source-workflow
	// conflict response now rides in the Problem Details `errors[]`
	// extensions (keyed by location so consumers can look them up
	// without format drift) instead of at the top level.
	//
	// FormulaV2 flag flow:
	//   1. newSlingTestServer → New() → syncFeatureFlags(state.cfg) sets the
	//      package-global `formula.IsFormulaV2Enabled` flag based on config,
	//      which is default-false out of newFakeMutatorState.
	//   2. We then set state.cfg.Daemon.FormulaV2 = true for reads that go
	//      through config (handler-level checks).
	//   3. The global flag is what formula compile calls, so we call
	//      formula.SetFormulaV2Enabled(true) AFTER newSlingTestServer so
	//      New()'s syncFeatureFlags doesn't stomp it back to false.
	prevFormulaV2 := formula.IsFormulaV2Enabled()
	prevGraphApply := molecule.IsGraphApplyEnabled()
	t.Cleanup(func() {
		formula.SetFormulaV2Enabled(prevFormulaV2)
		molecule.SetGraphApplyEnabled(prevGraphApply)
	})

	srv, state := newSlingTestServer(t)
	state.cfg.Daemon.FormulaV2 = true
	formula.SetFormulaV2Enabled(true)
	molecule.SetGraphApplyEnabled(true)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	state.cfg.Agents = append(state.cfg.Agents,
		config.Agent{Name: config.ControlDispatcherAgentName, MaxActiveSessions: intPtr(1)},
		config.Agent{Name: config.ControlDispatcherAgentName, Dir: "myrig", MaxActiveSessions: intPtr(1)},
	)
	if err := os.WriteFile(filepath.Join(formulaDir, "graph-work.formula.toml"), []byte(`
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	store := state.stores["myrig"]
	source, err := store.Create(beads.Bead{ID: "BL-42", Title: "test task", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	root, err := store.Create(beads.Bead{
		Title:  "existing workflow",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   source.ID,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	body := `{"target":"myrig/worker","formula":"graph-work","attached_bead_id":"` + source.ID + `"}`
	rec := httptest.NewRecorder()
	srv.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want 409; body = %s", rec.Code, rec.Body.String())
	}

	// Problem Details body: {title, status, detail, errors: [{location, value}, ...]}.
	var resp struct {
		Title  string `json:"title"`
		Status int    `json:"status"`
		Detail string `json:"detail"`
		Errors []struct {
			Location string `json:"location"`
			Value    any    `json:"value"`
		} `json:"errors"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if resp.Status != http.StatusConflict {
		t.Fatalf("status field = %d, want 409", resp.Status)
	}

	// Build a location -> value lookup so assertions don't depend on
	// the errors[] array order.
	got := map[string]any{}
	for _, e := range resp.Errors {
		got[e.Location] = e.Value
	}

	if got["body.source_bead_id"] != source.ID {
		t.Fatalf("source_bead_id = %#v, want %s", got["body.source_bead_id"], source.ID)
	}
	ids, ok := got["body.blocking_workflow_ids"].([]any)
	if !ok || len(ids) != 1 || ids[0] != root.ID {
		t.Fatalf("blocking_workflow_ids = %#v, want [%s]", got["body.blocking_workflow_ids"], root.ID)
	}
	hint, _ := got["body.hint"].(string)
	if !strings.Contains(hint, "--store-ref rig:myrig --apply") {
		t.Fatalf("hint = %q, want store-ref cleanup command", hint)
	}
}

// TestQualifySlingTarget covers the rig-aware target qualification
// helper. Given a rigContext (derived from scope_ref for UI dispatches
// or body.Rig for dashboard dispatches), the helper rewrites a bare
// target to "<rigContext>/<name>" when that qualified form resolves.
// In all other cases (empty context, already-qualified target, no
// matching agent) the target passes through unchanged.
func TestQualifySlingTarget(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", MaxActiveSessions: intPtr(1)},
			{Name: "mayor", MaxActiveSessions: intPtr(1)}, // city-scoped
		},
	}

	cases := []struct {
		name       string
		target     string
		rigContext string
		want       string
	}{
		{"rig_bare_qualifies", "worker", "myrig", "myrig/worker"},
		{"rig_bare_no_match_unchanged", "worker", "otherrig", "worker"},
		{"already_qualified_unchanged", "myrig/worker", "myrig", "myrig/worker"},
		{"empty_context_unchanged", "worker", "", "worker"},
		{"rig_city_scoped_fallthrough", "mayor", "myrig", "mayor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := qualifySlingTarget(cfg, tc.target, tc.rigContext)
			if got != tc.want {
				t.Errorf("qualifySlingTarget(%q, %q) = %q, want %q", tc.target, tc.rigContext, got, tc.want)
			}
		})
	}
}

// TestSlingRigContext locks in the rigContext derivation rules used
// by handleSling to pick between scope_ref (UI intent) and body.Rig
// (dashboard/--rig CLI intent).
func TestSlingRigContext(t *testing.T) {
	cases := []struct {
		name string
		body slingBody
		want string
	}{
		{"scope_ref_wins", slingBody{ScopeKind: "rig", ScopeRef: "myrig", Rig: "otherrig"}, "myrig"},
		{"body_rig_when_no_scope", slingBody{Rig: "myrig"}, "myrig"},
		{"empty_when_city_scope", slingBody{ScopeKind: "city", ScopeRef: "test-city", Rig: "myrig"}, ""},
		{"empty_when_nothing_set", slingBody{}, ""},
		{"rig_scope_without_ref_is_empty", slingBody{ScopeKind: "rig"}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := slingRigContext(tc.body); got != tc.want {
				t.Errorf("slingRigContext(%+v) = %q, want %q", tc.body, got, tc.want)
			}
		})
	}
}

// TestSlingDashboardRigQualifiesBareTarget is the E2E complement:
// the dashboard's sling command passes --rig=X as body.Rig (no
// scope_kind/scope_ref), and bare targets must still be qualified
// to the matching rig-scoped agent rather than 404ing.
func TestSlingDashboardRigQualifiesBareTarget(t *testing.T) {
	h, state := newSlingTestServer(t)
	// Bare "worker" with body.Rig="myrig" (no scope_kind) — mirrors
	// `sling <bead> worker --rig=myrig` via cmd/gc/dashboard/api.go.
	// Must resolve to myrig/worker and hit the happy direct-bead path.
	body := `{"target":"worker","bead":"abc","rig":"myrig"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code == http.StatusNotFound {
		t.Fatalf("got 404 — body.Rig qualification did not apply; body = %s", rec.Body.String())
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

// TestSlingRejectsScopeRefQualifiedTargetMismatch covers the split-brain
// case: a qualified target pointing at one rig while scope_ref names a
// different rig. Store selection follows agentCfg.Dir while the formula's
// ScopeRef flows from body.ScopeRef, so silently accepting this would
// route beads and formula scope to different rigs. Must reject upfront.
func TestSlingRejectsScopeRefQualifiedTargetMismatch(t *testing.T) {
	h, state := newSlingTestServer(t)
	// Add a second rig + agent so both "myrig/worker" and "otherrig/worker" exist.
	state.cfg.Rigs = append(state.cfg.Rigs, config.Rig{Name: "otherrig", Path: "/tmp/otherrig", Prefix: "gc"})
	state.cfg.Agents = append(state.cfg.Agents, config.Agent{
		Name: "worker", Dir: "otherrig", Provider: "test-agent", MaxActiveSessions: intPtr(1),
	})
	state.stores["otherrig"] = beads.NewMemStore()

	// Qualified target says otherrig; scope_ref says myrig — reject.
	body := `{"target":"otherrig/worker","formula":"mol-review","scope_kind":"rig","scope_ref":"myrig"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "conflicts with resolved target rig") {
		t.Errorf("error message = %s; expected mismatch diagnostic", rec.Body.String())
	}
}

// TestSlingAllowsScopeRefQualifiedTargetMatch verifies that when the
// qualified target's rig matches scope_ref, the handler does NOT reject.
// Belt-and-suspenders — ensures the mismatch guard doesn't fire on
// consistent inputs.
func TestSlingAllowsScopeRefQualifiedTargetMatch(t *testing.T) {
	h, state := newSlingTestServer(t)
	// Matching scope: target=myrig/worker, scope_ref=myrig — should pass
	// the mismatch guard and then trip the formula-required validation
	// (the next validation downstream). Either result is fine as long
	// as it is NOT the mismatch error.
	body := `{"target":"myrig/worker","bead":"BD-42","scope_kind":"rig","scope_ref":"myrig"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if strings.Contains(rec.Body.String(), "conflicts") {
		t.Errorf("should not reject matching scope_ref/target; body = %s", rec.Body.String())
	}
}

// TestSlingRejectsCityScopedAgentWithRigScope catches the bare-name
// fall-through that iter2's original guard missed: a caller asks for
// rig scope but the bare target resolves to a city-scoped agent
// (agentCfg.Dir == ""). findSlingStore would select the city bead
// store while FormulaOpts.ScopeRef would claim rig scope — split-brain.
func TestSlingRejectsCityScopedAgentWithRigScope(t *testing.T) {
	h, state := newSlingTestServer(t)
	// Add a city-scoped agent.
	state.cfg.Agents = append(state.cfg.Agents, config.Agent{
		Name:              "mayor",
		Provider:          "test-agent",
		MaxActiveSessions: intPtr(1),
	})

	// Bare "mayor" + scope_kind=rig — qualifySlingTarget will not find
	// "myrig/mayor", falls through to city-scoped mayor, guard must reject.
	body := `{"target":"mayor","formula":"mol-review","scope_kind":"rig","scope_ref":"myrig"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "city-scoped") {
		t.Errorf("error = %s; expected city-scoped diagnostic", rec.Body.String())
	}
}

// TestSlingRejectsBodyRigMismatch catches the case where the caller
// explicitly sets body.Rig to something different from scope_ref.
// body.Rig wins store selection in findSlingStore, so disagreement
// produces split-brain dispatch.
func TestSlingRejectsBodyRigMismatch(t *testing.T) {
	h, state := newSlingTestServer(t)
	state.cfg.Rigs = append(state.cfg.Rigs, config.Rig{Name: "otherrig", Path: "/tmp/otherrig", Prefix: "gc"})
	state.stores["otherrig"] = beads.NewMemStore()

	body := `{"target":"myrig/worker","formula":"mol-review","scope_kind":"rig","scope_ref":"myrig","rig":"otherrig"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400; body = %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "rig otherrig") {
		t.Errorf("error = %s; expected body-rig mismatch diagnostic", rec.Body.String())
	}
}

// TestSlingRigScopeRejectsUnknownBareTarget is the end-to-end sibling:
// a bare target that can't be rig-qualified must still 404 (not silently
// route to a wrong agent).
func TestSlingRigScopeRejectsUnknownBareTarget(t *testing.T) {
	h, state := newSlingTestServer(t)
	// No agent named "ghost" in any scope.
	body := `{"target":"ghost","bead":"abc","scope_kind":"rig","scope_ref":"myrig"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
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
	h, state := newSlingTestServer(t)
	// Bare "worker" must be qualified to "myrig/worker" by handleSling
	// before findAgent is called. If the rewrite is broken, findAgent
	// returns 404 for bare "worker". If it's working, the handler moves
	// on and trips the "formula required when scope is set" rule (400).
	body := `{"target":"worker","bead":"BD-42","scope_kind":"rig","scope_ref":"myrig"}`
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, newPostRequest(cityURL(state, "/sling"), strings.NewReader(body)))
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
// is either an intentional divergence (documented below) or a bug.
//
// Coverage dimensions:
//   - simple agents (rig-scoped + city-scoped)
//   - pool members (bare "polecat-N", qualified "rig/polecat-N")
//   - V2 BindingName pool prefixes ("pack.name-N")
//
// Intentional divergences:
//
//   - Bare rig-scoped name with no rig context: apiAgentResolver
//     deliberately omits the CLI's step-3 unambiguous-bare-name scan
//     to avoid ambiguity in multi-rig cities. agentutil with
//     UseAmbientRig=false also declines, so both agree — but the CLI
//     path (resolveAgentIdentity) would succeed; that's expected.
//
//   - Pool-instance shape: findAgent resolves "rig/polecat-N" to the
//     pool TEMPLATE agent, leaving synthesis to the caller, while
//     agentutil with AllowPoolMembers=true returns the synthesized
//     INSTANCE directly. Same shape difference applies to V2
//     BindingName pool members ("rig/binding.name-N"). Both shapes
//     are valid; a future unification will need to pick one.
func TestApiVsAgentutilResolverParity(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{
			{Name: "worker", Dir: "myrig", MaxActiveSessions: intPtr(1)},
			{Name: "worker", Dir: "otherrig", MaxActiveSessions: intPtr(1)},
			{Name: "mayor", MaxActiveSessions: intPtr(1)}, // city-scoped, unique
			// Pool agent (multi-session, unlimited)
			{Name: "polecat", Dir: "myrig", MaxActiveSessions: intPtr(-1)},
			// V2 bound pool (BindingName prefix)
			{Name: "witness", Dir: "myrig", BindingName: "gastown", MaxActiveSessions: intPtr(-1)},
		},
	}
	apiRes := apiAgentResolver{}

	// Each case locks in the expected behavior of BOTH resolvers for
	// the same input. Where apiWantQName != utilWantQName (or found
	// values differ), that is an intentional divergence — documented
	// in the function comment above. Changing either without a
	// matching update to the other means the parity guarantee has
	// shifted and the test call site should be audited.
	cases := []struct {
		name          string
		input         string
		rigContext    string
		apiWantFound  bool
		apiWantQName  string
		utilWantFound bool
		utilWantQName string
	}{
		// Shared behavior: simple agents, rig context, city-scoped.
		{"qualified_name", "myrig/worker", "", true, "myrig/worker", true, "myrig/worker"},
		{"qualified_name_with_rig_context", "myrig/worker", "otherrig", true, "myrig/worker", true, "myrig/worker"},
		{"bare_name_with_rig_context", "worker", "myrig", true, "myrig/worker", true, "myrig/worker"},
		{"bare_name_with_other_rig_context", "worker", "otherrig", true, "otherrig/worker", true, "otherrig/worker"},
		{"city_scoped_bare_name", "mayor", "", true, "mayor", true, "mayor"},
		{"city_scoped_bare_name_with_rig_context", "mayor", "myrig", true, "mayor", true, "mayor"},
		{"bare_name_no_context_ambiguous_rig_scoped", "worker", "", false, "", false, ""},

		// Pool-member divergence: findAgent resolves a pool instance
		// request to the POOL TEMPLATE (caller then synthesizes).
		// agentutil with AllowPoolMembers=true synthesizes the
		// INSTANCE directly. Both are valid shapes, but the contract
		// must not shift silently.
		{"qualified_pool_member", "myrig/polecat-2", "", true, "myrig/polecat", true, "myrig/polecat-2"},

		// V2 BindingName divergence: both resolvers recognize the
		// "<binding>.<name>-N" prefix, but with the same template
		// vs instance shape as the polecat case — findAgent returns
		// the pool template (binding-qualified), agentutil returns
		// the synthesized instance.
		{"v2_binding_pool_member", "myrig/gastown.witness-1", "", true, "myrig/gastown.witness", true, "myrig/gastown.witness-1"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			apiAgent, apiOK := apiRes.ResolveAgent(cfg, tc.input, tc.rigContext)
			if apiOK != tc.apiWantFound {
				t.Fatalf("apiAgentResolver found=%v, want %v", apiOK, tc.apiWantFound)
			}
			if apiOK && apiAgent.QualifiedName() != tc.apiWantQName {
				t.Fatalf("apiAgentResolver QualifiedName = %q, want %q", apiAgent.QualifiedName(), tc.apiWantQName)
			}
			utilAgent, utilOK := agentutil.ResolveAgent(cfg, tc.input, agentutil.ResolveOpts{
				UseAmbientRig:    tc.rigContext != "",
				RigContext:       tc.rigContext,
				AllowPoolMembers: true,
			})
			if utilOK != tc.utilWantFound {
				t.Fatalf("agentutil.ResolveAgent found=%v, want %v", utilOK, tc.utilWantFound)
			}
			if utilOK && utilAgent.QualifiedName() != tc.utilWantQName {
				t.Fatalf("agentutil.ResolveAgent QualifiedName = %q, want %q", utilAgent.QualifiedName(), tc.utilWantQName)
			}
		})
	}
}
