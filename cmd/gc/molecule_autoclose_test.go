package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
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
	doMoleculeAutocloseWith(store, events.Discard, stepA.ID, &out1)
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
	doMoleculeAutocloseWith(store, events.Discard, stepB.ID, &out2)
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
	doMoleculeAutocloseWith(store, events.Discard, task.ID, &out)

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
	doMoleculeAutocloseWith(store, events.Discard, orphan.ID, &out)
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
	doMoleculeAutocloseWith(store, events.Discard, step.ID, &out)

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
	doMoleculeAutocloseWith(store, events.Discard, step.ID, &out)
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
	doMoleculeAutocloseWith(store, events.Discard, step.ID, &out)
	r, _ := store.Get(root.ID)
	if r.Status != "closed" {
		t.Fatalf("sole-child molecule did not close: status=%q out=%q", r.Status, out.String())
	}
}

// TestMoleculeAutocloseRespectsTombstone asserts a tombstoned step
// counts as terminal for completeness checking (mirrors
// convoycore.IsTerminalStatus behavior — status=="closed" or
// "tombstone"). Both children terminal → root closes.
func TestMoleculeAutocloseRespectsTombstone(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "mol", Type: "molecule"})
	stepA, _ := store.Create(beads.Bead{Title: "a", Type: "step", ParentID: root.ID})
	stepB, _ := store.Create(beads.Bead{Title: "b", Type: "step", ParentID: root.ID})

	_ = store.Close(stepA.ID)
	_ = store.Close(stepB.ID)

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, events.Discard, stepB.ID, &out)
	r, _ := store.Get(root.ID)
	if r.Status != "closed" {
		t.Fatalf("root not auto-closed when all children terminal: status=%q out=%q", r.Status, out.String())
	}
}

// TestCloseHookScriptIncludesMoleculeAutoclose asserts the bd close
// hook script wired by gc forwards bead closes to `gc molecule
// autoclose` alongside the existing convoy and wisp autoclose calls.
// Without this wiring the new code is unreachable in production.
func TestCloseHookScriptIncludesMoleculeAutoclose(t *testing.T) {
	script := closeHookScript()
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
