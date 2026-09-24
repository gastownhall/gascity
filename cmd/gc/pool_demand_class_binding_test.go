package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
)

// The demand-derivation twin of the ga-whzrt rows.
//
// filterAssignedWorkBeadsForSessionWake learned to accept a claim recorded on a
// relocated class binding; filterAssignedWorkBeadsForPoolDemand never did. The
// two answer the same question from opposite ends — "is this holder still
// working?" and "does this work still need a worker?" — so a ref one accepts and
// the other rejects is a city that will not regenerate a worker for work it can
// plainly see.
//
// That gap is not reachable on a single-store city, which is why it survived:
// the demand filter's equality test is against the agent's configured RIG, and
// before the graph class was relocated a rig-scoped agent's molecule work sat in
// a leg whose ref was that rig. Relocating the class moved every graph-resident
// step bead to a "class:*" ref that no rig name can equal, so demand for a
// rig-scoped pool agent's in-progress work became structurally unreachable.
//
// The failure only SHOWS when a session dies holding a claim: while the work is
// still ready, scale_check supplies demand independently. Once it is claimed,
// the resume tier is the only source left, and it is reading a ref that never
// matches.

func rigScopedPoolDemandFixture(t *testing.T) (*config.City, string, []sessionpkg.Info) {
	t.Helper()
	cityPath := t.TempDir()
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "riga", Path: filepath.Join(cityPath, "riga")},
			{Name: "rigb", Path: filepath.Join(cityPath, "rigb")},
		},
		Agents: []config.Agent{
			{Name: "worker", Dir: "riga"},
			{Name: "worker", Dir: "rigb"},
		},
	}
	sessions := []beads.Bead{{
		ID:     "gcs-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "test-city--worker-1",
			"state":        "asleep",
			"pool_managed": "true",
		},
	}}
	return cfg, cityPath, sessionInfosFromBeads(sessions)
}

// routedPoolWorkBead is the one shape every row here needs: an in-progress claim
// held by the fixture's asleep pool session and routed to its rig-scoped
// template. The rows differ only in which store-ref the census hands the filter,
// so the bead itself is constant.
func routedPoolWorkBead() beads.Bead {
	return beads.Bead{
		ID:       "gcg-1",
		Status:   "in_progress",
		Assignee: "test-city--worker-1",
		Metadata: map[string]string{beadmeta.RoutedToMetadataKey: "riga/worker"},
	}
}

// The production row: the census labels the relocated binding leg with its own
// "class:*" ref, the work is routed to a rig-scoped pool template, and the
// holder is asleep. Pool demand must still see the bead — it is the only thing
// that can mint a replacement worker for an in-progress claim.
func TestPoolDemandFilterKeepsABindingRefClaimOnTheReconcilerPlane(t *testing.T) {
	cfg, cityPath, infos := rigScopedPoolDemandFixture(t)
	work := beads.NewMemStore()
	binding := beads.NewMemStore()
	routes := splitRoutes(binding)
	registerResidencyRoutes(cityPath, routes, func() beads.Store { return work })
	t.Cleanup(func() { unregisterResidencyRoutes(cityPath, routes) })

	candidates, err := censusStoreCandidates(cityPath, cfg, binding, nil, nil, censusRefBare)
	if err != nil {
		t.Fatalf("censusStoreCandidates: %v", err)
	}
	bindingRef := candidates[len(candidates)-1].ref
	if bindingRef == "" {
		t.Fatalf("the census labeled the binding leg %q; this row is about the DISTINCT label", bindingRef)
	}
	workBeads := []beads.Bead{routedPoolWorkBead()}

	kept := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, binding, infos, workBeads, []string{bindingRef})

	if len(kept) != 1 || kept[0].ID != "gcg-1" {
		t.Fatalf("filtered work = %#v, want the binding-resident claim kept under %q — the census emits that ref and pool demand must read it", kept, bindingRef)
	}
}

// The leading-arm spelling of the same claim: on the reconciler's plane the
// binding IS the store the arm was handed, so the row carries "".
func TestPoolDemandFilterKeepsAClaimOnTheClassBindingArm(t *testing.T) {
	cfg, cityPath, infos := rigScopedPoolDemandFixture(t)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	workBeads := []beads.Bead{routedPoolWorkBead()}

	kept := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, binding, infos, workBeads, []string{""})

	if len(kept) != 1 || kept[0].ID != "gcg-1" {
		t.Fatalf("filtered work = %#v, want the claim on the leading binding arm kept", kept)
	}
}

// Control: work sitting in ANOTHER RIG's store, routed to riga's template. The
// widening covers the relocated class binding only, so a genuine cross-rig
// mismatch must still be dropped. A different outcome from the rows above is
// what proves this is not a blanket "accept every ref".
func TestPoolDemandFilterStillDropsWorkResidentInAnotherRigsStore(t *testing.T) {
	cfg, cityPath, infos := rigScopedPoolDemandFixture(t)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	workBeads := []beads.Bead{routedPoolWorkBead()}

	kept := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, binding, infos, workBeads, []string{"rigb"})

	if len(kept) != 0 {
		t.Fatalf("filtered work = %#v, want work resident in rigb's store dropped for a riga-scoped agent", kept)
	}
}

// Control: the single-store city relocates nothing, so the binding ref set
// collapses to the work ref and behavior is exactly what it was.
func TestPoolDemandFilterUnchangedOnASingleStoreCity(t *testing.T) {
	cfg, cityPath, infos := rigScopedPoolDemandFixture(t)
	seedNoRoutes(t, cityPath)
	workBeads := []beads.Bead{routedPoolWorkBead()}

	kept := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, workBeads, []string{"riga"})
	if len(kept) != 1 {
		t.Fatalf("filtered work = %#v, want a rig-resident bead kept for its own rig's agent", kept)
	}

	dropped := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, workBeads, []string{"rigb"})
	if len(dropped) != 0 {
		t.Fatalf("filtered work = %#v, want a foreign-rig bead still dropped on a single-store city", dropped)
	}

	// The load-bearing half of this control. On a single-store city EVERY bead
	// lives under the work ref, so accepting "" for a rig-scoped agent would make
	// the rig gate vacuous — the exact regression
	// TestBuildDesiredState_RigPoolIgnoresAssignedWorkInUnreachableStore forbids.
	// assignedWorkRelocatedClaimRefs answering nil is what keeps it rejected.
	cityResident := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, workBeads, []string{""})
	if len(cityResident) != 0 {
		t.Fatalf("filtered work = %#v, want city-store work still dropped for a rig-scoped agent on a single-store city", cityResident)
	}
}

// The intentional behavior change, stated out loud: on a SPLIT city a rig-scoped
// pool agent does accept the work ref.
//
// This is not the gate going vacuous. On a split city "" names one specific leg
// among several, and the sibling control above proves rigb's leg is still
// rejected; on a single-store city "" names every leg, which is why the widening
// is gated off there entirely. The bead still has to be routed to this agent's
// template, and the resume tier downstream still requires the assignee to
// resolve to an open session bead, so widening the ref set does not widen
// ownership.
//
// It also has to be this way for the two filters to agree.
// filterAssignedWorkBeadsForSessionWake takes the work ref unconditionally
// (assignedWorkClaimRefs), so excluding it here would recreate in miniature the
// very asymmetry this change exists to remove: wake retains the holder, demand
// refuses to replace it.
func TestPoolDemandFilterAcceptsTheWorkRefOnASplitCity(t *testing.T) {
	cfg, cityPath, infos := rigScopedPoolDemandFixture(t)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	workBeads := []beads.Bead{routedPoolWorkBead()}

	kept := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, workBeads, []string{""})
	if len(kept) != 1 {
		t.Fatalf("filtered work = %#v, want city-work-store work kept for the rig agent it is routed to on a split city", kept)
	}

	stillDropped := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, beads.NewMemStore(), infos, workBeads, []string{"rigb"})
	if len(stillDropped) != 0 {
		t.Fatalf("filtered work = %#v, want rigb's leg still rejected — accepting the work ref must not make the rig gate vacuous", stillDropped)
	}
}
