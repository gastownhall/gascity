package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// TestMoleculeAutocloseClosesRootWhenAllStepsClosed is the headline
// regression test for gastownhall/gascity#1039: closing the last open
// step under a molecule root must transition the molecule from open to
// closed so the existing TTL-gated wisp GC becomes eligible to collect
// the closure.
func TestMoleculeAutocloseClosesRootWhenAllStepsClosed(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "mol-focus-review", Type: "molecule"})
	stepA, _ := store.Create(beads.Bead{Title: "Load context", Type: "step", ParentID: root.ID})
	stepB, _ := store.Create(beads.Bead{Title: "Run tests", Type: "step", ParentID: root.ID})

	// Close stepA first — root must NOT close (stepB still open).
	_ = store.Close(stepA.ID)
	var out1 bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, stepA.ID, &out1)
	r1, _ := store.Get(root.ID)
	if r1.Status == "closed" {
		t.Fatalf("root closed prematurely after first step close: status=%q out=%q", r1.Status, out1.String())
	}
	if out1.Len() != 0 {
		t.Fatalf("unexpected stdout while root still has open children: %q", out1.String())
	}

	// Close stepB — root MUST now auto-close.
	_ = store.Close(stepB.ID)
	var out2 bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, stepB.ID, &out2)
	r2, _ := store.Get(root.ID)
	if r2.Status != "closed" {
		t.Fatalf("root not auto-closed after all steps closed: status=%q out=%q", r2.Status, out2.String())
	}
	if !strings.Contains(out2.String(), "Auto-closed molecule "+root.ID) {
		t.Fatalf("stdout = %q, want auto-close announcement for %s", out2.String(), root.ID)
	}
	reason := r2.Metadata["close_reason"]
	if reason != moleculeAutocloseReason {
		t.Errorf("close_reason = %q, want %q", reason, moleculeAutocloseReason)
	}
}

// TestMoleculeAutocloseIgnoresNonStepCloses asserts the hook only
// reacts to closes of type="step" — a "task" bead attached to a
// molecule represents real work the user may close independently of
// the parent's lifecycle.
func TestMoleculeAutocloseIgnoresNonStepCloses(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "mol", Type: "molecule"})
	task, _ := store.Create(beads.Bead{Title: "real work", Type: "task", ParentID: root.ID})

	_ = store.Close(task.ID)

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, task.ID, &out)

	r, _ := store.Get(root.ID)
	if r.Status == "closed" {
		t.Fatalf("root closed off a non-step task close: status=%q", r.Status)
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected stdout for non-step close: %q", out.String())
	}
}

// TestMoleculeAutocloseIgnoresStepWithoutParent asserts a stray step
// bead (no ParentID) does not produce a panic or surprising side
// effect. This guards against the orphan-detector collision flagged
// in #1033.
func TestMoleculeAutocloseIgnoresStepWithoutParent(t *testing.T) {
	store := beads.NewMemStore()
	orphan, _ := store.Create(beads.Bead{Title: "orphan step", Type: "step"})
	_ = store.Close(orphan.ID)

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, orphan.ID, &out)
	if out.Len() != 0 {
		t.Fatalf("unexpected stdout for orphan step close: %q", out.String())
	}
}

// TestMoleculeAutocloseIgnoresParentNotMolecule asserts step beads
// parented to a non-molecule bead don't trigger an autoclose of the
// parent (which would be surprising — that parent represents user
// work, not scaffolding).
func TestMoleculeAutocloseIgnoresParentNotMolecule(t *testing.T) {
	store := beads.NewMemStore()
	parent, _ := store.Create(beads.Bead{Title: "user task", Type: "task"})
	step, _ := store.Create(beads.Bead{Title: "step", Type: "step", ParentID: parent.ID})
	_ = store.Close(step.ID)

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, step.ID, &out)

	p, _ := store.Get(parent.ID)
	if p.Status == "closed" {
		t.Fatalf("non-molecule parent closed: status=%q", p.Status)
	}
}

// TestMoleculeAutocloseIdempotentOnAlreadyClosedRoot asserts a second
// call after the root has already closed is a no-op (no double-close
// event, no panic).
func TestMoleculeAutocloseIdempotentOnAlreadyClosedRoot(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "mol", Type: "molecule"})
	step, _ := store.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID})

	_ = store.Close(step.ID)
	_ = store.Close(root.ID) // pre-close the root directly

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, step.ID, &out)
	if out.Len() != 0 {
		t.Fatalf("unexpected stdout for already-closed root: %q", out.String())
	}
}

// TestMoleculeAutocloseSoleChildClosesRoot asserts a molecule with a
// single step child closes when that step closes (the common "small
// molecule" case). Exercises the same path the empty-children guard
// protects, just with a present child.
func TestMoleculeAutocloseSoleChildClosesRoot(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "single-step mol", Type: "molecule"})
	step, _ := store.Create(beads.Bead{Title: "only step", Type: "step", ParentID: root.ID})
	_ = store.Close(step.ID)

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, step.ID, &out)
	r, _ := store.Get(root.ID)
	if r.Status != "closed" {
		t.Fatalf("sole-child molecule did not close: status=%q out=%q", r.Status, out.String())
	}
}

// TestMoleculeAutocloseRespectsTombstone asserts a tombstoned step
// counts as terminal for completeness checking (mirrors
// convoycore.IsTerminalStatus behavior — status=="closed" or
// "tombstone"). One child closed + one explicitly tombstoned → root
// closes. Previously this test closed both children, which doesn't
// actually exercise the tombstone branch of IsTerminalStatus.
func TestMoleculeAutocloseRespectsTombstone(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "mol", Type: "molecule"})
	stepA, _ := store.Create(beads.Bead{Title: "a", Type: "step", ParentID: root.ID})
	stepB, _ := store.Create(beads.Bead{Title: "b", Type: "step", ParentID: root.ID})

	_ = store.Close(stepA.ID)
	tombstone := "tombstone"
	if err := store.Update(stepB.ID, beads.UpdateOpts{Status: &tombstone}); err != nil {
		t.Fatalf("set tombstone on stepB: %v", err)
	}

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, stepB.ID, &out)
	r, _ := store.Get(root.ID)
	if r.Status != "closed" {
		t.Fatalf("root not auto-closed when one child closed + one tombstoned: status=%q out=%q", r.Status, out.String())
	}
}

// TestMoleculeAutocloseNestedStepUsesRootBeadIDMetadata pins the Copilot
// finding on PR #2526 line 95: when a nested step (or a typed "gate" /
// "epic" / non-step formula-scaffolded bead) closes, its ParentID does
// not point at the molecule root. The autocloser must instead jump to
// the molecule root via the gc.root_bead_id metadata that
// molecule.Instantiate stamps onto every member, then evaluate
// completeness over the full transitive subtree (Copilot finding on
// line 118). Without both fixes, nested-step molecules never auto-close.
func TestMoleculeAutocloseNestedStepUsesRootBeadIDMetadata(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "nested-mol", Type: "molecule"})
	intermediate, _ := store.Create(beads.Bead{
		Title:    "intermediate epic step",
		Type:     "step",
		ParentID: root.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	nested, _ := store.Create(beads.Bead{
		Title:    "deeply-nested step",
		Type:     "step",
		ParentID: intermediate.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})

	_ = store.Close(intermediate.ID)
	_ = store.Close(nested.ID)

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, nested.ID, &out)
	r, _ := store.Get(root.ID)
	if r.Status != "closed" {
		t.Fatalf("nested-step close did not auto-close molecule root (gc.root_bead_id path or ListSubtree traversal regressed): status=%q out=%q", r.Status, out.String())
	}
}

// TestMoleculeAutocloseLeavesOpenWhenNestedDescendantStillOpen pins the
// matching no-false-positive guard: when ListSubtree finds at least one
// non-terminal descendant — even if all DIRECT children of the molecule
// root are terminal — the autocloser must leave the root open. This is
// the failure mode the previous store.Children-only path would not
// catch.
func TestMoleculeAutocloseLeavesOpenWhenNestedDescendantStillOpen(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "nested-mol-partial", Type: "molecule"})
	intermediate, _ := store.Create(beads.Bead{
		Title:    "epic step",
		Type:     "step",
		ParentID: root.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	nestedOpen, _ := store.Create(beads.Bead{
		Title:    "still-open nested step",
		Type:     "step",
		ParentID: intermediate.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	nestedClosed, _ := store.Create(beads.Bead{
		Title:    "closed nested step",
		Type:     "step",
		ParentID: intermediate.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})

	// Close the intermediate and one nested step. The other nested
	// step stays open: direct-children-only would see all closed and
	// fire, but transitive-subtree must see the open descendant.
	_ = store.Close(intermediate.ID)
	_ = store.Close(nestedClosed.ID)
	_ = nestedOpen // keep open intentionally

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, nestedClosed.ID, &out)
	r, _ := store.Get(root.ID)
	if r.Status == "closed" {
		t.Fatalf("root closed despite nested descendant still open (ListSubtree regressed to direct-children-only): status=%q out=%q", r.Status, out.String())
	}
}

// TestCloseMoleculeWithReasonTrimsWhitespace pins the Copilot finding
// on PR #2526 line 148: whitespace-only reason must fall through to the
// plain store.Close path, matching closeConvoyWithReason's behavior.
// Without the trim, a whitespace-only reason would stamp a meaningless
// close_reason metadata value and potentially trip downstream validators.
func TestCloseMoleculeWithReasonTrimsWhitespace(t *testing.T) {
	store := beads.NewMemStore()
	mol, _ := store.Create(beads.Bead{Title: "mol", Type: "molecule"})

	if err := closeMoleculeWithReason(store, mol.ID, "   \t\n"); err != nil {
		t.Fatalf("closeMoleculeWithReason whitespace reason: %v", err)
	}
	r, _ := store.Get(mol.ID)
	if r.Status != "closed" {
		t.Fatalf("whitespace reason did not close molecule: status=%q", r.Status)
	}
	if got := r.Metadata["close_reason"]; got != "" {
		t.Fatalf("close_reason = %q, want empty (whitespace-only reason should fall through to plain Close)", got)
	}
}

// TestCloseHookScriptIncludesMoleculeAutoclose asserts the bd close
// hook script wired by gc forwards bead closes to `gc molecule
// autoclose` alongside the existing convoy and wisp autoclose calls.
// Without this wiring the new code is unreachable in production.
func TestCloseHookScriptIncludesMoleculeAutoclose(t *testing.T) {
	script := closeHookScript("")
	if !strings.Contains(script, "molecule autoclose") {
		t.Fatalf("close hook script missing 'molecule autoclose' dispatch:\n%s", script)
	}
	// Sanity: the existing siblings are still present.
	for _, sib := range []string{"convoy autoclose", "wisp autoclose", "bead.closed"} {
		if !strings.Contains(script, sib) {
			t.Errorf("close hook script missing %q (regression in sibling wiring):\n%s", sib, script)
		}
	}
}

// TestMoleculeAutocloseClosesWorkflowRootOnSourceBeadClose is the headline
// regression: a graph.v2 workflow wisp (issue_type "task", not
// "molecule") with no expanded step children orphans when the worker closes
// the work bead directly. Closing the source/work bead — via `gc bd close`
// or a bare `bd update --status=closed`, both of which fire the same on_close
// hook — must auto-close the workflow root whose gc.source_bead_id points at
// it. Without the reverse source-bead lookup the root stays open forever and
// gets re-routed to a fresh worker.
func TestMoleculeAutocloseClosesWorkflowRootOnSourceBeadClose(t *testing.T) {
	store := beads.NewMemStore()
	work, _ := store.Create(beads.Bead{Title: "fix the bug", Type: "task"})
	root, _ := store.Create(beads.Bead{
		Title: "mol-focus-review",
		Type:  "task", // graph.v2 wisps are issue_type "task", not "molecule"
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   work.ID,
		},
	})

	_ = store.Close(work.ID)
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, work.ID, &out)

	r, _ := store.Get(root.ID)
	if r.Status != "closed" {
		t.Fatalf("stepless workflow root not auto-closed on source bead close: status=%q out=%q", r.Status, out.String())
	}
	if !strings.Contains(out.String(), "Auto-closed molecule "+root.ID) {
		t.Fatalf("stdout = %q, want auto-close announcement for %s", out.String(), root.ID)
	}
	if got := r.Metadata["close_reason"]; got != moleculeSourceAutocloseReason {
		t.Errorf("close_reason = %q, want %q", got, moleculeSourceAutocloseReason)
	}
}

// TestMoleculeAutocloseLeavesWorkflowRootOpenWhenStepOpenOnSourceClose asserts
// the source-bead trigger does NOT close a multi-step workflow root that still
// has genuine open work (e.g. an un-run review step). Only a root whose entire
// subtree is already terminal may close.
func TestMoleculeAutocloseLeavesWorkflowRootOpenWhenStepOpenOnSourceClose(t *testing.T) {
	store := beads.NewMemStore()
	work, _ := store.Create(beads.Bead{Title: "work", Type: "task"})
	root, _ := store.Create(beads.Bead{
		Title: "mol",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": work.ID,
		},
	})
	_, _ = store.Create(beads.Bead{
		Title:    "open review step",
		Type:     "step",
		ParentID: root.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})

	_ = store.Close(work.ID)
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, work.ID, &out)

	r, _ := store.Get(root.ID)
	if r.Status == "closed" {
		t.Fatalf("workflow root closed while a step is still open: status=%q out=%q", r.Status, out.String())
	}
	if out.Len() != 0 {
		t.Fatalf("unexpected stdout while root still has open step: %q", out.String())
	}
}

// TestMoleculeAutocloseClosesWorkflowRootWithTerminalStepsOnSourceClose
// asserts the source-bead trigger closes a multi-step workflow root once both
// the source bead and every step are terminal.
func TestMoleculeAutocloseClosesWorkflowRootWithTerminalStepsOnSourceClose(t *testing.T) {
	store := beads.NewMemStore()
	work, _ := store.Create(beads.Bead{Title: "work", Type: "task"})
	root, _ := store.Create(beads.Bead{
		Title: "mol",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": work.ID,
		},
	})
	step, _ := store.Create(beads.Bead{
		Title:    "done step",
		Type:     "step",
		ParentID: root.ID,
		Metadata: map[string]string{"gc.root_bead_id": root.ID},
	})
	_ = store.Close(step.ID)

	_ = store.Close(work.ID)
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, work.ID, &out)

	r, _ := store.Get(root.ID)
	if r.Status != "closed" {
		t.Fatalf("workflow root not closed when source + all steps terminal: status=%q out=%q", r.Status, out.String())
	}
}

// TestMoleculeAutocloseSourceCloseNoMatchingRootIsNoop asserts closing a bead
// that is no workflow's source bead is a silent no-op (no panic, no stdout).
func TestMoleculeAutocloseSourceCloseNoMatchingRootIsNoop(t *testing.T) {
	store := beads.NewMemStore()
	work, _ := store.Create(beads.Bead{Title: "lonely task", Type: "task"})
	_ = store.Close(work.ID)

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, work.ID, &out)
	if out.Len() != 0 {
		t.Fatalf("unexpected stdout closing a task with no workflow root: %q", out.String())
	}
}

// TestMoleculeAutocloseSourceCloseScopesToStoreRef pins the Copilot finding on
// PR #2972: the source-bead reverse lookup must scope to the closing bead's
// store-ref. Two stepless workflow roots in one store share the same
// gc.source_bead_id but were slung from same-ID source beads in different
// stores (distinguished by gc.source_store_ref). Closing the source bead in
// store "rig:alpha" must auto-close only the root sourced from "rig:alpha" —
// the root sourced from "city:test" belongs to a different (colliding) source
// and must stay open. With an empty store-ref the lookup matched on bead ID
// alone and would wrongly close both.
func TestMoleculeAutocloseSourceCloseScopesToStoreRef(t *testing.T) {
	store := beads.NewMemStore()
	work, _ := store.Create(beads.Bead{Title: "work", Type: "task"})
	mine, _ := store.Create(beads.Bead{
		Title: "root sourced from this store",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                                "workflow",
			"gc.source_bead_id":                      work.ID,
			sourceworkflow.SourceStoreRefMetadataKey: "rig:alpha",
		},
	})
	other, _ := store.Create(beads.Bead{
		Title: "root sourced from a colliding bead in another store",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":                                "workflow",
			"gc.source_bead_id":                      work.ID,
			sourceworkflow.SourceStoreRefMetadataKey: "city:test",
		},
	})

	_ = store.Close(work.ID)
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "rig:alpha", events.Discard, work.ID, &out)

	m, _ := store.Get(mine.ID)
	if m.Status != "closed" {
		t.Fatalf("root sourced from this store not auto-closed: status=%q out=%q", m.Status, out.String())
	}
	o, _ := store.Get(other.ID)
	if o.Status == "closed" {
		t.Fatalf("root sourced from a colliding cross-store bead was wrongly closed: status=%q out=%q", o.Status, out.String())
	}
	if strings.Contains(out.String(), "Auto-closed molecule "+other.ID) {
		t.Fatalf("stdout announced close of cross-store root %s: %q", other.ID, out.String())
	}
}

// TestMoleculeAutocloseSourceCloseIdempotentOnClosedRoot asserts that once the
// workflow root is already closed, a repeat source-bead close is a no-op —
// ListLiveRoots excludes closed roots, so no double-close announcement fires.
func TestMoleculeAutocloseSourceCloseIdempotentOnClosedRoot(t *testing.T) {
	store := beads.NewMemStore()
	work, _ := store.Create(beads.Bead{Title: "work", Type: "task"})
	root, _ := store.Create(beads.Bead{
		Title: "mol",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": work.ID,
		},
	})
	_ = store.Close(work.ID)
	_ = store.Close(root.ID) // pre-close the root directly

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, work.ID, &out)
	if out.Len() != 0 {
		t.Fatalf("unexpected stdout for already-closed workflow root: %q", out.String())
	}
}
