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
	"github.com/gastownhall/gascity/internal/formulatest"
)

func TestFormulaListReturnsCatalogSummaries(t *testing.T) {
	state := newFakeState(t)
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
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(catalog): %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %+v, want 1 entry", resp.Items)
	}
	item := resp.Items[0]
	if item.Name != "mol-adopt-pr-v2" {
		t.Fatalf("name = %q, want mol-adopt-pr-v2", item.Name)
	}
	if item.Description != "Review and fix a PR with a retry loop." {
		t.Fatalf("description = %q", item.Description)
	}
	if item.Version != "2" {
		t.Fatalf("version = %q, want 2", item.Version)
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
			"gc.run_target":       "mayor",
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
			"gc.run_target":       "myrig/claude",
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
			"gc.run_target":       "myrig/claude",
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
				"gc.run_target":       "mayor",
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
			"gc.run_target":       "mayor",
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
			"gc.run_target":       "mayor",
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
contract = "graph.v2"

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
metadata = { "gc.kind" = "run", "gc.scope_ref" = "body" }
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
	if detail.Preview.Nodes[1].Kind != "run" || detail.Preview.Nodes[1].ScopeRef != "body" {
		t.Fatalf("preview node = %+v, want run node with scope_ref", detail.Preview.Nodes[1])
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
