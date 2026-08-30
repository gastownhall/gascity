package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The ga-whzrt rows: a claim a rig-scoped worker HOLDS on the leading
// class-binding arm must reach the wake machinery, and nothing else must.
//
// S2 PORT: the arm's ref is no longer the classBindingAssignedWorkStoreRef
// constant but whatever assignedWorkClaimRefs resolves for the city, and the
// split is expressed as SERVED ROUTES rather than as a [storage] section in
// cfg. Those are the same change: the constant was the empty string because the
// reconciler's leading arm happens to be the binding, and the cfg shape gate
// answered "no split" for a city whose section was deleted after it had served
// one. Both facts now come from the routes.
//
// The rows themselves are unchanged, which is the point of a pin.

func rigScopedWakeFixture(t *testing.T) (*config.City, string, []sessionpkg.Info) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs:      []config.Rig{{Name: "riga", Path: filepath.Join(cityPath, "riga")}},
		Agents:    []config.Agent{{Name: "worker", Dir: "riga"}},
	}
	sessions := []beads.Bead{{
		ID:     "gcs-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "test-city--worker-1",
			"state":        "active",
			"pool_managed": "true",
		},
	}}
	return cfg, cityPath, sessionInfosFromBeads(sessions)
}

// TestSessionWakeFilterKeepsAClaimOnTheClassBindingArm is the collection half of
// the repair. The claim lives on the leading arm (the relocated
// coordination-class binding on a split city, ref "" in the assigned-work
// index), the owning agent is rig-scoped, and the assignee is this session's own
// exact identity — so the wake filter must keep it.
func TestSessionWakeFilterKeepsAClaimOnTheClassBindingArm(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	work := []beads.Bead{{ID: "gcg-1", Status: "in_progress", Assignee: "test-city--worker-1"}}
	refs := []string{""}

	kept, keptRefs := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, binding, infos, work, refs)

	if len(kept) != 1 || kept[0].ID != "gcg-1" {
		t.Fatalf("filtered work = %#v, want the binding-resident claim kept", kept)
	}
	if len(keptRefs) != 1 || keptRefs[0] != "" {
		t.Fatalf("filtered refs = %#v, want the binding arm's ref preserved", keptRefs)
	}
}

// The mixed-version row (§7 attack 2): the same claim recorded under the
// BINDING's own ref rather than under the leading arm's "". Nothing emits that
// ref today — the census records the binding arm as the city arm — but S3 makes
// it a distinct census leg, and a city rolling from one binary to the other has
// rows of both shapes in flight. One filter has to read both.
func TestSessionWakeFilterKeepsAClaimRecordedUnderTheBindingRef(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	work := []beads.Bead{{ID: "gcg-1", Status: "in_progress", Assignee: "test-city--worker-1"}}
	refs := []string{"class:gmnos"}

	// A work-led plane: the leading store is NOT the binding, so the binding
	// carries its own ref and both spellings are in the accepted set.
	kept, keptRefs := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, beads.NewMemStore(), infos, work, refs)

	if len(kept) != 1 || kept[0].ID != "gcg-1" || len(keptRefs) != 1 || keptRefs[0] != "class:gmnos" {
		t.Fatalf("filtered work = %#v refs = %#v, want the claim kept under the binding's own ref", kept, keptRefs)
	}
}

// The S3 row the one above pre-declared: the same claim under the binding's own
// ref, seen from the RECONCILER'S plane — where the leading store IS the
// binding, because buildDesiredState hands the census its sessions-class store.
//
// That combination is the one production actually runs, and it is where the ref
// vocabulary can strand a claim: the census now labels binding-resident rows
// "class:*", so if the accepted set is derived from a topology whose work leg is
// ALSO the binding, the two collapse to one ref and the new label matches
// nothing. A ref the census emits and the wake filter rejects is ga-whzrt with
// the arrow reversed — the claim is collected and then dropped, and its holder
// drains with no wake reason.
func TestSessionWakeFilterKeepsABindingRefClaimOnTheReconcilerPlane(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	work := beads.NewMemStore()
	binding := beads.NewMemStore()
	routes := splitRoutes(binding)
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return work })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	// Exactly what the census produces for this claim on this city.
	candidates, err := censusStoreCandidates(cityPath, cfg, binding, nil, nil, censusRefBare)
	if err != nil {
		t.Fatalf("censusStoreCandidates: %v", err)
	}
	bindingRef := candidates[len(candidates)-1].ref
	if bindingRef == "" {
		t.Fatalf("the census labeled the binding leg %q; this row is about the DISTINCT label", bindingRef)
	}
	workBeads := []beads.Bead{{ID: "gcg-1", Status: "in_progress", Assignee: "test-city--worker-1"}}

	kept, keptRefs := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, binding, infos, workBeads, []string{bindingRef})

	if len(kept) != 1 || kept[0].ID != "gcg-1" || len(keptRefs) != 1 || keptRefs[0] != bindingRef {
		t.Fatalf("filtered work = %#v refs = %#v, want the claim kept under %q — the census emits that ref and the filter must accept it", kept, keptRefs, bindingRef)
	}
}

// Control: the same bead in the same arm, assigned to an unrelated identity. The
// widening is COLLECTION only, so this must still be dropped — a different
// outcome from the row above, which is what proves the keep is identity-scoped
// and not a blanket "keep the leading arm".
func TestSessionWakeFilterStillDropsAForeignClaimOnTheClassBindingArm(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	work := []beads.Bead{{ID: "gcg-1", Status: "in_progress", Assignee: "test-city--someone-else"}}
	refs := []string{""}

	kept, _ := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, binding, infos, work, refs)

	if len(kept) != 0 {
		t.Fatalf("filtered work = %#v, want a foreign assignee dropped", kept)
	}
}

// Control: a TEMPLATE-key match on the binding arm stays rig-scoped. A template
// is a scope statement, not an ownership one, so widening it would wake every
// session of the template on one another's stores.
func TestSessionWakeFilterKeepsTemplateMatchesRigScoped(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	work := []beads.Bead{{ID: "gcg-1", Status: "in_progress", Assignee: "riga/worker"}}
	refs := []string{""}

	kept, _ := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, binding, infos, work, refs)

	if len(kept) != 0 {
		t.Fatalf("filtered work = %#v, want a template-key match to stay scoped to its rig store", kept)
	}
}

// TestClassBindingClaimYieldsAnAssignedWorkWakeReason carries the kept bead
// through the production chain to the decision that mattered: with the claim
// dropped, ComputeAwakeSet produced AwakeDecision{Reason:""}, which the drain
// arm renders as "no-wake-reason" and recycles a live claim holder.
func TestClassBindingClaimYieldsAnAssignedWorkWakeReason(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	work := []beads.Bead{{ID: "gcg-1", Status: "in_progress", Assignee: "test-city--worker-1"}}
	refs := []string{""}

	kept, keptRefs := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, binding, infos, work, refs)
	readyFlags := readyAssignedFlagsForBeads(
		map[storeScopedBeadKey]bool{{StoreRef: "", ID: "gcg-1"}: true},
		kept, keptRefs)

	input := buildAwakeInputFromReconciler(
		cfg, cityPath, infos,
		map[string]int{}, nil, nil, nil, nil,
		kept, readyFlags, nil,
		runtime.NewFake(), time.Now(),
	)
	decision := ComputeAwakeSet(input)["test-city--worker-1"]

	if !decision.ShouldWake || decision.Reason != "assigned-work" {
		t.Fatalf("awake decision = %+v, want ShouldWake with reason assigned-work", decision)
	}
	if !decision.HasAssignedWork {
		t.Fatal("awake decision reports no assigned work for a session holding an in-progress claim")
	}
}

// TestReachableStoresScanTheClassBindingOnASplitCity is the live-re-read half:
// every drain guard that asks "does this session still have assigned work"
// resolves its store set here, and on a split city the answer must include the
// ledger a routed claim was written into.
func TestReachableStoresScanTheClassBindingOnASplitCity(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	binding := beads.NewMemStore()  // the sessions/graph binding the reconciler leads with
	rigStore := beads.NewMemStore() // the rig work store
	seedSplitRoutes(t, cityPath, binding)

	plan, err := assignedWorkPlanForSessionInfo(cityPath, cfg, binding, map[string]beads.Store{"riga": rigStore}, infos[0])
	if err != nil {
		t.Fatalf("assignedWorkPlanForSessionInfo: %v", err)
	}
	if got := planStores(t, plan); !sameStores(got, rigStore, binding) {
		t.Fatalf("reachable stores = %#v, want [rig store, class binding] in that order", got)
	}
}

// Control: a city that relocates nothing keeps the single-store scan it has
// today. The extra leg is a property of the SPLIT, not a general widening.
func TestReachableStoresStayRigScopedOnASingleStoreCity(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	seedNoRoutes(t, cityPath)
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	plan, err := assignedWorkPlanForSessionInfo(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, infos[0])
	if err != nil {
		t.Fatalf("assignedWorkPlanForSessionInfo: %v", err)
	}
	if got := planStores(t, plan); !sameStores(got, rigStore) {
		t.Fatalf("reachable stores = %#v, want only the rig store on a city that relocates nothing", got)
	}
}
