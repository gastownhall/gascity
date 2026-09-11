package main

import (
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

// The demand-tier twin of pool_demand_class_binding_test.go, one tier over.
//
// filterAssignedWorkBeadsForPoolDemand learned to see a claim recorded under a
// relocated "class:*" binding (ga-whzrt); the named-session direct-demand match
// at build_desired_state.go:975 did not. The two answer the same question —
// "does this work still need a worker?" — for the two session shapes a city
// runs, so a ref pool demand accepts and named demand rejects is the same
// "drains a slot then refuses to refill it" asymmetry, reproduced for a
// rig-scoped [[named_session]] that dies holding a class-routed graph claim.
//
// namedWorkReady is exported verbatim as DesiredStateResult.NamedSessionDemand
// (build_desired_state.go:1084), so these tests drive the whole builder and read
// that map — the tightest end-to-end pin on the :975 site.

// namedSessionClassBindingIdentity is the rig-scoped named session under test.
// Rig scope is load-bearing: a rig-scoped agent is NOT cross-store-eligible, so
// its claim must pass the rig-equality gate that a "class:*" ref can never
// satisfy — exactly the reachability the widening restores.
const namedSessionClassBindingIdentity = "riga/patrol"

// namedSessionClassBindingCfg is a two-rig city with a rig-scoped, on_demand
// [[named_session]]. on_demand is what makes assigned work the sole materialize
// signal, so the store-ref reachability check is the only thing standing between
// the claim and demand.
func namedSessionClassBindingCfg(t *testing.T, cityPath string) *config.City {
	t.Helper()
	for _, rig := range []string{"riga", "rigb"} {
		if err := os.MkdirAll(filepath.Join(cityPath, rig), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Rigs: []config.Rig{
			{Name: "riga", Path: filepath.Join(cityPath, "riga")},
			{Name: "rigb", Path: filepath.Join(cityPath, "rigb")},
		},
		Agents: []config.Agent{{
			Name:         "patrol",
			Dir:          "riga",
			StartCommand: "true",
			WorkQuery:    "printf ''",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "patrol",
			Dir:      "riga",
			Mode:     "on_demand",
		}},
	}
}

// seedNamedSessionClaim records an in_progress claim held by the named session's
// runtime identity — the tmux-safe "riga--patrol" form live cities record, which
// namedSessionAssigneeMatchesSpec accepts (ga-e70d2). in_progress bypasses the
// readiness gate so only the store-ref reachability check is under test.
func seedNamedSessionClaim(t *testing.T, store beads.Store) {
	t.Helper()
	runtimeName := agent.SanitizeQualifiedNameForSession(namedSessionClassBindingIdentity)
	b, err := store.Create(beads.Bead{
		Title:    "graph step claimed by a rig-scoped named session",
		Type:     "task",
		Status:   "open",
		Assignee: runtimeName,
		Metadata: map[string]string{"gc.routed_to": namedSessionClassBindingIdentity},
	})
	if err != nil {
		t.Fatal(err)
	}
	inProgress := "in_progress"
	if err := store.Update(b.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatal(err)
	}
}

// The finding. On a split city a rig-scoped named session's in_progress claim is
// resident under the relocated class binding ("class:gmnos"), a ref no rig name
// equals. Before the widening the demand tier could not see it, so a session
// that died holding it was never resumed — even though the wake filter still
// retained the holder. NamedSessionDemand must be true.
func TestBuildDesiredState_NamedSessionResumesClassRelocatedClaim(t *testing.T) {
	cityPath := t.TempDir()
	cfg := namedSessionClassBindingCfg(t, cityPath)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)
	seedNamedSessionClaim(t, binding)

	cityStore := beads.NewMemStore()
	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"riga": beads.NewMemStore(), "rigb": beads.NewMemStore()}, nil, nil, io.Discard,
	)

	if !dsResult.NamedSessionDemand[namedSessionClassBindingIdentity] {
		t.Fatalf("named session %q holds an in_progress claim resident under the relocated class binding "+
			"but generated no demand (NamedSessionDemand=%v).\n"+
			"build_desired_state.go:975 gated named demand on pure rig-equality, so a \"class:*\" ref no rig "+
			"name can equal made the claim unreachable — the pool tier's ga-whzrt bug, one tier over.",
			namedSessionClassBindingIdentity, dsResult.NamedSessionDemand)
	}
}

// Gate-not-vacuous control. On the SAME split city a claim resident in rigb's
// store must still be dropped for the riga-scoped named session: the widening
// covers the relocated class binding only, never another rig's ref. A different
// outcome from the row above is what proves this is not a blanket "accept every
// ref". Holds before and after the fix.
func TestBuildDesiredState_NamedSessionDropsForeignRigClaimOnASplitCity(t *testing.T) {
	cityPath := t.TempDir()
	cfg := namedSessionClassBindingCfg(t, cityPath)
	binding := beads.NewMemStore()
	seedSplitRoutes(t, cityPath, binding)

	rigbStore := beads.NewMemStore()
	seedNamedSessionClaim(t, rigbStore) // riga's named session, claim resident in RIGB's store

	cityStore := beads.NewMemStore()
	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"riga": beads.NewMemStore(), "rigb": rigbStore}, nil, nil, io.Discard,
	)

	if dsResult.NamedSessionDemand[namedSessionClassBindingIdentity] {
		t.Fatalf("a claim resident in rigb's store woke the riga-scoped named session %q "+
			"(NamedSessionDemand=%v); the widening covers the relocated class binding only, never another "+
			"rig's ref — the gate must not be vacuous", namedSessionClassBindingIdentity, dsResult.NamedSessionDemand)
	}
}

// Single-store control, the load-bearing half. A city that relocates nothing has
// its work under the empty ref, and accepting that for a rig-scoped agent would
// make the rig gate vacuous — the demand-tier twin of the regression
// TestBuildDesiredState_RigPoolIgnoresAssignedWorkInUnreachableStore forbids.
// assignedWorkRelocatedClaimRefs answering nil on a single-store city is what
// keeps it dropped. Holds before and after the fix.
func TestBuildDesiredState_NamedSessionDropsCityWorkOnASingleStoreCity(t *testing.T) {
	cityPath := t.TempDir()
	cfg := namedSessionClassBindingCfg(t, cityPath)
	seedNoRoutes(t, cityPath)

	cityStore := beads.NewMemStore()
	seedNamedSessionClaim(t, cityStore) // resident in the city work store, ref ""

	dsResult := buildDesiredStateWithSessionBeads(
		"test-city", cityPath, time.Now().UTC(), cfg, runtime.NewFake(),
		cityStore, map[string]beads.Store{"riga": beads.NewMemStore(), "rigb": beads.NewMemStore()}, nil, nil, io.Discard,
	)

	if dsResult.NamedSessionDemand[namedSessionClassBindingIdentity] {
		t.Fatalf("city-store work woke the rig-scoped named session %q on a single-store city "+
			"(NamedSessionDemand=%v). assignedWorkRelocatedClaimRefs must answer nil where nothing was "+
			"relocated, or the rig gate goes vacuous — the demand-tier twin of "+
			"TestPoolDemandFilterUnchangedOnASingleStoreCity.", namedSessionClassBindingIdentity, dsResult.NamedSessionDemand)
	}
}
