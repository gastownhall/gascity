package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formulatest"
)

func TestFormulaListReturnsCatalogSummaries(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "mol-adopt-pr-v2", `
description = "Review and fix a PR with a retry loop."
formula = "mol-adopt-pr-v2"
version = 2

[vars]
[vars.pr_url]
description = "Pull request URL to adopt."
required = true

[[steps]]
id = "review"
title = "Review PR"
`)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas?scope_kind=city&scope_ref=test-city&target=worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Items []formulaSummaryResponse `json:"items"`
		Total int                      `json:"total"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(catalog): %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %+v, want 1 entry", resp.Items)
	}
	if resp.Total != len(resp.Items) {
		t.Fatalf("total = %d, want len(items)=%d", resp.Total, len(resp.Items))
	}
	item := resp.Items[0]
	if item.Name != "mol-adopt-pr-v2" {
		t.Fatalf("name = %q, want mol-adopt-pr-v2", item.Name)
	}
	if item.Description != "Review and fix a PR with a retry loop." {
		t.Fatalf("description = %q", item.Description)
	}
	if len(item.VarDefs) != 1 || item.VarDefs[0].Name != "pr_url" || !item.VarDefs[0].Required {
		t.Fatalf("var_defs = %+v, want required pr_url", item.VarDefs)
	}
	if item.RunCount != 0 || len(item.RecentRuns) != 0 {
		t.Fatalf("run data = count %d runs %+v, want no runs for empty store", item.RunCount, item.RecentRuns)
	}
}

func TestFormulaListSkipsWorkflowHistoryQueries(t *testing.T) {
	state := newFakeState(t)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	state.stores["alpha"] = failListStore{Store: beads.NewMemStore()}
	state.cityBeadStore = failListStore{Store: beads.NewMemStore()}

	writeTestFormula(t, formulaDir, "mol-adopt-pr-v2", `
description = "Review and fix a PR with a retry loop."
formula = "mol-adopt-pr-v2"
version = 2

[[steps]]
id = "review"
title = "Review PR"
`)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas?scope_kind=city&scope_ref=test-city&target=worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Items []formulaSummaryResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(catalog): %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %+v, want 1 entry", resp.Items)
	}
	if resp.Items[0].RunCount != 0 || len(resp.Items[0].RecentRuns) != 0 {
		t.Fatalf("run data = %+v, want empty workflow summaries from cheap catalog", resp.Items[0])
	}
}

func TestFormulaRecentRunsForSortsByUpdatedAtDescending(t *testing.T) {
	runs := []workflowRunProjection{
		{
			WorkflowID:  "wf-active",
			FormulaName: "mol-adopt-pr-v2",
			Status:      "pending",
			Target:      "myrig/claude",
			StartedAt:   time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC),
			UpdatedAt:   time.Date(2026, 3, 26, 10, 0, 0, 0, time.UTC),
		},
		{
			WorkflowID:  "wf-done",
			FormulaName: "mol-adopt-pr-v2",
			Status:      "done",
			Target:      "mayor",
			StartedAt:   time.Date(2026, 3, 26, 9, 0, 0, 0, time.UTC),
			UpdatedAt:   time.Date(2026, 3, 26, 11, 0, 0, 0, time.UTC),
		},
	}

	recent := formulaRecentRunsFor("mol-adopt-pr-v2", runs, 2)
	if len(recent) != 2 {
		t.Fatalf("len(recent) = %d, want 2", len(recent))
	}
	if recent[0].WorkflowID != "wf-done" {
		t.Fatalf("recent[0] = %+v, want wf-done first", recent[0])
	}
	if recent[1].WorkflowID != "wf-active" {
		t.Fatalf("recent[1] = %+v, want wf-active second", recent[1])
	}
}

func TestFormulaRunsIncludesWorkflowRunCountsAndRecentRuns(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "mol-adopt-pr-v2", `
description = "Review and fix a PR with a retry loop."
formula = "mol-adopt-pr-v2"
version = 2

[[steps]]
id = "review"
title = "Review PR"
`)

	store := state.cityBeadStore
	if store == nil {
		t.Fatal("expected city store")
	}

	root, err := store.Create(beads.Bead{
		Title: "Adopt PR",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-healthy",
			"gc.routed_to":        "mayor",
			"gc.scope_kind":       "city",
			"gc.scope_ref":        "test-city",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(root.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set workflow status: %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/mol-adopt-pr-v2/runs?scope_kind=city&scope_ref=test-city&limit=2"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp formulaRunsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(runs): %v", err)
	}
	if resp.Formula != "mol-adopt-pr-v2" {
		t.Fatalf("formula = %q, want mol-adopt-pr-v2", resp.Formula)
	}
	if resp.RunCount != 1 {
		t.Fatalf("run_count = %d, want 1", resp.RunCount)
	}
	if len(resp.RecentRuns) != 1 {
		t.Fatalf("recent_runs = %+v, want 1 city-scoped run", resp.RecentRuns)
	}
	if resp.RecentRuns[0].WorkflowID != "wf-healthy" || resp.RecentRuns[0].Status != "pending" {
		t.Fatalf("recent_runs[0] = %+v, want wf-healthy pending", resp.RecentRuns[0])
	}
}

func TestFormulaRunsCityScopeExcludesRigRunsWithoutProvenance(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "mol-adopt-pr-v2", `
description = "Review and fix a PR with a retry loop."
formula = "mol-adopt-pr-v2"
version = 2

[[steps]]
id = "review"
title = "Review PR"
`)

	store := state.stores["myrig"]
	if store == nil {
		t.Fatal("expected rig store")
	}

	root, err := store.Create(beads.Bead{
		Title: "Rig override run",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-rig-only",
			"gc.routed_to":        "myrig/claude",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
		},
	})
	if err != nil {
		t.Fatalf("create rig workflow: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(root.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set workflow status: %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/mol-adopt-pr-v2/runs?scope_kind=city&scope_ref=test-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp formulaRunsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(runs): %v", err)
	}
	if resp.RunCount != 0 {
		t.Fatalf("run_count = %d, want 0 until workflow provenance exists", resp.RunCount)
	}
	if len(resp.RecentRuns) != 0 {
		t.Fatalf("recent_runs = %+v, want none for city formula runs without provenance", resp.RecentRuns)
	}
}

func TestFormulaRunsReturnsNotFoundForMissingRigScope(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "mol-adopt-pr-v2", `
description = "Review and fix a PR with a retry loop."
formula = "mol-adopt-pr-v2"
version = 2

[[steps]]
id = "review"
title = "Review PR"
`)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/mol-adopt-pr-v2/runs?scope_kind=rig&scope_ref=missing"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404: %s", rec.Code, rec.Body.String())
	}
}

func TestFormulaFeedReturnsWorkflowRunsOnly(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	store := state.stores["myrig"]
	if store == nil {
		t.Fatal("expected rig store")
	}

	root, err := store.Create(beads.Bead{
		Title: "Rig workflow run",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-rig-monitor",
			"gc.routed_to":        "myrig/claude",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
		},
	})
	if err != nil {
		t.Fatalf("create rig workflow root: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(root.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set workflow status: %v", err)
	}

	if _, err := state.cityBeadStore.Create(beads.Bead{
		Title:  "order:spawn-storm-detect",
		Labels: []string{"order-tracking"},
	}); err != nil {
		t.Fatalf("create order tracking bead: %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/feed?scope_kind=city&scope_ref=test-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Items []monitorFeedItemResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(feed): %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %+v, want exactly 1 workflow item", resp.Items)
	}
	if resp.Items[0].WorkflowID != "wf-rig-monitor" || resp.Items[0].Type != "formula" {
		t.Fatalf("items[0] = %+v, want rig workflow formula item", resp.Items[0])
	}
}

func TestFormulaRunsClampsRequestedLimit(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "mol-adopt-pr-v2", `
description = "Review and fix a PR with a retry loop."
formula = "mol-adopt-pr-v2"
version = 2

[[steps]]
id = "review"
title = "Review PR"
`)

	for i := 0; i < maxFormulaRunsLimit+5; i++ {
		root, err := state.cityBeadStore.Create(beads.Bead{
			Title: "Workflow root",
			Ref:   "mol-adopt-pr-v2",
			Metadata: map[string]string{
				"gc.kind":             "workflow",
				"gc.formula_contract": "graph.v2",
				"gc.workflow_id":      fmt.Sprintf("wf-%02d", i),
				"gc.routed_to":        "mayor",
				"gc.scope_kind":       "city",
				"gc.scope_ref":        "test-city",
			},
		})
		if err != nil {
			t.Fatalf("create workflow root %d: %v", i, err)
		}
		inProgress := "in_progress"
		if err := state.cityBeadStore.Update(root.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
			t.Fatalf("set workflow status %d: %v", i, err)
		}
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/mol-adopt-pr-v2/runs?scope_kind=city&scope_ref=test-city&limit=9999"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp formulaRunsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(runs): %v", err)
	}
	if len(resp.RecentRuns) != maxFormulaRunsLimit {
		t.Fatalf("recent_runs len = %d, want %d", len(resp.RecentRuns), maxFormulaRunsLimit)
	}
}

func TestFormulaRunsFallsBackToOpenWorkflowRootsWhenHistoryLookupFails(t *testing.T) {
	state := newFakeState(t)
	baseStore := beads.NewMemStore()
	state.cityBeadStore = failWorkflowRootLookupStore{Store: baseStore}
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "mol-adopt-pr-v2", `
description = "Review and fix a PR with a retry loop."
formula = "mol-adopt-pr-v2"
version = 2

[[steps]]
id = "review"
title = "Review PR"
`)

	root, err := baseStore.Create(beads.Bead{
		Title: "Open workflow root",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-open-root",
			"gc.routed_to":        "mayor",
			"gc.scope_kind":       "city",
			"gc.scope_ref":        "test-city",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	inProgress := "in_progress"
	if err := baseStore.Update(root.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set workflow status: %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/mol-adopt-pr-v2/runs?scope_kind=city&scope_ref=test-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp formulaRunsResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(runs): %v", err)
	}
	if resp.RunCount != 1 {
		t.Fatalf("run_count = %d, want 1", resp.RunCount)
	}
	if !resp.Partial {
		t.Fatalf("partial = false, want true")
	}
	if len(resp.PartialErrors) == 0 {
		t.Fatalf("partial_errors = %+v, want fallback warning", resp.PartialErrors)
	}
}

func TestFormulaFeedUsesRootOnlyProjectionWithoutChildLookup(t *testing.T) {
	state := newFakeState(t)
	baseStore := beads.NewMemStore()
	// failPerRootChildLookupStore fails on per-root child List calls
	// (queries with gc.root_bead_id metadata).  The feed endpoint uses
	// buildWorkflowRunProjectionsRootOnly which never issues those
	// queries, so this test verifies the fast path is in use.
	state.cityBeadStore = failPerRootChildLookupStore{Store: baseStore}

	root, err := baseStore.Create(beads.Bead{
		Title: "Open workflow root",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-fast-path",
			"gc.routed_to":        "mayor",
			"gc.scope_kind":       "city",
			"gc.scope_ref":        "test-city",
		},
	})
	if err != nil {
		t.Fatalf("create workflow root: %v", err)
	}
	inProgress := "in_progress"
	if err := baseStore.Update(root.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("set workflow status: %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/feed?scope_kind=city&scope_ref=test-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Items []monitorFeedItemResponse `json:"items"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(feed): %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %+v, want 1 fast-path item", resp.Items)
	}
	if resp.Items[0].WorkflowID != "wf-fast-path" {
		t.Fatalf("items[0] = %+v, want wf-fast-path", resp.Items[0])
	}
}

type failWorkflowRootLookupStore struct {
	beads.Store
}

func (s failWorkflowRootLookupStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Metadata["gc.kind"] == "workflow" && query.Metadata["gc.formula_contract"] == "graph.v2" {
		return nil, errors.New("workflow root lookup failed")
	}
	return s.Store.List(query)
}

type failPerRootChildLookupStore struct {
	beads.Store
}

func (s failPerRootChildLookupStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if query.Metadata["gc.root_bead_id"] != "" {
		return nil, errors.New("per-root child lookup should not be called on the fast path")
	}
	return s.Store.List(query)
}

func TestFormulaPreviewAcceptsTypedVarsBody(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	// api.New(state) calls syncFeatureFlags(state.Config()), which pulls
	// formula_v2 back out of cfg.Daemon and overrides the global set above.
	// Without this, the server sees v2 as disabled and rejects the v2
	// formula compile with a 400 "formula_v2 is disabled" error.
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "mol-preview", `
description = "Preview {{issue}}"
formula = "mol-preview"
version = 2

[vars]
[vars.issue]
description = "Issue bead ID"
required = true

[[steps]]
id = "prep"
title = "Prep {{issue}}"

[[steps]]
id = "review"
title = "Review {{issue}}"
needs = ["prep"]
`)

	body := bytes.NewBufferString(`{"scope_kind":"city","scope_ref":"test-city","target":"worker","vars":{"issue":"BD-123"}}`)
	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodPost, cityURL(state, "/formulas/mol-preview/preview"), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var detail formulaDetailResponse
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatalf("Decode(detail): %v", err)
	}
	if detail.Name != "mol-preview" {
		t.Fatalf("name = %q, want mol-preview", detail.Name)
	}
	if detail.Description != "Preview BD-123" {
		t.Fatalf("description = %q, want substituted preview", detail.Description)
	}
	if len(detail.Steps) != 2 {
		t.Fatalf("steps = %+v, want 2 non-root steps", detail.Steps)
	}
	if detail.Steps[0].Title != "Prep BD-123" {
		t.Fatalf("step[0].title = %v, want substituted title", detail.Steps[0].Title)
	}
	if len(detail.Deps) != 1 || detail.Deps[0].From != "mol-preview.prep" || detail.Deps[0].To != "mol-preview.review" {
		t.Fatalf("deps = %+v, want prep -> review", detail.Deps)
	}
	if len(detail.Preview.Nodes) != 2 {
		t.Fatalf("preview.nodes = %+v, want 2 nodes", detail.Preview.Nodes)
	}
}

func TestFormulaPreviewGraphV2InjectsTargetConvoy(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "graph-preview", `
description = "Preview {{convoy_id}}"
formula = "graph-preview"
version = 2
contract = "graph.v2"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	convoy, err := state.cityBeadStore.Create(beads.Bead{Title: "input", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create(convoy): %v", err)
	}

	body := bytes.NewBufferString(fmt.Sprintf(`{"scope_kind":"city","scope_ref":"test-city","target":%q}`, convoy.ID))
	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodPost, cityURL(state, "/formulas/graph-preview/preview"), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var detail formulaDetailResponse
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatalf("Decode(detail): %v", err)
	}
	if detail.Description != "Preview "+convoy.ID {
		t.Fatalf("description = %q, want injected convoy", detail.Description)
	}
	if len(detail.Steps) != 1 || detail.Steps[0].Title != "Inspect "+convoy.ID {
		t.Fatalf("steps = %+v, want substituted graph.v2 preview step", detail.Steps)
	}
}

func TestFormulaPreviewGraphV2UsesPreviewInputConvoyForBeadTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "graph-preview", `
description = "Preview {{convoy_id}}"
formula = "graph-preview"
version = 2
contract = "graph.v2"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	target, err := state.cityBeadStore.Create(beads.Bead{Title: "input", Type: "task"})
	if err != nil {
		t.Fatalf("Create(target): %v", err)
	}

	body := bytes.NewBufferString(fmt.Sprintf(`{"scope_kind":"city","scope_ref":"test-city","target":%q}`, target.ID))
	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodPost, cityURL(state, "/formulas/graph-preview/preview"), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var detail formulaDetailResponse
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatalf("Decode(detail): %v", err)
	}
	want := "preview-input-convoy:" + target.ID
	if detail.Description != "Preview "+want {
		t.Fatalf("description = %q, want preview input convoy", detail.Description)
	}
	if len(detail.Steps) != 1 || detail.Steps[0].Title != "Inspect "+want {
		t.Fatalf("steps = %+v, want preview input graph.v2 step", detail.Steps)
	}
	matches, err := state.cityBeadStore.List(beads.ListQuery{Type: "convoy"})
	if err != nil {
		t.Fatalf("List input convoys: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("preview persisted input convoys = %+v, want none", matches)
	}
}

func TestFormulaPreviewRequiresGraphCompilerInjectsTargetConvoy(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "graph-preview", `
description = "Preview {{convoy_id}}"
formula = "graph-preview"
version = 2

[requires]
formula_compiler = ">=2.0.0"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`)
	convoy, err := state.cityBeadStore.Create(beads.Bead{Title: "input", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create(convoy): %v", err)
	}

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/graph-preview?scope_kind=city&scope_ref=test-city&target="+convoy.ID), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var detail formulaDetailResponse
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatalf("Decode(GET detail): %v", err)
	}
	if detail.Description != "Preview "+convoy.ID {
		t.Fatalf("GET description = %q, want injected convoy", detail.Description)
	}
	if len(detail.Steps) != 1 || detail.Steps[0].Title != "Inspect "+convoy.ID {
		t.Fatalf("GET steps = %+v, want substituted graph.v2 detail step", detail.Steps)
	}

	body := bytes.NewBufferString(fmt.Sprintf(`{"scope_kind":"city","scope_ref":"test-city","target":%q}`, convoy.ID))
	req = httptest.NewRequest(http.MethodPost, cityURL(state, "/formulas/graph-preview/preview"), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST preview status = %d, want 200: %s", rec.Code, rec.Body.String())
	}
	detail = formulaDetailResponse{}
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatalf("Decode(POST detail): %v", err)
	}
	if detail.Description != "Preview "+convoy.ID {
		t.Fatalf("POST description = %q, want injected convoy", detail.Description)
	}
	if len(detail.Steps) != 1 || detail.Steps[0].Title != "Inspect "+convoy.ID {
		t.Fatalf("POST steps = %+v, want substituted graph.v2 preview step", detail.Steps)
	}
}

func TestFormulaDetailGraphV2TargetlessAcceptsAgentTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "graph-detail", `
description = "Targetless detail"
formula = "graph-detail"
version = 2
contract = "graph.v2"

[[steps]]
id = "inspect"
title = "Inspect"
`)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/graph-detail?scope_kind=city&scope_ref=test-city&target=worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200 for agent target on targetless graph.v2 formula: %s", rec.Code, rec.Body.String())
	}
	var detail formulaDetailResponse
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatalf("Decode(detail): %v", err)
	}
	if detail.Description != "Targetless detail" {
		t.Fatalf("description = %q, want targetless detail", detail.Description)
	}
	if len(detail.Steps) != 1 || detail.Steps[0].Title != "Inspect" {
		t.Fatalf("steps = %+v, want unsubstituted graph.v2 detail step", detail.Steps)
	}
}

// graphAgentTargetFormulaTOML is a graph.v2 formula that references the
// input convoy, so the preview compilation requires a target.
const graphAgentTargetFormulaTOML = `
description = "Preview {{convoy_id}}"
formula = "graph-agent-target"
version = 2
contract = "graph.v2"

[[steps]]
id = "inspect"
title = "Inspect {{convoy_id}}"
`

// Workflow roots persist the routed agent identity as gc.routed_to
// (ga-eld2x / #2763); run-detail clients echo that identity back as the
// preview target. A configured agent identity has no bead-store entry, so
// the detail endpoint must resolve it against config instead of failing the
// graph.v2 bead lookup (dashboard audit finding M3 follow-up).
func TestFormulaDetailGraphV2AcceptsConfiguredAgentTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "graph-agent-target", graphAgentTargetFormulaTOML)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/graph-agent-target?scope_kind=city&scope_ref=test-city&target=myrig/worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200 for configured agent target: %s", rec.Code, rec.Body.String())
	}
	var detail formulaDetailResponse
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatalf("Decode(detail): %v", err)
	}
	want := "preview-input-convoy:myrig/worker"
	if detail.Description != "Preview "+want {
		t.Fatalf("description = %q, want routing-identity preview input convoy", detail.Description)
	}
	if len(detail.Steps) != 1 || detail.Steps[0].Title != "Inspect "+want {
		t.Fatalf("steps = %+v, want routing-identity graph.v2 detail step", detail.Steps)
	}
	matches, err := state.cityBeadStore.List(beads.ListQuery{Type: "convoy"})
	if err != nil {
		t.Fatalf("List input convoys: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("preview persisted input convoys = %+v, want none", matches)
	}
}

func TestFormulaPreviewGraphV2AcceptsConfiguredAgentTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "graph-agent-target", graphAgentTargetFormulaTOML)

	body := bytes.NewBufferString(`{"scope_kind":"city","scope_ref":"test-city","target":"myrig/worker"}`)
	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodPost, cityURL(state, "/formulas/graph-agent-target/preview"), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("POST preview status = %d, want 200 for configured agent target: %s", rec.Code, rec.Body.String())
	}
	var detail formulaDetailResponse
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatalf("Decode(detail): %v", err)
	}
	want := "preview-input-convoy:myrig/worker"
	if detail.Description != "Preview "+want {
		t.Fatalf("description = %q, want routing-identity preview input convoy", detail.Description)
	}
	if len(detail.Steps) != 1 || detail.Steps[0].Title != "Inspect "+want {
		t.Fatalf("steps = %+v, want routing-identity graph.v2 preview step", detail.Steps)
	}
}

// The live failure that motivated routing-identity acceptance used a V2
// binding-qualified identity (dir/binding.name) under rig scope, while the
// fixture agent above exercises only the V1 dir/name fallback branch of
// AgentMatchesIdentity. Pin the binding-qualified shape at endpoint level so
// a change to the V2 matching branch cannot silently drop it.
func TestFormulaDetailGraphV2AcceptsBindingQualifiedAgentTarget(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Daemon.FormulaV2 = true
	state.cfg.Agents = append(state.cfg.Agents, config.Agent{
		Name:              "operator",
		Dir:               "myrig",
		BindingName:       "mypack",
		Provider:          "test-agent",
		MaxActiveSessions: intPtr(1),
	})
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "graph-agent-target", graphAgentTargetFormulaTOML)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/graph-agent-target?scope_kind=rig&scope_ref=myrig&target=myrig/mypack.operator"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200 for binding-qualified agent target: %s", rec.Code, rec.Body.String())
	}
	var detail formulaDetailResponse
	if err := json.NewDecoder(rec.Body).Decode(&detail); err != nil {
		t.Fatalf("Decode(detail): %v", err)
	}
	want := "preview-input-convoy:myrig/mypack.operator"
	if detail.Description != "Preview "+want {
		t.Fatalf("description = %q, want binding-qualified routing-identity preview input convoy", detail.Description)
	}
	if len(detail.Steps) != 1 || detail.Steps[0].Title != "Inspect "+want {
		t.Fatalf("steps = %+v, want binding-qualified routing-identity graph.v2 detail step", detail.Steps)
	}
	matches, err := state.stores["myrig"].List(beads.ListQuery{Type: "convoy"})
	if err != nil {
		t.Fatalf("List input convoys: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("preview persisted input convoys = %+v, want none", matches)
	}
}

// The routing-identity lookup must not run before the endpoint's existing
// config-availability guard: with a nil city config the detail and preview
// endpoints keep returning the typed 503 instead of panicking on a nil
// config dereference.
func TestFormulaDetailNilConfigReturns503(t *testing.T) {
	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg = nil

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/graph-agent-target?scope_kind=city&scope_ref=test-city&target=myrig/worker"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("GET detail status = %d, want 503 when config is unavailable: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "config is unavailable") {
		t.Fatalf("body = %s, want config-unavailable error", rec.Body.String())
	}
}

// A target that is neither a bead nor a configured agent identity must keep
// failing with the existing not-found error: routing-identity acceptance is
// config-resolved, not a blanket fallback that would mask mistyped bead IDs.
// The error must also say that config-identity resolution was attempted, so
// a stale or mistyped agent identity is not misread as a bead-store problem.
func TestFormulaDetailGraphV2UnknownTargetStillRejected(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "graph-agent-target", graphAgentTargetFormulaTOML)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/graph-agent-target?scope_kind=city&scope_ref=test-city&target=ga-nope"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("GET detail status = %d, want 400 for unknown target: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "not found") {
		t.Fatalf("body = %s, want graph.v2 target not-found error", rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "matches neither a bead/convoy nor a configured agent identity") {
		t.Fatalf("body = %s, want agent-identity resolution context in not-found error", rec.Body.String())
	}
}

func TestFormulaPreviewRejectsMissingRequiredVars(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "mol-preview", `
description = "Preview {{issue}}"
formula = "mol-preview"
version = 2

[vars]
[vars.issue]
description = "Issue bead ID"
required = true

[[steps]]
id = "prep"
title = "Prep {{issue}}"
`)

	body := bytes.NewBufferString(`{"scope_kind":"city","scope_ref":"test-city","target":"worker","vars":{"other":"BD-123"}}`)
	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodPost, cityURL(state, "/formulas/mol-preview/preview"), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&problem); err != nil {
		t.Fatalf("Decode(problem): %v", err)
	}
	if !strings.Contains(problem.Detail, `variable "issue" is required`) {
		t.Fatalf("detail = %q, want missing issue validation error", problem.Detail)
	}
}

func TestFormulaPreviewRejectsMissingRequiredVarsWithoutVarsBody(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "mol-preview", `
description = "Preview {{issue}}"
formula = "mol-preview"
version = 2

[vars]
[vars.issue]
description = "Issue bead ID"
required = true

[[steps]]
id = "prep"
title = "Prep {{issue}}"
`)

	body := bytes.NewBufferString(`{"scope_kind":"city","scope_ref":"test-city","target":"worker"}`)
	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodPost, cityURL(state, "/formulas/mol-preview/preview"), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
	var problem struct {
		Detail string `json:"detail"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&problem); err != nil {
		t.Fatalf("Decode(problem): %v", err)
	}
	if !strings.Contains(problem.Detail, `variable "issue" is required`) {
		t.Fatalf("detail = %q, want missing issue validation error", problem.Detail)
	}
}

func TestFormulaDetailGraphV2DrainSkipsItemRuntimeValidation(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	state.cityBeadStore = beads.NewMemStore()
	state.cfg.Daemon.FormulaV2 = true
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	target, err := state.cityBeadStore.Create(beads.Bead{Title: "target", Type: "task"})
	if err != nil {
		t.Fatalf("Create(target): %v", err)
	}

	writeTestFormula(t, formulaDir, "parent-drain", `
formula = "parent-drain"
version = 2
contract = "graph.v2"

[[steps]]
id = "drain"
title = "Drain {{convoy_id}}"

[steps.drain]
context = "separate"
formula = "item-work"
`)
	writeTestFormula(t, formulaDir, "item-work", `
formula = "item-work"
version = 2
contract = "graph.v2"

[vars]
[vars.extra]
description = "Required only when validating a runnable preview"
required = true

[[steps]]
id = "work"
title = "Work {{convoy_id}} with {{extra}}"
`)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/parent-drain?scope_kind=city&scope_ref=test-city&target="+target.ID), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("GET detail status = %d, want 200 without item runtime validation: %s", rec.Code, rec.Body.String())
	}

	body := bytes.NewBufferString(fmt.Sprintf(`{"scope_kind":"city","scope_ref":"test-city","target":%q}`, target.ID))
	req = httptest.NewRequest(http.MethodPost, cityURL(state, "/formulas/parent-drain/preview"), body)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-GC-Request", "true")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("POST preview status = %d, want 400 with item runtime validation: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "extra") {
		t.Fatalf("POST preview body = %s, want missing extra", rec.Body.String())
	}
}

// TestFormulaDetailRejectsLegacyVarQueryParams pins the §3.5.1 migration
// behavior: undeclared var.* query parameters on the GET detail endpoint
// are now rejected with a 4xx + migration hint pointing at POST /preview.
// Silent-ignore was worse than either accept-or-reject because bookmarked
// curl scripts rendered the default-substituted preview the user thought
// was customized.
func TestFormulaDetailRejectsLegacyVarQueryParams(t *testing.T) {
	formulatest.EnableV2ForTest(t)

	state := newFakeState(t)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "mol-preview", `
description = "Preview {{issue}}"
formula = "mol-preview"
version = 2

[vars]
[vars.issue]
description = "Issue bead ID"
default = "DEFAULT"

[[steps]]
id = "prep"
title = "Prep {{issue}}"
`)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/mol-preview?scope_kind=city&scope_ref=test-city&target=worker&var.issue=BD-123"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	if rec.Code < 400 || rec.Code >= 500 {
		t.Fatalf("status = %d, want 4xx rejecting var.* params: %s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "var.") {
		t.Fatalf("response body does not mention var.* migration: %s", rec.Body.String())
	}
}

func TestFormulaDetailRequiresTarget(t *testing.T) {
	state := newFakeState(t)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}

	writeTestFormula(t, formulaDir, "mol-preview", `
description = "Preview"
formula = "mol-preview"
version = 2

[[steps]]
id = "prep"
title = "Prep"
`)

	h := newTestCityHandler(t, state)
	req := httptest.NewRequest(http.MethodGet, cityURL(state, "/formulas/mol-preview?scope_kind=city&scope_ref=test-city"), nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	// target is declared required:"true" on FormulaDetailInput, so Huma
	// fails validation with 422 before the handler runs.
	if rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422: %s", rec.Code, rec.Body.String())
	}
}

func writeTestFormula(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name+".toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
