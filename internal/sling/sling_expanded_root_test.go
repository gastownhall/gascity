package sling

import (
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/runtime"
)

// recordingRouter stamps gc.routed_to the way the CLI's built-in router does
// (cmd/gc's cliBeadRouter) and records which beads went through it, so tests
// can tell the router's own write apart from the member-routing plan's.
type recordingRouter struct {
	store  beads.Store
	routed []string
}

func (r *recordingRouter) Route(_ context.Context, req RouteRequest) error {
	if err := r.store.SetMetadata(req.BeadID, beadmeta.RoutedToMetadataKey, req.Target); err != nil {
		return err
	}
	r.routed = append(r.routed, req.BeadID)
	return nil
}

// cookedWorkflowStore seeds the store with what `gc formula cook` leaves
// behind: an expanded workflow root, its still-unrouted step beads, and a
// control member carrying its own dispatcher route but no execution route.
func cookedWorkflowStore(t *testing.T) *beads.MemStore {
	t.Helper()
	root := beads.Bead{
		ID: "wf-1", Title: "cook pancakes", Type: "task", Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:             beadmeta.KindWorkflow,
			beadmeta.WorkflowExpandedMetadataKey: "true",
		},
	}
	steps := []beads.Bead{
		{
			ID: "wf-1.mix", Title: "mix the batter", Type: "task", Status: "open",
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
		},
		{
			ID: "wf-1.flip", Title: "flip the pancake", Type: "task", Status: "open",
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
		},
		// A control member: cook stamps its own route (the control
		// dispatcher) and leaves its execution route empty.
		{
			ID: "wf-1.finalize", Title: "finalize the run", Type: "task", Status: "open",
			Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey: root.ID,
				beadmeta.KindMetadataKey:       beadmeta.KindWorkflowFinalize,
				beadmeta.RoutedToMetadataKey:   "core.control-dispatcher",
			},
		},
	}
	return beads.NewMemStoreFrom(0, append([]beads.Bead{root}, steps...), nil)
}

func TestSlingExpandedRootRoutesMembers(t *testing.T) {
	store := cookedWorkflowStore(t)
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker}}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps = withStore(deps, store)
	router := &recordingRouter{store: store}
	deps.Router = router

	result, err := DoSling(testOpts(worker, "wf-1"), deps, nil)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.RoutedMembers != 3 {
		t.Fatalf("RoutedMembers = %d, want 3 (both unrouted steps and the control's execution route)", result.RoutedMembers)
	}
	for _, id := range []string{"wf-1", "wf-1.mix", "wf-1.flip"} {
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("reading %s after the sling: %v", id, err)
		}
		if got := bead.Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
			t.Errorf("%s gc.routed_to = %q, want worker", id, got)
		}
	}
	// The control keeps its dispatcher route and gains the execution route a
	// --formula launch would have stamped.
	control, err := store.Get("wf-1.finalize")
	if err != nil {
		t.Fatalf("reading the control after the sling: %v", err)
	}
	if got := control.Metadata[beadmeta.RoutedToMetadataKey]; got != "core.control-dispatcher" {
		t.Errorf("control gc.routed_to = %q, want core.control-dispatcher (untouched)", got)
	}
	if got := control.Metadata[beadmeta.ExecutionRoutedToMetadataKey]; got != "worker" {
		t.Errorf("control gc.execution_routed_to = %q, want worker", got)
	}
	if len(router.routed) != 1 || router.routed[0] != "wf-1" {
		t.Errorf("router routed %v, want only the root wf-1 (the plan stamps the members)", router.routed)
	}
}

// TestSlingExpandedRootReSlingCompletesHalfRoutedRun is the upgrade path of
// #9039: a pre-fix sling routed the cooked root and minted its auto-convoy
// but left every member unrouted. Root routed plus a live tracking convoy is
// exactly the state the idempotent short-circuit answers "already routed"
// to, so the re-sling must notice the run is still half-routed and complete
// the member routing rather than dropping the request.
func TestSlingExpandedRootReSlingCompletesHalfRoutedRun(t *testing.T) {
	store := cookedWorkflowStore(t)
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker}}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps = withStore(deps, store)
	deps.Router = &recordingRouter{store: store}

	// The stuck shape an old binary leaves: root routed, auto-convoy minted,
	// members still carrying no route.
	if err := store.SetMetadata("wf-1", beadmeta.RoutedToMetadataKey, "worker"); err != nil {
		t.Fatalf("pre-route the root: %v", err)
	}
	convoy, err := deps.Store.Create(beads.Bead{Title: AutoConvoyRootTitle("wf-1"), Type: "convoy"})
	if err != nil {
		t.Fatalf("mint the old auto-convoy: %v", err)
	}
	if err := convoycore.TrackItemIn(convoycore.MemberClasses{Convoy: deps.Store}, convoy.ID, "wf-1"); err != nil {
		t.Fatalf("track the root in the old auto-convoy: %v", err)
	}

	// The live CLI passes the work store as the querier (Dispatch →
	// DoSlingBatch → DoSling); a nil querier would skip the idempotent
	// short-circuit this test exists to exercise.
	result, err := DoSling(testOpts(worker, "wf-1"), deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.Idempotent {
		t.Fatal("re-sling of a half-routed run short-circuited as idempotent: the unrouted members would never wake a worker")
	}
	if result.RoutedMembers != 3 {
		t.Fatalf("RoutedMembers = %d, want 3 (the members the first sling never routed)", result.RoutedMembers)
	}
	for _, id := range []string{"wf-1.mix", "wf-1.flip"} {
		bead, err := store.Get(id)
		if err != nil {
			t.Fatalf("reading %s after the re-sling: %v", id, err)
		}
		if got := bead.Metadata[beadmeta.RoutedToMetadataKey]; got != "worker" {
			t.Errorf("%s gc.routed_to = %q, want worker", id, got)
		}
	}
	control, err := store.Get("wf-1.finalize")
	if err != nil {
		t.Fatalf("reading the control after the re-sling: %v", err)
	}
	if got := control.Metadata[beadmeta.ExecutionRoutedToMetadataKey]; got != "worker" {
		t.Errorf("control gc.execution_routed_to = %q, want worker", got)
	}
	// The re-sling must reuse the live auto-convoy, not mint a second one.
	live, err := liveAutoConvoyRoots(deps.Store, "wf-1")
	if err != nil {
		t.Fatalf("liveAutoConvoyRoots: %v", err)
	}
	if len(live) != 1 || live[0].ID != convoy.ID {
		t.Fatalf("auto-convoy roots after the re-sling = %+v, want exactly the reused %s", live, convoy.ID)
	}
}

// TestSlingExpandedRootFullyRoutedStaysIdempotent pins the settled half of
// the re-sling story: once every member of the run carries the route (steps
// via gc.routed_to, the control via gc.execution_routed_to), the plain sling
// is a no-op again — idempotent, nothing written, no second auto-convoy. The
// member-routing refinement must fill missing routes, never rewrite settled
// ones.
func TestSlingExpandedRootFullyRoutedStaysIdempotent(t *testing.T) {
	store := cookedWorkflowStore(t)
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker}}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps = withStore(deps, store)
	router := &recordingRouter{store: store}
	deps.Router = router

	// The settled shape a completed sling leaves: root routed, live convoy,
	// every step routed, the control carrying its execution route.
	for id, meta := range map[string]map[string]string{
		"wf-1":          {beadmeta.RoutedToMetadataKey: "worker"},
		"wf-1.mix":      {beadmeta.RoutedToMetadataKey: "worker"},
		"wf-1.flip":     {beadmeta.RoutedToMetadataKey: "worker"},
		"wf-1.finalize": {beadmeta.ExecutionRoutedToMetadataKey: "worker"},
	} {
		for k, v := range meta {
			if err := store.SetMetadata(id, k, v); err != nil {
				t.Fatalf("settling %s: %v", id, err)
			}
		}
	}
	convoy, err := deps.Store.Create(beads.Bead{Title: AutoConvoyRootTitle("wf-1"), Type: "convoy"})
	if err != nil {
		t.Fatalf("mint the auto-convoy: %v", err)
	}
	if err := convoycore.TrackItemIn(convoycore.MemberClasses{Convoy: deps.Store}, convoy.ID, "wf-1"); err != nil {
		t.Fatalf("track the root in the auto-convoy: %v", err)
	}

	result, err := DoSling(testOpts(worker, "wf-1"), deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if !result.Idempotent {
		t.Fatal("a fully routed run is not idempotent: the sling would rewrite settled routes")
	}
	if result.RoutedMembers != 0 {
		t.Fatalf("RoutedMembers = %d, want 0 on the settled run", result.RoutedMembers)
	}
	if len(router.routed) != 0 {
		t.Errorf("router routed %v, want nothing on the idempotent return", router.routed)
	}
	live, err := liveAutoConvoyRoots(deps.Store, "wf-1")
	if err != nil {
		t.Fatalf("liveAutoConvoyRoots: %v", err)
	}
	if len(live) != 1 || live[0].ID != convoy.ID {
		t.Fatalf("auto-convoy roots = %+v, want exactly the existing %s", live, convoy.ID)
	}
}

// TestSlingExpandedRootWarnsOtherRoutedMembers pins the never-move rule's
// observability: members already routed to a different agent keep their
// routes when the root is slung to a new target, and the sling says so in one
// bead warning naming each member and the agent it stays with.
func TestSlingExpandedRootWarnsOtherRoutedMembers(t *testing.T) {
	store := cookedWorkflowStore(t)
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	other := config.Agent{Name: "other", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker, other}}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps = withStore(deps, store)
	deps.Router = &recordingRouter{store: store}

	// Half the run already runs on worker: one step routed, the control
	// execution-routed. wf-1.mix carries no route.
	if err := store.SetMetadata("wf-1.flip", beadmeta.RoutedToMetadataKey, "worker"); err != nil {
		t.Fatalf("pre-route wf-1.flip: %v", err)
	}
	if err := store.SetMetadata("wf-1.finalize", beadmeta.ExecutionRoutedToMetadataKey, "worker"); err != nil {
		t.Fatalf("pre-route the control's execution: %v", err)
	}

	result, err := DoSling(testOpts(other, "wf-1"), deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.Idempotent {
		t.Fatal("sling of a partly unrouted run came back idempotent; nothing would be routed")
	}
	// Only the unrouted member takes the new target's route.
	if result.RoutedMembers != 1 {
		t.Fatalf("RoutedMembers = %d, want 1 (only the unrouted wf-1.mix)", result.RoutedMembers)
	}
	if got := mustMeta(t, store, "wf-1.mix", beadmeta.RoutedToMetadataKey); got != "other" {
		t.Errorf("wf-1.mix gc.routed_to = %q, want other", got)
	}
	// The worker-routed members are left exactly where they were.
	if got := mustMeta(t, store, "wf-1.flip", beadmeta.RoutedToMetadataKey); got != "worker" {
		t.Errorf("wf-1.flip gc.routed_to = %q, want worker (in-flight work never moves)", got)
	}
	if got := mustMeta(t, store, "wf-1.finalize", beadmeta.ExecutionRoutedToMetadataKey); got != "worker" {
		t.Errorf("control gc.execution_routed_to = %q, want worker (in-flight work never moves)", got)
	}
	if got := mustMeta(t, store, "wf-1.finalize", beadmeta.RoutedToMetadataKey); got != "core.control-dispatcher" {
		t.Errorf("control gc.routed_to = %q, want core.control-dispatcher (untouched)", got)
	}
	// One warning names both members that stayed with worker.
	if len(result.BeadWarnings) != 1 {
		t.Fatalf("BeadWarnings = %q, want exactly one other-routed warning", result.BeadWarnings)
	}
	for _, want := range []string{"wf-1.flip", "wf-1.finalize", "worker"} {
		if !strings.Contains(result.BeadWarnings[0], want) {
			t.Errorf("warning %q does not name %q", result.BeadWarnings[0], want)
		}
	}
}

func mustMeta(t *testing.T, store *beads.MemStore, id, key string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatalf("reading %s: %v", id, err)
	}
	return b.Metadata[key]
}
