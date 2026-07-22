package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	gcapi "github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
	"github.com/gastownhall/gascity/internal/testutil"
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
	rec := events.NewFake()
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", rec, work.ID, &out)

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
	assertMoleculeAutocloseBeadClosedSnapshot(t, rec.Events, r, moleculeSourceAutocloseReason)

	resolved := eventsOfType(rec.Events, events.MoleculeResolved)
	if len(resolved) != 1 {
		t.Fatalf("got %d molecule.resolved events, want 1: %+v", len(resolved), rec.Events)
	}
	p := decodeMoleculeResolvedPayload(t, resolved[0])
	if p.IssueID != root.ID || p.FromStatus != "open" || p.ToStatus != "closed" {
		t.Errorf("molecule.resolved transition = issue:%q %q->%q, want %s open->closed", p.IssueID, p.FromStatus, p.ToStatus, root.ID)
	}
	if p.CloseReason != moleculeSourceAutocloseReason {
		t.Errorf("molecule.resolved CloseReason = %q, want %q", p.CloseReason, moleculeSourceAutocloseReason)
	}
}

func TestMoleculeAutocloseClosesSpecSidecarsOnSourceBeadClose(t *testing.T) {
	store := beads.NewMemStore()
	work, _ := store.Create(beads.Bead{Title: "fix the bug", Type: "task"})
	root, _ := store.Create(beads.Bead{
		Title: "mol-focus-review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.source_bead_id":   work.ID,
		},
	})
	spec, _ := store.Create(beads.Bead{
		Title: "generated step spec",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": root.ID,
			"gc.spec_for":     "implement",
			"gc.spec_for_ref": "implement",
		},
	})

	_ = store.Close(work.ID)
	var out bytes.Buffer
	if retry := doMoleculeAutocloseWith(store, "", events.NewFake(), work.ID, &out).Wait(); retry {
		t.Fatal("autoclose retry = true, want lifecycle complete")
	}

	specAfter, _ := store.Get(spec.ID)
	if specAfter.Status != "closed" {
		t.Fatalf("spec status = %q, want closed", specAfter.Status)
	}
	if got := specAfter.Metadata["close_reason"]; got != sourceworkflow.WorkflowSpecSidecarClosedReason {
		t.Fatalf("spec close_reason = %q, want %q", got, sourceworkflow.WorkflowSpecSidecarClosedReason)
	}
}

func TestMoleculeAutocloseDefersSpecSidecarsToRecoveryWhenAnotherWriterClosesRoot(t *testing.T) {
	base := beads.NewMemStore()
	work, _ := base.Create(beads.Bead{Title: "fix the bug", Type: "task"})
	root, _ := base.Create(beads.Bead{
		Title: "mol-focus-review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": work.ID,
		},
	})
	spec, _ := base.Create(beads.Bead{
		Title: "generated step spec",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": root.ID,
			"gc.spec_for":     "implement",
			"gc.spec_for_ref": "implement",
		},
	})
	if err := base.Close(work.ID); err != nil {
		t.Fatalf("Close(work): %v", err)
	}

	readOpen := make(chan struct{})
	release := make(chan struct{})
	store := &moleculeAutocloseOrdinaryCloseRaceStore{
		Store:    base,
		id:       root.ID,
		readOpen: readOpen,
		release:  release,
	}
	rec := events.NewFake()
	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		doMoleculeAutocloseWith(store, "", rec, work.ID, &out)
	}()

	select {
	case <-readOpen:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("autoclose did not reach the open-root close window")
	}
	if err := base.Close(root.ID); err != nil {
		t.Fatalf("ordinary Close(root): %v", err)
	}
	close(release)
	select {
	case <-done:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("autoclose did not return after the race was released")
	}

	rootAfter, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if got := rootAfter.Metadata["close_reason"]; got != "" {
		t.Fatalf("losing autoclose stamped root close_reason %q", got)
	}
	specAfter, err := base.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get(spec): %v", err)
	}
	if specAfter.Status != "open" {
		t.Fatalf("spec status = %q, want open until the root winner's lifecycle or residue recovery", specAfter.Status)
	}
	if len(rec.Events) != 0 || out.Len() != 0 {
		t.Fatalf("losing autoclose emitted success: events=%+v stdout=%q", rec.Events, out.String())
	}

	if retry := recoverMoleculeLifecycleIntents(base, rec); retry {
		t.Fatal("residue recovery retry = true, want complete")
	}
	specAfter, err = base.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get(spec after recovery): %v", err)
	}
	if specAfter.Status != "closed" {
		t.Fatalf("spec status after residue recovery = %q, want closed", specAfter.Status)
	}
	if got := specAfter.Metadata["close_reason"]; got != sourceworkflow.WorkflowSpecSidecarClosedReason {
		t.Fatalf("spec close_reason = %q, want %q", got, sourceworkflow.WorkflowSpecSidecarClosedReason)
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

// TestMoleculeAutocloseEmitsMoleculeResolvedWithSessionAttribution is the
// headline test for the honesty-gate C.0 attribution backbone: when a
// molecule auto-closes, an additive molecule.resolved event carries the
// state transition (from/to status, close reason) joined to the resolving
// session resolved from the root's stamped gc.session_* / gc.work_dir
// metadata. The existing bead.closed emission must remain untouched.
func TestMoleculeAutocloseEmitsMoleculeResolvedWithSessionAttribution(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{
		Title: "mol-focus-review",
		Type:  "molecule",
		Metadata: map[string]string{
			beadmeta.SessionNameMetadataKey: "polecat-gc-42",
			beadmeta.SessionIDMetadataKey:   "gc-42",
			beadmeta.StepIDMetadataKey:      "finalize",
			beadmeta.WorkDirMetadataKey:     "/home/ds/gascity-worktrees/polecat-1",
		},
	})
	step, _ := store.Create(beads.Bead{Title: "Run tests", Type: "step", ParentID: root.ID})

	_ = store.Close(step.ID)
	rec := events.NewFake()
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", rec, step.ID, &out)

	r, _ := store.Get(root.ID)
	if r.Status != "closed" {
		t.Fatalf("root not auto-closed: status=%q", r.Status)
	}

	resolved := eventsOfType(rec.Events, events.MoleculeResolved)
	if len(resolved) != 1 {
		t.Fatalf("got %d molecule.resolved events, want 1: %+v", len(resolved), rec.Events)
	}
	ev := resolved[0]
	if ev.Subject != root.ID {
		t.Errorf("Subject = %q, want root %q", ev.Subject, root.ID)
	}
	if ev.Actor == "" {
		t.Errorf("event Actor empty, want eventActor() identity")
	}

	p := decodeMoleculeResolvedPayload(t, ev)
	if p.IssueID != root.ID {
		t.Errorf("IssueID = %q, want %q", p.IssueID, root.ID)
	}
	if p.FromStatus != "open" {
		t.Errorf("FromStatus = %q, want pre-close %q", p.FromStatus, "open")
	}
	if p.ToStatus != "closed" {
		t.Errorf("ToStatus = %q, want closed", p.ToStatus)
	}
	if p.CloseReason != moleculeAutocloseReason {
		t.Errorf("CloseReason = %q, want %q", p.CloseReason, moleculeAutocloseReason)
	}
	if p.SessionName != "polecat-gc-42" {
		t.Errorf("SessionName = %q, want polecat-gc-42", p.SessionName)
	}
	if p.SessionID != "gc-42" {
		t.Errorf("SessionID = %q, want gc-42", p.SessionID)
	}
	if p.WorkDir != "/home/ds/gascity-worktrees/polecat-1" {
		t.Errorf("WorkDir = %q, want worktree path", p.WorkDir)
	}
	if p.Ts.IsZero() {
		t.Errorf("Ts is zero, want a resolution timestamp")
	}

	// Additive, not a replacement: bead.closed must still fire exactly once
	// with the durable post-close root snapshot as its canonical payload.
	assertMoleculeAutocloseBeadClosedSnapshot(t, rec.Events, r, moleculeAutocloseReason)
	if len(rec.Events) != 2 || rec.Events[0].Type != events.BeadClosed || rec.Events[1].Type != events.MoleculeResolved {
		t.Errorf("event order = %+v, want bead.closed then molecule.resolved", rec.Events)
	}
	for _, event := range rec.Events {
		if event.RunID != root.ID || event.SessionID != "gc-42" || event.StepID != "finalize" {
			t.Errorf("%s correlation = run:%q session:%q step:%q, want %q/gc-42/finalize", event.Type, event.RunID, event.SessionID, event.StepID, root.ID)
		}
	}
}

func TestMoleculeAutocloseCachingStoreEmitsOneBeadClosed(t *testing.T) {
	base := beads.NewMemStore()
	root, _ := base.Create(beads.Bead{Title: "mol", Type: "molecule"})
	step, _ := base.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID})
	rec := events.NewFake()
	store := beads.NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
		rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
	})
	if err := store.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}
	if err := store.Close(step.ID); err != nil {
		t.Fatalf("Close(step): %v", err)
	}
	start := len(rec.Events)

	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", rec, step.ID, &out)
	emitted := rec.Events[start:]
	after, _ := base.Get(root.ID)
	assertMoleculeAutocloseBeadClosedSnapshot(t, emitted, after, moleculeAutocloseReason)
	if got := len(eventsOfType(emitted, events.MoleculeResolved)); got != 1 {
		t.Errorf("got %d molecule.resolved events, want 1: %+v", got, emitted)
	}
	closedAt, resolvedAt := -1, -1
	for i, event := range emitted {
		switch event.Type {
		case events.BeadClosed:
			closedAt = i
		case events.MoleculeResolved:
			resolvedAt = i
		}
	}
	if closedAt < 0 || resolvedAt < 0 || closedAt >= resolvedAt {
		t.Errorf("event order = %+v, want bead.closed before molecule.resolved", emitted)
	}
}

func TestMoleculeAutocloseConcurrentCloseOwnsOneTransition(t *testing.T) {
	tests := []struct {
		name   string
		stores func(t *testing.T, base *beads.MemStore, rec *events.Fake) []beads.Store
	}{
		{
			name: "raw store",
			stores: func(_ *testing.T, base *beads.MemStore, _ *events.Fake) []beads.Store {
				return []beads.Store{base, base}
			},
		},
		{
			name: "cold cache",
			stores: func(_ *testing.T, base *beads.MemStore, rec *events.Fake) []beads.Store {
				cache := beads.NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
					rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
				})
				return []beads.Store{cache, cache}
			},
		},
		{
			name: "independent caches",
			stores: func(t *testing.T, base *beads.MemStore, rec *events.Fake) []beads.Store {
				newCache := func() *beads.CachingStore {
					return beads.NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
						rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
					})
				}
				first, second := newCache(), newCache()
				if err := first.PrimeActive(); err != nil {
					t.Fatalf("PrimeActive(first): %v", err)
				}
				if err := second.PrimeActive(); err != nil {
					t.Fatalf("PrimeActive(second): %v", err)
				}
				return []beads.Store{first, second}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := beads.NewMemStore()
			root, _ := base.Create(beads.Bead{Title: "mol", Type: "molecule"})
			rec := events.NewFake()
			stores := tc.stores(t, base, rec)

			begin := make(chan struct{})
			results := make(chan bool, len(stores))
			var wg sync.WaitGroup
			for _, store := range stores {
				wg.Add(1)
				go func(store beads.Store) {
					defer wg.Done()
					var out bytes.Buffer
					<-begin
					results <- announceClosedMolecule(store, rec, root, moleculeAutocloseReason, &out)
				}(store)
			}
			close(begin)
			wg.Wait()
			close(results)

			successes := 0
			for result := range results {
				if result {
					successes++
				}
			}
			if successes != 1 {
				t.Errorf("successful announcements = %d, want 1", successes)
			}
			after, _ := base.Get(root.ID)
			assertMoleculeAutocloseBeadClosedSnapshot(t, rec.Events, after, moleculeAutocloseReason)
			if got := len(eventsOfType(rec.Events, events.MoleculeResolved)); got != 1 {
				t.Errorf("got %d molecule.resolved events, want 1: %+v", got, rec.Events)
			}
		})
	}
}

func TestAnnounceClosedMoleculeAlreadyClosedPreservesReasonAndEmitsNothing(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "mol", Type: "molecule"})
	const originalReason = "closed before autoclose raced"
	if err := store.SetMetadata(root.ID, "close_reason", originalReason); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := store.Close(root.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	rec := events.NewFake()
	var out bytes.Buffer

	if announceClosedMolecule(store, rec, root, moleculeAutocloseReason, &out) {
		t.Fatal("announceClosedMolecule returned true for an already-closed molecule")
	}
	after, _ := store.Get(root.ID)
	if got := after.Metadata["close_reason"]; got != originalReason {
		t.Errorf("close_reason = %q, want preserved %q", got, originalReason)
	}
	if len(rec.Events) != 0 || out.Len() != 0 {
		t.Fatalf("already-closed molecule emitted success: events=%+v stdout=%q", rec.Events, out.String())
	}
}

func TestMoleculeAutocloseConcurrentReasonsMatchDurableWinner(t *testing.T) {
	base := beads.NewMemStore()
	root, _ := base.Create(beads.Bead{Title: "mol", Type: "molecule"})
	rec := events.NewFake()
	newCache := func() *beads.CachingStore {
		return beads.NewCachingStoreForTest(base, func(eventType, beadID string, payload json.RawMessage) {
			rec.Record(events.Event{Type: eventType, Subject: beadID, Payload: payload})
		})
	}
	stores := []beads.Store{newCache(), newCache()}
	for i, store := range stores {
		if err := store.(*beads.CachingStore).PrimeActive(); err != nil {
			t.Fatalf("PrimeActive(%d): %v", i, err)
		}
	}
	reasons := []string{
		"molecule autoclose: all step children closed",
		"source molecule autoclose: source bead closed",
	}

	begin := make(chan struct{})
	results := make(chan bool, len(stores))
	var wg sync.WaitGroup
	for i, store := range stores {
		wg.Add(1)
		go func(store beads.Store, reason string) {
			defer wg.Done()
			var out bytes.Buffer
			<-begin
			results <- announceClosedMolecule(store, rec, root, reason, &out)
		}(store, reasons[i])
	}
	close(begin)
	wg.Wait()
	close(results)

	successes := 0
	for result := range results {
		if result {
			successes++
		}
	}
	if successes != 1 {
		t.Errorf("successful announcements = %d, want 1", successes)
	}
	after, _ := base.Get(root.ID)
	durableReason := after.Metadata["close_reason"]
	if durableReason != reasons[0] && durableReason != reasons[1] {
		t.Fatalf("durable close_reason = %q, want one contender reason", durableReason)
	}
	assertMoleculeAutocloseBeadClosedSnapshot(t, rec.Events, after, durableReason)
	resolved := eventsOfType(rec.Events, events.MoleculeResolved)
	if len(resolved) != 1 {
		t.Fatalf("got %d molecule.resolved events, want 1: %+v", len(resolved), rec.Events)
	}
	if got := decodeMoleculeResolvedPayload(t, resolved[0]).CloseReason; got != durableReason {
		t.Errorf("molecule.resolved close_reason = %q, want durable %q", got, durableReason)
	}
}

func TestMoleculeAutocloseOrdinaryCloseWinningRaceEmitsNothing(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "mol", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	readOpen := make(chan struct{})
	release := make(chan struct{})
	store := &moleculeAutocloseOrdinaryCloseRaceStore{
		Store:    base,
		id:       root.ID,
		readOpen: readOpen,
		release:  release,
	}
	rec := events.NewFake()
	var out bytes.Buffer
	done := make(chan bool, 1)
	go func() {
		done <- announceClosedMolecule(store, rec, root, moleculeAutocloseReason, &out)
	}()

	select {
	case <-readOpen:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("autoclose did not reach the open-row close window")
	}
	if err := base.Close(root.ID); err != nil {
		t.Fatalf("ordinary Close: %v", err)
	}
	close(release)

	select {
	case announced := <-done:
		if announced {
			t.Fatal("autoclose claimed a transition after ordinary Close won")
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("autoclose did not return after the race was released")
	}
	after, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got := after.Metadata["close_reason"]; got != "" {
		t.Fatalf("losing autoclose stamped close_reason %q onto the ordinary winner", got)
	}
	if len(rec.Events) != 0 || out.Len() != 0 {
		t.Fatalf("losing autoclose emitted success: events=%+v stdout=%q", rec.Events, out.String())
	}
}

func TestMoleculeAutocloseUsesWinningDurableSnapshot(t *testing.T) {
	store := beads.NewMemStore()
	stale, err := store.Create(beads.Bead{
		Title: "stale title",
		Type:  "molecule",
		Metadata: map[string]string{
			beadmeta.SessionIDMetadataKey: "stale-session",
		},
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	freshTitle := "durable title"
	freshStatus := "in_progress"
	if err := store.Update(stale.ID, beads.UpdateOpts{
		Title:  &freshTitle,
		Status: &freshStatus,
		Metadata: map[string]string{
			beadmeta.SessionNameMetadataKey: "durable-agent",
			beadmeta.SessionIDMetadataKey:   "durable-session",
			beadmeta.WorkDirMetadataKey:     "/durable/worktree",
			"durable_marker":                "winner",
		},
	}); err != nil {
		t.Fatalf("Update durable row: %v", err)
	}

	rec := events.NewFake()
	var out bytes.Buffer
	if !announceClosedMolecule(store, rec, stale, moleculeAutocloseReason, &out) {
		t.Fatal("announceClosedMolecule returned false")
	}
	after, err := store.Get(stale.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertMoleculeAutocloseBeadClosedSnapshot(t, rec.Events, after, moleculeAutocloseReason)
	closed := eventsOfType(rec.Events, events.BeadClosed)
	decoded, ok := beads.DecodeBeadEventPayload(closed[0].Payload)
	if !ok {
		t.Fatalf("decode bead.closed payload: %s", closed[0].Payload)
	}
	if decoded.Title != freshTitle || decoded.Metadata["durable_marker"] != "winner" {
		t.Fatalf("bead.closed used stale caller data: %+v", decoded)
	}
	resolved := eventsOfType(rec.Events, events.MoleculeResolved)
	if len(resolved) != 1 {
		t.Fatalf("molecule.resolved events = %d, want 1", len(resolved))
	}
	payload := decodeMoleculeResolvedPayload(t, resolved[0])
	if payload.FromStatus != freshStatus {
		t.Errorf("FromStatus = %q, want durable pre-close %q", payload.FromStatus, freshStatus)
	}
	if payload.SessionName != "durable-agent" || payload.SessionID != "durable-session" || payload.WorkDir != "/durable/worktree" {
		t.Errorf("session attribution = %q/%q/%q, want durable snapshot", payload.SessionName, payload.SessionID, payload.WorkDir)
	}
	if !strings.Contains(out.String(), freshTitle) {
		t.Errorf("stdout = %q, want durable title %q", out.String(), freshTitle)
	}
}

// TestMoleculeAutocloseFailedCloseEmitsNoResolutionEvents asserts event
// emission follows the durable state transition. A failed root close must not
// claim either bead.closed or molecule.resolved, and must not print a success
// announcement.
func TestMoleculeAutocloseFailedCloseEmitsNoResolutionEvents(t *testing.T) {
	base := beads.NewMemStore()
	root, _ := base.Create(beads.Bead{Title: "mol", Type: "molecule"})
	step, _ := base.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID})
	_ = base.Close(step.ID)

	store := &moleculeAutocloseCloseFailStore{Store: base, failID: root.ID}
	rec := events.NewFake()
	var out bytes.Buffer
	if retry := doMoleculeAutocloseWith(store, "", rec, step.ID, &out).Wait(); !retry {
		t.Fatal("failed prepared close retry = false, want prompt retry")
	}

	after, _ := base.Get(root.ID)
	if after.Status == "closed" {
		t.Fatal("root status = closed after injected close failure, want open")
	}
	if got := after.Metadata["close_reason"]; got != moleculeAutocloseReason {
		t.Fatalf("failed close_reason = %q, want durable prepared reason %q", got, moleculeAutocloseReason)
	}
	if after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 ||
		after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] == "" {
		t.Fatalf("failed close lifecycle metadata = %#v, want retained durable intent", after.Metadata)
	}
	if got := len(eventsOfType(rec.Events, events.BeadClosed)); got != 0 {
		t.Errorf("got %d bead.closed events after failed close, want 0: %+v", got, rec.Events)
	}
	if got := len(eventsOfType(rec.Events, events.MoleculeResolved)); got != 0 {
		t.Errorf("got %d molecule.resolved events after failed close, want 0: %+v", got, rec.Events)
	}
	if out.Len() != 0 {
		t.Errorf("stdout = %q after failed close, want no success announcement", out.String())
	}
}

func TestMoleculeAutocloseLegacyCloseFailureResumesDurableIntent(t *testing.T) {
	base := beads.NewMemStore()
	root, _ := base.Create(beads.Bead{Title: "retryable legacy molecule", Type: "molecule"})
	step, _ := base.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID})
	_ = base.Close(step.ID)

	store := &moleculeAutocloseFailOnceLegacyCloseStore{Store: base, failID: root.ID, failures: 1}
	rec := events.NewFake()
	var out bytes.Buffer
	if retry := doMoleculeAutocloseWith(store, "", rec, step.ID, &out).Wait(); !retry {
		t.Fatal("first autoclose retry = false after injected pre-commit close failure")
	}

	pending, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get pending root: %v", err)
	}
	if pending.Status != "open" {
		t.Fatalf("pending root status = %q, want open", pending.Status)
	}
	if pending.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != moleculeLifecycleVersionV1 ||
		pending.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] == "" {
		t.Fatalf("pending lifecycle metadata = %#v, want durable v1 intent", pending.Metadata)
	}
	if len(rec.Events) != 0 {
		t.Fatalf("events after failed close = %+v, want none", rec.Events)
	}

	if retry := doMoleculeAutocloseWith(store, "", rec, step.ID, &out).Wait(); retry {
		t.Fatal("resumed autoclose retry = true, want complete")
	}
	after, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get resumed root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("resumed root status = %q, want closed", after.Status)
	}
	if got := len(eventsOfType(rec.Events, events.BeadClosed)); got != 1 {
		t.Fatalf("bead.closed events after resume = %d, want 1: %+v", got, rec.Events)
	}
	if got := len(eventsOfType(rec.Events, events.MoleculeResolved)); got != 1 {
		t.Fatalf("molecule.resolved events after resume = %d, want 1: %+v", got, rec.Events)
	}
	if after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey] != "" ||
		after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey] != "" {
		t.Fatalf("lifecycle metadata after resume = %#v, want cleared", after.Metadata)
	}
}

func TestMoleculeAutocloseLegacyPrepareAndCloseShareLifecycleLease(t *testing.T) {
	base := beads.NewMemStore()
	root, _ := base.Create(beads.Bead{Title: "lease-fenced legacy molecule", Type: "molecule"})
	step, _ := base.Create(beads.Bead{Title: "terminal step", Type: "step", ParentID: root.ID})
	_ = base.Close(step.ID)

	store := &moleculeLifecycleLeaseRaceStore{
		Store:             base,
		rootID:            root.ID,
		pendingWritten:    make(chan struct{}),
		releasePending:    make(chan struct{}),
		updateAttempted:   make(chan struct{}),
		updateObservation: make(chan string, 1),
	}
	rec := events.NewFake()
	var out bytes.Buffer
	autocloseDone := make(chan moleculeAutocloseCompletion, 1)
	go func() {
		autocloseDone <- doMoleculeAutocloseWith(store, "", rec, step.ID, &out)
	}()

	select {
	case <-store.pendingWritten:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("autoclose did not durably persist the pending lifecycle marker")
	}

	inProgress := "in_progress"
	updateDone := make(chan error, 1)
	go func() {
		updateDone <- store.Update(root.ID, beads.UpdateOpts{Status: &inProgress})
	}()
	select {
	case <-store.updateAttempted:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("racing status mutation did not attempt the lifecycle lease")
	}
	select {
	case observed := <-store.updateObservation:
		t.Fatalf("status mutation entered while lifecycle lease was held; observed root status %q", observed)
	case <-time.After(25 * time.Millisecond):
	}

	close(store.releasePending)
	var completion moleculeAutocloseCompletion
	select {
	case completion = <-autocloseDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("autoclose did not finish after releasing the pending-marker gate")
	}
	if retry := completion.Wait(); retry {
		current, _ := base.Get(root.ID)
		t.Fatalf("autoclose retry = true, want lifecycle complete; root=%+v events=%+v", current, rec.Events)
	}
	select {
	case err := <-updateDone:
		if err != nil {
			t.Fatalf("racing Update: %v", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("racing status mutation did not finish after lifecycle close")
	}
	select {
	case observed := <-store.updateObservation:
		if observed != "closed" {
			t.Fatalf("status mutation entered against %q root, want closed root after uninterrupted lease", observed)
		}
	default:
		t.Fatal("racing status mutation did not record its post-lease root observation")
	}

	after, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get root: %v", err)
	}
	if after.Status != "closed" {
		t.Fatalf("root status = %q, want closed", after.Status)
	}
}

func TestAnnounceClosedMoleculeLegacyCloseEmitsAtLeastOnceLifecycleEvents(t *testing.T) {
	tests := []struct {
		name string
		wrap func(beads.Store) beads.Store
	}{
		{
			name: "capability absent",
			wrap: func(store beads.Store) beads.Store {
				return &moleculeAutocloseNoTransitionStore{Store: store}
			},
		},
		{
			name: "capability unsupported at runtime",
			wrap: func(store beads.Store) beads.Store {
				return &moleculeAutocloseUnsupportedTransitionStore{Store: store}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			base := beads.NewMemStore()
			root, _ := base.Create(beads.Bead{Title: "mol", Type: "molecule"})
			store := tc.wrap(base)
			rec := events.NewFake()
			var out bytes.Buffer

			if !announceClosedMolecule(store, rec, root, moleculeAutocloseReason, &out) {
				t.Fatal("announceClosedMolecule returned false for the legacy close path")
			}
			after, _ := base.Get(root.ID)
			if after.Status != "closed" || after.Metadata["close_reason"] != moleculeAutocloseReason {
				t.Fatalf("legacy autoclose result = %+v, want closed root with reason", after)
			}
			assertMoleculeAutocloseBeadClosedSnapshot(t, rec.Events, after, moleculeAutocloseReason)
			resolved := eventsOfType(rec.Events, events.MoleculeResolved)
			if len(resolved) != 1 {
				t.Fatalf("legacy molecule.resolved events = %d, want 1: %+v", len(resolved), rec.Events)
			}
			payload := decodeMoleculeResolvedPayload(t, resolved[0])
			if payload.FromStatus != root.Status || payload.CloseReason != moleculeAutocloseReason {
				t.Fatalf("legacy molecule.resolved payload = %+v, want from_status %q and durable reason", payload, root.Status)
			}
			if !strings.Contains(out.String(), "Auto-closed molecule") || !strings.Contains(out.String(), "lifecycle delivery is at-least-once") {
				t.Fatalf("stdout = %q, want close announcement and at-least-once diagnostic", out.String())
			}
		})
	}
}

func TestAnnounceClosedMoleculeCommitThenErrorPublishesFromDurableIntent(t *testing.T) {
	base := beads.NewMemStore()
	root, err := base.Create(beads.Bead{Title: "ambiguous close", Type: "molecule"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	store := &moleculeAutocloseCommitErrorStore{Store: base}
	rec := events.NewFake()
	var out bytes.Buffer

	if !announceClosedMolecule(store, rec, root, moleculeAutocloseReason, &out) {
		t.Fatal("announceClosedMolecule returned false after the live row confirmed the ambiguous close committed")
	}
	after, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	assertMoleculeAutocloseBeadClosedSnapshot(t, rec.Events, after, moleculeAutocloseReason)
	if got := len(eventsOfType(rec.Events, events.MoleculeResolved)); got != 1 {
		t.Fatalf("molecule.resolved events = %d, want 1", got)
	}
	if got := after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey]; got != "" {
		t.Fatalf("pending marker = %q, want cleared after confirmed publication", got)
	}
	if got := after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey]; got != "" {
		t.Fatalf("intent = %q, want cleared after confirmed publication", got)
	}
}

func TestMoleculeAutocloseLegacyCachedStorePublishesSidecarCloseEdgeOnceAndRecorderOwnsRoot(t *testing.T) {
	tests := []struct {
		name         string
		backing      func(beads.Store) beads.Store
		wrapStore    func(beads.Store) beads.Store
		wrapPolicies bool
	}{
		{
			name: "capability absent",
			backing: func(store beads.Store) beads.Store {
				return &moleculeAutocloseNoTransitionStore{Store: store}
			},
		},
		{
			name: "capability unsupported at runtime",
			backing: func(store beads.Store) beads.Store {
				return &moleculeAutocloseUnsupportedTransitionStore{Store: store}
			},
		},
		{
			name: "production policy wrapper",
			backing: func(store beads.Store) beads.Store {
				return &moleculeAutocloseUnsupportedTransitionStore{Store: store}
			},
			wrapPolicies: true,
		},
		{
			name: "runtime unsupported forwarding wrapper",
			backing: func(store beads.Store) beads.Store {
				return &moleculeAutocloseNoTransitionStore{Store: store}
			},
			wrapStore: func(store beads.Store) beads.Store {
				return &moleculeAutocloseUnsupportedTransitionStore{Store: store}
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			type observedEvent struct {
				eventType string
				beadID    string
			}
			var observed []observedEvent
			cache := beads.NewCachingStoreForTest(tc.backing(beads.NewMemStore()), func(eventType, beadID string, _ json.RawMessage) {
				observed = append(observed, observedEvent{eventType: eventType, beadID: beadID})
			})
			work, _ := cache.Create(beads.Bead{Title: "fix the bug", Type: "task"})
			root, _ := cache.Create(beads.Bead{
				Title: "mol-focus-review",
				Type:  "task",
				Metadata: map[string]string{
					"gc.kind":             "workflow",
					"gc.formula_contract": "graph.v2",
					"gc.source_bead_id":   work.ID,
				},
			})
			spec, _ := cache.Create(beads.Bead{
				Title: "generated step spec",
				Type:  "spec",
				Metadata: map[string]string{
					"gc.kind":         "spec",
					"gc.root_bead_id": root.ID,
					"gc.spec_for":     "implement",
					"gc.spec_for_ref": "implement",
				},
			})
			if err := cache.Close(work.ID); err != nil {
				t.Fatalf("Close(work): %v", err)
			}
			observed = nil

			var store beads.Store = cache
			if tc.wrapStore != nil {
				store = tc.wrapStore(store)
			}
			if tc.wrapPolicies {
				store = wrapStoreWithBeadPolicies(store, nil)
			}
			rec := events.NewFake()
			var out bytes.Buffer
			if retry := doMoleculeAutocloseWith(store, "", rec, work.ID, &out).Wait(); retry {
				t.Fatal("cached autoclose retry = true, want lifecycle complete")
			}

			rootAfter, _ := cache.Get(root.ID)
			if rootAfter.Status != "closed" {
				t.Fatalf("root status = %q, want closed", rootAfter.Status)
			}
			specAfter, _ := cache.Get(spec.ID)
			if specAfter.Status != "closed" {
				t.Fatalf("spec status = %q, want closed", specAfter.Status)
			}
			assertMoleculeAutocloseBeadClosedSnapshot(t, rec.Events, rootAfter, moleculeSourceAutocloseReason)
			if got := len(eventsOfType(rec.Events, events.MoleculeResolved)); got != 1 {
				t.Fatalf("legacy molecule.resolved events = %d, want 1: %+v", got, rec.Events)
			}
			rootObserverEvents := 0
			sidecarObserverEvents := 0
			for _, event := range observed {
				if event.eventType != events.BeadClosed {
					continue
				}
				switch event.beadID {
				case root.ID:
					rootObserverEvents++
				case spec.ID:
					sidecarObserverEvents++
				}
			}
			if rootObserverEvents != 0 {
				t.Fatalf("root observer events = %d, want 0 because the recorder owns fallback emission: %+v", rootObserverEvents, observed)
			}
			// A definite successful legacy close publishes its close edge via the
			// observer: eventexport drops bead.updated, so suppressing the edge
			// here would erase the sidecar's close from accounting. The recorder
			// never emits for sidecars, so exactly one edge means no duplicate.
			if sidecarObserverEvents != 1 {
				t.Fatalf("sidecar observer events = %d, want exactly one definite legacy close edge: %+v", sidecarObserverEvents, observed)
			}
			if !strings.Contains(out.String(), "lifecycle delivery is at-least-once") {
				t.Fatalf("stdout = %q, want at-least-once diagnostic", out.String())
			}
		})
	}
}

func TestAnnounceClosedMoleculeRetriesPostCloseGetForAuthoritativeLifecyclePayload(t *testing.T) {
	const (
		expectedPostCloseReadAttempts = 3
		// After the retry loop finds the authoritative row, the publication
		// transaction reads it once more while also validating cleanup ownership.
		// Marker-last cleanup then reads once after clearing the marker before
		// removing the owned intent.
		expectedPostCloseLifecycleReads = expectedPostCloseReadAttempts + 2
	)

	base := beads.NewMemStore()
	root, _ := base.Create(beads.Bead{Title: "mol", Type: "molecule"})
	store := &moleculeAutoclosePostCloseGetFailStore{
		Store:                 base,
		failID:                root.ID,
		postCloseFailuresLeft: expectedPostCloseReadAttempts - 1,
	}
	rec := events.NewFake()
	var out bytes.Buffer

	if !announceClosedMolecule(store, rec, root, moleculeAutocloseReason, &out) {
		t.Fatal("announceClosedMolecule returned false after the authoritative reread recovered")
	}
	after, _ := base.Get(root.ID)
	if after.Status != "closed" {
		t.Fatalf("root status = %q, want closed", after.Status)
	}
	if got := store.postCloseGetCount(); got != expectedPostCloseLifecycleReads {
		t.Fatalf("post-close lifecycle Get calls = %d, want %d", got, expectedPostCloseLifecycleReads)
	}
	assertMoleculeAutocloseBeadClosedSnapshot(t, rec.Events, after, moleculeAutocloseReason)
	if got := len(eventsOfType(rec.Events, events.MoleculeResolved)); got != 1 {
		t.Errorf("got %d molecule.resolved events, want 1", got)
	}
}

func TestAnnounceClosedMoleculePostCloseGetFailureLeavesDurableIntentForRecovery(t *testing.T) {
	const expectedPostCloseReadAttempts = 3

	base := beads.NewMemStore()
	root, _ := base.Create(beads.Bead{Title: "mol", Type: "molecule"})
	store := &moleculeAutoclosePostCloseGetFailStore{
		Store:               base,
		failID:              root.ID,
		failAllPostCloseGet: true,
	}
	rec := events.NewFake()
	var out bytes.Buffer

	if !announceClosedMolecule(store, rec, root, moleculeAutocloseReason, &out) {
		t.Fatal("announceClosedMolecule returned false after a successful durable close")
	}
	after, _ := base.Get(root.ID)
	if after.Status != "closed" {
		t.Fatalf("root status = %q, want closed", after.Status)
	}
	if got := after.Metadata["close_reason"]; got != moleculeAutocloseReason {
		t.Fatalf("root close_reason = %q, want %q", got, moleculeAutocloseReason)
	}
	if got := store.postCloseGetCount(); got != expectedPostCloseReadAttempts {
		t.Fatalf("post-close Get calls = %d, want bounded %d", got, expectedPostCloseReadAttempts)
	}
	if len(rec.Events) != 0 {
		t.Fatalf("events = %+v, want none without an authoritative post-close snapshot", rec.Events)
	}
	if got := after.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey]; got != moleculeLifecycleVersionV1 {
		t.Fatalf("pending marker = %q, want durable v1 recovery marker", got)
	}
	if got := after.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey]; got == "" {
		t.Fatal("durable lifecycle intent is empty after bounded live reads failed")
	}
	if diagnostic := out.String(); !strings.Contains(diagnostic, "Auto-closed molecule "+root.ID) || !strings.Contains(diagnostic, "lifecycle publication pending authoritative recovery") {
		t.Errorf("stdout = %q, want an honest pending-recovery diagnostic", diagnostic)
	}

	store.failAllPostCloseGet = false
	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("recoverMoleculeLifecycleIntents retry = true after live reads recovered")
	}
	afterRecovery, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get after recovery: %v", err)
	}
	assertMoleculeAutocloseBeadClosedSnapshot(t, rec.Events, after, moleculeAutocloseReason)
	if got := len(eventsOfType(rec.Events, events.MoleculeResolved)); got != 1 {
		t.Fatalf("molecule.resolved events after recovery = %d, want 1", got)
	}
	if got := afterRecovery.Metadata[beadmeta.MoleculeLifecyclePendingMetadataKey]; got != "" {
		t.Fatalf("pending marker after recovery = %q, want cleared", got)
	}
	if got := afterRecovery.Metadata[beadmeta.MoleculeLifecycleIntentMetadataKey]; got != "" {
		t.Fatalf("intent after recovery = %q, want cleared", got)
	}
}

func TestMoleculeAutocloseRecoveryClosesSpecSidecarsAfterRootLifecycle(t *testing.T) {
	base := beads.NewMemStore()
	work, _ := base.Create(beads.Bead{Title: "fix the bug", Type: "task"})
	root, _ := base.Create(beads.Bead{
		Title: "mol-focus-review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":           "workflow",
			"gc.source_bead_id": work.ID,
		},
	})
	spec, _ := base.Create(beads.Bead{
		Title: "generated step spec",
		Type:  "spec",
		Metadata: map[string]string{
			"gc.kind":         "spec",
			"gc.root_bead_id": root.ID,
			"gc.spec_for":     "implement",
			"gc.spec_for_ref": "implement",
		},
	})
	if err := base.Close(work.ID); err != nil {
		t.Fatalf("Close(work): %v", err)
	}
	store := &moleculeAutoclosePostCloseGetFailStore{
		Store:               base,
		failID:              root.ID,
		failAllPostCloseGet: true,
	}
	rec := events.NewFake()
	var out bytes.Buffer

	doMoleculeAutocloseWith(store, "", rec, work.ID, &out)

	rootAfter, err := base.Get(root.ID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if rootAfter.Status != "closed" {
		t.Fatalf("root status = %q, want closed", rootAfter.Status)
	}
	specAfter, err := base.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get(spec): %v", err)
	}
	if specAfter.Status != "open" {
		t.Fatalf("spec status before root lifecycle recovery = %q, want open", specAfter.Status)
	}
	if len(rec.Events) != 0 {
		t.Fatalf("events before authoritative root recovery = %+v, want none", rec.Events)
	}

	store.failAllPostCloseGet = false
	if retry := recoverMoleculeLifecycleIntents(store, rec); retry {
		t.Fatal("recoverMoleculeLifecycleIntents retry = true after live reads recovered")
	}
	specAfter, err = base.Get(spec.ID)
	if err != nil {
		t.Fatalf("Get(spec after recovery): %v", err)
	}
	if specAfter.Status != "closed" {
		t.Fatalf("spec status after root lifecycle recovery = %q, want closed", specAfter.Status)
	}
	if got := specAfter.Metadata["close_reason"]; got != sourceworkflow.WorkflowSpecSidecarClosedReason {
		t.Fatalf("spec close_reason = %q, want %q", got, sourceworkflow.WorkflowSpecSidecarClosedReason)
	}
	if got := len(eventsOfType(rec.Events, events.BeadClosed)); got != 1 {
		t.Fatalf("bead.closed events after recovery = %d, want 1", got)
	}
	if got := len(eventsOfType(rec.Events, events.MoleculeResolved)); got != 1 {
		t.Fatalf("molecule.resolved events after recovery = %d, want 1", got)
	}
}

// TestMoleculeAutocloseMoleculeResolvedDegradesWithoutStampedSession asserts
// the build-time edge the spec pins: a molecule that resolves before any
// reconcile stamped its identity emits molecule.resolved with empty session
// fields — graceful degradation, not a crash.
func TestMoleculeAutocloseMoleculeResolvedDegradesWithoutStampedSession(t *testing.T) {
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "mol", Type: "molecule"})
	step, _ := store.Create(beads.Bead{Title: "step", Type: "step", ParentID: root.ID})

	_ = store.Close(step.ID)
	rec := events.NewFake()
	var out bytes.Buffer
	doMoleculeAutocloseWith(store, "", rec, step.ID, &out)

	resolved := eventsOfType(rec.Events, events.MoleculeResolved)
	if len(resolved) != 1 {
		t.Fatalf("got %d molecule.resolved events, want 1 (graceful, not crash): %+v", len(resolved), rec.Events)
	}
	p := decodeMoleculeResolvedPayload(t, resolved[0])
	if p.SessionName != "" || p.SessionID != "" || p.WorkDir != "" {
		t.Errorf("unstamped root must degrade to empty session fields, got name=%q id=%q dir=%q", p.SessionName, p.SessionID, p.WorkDir)
	}
	if p.IssueID != root.ID {
		t.Errorf("IssueID = %q, want %q", p.IssueID, root.ID)
	}
	if p.ToStatus != "closed" {
		t.Errorf("ToStatus = %q, want closed", p.ToStatus)
	}
}

// eventsOfType returns the subset of evs whose Type equals typ.
func eventsOfType(evs []events.Event, typ string) []events.Event {
	var out []events.Event
	for _, e := range evs {
		if e.Type == typ {
			out = append(out, e)
		}
	}
	return out
}

// assertMoleculeAutocloseBeadClosedSnapshot verifies the bead.closed wire
// contract used by event consumers: one event whose payload is the canonical
// post-close bead snapshot, including the persisted close reason.
func assertMoleculeAutocloseBeadClosedSnapshot(t *testing.T, evs []events.Event, want beads.Bead, wantReason string) {
	t.Helper()
	closed := eventsOfType(evs, events.BeadClosed)
	if len(closed) != 1 {
		t.Fatalf("got %d bead.closed events, want 1: %+v", len(closed), evs)
	}
	if closed[0].Subject != want.ID {
		t.Errorf("bead.closed Subject = %q, want %q", closed[0].Subject, want.ID)
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(closed[0].Payload, &raw); err != nil {
		t.Fatalf("unmarshal canonical bead.closed payload: %v", err)
	}
	if _, ok := raw["id"]; !ok {
		t.Errorf("bead.closed payload has no top-level id: %s", closed[0].Payload)
	}
	if _, wrapped := raw["bead"]; wrapped {
		t.Errorf("bead.closed payload uses wrapped fallback instead of canonical raw snapshot: %s", closed[0].Payload)
	}
	got, ok := beads.DecodeBeadEventPayload(closed[0].Payload)
	if !ok {
		t.Fatalf("bead.closed payload did not decode as a bead snapshot: %s", closed[0].Payload)
	}
	if got.ID != want.ID {
		t.Errorf("bead.closed payload ID = %q, want %q", got.ID, want.ID)
	}
	if got.Status != "closed" {
		t.Errorf("bead.closed payload status = %q, want closed", got.Status)
	}
	if got.Title != want.Title || got.Type != want.Type {
		t.Errorf("bead.closed payload identity = title:%q type:%q, want title:%q type:%q", got.Title, got.Type, want.Title, want.Type)
	}
	if got.Metadata["close_reason"] != wantReason {
		t.Errorf("bead.closed payload close_reason = %q, want %q", got.Metadata["close_reason"], wantReason)
	}
}

type moleculeAutocloseCloseFailStore struct {
	beads.Store
	failID string
}

func (s *moleculeAutocloseCloseFailStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *moleculeAutocloseCloseFailStore) Close(id string) error {
	if id == s.failID {
		return errors.New("injected molecule close failure")
	}
	return s.Store.Close(id)
}

type moleculeAutocloseNoTransitionStore struct {
	beads.Store
}

func (s *moleculeAutocloseNoTransitionStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

type moleculeAutocloseFailOnceLegacyCloseStore struct {
	beads.Store
	mu       sync.Mutex
	failID   string
	failures int
}

func (s *moleculeAutocloseFailOnceLegacyCloseStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

type moleculeLifecycleLeaseRaceStore struct {
	beads.Store
	rootID string

	lease sync.Mutex

	pendingWritten    chan struct{}
	releasePending    chan struct{}
	updateAttempted   chan struct{}
	updateObservation chan string
	pendingOnce       sync.Once
	updateOnce        sync.Once
}

func (s *moleculeLifecycleLeaseRaceStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *moleculeLifecycleLeaseRaceStore) WithLifecycleMetadataTransaction(id string, fn func(beads.LifecycleMetadataTransaction) error) error {
	s.lease.Lock()
	tx := &moleculeLifecycleLeaseRaceTransaction{store: s, id: id}
	err := fn(tx)
	s.lease.Unlock()
	if tx.prepared && !tx.closed {
		// Make the old prepare-then-close gap deterministic: once the prepare
		// callback releases the lease, let the already-waiting status writer win
		// before returning to the caller's separate close operation.
		observed := <-s.updateObservation
		s.updateObservation <- observed
	}
	return err
}

func (s *moleculeLifecycleLeaseRaceStore) Update(id string, opts beads.UpdateOpts) error {
	s.updateOnce.Do(func() { close(s.updateAttempted) })
	s.lease.Lock()
	defer s.lease.Unlock()
	fresh, err := s.Get(id)
	if err != nil {
		return err
	}
	s.updateObservation <- fresh.Status
	if fresh.Status == "closed" {
		return nil
	}
	return s.Store.Update(id, opts)
}

func (s *moleculeLifecycleLeaseRaceStore) Close(id string) error {
	s.lease.Lock()
	defer s.lease.Unlock()
	return s.Store.Close(id)
}

type moleculeLifecycleLeaseRaceTransaction struct {
	store    *moleculeLifecycleLeaseRaceStore
	id       string
	prepared bool
	closed   bool
}

func (tx *moleculeLifecycleLeaseRaceTransaction) Get() (beads.Bead, error) {
	return tx.store.Get(tx.id)
}

func (tx *moleculeLifecycleLeaseRaceTransaction) GetByID(id string) (beads.Bead, error) {
	return tx.store.Get(id)
}

func (tx *moleculeLifecycleLeaseRaceTransaction) List(query beads.ListQuery) ([]beads.Bead, error) {
	return beads.HandlesFor(tx.store.Store).Live.List(query)
}

func (tx *moleculeLifecycleLeaseRaceTransaction) SetMetadata(key, value string) error {
	if err := tx.store.SetMetadata(tx.id, key, value); err != nil {
		return err
	}
	if key == beadmeta.MoleculeLifecyclePendingMetadataKey && value == moleculeLifecycleVersionV1 {
		tx.prepared = true
		tx.store.pendingOnce.Do(func() { close(tx.store.pendingWritten) })
		<-tx.store.releasePending
	}
	return nil
}

func (tx *moleculeLifecycleLeaseRaceTransaction) SetMetadataBatch(values map[string]string) error {
	return tx.store.SetMetadataBatch(tx.id, values)
}

func (tx *moleculeLifecycleLeaseRaceTransaction) CloseWithReasonWithoutObserver(string) (beads.LifecycleCloseResult, error) {
	before, err := tx.Get()
	if err != nil {
		return beads.LifecycleCloseResult{}, err
	}
	tx.closed = true
	if err := tx.store.Store.Close(tx.id); err != nil {
		return beads.LifecycleCloseResult{Before: before}, err
	}
	after, err := tx.Get()
	return beads.LifecycleCloseResult{
		Before:         before,
		After:          after,
		Transitioned:   err == nil && before.Status != "closed" && after.Status == "closed",
		CloseSucceeded: true,
	}, err
}

func (s *moleculeAutocloseFailOnceLegacyCloseStore) Close(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if id == s.failID && s.failures > 0 {
		s.failures--
		return errors.New("injected pre-commit legacy close failure")
	}
	return s.Store.Close(id)
}

type moleculeAutocloseCommitErrorStore struct {
	beads.Store
}

func (s *moleculeAutocloseCommitErrorStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *moleculeAutocloseCommitErrorStore) Close(id string) error {
	if err := s.Store.Close(id); err != nil {
		return err
	}
	return errors.New("injected transport error after committed close")
}

type moleculeAutocloseUnsupportedTransitionStore struct {
	beads.Store
}

func (s *moleculeAutocloseUnsupportedTransitionStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *moleculeAutocloseUnsupportedTransitionStore) CloseWithReasonIfOpen(string, string) (beads.CloseTransition, error) {
	return beads.CloseTransition{}, beads.ErrCloseTransitionUnsupported
}

func (s *moleculeAutocloseUnsupportedTransitionStore) CloseObserverSuppressorHandle() (beads.CloseObserverSuppressor, bool) {
	return beads.CloseObserverSuppressorFor(s.Store)
}

type moleculeAutocloseOrdinaryCloseRaceStore struct {
	beads.Store
	id       string
	readOpen chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (s *moleculeAutocloseOrdinaryCloseRaceStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *moleculeAutocloseOrdinaryCloseRaceStore) gatedOpen(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err == nil && id == s.id && b.Status != "closed" {
		s.once.Do(func() {
			close(s.readOpen)
			<-s.release
		})
	}
	return b, err
}

func (s *moleculeAutocloseOrdinaryCloseRaceStore) Get(id string) (beads.Bead, error) {
	return s.gatedOpen(id)
}

func (s *moleculeAutocloseOrdinaryCloseRaceStore) CloseWithReasonIfOpen(id, reason string) (beads.CloseTransition, error) {
	if _, err := s.gatedOpen(id); err != nil {
		return beads.CloseTransition{}, err
	}
	closer, ok := beads.CloseTransitionerFor(s.Store)
	if !ok {
		return beads.CloseTransition{}, beads.ErrCloseTransitionUnsupported
	}
	return closer.CloseWithReasonIfOpen(id, reason)
}

type moleculeAutoclosePostCloseGetFailStore struct {
	beads.Store
	failID                 string
	failAllPostCloseGet    bool
	postCloseFailuresLeft  int
	postCloseGetCountMu    sync.Mutex
	postCloseGetCountValue int
}

func (s *moleculeAutoclosePostCloseGetFailStore) ConditionalWritesResolveTarget() beads.Store {
	return s.Store
}

func (s *moleculeAutoclosePostCloseGetFailStore) Get(id string) (beads.Bead, error) {
	b, err := s.Store.Get(id)
	if err == nil && id == s.failID && b.Status == "closed" {
		s.postCloseGetCountMu.Lock()
		s.postCloseGetCountValue++
		fail := s.failAllPostCloseGet || s.postCloseFailuresLeft > 0
		if s.postCloseFailuresLeft > 0 {
			s.postCloseFailuresLeft--
		}
		s.postCloseGetCountMu.Unlock()
		if fail {
			return beads.Bead{}, errors.New("injected post-close get failure")
		}
	}
	return b, err
}

func (s *moleculeAutoclosePostCloseGetFailStore) postCloseGetCount() int {
	s.postCloseGetCountMu.Lock()
	defer s.postCloseGetCountMu.Unlock()
	return s.postCloseGetCountValue
}

func (s *moleculeAutocloseCloseFailStore) CloseWithReasonIfOpen(id, reason string) (beads.CloseTransition, error) {
	if id == s.failID {
		return beads.CloseTransition{}, errors.New("injected molecule close failure")
	}
	closer, ok := beads.CloseTransitionerFor(s.Store)
	if !ok {
		return beads.CloseTransition{}, beads.ErrCloseTransitionUnsupported
	}
	return closer.CloseWithReasonIfOpen(id, reason)
}

// decodeMoleculeResolvedPayload unmarshals the typed molecule.resolved payload
// off a recorded event, failing the test on a malformed payload.
func decodeMoleculeResolvedPayload(t *testing.T, ev events.Event) gcapi.MoleculeResolvedPayload {
	t.Helper()
	var p gcapi.MoleculeResolvedPayload
	if err := json.Unmarshal(ev.Payload, &p); err != nil {
		t.Fatalf("unmarshal molecule.resolved payload: %v", err)
	}
	return p
}
