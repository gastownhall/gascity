package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/session/sessiontest"
)

// A numeric pool slot ("gascity/gc.run-operator-1") is a REBINDING name: the
// controller hands it to a fresh session whenever the previous holder dies, so
// it cannot identify an owner. #4981 flipped `gc hook --claim` to prefer
// GC_ALIAS, and pool spawns stamped the slot into GC_ALIAS, so pool claims
// silently moved from the session name (which every reconciler guard
// enumerates) to the slot form (which none of them do) — the reconciler drained
// claim-holding workers as "orphaned" ~2min after the claim.
//
// The fix unaliases transient pool slots upstream, so the guards stay narrow on
// purpose. These are NEGATIVE pins: a slot-form assignee must match NO session,
// including the session currently bound to that slot. If a future change makes
// the guards enumerate slot aliases, a rebind would let a fresh session shield
// (or inherit) a dead session's claim — the ambiguity this design rejects.

// legacyAliasedPoolSessionBead is a pre-fix pool session bead: it still carries
// the slot in metadata["alias"], the shape in-flight beads have at deploy time.
func legacyAliasedPoolSessionBead(id, sessionName, alias, aliasHistory string) beads.Bead {
	metadata := map[string]string{
		"template":             "worker",
		"session_name":         sessionName,
		poolManagedMetadataKey: boolMetadata(true),
		"pool_slot":            "1",
		"alias":                alias,
	}
	if aliasHistory != "" {
		metadata["alias_history"] = aliasHistory
	}
	return beads.Bead{
		ID:       id,
		Type:     sessionBeadType,
		Status:   "open",
		Labels:   []string{sessionBeadLabel},
		Metadata: metadata,
	}
}

func aliasGuardConfig() *config.City {
	return &config.City{Agents: []config.Agent{{Name: "worker"}}}
}

// mustCreateInProgressWork seeds a work bead already claimed by assignee, the
// shape a pool agent leaves behind while it is running a step.
func mustCreateInProgressWork(t *testing.T, store beads.Store, assignee string) beads.Bead {
	t.Helper()
	work, err := store.Create(beads.Bead{
		Title:    "pool work",
		Type:     "task",
		Status:   "open",
		Assignee: assignee,
	})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(work.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark work in_progress: %v", err)
	}
	got, err := store.Get(work.ID)
	if err != nil {
		t.Fatalf("reload work bead: %v", err)
	}
	return got
}

// TestDrainGuardsKeepPoolSessionClaimingUnderItsSessionName is the goal-state
// control: with pool claims written session-name form, every guard already sees
// them with no guard change at all. This is what the upstream unaliasing buys.
func TestDrainGuardsKeepPoolSessionClaimingUnderItsSessionName(t *testing.T) {
	const sessionName = "gc__run-operator-gcg-session-x"
	store := beads.NewMemStore()
	info := sessiontest.SeedBead(t, legacyAliasedPoolSessionBead("gcg-session-x", sessionName, "", ""))
	mustCreateInProgressWork(t, store, sessionName)

	has, err := sessionHasOpenAssignedWorkForConfigInfo("", aliasGuardConfig(), store, nil, info)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForConfigInfo: %v", err)
	}
	if !has {
		t.Fatal("orphan-drain guard missed a session-name-form pool claim; the whole fix rests on this form being visible")
	}

	closeGate, err := sessionHasOpenAssignedWorkForReachableStoreForCloseGate("", aliasGuardConfig(), store, nil, info)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStoreForCloseGate: %v", err)
	}
	if !closeGate {
		t.Fatal("drain-ack close gate missed a session-name-form pool claim")
	}
}

// TestAssignmentGuardsIgnoreTransientPoolSlotAliases is the negative pin. Even
// for the session that currently holds the slot, a slot-form assignee must not
// register as assigned work: slot names rebind, so honoring them would let the
// next holder answer for the previous holder's abandoned claim.
func TestAssignmentGuardsIgnoreTransientPoolSlotAliases(t *testing.T) {
	store := beads.NewMemStore()
	info := sessiontest.SeedBead(t, legacyAliasedPoolSessionBead(
		"gcg-session-x", "gc__run-operator-gcg-session-x", "gascity/gc.run-operator-1", "gascity/gc.run-operator-0"))

	for _, assignee := range []string{"gascity/gc.run-operator-1", "gascity/gc.run-operator-0"} {
		mustCreateInProgressWork(t, store, assignee)
	}

	has, err := sessionHasOpenAssignedWorkForConfigInfo("", aliasGuardConfig(), store, nil, info)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForConfigInfo: %v", err)
	}
	if has {
		t.Fatal("a transient pool slot alias registered as assigned work; slot names rebind, so slot-form " +
			"ownership must never keep a session alive")
	}

	closeGate, err := sessionHasOpenAssignedWorkForReachableStoreForCloseGate("", aliasGuardConfig(), store, nil, info)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStoreForCloseGate: %v", err)
	}
	if closeGate {
		t.Fatal("the drain-ack close gate honored a transient pool slot alias")
	}

	if sessionBeadHasAssignedWorkInfo([]beads.Bead{{ID: "wb", Status: "in_progress", Assignee: "gascity/gc.run-operator-1"}}, info) {
		t.Fatal("the pool reuse predicate honored a transient pool slot alias")
	}
}
