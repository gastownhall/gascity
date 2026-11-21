package sling

import (
	"maps"
	"strings"
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

// slungBeforeFix leaves run in the state a pre-fix `gc sling worker <root>`
// left: the root routed to worker and tracked by its auto-convoy, the steps
// still unrouted. It returns the auto-convoy.
func (r *cookedRun) slungBeforeFix(t *testing.T, target string) beads.Bead {
	t.Helper()
	if err := r.store.SetMetadata(r.root.ID, beadmeta.RoutedToMetadataKey, target); err != nil {
		t.Fatalf("route root: %v", err)
	}
	convoy, err := r.store.Create(beads.Bead{Title: AutoConvoyRootTitle(r.root.ID), Type: "convoy", Status: "open"})
	if err != nil {
		t.Fatalf("create auto-convoy: %v", err)
	}
	if err := r.store.DepAdd(convoy.ID, r.root.ID, "tracks"); err != nil {
		t.Fatalf("track root: %v", err)
	}
	return convoy
}

// TestSlingExpandedRootReslingRepairsStuckRun is AC-3: re-slinging a run that
// a pre-fix sling left stuck is not an idempotent no-op; it routes the
// unrouted steps and reuses the root's one auto-convoy.
func TestSlingExpandedRootReslingRepairsStuckRun(t *testing.T) {
	run := newCookedRun(t)
	step := run.step(t, "mix batter")
	convoy := run.slungBeforeFix(t, "worker")
	worker := poolWorker()
	deps, _ := cookedRunDeps(run, worker)

	result, err := DoSling(testOpts(worker, run.root.ID), deps, run.store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.Idempotent {
		t.Fatal("re-sling of a stuck cooked run reported idempotent; its steps stay unrouted")
	}
	if got := run.get(t, step.ID).Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
		t.Errorf("step gc.routed_to = %q, want worker", got)
	}
	roots, err := liveAutoConvoyRoots(deps.Store, run.root.ID)
	if err != nil {
		t.Fatalf("liveAutoConvoyRoots: %v", err)
	}
	if len(roots) != 1 || roots[0].ID != convoy.ID {
		t.Errorf("live auto-convoys tracking the root = %v, want exactly %s", roots, convoy.ID)
	}
	if result.ConvoyID != convoy.ID {
		t.Errorf("ConvoyID = %q, want the existing %s", result.ConvoyID, convoy.ID)
	}
}

// metadataWriteRecorder records the bead IDs whose metadata is written.
type metadataWriteRecorder struct {
	*beads.MemStore
	written []string
}

func (r *metadataWriteRecorder) SetMetadata(id, key, value string) error {
	r.written = append(r.written, id)
	return r.MemStore.SetMetadata(id, key, value)
}

func (r *metadataWriteRecorder) SetMetadataBatch(id string, kvs map[string]string) error {
	r.written = append(r.written, id)
	return r.MemStore.SetMetadataBatch(id, kvs)
}

// TestSlingExpandedRootFullyRoutedReslingIsIdempotent is AC-4: once the root
// and every executable member are routed to the target, a re-sling is the
// idempotent no-op it always was and writes no member metadata.
func TestSlingExpandedRootFullyRoutedReslingIsIdempotent(t *testing.T) {
	run := newCookedRun(t)
	mix := run.member(t, "mix batter", map[string]string{beadmeta.RoutedToMetadataKey: "worker"})
	cook := run.member(t, "cook", map[string]string{beadmeta.RoutedToMetadataKey: "worker"})
	run.slungBeforeFix(t, "worker")
	recorder := &metadataWriteRecorder{MemStore: run.store}
	worker := poolWorker()
	deps, _ := cookedRunDeps(run, worker)
	deps = withStore(deps, recorder)
	deps.Router = &storeStampingRouter{fallback: recorder}

	result, err := DoSling(testOpts(worker, run.root.ID), deps, recorder)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if !result.Idempotent {
		t.Error("Idempotent = false, want true for a fully routed cooked run")
	}
	for _, id := range recorder.written {
		if id == mix.ID || id == cook.ID {
			t.Errorf("idempotent re-sling wrote metadata on member %s", id)
		}
	}
	if result.RoutedMembers != 0 {
		t.Errorf("RoutedMembers = %d, want 0", result.RoutedMembers)
	}
}

// TestSlingExpandedRootToAnotherTargetFillsOnlyUnroutedAndWarns is AC-5:
// slinging a run mostly routed to worker to other routes only the member with
// no route, leaves the worker members where they are, and names them in a
// warning.
func TestSlingExpandedRootToAnotherTargetFillsOnlyUnroutedAndWarns(t *testing.T) {
	run := newCookedRun(t)
	mix := run.member(t, "mix batter", map[string]string{beadmeta.RoutedToMetadataKey: "worker"})
	cook := run.member(t, "cook", map[string]string{beadmeta.RoutedToMetadataKey: "worker"})
	serve := run.step(t, "serve")
	run.slungBeforeFix(t, "worker")
	worker := poolWorker()
	other := config.Agent{Name: "other", MaxActiveSessions: intPtr(-1)}
	deps, _ := cookedRunDeps(run, worker, other)

	result, err := DoSling(testOpts(other, run.root.ID), deps, run.store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if got := run.get(t, serve.ID).Metadata[beadmeta.RoutedToMetadataKey]; got != "other" {
		t.Errorf("unrouted member gc.routed_to = %q, want other", got)
	}
	for _, id := range []string{mix.ID, cook.ID} {
		if got := run.get(t, id).Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
			t.Errorf("member %s gc.routed_to = %q, want it left on worker", id, got)
		}
	}
	var warning string
	for _, w := range result.BeadWarnings {
		if strings.Contains(w, mix.ID) {
			warning = w
		}
	}
	if warning == "" || !strings.Contains(warning, cook.ID) {
		t.Fatalf("BeadWarnings = %q, want one warning naming %s and %s", result.BeadWarnings, mix.ID, cook.ID)
	}
	if strings.Contains(warning, serve.ID) {
		t.Errorf("warning %q names %s, which this sling routed", warning, serve.ID)
	}
}

// TestSlingExpandedRootCustomSlingQueryWarnsAndWritesNoMembers is AC-6: a
// custom sling_query owns routing, so Go cannot know what routing the run's
// members means for it. The custom command runs, no member is written, and
// the result says the unrouted steps were not routed.
func TestSlingExpandedRootCustomSlingQueryWarnsAndWritesNoMembers(t *testing.T) {
	run := newCookedRun(t)
	mix := run.step(t, "mix batter")
	cook := run.step(t, "cook")
	custom := config.Agent{Name: "worker", MaxActiveSessions: intPtr(1), SlingQuery: "custom-dispatch {}"}
	deps, _ := cookedRunDeps(run, custom)
	runner := newFakeRunner()
	deps.Router = nil
	deps.Runner = runner.run
	before := map[string]map[string]string{
		mix.ID:  maps.Clone(run.get(t, mix.ID).Metadata),
		cook.ID: maps.Clone(run.get(t, cook.ID).Metadata),
	}

	result, err := DoSling(testOpts(custom, run.root.ID), deps, run.store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	want := "custom-dispatch '" + run.root.ID + "'"
	if len(runner.calls) != 1 || runner.calls[0] != want {
		t.Fatalf("runner calls = %#v, want [%q]", runner.calls, want)
	}
	for id, meta := range before {
		if got := run.get(t, id).Metadata; !maps.Equal(got, meta) {
			t.Errorf("member %s metadata = %v, want unchanged %v", id, got, meta)
		}
	}
	if result.RoutedMembers != 0 {
		t.Errorf("RoutedMembers = %d, want 0", result.RoutedMembers)
	}
	var warned bool
	for _, w := range result.BeadWarnings {
		if strings.Contains(w, run.root.ID) && strings.Contains(w, "2 unrouted") && strings.Contains(w, "sling_query") {
			warned = true
		}
	}
	if !warned {
		t.Fatalf("BeadWarnings = %q, want one naming %s, its 2 unrouted steps and the custom sling_query", result.BeadWarnings, run.root.ID)
	}
}

// TestSlingNonExpandedBeadsUnchanged is AC-7: a bead that is not an expanded
// workflow root (a plain task, or a root-only workflow bead) routes only
// itself, even when other beads name it as their root.
func TestSlingNonExpandedBeadsUnchanged(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]string
	}{
		{name: "plain task", meta: map[string]string{}},
		{name: "root-only workflow", meta: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			slung, err := store.Create(beads.Bead{Title: "slung", Type: "task", Status: "open", Metadata: tc.meta})
			if err != nil {
				t.Fatalf("create slung bead: %v", err)
			}
			other, err := store.Create(beads.Bead{Title: "other", Type: "task", Status: "open", ParentID: slung.ID, Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: slung.ID,
				beadmeta.RoutedToMetadataKey:   "",
			}})
			if err != nil {
				t.Fatalf("create other bead: %v", err)
			}
			run := &cookedRun{store: store, root: slung}
			worker := poolWorker()
			deps, router := cookedRunDeps(run, worker)

			result, err := DoSling(testOpts(worker, slung.ID), deps, store)
			if err != nil {
				t.Fatalf("DoSling: %v", err)
			}
			if len(router.routed) != 1 || router.routed[0].BeadID != slung.ID {
				t.Fatalf("routed = %+v, want only %s", router.routed, slung.ID)
			}
			if got := run.get(t, slung.ID).Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
				t.Errorf("slung gc.routed_to = %q, want worker", got)
			}
			if got := run.get(t, other.ID).Metadata; !maps.Equal(got, other.Metadata) {
				t.Errorf("other bead metadata = %v, want unchanged %v", got, other.Metadata)
			}
			if result.RoutedMembers != 0 || len(result.BeadWarnings) != 0 {
				t.Errorf("RoutedMembers = %d, BeadWarnings = %q, want 0 and none", result.RoutedMembers, result.BeadWarnings)
			}
		})
	}
}

// TestSlingExpandedRootRefusesInProgressInstantiation is AC-8: a run whose
// root or a member still carries gc.instantiating=true is not finished being
// created, so slinging it fails naming the instantiation and writes nothing,
// the root included.
func TestSlingExpandedRootRefusesInProgressInstantiation(t *testing.T) {
	for _, flagged := range []string{"root", "member"} {
		t.Run(flagged, func(t *testing.T) {
			run := newCookedRun(t)
			mix := run.step(t, "mix batter")
			flaggedID := mix.ID
			if flagged == "root" {
				flaggedID = run.root.ID
			}
			if err := run.store.SetMetadata(flaggedID, beadmeta.InstantiatingMetadataKey, "true"); err != nil {
				t.Fatalf("flag %s instantiating: %v", flaggedID, err)
			}
			worker := poolWorker()
			deps, router := cookedRunDeps(run, worker)
			opts := testOpts(worker, run.root.ID)
			opts.Initiator = "human"
			before := map[string]map[string]string{
				run.root.ID: maps.Clone(run.get(t, run.root.ID).Metadata),
				mix.ID:      maps.Clone(run.get(t, mix.ID).Metadata),
			}

			_, err := DoSling(opts, deps, run.store)
			if err == nil {
				t.Fatal("DoSling succeeded, want an error naming the in-progress instantiation")
			}
			if !strings.Contains(err.Error(), "instantiat") || !strings.Contains(err.Error(), flaggedID) {
				t.Errorf("error = %q, want it to name the in-progress instantiation of %s", err, flaggedID)
			}
			if len(router.routed) != 0 {
				t.Errorf("routed = %+v, want nothing routed", router.routed)
			}
			for id, meta := range before {
				if got := run.get(t, id).Metadata; !maps.Equal(got, meta) {
					t.Errorf("bead %s metadata = %v, want unchanged %v", id, got, meta)
				}
			}
		})
	}
}
