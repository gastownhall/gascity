package main

// Deep-investigator evidence for crn-vbsr0 / crn-8rpo3.
//
// Models the EXACT live shape of cairn molecule crn-39stb as read from the
// live store on 2026-08-14:
//
//	crn-xkjr  issue_type=bug       metadata.molecule_id=crn-39stb   (the target)
//	crn-39stb issue_type=molecule  gc.formula_name=mol-code-review  (the root)
//	  5x      issue_type=step      parent=crn-39stb
//	          metadata = {gc.step_id, gc.step_ref, gc.native_step_dependencies.v1}
//	          NOTE: live step beads carry NO gc.root_bead_id.
//
// The missing gc.root_bead_id is what makes case B load-bearing: the step-close
// trigger in doMoleculeAutocloseWith can only reach the root through the LEGACY
// fallback (molecule_autoclose.go:151 — Type=="step" && ParentID!=""), not
// through the metadata path the modern instantiator is documented to stamp.

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// liveShape builds the crn-39stb molecule and returns (targetID, rootID, stepIDs).
func liveShape(t *testing.T, store beads.Store) (string, string, []string) {
	t.Helper()
	root, err := store.Create(beads.Bead{
		Title: "mol-code-review",
		Type:  "molecule",
		Metadata: map[string]string{
			"gc.formula_name": "mol-code-review",
			"gc.step_id":      "mol-code-review",
			"gc.var.issue":    "crn-xkjr",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	target, err := store.Create(beads.Bead{
		Title:    "Multi-routing-label beads are routed by deacon STEP ORDER",
		Type:     "bug",
		Metadata: map[string]string{"molecule_id": root.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	var steps []string
	for _, name := range []string{"intake", "style", "security", "spec", "verdict"} {
		s, err := store.Create(beads.Bead{
			Title:    name,
			Type:     "step",
			ParentID: root.ID,
			Metadata: map[string]string{
				"gc.step_id":  "mol-code-review." + name,
				"gc.step_ref": "mol-code-review." + name,
				// deliberately NO gc.root_bead_id — matches live data
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		steps = append(steps, s.ID)
	}
	return target.ID, root.ID, steps
}

// TestCrnVbsr0_A_TargetCloseWithOpenStepsOrphans reproduces the reported bug on
// the live shape: closing the target bead while steps are open reaps nothing.
// This is the FAILING assertion — it documents current (defective) behavior.
func TestCrnVbsr0_A_TargetCloseWithOpenStepsOrphans(t *testing.T) {
	store := beads.NewMemStore()
	targetID, rootID, steps := liveShape(t, store)

	if err := store.Close(targetID); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	doWispAutocloseWith(store, targetID, &out)

	root, _ := store.Get(rootID)
	openSteps := 0
	for _, id := range steps {
		b, _ := store.Get(id)
		if b.Status != "closed" {
			openSteps++
		}
	}
	t.Logf("after target close: autoclose stdout=%q root=%s openSteps=%d/%d",
		out.String(), root.Status, openSteps, len(steps))

	if root.Status == "closed" && openSteps == 0 {
		t.Fatalf("BUG FIXED: molecule reaped on target close (expected orphan under current code)")
	}
	t.Logf("ORPHAN REPRODUCED: root=%s with %d open step beads, nothing reaped", root.Status, openSteps)
}

// TestCrnVbsr0_B_LastStepCloseSelfHealsRoot is the decisive question for the
// packs-only fix (option A). If each agent closes its own step bead, does the
// molecule root eventually reap on its own — even though the target bead ALREADY
// closed and its parked-guarded reap was skipped?
//
// If this passes, option A is sufficient end-to-end and ordering does not matter.
// If it fails, packs alone leave every molecule root open forever and an engine
// or patrol fix is mandatory.
func TestCrnVbsr0_B_LastStepCloseSelfHealsRoot(t *testing.T) {
	store := beads.NewMemStore()
	targetID, rootID, steps := liveShape(t, store)

	// 1. Target closes first with all steps open — the orphaning event.
	_ = store.Close(targetID)
	var wispOut bytes.Buffer
	doWispAutocloseWith(store, targetID, &wispOut)

	// 2. Now each step closes, as an "always close your step bead" pack fix
	//    would have each agent do. Fire the molecule autoclose hook per close.
	for i, id := range steps {
		if err := store.Close(id); err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		doMoleculeAutocloseWith(store, "", events.Discard, id, &out)
		root, _ := store.Get(rootID)
		t.Logf("closed step %d/%d (%s): root=%s stdout=%q", i+1, len(steps), id, root.Status, out.String())
	}

	root, _ := store.Get(rootID)
	if root.Status != "closed" {
		t.Fatalf("PACKS-ONLY FIX INSUFFICIENT: all %d steps closed but molecule root %s is still %q — "+
			"nothing re-triggers the reap after the target-close path was skipped by the parked guard",
			len(steps), rootID, root.Status)
	}
	t.Logf("PACKS-ONLY FIX SUFFICIENT: root self-healed to %s once the last step closed", root.Status)
}

// TestCrnVbsr0_C_StepWithoutParentNeverReaps is the control. The legacy fallback
// at molecule_autoclose.go:151 requires ParentID. A step carrying neither
// gc.root_bead_id NOR a parent edge can never reach its root, so no amount of
// pack-side "close your step" instruction would reap it. This pins WHICH link
// the self-heal in case B actually depends on.
func TestCrnVbsr0_C_StepWithoutParentNeverReaps(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "mol-code-review", Type: "molecule"})
	// A step linked to the root by NOTHING but convention.
	step, _ := store.Create(beads.Bead{
		Title:    "verdict",
		Type:     "step",
		Metadata: map[string]string{"gc.step_id": "mol-code-review.verdict"},
	})

	_ = store.Close(step.ID)
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, step.ID, &out)

	r, _ := store.Get(root.ID)
	t.Logf("parentless step closed: root=%s stdout=%q", r.Status, out.String())
	if r.Status == "closed" {
		t.Fatalf("unexpected: parentless step reaped root %s", root.ID)
	}
	t.Logf("CONFIRMED: the self-heal depends on the parent-child edge, not on gc.step_id convention")
}
