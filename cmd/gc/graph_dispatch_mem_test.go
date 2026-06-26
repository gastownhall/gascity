package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/graphroute"
	"github.com/gastownhall/gascity/internal/runtime"
)

func builtinFormulaDir(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	// Built-in formulas now live in the core bootstrap pack. cwd is cmd/gc,
	// so walk up to the repo root and into the core pack's formulas dir.
	return filepath.Join(cwd, "..", "..", "internal", "bootstrap", "packs", "core", "formulas")
}

func buildMemGraphWorkflowConfig(t *testing.T) *config.City {
	t.Helper()
	cfg := &config.City{
		Daemon: config.DaemonConfig{
			FormulaV2: boolPtr(true),
		},
		Workspace: config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{
			City: []string{builtinFormulaDir(t)},
		},
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1)},
		},
	}
	addTestControlDispatcherAgents(cfg, "")
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	return cfg
}

func addTestControlDispatcherAgents(cfg *config.City, dirs ...string) {
	if cfg == nil {
		return
	}
	for _, dir := range dirs {
		if hasTestControlDispatcherAgent(cfg, dir) {
			continue
		}
		cfg.Agents = append(cfg.Agents, testControlDispatcherAgent(dir))
	}
}

func hasTestControlDispatcherAgent(cfg *config.City, dir string) bool {
	for _, agent := range cfg.Agents {
		if agent.Name == config.ControlDispatcherAgentName && agent.Dir == dir {
			return true
		}
	}
	return false
}

func testControlDispatcherAgent(dir string) config.Agent {
	return config.Agent{
		Name:              config.ControlDispatcherAgentName,
		Dir:               dir,
		StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
		ProcessNames:      []string{"gc"},
		MaxActiveSessions: intPtr(1),
	}
}

func testControlDispatcherAgentTOML(dir string) string {
	var b strings.Builder
	b.WriteString("\n[[agent]]\n")
	b.WriteString("name = \"control-dispatcher\"\n")
	if dir != "" {
		b.WriteString(fmt.Sprintf("dir = %q\n", dir))
	}
	b.WriteString("start_command = \"gc convoy control --serve --follow {{.Agent}}\"\n")
	b.WriteString("prompt_mode = \"none\"\n")
	b.WriteString("process_names = [\"gc\"]\n")
	b.WriteString("max_active_sessions = 1\n")
	return b.String()
}

func mustGetMemBead(t *testing.T, store beads.Store, id string) beads.Bead {
	t.Helper()
	bead, err := store.Get(id)
	if err != nil {
		t.Fatalf("Get(%s): %v", id, err)
	}
	return bead
}

func beadRef(bead beads.Bead) string {
	if bead.Ref != "" {
		return bead.Ref
	}
	return bead.Metadata["gc.step_ref"]
}

func selectExecutableGraphWorkerBead(ready []beads.Bead, assignee string) (beads.Bead, bool, error) {
	for _, bead := range ready {
		if bead.Assignee != assignee {
			continue
		}
		kind := bead.Metadata["gc.kind"]
		switch {
		case graphroute.IsControlDispatcherKind(kind):
			return beads.Bead{}, false, fmt.Errorf("worker queue exposed control bead %s kind=%s ref=%s", bead.ID, kind, beadRef(bead))
		case kind == "workflow" || kind == "scope" || kind == "ralph" || kind == "retry":
			return beads.Bead{}, false, fmt.Errorf("worker queue exposed latch bead %s kind=%s ref=%s", bead.ID, kind, beadRef(bead))
		case bead.Status != "open":
			continue
		case bead.Metadata["gc.outcome"] == "skipped":
			continue
		default:
			return bead, true, nil
		}
	}
	return beads.Bead{}, false, nil
}

func executeMemGraphWorkerBead(t *testing.T, store beads.Store, bead beads.Bead, targetID, cityPath, mode string) {
	t.Helper()

	ref := beadRef(bead)
	switch {
	case strings.Contains(ref, ".workspace-setup"):
		workDir := filepath.Join(cityPath, "worktrees", targetID)
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", workDir, err)
		}
		if err := store.SetMetadata(targetID, "work_dir", workDir); err != nil {
			t.Fatalf("SetMetadata(work_dir): %v", err)
		}
	case strings.Contains(ref, ".implement"):
		target := mustGetMemBead(t, store, targetID)
		workDir := target.Metadata["work_dir"]
		if workDir == "" {
			t.Fatalf("implement step missing work_dir on target convoy %s", targetID)
		}
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", workDir, err)
		}
		if err := os.WriteFile(filepath.Join(workDir, "implemented.txt"), []byte("implemented\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(implemented.txt): %v", err)
		}
	case strings.Contains(ref, ".submit"):
		if err := store.SetMetadata(targetID, "submitted", "true"); err != nil {
			t.Fatalf("SetMetadata(submitted): %v", err)
		}
	case strings.Contains(ref, ".cleanup-worktree"):
		target := mustGetMemBead(t, store, targetID)
		workDir := target.Metadata["work_dir"]
		if workDir != "" {
			_ = os.RemoveAll(workDir)
		}
		if err := store.SetMetadata(targetID, "work_dir", ""); err != nil {
			t.Fatalf("SetMetadata(clear work_dir): %v", err)
		}
	case strings.Contains(ref, ".preflight-tests") && mode == "fail-preflight":
		if err := store.SetMetadataBatch(bead.ID, map[string]string{
			"gc.outcome":        "fail",
			"gc.failure_class":  "hard",
			"gc.failure_reason": "preflight_failed",
		}); err != nil {
			t.Fatalf("SetMetadataBatch(fail): %v", err)
		}
	}

	if mode != "fail-preflight" || !strings.Contains(ref, ".preflight-tests") {
		if err := store.SetMetadata(bead.ID, "gc.outcome", "pass"); err != nil {
			t.Fatalf("SetMetadata(gc.outcome=pass): %v", err)
		}
	}

	if err := store.Close(bead.ID); err != nil {
		t.Fatalf("Close(%s): %v", bead.ID, err)
	}
}

func memGraphReady(t *testing.T, store beads.Store) []beads.Bead {
	t.Helper()

	all, err := store.List(beads.ListQuery{AllowScan: true, IncludeClosed: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List(graph ready): %v", err)
	}
	statusByID := make(map[string]string, len(all))
	for _, bead := range all {
		statusByID[bead.ID] = bead.Status
	}

	var ready []beads.Bead
	for _, bead := range all {
		if bead.Status != "open" || beads.IsReadyExcludedType(bead.Type) {
			continue
		}
		deps, err := store.DepList(bead.ID, "down")
		if err != nil {
			t.Fatalf("DepList(%s, down): %v", bead.ID, err)
		}
		blocked := false
		for _, dep := range deps {
			switch dep.Type {
			case "blocks", "waits-for", "conditional-blocks":
			default:
				continue
			}
			if statusByID[dep.DependsOnID] != "closed" {
				blocked = true
				break
			}
		}
		if !blocked {
			ready = append(ready, bead)
		}
	}
	return ready
}

func runMemGraphWorkflowToCompletion(t *testing.T, store beads.Store, workflowID, targetID, workerSession, cityPath, mode string) {
	t.Helper()

	for step := 0; step < 200; step++ {
		root := mustGetMemBead(t, store, workflowID)
		if root.Status == "closed" {
			return
		}

		ready := memGraphReady(t, store)

		progressed := false
		for _, bead := range ready {
			if !graphroute.IsControlDispatcherKind(bead.Metadata["gc.kind"]) {
				continue
			}
			result, err := dispatch.ProcessControl(store, bead, dispatch.ProcessOptions{CityPath: cityPath})
			if err != nil {
				t.Fatalf("ProcessControl(%s): %v", bead.ID, err)
			}
			progressed = progressed || result.Processed
		}

		ready = memGraphReady(t, store)
		for {
			bead, ok, err := selectExecutableGraphWorkerBead(ready, workerSession)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			executeMemGraphWorkerBead(t, store, bead, targetID, cityPath, mode)
			progressed = true
			ready = memGraphReady(t, store)
		}

		if progressed {
			continue
		}

		var readySummary []string
		for _, bead := range ready {
			readySummary = append(readySummary,
				fmt.Sprintf("%s kind=%s ref=%s assignee=%s status=%s outcome=%s",
					bead.ID, bead.Metadata["gc.kind"], beadRef(bead), bead.Assignee, bead.Status, bead.Metadata["gc.outcome"]))
		}
		t.Fatalf("workflow %s made no progress; ready=%v", workflowID, readySummary)
	}

	t.Fatalf("workflow %s did not finish within step budget", workflowID)
}

func startMemScopedWorkflow(t *testing.T) (*beads.MemStore, string, string) {
	t.Helper()

	runner := newFakeRunner()
	cfg := buildMemGraphWorkflowConfig(t)
	store := beads.NewMemStore()
	first, err := store.Create(beads.Bead{Title: "Run scoped workflow part one", Type: "task"})
	if err != nil {
		t.Fatalf("Create(first issue): %v", err)
	}
	second, err := store.Create(beads.Bead{Title: "Run scoped workflow part two", Type: "task"})
	if err != nil {
		t.Fatalf("Create(second issue): %v", err)
	}
	convoy, err := store.Create(beads.Bead{Title: "Run scoped workflow", Type: "convoy"})
	if err != nil {
		t.Fatalf("Create(convoy): %v", err)
	}
	for _, id := range []string{first.ID, second.ID} {
		if err := convoycore.TrackItem(store, convoy.ID, id); err != nil {
			t.Fatalf("TrackItem(%s, %s): %v", convoy.ID, id, err)
		}
	}

	deps, stdout, stderr := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = store
	deps.CityPath = t.TempDir()

	worker, ok := resolveAgentIdentity(cfg, "worker", "")
	if !ok {
		t.Fatal("resolveAgentIdentity(worker) failed")
	}

	oldPoke := slingPokeController
	slingPokeController = func(string) error { return nil }
	t.Cleanup(func() { slingPokeController = oldPoke })

	opts := testOpts(worker, convoy.ID)
	opts.OnFormula = "mol-scoped-work"
	if code := doSling(opts, deps, store, stdout, stderr); code != 0 {
		t.Fatalf("doSling returned %d; stderr=%s", code, stderr.String())
	}

	roots, err := store.ListByMetadata(map[string]string{"gc.input_convoy_id": convoy.ID, "gc.kind": "workflow"}, 1)
	if err != nil {
		t.Fatalf("ListByMetadata(workflow root): %v", err)
	}
	if len(roots) != 1 {
		t.Fatalf("workflow root count = %d, want 1", len(roots))
	}
	return store, convoy.ID, roots[0].ID
}

func TestSelectExecutableGraphWorkerBeadRejectsControlKinds(t *testing.T) {
	ready := []beads.Bead{{
		ID:       "gc-2",
		Status:   "open",
		Assignee: "worker",
		Metadata: map[string]string{
			"gc.kind":     "scope-check",
			"gc.step_ref": "mol-scoped-work.preflight-tests-scope-check",
		},
	}}

	_, _, err := selectExecutableGraphWorkerBead(ready, "worker")
	if err == nil || !strings.Contains(err.Error(), "control bead") {
		t.Fatalf("err = %v, want control bead error", err)
	}
}

func TestSelectExecutableGraphWorkerBeadRejectsLatchKinds(t *testing.T) {
	ready := []beads.Bead{{
		ID:       "gc-3",
		Status:   "open",
		Assignee: "worker",
		Metadata: map[string]string{
			"gc.kind":     "scope",
			"gc.step_ref": "mol-scoped-work.body",
		},
	}}

	_, _, err := selectExecutableGraphWorkerBead(ready, "worker")
	if err == nil || !strings.Contains(err.Error(), "latch bead") {
		t.Fatalf("err = %v, want latch bead error", err)
	}
}

func TestSelectExecutableGraphWorkerBeadSkipsForeignAndSkippedWork(t *testing.T) {
	ready := []beads.Bead{
		{
			ID:       "gc-4",
			Status:   "open",
			Assignee: "other",
			Metadata: map[string]string{"gc.step_ref": "mol-scoped-work.load-context"},
		},
		{
			ID:       "gc-5",
			Status:   "open",
			Assignee: "worker",
			Metadata: map[string]string{"gc.step_ref": "mol-scoped-work.implement", "gc.outcome": "skipped"},
		},
		{
			ID:       "gc-6",
			Status:   "open",
			Assignee: "worker",
			Metadata: map[string]string{"gc.step_ref": "mol-scoped-work.self-review"},
		},
	}

	bead, ok, err := selectExecutableGraphWorkerBead(ready, "worker")
	if err != nil {
		t.Fatalf("selectExecutableGraphWorkerBead: %v", err)
	}
	if !ok {
		t.Fatal("expected executable bead")
	}
	if bead.ID != "gc-6" {
		t.Fatalf("selected bead = %s, want gc-6", bead.ID)
	}
}

func TestGraphWorkflowInMemorySuccessPath(t *testing.T) {
	store, convoyID, workflowID := startMemScopedWorkflow(t)
	cityPath := t.TempDir()

	runMemGraphWorkflowToCompletion(t, store, workflowID, convoyID, "worker", cityPath, "success")

	root := mustGetMemBead(t, store, workflowID)
	if root.Status != "closed" {
		t.Fatalf("root status = %q, want closed", root.Status)
	}
	if got := root.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("root outcome = %q, want pass", got)
	}

	convoy := mustGetMemBead(t, store, convoyID)
	if got := convoy.Metadata["submitted"]; got != "true" {
		t.Fatalf("submitted = %q, want true", got)
	}
	if got := convoy.Metadata["work_dir"]; got != "" {
		t.Fatalf("work_dir = %q, want empty after cleanup", got)
	}
}

func TestGraphWorkflowInMemoryFailureRunsCleanup(t *testing.T) {
	store, convoyID, workflowID := startMemScopedWorkflow(t)
	cityPath := t.TempDir()

	runMemGraphWorkflowToCompletion(t, store, workflowID, convoyID, "worker", cityPath, "fail-preflight")

	root := mustGetMemBead(t, store, workflowID)
	if root.Status != "closed" {
		t.Fatalf("root status = %q, want closed", root.Status)
	}
	if got := root.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("root outcome = %q, want fail", got)
	}

	convoy := mustGetMemBead(t, store, convoyID)
	if got := convoy.Metadata["submitted"]; got != "" {
		t.Fatalf("submitted = %q, want empty on failed workflow", got)
	}
	if got := convoy.Metadata["work_dir"]; got != "" {
		t.Fatalf("work_dir = %q, want empty after cleanup", got)
	}

	all, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{"gc.root_bead_id": workflowID},
		IncludeClosed: true,
		TierMode:      beads.TierBoth,
	})
	if err != nil {
		t.Fatalf("ListByMetadata(gc.root_bead_id=%q): %v", workflowID, err)
	}
	wantSkipped := map[string]bool{
		"mol-scoped-work.implement":   false,
		"mol-scoped-work.self-review": false,
		"mol-scoped-work.submit":      false,
	}
	for _, bead := range all {
		ref := beadRef(bead)
		if _, ok := wantSkipped[ref]; !ok {
			continue
		}
		wantSkipped[ref] = true
		if bead.Status != "closed" {
			t.Fatalf("%s status = %q, want closed", ref, bead.Status)
		}
		if bead.Metadata["gc.outcome"] != "skipped" {
			t.Fatalf("%s outcome = %q, want skipped", ref, bead.Metadata["gc.outcome"])
		}
	}
	for ref, seen := range wantSkipped {
		if !seen {
			t.Fatalf("missing skipped bead %s", ref)
		}
	}
}

func TestGraphWorkflowInMemoryCreateExecuteWaitFlow(t *testing.T) {
	store, convoyID, workflowID := startMemScopedWorkflow(t)
	if convoyID == "" || workflowID == "" {
		t.Fatalf("convoy/workflow ids must be non-empty: convoy=%q workflow=%q", convoyID, workflowID)
	}

	root := mustGetMemBead(t, store, workflowID)
	if root.Metadata["gc.kind"] != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", root.Metadata["gc.kind"])
	}
	if root.Status != "in_progress" {
		t.Fatalf("root status = %q, want in_progress", root.Status)
	}
	if root.Metadata["gc.source_bead_id"] != "" {
		t.Fatalf("root source_bead_id = %q, want empty", root.Metadata["gc.source_bead_id"])
	}
	if root.Metadata["gc.input_convoy_id"] != convoyID {
		t.Fatalf("root input_convoy_id = %q, want %q", root.Metadata["gc.input_convoy_id"], convoyID)
	}

	runMemGraphWorkflowToCompletion(t, store, workflowID, convoyID, "worker", t.TempDir(), "success")

	root = mustGetMemBead(t, store, workflowID)
	if root.Status != "closed" || root.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root = status %q outcome %q, want closed/pass", root.Status, root.Metadata["gc.outcome"])
	}
}

func TestGraphWorkflowInMemoryRouteUsesControlDispatcherForControlBeads(t *testing.T) {
	store, _, workflowID := startMemScopedWorkflow(t)

	all, err := store.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	foundControl := false
	for _, bead := range all {
		if bead.Metadata["gc.root_bead_id"] != workflowID {
			continue
		}
		if !graphroute.IsControlDispatcherKind(bead.Metadata["gc.kind"]) {
			continue
		}
		foundControl = true
		if bead.Assignee != "" {
			t.Fatalf("control bead %s assignee = %q, want empty routed control-dispatcher queue", bead.ID, bead.Assignee)
		}
		if got := bead.Metadata["gc.routed_to"]; got != config.ControlDispatcherAgentName {
			t.Fatalf("control bead %s gc.routed_to = %q, want %q", bead.ID, got, config.ControlDispatcherAgentName)
		}
	}
	if !foundControl {
		t.Fatal("expected at least one control-dispatcher bead")
	}
}

func TestGraphWorkflowRoutingLeavesSpecBeadsUnrouted(t *testing.T) {
	cfg := buildMemGraphWorkflowConfig(t)
	store := beads.NewMemStore()
	cityPath := t.TempDir()
	worker, ok := resolveAgentIdentity(cfg, "worker", "")
	if !ok {
		t.Fatal("resolveAgentIdentity(worker) failed")
	}

	recipe := &formula.Recipe{
		Name: "wf",
		Steps: []formula.RecipeStep{
			{
				ID:     "wf",
				Title:  "Workflow",
				Type:   "task",
				IsRoot: true,
				Metadata: map[string]string{
					"gc.kind":             "workflow",
					"gc.formula_contract": "graph.v2",
				},
			},
			{
				ID:    "wf.review",
				Title: "Review",
				Type:  "task",
				Metadata: map[string]string{
					"gc.run_target": "worker",
				},
			},
			{
				ID:          "wf.review.spec",
				Title:       "Review spec",
				Type:        "spec",
				Description: `{"id":"review"}`,
				Metadata: map[string]string{
					"gc.kind":     "spec",
					"gc.spec_for": "review",
				},
			},
			{ID: "wf.workflow-finalize", Title: "Finalize", Type: "task", Metadata: map[string]string{"gc.kind": "workflow-finalize"}},
		},
		Deps: []formula.RecipeDep{
			{StepID: "wf.workflow-finalize", DependsOnID: "wf.review", Type: "blocks"},
			{StepID: "wf", DependsOnID: "wf.workflow-finalize", Type: "blocks"},
		},
	}

	if err := applyGraphRouting(recipe, &worker, worker.QualifiedName(), nil, "", "", "city:test-city", store, cfg.Workspace.Name, cityPath, cfg); err != nil {
		t.Fatalf("applyGraphRouting: %v", err)
	}

	var spec *formula.RecipeStep
	for i := range recipe.Steps {
		if recipe.Steps[i].ID == "wf.review.spec" {
			spec = &recipe.Steps[i]
			break
		}
	}
	if spec == nil {
		t.Fatal("missing spec step")
	}
	if spec.Assignee != "" {
		t.Fatalf("spec Assignee = %q, want empty", spec.Assignee)
	}
	for _, key := range []string{"gc.routed_to", graphroute.GraphExecutionRouteMetaKey, "gc.run_target"} {
		if spec.Metadata[key] != "" {
			t.Fatalf("spec metadata %s = %q, want empty; full metadata: %#v", key, spec.Metadata[key], spec.Metadata)
		}
	}
}

func TestGraphWorkflowInMemoryInstantiationUsesFormulaLayers(t *testing.T) {
	cfg := buildMemGraphWorkflowConfig(t)
	paths := cfg.FormulaLayers.SearchPaths("")
	if len(paths) == 0 {
		t.Fatal("formula search paths empty")
	}
	found := false
	for _, path := range paths {
		if strings.HasSuffix(path, string(filepath.Separator)+"formulas") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("formula search paths = %v, want built-in formulas dir", paths)
	}
}
