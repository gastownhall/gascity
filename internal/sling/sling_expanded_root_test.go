package sling

import (
	"context"
	"fmt"
	"maps"
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
	// Record every metadata write from here on, so the idempotent return is
	// proven to write nothing on the members.
	recorder := &metadataWriteRecorder{MemStore: store}
	deps = withStore(deps, recorder)
	router.store = recorder

	result, err := DoSling(testOpts(worker, "wf-1"), deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if !result.Idempotent {
		t.Fatal("a fully routed run is not idempotent: the sling would rewrite settled routes")
	}
	for _, id := range recorder.written {
		if id != "wf-1" {
			t.Errorf("idempotent re-sling wrote metadata on member %s", id)
		}
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

// metadataWriteRecorder records the bead ID of every metadata write that
// reaches the store, fenced (UpdateIfMatch) or not.
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

func (r *metadataWriteRecorder) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	r.written = append(r.written, id)
	return r.MemStore.UpdateIfMatch(id, expectedRevision, opts)
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

// TestSlingExpandedRootCustomQueryLeavesMembersToTheCommand pins that member
// routing is a built-in feature only: when the target routes through its own
// sling_query command (or no typed router is wired at all), the command
// receives the root alone and the plan stamps nothing on the members — it
// counts them and warns, so the operator knows the run's steps did not move.
func TestSlingExpandedRootCustomQueryLeavesMembersToTheCommand(t *testing.T) {
	store := cookedWorkflowStore(t)
	worker := config.Agent{
		Name: "worker", MaxActiveSessions: intPtr(-1),
		SlingQuery: "custom-dispatch {}",
	}
	cfg := &config.City{Agents: []config.Agent{worker}}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps = withStore(deps, store)
	// No typed router: the agent's own command routes the root, exactly as a
	// caller without built-in routing wires it.
	deps.Router = nil

	result, err := DoSling(testOpts(worker, "wf-1"), deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.Idempotent {
		t.Fatal("sling of an unrouted run came back idempotent; the root would never be routed")
	}
	if result.RoutedMembers != 0 {
		t.Fatalf("RoutedMembers = %d, want 0: the custom command owns routing", result.RoutedMembers)
	}
	// The command routed the root…
	if len(runner.calls) == 0 {
		t.Fatal("the custom sling_query never ran; the root was not routed")
	}
	// …and only the root: every member keeps exactly the metadata cook left.
	for _, id := range []string{"wf-1.mix", "wf-1.flip"} {
		if got := mustMeta(t, store, id, beadmeta.RoutedToMetadataKey); got != "" {
			t.Errorf("%s gc.routed_to = %q, want empty: the plan must not stamp members on a custom query", id, got)
		}
	}
	if got := mustMeta(t, store, "wf-1.finalize", beadmeta.ExecutionRoutedToMetadataKey); got != "" {
		t.Errorf("control gc.execution_routed_to = %q, want empty: the plan must not stamp members on a custom query", got)
	}
	if got := mustMeta(t, store, "wf-1.finalize", beadmeta.RoutedToMetadataKey); got != "core.control-dispatcher" {
		t.Errorf("control gc.routed_to = %q, want core.control-dispatcher (untouched)", got)
	}
	// One warning names how many members were left for the command's world.
	if len(result.BeadWarnings) != 1 {
		t.Fatalf("BeadWarnings = %q, want exactly one unrouted-members warning", result.BeadWarnings)
	}
	for _, want := range []string{"3", "wf-1"} {
		if !strings.Contains(result.BeadWarnings[0], want) {
			t.Errorf("warning %q does not mention %q", result.BeadWarnings[0], want)
		}
	}
}

// TestSlingMemberRoutingSkipsNonExpandedRoots pins the scope of member
// routing: it exists only for expanded graph.v2 workflow roots. A plain task
// and a root-only workflow bead (gc.kind=workflow without
// gc.workflow_expanded — the v1 materialization shape) route exactly as
// slings always have: the bead's own metadata changes and nothing else's,
// members or no members.
func TestSlingMemberRoutingSkipsNonExpandedRoots(t *testing.T) {
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker}}
	runner := newFakeRunner()

	t.Run("plain task", func(t *testing.T) {
		store := beads.NewMemStoreFrom(0, []beads.Bead{
			{ID: "task-1", Title: "a plain task", Type: "task", Status: "open", Metadata: map[string]string{}},
		}, nil)
		deps := testDeps(cfg, runtime.NewFake(), runner.run)
		deps = withStore(deps, store)
		deps.Router = &recordingRouter{store: store}

		result, err := DoSling(testOpts(worker, "task-1"), deps, deps.Store)
		if err != nil {
			t.Fatalf("DoSling: %v", err)
		}
		if result.RoutedMembers != 0 {
			t.Fatalf("RoutedMembers = %d, want 0: a plain task has no members to route", result.RoutedMembers)
		}
		if got := mustMeta(t, store, "task-1", beadmeta.RoutedToMetadataKey); got != "worker" {
			t.Fatalf("task-1 gc.routed_to = %q, want worker", got)
		}
	})

	t.Run("root-only workflow with members", func(t *testing.T) {
		// The v1 shape: a workflow root that never expanded, carrying child
		// beads through gc.root_bead_id. Member routing must not adopt them.
		root := beads.Bead{
			ID: "mol-1", Title: "v1 molecule root", Type: "task", Status: "open",
			Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow},
		}
		child := beads.Bead{
			ID: "mol-1.step", Title: "a v1 step", Type: "task", Status: "open",
			Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "mol-1"},
		}
		store := beads.NewMemStoreFrom(0, []beads.Bead{root, child}, nil)
		deps := testDeps(cfg, runtime.NewFake(), runner.run)
		deps = withStore(deps, store)
		deps.Router = &recordingRouter{store: store}

		result, err := DoSling(testOpts(worker, "mol-1"), deps, deps.Store)
		if err != nil {
			t.Fatalf("DoSling: %v", err)
		}
		if result.RoutedMembers != 0 {
			t.Fatalf("RoutedMembers = %d, want 0: a root-only workflow is not an expanded run", result.RoutedMembers)
		}
		if got := mustMeta(t, store, "mol-1", beadmeta.RoutedToMetadataKey); got != "worker" {
			t.Fatalf("mol-1 gc.routed_to = %q, want worker (its own route is untouched business)", got)
		}
		if got := mustMeta(t, store, "mol-1.step", beadmeta.RoutedToMetadataKey); got != "" {
			t.Fatalf("mol-1.step gc.routed_to = %q, want empty: v1 members are not member routing's to route", got)
		}
		if len(result.BeadWarnings) != 0 {
			t.Fatalf("BeadWarnings = %q, want none: nothing was left behind", result.BeadWarnings)
		}
	})
}

// TestSlingExpandedRootRefusesMidInstantiation pins the latch guard: a run
// still carrying gc.instantiating (sequential fallback wiring its dependency
// graph) must not be member-routed out from under the instantiation. The
// sling fails naming the latched bead and writes nothing — not the members,
// not even the root's own route.
func TestSlingExpandedRootRefusesMidInstantiation(t *testing.T) {
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker}}
	runner := newFakeRunner()

	for name, latched := range map[string]string{
		"latched root":    "wf-1",
		"latched member":  "wf-1.flip",
		"latched control": "wf-1.finalize",
	} {
		t.Run(name, func(t *testing.T) {
			store := cookedWorkflowStore(t)
			if err := store.SetMetadata(latched, beadmeta.InstantiatingMetadataKey, "true"); err != nil {
				t.Fatalf("latching %s: %v", latched, err)
			}
			deps := testDeps(cfg, runtime.NewFake(), runner.run)
			deps = withStore(deps, store)
			router := &recordingRouter{store: store}
			deps.Router = router
			// An initiator makes the sling stamp the root before routing, so
			// the refusal must also prove that stamp never lands.
			opts := testOpts(worker, "wf-1")
			opts.Initiator = "human"
			before := map[string]map[string]string{}
			for _, id := range []string{"wf-1", "wf-1.mix", "wf-1.flip", "wf-1.finalize"} {
				b, err := store.Get(id)
				if err != nil {
					t.Fatalf("reading %s before the sling: %v", id, err)
				}
				before[id] = maps.Clone(b.Metadata)
			}

			_, err := DoSling(opts, deps, deps.Store)
			if err == nil {
				t.Fatal("sling of a mid-instantiation run succeeded; it would race the instantiation")
			}
			for _, want := range []string{latched, "gc.instantiating"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error %q does not name %q", err.Error(), want)
				}
			}
			// Nothing was written: every bead keeps exactly what it carried,
			// the root's initiator stamp included.
			for id, meta := range before {
				after, err := store.Get(id)
				if err != nil {
					t.Fatalf("reading %s after the refusal: %v", id, err)
				}
				if !maps.Equal(meta, after.Metadata) {
					t.Errorf("%s metadata = %v, want unchanged %v (no writes on refusal)", id, after.Metadata, meta)
				}
			}
			if len(router.routed) != 0 {
				t.Errorf("router routed %v, want nothing on refusal", router.routed)
			}
		})
	}
}

// TestSlingExpandedRootDryRunWritesNothing: --dry-run previews the sling of a
// cooked root without touching the root or any member.
func TestSlingExpandedRootDryRunWritesNothing(t *testing.T) {
	store := cookedWorkflowStore(t)
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker}}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps = withStore(deps, store)
	router := &recordingRouter{store: store}
	deps.Router = router
	opts := testOpts(worker, "wf-1")
	opts.DryRun = true

	if _, err := DoSling(opts, deps, deps.Store); err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	for _, id := range []string{"wf-1", "wf-1.mix", "wf-1.flip"} {
		if got := mustMeta(t, store, id, beadmeta.RoutedToMetadataKey); got != "" {
			t.Errorf("dry-run stamped %s gc.routed_to = %q, want empty", id, got)
		}
	}
	if got := mustMeta(t, store, "wf-1.finalize", beadmeta.ExecutionRoutedToMetadataKey); got != "" {
		t.Errorf("dry-run stamped control gc.execution_routed_to = %q, want empty", got)
	}
	if len(router.routed) != 0 {
		t.Errorf("dry-run routed %v, want nothing", router.routed)
	}
}

// TestSlingExpandedRootStampsSessionNameForSingleSessionTarget: a
// single-session target gets its session back-reference on each routed step,
// as a --formula launch stamps it.
func TestSlingExpandedRootStampsSessionNameForSingleSessionTarget(t *testing.T) {
	store := cookedWorkflowStore(t)
	solo := config.Agent{Name: "solo", MaxActiveSessions: intPtr(1)}
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}, Agents: []config.Agent{solo}}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps = withStore(deps, store)
	deps.Router = &recordingRouter{store: store}

	if _, err := DoSling(testOpts(solo, "wf-1"), deps, deps.Store); err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	for _, id := range []string{"wf-1.mix", "wf-1.flip"} {
		if got := mustMeta(t, store, id, beadmeta.RoutedToMetadataKey); got != "solo" {
			t.Errorf("%s gc.routed_to = %q, want solo", id, got)
		}
		if got := mustMeta(t, store, id, beadmeta.SessionNameMetadataKey); got == "" {
			t.Errorf("%s gc.session_name is empty, want the single-session target's session name", id)
		}
	}
}

// TestSlingExpandedRootRoutesControlExecutionAndSkipsSettledMembers: a
// control keeps its control-dispatcher route and gains the target as its
// execution route, while a scope latch, a spec sidecar, a step routed to
// another agent, a closed step and an assigned step are left untouched.
func TestSlingExpandedRootRoutesControlExecutionAndSkipsSettledMembers(t *testing.T) {
	store := cookedWorkflowStore(t)
	member := func(title string, meta map[string]string) beads.Bead {
		t.Helper()
		full := map[string]string{beadmeta.RootBeadIDMetadataKey: "wf-1"}
		maps.Copy(full, meta)
		b, err := store.Create(beads.Bead{Title: title, Type: "task", Status: "open", Metadata: full})
		if err != nil {
			t.Fatalf("create member %q: %v", title, err)
		}
		return b
	}
	scope := member("scope", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindScope})
	spec := member("spec", map[string]string{beadmeta.KindMetadataKey: beadmeta.KindSpec})
	explicit := member("review", map[string]string{beadmeta.RoutedToMetadataKey: "reviewer"})
	closed := member("already done", nil)
	if err := store.Close(closed.ID); err != nil {
		t.Fatalf("close step: %v", err)
	}
	assigned := member("in hand", nil)
	claimer := "someone"
	if err := store.Update(assigned.ID, beads.UpdateOpts{Assignee: &claimer}); err != nil {
		t.Fatalf("assign step: %v", err)
	}
	untouched := map[string]map[string]string{}
	for _, b := range []beads.Bead{scope, spec, explicit, closed, assigned} {
		got, err := store.Get(b.ID)
		if err != nil {
			t.Fatalf("reading %s before the sling: %v", b.ID, err)
		}
		untouched[b.ID] = maps.Clone(got.Metadata)
	}

	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	reviewer := config.Agent{Name: "reviewer", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker, reviewer}}
	runner := newFakeRunner()
	deps := testDeps(cfg, runtime.NewFake(), runner.run)
	deps = withStore(deps, store)
	deps.Router = &recordingRouter{store: store}

	result, err := DoSling(testOpts(worker, "wf-1"), deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if got := mustMeta(t, store, "wf-1.finalize", beadmeta.RoutedToMetadataKey); got != "core.control-dispatcher" {
		t.Errorf("control gc.routed_to = %q, want core.control-dispatcher (untouched)", got)
	}
	if got := mustMeta(t, store, "wf-1.finalize", beadmeta.ExecutionRoutedToMetadataKey); got != "worker" {
		t.Errorf("control gc.execution_routed_to = %q, want worker", got)
	}
	for _, id := range []string{"wf-1.mix", "wf-1.flip"} {
		if got := mustMeta(t, store, id, beadmeta.RoutedToMetadataKey); got != "worker" {
			t.Errorf("%s gc.routed_to = %q, want worker", id, got)
		}
	}
	for id, before := range untouched {
		after, err := store.Get(id)
		if err != nil {
			t.Fatalf("reading %s after the sling: %v", id, err)
		}
		if !maps.Equal(before, after.Metadata) {
			t.Errorf("member %s (%s) metadata changed:\n before %v\n after  %v", id, after.Title, before, after.Metadata)
		}
	}
	if result.RoutedMembers != 3 {
		t.Errorf("RoutedMembers = %d, want 3 (the control and the two open steps)", result.RoutedMembers)
	}
}

// interleavingStore lands one concurrent change on the cooked run while the
// sling under test is writing: land runs once, just before the first metadata
// write to the bead at, as another sling or a session claim would land after
// this sling planned its member routes.
type interleavingStore struct {
	*beads.MemStore
	at     string
	land   func(*beads.MemStore)
	landed bool
}

func (s *interleavingStore) before(id string) {
	if !s.landed && id == s.at {
		s.landed = true
		s.land(s.MemStore)
	}
}

func (s *interleavingStore) SetMetadata(id, key, value string) error {
	s.before(id)
	return s.MemStore.SetMetadata(id, key, value)
}

func (s *interleavingStore) SetMetadataBatch(id string, kvs map[string]string) error {
	s.before(id)
	return s.MemStore.SetMetadataBatch(id, kvs)
}

func (s *interleavingStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	s.before(id)
	return s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
}

// TestSlingExpandedRootLeavesMembersChangedAfterThePlan pins the re-read
// before each member write: a route, claim, or close that lands on a member
// after the sling planned its routes is kept, never overwritten with the
// sling's target, and the sling names the member in a warning. A member a
// concurrent sling routed to the same target is routed as asked and needs no
// warning.
func TestSlingExpandedRootLeavesMembersChangedAfterThePlan(t *testing.T) {
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	other := config.Agent{Name: "other", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker, other}}
	setMeta := func(t *testing.T, store *beads.MemStore, id, key, value string) {
		t.Helper()
		if err := store.SetMetadata(id, key, value); err != nil {
			t.Fatalf("landing %s=%s on %s: %v", key, value, id, err)
		}
	}

	for _, tc := range []struct {
		name string
		// initiator makes the sling stamp the root before any member write,
		// so a change landing at the root lands before apply starts.
		initiator string
		at        string
		land      func(t *testing.T, store *beads.MemStore)
		// wantRoutedMembers counts the members this sling itself wrote.
		wantRoutedMembers int
		// want is each member's metadata after the sling, key by key.
		want map[string]map[string]string
		// wantAssignee is each member's assignee after the sling.
		wantAssignee map[string]string
		// wantNamed and wantUnnamed are the warning's member entries.
		wantNamed   []string
		wantUnnamed []string
	}{
		{
			name: "between two member writes",
			// Controls are written first: the change lands after the
			// control's route and before either step's.
			at: "wf-1.finalize",
			land: func(t *testing.T, store *beads.MemStore) {
				setMeta(t, store, "wf-1.flip", beadmeta.RoutedToMetadataKey, "other")
				rival := "rival-session"
				if err := store.Update("wf-1.mix", beads.UpdateOpts{Assignee: &rival}); err != nil {
					t.Fatalf("claiming wf-1.mix: %v", err)
				}
			},
			wantRoutedMembers: 1,
			want: map[string]map[string]string{
				"wf-1.finalize": {beadmeta.ExecutionRoutedToMetadataKey: "worker"},
				"wf-1.flip":     {beadmeta.RoutedToMetadataKey: "other"},
				"wf-1.mix":      {beadmeta.RoutedToMetadataKey: ""},
			},
			wantAssignee: map[string]string{"wf-1.mix": "rival-session"},
			wantNamed:    []string{"wf-1.flip (routed to other)", "wf-1.mix (claimed by rival-session)"},
		},
		{
			name:      "before the first member write",
			initiator: "human",
			at:        "wf-1",
			land: func(t *testing.T, store *beads.MemStore) {
				setMeta(t, store, "wf-1.finalize", beadmeta.ExecutionRoutedToMetadataKey, "other")
				setMeta(t, store, "wf-1.flip", beadmeta.RoutedToMetadataKey, "worker")
				if err := store.Close("wf-1.mix"); err != nil {
					t.Fatalf("closing wf-1.mix: %v", err)
				}
			},
			wantRoutedMembers: 0,
			want: map[string]map[string]string{
				"wf-1.finalize": {
					beadmeta.ExecutionRoutedToMetadataKey: "other",
					beadmeta.RoutedToMetadataKey:          "core.control-dispatcher",
				},
				"wf-1.flip": {beadmeta.RoutedToMetadataKey: "worker"},
				"wf-1.mix":  {beadmeta.RoutedToMetadataKey: ""},
			},
			wantNamed:   []string{"wf-1.finalize (routed to other)", "wf-1.mix (closed)"},
			wantUnnamed: []string{"wf-1.flip"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := cookedWorkflowStore(t)
			store := &interleavingStore{MemStore: mem, at: tc.at, land: func(s *beads.MemStore) { tc.land(t, s) }}
			deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
			deps = withStore(deps, store)
			deps.Router = &recordingRouter{store: store}
			opts := testOpts(worker, "wf-1")
			opts.Initiator = tc.initiator

			result, err := DoSling(opts, deps, deps.Store)
			if err != nil {
				t.Fatalf("DoSling: %v", err)
			}
			if !store.landed {
				t.Fatalf("the concurrent change never landed: the sling wrote nothing to %s", tc.at)
			}
			if got := mustMeta(t, mem, "wf-1", beadmeta.RoutedToMetadataKey); got != "worker" {
				t.Errorf("root gc.routed_to = %q, want worker", got)
			}
			for id, keys := range tc.want {
				for key, want := range keys {
					if got := mustMeta(t, mem, id, key); got != want {
						t.Errorf("%s %s = %q, want %q", id, key, got, want)
					}
				}
			}
			for id, want := range tc.wantAssignee {
				b, err := mem.Get(id)
				if err != nil {
					t.Fatalf("reading %s: %v", id, err)
				}
				if b.Assignee != want {
					t.Errorf("%s assignee = %q, want %q (the claim stands)", id, b.Assignee, want)
				}
			}
			if result.RoutedMembers != tc.wantRoutedMembers {
				t.Errorf("RoutedMembers = %d, want %d (only the members this sling wrote)", result.RoutedMembers, tc.wantRoutedMembers)
			}
			if len(result.BeadWarnings) != 1 {
				t.Fatalf("BeadWarnings = %q, want exactly one changed-members warning", result.BeadWarnings)
			}
			warning := result.BeadWarnings[0]
			for _, want := range append([]string{"wf-1", "changed after this sling read them"}, tc.wantNamed...) {
				if !strings.Contains(warning, want) {
					t.Errorf("warning %q does not name %q", warning, want)
				}
			}
			for _, unwanted := range tc.wantUnnamed {
				if strings.Contains(warning, unwanted) {
					t.Errorf("warning %q names %s, which is routed to the sling's own target", warning, unwanted)
				}
			}
		})
	}
}

// rereadRacingStore lands a concurrent change on one member between the
// sling's re-read of it and its write: land runs right after a read of the
// bead at returns its snapshot (after the first read, or after every read
// when every is set), as a claim or another sling would land after apply
// judged the member. refused counts the fenced writes the store turned down,
// so a test can tell the fence caught the change rather than a read seeing it.
type rereadRacingStore struct {
	*beads.MemStore
	at      string
	land    func(*beads.MemStore)
	every   bool
	landed  int
	refused int
}

func (s *rereadRacingStore) Get(id string) (beads.Bead, error) {
	b, err := s.MemStore.Get(id)
	if err == nil && id == s.at && (s.every || s.landed == 0) {
		s.landed++
		s.land(s.MemStore)
	}
	return b, err
}

func (s *rereadRacingStore) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	err := s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
	if beads.IsPreconditionFailed(err) {
		s.refused++
	}
	return err
}

// TestSlingExpandedRootFencesEachMemberWriteOnItsReRead pins the fence on
// apply's member writes: a route, claim, or unrelated write that lands on a
// member after apply re-read it, and before the write, fails the write on the
// store's revision fence instead of being overwritten. The member is read and
// judged again: one another agent took keeps that route or claim and is named
// in the warning, one routed to the sling's own target needs nothing more,
// and one that only changed elsewhere still takes its route.
func TestSlingExpandedRootFencesEachMemberWriteOnItsReRead(t *testing.T) {
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	other := config.Agent{Name: "other", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker, other}}
	setMeta := func(id, key, value string) func(*testing.T, *beads.MemStore) {
		return func(t *testing.T, store *beads.MemStore) {
			t.Helper()
			if err := store.SetMetadata(id, key, value); err != nil {
				t.Fatalf("landing %s=%s on %s: %v", key, value, id, err)
			}
		}
	}

	for _, tc := range []struct {
		name string
		at   string
		land func(*testing.T, *beads.MemStore)
		// wantRoutedMembers counts the members this sling itself wrote.
		wantRoutedMembers int
		// want is the raced member's metadata after the sling, key by key.
		want map[string]string
		// wantAssignee is the raced member's assignee after the sling.
		wantAssignee string
		// wantNamed is the warning's entry for the raced member; empty means
		// the sling warns about nothing.
		wantNamed string
	}{
		{
			name:              "another sling routes a step",
			at:                "wf-1.flip",
			land:              setMeta("wf-1.flip", beadmeta.RoutedToMetadataKey, "other"),
			wantRoutedMembers: 2,
			want:              map[string]string{beadmeta.RoutedToMetadataKey: "other"},
			wantNamed:         "wf-1.flip (routed to other)",
		},
		{
			name: "a session claims a step",
			at:   "wf-1.mix",
			land: func(t *testing.T, store *beads.MemStore) {
				rival := "rival-session"
				if err := store.Update("wf-1.mix", beads.UpdateOpts{Assignee: &rival}); err != nil {
					t.Fatalf("claiming wf-1.mix: %v", err)
				}
			},
			wantRoutedMembers: 2,
			want:              map[string]string{beadmeta.RoutedToMetadataKey: ""},
			wantAssignee:      "rival-session",
			wantNamed:         "wf-1.mix (claimed by rival-session)",
		},
		{
			name:              "another sling execution-routes the control",
			at:                "wf-1.finalize",
			land:              setMeta("wf-1.finalize", beadmeta.ExecutionRoutedToMetadataKey, "other"),
			wantRoutedMembers: 2,
			want: map[string]string{
				beadmeta.ExecutionRoutedToMetadataKey: "other",
				beadmeta.RoutedToMetadataKey:          "core.control-dispatcher",
			},
			wantNamed: "wf-1.finalize (routed to other)",
		},
		{
			name:              "a concurrent sling routes a step to the same target",
			at:                "wf-1.flip",
			land:              setMeta("wf-1.flip", beadmeta.RoutedToMetadataKey, "worker"),
			wantRoutedMembers: 2,
			want:              map[string]string{beadmeta.RoutedToMetadataKey: "worker"},
		},
		{
			name:              "an unrelated write moves the step's revision",
			at:                "wf-1.flip",
			land:              setMeta("wf-1.flip", "note", "touched"),
			wantRoutedMembers: 3,
			want:              map[string]string{beadmeta.RoutedToMetadataKey: "worker", "note": "touched"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem := cookedWorkflowStore(t)
			store := &rereadRacingStore{MemStore: mem, at: tc.at, land: func(s *beads.MemStore) { tc.land(t, s) }}
			deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
			deps = withStore(deps, store)
			deps.Router = &recordingRouter{store: store}

			result, err := DoSling(testOpts(worker, "wf-1"), deps, deps.Store)
			if err != nil {
				t.Fatalf("DoSling: %v", err)
			}
			for key, want := range tc.want {
				if got := mustMeta(t, mem, tc.at, key); got != want {
					t.Errorf("%s %s = %q, want %q", tc.at, key, got, want)
				}
			}
			if store.refused != 1 {
				t.Errorf("fenced writes refused = %d, want 1: the change must land between %s's re-read and its write", store.refused, tc.at)
			}
			raced, err := mem.Get(tc.at)
			if err != nil {
				t.Fatalf("reading %s: %v", tc.at, err)
			}
			if raced.Assignee != tc.wantAssignee {
				t.Errorf("%s assignee = %q, want %q", tc.at, raced.Assignee, tc.wantAssignee)
			}
			// Every member the race left alone takes the sling's route.
			for _, id := range []string{"wf-1.mix", "wf-1.flip"} {
				if got := mustMeta(t, mem, id, beadmeta.RoutedToMetadataKey); id != tc.at && got != "worker" {
					t.Errorf("%s gc.routed_to = %q, want worker", id, got)
				}
			}
			if got := mustMeta(t, mem, "wf-1.finalize", beadmeta.ExecutionRoutedToMetadataKey); tc.at != "wf-1.finalize" && got != "worker" {
				t.Errorf("control gc.execution_routed_to = %q, want worker", got)
			}
			if got := mustMeta(t, mem, "wf-1", beadmeta.RoutedToMetadataKey); got != "worker" {
				t.Errorf("root gc.routed_to = %q, want worker", got)
			}
			if result.RoutedMembers != tc.wantRoutedMembers {
				t.Errorf("RoutedMembers = %d, want %d (only the members this sling wrote)", result.RoutedMembers, tc.wantRoutedMembers)
			}
			if tc.wantNamed == "" {
				if len(result.BeadWarnings) != 0 {
					t.Fatalf("BeadWarnings = %q, want none", result.BeadWarnings)
				}
				return
			}
			if len(result.BeadWarnings) != 1 {
				t.Fatalf("BeadWarnings = %q, want exactly one changed-members warning", result.BeadWarnings)
			}
			for _, want := range []string{"wf-1", "changed after this sling read them", tc.wantNamed} {
				if !strings.Contains(result.BeadWarnings[0], want) {
					t.Errorf("warning %q does not name %q", result.BeadWarnings[0], want)
				}
			}
		})
	}
}

// TestSlingExpandedRootFailsAMemberThatKeepsChanging pins the bound on the
// fenced member write: a member whose revision moves after every re-read is
// written memberRouteWriteAttempts times, and then the sling fails naming the
// member and the re-run. The root stays unrouted, so the re-run is not
// short-circuited as idempotent and completes the run.
func TestSlingExpandedRootFailsAMemberThatKeepsChanging(t *testing.T) {
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker}}
	mem := cookedWorkflowStore(t)
	touches := 0
	store := &rereadRacingStore{MemStore: mem, at: "wf-1.flip", every: true, land: func(s *beads.MemStore) {
		touches++
		if err := s.SetMetadata("wf-1.flip", "note", fmt.Sprintf("touch %d", touches)); err != nil {
			t.Fatalf("touching wf-1.flip: %v", err)
		}
	}}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps = withStore(deps, store)
	router := &recordingRouter{store: store}
	deps.Router = router

	_, err := DoSling(testOpts(worker, "wf-1"), deps, deps.Store)
	if err == nil {
		t.Fatal("DoSling succeeded although wf-1.flip changed under every write")
	}
	if !beads.IsPreconditionFailed(err) {
		t.Errorf("error %q does not wrap the refused fence", err)
	}
	for _, want := range []string{"wf-1.flip", "re-run the same sling"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %q", err, want)
		}
	}
	if store.refused != memberRouteWriteAttempts {
		t.Errorf("fenced writes refused = %d, want %d", store.refused, memberRouteWriteAttempts)
	}
	if got := mustMeta(t, mem, "wf-1.flip", beadmeta.RoutedToMetadataKey); got != "" {
		t.Errorf("wf-1.flip gc.routed_to = %q, want empty: no write landed", got)
	}
	if got := mustMeta(t, mem, "wf-1", beadmeta.RoutedToMetadataKey); got != "" {
		t.Errorf("root gc.routed_to = %q, want empty: a failed member write leaves the root for the re-run", got)
	}
	if len(router.routed) != 0 {
		t.Errorf("router routed %v, want nothing after a failed member write", router.routed)
	}
}

// noConditionalWriterStore exposes only the beads.Store methods of the store
// it wraps: a store with no conditional writer to fence a write on.
type noConditionalWriterStore struct {
	beads.Store
}

// TestSlingExpandedRootRoutesMembersOnAStoreThatCannotFence pins the
// fallback: on a store with no conditional writer, or one that refuses
// conditional writes as unsupported (BdStore without --if-revision), apply
// still routes every member, with the plain write after the re-read.
func TestSlingExpandedRootRoutesMembersOnAStoreThatCannotFence(t *testing.T) {
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker}}
	for name, wrap := range map[string]func(*beads.MemStore) beads.Store{
		"no conditional writer": func(mem *beads.MemStore) beads.Store {
			return noConditionalWriterStore{Store: mem}
		},
		"conditional writes unsupported": func(mem *beads.MemStore) beads.Store {
			mem.DisableConditionalWrites = true
			return mem
		},
	} {
		t.Run(name, func(t *testing.T) {
			mem := cookedWorkflowStore(t)
			store := wrap(mem)
			deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
			deps = withStore(deps, store)
			deps.Router = &recordingRouter{store: store}

			result, err := DoSling(testOpts(worker, "wf-1"), deps, deps.Store)
			if err != nil {
				t.Fatalf("DoSling: %v", err)
			}
			if result.RoutedMembers != 3 {
				t.Errorf("RoutedMembers = %d, want 3", result.RoutedMembers)
			}
			for _, id := range []string{"wf-1", "wf-1.mix", "wf-1.flip"} {
				if got := mustMeta(t, mem, id, beadmeta.RoutedToMetadataKey); got != "worker" {
					t.Errorf("%s gc.routed_to = %q, want worker", id, got)
				}
			}
			if got := mustMeta(t, mem, "wf-1.finalize", beadmeta.ExecutionRoutedToMetadataKey); got != "worker" {
				t.Errorf("control gc.execution_routed_to = %q, want worker", got)
			}
		})
	}
}

// TestSlingExpandedRootLeavesCrossStoreGatesUnrouted pins that member routing
// skips the gates a drain control mints for a blocker held in another store:
// they carry the run's gc.root_bead_id, but no launch routes them. The sling
// routes and counts the run's executable members only, and a gate minted
// after the run is fully routed leaves the re-sling idempotent.
func TestSlingExpandedRootLeavesCrossStoreGatesUnrouted(t *testing.T) {
	store := cookedWorkflowStore(t)
	mintGate := func(blocker string) beads.Bead {
		t.Helper()
		gate, err := store.Create(beads.Bead{
			Title: "Waits for " + blocker + ", held in another store", Type: "gate", Status: "open",
			Labels: []string{beadmeta.CrossStoreGateLabel},
			Metadata: map[string]string{
				beadmeta.RootBeadIDMetadataKey:          "wf-1",
				beadmeta.CrossStoreGateOwnerMetadataKey: "wf-1.finalize",
				beadmeta.CrossStoreBlockerMetadataKey:   blocker,
			},
		})
		if err != nil {
			t.Fatalf("minting the cross-store gate for %s: %v", blocker, err)
		}
		return gate
	}
	first := mintGate("rig-1")
	worker := config.Agent{Name: "worker", MaxActiveSessions: intPtr(-1)}
	cfg := &config.City{Agents: []config.Agent{worker}}
	deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
	deps = withStore(deps, store)
	deps.Router = &recordingRouter{store: store}

	result, err := DoSling(testOpts(worker, "wf-1"), deps, deps.Store)
	if err != nil {
		t.Fatalf("DoSling: %v", err)
	}
	if result.RoutedMembers != 3 {
		t.Errorf("RoutedMembers = %d, want 3 (the two steps and the control, not the gate)", result.RoutedMembers)
	}
	if len(result.BeadWarnings) != 0 {
		t.Errorf("BeadWarnings = %q, want none", result.BeadWarnings)
	}

	// The drain mints another gate once the run is fully routed.
	second := mintGate("rig-2")
	again, err := DoSling(testOpts(worker, "wf-1"), deps, deps.Store)
	if err != nil {
		t.Fatalf("re-sling: %v", err)
	}
	if !again.Idempotent {
		t.Errorf("re-sling of the fully routed run is not idempotent (RoutedMembers=%d): a new gate voided the short-circuit", again.RoutedMembers)
	}
	for _, gate := range []beads.Bead{first, second} {
		if got := mustMeta(t, store, gate.ID, beadmeta.RoutedToMetadataKey); got != "" {
			t.Errorf("gate %s gc.routed_to = %q, want empty: no launch routes a cross-store gate", gate.ID, got)
		}
	}
}
