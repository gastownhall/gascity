package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

func TestWorkflowGetSelectsScopedRootMatch(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	state.cityBeadStore = cityStore
	state.stores = map[string]beads.Store{"alpha": rigStore}

	_, err := cityStore.Create(beads.Bead{
		Title: "City workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_shared",
		},
	})
	if err != nil {
		t.Fatalf("Create(cityRoot): %v", err)
	}
	rigRoot, err := rigStore.Create(beads.Bead{
		Title: "Rig workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_shared",
		},
	})
	if err != nil {
		t.Fatalf("Create(rigRoot): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_shared?scope_kind=rig&scope_ref=alpha", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	if snapshot.RootBeadID != rigRoot.ID {
		t.Fatalf("root_bead_id = %q, want %q", snapshot.RootBeadID, rigRoot.ID)
	}
	if snapshot.RootStoreRef != "rig:alpha" {
		t.Fatalf("root_store_ref = %q, want rig:alpha", snapshot.RootStoreRef)
	}
	if snapshot.ScopeKind != "rig" || snapshot.ScopeRef != "alpha" {
		t.Fatalf("scope = %s:%s, want rig:alpha", snapshot.ScopeKind, snapshot.ScopeRef)
	}
	if len(snapshot.Beads) == 0 || snapshot.Beads[0].Title != rigRoot.Title {
		t.Fatalf("selected workflow title = %q, want %q", firstWorkflowBeadTitle(snapshot.Beads), rigRoot.Title)
	}
}

func TestWorkflowGetPreservesRequestedScopeForUniqueCrossStoreWorkflow(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	rigStore := beads.NewMemStore()
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	root, err := rigStore.Create(beads.Bead{
		Title: "Cross-store workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_city_scope",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_city_scope?scope_kind=city&scope_ref=gascity", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	if snapshot.RootBeadID != root.ID {
		t.Fatalf("root_bead_id = %q, want %q", snapshot.RootBeadID, root.ID)
	}
	if snapshot.RootStoreRef != "rig:alpha" {
		t.Fatalf("root_store_ref = %q, want rig:alpha", snapshot.RootStoreRef)
	}
	if snapshot.ScopeKind != "city" || snapshot.ScopeRef != "gascity" {
		t.Fatalf("scope = %s:%s, want city:gascity", snapshot.ScopeKind, snapshot.ScopeRef)
	}
}

func TestWorkflowGetRejectsMismatchedCityScopeForUniqueCrossStoreWorkflow(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	rigStore := beads.NewMemStore()
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	if _, err := rigStore.Create(beads.Bead{
		Title: "Cross-store workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_wrong_city_scope",
		},
	}); err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_wrong_city_scope?scope_kind=city&scope_ref=other-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkflowGetRejectsInvalidScopeKind(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore

	if _, err := cityStore.Create(beads.Bead{
		Title: "Workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":        "workflow",
			"gc.workflow_id": "wf_invalid_scope",
		},
	}); err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_invalid_scope?scope_kind=workspace&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkflowGetRejectsMismatchedRigScopeForUniqueCrossStoreWorkflow(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	rigStore := beads.NewMemStore()
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	if _, err := rigStore.Create(beads.Bead{
		Title: "Rig workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_rig_only",
		},
	}); err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_rig_only?scope_kind=rig&scope_ref=beta", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestWorkflowGetMarksSnapshotPartialWhenDepListFails(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	memStore := beads.NewMemStore()
	state.cityBeadStore = depListFailStore{Store: memStore}

	root, err := memStore.Create(beads.Bead{
		Title: "Workflow with dep errors",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_partial",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	if _, err := memStore.Create(beads.Bead{
		Title: "Work",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.work",
		},
	}); err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_partial?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}
	if !snapshot.Partial {
		t.Fatalf("partial = %v, want true", snapshot.Partial)
	}
}

func TestWorkflowGetAllowsMissingScopeFields(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore

	root, err := cityStore.Create(beads.Bead{
		Title: "Scope optional workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_missing_scope",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_missing_scope", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}
	if snapshot.RootBeadID != root.ID {
		t.Fatalf("root_bead_id = %q, want %q", snapshot.RootBeadID, root.ID)
	}
}

func TestWorkflowGetScopedRequestSurvivesUnrelatedStoreListFailure(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore
	state.stores = map[string]beads.Store{
		"alpha": failListStore{Store: beads.NewMemStore()},
	}

	if _, err := cityStore.Create(beads.Bead{
		Title: "City workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_city_partial",
		},
	}); err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_city_partial?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}
	if snapshot.RootStoreRef != "city:test-city" {
		t.Fatalf("root_store_ref = %q, want city:test-city", snapshot.RootStoreRef)
	}
	if !snapshot.Partial {
		t.Fatalf("partial = %v, want true", snapshot.Partial)
	}
}

func TestWorkflowGetUsesSingleSnapshotIndexForHeaderAndBody(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	state.cityBeadStore = beads.NewMemStore()
	state.eventProv = &incrementingLatestSeqProvider{}

	if _, err := state.cityBeadStore.Create(beads.Bead{
		Title: "Workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_index",
		},
	}); err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/wf_index?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	if got := rec.Header().Get("X-GC-Index"); got != "1" {
		t.Fatalf("X-GC-Index = %q, want 1", got)
	}
	if snapshot.SnapshotVersion != 1 {
		t.Fatalf("snapshot_version = %d, want 1", snapshot.SnapshotVersion)
	}
	if snapshot.SnapshotEventSeq == nil || *snapshot.SnapshotEventSeq != 1 {
		t.Fatalf("snapshot_event_seq = %v, want 1", snapshot.SnapshotEventSeq)
	}
}

func TestWorkflowStoreByRef(t *testing.T) {
	state := newFakeState(t)
	state.cityName = ""
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{
		"alpha": beads.NewMemStore(),
	}

	cityInfo, ok := workflowStoreByRef(state, "city:city")
	if !ok {
		t.Fatal("workflowStoreByRef(city:city) = false, want true")
	}
	if cityInfo.ref != "city:city" || cityInfo.scopeKind != "city" || cityInfo.scopeRef != "city" {
		t.Fatalf("city info = %+v, want city:city", cityInfo)
	}
	if cityInfo.store != state.cityBeadStore {
		t.Fatal("city store mismatch")
	}

	rigInfo, ok := workflowStoreByRef(state, "rig:alpha")
	if !ok {
		t.Fatal("workflowStoreByRef(rig:alpha) = false, want true")
	}
	if rigInfo.ref != "rig:alpha" || rigInfo.scopeKind != "rig" || rigInfo.scopeRef != "alpha" {
		t.Fatalf("rig info = %+v, want rig:alpha", rigInfo)
	}
	if rigInfo.store != state.stores["alpha"] {
		t.Fatal("rig store mismatch")
	}

	if _, ok := workflowStoreByRef(state, "city:bright-lights"); ok {
		t.Fatal("workflowStoreByRef(city:bright-lights) = true, want false")
	}
	if _, ok := workflowStoreByRef(state, "rig:missing"); ok {
		t.Fatal("workflowStoreByRef(rig:missing) = true, want false")
	}
}

func TestWorkflowStoresSkipsCityStoreEntriesFromBeadStoreMap(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore
	state.cfg.Rigs = []config.Rig{
		{Name: "alpha", Path: t.TempDir()},
	}
	state.stores = map[string]beads.Store{
		"bright-lights": cityStore,
		"alpha":         beads.NewMemStore(),
	}

	stores := workflowStores(state)
	if len(stores) != 2 {
		t.Fatalf("workflowStores() returned %d entries, want 2", len(stores))
	}
	if stores[0].ref != "city:bright-lights" {
		t.Fatalf("stores[0].ref = %q, want city:bright-lights", stores[0].ref)
	}
	if stores[1].ref != "rig:alpha" {
		t.Fatalf("stores[1].ref = %q, want rig:alpha", stores[1].ref)
	}
}

func TestWorkflowStorePathResolvesCityAndRigPaths(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.cityPath = t.TempDir()
	state.cityBeadStore = beads.NewMemStore()
	absoluteRigPath := t.TempDir()
	state.cfg.Rigs = []config.Rig{
		{Name: "alpha", Path: absoluteRigPath},
		{Name: "beta", Path: "repos/beta"},
	}
	state.stores = map[string]beads.Store{
		"alpha": beads.NewMemStore(),
		"beta":  beads.NewMemStore(),
	}

	cityInfo, ok := workflowStoreByRef(state, "city:bright-lights")
	if !ok {
		t.Fatal("workflowStoreByRef(city:bright-lights) = false, want true")
	}
	cityPath, ok := workflowStorePath(state, cityInfo)
	if !ok {
		t.Fatal("workflowStorePath(city) = false, want true")
	}
	if cityPath != state.cityPath {
		t.Fatalf("cityPath = %q, want %q", cityPath, state.cityPath)
	}

	alphaInfo, ok := workflowStoreByRef(state, "rig:alpha")
	if !ok {
		t.Fatal("workflowStoreByRef(rig:alpha) = false, want true")
	}
	alphaPath, ok := workflowStorePath(state, alphaInfo)
	if !ok {
		t.Fatal("workflowStorePath(rig:alpha) = false, want true")
	}
	if alphaPath != absoluteRigPath {
		t.Fatalf("alphaPath = %q, want %q", alphaPath, absoluteRigPath)
	}

	betaInfo, ok := workflowStoreByRef(state, "rig:beta")
	if !ok {
		t.Fatal("workflowStoreByRef(rig:beta) = false, want true")
	}
	betaPath, ok := workflowStorePath(state, betaInfo)
	if !ok {
		t.Fatal("workflowStorePath(rig:beta) = false, want true")
	}
	wantBetaPath := filepath.Join(state.cityPath, "repos/beta")
	if betaPath != wantBetaPath {
		t.Fatalf("betaPath = %q, want %q", betaPath, wantBetaPath)
	}
}

func TestWorkflowSQLStoreCandidatesPreferRequestedScope(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.cityPath = t.TempDir()
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{
		"alpha": beads.NewMemStore(),
		"beta":  beads.NewMemStore(),
	}
	state.cfg.Rigs = []config.Rig{
		{Name: "alpha", Path: t.TempDir()},
		{Name: "beta", Path: "repos/beta"},
	}

	candidates := workflowSQLStoreCandidates(state, "rig", "beta")
	if len(candidates) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(candidates))
	}
	if candidates[0].info.ref != "rig:beta" {
		t.Fatalf("candidate.ref = %q, want rig:beta", candidates[0].info.ref)
	}
	wantPath := filepath.Join(state.cityPath, "repos/beta")
	if candidates[0].path != wantPath {
		t.Fatalf("candidate.path = %q, want %q", candidates[0].path, wantPath)
	}

	allCandidates := workflowSQLStoreCandidates(state, "", "")
	if len(allCandidates) != 3 {
		t.Fatalf("len(allCandidates) = %d, want 3", len(allCandidates))
	}
	if allCandidates[0].info.ref != "city:bright-lights" {
		t.Fatalf("allCandidates[0].ref = %q, want city:bright-lights", allCandidates[0].info.ref)
	}
}

func TestWorkflowSQLCandidatesForWorkflowIDResolveBeadPrefixViaRoutes(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.cityPath = t.TempDir()
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Rigs = []config.Rig{
		{Name: "alpha", Path: "rigs/alpha"},
		{Name: "beta", Path: "rigs/beta"},
	}
	state.stores = map[string]beads.Store{
		"alpha": beads.NewMemStore(),
		"beta":  beads.NewMemStore(),
	}

	alphaPath := filepath.Join(state.cityPath, "rigs/alpha")
	if err := os.MkdirAll(filepath.Join(alphaPath, ".beads"), 0o755); err != nil {
		t.Fatalf("MkdirAll(alpha .beads): %v", err)
	}
	routes := `{"prefix":"ga","path":"."}` + "\n" + `{"prefix":"gb","path":"../beta"}`
	if err := os.WriteFile(filepath.Join(alphaPath, ".beads", "routes.jsonl"), []byte(routes), 0o644); err != nil {
		t.Fatalf("WriteFile(routes.jsonl): %v", err)
	}

	candidates := workflowSQLCandidatesForWorkflowID(state, "ga-abcd", "", "")
	if len(candidates) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(candidates))
	}
	if candidates[0].info.ref != "rig:alpha" {
		t.Fatalf("candidate.ref = %q, want rig:alpha", candidates[0].info.ref)
	}
	if candidates[0].path != alphaPath {
		t.Fatalf("candidate.path = %q, want %q", candidates[0].path, alphaPath)
	}
}

func TestWorkflowGetNormalizesShortScopeRefs(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore

	root, err := cityStore.Create(beads.Bead{
		Title: "Scoped workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	_, err = cityStore.Create(beads.Bead{
		Title: "Worktree scope",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "expansion.worktree",
			"gc.scope_role":   "body",
		},
	})
	if err != nil {
		t.Fatalf("Create(body): %v", err)
	}

	member, err := cityStore.Create(beads.Bead{
		Title: "Implement",
		Type:  "task",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "expansion.implement",
			"gc.scope_ref":    "worktree",
			"gc.scope_role":   "member",
		},
	})
	if err != nil {
		t.Fatalf("Create(member): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/"+root.ID+"?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	// Verify the member bead is present in the raw beads array with its scope_ref.
	found := false
	for _, b := range snapshot.Beads {
		if b.ID == member.ID {
			found = true
			if b.ScopeRef != "worktree" {
				t.Fatalf("member scope_ref = %q, want worktree", b.ScopeRef)
			}
		}
	}
	if !found {
		t.Fatalf("member bead %q not found in snapshot", member.ID)
	}
}

func TestWorkflowStatusTreatsOpenAssignedWorkAsPending(t *testing.T) {
	t.Parallel()

	bead := beads.Bead{
		Status:   "open",
		Assignee: "mayor",
		Metadata: map[string]string{
			"gc.routed_to": "mayor",
		},
	}

	if got := workflowStatus(bead); got != "pending" {
		t.Fatalf("workflowStatus(open assigned) = %q, want pending", got)
	}
}

func TestWorkflowStatusRequiresAssignmentForActive(t *testing.T) {
	t.Parallel()

	bead := beads.Bead{
		Status: "in_progress",
	}

	if got := workflowStatus(bead); got != "pending" {
		t.Fatalf("workflowStatus(in_progress unassigned) = %q, want pending", got)
	}
}

func TestWorkflowStatusDoesNotTreatRoutedOnlyWorkAsActive(t *testing.T) {
	t.Parallel()

	bead := beads.Bead{
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.routed_to": "mayor",
		},
	}

	if got := workflowStatus(bead); got != "pending" {
		t.Fatalf("workflowStatus(in_progress routed-only) = %q, want pending", got)
	}
}

func TestWorkflowStatusTreatsSkippedAsSkipped(t *testing.T) {
	t.Parallel()

	bead := beads.Bead{
		Status: "closed",
		Metadata: map[string]string{
			"gc.outcome": "skipped",
		},
	}

	if got := workflowStatus(bead); got != "skipped" {
		t.Fatalf("workflowStatus(closed skipped) = %q, want skipped", got)
	}
}

func TestWorkflowGetRejectsNonWorkflowRoot(t *testing.T) {
	state := newFakeState(t)
	cityStore := beads.NewMemStore()
	state.cityBeadStore = cityStore

	bead, err := cityStore.Create(beads.Bead{
		Title: "Not a workflow",
		Type:  "task",
	})
	if err != nil {
		t.Fatalf("Create(bead): %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/workflow/"+bead.ID+"?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func firstWorkflowBeadTitle(beads []workflowBeadResponse) string {
	if len(beads) == 0 {
		return ""
	}
	return beads[0].Title
}

type depListFailStore struct {
	beads.Store
}

func (s depListFailStore) DepList(string, string) ([]beads.Dep, error) {
	return nil, errors.New("dep list failed")
}

type failListStore struct {
	beads.Store
}

func (s failListStore) ListOpen(_ ...string) ([]beads.Bead, error) {
	return nil, errors.New("list failed")
}

type incrementingLatestSeqProvider struct {
	seq uint64
}

func (p *incrementingLatestSeqProvider) Record(events.Event) {}

func (p *incrementingLatestSeqProvider) List(events.Filter) ([]events.Event, error) {
	return nil, nil
}

func (p *incrementingLatestSeqProvider) LatestSeq() (uint64, error) {
	p.seq++
	return p.seq, nil
}

func (p *incrementingLatestSeqProvider) Watch(context.Context, uint64) (events.Watcher, error) {
	return nil, errors.New("not implemented")
}

func (p *incrementingLatestSeqProvider) Close() error {
	return nil
}
