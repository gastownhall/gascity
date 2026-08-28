package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// These tests guard the gci-7ba / gastownhall/gascity#2848 regression: when a
// formula is attached-and-routed onto an existing work bead with
// `gc sling <pool> <bead> --on <formula>`, the pool's demand-bearing unit must
// stay open + routed + unassigned so a COLD pool (no live sessions) computes
// scale_check demand > 0 and spawns a worker. The deployed-v1.4.0 regression
// stamped gc.routed_to on the workflow root, which is flipped open->in_progress
// at launch and is therefore invisible to the scale_check "ready" predicate
// (bd ready --metadata-field gc.routed_to=<target> --unassigned) -> cold-pool
// demand = 0 -> the attached work was silently stranded (no session ever spawns
// to run buildOnBoot's reopen, so the pool never self-heals).
//
// The demand assertion (defaultScaleCheckCounts sees the pool) is the durable
// invariant; it is deliberately agnostic to WHICH bead carries the route,
// because the two attach paths in attachFormulaToBead satisfy it differently:
//
//   - legacy [[steps]] formula: the SOURCE bead itself is routed (and the wisp
//     root is left unrouted / privatized out of Ready()); the source is the
//     demand unit. See TestOnFormulaAttachesAndRoutes for the routing half.
//   - graph.v2 pour formula: the source becomes an input-convoy member and the
//     workflow root goes in_progress+routed (excluded from demand); the routed,
//     unassigned executable step is the demand unit for a pool target.

// TestOnFormulaLegacyColdPoolSeesDemand covers the legacy [[steps]] attach path
// on a rig pool and closes the loop end-to-end from `--on` routing to cold-pool
// demand. It mirrors the store/rig setup of
// TestBuiltInSlingPoolRouteContractUsesMetadataOnly (the only other real
// sling -> defaultScaleCheckCounts chain) plus the `--on` wiring of
// TestOnFormulaAttachesAndRoutes.
func TestOnFormulaLegacyColdPoolSeesDemand(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	maxPolecats := 5
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "saitoc", Path: "/tmp/saitoc", Prefix: "gc"}},
		Agents: []config.Agent{
			{Name: "polecat", Dir: "saitoc", MaxActiveSessions: &maxPolecats},
		},
	}
	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	store := newSlingTestStore()
	deps.Store = store
	// The bead lives in the saitoc rig store, reused below as the rig store for
	// the scale_check probe; align StoreRef so the HQ->rig-pool route is allowed
	// and the bead ID prefix matches the rig prefix (cross-rig guard).
	deps.StoreRef = "rig:saitoc"

	created, err := store.Create(beads.Bead{Title: "cold-pool --on work", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	opts := testOpts(cfg.Agents[0], created.ID)
	opts.OnFormula = "code-review"
	if code := doSling(opts, deps, store, stdout, stderr); code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}

	// Route lands on the SOURCE (work) bead, which stays open + unassigned.
	source, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if got := source.Metadata["gc.routed_to"]; got != "saitoc/polecat" {
		t.Fatalf("source gc.routed_to = %q, want saitoc/polecat", got)
	}
	if got := source.Status; got != "open" {
		t.Fatalf("source status = %q, want open (claimable)", got)
	}
	if got := source.Assignee; got != "" {
		t.Fatalf("source assignee = %q, want empty (unassigned)", got)
	}
	// The wisp root must NOT carry the pool route (that was the #2848 regression:
	// routing the immediately-in_progress root strands the work).
	rootID := source.Metadata["molecule_id"]
	if rootID == "" {
		t.Fatal("source bead missing molecule_id after --on attach")
	}
	if root, err := store.Get(rootID); err == nil {
		if got := root.Metadata["gc.routed_to"]; got != "" {
			t.Fatalf("wisp root %s gc.routed_to = %q, want empty (root must not carry the pool route)", rootID, got)
		}
	}

	// The anti-stranding invariant: a cold pool sees demand from the routed source.
	counts, partials, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{
		defaultScaleCheckTargetForAgent(sharedTestCityDir, cfg, &cfg.Agents[0], nil, map[string]beads.Store{"saitoc": store}),
	})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errors: %v", errs)
	}
	if len(partials) != 0 {
		t.Fatalf("defaultScaleCheckCounts partials: %v", partials)
	}
	if got := counts["saitoc/polecat"]; got != 1 {
		t.Fatalf("cold-pool demand after legacy --on = %d, want 1 (work stranded if 0)", got)
	}
}

// TestOnFormulaGraphV2ColdPoolSeesDemand covers the graph.v2 pour attach path —
// the exact path that regressed in gci-7ba — on a POOL target, so the executable
// step binds MetadataOnly (unassigned + routed) and must supply cold-pool demand
// even though the workflow root is in_progress. It mirrors
// TestOnFormulaGraphWorkflowPreassignsNonLatchBeadsForFixedAgent but uses a
// pooled (MaxActiveSessions>1) target instead of a fixed single-session agent,
// which is the difference between a pre-assigned step and a cold-pool-demand step.
func TestOnFormulaGraphV2ColdPoolSeesDemand(t *testing.T) {
	runner := newFakeRunner()
	sp := runtime.NewFake()
	maxPolecats := 5
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	applyFeatureFlags(cfg)
	t.Cleanup(func() { applyFeatureFlags(&config.City{}) })
	dir := testFormulaDir(t)
	cfg.FormulaLayers.City = []string{dir}
	graphFormula := `
formula = "graph-work"
version = 2
contract = "graph.v2"

[[steps]]
id = "step"
title = "Do work"
`
	if err := os.WriteFile(filepath.Join(dir, "graph-work.toml"), []byte(graphFormula), 0o644); err != nil {
		t.Fatal(err)
	}

	deps, stdout, stderr := testDeps(cfg, sp, runner.run)
	store := beads.NewMemStoreFrom(1, []beads.Bead{
		{ID: "BL-42", Title: "Work", Type: "task", Status: "open"},
	}, nil)
	deps.Store = store
	config.InjectImplicitAgents(cfg)
	addTestControlDispatcherAgents(cfg, "")

	// City-level pool target: pooled so the executable step binds MetadataOnly
	// (routed + unassigned) rather than being pre-assigned to a fixed session.
	a := config.Agent{Name: "polecat", MaxActiveSessions: &maxPolecats}
	opts := testOpts(a, "BL-42")
	opts.OnFormula = "graph-work"
	opts.ScopeKind = "city"
	opts.ScopeRef = "test-city"
	if code := doSling(opts, deps, nil, stdout, stderr); code != 0 {
		t.Fatalf("doSling returned %d, want 0; stderr: %s", code, stderr.String())
	}

	// The source bead is an input-convoy member: open, unrouted, unassigned. It
	// is NOT the demand unit in the pour model.
	source, err := store.Get("BL-42")
	if err != nil {
		t.Fatalf("get source: %v", err)
	}
	if got := source.Status; got != "open" {
		t.Fatalf("source status = %q, want open", got)
	}
	if got := source.Metadata["gc.routed_to"]; got != "" {
		t.Fatalf("source gc.routed_to = %q, want empty (source is a convoy member, not the routed unit)", got)
	}

	// Classify the workflow beads: the root must be in_progress+routed (and thus
	// excluded from demand), and at least one executable step must be
	// open+routed+unassigned (the cold-pool demand unit). This is the precise
	// shape that gci-7ba broke: with only an in_progress routed root, demand=0.
	all, err := store.List(beads.ListQuery{AllowScan: true, TierMode: beads.TierBoth})
	if err != nil {
		t.Fatalf("list beads: %v", err)
	}
	var root beads.Bead
	readyStep := false
	for _, b := range all {
		if b.Metadata["gc.kind"] == "workflow" && b.Metadata["gc.routed_to"] == "polecat" {
			root = b
			continue
		}
		if b.ID != "BL-42" &&
			b.Metadata["gc.routed_to"] == "polecat" &&
			b.Metadata["gc.kind"] != "workflow-finalize" &&
			b.Status == "open" && b.Assignee == "" {
			readyStep = true
		}
	}
	if root.ID == "" {
		t.Fatalf("no routed workflow root found; beads=%#v", all)
	}
	if got := root.Status; got != "in_progress" {
		t.Fatalf("workflow root status = %q, want in_progress (routed root must be excluded from demand)", got)
	}
	if !readyStep {
		t.Fatalf("no open+routed+unassigned executable step to supply cold-pool demand; beads=%#v", all)
	}

	// The anti-stranding invariant: a cold pool sees demand even though the
	// routed root is in_progress — it comes from the routed, unassigned step.
	counts, partials, errs := defaultScaleCheckCounts([]defaultScaleCheckTarget{
		defaultScaleCheckTargetForAgent(sharedTestCityDir, cfg, &a, store, nil),
	})
	if len(errs) != 0 {
		t.Fatalf("defaultScaleCheckCounts errors: %v", errs)
	}
	if len(partials) != 0 {
		t.Fatalf("defaultScaleCheckCounts partials: %v", partials)
	}
	if got := counts["polecat"]; got != 1 {
		t.Fatalf("cold-pool demand after graph.v2 --on = %d, want 1 (work stranded if 0)", got)
	}
}
