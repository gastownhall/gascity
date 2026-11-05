package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// mustCreateOpen creates a bead and leaves it in the default open state.
func mustCreateOpen(t *testing.T, store *beads.MemStore, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	return created
}

// TestResolveActiveWispStep_PreClaimBridgeMisses reproduces ga-cjjtkh: an
// attached (v1) molecule is never entered because the molecule_id bridge cannot
// resolve at the moment the injection actually runs.
//
// Live shape this mirrors (beads rig, 2026-08-14T23:41:23Z):
//
//	be-08y  molecule root, gc.formula_name=mol-deployer-gate, open, UNASSIGNED
//	be-01h be-34a be-770 be-awb  4 steps, open, no labels, no assignee
//	be-9b8  source bead, gc.routed_to=beads/deployer, molecule_id=be-08y
//
// cliBeadRouter.Route (cmd/gc/cmd_sling.go:776) stamps ONLY gc.routed_to, so at
// wake/nudge time the source bead is still status=open and unassigned — the
// tier-3 pool query (`bd ready --metadata-field gc.routed_to=X --unassigned`)
// requires exactly that. But resolveMoleculeRootViaBridge
// (cmd/gc/wisp_step_inject.go:189-198) lists with Status:"in_progress" +
// Assignees, so it cannot match the very bead it is meant to bridge from.
//
// The injection at cmd/gc/cmd_nudge.go:454 therefore returns "" on the wake that
// is supposed to carry the agent into its molecule. The agent works the source
// bead plainly, closes it, and the molecule leaks with every step untouched
// (created_at == updated_at).
func TestResolveActiveWispStep_PreClaimBridgeMisses(t *testing.T) {
	workStore := beads.NewMemStore()
	graphStore := beads.NewMemStoreFrom(100, nil, nil)

	// Molecule root: attached formulas deliberately leave it unrouted and
	// unassigned (sling_core.go:585-594). Keep graph data in a disjoint store
	// so a same-store fixture cannot hide an inverted bridge lookup.
	root := mustCreateOpen(t, graphStore, beads.Bead{
		Title: "mol-deployer-gate",
		Type:  "molecule",
	})

	// Step children: open, unassigned, no labels — invisible to every
	// work_query by design.
	step := mustCreateOpen(t, graphStore, beads.Bead{
		Title:       "Step 1: verify the gate",
		Description: "Run the deployer gate checks",
		Type:        "step",
		ParentID:    root.ID,
	})

	// Source work bead in its REAL post-sling, pre-claim state: routed, carrying
	// molecule_id, but still open and unassigned.
	source := mustCreateOpen(t, workStore, beads.Bead{
		Title:       "needs-deploy: ship the reviewed branch",
		Description: "Deploy the reviewed work",
		Type:        "task",
	})
	if err := workStore.SetMetadata(source.ID, beadmeta.MoleculeIDMetadataKey, root.ID); err != nil {
		t.Fatalf("SetMetadata(molecule_id): %v", err)
	}
	if err := workStore.SetMetadata(source.ID, beadmeta.RoutedToMetadataKey, "beads/deployer"); err != nil {
		t.Fatalf("SetMetadata(gc.routed_to): %v", err)
	}

	// This is the wake: gc nudge --inject calls wispStepInjectionContent, which
	// resolves against the agent's identities.
	got, err := resolveActiveWispStep(workStore, graphStore, []string{"beads/deployer", "deployer-gm-wisp-sl677d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatalf("ORPHAN REPRODUCED: molecule %s has entry step %s, but the bridge "+
			"resolved nothing at wake time because source bead %s is still "+
			"status=open/unassigned. Nothing tells the agent the molecule exists; "+
			"it works the source bead plainly and all %d steps leak untouched.",
			root.ID, step.ID, source.ID, 1)
	}
	if got.ID != step.ID {
		t.Fatalf("got %q, want entry step %q", got.ID, step.ID)
	}
}

// TestResolveActiveWispStep_PostClaimBridgeResolves is the control that isolates
// the cause to the status/assignee filter alone. It is byte-identical to the
// test above except the source bead is in_progress and assigned — the state that
// only exists AFTER the agent has already claimed and begun working. Here the
// bridge resolves fine.
//
// Together the pair show the defect is an ordering inversion, not a missing
// mechanism: the bridge requires post-claim state to deliver a pre-claim
// instruction.
func TestResolveActiveWispStep_PostClaimBridgeResolves(t *testing.T) {
	workStore := beads.NewMemStore()
	graphStore := beads.NewMemStoreFrom(100, nil, nil)

	root := mustCreateOpen(t, graphStore, beads.Bead{
		Title: "mol-deployer-gate",
		Type:  "molecule",
	})
	step := mustCreateOpen(t, graphStore, beads.Bead{
		Title:       "Step 1: verify the gate",
		Description: "Run the deployer gate checks",
		Type:        "step",
		ParentID:    root.ID,
	})

	// Same bead, but claimed: in_progress + assigned.
	source := mustCreateInProgress(t, workStore, beads.Bead{
		Title:       "needs-deploy: ship the reviewed branch",
		Description: "Deploy the reviewed work",
		Type:        "task",
		Assignee:    "deployer-gm-wisp-sl677d",
	})
	if err := workStore.SetMetadata(source.ID, beadmeta.MoleculeIDMetadataKey, root.ID); err != nil {
		t.Fatalf("SetMetadata(molecule_id): %v", err)
	}

	got, err := resolveActiveWispStep(workStore, graphStore, []string{"beads/deployer", "deployer-gm-wisp-sl677d"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("control failed: bridge did not resolve even post-claim")
	}
	if got.ID != step.ID {
		t.Fatalf("got %q, want entry step %q", got.ID, step.ID)
	}
}
