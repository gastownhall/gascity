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
	work := []beads.Bead{{ID: "gcg-1", Status: "in_progress", Assignee: "test-city--worker-1"}}
	refs := []string{classBindingAssignedWorkStoreRef}

	kept, keptRefs := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, infos, work, refs)

	if len(kept) != 1 || kept[0].ID != "gcg-1" {
		t.Fatalf("filtered work = %#v, want the binding-resident claim kept", kept)
	}
	if len(keptRefs) != 1 || keptRefs[0] != classBindingAssignedWorkStoreRef {
		t.Fatalf("filtered refs = %#v, want the binding arm's ref preserved", keptRefs)
	}
}

// Control: the same bead in the same arm, assigned to an unrelated identity. The
// widening is COLLECTION only, so this must still be dropped — a different
// outcome from the row above, which is what proves the keep is identity-scoped
// and not a blanket "keep the leading arm".
func TestSessionWakeFilterStillDropsAForeignClaimOnTheClassBindingArm(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	work := []beads.Bead{{ID: "gcg-1", Status: "in_progress", Assignee: "test-city--someone-else"}}
	refs := []string{classBindingAssignedWorkStoreRef}

	kept, _ := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, infos, work, refs)

	if len(kept) != 0 {
		t.Fatalf("filtered work = %#v, want a foreign assignee dropped", kept)
	}
}

// Control: a TEMPLATE-key match on the binding arm stays rig-scoped. A template
// is a scope statement, not an ownership one, so widening it would wake every
// session of the template on one another's stores.
func TestSessionWakeFilterKeepsTemplateMatchesRigScoped(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	work := []beads.Bead{{ID: "gcg-1", Status: "in_progress", Assignee: "riga/worker"}}
	refs := []string{classBindingAssignedWorkStoreRef}

	kept, _ := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, infos, work, refs)

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
	work := []beads.Bead{{ID: "gcg-1", Status: "in_progress", Assignee: "test-city--worker-1"}}
	refs := []string{classBindingAssignedWorkStoreRef}

	kept, keptRefs := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, infos, work, refs)
	readyFlags := readyAssignedFlagsForBeads(
		map[storeScopedBeadKey]bool{{StoreRef: classBindingAssignedWorkStoreRef, ID: "gcg-1"}: true},
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
	cfg.Storage = infraSplitConfig(filepath.Join(cityPath, "infra")).Storage
	binding := beads.NewMemStore()   // the sessions/graph binding the reconciler leads with
	rigStore := beads.NewMemStore()  // the rig work store
	cityStore := beads.NewMemStore() // unrelated: proves the extra leg is the LEADING store, not a fan-out

	stores, err := reachableStoresForSessionInfo(cityPath, cfg, binding, map[string]beads.Store{"riga": rigStore}, infos[0])
	if err != nil {
		t.Fatalf("reachableStoresForSessionInfo: %v", err)
	}
	if len(stores) != 2 || stores[0] != rigStore || stores[1] != binding {
		t.Fatalf("reachable stores = %#v, want [rig store, class binding] in that order", stores)
	}
	if sameStoreSet(stores, cityStore) {
		t.Fatal("an unrelated store leaked into the scan")
	}
}

// Control: a city that relocates nothing keeps the single-store scan it has
// today. The extra leg is a property of the SPLIT, not a general widening.
func TestReachableStoresStayRigScopedOnASingleStoreCity(t *testing.T) {
	cfg, cityPath, infos := rigScopedWakeFixture(t)
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()

	stores, err := reachableStoresForSessionInfo(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, infos[0])
	if err != nil {
		t.Fatalf("reachableStoresForSessionInfo: %v", err)
	}
	if len(stores) != 1 || stores[0] != rigStore {
		t.Fatalf("reachable stores = %#v, want only the rig store on a city that relocates nothing", stores)
	}
}

func sameStoreSet(stores []beads.Store, want beads.Store) bool {
	for _, s := range stores {
		if s == want {
			return true
		}
	}
	return false
}
