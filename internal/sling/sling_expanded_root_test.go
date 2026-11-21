package sling

import (
	"maps"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// cookedRun is the shape `gc formula cook` leaves in a store: an expanded
// graph.v2 workflow root and member beads carrying gc.root_bead_id.
type cookedRun struct {
	store *beads.MemStore
	root  beads.Bead
}

func newCookedRun(t *testing.T) *cookedRun {
	t.Helper()
	store := beads.NewMemStore()
	root, err := store.Create(beads.Bead{
		Title:  "pancakes",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.WorkflowExpandedMetadataKey: "true",
			beadmeta.FormulaContractMetadataKey:  beadmeta.FormulaContractGraphV2,
			beadmeta.ScopeKindMetadataKey:        "formula-cook",
			beadmeta.RoutedToMetadataKey:         "",
		},
	})
	if err != nil {
		t.Fatalf("create cooked root: %v", err)
	}
	return &cookedRun{store: store, root: root}
}

// member creates a run member with meta on top of its gc.root_bead_id.
func (r *cookedRun) member(t *testing.T, title string, meta map[string]string) beads.Bead {
	t.Helper()
	full := map[string]string{beadmeta.RootBeadIDMetadataKey: r.root.ID}
	for k, v := range meta {
		full[k] = v
	}
	b, err := r.store.Create(beads.Bead{Title: title, Type: "task", Status: "open", ParentID: r.root.ID, Metadata: full})
	if err != nil {
		t.Fatalf("create member %q: %v", title, err)
	}
	return b
}

// step creates an unrouted executable step, the shape cook leaves.
func (r *cookedRun) step(t *testing.T, title string) beads.Bead {
	t.Helper()
	return r.member(t, title, map[string]string{beadmeta.RoutedToMetadataKey: ""})
}

func (r *cookedRun) get(t *testing.T, id string) beads.Bead {
	t.Helper()
	b, err := r.store.Get(id)
	if err != nil {
		t.Fatalf("get %s: %v", id, err)
	}
	return b
}

func poolWorker() config.Agent {
	return config.Agent{Name: "worker", MinActiveSessions: intPtr(0), MaxActiveSessions: intPtr(-1)}
}

// cookedRunDeps wires deps over the run's store with a built-in router that
// stamps gc.routed_to the way cmd/gc and the API do.
func cookedRunDeps(r *cookedRun, agents ...config.Agent) (SlingDeps, *storeStampingRouter) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: agents}
	deps := withStore(testDeps(cfg, runtime.NewFake(), newFakeRunner().run), r.store)
	router := &storeStampingRouter{fallback: r.store}
	deps.Router = router
	return deps, router
}

// TestSlingExpandedRootRoutesUnroutedSteps is AC-1 at the core: slinging a
// cooked root to an on-demand pool routes every open unrouted executable step
// to the target, so the pool sees demand even though the root never counts.
func TestSlingExpandedRootRoutesUnroutedSteps(t *testing.T) {
	run := newCookedRun(t)
	mix := run.step(t, "mix batter")
	cook := run.step(t, "cook")
	worker := poolWorker()
	deps, _ := cookedRunDeps(run, worker)

	result, err := DoSling(testOpts(worker, run.root.ID), deps, run.store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	for _, id := range []string{mix.ID, cook.ID} {
		if got := run.get(t, id).Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
			t.Errorf("step %s gc.routed_to = %q, want worker", id, got)
		}
	}
	if got := run.get(t, run.root.ID).Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
		t.Errorf("root gc.routed_to = %q, want worker", got)
	}
	if result.RoutedMembers != 2 {
		t.Errorf("RoutedMembers = %d, want 2", result.RoutedMembers)
	}
	if result.Idempotent {
		t.Error("Idempotent = true, want false")
	}
}

// TestSlingExpandedRootStampsSessionNameForSingleSessionTarget: a
// single-session target gets its session back-reference on each routed step,
// as a --formula launch stamps it.
func TestSlingExpandedRootStampsSessionNameForSingleSessionTarget(t *testing.T) {
	run := newCookedRun(t)
	step := run.step(t, "mix batter")
	solo := config.Agent{Name: "solo", MaxActiveSessions: intPtr(1)}
	deps, _ := cookedRunDeps(run, solo)

	if _, err := DoSling(testOpts(solo, run.root.ID), deps, run.store); err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	got := run.get(t, step.ID)
	if got.Metadata[beadmeta.RoutedToMetadataKey] != "solo" {
		t.Errorf("gc.routed_to = %q, want solo", got.Metadata[beadmeta.RoutedToMetadataKey])
	}
	if got.Metadata[beadmeta.SessionNameMetadataKey] == "" {
		t.Error("gc.session_name is empty, want the single-session target's session name")
	}
}

// TestSlingExpandedRootDryRunWritesNothing: --dry-run previews without
// touching any member.
func TestSlingExpandedRootDryRunWritesNothing(t *testing.T) {
	run := newCookedRun(t)
	step := run.step(t, "mix batter")
	worker := poolWorker()
	deps, router := cookedRunDeps(run, worker)
	opts := testOpts(worker, run.root.ID)
	opts.DryRun = true

	if _, err := DoSling(opts, deps, run.store); err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if got := run.get(t, step.ID).Metadata[beadmeta.RoutedToMetadataKey]; got != "" {
		t.Errorf("dry-run routed step to %q", got)
	}
	if len(router.routed) != 0 {
		t.Errorf("dry-run routed %d beads", len(router.routed))
	}
}

// TestSlingExpandedRootRoutesControlExecutionAndSkipsSettledMembers is AC-2:
// a control keeps its control-dispatcher route and gains the target as its
// execution route, while a scope latch, a spec sidecar, a step routed to
// another agent, a closed step and an assigned step are left untouched.
func TestSlingExpandedRootRoutesControlExecutionAndSkipsSettledMembers(t *testing.T) {
	run := newCookedRun(t)
	control := run.member(t, "check", map[string]string{
		beadmeta.KindMetadataKey:     beadmeta.KindCheck,
		beadmeta.RoutedToMetadataKey: config.ControlDispatcherAgentName,
	})
	scope := run.member(t, "scope", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScope})
	spec := run.member(t, "spec", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindSpec})
	explicit := run.member(t, "review", map[string]string{beadmeta.RoutedToMetadataKey: "reviewer"})
	closed := run.step(t, "already done")
	if err := run.store.Close(closed.ID); err != nil {
		t.Fatalf("close step: %v", err)
	}
	assigned := run.step(t, "in hand")
	claimer := "someone"
	if err := run.store.Update(assigned.ID, beads.UpdateOpts{Assignee: &claimer}); err != nil {
		t.Fatalf("assign step: %v", err)
	}
	open := run.step(t, "mix batter")

	untouched := map[string]beads.Bead{}
	for _, b := range []beads.Bead{scope, spec, explicit, closed, assigned} {
		untouched[b.ID] = run.get(t, b.ID)
	}
	worker := poolWorker()
	reviewer := config.Agent{Name: "reviewer", MaxActiveSessions: intPtr(-1)}
	deps, _ := cookedRunDeps(run, worker, reviewer)

	result, err := DoSling(testOpts(worker, run.root.ID), deps, run.store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}

	gotControl := run.get(t, control.ID)
	if got := gotControl.Metadata[beadmeta.RoutedToMetadataKey]; got != config.ControlDispatcherAgentName {
		t.Errorf("control gc.routed_to = %q, want %q", got, config.ControlDispatcherAgentName)
	}
	if got := gotControl.Metadata[beadmeta.ExecutionRoutedToMetadataKey]; got != "worker" {
		t.Errorf("control gc.execution_routed_to = %q, want worker", got)
	}
	if got := run.get(t, open.ID).Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
		t.Errorf("open step gc.routed_to = %q, want worker", got)
	}
	for id, before := range untouched {
		after := run.get(t, id)
		if !maps.Equal(before.Metadata, after.Metadata) {
			t.Errorf("member %s (%s) metadata changed:\n before %v\n after  %v", id, before.Title, before.Metadata, after.Metadata)
		}
	}
	if result.RoutedMembers != 2 {
		t.Errorf("RoutedMembers = %d, want 2 (the control and the open step)", result.RoutedMembers)
	}
}
