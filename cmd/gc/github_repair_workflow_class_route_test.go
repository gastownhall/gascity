package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/githubmonitor"
)

// The GitHub monitor's one call into molecule instantiation, held to the same
// class-routing invariant oneshot_class_routes_test.go holds `gc formula cook`
// to. It needs its own file because nothing else reaches this path: the one
// existing test of the monitor replaces attachGitHubPRRepairWorkflow with a
// stub, so the real function could route every repair workflow into the wrong
// ledger with the whole suite green.

// attachRepairWorkflowFixture stands up a city plus the work store the monitor
// itself would be holding, and — on the split arm — the class binding a
// graph-class molecule belongs in.
func attachRepairWorkflowFixture(t *testing.T, split bool) (string, beads.Store, beads.Store) {
	t.Helper()
	cityDir := oneShotCookCity(t)
	var graph beads.Store
	if split {
		graph = splittest.NewClassStore(t, config.BeadClassGraph)
		seedCLIStorageRoutes(t, cityDir, messagingSplitRoutes(graph))
	} else {
		seedCLIStorageRoutes(t, cityDir, nil)
	}
	work, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("open work store: %v", err)
	}
	t.Cleanup(func() { _ = work.Close("test cleanup") })
	return cityDir, graph, work
}

// attachRepairWorkflow drives defaultAttachGitHubPRRepairWorkflow the way the
// monitor does: a PR bead already on the work ledger, a workflow the monitor's
// config names, and the graph leg derived through the same cliGraphStore seam
// the command root uses (cmd_github.go's attachGitHubPRRepairWorkflow(store,
// cliGraphStore(store, cfg, cityPath), ...)) — handing in a store the caller
// would never produce would make the routing assertions the fixture's, not the
// monitor's.
func attachRepairWorkflow(t *testing.T, cityDir string, work beads.Store, workflow string) beads.Bead {
	t.Helper()
	parent, err := work.Create(beads.Bead{Title: "PR needs repair", Type: "task"})
	if err != nil {
		t.Fatalf("creating the PR bead: %v", err)
	}
	cfg := &config.City{FormulaLayers: config.FormulaLayers{City: []string{filepath.Join(cityDir, "formulas")}}}
	rig := config.Rig{Path: cityDir}
	monitor := config.GitHubPRMonitor{RepairWorkflow: workflow}
	result := githubmonitor.Result{Owner: "gastownhall", Repo: "gascity", Number: 42, HeadRefName: "fix/x"}
	if err := defaultAttachGitHubPRRepairWorkflow(work, cliGraphStore(work, cfg, cityDir), cfg, rig, monitor, parent, result); err != nil {
		t.Fatalf("attaching repair workflow %q: %v", workflow, err)
	}
	return parent
}

// wispRootIn returns the wisp root a repair workflow materialized into store,
// or nil. gc.kind=wisp is coordclass.Classify's first arm, which is what makes
// a root-only repair workflow graph class.
func wispRootIn(t *testing.T, store beads.Store) *beads.Bead {
	t.Helper()
	for _, b := range allBeads(t, store) {
		if b.Metadata[beadmeta.KindMetadataKey] == beadmeta.KindWisp {
			return &b
		}
	}
	return nil
}

// TestAttachGitHubPRRepairWorkflowWispLandsInGraphStoreOnSplitCity is the
// producer half. `gc github` opens one work store for the rig it watches and
// hands it to the attach; a repair workflow that compiles root-only carries
// gc.kind=wisp, so on a split city it belongs in the binding. Materialized
// through the work store instead, it is exactly the stranded-infrastructure-bead
// shape that refuses boot.
func TestAttachGitHubPRRepairWorkflowWispLandsInGraphStoreOnSplitCity(t *testing.T) {
	cityDir, graph, work := attachRepairWorkflowFixture(t, true)

	parent := attachRepairWorkflow(t, cityDir, work, "vapor-work")

	if wispRootIn(t, graph) == nil {
		t.Fatal("the repair workflow's wisp root is not resident in the graph binding; the monitor materialized it through the work store it was handed")
	}
	for _, b := range allBeads(t, work) {
		if b.ID == parent.ID {
			continue
		}
		if kind := b.Metadata[beadmeta.KindMetadataKey]; kind != "" {
			t.Errorf("work store holds repair-workflow bead %s (gc.kind=%q); on a split city boot refuses on it", b.ID, kind)
		}
	}
}

// TestAttachGitHubPRRepairWorkflowPouredMoleculeStaysOnTheWorkStore is the
// direction a blanket "route the monitor's molecules to the binding" fix would
// break: a POURED v1 repair workflow is work class end to end, and relocating it
// hides the repair steps from `gc hook` and every other work-scope reader.
func TestAttachGitHubPRRepairWorkflowPouredMoleculeStaysOnTheWorkStore(t *testing.T) {
	cityDir, graph, work := attachRepairWorkflowFixture(t, true)

	attachRepairWorkflow(t, cityDir, work, "legacy-work")

	if got := len(allBeads(t, graph)); got != 0 {
		t.Errorf("the graph binding holds %d bead(s) from a work-class repair workflow; its steps are now invisible to every work-scope reader", got)
	}
	if got := len(allBeads(t, work)); got < 2 {
		t.Errorf("the work store holds %d bead(s), want the PR bead plus the materialized molecule", got)
	}
}

// TestAttachGitHubPRRepairWorkflowStaysOnTheOneStoreOnSingleStoreCity is the
// compatibility half: a city that relocates nothing attaches into the one store
// it always used. Green before and after by design.
func TestAttachGitHubPRRepairWorkflowStaysOnTheOneStoreOnSingleStoreCity(t *testing.T) {
	cityDir, _, work := attachRepairWorkflowFixture(t, false)

	attachRepairWorkflow(t, cityDir, work, "vapor-work")

	if wispRootIn(t, work) == nil {
		t.Error("the repair workflow's wisp root is not in the one store a single-store city has")
	}
}
