package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestFormulaListReturnsCatalogSummaries(t *testing.T) {
	state := newFakeState(t)
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

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/formulas?scope_kind=city&scope_ref=test-city&target=worker", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

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

func TestFormulaListIncludesWorkflowRunCountsAndRecentRuns(t *testing.T) {
	state := newFakeState(t)
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

	activeRoot, err := store.Create(beads.Bead{
		Title: "Adopt PR",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-active",
			"gc.run_target":       "myrig/claude",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "myrig",
		},
	})
	if err != nil {
		t.Fatalf("create active workflow: %v", err)
	}
	statusInProgress := "in_progress"
	if err := store.Update(activeRoot.ID, beads.UpdateOpts{Status: &statusInProgress}); err != nil {
		t.Fatalf("set active workflow status: %v", err)
	}

	doneRoot, err := store.Create(beads.Bead{
		Title: "Adopt PR Earlier",
		Ref:   "mol-adopt-pr-v2",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.workflow_id":      "wf-done",
			"gc.run_target":       "mayor",
			"gc.scope_kind":       "city",
			"gc.scope_ref":        "test-city",
		},
	})
	if err != nil {
		t.Fatalf("create completed workflow: %v", err)
	}
	if err := store.SetMetadata(doneRoot.ID, "gc.outcome", "pass"); err != nil {
		t.Fatalf("set outcome: %v", err)
	}
	if err := store.Close(doneRoot.ID); err != nil {
		t.Fatalf("close completed workflow: %v", err)
	}

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/formulas?scope_kind=city&scope_ref=test-city&target=worker", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

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
	if item.RunCount != 1 {
		t.Fatalf("run_count = %d, want 1 city-scoped run", item.RunCount)
	}
	if len(item.RecentRuns) != 1 {
		t.Fatalf("recent_runs = %+v, want 1 city-scoped run", item.RecentRuns)
	}
	if item.RecentRuns[0].WorkflowID != "wf-done" || item.RecentRuns[0].Status != "done" {
		t.Fatalf("recent_runs[0] = %+v, want city workflow only", item.RecentRuns[0])
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

func TestFormulaListIgnoresUnrelatedStoreListFailures(t *testing.T) {
	state := newFakeState(t)
	formulaDir := t.TempDir()
	state.cfg.FormulaLayers.City = []string{formulaDir}
	state.stores["alpha"] = failListStore{Store: beads.NewMemStore()}
	state.cityBeadStore = beads.NewMemStore()

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

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/formulas?scope_kind=city&scope_ref=test-city&target=worker", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200: %s", rec.Code, rec.Body.String())
	}

	var resp struct {
		Items         []formulaSummaryResponse `json:"items"`
		Partial       bool                     `json:"partial"`
		PartialErrors []string                 `json:"partial_errors"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("Decode(catalog): %v", err)
	}
	if len(resp.Items) != 1 {
		t.Fatalf("items = %+v, want 1 entry", resp.Items)
	}
	if resp.Items[0].RunCount != 1 {
		t.Fatalf("run_count = %d, want 1", resp.Items[0].RunCount)
	}
	if !resp.Partial {
		t.Fatalf("partial = false, want true")
	}
	if len(resp.PartialErrors) != 1 || resp.PartialErrors[0] != "rig:alpha store unavailable" {
		t.Fatalf("partial_errors = %v, want rig:alpha store unavailable", resp.PartialErrors)
	}
}

func TestFormulaListCityScopeExcludesRigRunsWithoutProvenance(t *testing.T) {
	state := newFakeState(t)
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

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/formulas?scope_kind=city&scope_ref=test-city&target=worker", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

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
	if resp.Items[0].RunCount != 0 {
		t.Fatalf("run_count = %d, want 0 until workflow provenance exists", resp.Items[0].RunCount)
	}
	if len(resp.Items[0].RecentRuns) != 0 {
		t.Fatalf("recent_runs = %+v, want none for city catalog without provenance", resp.Items[0].RecentRuns)
	}
}

func TestFormulaDetailReturnsCompiledPreview(t *testing.T) {
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

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/formulas/mol-preview?scope_kind=city&scope_ref=test-city&target=worker&var.issue=BD-123", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

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
	if detail.Steps[0]["title"] != "Prep BD-123" {
		t.Fatalf("step[0].title = %v, want substituted title", detail.Steps[0]["title"])
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

	server := New(state)
	req := httptest.NewRequest(http.MethodGet, "/v0/formulas/mol-preview?scope_kind=city&scope_ref=test-city", nil)
	rec := httptest.NewRecorder()
	server.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400: %s", rec.Code, rec.Body.String())
	}
}

func writeTestFormula(t *testing.T, dir, name, body string) {
	t.Helper()
	path := filepath.Join(dir, name+".formula.toml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("WriteFile(%s): %v", path, err)
	}
}
