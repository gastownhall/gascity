package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// mergeStrategyCity builds a city whose single rig carries the given default
// merge strategy, matching the shape SlingFormulaTargetBranch's tests use.
func mergeStrategyCity(strategy string) *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test"},
		Rigs: []config.Rig{
			{Name: "scamper", Path: "/scamper", Prefix: "SC", DefaultMergeStrategy: strategy},
		},
	}
}

func TestSlingMergeStrategy_PrefersExplicitFlag(t *testing.T) {
	deps := SlingDeps{Cfg: mergeStrategyCity("mr"), Store: beads.NewMemStore()}
	a := config.Agent{Name: "polecat", Dir: "scamper"}

	if got := SlingMergeStrategy("direct", "SC-1", deps, a); got != "direct" {
		t.Errorf("SlingMergeStrategy = %q, want %q (explicit --merge wins)", got, "direct")
	}
}

func TestSlingMergeStrategy_UsesRigDefaultByBeadPrefix(t *testing.T) {
	deps := SlingDeps{Cfg: mergeStrategyCity("mr"), Store: beads.NewMemStore()}
	a := config.Agent{Name: "polecat"} // no Dir — bead-prefix lookup must win

	if got := SlingMergeStrategy("", "SC-1", deps, a); got != "mr" {
		t.Errorf("SlingMergeStrategy = %q, want %q (rig stored default by bead prefix)", got, "mr")
	}
}

func TestSlingMergeStrategy_UsesRigDefaultByAgent(t *testing.T) {
	deps := SlingDeps{Cfg: mergeStrategyCity("mr"), Store: beads.NewMemStore()}
	a := config.Agent{Name: "polecat", Dir: "scamper"}

	if got := SlingMergeStrategy("", "", deps, a); got != "mr" {
		t.Errorf("SlingMergeStrategy = %q, want %q (rig stored default by agent.Dir)", got, "mr")
	}
}

func TestSlingMergeStrategy_EmptyWhenRigHasNoDefault(t *testing.T) {
	deps := SlingDeps{Cfg: mergeStrategyCity(""), Store: beads.NewMemStore()}
	a := config.Agent{Name: "polecat", Dir: "scamper"}

	if got := SlingMergeStrategy("", "SC-1", deps, a); got != "" {
		t.Errorf("SlingMergeStrategy = %q, want empty (nothing configured)", got)
	}
}

func TestSlingMergeStrategy_EmptyWhenNoConfig(t *testing.T) {
	deps := SlingDeps{Store: beads.NewMemStore()}
	a := config.Agent{Name: "polecat", Dir: "scamper"}

	if got := SlingMergeStrategy("", "SC-1", deps, a); got != "" {
		t.Errorf("SlingMergeStrategy = %q, want empty (nil config)", got)
	}
}

func TestSlingMergeStrategy_TrimsExplicitFlag(t *testing.T) {
	deps := SlingDeps{Cfg: mergeStrategyCity("mr"), Store: beads.NewMemStore()}
	a := config.Agent{Name: "polecat", Dir: "scamper"}

	if got := SlingMergeStrategy("  ", "SC-1", deps, a); got != "mr" {
		t.Errorf("SlingMergeStrategy = %q, want %q (blank flag is unset)", got, "mr")
	}
}

// slingRigBead routes the rig-prefixed bead SC-1 through the plain-bead path
// and returns it as a merge consumer would later read it. The ID must be
// seeded rather than created: MemStore.Create mints its own, and a non-rig
// prefix trips DoSling's cross-rig routing guard before finalize runs.
func slingRigBead(t *testing.T, rigDefault, explicitMerge string) beads.Bead {
	t.Helper()
	runner := newFakeRunner()
	deps := testDeps(mergeStrategyCity(rigDefault), runtime.NewFake(), runner.run)
	deps.Store = seededStore("SC-1")
	a := config.Agent{Name: "polecat", Dir: "scamper", MaxActiveSessions: intPtr(1)}

	if _, err := DoSling(SlingOpts{Target: a, BeadOrFormula: "SC-1", Merge: explicitMerge}, deps, deps.Store); err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	got, err := deps.Store.Get("SC-1")
	if err != nil {
		t.Fatalf("re-reading bead: %v", err)
	}
	return got
}

// TestFinalizeStampsRigDefaultMergeStrategy is the regression test for the
// stranding class this field exists to remove: a bare `gc sling` on a rig
// whose rules of engagement deliver work through a pull request used to leave
// merge_strategy unstamped, which the refinery reads as "direct" and then
// refuses as a false completion when the work landed upstream instead.
func TestFinalizeStampsRigDefaultMergeStrategy(t *testing.T) {
	got := slingRigBead(t, "mr", "")
	if strategy := got.Metadata[beadmeta.MergeStrategyMetadataKey]; strategy != "mr" {
		t.Errorf("metadata[%s] = %q, want %q (rig default stamped without --merge)",
			beadmeta.MergeStrategyMetadataKey, strategy, "mr")
	}
}

func TestFinalizeExplicitMergeBeatsRigDefault(t *testing.T) {
	got := slingRigBead(t, "mr", "direct")
	if strategy := got.Metadata[beadmeta.MergeStrategyMetadataKey]; strategy != "direct" {
		t.Errorf("metadata[%s] = %q, want %q (explicit --merge wins)",
			beadmeta.MergeStrategyMetadataKey, strategy, "direct")
	}
}

// startGraphWorkflowFixture seeds a workflow root plus a separate work bead in
// one store, the shape doStartGraphWorkflow sees when a graph (v2) formula is
// attached to a bead.
func startGraphWorkflowFixture(t *testing.T, strategy string) (SlingDeps, config.Agent, string, string) {
	t.Helper()
	runner := newFakeRunner()
	deps := testDeps(mergeStrategyCity(strategy), runtime.NewFake(), runner.run)
	root, err := deps.Store.Create(beads.Bead{Title: "workflow root", Type: "task"})
	if err != nil {
		t.Fatalf("seeding workflow root: %v", err)
	}
	work, err := deps.Store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("seeding work bead: %v", err)
	}
	return deps, config.Agent{Name: "polecat", Dir: "scamper", MaxActiveSessions: intPtr(1)}, root.ID, work.ID
}

// TestDoStartGraphWorkflowStampsMergeStrategy is the regression test for the
// half of the defect that v1-only stamping missed: graph (v2) formula launches
// never pass through finalize(), so before this fix a v2 sling dropped both
// --merge and the rig's default and left the work bead unstamped.
func TestDoStartGraphWorkflowStampsMergeStrategy(t *testing.T) {
	deps, a, rootID, workID := startGraphWorkflowFixture(t, "mr")

	strategy := SlingMergeStrategy("", workID, deps, a)
	if _, err := doStartGraphWorkflow(rootID, "", workID, strategy, a, "default formula", deps); err != nil {
		t.Fatalf("doStartGraphWorkflow: %v", err)
	}

	got, err := deps.Store.Get(workID)
	if err != nil {
		t.Fatalf("re-reading work bead: %v", err)
	}
	if stamped := got.Metadata[beadmeta.MergeStrategyMetadataKey]; stamped != "mr" {
		t.Errorf("metadata[%s] = %q, want %q on the work bead",
			beadmeta.MergeStrategyMetadataKey, stamped, "mr")
	}
}

// TestDoStartGraphWorkflowLeavesRootUnstamped keeps the stamp on the bead the
// merge consumer actually reads. The workflow root is a control artifact and
// is never the thing that gets merged.
func TestDoStartGraphWorkflowLeavesRootUnstamped(t *testing.T) {
	deps, a, rootID, workID := startGraphWorkflowFixture(t, "mr")

	if _, err := doStartGraphWorkflow(rootID, "", workID, "mr", a, "default formula", deps); err != nil {
		t.Fatalf("doStartGraphWorkflow: %v", err)
	}

	got, err := deps.Store.Get(rootID)
	if err != nil {
		t.Fatalf("re-reading workflow root: %v", err)
	}
	if stamped, ok := got.Metadata[beadmeta.MergeStrategyMetadataKey]; ok && stamped != "" {
		t.Errorf("workflow root metadata[%s] = %q, want unstamped",
			beadmeta.MergeStrategyMetadataKey, stamped)
	}
}

// TestDoStartGraphWorkflowSkipsMergeStampWithoutWorkBead covers the standalone
// `gc sling --formula` launch: there is no bead to merge, so nothing is
// stamped anywhere.
func TestDoStartGraphWorkflowSkipsMergeStampWithoutWorkBead(t *testing.T) {
	deps, a, rootID, _ := startGraphWorkflowFixture(t, "mr")

	if _, err := doStartGraphWorkflow(rootID, "", "", "mr", a, "formula", deps); err != nil {
		t.Fatalf("doStartGraphWorkflow: %v", err)
	}

	got, err := deps.Store.Get(rootID)
	if err != nil {
		t.Fatalf("re-reading workflow root: %v", err)
	}
	if stamped, ok := got.Metadata[beadmeta.MergeStrategyMetadataKey]; ok && stamped != "" {
		t.Errorf("metadata[%s] = %q, want unstamped with no work bead",
			beadmeta.MergeStrategyMetadataKey, stamped)
	}
}

func TestDoStartGraphWorkflowSkipsMergeStampWithoutStrategy(t *testing.T) {
	deps, a, rootID, workID := startGraphWorkflowFixture(t, "")

	strategy := SlingMergeStrategy("", workID, deps, a)
	if _, err := doStartGraphWorkflow(rootID, "", workID, strategy, a, "default formula", deps); err != nil {
		t.Fatalf("doStartGraphWorkflow: %v", err)
	}

	got, err := deps.Store.Get(workID)
	if err != nil {
		t.Fatalf("re-reading work bead: %v", err)
	}
	if stamped, ok := got.Metadata[beadmeta.MergeStrategyMetadataKey]; ok && stamped != "" {
		t.Errorf("metadata[%s] = %q, want unstamped with nothing configured",
			beadmeta.MergeStrategyMetadataKey, stamped)
	}
}

// TestFinalizeLeavesMergeStrategyUnstampedWithoutDefault pins the unchanged
// behavior for rigs that configure nothing: no key is written, so consumers
// keep applying their own implicit default.
func TestFinalizeLeavesMergeStrategyUnstampedWithoutDefault(t *testing.T) {
	got := slingRigBead(t, "", "")
	if strategy, ok := got.Metadata[beadmeta.MergeStrategyMetadataKey]; ok && strategy != "" {
		t.Errorf("metadata[%s] = %q, want unstamped", beadmeta.MergeStrategyMetadataKey, strategy)
	}
}
