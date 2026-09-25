package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
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

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_shared?scope_kind=rig&scope_ref=alpha"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_city_scope?scope_kind=city&scope_ref=gascity"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

// TestWorkflowGetRejectsCityScopeForRigRootedWorkflow pins the intended answer
// for the shape the preserve path above no longer covers: the same unique
// rig-stored workflow, but with a root that names rig:alpha itself. The root
// ref is the scope the workflow belongs to, so the city-scoped read is a miss
// (404) and the rig-scoped read is the hit — the deliberate ga-dezas
// consequence, not a regression to restore. graphroute stamps
// gc.root_store_ref on every launched step, so this is the shape current
// graph.v2 workflows actually take.
func TestWorkflowGetRejectsCityScopeForRigRootedWorkflow(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "gascity"
	rigStore := beads.NewMemStore()
	state.cityBeadStore = beads.NewMemStore()
	state.stores = map[string]beads.Store{"alpha": rigStore}

	root, err := rigStore.Create(beads.Bead{
		Title: "Rig-rooted workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_rig_rooted_scope",
			"gc.root_store_ref":   "rig:alpha",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_rig_rooted_scope?scope_kind=city&scope_ref=gascity"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("city-scoped status = %d, want 404: %s", rec.Code, rec.Body.String())
	}

	// The 404 must be the scope answer, not the workflow being unreachable.
	req = httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_rig_rooted_scope?scope_kind=rig&scope_ref=alpha"), nil)
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("rig-scoped status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}
	if snapshot.RootBeadID != root.ID {
		t.Fatalf("root_bead_id = %q, want %q", snapshot.RootBeadID, root.ID)
	}
	if snapshot.ScopeKind != "rig" || snapshot.ScopeRef != "alpha" {
		t.Fatalf("scope = %s:%s, want rig:alpha", snapshot.ScopeKind, snapshot.ScopeRef)
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

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_wrong_city_scope?scope_kind=city&scope_ref=other-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_invalid_scope?scope_kind=workspace&scope_ref=test-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_rig_only?scope_kind=rig&scope_ref=beta"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

// TestWorkflowRootScopeFallsBackToRootStoreRef pins the root-ref fallback
// (ga-dezas): sling-launch roots stamp only gc.root_store_ref, so the scope a
// workflow is presented in comes from the ref its root names, not from the
// store leg the root happened to be read through. Mirrors runproj's
// ScopeFromRootStoreRef, which already resolves the run projection this way.
func TestWorkflowRootScopeFallsBackToRootStoreRef(t *testing.T) {
	tests := []struct {
		name     string
		metadata map[string]string
		wantKind string
		wantRef  string
	}{
		{
			name: "explicit stamps win over the root store ref",
			metadata: map[string]string{
				"gc.scope_kind":     "rig",
				"gc.scope_ref":      "alpha",
				"gc.root_store_ref": "city:test-city",
			},
			wantKind: "rig",
			wantRef:  "alpha",
		},
		{
			name:     "rig root store ref",
			metadata: map[string]string{"gc.root_store_ref": "rig:beads"},
			wantKind: "rig",
			wantRef:  "beads",
		},
		{
			name:     "city root store ref",
			metadata: map[string]string{"gc.root_store_ref": "city:test-city"},
			wantKind: "city",
			wantRef:  "test-city",
		},
		{
			name:     "class root store ref is not a scope",
			metadata: map[string]string{"gc.root_store_ref": "class:gmnos"},
		},
		{
			name:     "empty root store ref",
			metadata: map[string]string{"gc.root_store_ref": ""},
		},
		{
			name: "no metadata at all",
		},
		{
			name:     "malformed root store ref",
			metadata: map[string]string{"gc.root_store_ref": "rig:"},
		},
		{
			name:     "kindless root store ref",
			metadata: map[string]string{"gc.root_store_ref": ":beads"},
		},
		{
			name:     "half-stamped root falls through",
			metadata: map[string]string{"gc.scope_kind": "rig"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotKind, gotRef := workflowRootScope(beads.Bead{Metadata: tc.metadata})
			if gotKind != tc.wantKind || gotRef != tc.wantRef {
				t.Fatalf("workflowRootScope = %q:%q, want %q:%q", gotKind, gotRef, tc.wantKind, tc.wantRef)
			}
		})
	}
}

// TestWorkflowScopeMatchesBindingLegRigRootedRoot covers the maintainer-city
// shape behind ga-dezas: a rig-rooted graph.v2 root served through the graph
// class binding, whose leg carries the city scope. The request scope the root
// ref names must match; the leg's scope must not.
func TestWorkflowScopeMatchesBindingLegRigRootedRoot(t *testing.T) {
	info := workflowStoreInfo{ref: "graph:test-city", scopeKind: "city", scopeRef: "test-city"}
	root := beads.Bead{Metadata: map[string]string{"gc.root_store_ref": "rig:beads"}}

	if !workflowScopeMatches(info, root, "rig", "beads") {
		t.Fatal("workflowScopeMatches(rig, beads) = false, want true for a rig-rooted root read through the binding leg")
	}
	if workflowScopeMatches(info, root, "city", "test-city") {
		t.Fatal("workflowScopeMatches(city, test-city) = true, want false: the binding leg is where the root lives, not the scope it belongs to")
	}
}

// TestWorkflowGetResolvesRigScopeFromRootStoreRefOnBindingLeg is the
// handler-level half of ga-dezas: GET /v0/city/{city}/workflow/{id} with the
// rig scope resolves a rig-rooted root that physically lives on the graph
// class binding, and reports that rig scope while root_store_ref still names
// the physical leg.
func TestWorkflowGetResolvesRigScopeFromRootStoreRefOnBindingLeg(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	state.cityBeadStore = beads.NewMemStore()
	graphStore := beads.NewMemStore()
	state.graphBeadStore = graphStore
	state.stores = map[string]beads.Store{}

	root, err := graphStore.Create(beads.Bead{
		Title: "Sling-launched workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_rig_rooted",
			"gc.root_store_ref":   "rig:beads",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_rig_rooted?scope_kind=rig&scope_ref=beads"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	if snapshot.ScopeKind != "rig" || snapshot.ScopeRef != "beads" {
		t.Fatalf("scope = %s:%s, want rig:beads", snapshot.ScopeKind, snapshot.ScopeRef)
	}
	if snapshot.RootStoreRef != "graph:test-city" {
		t.Fatalf("root_store_ref = %q, want graph:test-city (the physical leg)", snapshot.RootStoreRef)
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

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_partial?scope_kind=city&scope_ref=test-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

func TestWorkflowGetHistoricalSnapshotIncludesClosedFallbackChildren(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	memStore := beads.NewMemStore()
	state.cityBeadStore = memStore

	root, err := memStore.Create(beads.Bead{
		Title:  "Closed workflow",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_closed_history",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := memStore.Create(beads.Bead{
		Title:  "Closed step",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.closed",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_closed_history?scope_kind=city&scope_ref=test-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	if len(snapshot.Beads) != 2 {
		t.Fatalf("snapshot beads = %d, want 2", len(snapshot.Beads))
	}
	foundChild := false
	for _, bead := range snapshot.Beads {
		if bead.ID == child.ID {
			foundChild = true
			break
		}
	}
	if !foundChild {
		t.Fatalf("closed child %q missing from historical snapshot", child.ID)
	}
}

func TestWorkflowGetOpenSnapshotIncludesClosedFallbackChildren(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	memStore := beads.NewMemStore()
	state.cityBeadStore = memStore

	root, err := memStore.Create(beads.Bead{
		Title: "Open workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_open_history",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := memStore.Create(beads.Bead{
		Title:  "Completed step",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.done",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_open_history?scope_kind=city&scope_ref=test-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var snapshot workflowSnapshotResponse
	if err := json.NewDecoder(rec.Body).Decode(&snapshot); err != nil {
		t.Fatalf("Decode(snapshot): %v", err)
	}

	if len(snapshot.Beads) != 2 {
		t.Fatalf("snapshot beads = %d, want 2", len(snapshot.Beads))
	}
	foundChild := false
	for _, bead := range snapshot.Beads {
		if bead.ID == child.ID {
			foundChild = true
			break
		}
	}
	if !foundChild {
		t.Fatalf("closed child %q missing from open snapshot", child.ID)
	}
}

func TestWorkflowDeleteIncludesClosedDescendantsAndDeletesBeads(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	memStore := beads.NewMemStore()
	state.cityBeadStore = memStore

	root, err := memStore.Create(beads.Bead{
		Title:  "Closed workflow",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_delete_closed",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := memStore.Create(beads.Bead{
		Title:  "Closed step",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.closed",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodDelete, cityURL(state, "/workflow/")+root.ID+"?scope_kind=city&scope_ref=test-city&delete=true", nil)
	req.Header.Set("X-GC-Request", "test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Deleted int `json:"deleted"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(response): %v", err)
	}
	if resp.Deleted != 2 {
		t.Fatalf("deleted = %d, want 2", resp.Deleted)
	}
	if _, err := memStore.Get(root.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(root) err = %v, want ErrNotFound", err)
	}
	if _, err := memStore.Get(child.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(child) err = %v, want ErrNotFound", err)
	}
}

func TestWorkflowDeleteResolvesLogicalWorkflowID(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	memStore := beads.NewMemStore()
	state.cityBeadStore = memStore

	root, err := memStore.Create(beads.Bead{
		Title:  "Logical workflow",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf_delete_logical",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := memStore.Create(beads.Bead{
		Title:  "Logical child",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.closed",
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodDelete, cityURL(state, "/workflow/wf_delete_logical?scope_kind=city&scope_ref=test-city&delete=true"), nil)
	req.Header.Set("X-GC-Request", "test")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	if _, err := memStore.Get(root.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(root) err = %v, want ErrNotFound", err)
	}
	if _, err := memStore.Get(child.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Get(child) err = %v, want ErrNotFound", err)
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

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_missing_scope"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_city_partial?scope_kind=city&scope_ref=test-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/wf_index?scope_kind=city&scope_ref=test-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

	// A city that relocates nothing has no orders binding to name: OrdersBeadStore()
	// is the city store, and an orders: ref there would be a second name for a store
	// the city: ref already resolves.
	if _, ok := workflowStoreByRef(state, "orders:city"); ok {
		t.Fatal("workflowStoreByRef(orders:city) on a single-store city = true, want false")
	}
}

// TestWorkflowStoreByRefResolvesTheOrdersBinding covers the class ref the order
// history list mints. A ref one endpoint publishes and its sibling cannot resolve
// is a 404 on a bead that exists.
func TestWorkflowStoreByRefResolvesTheOrdersBinding(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.cityBeadStore = beads.NewMemStore()
	binding := beads.NewMemStore()
	state.ordersBeadStore = binding
	state.stores = nil
	state.cfg.Rigs = nil

	info, ok := workflowStoreByRef(state, "orders:bright-lights")
	if !ok {
		t.Fatal("workflowStoreByRef(orders:bright-lights) = false, want true")
	}
	if info.ref != "orders:bright-lights" || info.scopeKind != "city" || info.scopeRef != "bright-lights" {
		t.Fatalf("orders info = %+v, want orders:bright-lights", info)
	}
	if info.store != beads.Store(binding) {
		t.Fatal("orders store mismatch")
	}

	if _, ok := workflowStoreByRef(state, "orders:other-city"); ok {
		t.Fatal("workflowStoreByRef(orders:other-city) = true, want false")
	}
}

// TestWorkflowStorePathSkipsClassRefs pins that a class ref — one naming a
// binding rather than a scope root — yields no rig/city path to run SQL against.
// Answering the city path for the orders binding would point the workflow SQL
// fast path at the wrong database.
func TestWorkflowStorePathSkipsClassRefs(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.cityPath = t.TempDir()
	state.cityBeadStore = beads.NewMemStore()

	for _, ref := range []string{"graph:bright-lights", "orders:bright-lights"} {
		info := workflowStoreInfo{
			ref:       ref,
			scopeKind: beadmeta.ScopeKindCity,
			scopeRef:  "bright-lights",
			store:     beads.NewMemStore(),
		}
		if path, ok := workflowStorePath(state, info); ok {
			t.Fatalf("workflowStorePath(%q) = %q, true; want no path", ref, path)
		}
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
	if err := os.MkdirAll(filepath.Join(alphaPath, ".beads"), 0o700); err != nil {
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

func TestWorkflowSQLCandidatesForWorkflowIDUsesConfiguredHyphenatedPrefix(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "bright-lights"
	state.cityPath = t.TempDir()
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Rigs = []config.Rig{
		{Name: "pieces", Path: "rigs/pieces", Prefix: "pieces"},
		{Name: "pieces-annotator", Path: "rigs/pieces-annotator", Prefix: "pieces-annotator"},
	}
	state.stores = map[string]beads.Store{
		"pieces":           beads.NewMemStore(),
		"pieces-annotator": beads.NewMemStore(),
	}

	piecesPath := filepath.Join(state.cityPath, "rigs/pieces")
	if err := os.MkdirAll(filepath.Join(piecesPath, ".beads"), 0o700); err != nil {
		t.Fatalf("MkdirAll(pieces .beads): %v", err)
	}
	if err := os.WriteFile(filepath.Join(piecesPath, ".beads", "routes.jsonl"), []byte(`{"prefix":"pieces","path":"."}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pieces routes.jsonl): %v", err)
	}

	annotatorPath := filepath.Join(state.cityPath, "rigs/pieces-annotator")
	if err := os.MkdirAll(filepath.Join(annotatorPath, ".beads"), 0o700); err != nil {
		t.Fatalf("MkdirAll(pieces-annotator .beads): %v", err)
	}
	if err := os.WriteFile(filepath.Join(annotatorPath, ".beads", "routes.jsonl"), []byte(`{"prefix":"pieces-annotator","path":"."}`+"\n"), 0o644); err != nil {
		t.Fatalf("WriteFile(pieces-annotator routes.jsonl): %v", err)
	}

	candidates := workflowSQLCandidatesForWorkflowID(state, "pieces-annotator-gnpgief", "", "")
	if len(candidates) != 1 {
		t.Fatalf("len(candidates) = %d, want 1", len(candidates))
	}
	if candidates[0].info.ref != "rig:pieces-annotator" {
		t.Fatalf("candidate.ref = %q, want rig:pieces-annotator", candidates[0].info.ref)
	}
	if candidates[0].path != annotatorPath {
		t.Fatalf("candidate.path = %q, want %q", candidates[0].path, annotatorPath)
	}
}

func TestWorkflowSQLDepFromRowDefaultsMissingTypeToBlocks(t *testing.T) {
	for _, tc := range []struct {
		name    string
		depType sql.NullString
	}{
		{name: "null", depType: sql.NullString{}},
		{name: "empty", depType: sql.NullString{Valid: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dep := workflowSQLDepFromRow(
				sql.NullString{String: "child", Valid: true},
				sql.NullString{String: "parent", Valid: true},
				tc.depType,
			)
			if dep.Type != "blocks" {
				t.Fatalf("dep.Type = %q, want blocks", dep.Type)
			}
		})
	}
}

func TestWorkflowSQLDependsOnExprFromColumnsSupportsBD104AndBD105(t *testing.T) {
	tests := []struct {
		name    string
		columns map[string]bool
		want    string
	}{
		{
			name:    "bd 1.0.4 depends_on_id",
			columns: map[string]bool{"depends_on_id": true},
			want:    "COALESCE(NULLIF(d.depends_on_id, ''), '')",
		},
		{
			name: "bd 1.0.5 split target columns",
			columns: map[string]bool{
				"depends_on_issue_id": true,
				"depends_on_wisp_id":  true,
				"depends_on_external": true,
			},
			want: "COALESCE(NULLIF(d.depends_on_issue_id, ''), NULLIF(d.depends_on_wisp_id, ''), NULLIF(d.depends_on_external, ''), '')",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := workflowSQLDependsOnExprFromColumns("d", tt.columns)
			if err != nil {
				t.Fatalf("workflowSQLDependsOnExprFromColumns() error = %v", err)
			}
			if got != tt.want {
				t.Fatalf("workflowSQLDependsOnExprFromColumns() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestWorkflowSQLSnapshotScopeDefaultsToSelectedStore(t *testing.T) {
	root := beads.Bead{Metadata: map[string]string{}}
	info := workflowStoreInfo{scopeKind: "rig", scopeRef: "gascity"}

	scopeKind, scopeRef := workflowSQLSnapshotScope(root, info, "", "")
	if scopeKind != "rig" || scopeRef != "gascity" {
		t.Fatalf("scope = (%q, %q), want selected store scope (rig, gascity)", scopeKind, scopeRef)
	}

	root.Metadata["gc.scope_kind"] = "city"
	root.Metadata["gc.scope_ref"] = "maintainer-city"
	scopeKind, scopeRef = workflowSQLSnapshotScope(root, info, "rig", "fallback")
	if scopeKind != "city" || scopeRef != "maintainer-city" {
		t.Fatalf("metadata scope = (%q, %q), want metadata override (city, maintainer-city)", scopeKind, scopeRef)
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

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/")+root.ID+"?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

// oldInlineWorkflowBeadResponse reproduces the pre-refactor inline
// struct-literal construction that the three build loops used, so
// TestWorkflowBeadResponseFromBeadEquivalence can pin the new codec-backed
// mapper against it field-for-field.
func oldInlineWorkflowBeadResponse(bead beads.Bead) workflowBeadResponse {
	return workflowBeadResponse{
		ID:            bead.ID,
		Title:         bead.Title,
		Status:        workflowStatus(bead),
		Kind:          workflowKind(bead),
		StepRef:       strings.TrimSpace(bead.Metadata[beadmeta.StepRefMetadataKey]),
		Attempt:       workflowAttempt(bead),
		LogicalBeadID: strings.TrimSpace(bead.Metadata[beadmeta.LogicalBeadIDMetadataKey]),
		ScopeRef:      strings.TrimSpace(bead.Metadata[beadmeta.ScopeRefMetadataKey]),
		Assignee:      strings.TrimSpace(bead.Assignee),
		Metadata:      cloneStringMap(bead.Metadata),
	}
}

func TestWorkflowBeadResponseFromBeadEquivalence(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		bead beads.Bead
	}{
		{
			name: "fully populated with padded metadata",
			bead: beads.Bead{
				ID:       "step-1",
				Title:    "Do the thing",
				Status:   "in_progress",
				Assignee: "  worker-1  ",
				Type:     "task",
				Metadata: map[string]string{
					beadmeta.KindMetadataKey:          "  run  ",
					beadmeta.OutcomeMetadataKey:       "",
					beadmeta.AttemptMetadataKey:       "  2  ",
					beadmeta.StepRefMetadataKey:       "  iteration.1.review  ",
					beadmeta.LogicalBeadIDMetadataKey: "  logical-9  ",
					beadmeta.ScopeRefMetadataKey:      "  gascity  ",
				},
			},
		},
		{
			name: "minimal bead with nil metadata",
			bead: beads.Bead{ID: "root-2", Title: "bare"},
		},
		{
			name: "closed with fail outcome",
			bead: beads.Bead{
				ID:       "step-3",
				Title:    "failed step",
				Status:   "closed",
				Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := workflowBeadResponseFromBead(tc.bead)
			want := oldInlineWorkflowBeadResponse(tc.bead)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("workflowBeadResponseFromBead mismatch:\n got=%#v\nwant=%#v", got, want)
			}
			// Attempt pointer semantics: nil when unset, non-nil when > 0.
			if (got.Attempt == nil) != (want.Attempt == nil) {
				t.Fatalf("attempt pointer nilness mismatch: got=%v want=%v", got.Attempt, want.Attempt)
			}
			// nil source metadata must stay nil so the wire keeps "metadata": null.
			if tc.bead.Metadata == nil && got.Metadata != nil {
				t.Fatalf("nil metadata projected to non-nil: %#v", got.Metadata)
			}
		})
	}

	// Clone independence: mutating the source metadata after projection must
	// not change the response map.
	src := map[string]string{beadmeta.KindMetadataKey: "workflow"}
	resp := workflowBeadResponseFromBead(beads.Bead{ID: "root-1", Metadata: src})
	src[beadmeta.KindMetadataKey] = "mutated"
	if resp.Metadata[beadmeta.KindMetadataKey] != "workflow" {
		t.Fatalf("response metadata not independent of source: %q", resp.Metadata[beadmeta.KindMetadataKey])
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

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/")+bead.ID+"?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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

func (s failListStore) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("list failed")
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

// Intentionally NOT t.Parallel(): it redirects the global log writer, so it
// must not run concurrently with any other test that logs or rebinds output.
func TestLogWorkflowSQLFallbackSurfacesGenuineFailures(t *testing.T) {
	var buf bytes.Buffer
	origOut := log.Writer()
	origFlags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() {
		log.SetOutput(origOut)
		log.SetFlags(origFlags)
	})

	// A deployment with no SQL workflow store runs the per-store scan as its
	// steady state, not a regression — it must stay quiet so the supervisor
	// does not log on every workflow fetch (gascity#2940).
	logWorkflowSQLFallback("wf_quiet", nil)
	logWorkflowSQLFallback("wf_quiet", errNoSQLWorkflowStores)
	if buf.Len() != 0 {
		t.Fatalf("benign SQL fallbacks logged, want quiet: %q", buf.String())
	}

	// A genuine fast-path failure (Dolt unreachable, workflow absent from the
	// scope's SQL stores, …) is the perf regression operators need to see.
	logWorkflowSQLFallback("wf_loud", errors.New("dial tcp 127.0.0.1:3306: connection refused"))
	out := buf.String()
	if !strings.Contains(out, "wf_loud") || !strings.Contains(out, "connection refused") {
		t.Fatalf("genuine SQL fast-path failure not surfaced: %q", out)
	}
}

// workflowIDListSpyStore counts only the scan's gc.workflow_id metadata List
// queries (snapshotFromStore lists by gc.root_bead_id, which is not counted).
type workflowIDListSpyStore struct {
	beads.Store
	calls *int
}

func (s workflowIDListSpyStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Metadata["gc.workflow_id"] != "" {
		*s.calls++
	}
	return s.Store.List(q)
}

// Locks in the #2940 scan bound: when a point Get by physical bead id resolves
// the workflow root, the gc.workflow_id metadata List sweep is skipped entirely
// across all stores (the pre-bound single pass ran it in every store).
func TestWorkflowGetSkipsMetadataListSweepWhenIDResolvesDirectly(t *testing.T) {
	state := newFakeState(t)
	state.cityName = "test-city"
	state.cityBeadStore = beads.NewMemStore()
	rigMem := beads.NewMemStore()
	var listCalls int
	state.stores = map[string]beads.Store{
		"alpha": workflowIDListSpyStore{Store: rigMem, calls: &listCalls},
	}

	root, err := rigMem.Create(beads.Bead{
		Title: "Rig workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}

	h := newTestCityHandler(t, state)
	// Request by the physical bead id so Phase-1 Get resolves it directly.
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/workflow/"+root.ID+"?scope_kind=rig&scope_ref=alpha"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

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
	if listCalls != 0 {
		t.Fatalf("gc.workflow_id metadata List ran %d time(s); want 0 once Get resolved the id directly", listCalls)
	}
}
