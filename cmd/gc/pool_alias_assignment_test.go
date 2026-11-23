package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// A namepool member keeps its alias ("rig/furiosa"): unlike a rebinding numeric
// slot it is a stable identity, and session.AssigneeIdentifier is alias-first,
// so `gc hook --claim` records the member's claims under that alias. Every
// reconciler reader that asks "does this seat hold work?" (the awake set, the
// drain guards and the pool reuse guard) must therefore see alias-form work,
// or a seat mid-claim reads as idle, gets retired when template demand drops,
// and the orphan-release path (which already honors the alias) reopens its work
// for a fresh seat (#6413).
//
// The transient-slot counterpart is TestAssignmentGuardsIgnoreTransientPoolSlotAliases:
// a slot-form alias must still match no session.

func namepoolPoolConfig() *config.City {
	return &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              "polecat",
			Dir:               "rig",
			StartCommand:      "true",
			NamepoolNames:     []string{"furiosa", "nux"},
			MaxActiveSessions: intPtr(2),
			ScaleCheck:        "printf 1",
		}},
	}
}

func namepoolSessionBead(id, sessionName, alias string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Type:   sessionBeadType,
		Status: "open",
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"template":             "rig/polecat",
			"session_name":         sessionName,
			poolManagedMetadataKey: boolMetadata(true),
			"pool_slot":            "1",
			"alias":                alias,
			"state":                "active",
		},
	}
}

func identifiersContain(ids []string, want string) bool {
	for _, id := range ids {
		if id == want {
			return true
		}
	}
	return false
}

func TestNamepoolAliasClaimKeepsSeatAwake(t *testing.T) {
	cfg := namepoolPoolConfig()
	holder := sessiontest.SeedBead(t, namepoolSessionBead("ga-furiosa", "polecat-ga-furiosa", "rig/furiosa"))
	idle := sessiontest.SeedBead(t, namepoolSessionBead("ga-nux", "polecat-ga-nux", "rig/nux"))
	work := []beads.Bead{{ID: "wb-1", Status: "in_progress", Assignee: "rig/furiosa"}}

	input := buildAwakeInputFromReconciler(cfg, t.TempDir(), []sessionpkg.Info{holder, idle},
		map[string]int{"rig/polecat": 0}, nil, nil, nil, nil, work, []bool{false}, nil, nil, time.Now().UTC())
	if got := input.SessionBeads[0].Alias; got != "rig/furiosa" {
		t.Fatalf("awake input alias = %q, want the namepool alias %q", got, "rig/furiosa")
	}

	result := ComputeAwakeSet(input)
	got := result["polecat-ga-furiosa"]
	if !got.ShouldWake || got.Reason != "assigned-work" {
		t.Fatalf("namepool seat holding alias-form work: decision = %+v, want awake for assigned-work", got)
	}
	if !got.HasAssignedWork || !got.AssignedWorkClaimed || got.AssignedWorkBeadID != "wb-1" {
		t.Fatalf("namepool seat holding alias-form work: decision = %+v, want claimed wb-1", got)
	}
	if other := result["polecat-ga-nux"]; other.ShouldWake || other.HasAssignedWork {
		t.Fatalf("work-free namepool seat: decision = %+v, want not awake with zero demand", other)
	}
}

func TestNamepoolAliasClaimBlocksDrainAndReuse(t *testing.T) {
	cfg := namepoolPoolConfig()
	bead := namepoolSessionBead("ga-furiosa", "polecat-ga-furiosa", "rig/furiosa")
	info := sessiontest.SeedBead(t, bead)

	for name, ids := range map[string][]string{
		"bead": sessionAssignmentIdentifiersForConfig(bead, cfg),
		"info": sessionAssignmentIdentifiersForConfigInfo(info, cfg),
	} {
		if !identifiersContain(ids, "rig/furiosa") {
			t.Fatalf("%s assignment identifiers %v omit the namepool alias", name, ids)
		}
	}

	store := beads.NewMemStore()
	mustCreateInProgressWork(t, store, "rig/furiosa")
	has, err := sessionHasOpenAssignedWorkForConfigInfo(testSeatWork("", cfg, store, nil), info)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForConfigInfo: %v", err)
	}
	if !has {
		t.Fatal("drain guard missed alias-form work held by a namepool seat")
	}
	closeGate, err := sessionHasOpenAssignedWorkForReachableStoreForCloseGate(testSeatWork("", cfg, store, nil), info)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStoreForCloseGate: %v", err)
	}
	if !closeGate {
		t.Fatal("drain-ack close gate missed alias-form work held by a namepool seat")
	}

	work := []beads.Bead{{ID: "wb-1", Status: "in_progress", Assignee: "rig/furiosa"}}
	if !sessionBeadHasAssignedWorkInfo(work, info, cfg) {
		t.Fatal("pool reuse guard missed alias-form work held by a namepool seat")
	}
}

// A slot-form alias on a legacy bead of a rebinding pool must stay invisible to
// the awake set, the same negative pin the guards carry. When the bead's agent
// cannot be resolved, its slot alias is treated the same way.
func TestRebindingPoolSlotAliasDoesNotKeepSeatAwake(t *testing.T) {
	info := sessiontest.SeedBead(t, legacyAliasedPoolSessionBead(
		"gcg-session-x", "gc__run-operator-gcg-session-x", "gascity/gc.run-operator-1", ""))
	work := []beads.Bead{{ID: "wb", Status: "in_progress", Assignee: "gascity/gc.run-operator-1"}}

	for name, cfg := range map[string]*config.City{
		"transient slot":     aliasGuardConfig(),
		"unresolvable agent": {Agents: []config.Agent{{Name: "other"}}},
		"no config":          nil,
	} {
		if got := stableAssignmentAliasForConfigInfo(info, cfg); got != "" {
			t.Errorf("%s: stable alias = %q, want empty for a rebinding slot", name, got)
		}
		if sessionBeadHasAssignedWorkInfo(work, info, cfg) {
			t.Errorf("%s: pool reuse guard honored a rebinding slot alias", name)
		}
		if cfg == nil {
			continue
		}
		input := buildAwakeInputFromReconciler(cfg, t.TempDir(), []sessionpkg.Info{info},
			nil, nil, nil, nil, nil, work, []bool{false}, nil, nil, time.Now().UTC())
		if got := input.SessionBeads[0].Alias; got != "" {
			t.Errorf("%s: awake input alias = %q, want empty for a rebinding slot", name, got)
		}
		if d := ComputeAwakeSet(input)["gc__run-operator-gcg-session-x"]; d.HasAssignedWork {
			t.Errorf("%s: slot-form work kept the current slot holder awake: %+v", name, d)
		}
	}
}

// TestNamepoolAliasStaysAssignmentIdentityAcrossReconcileTicks runs the real
// build -> sync order twice and checks that the alias sync keeps stamping on a
// namepool member is both the identity the member claims under
// (AssigneeIdentifier, which feeds GC_ALIAS/BEADS_ACTOR) and an identity the
// guards and the awake input recognize on every tick.
func TestNamepoolAliasStaysAssignmentIdentityAcrossReconcileTicks(t *testing.T) {
	cityPath := t.TempDir()
	store := beads.NewMemStore()
	cfg := namepoolPoolConfig()
	clk := &clock.Fake{Time: time.Date(2026, 9, 28, 12, 0, 0, 0, time.UTC)}

	var beadID, alias string
	for tick := 1; tick <= 2; tick++ {
		var stderr bytes.Buffer
		dsResult := buildDesiredState("test-city", cityPath, clk.Now(), cfg, runtime.NewFake(), store, &stderr)
		syncSessionBeads(cityPath, store, dsResult.State, runtime.NewFake(), allConfiguredDS(dsResult.State), cfg, clk, &stderr, true)

		sessionBeads, err := loadSessionBeads(store)
		if err != nil {
			t.Fatalf("tick %d: load session beads: %v", tick, err)
		}
		if len(sessionBeads) != 1 {
			t.Fatalf("tick %d: session beads = %d, want 1; stderr=%q", tick, len(sessionBeads), stderr.String())
		}
		got := sessionBeads[0]
		if beadID == "" {
			beadID, alias = got.ID, got.Metadata["alias"]
			if alias == "" || got.Metadata["pool_slot"] == "" {
				t.Fatalf("tick 1: namepool bead alias=%q pool_slot=%q, want both set", alias, got.Metadata["pool_slot"])
			}
		} else if got.ID != beadID || got.Metadata["alias"] != alias {
			t.Fatalf("tick %d: bead %q alias %q, want tick-1 bead %q alias %q", tick, got.ID, got.Metadata["alias"], beadID, alias)
		}

		info, err := sessionFrontDoor(store).Get(got.ID)
		if err != nil {
			t.Fatalf("tick %d: project session info: %v", tick, err)
		}
		if claim := sessionpkg.AssigneeIdentifier(info); claim != alias {
			t.Fatalf("tick %d: AssigneeIdentifier = %q, want the namepool alias %q", tick, claim, alias)
		}
		if ids := sessionAssignmentIdentifiersForConfigInfo(info, cfg); !identifiersContain(ids, alias) {
			t.Fatalf("tick %d: assignment identifiers %v omit the claim identity %q", tick, ids, alias)
		}
		input := buildAwakeInputFromReconciler(cfg, cityPath, []sessionpkg.Info{info},
			nil, nil, nil, nil, nil, nil, nil, nil, nil, clk.Now())
		if len(input.SessionBeads) != 1 || input.SessionBeads[0].Alias != alias {
			t.Fatalf("tick %d: awake input session beads = %+v, want alias %q", tick, input.SessionBeads, alias)
		}
	}
}
