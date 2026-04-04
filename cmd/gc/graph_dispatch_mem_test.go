package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/runtime"
)

func builtinFormulaDir(t *testing.T) string {
	t.Helper()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatalf("getwd: %v", err)
	}
	return filepath.Join(cwd, "formulas")
}

func buildMemGraphWorkflowConfig(t *testing.T) *config.City {
	t.Helper()
	cfg := &config.City{
		Daemon: config.DaemonConfig{
			FormulaV2: true,
		},
		Workspace: config.Workspace{Name: "test-city"},
		FormulaLayers: config.FormulaLayers{
			City: []string{builtinFormulaDir(t)},
		},
		Agents: []config.Agent{
			{Name: "worker", MaxActiveSessions: intPtr(1)},
		},
	}
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	config.InjectImplicitAgents(cfg)
	return cfg
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
		case isControlDispatcherKind(kind):
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

func executeMemGraphWorkerBead(t *testing.T, store beads.Store, bead beads.Bead, sourceID, cityPath, mode string) {
	t.Helper()

	ref := beadRef(bead)
	switch {
	case strings.Contains(ref, ".workspace-setup"):
		workDir := filepath.Join(cityPath, "worktrees", sourceID)
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", workDir, err)
		}
		if err := store.SetMetadata(sourceID, "work_dir", workDir); err != nil {
			t.Fatalf("SetMetadata(work_dir): %v", err)
		}
	case strings.Contains(ref, ".implement"):
		source := mustGetMemBead(t, store, sourceID)
		workDir := source.Metadata["work_dir"]
		if workDir == "" {
			t.Fatalf("implement step missing work_dir on source bead %s", sourceID)
		}
		if err := os.MkdirAll(workDir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q): %v", workDir, err)
		}
		if err := os.WriteFile(filepath.Join(workDir, "implemented.txt"), []byte("implemented\n"), 0o644); err != nil {
			t.Fatalf("WriteFile(implemented.txt): %v", err)
		}
	case strings.Contains(ref, ".submit"):
		if err := store.SetMetadata(sourceID, "submitted", "true"); err != nil {
			t.Fatalf("SetMetadata(submitted): %v", err)
		}
	case strings.Contains(ref, ".cleanup-worktree"):
		source := mustGetMemBead(t, store, sourceID)
		workDir := source.Metadata["work_dir"]
		if workDir != "" {
			_ = os.RemoveAll(workDir)
		}
		if err := store.SetMetadata(sourceID, "work_dir", ""); err != nil {
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

func runMemGraphWorkflowToCompletion(t *testing.T, store beads.Store, workflowID, sourceID, workerSession, cityPath, mode string) {
	t.Helper()

	for step := 0; step < 200; step++ {
		root := mustGetMemBead(t, store, workflowID)
		if root.Status == "closed" {
			return
		}

		ready, err := store.Ready()
		if err != nil {
			t.Fatalf("Ready(): %v", err)
		}

		progressed := false
		for _, bead := range ready {
			if !isControlDispatcherKind(bead.Metadata["gc.kind"]) {
				continue
			}
			result, err := dispatch.ProcessControl(store, bead, dispatch.ProcessOptions{CityPath: cityPath})
			if err != nil {
				t.Fatalf("ProcessControl(%s): %v", bead.ID, err)
			}
			progressed = progressed || result.Processed
		}

		ready, err = store.Ready()
		if err != nil {
			t.Fatalf("Ready() after control: %v", err)
		}
		for {
			bead, ok, err := selectExecutableGraphWorkerBead(ready, workerSession)
			if err != nil {
				t.Fatal(err)
			}
			if !ok {
				break
			}
			executeMemGraphWorkerBead(t, store, bead, sourceID, cityPath, mode)
			progressed = true
			ready, err = store.Ready()
			if err != nil {
				t.Fatalf("Ready() after worker step: %v", err)
			}
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
	issue, err := store.Create(beads.Bead{Title: "Run scoped workflow", Type: "task"})
	if err != nil {
		t.Fatalf("Create(issue): %v", err)
	}

	deps, _, stderr := testDeps(cfg, runtime.NewFake(), runner.run)
	deps.Store = store
	deps.CityPath = t.TempDir()

	worker, ok := resolveAgentIdentity(cfg, "worker", "")
	if !ok {
		t.Fatal("resolveAgentIdentity(worker) failed")
	}

	oldPoke := slingPokeController
	slingPokeController = func(string) error { return nil }
	t.Cleanup(func() { slingPokeController = oldPoke })

	opts := testOpts(worker, issue.ID)
	opts.OnFormula = "mol-scoped-work"
	opts.Vars = []string{"issue=" + issue.ID}
	if code := doSling(opts, deps, store); code != 0 {
		t.Fatalf("doSling returned %d; stderr=%s", code, stderr.String())
	}

	source := mustGetMemBead(t, store, issue.ID)
	workflowID := source.Metadata["workflow_id"]
	if workflowID == "" {
		t.Fatal("source bead workflow_id missing")
	}
	return store, issue.ID, workflowID
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
	store, issueID, workflowID := startMemScopedWorkflow(t)
	cityPath := t.TempDir()

	runMemGraphWorkflowToCompletion(t, store, workflowID, issueID, "worker", cityPath, "success")

	root := mustGetMemBead(t, store, workflowID)
	if root.Status != "closed" {
		t.Fatalf("root status = %q, want closed", root.Status)
	}
	if got := root.Metadata["gc.outcome"]; got != "pass" {
		t.Fatalf("root outcome = %q, want pass", got)
	}

	issue := mustGetMemBead(t, store, issueID)
	if got := issue.Metadata["submitted"]; got != "true" {
		t.Fatalf("submitted = %q, want true", got)
	}
	if got := issue.Metadata["work_dir"]; got != "" {
		t.Fatalf("work_dir = %q, want empty after cleanup", got)
	}
}

func TestGraphWorkflowInMemoryFailureRunsCleanup(t *testing.T) {
	store, issueID, workflowID := startMemScopedWorkflow(t)
	cityPath := t.TempDir()

	runMemGraphWorkflowToCompletion(t, store, workflowID, issueID, "worker", cityPath, "fail-preflight")

	root := mustGetMemBead(t, store, workflowID)
	if root.Status != "closed" {
		t.Fatalf("root status = %q, want closed", root.Status)
	}
	if got := root.Metadata["gc.outcome"]; got != "fail" {
		t.Fatalf("root outcome = %q, want fail", got)
	}

	issue := mustGetMemBead(t, store, issueID)
	if got := issue.Metadata["submitted"]; got != "" {
		t.Fatalf("submitted = %q, want empty on failed workflow", got)
	}
	if got := issue.Metadata["work_dir"]; got != "" {
		t.Fatalf("work_dir = %q, want empty after cleanup", got)
	}

	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("List(): %v", err)
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
	store, issueID, workflowID := startMemScopedWorkflow(t)
	if issueID == "" || workflowID == "" {
		t.Fatalf("issue/workflow ids must be non-empty: issue=%q workflow=%q", issueID, workflowID)
	}

	root := mustGetMemBead(t, store, workflowID)
	if root.Metadata["gc.kind"] != "workflow" {
		t.Fatalf("root gc.kind = %q, want workflow", root.Metadata["gc.kind"])
	}
	if root.Status != "in_progress" {
		t.Fatalf("root status = %q, want in_progress", root.Status)
	}
	if root.Metadata["gc.source_bead_id"] != issueID {
		t.Fatalf("root source_bead_id = %q, want %q", root.Metadata["gc.source_bead_id"], issueID)
	}

	runMemGraphWorkflowToCompletion(t, store, workflowID, issueID, "worker", t.TempDir(), "success")

	root = mustGetMemBead(t, store, workflowID)
	if root.Status != "closed" || root.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("root = status %q outcome %q, want closed/pass", root.Status, root.Metadata["gc.outcome"])
	}
}

func TestGraphWorkflowInMemoryRouteUsesControlDispatcherForControlBeads(t *testing.T) {
	store, _, workflowID := startMemScopedWorkflow(t)

	all, err := store.ListOpen()
	if err != nil {
		t.Fatalf("List(): %v", err)
	}
	foundControl := false
	for _, bead := range all {
		if bead.Metadata["gc.root_bead_id"] != workflowID {
			continue
		}
		if !isControlDispatcherKind(bead.Metadata["gc.kind"]) {
			continue
		}
		foundControl = true
		if bead.Assignee != config.ControlDispatcherAgentName {
			t.Fatalf("control bead %s assignee = %q, want %q", bead.ID, bead.Assignee, config.ControlDispatcherAgentName)
		}
		if bead.Metadata["gc.routed_to"] != config.ControlDispatcherAgentName {
			t.Fatalf("control bead %s gc.routed_to = %q, want %q", bead.ID, bead.Metadata["gc.routed_to"], config.ControlDispatcherAgentName)
		}
	}
	if !foundControl {
		t.Fatal("expected at least one control-dispatcher bead")
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
