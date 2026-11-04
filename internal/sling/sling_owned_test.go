package sling

import (
	"maps"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestExistingGraphV2RootTakesOnlyARoot (architecture B): a task carrying a
// launch's gc.graphv2_root_key neither stands in for its root nor makes the
// launch ambiguous ("multiple live roots"): with none, the launch mints;
// with the real root, it resumes that one.
func TestExistingGraphV2RootTakesOnlyARoot(t *testing.T) {
	store := beads.NewMemStore()
	recipe := &formula.Recipe{Steps: []formula.RecipeStep{{ID: "root", Metadata: map[string]string{beadmeta.Graphv2RootKeyMetadataKey: "k"}}}}
	if _, err := store.Create(beads.Bead{Title: "forged", Type: "task", Metadata: map[string]string{beadmeta.Graphv2RootKeyMetadataKey: "k"}}); err != nil {
		t.Fatal(err)
	}
	if got, err := existingGraphV2Root(store, recipe); err != nil || got != nil {
		t.Fatalf("existingGraphV2Root = %+v (%v), want none", got, err)
	}
	root, err := store.Create(beads.Bead{Title: "root", Type: "task", Metadata: map[string]string{beadmeta.Graphv2RootKeyMetadataKey: "k", beadmeta.KindMetadataKey: beadmeta.KindWorkflow, beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2}})
	if err != nil {
		t.Fatal(err)
	}
	if got, err := existingGraphV2Root(store, recipe); err != nil || got == nil || got.RootID != root.ID {
		t.Fatalf("existingGraphV2Root = %+v (%v), want %s", got, err, root.ID)
	}
}

// TestDoSlingBatchRefusesAControllerOwnedChild (architecture B, the #7742
// review): a convoy tracking a controller-owned row (a session row, a
// startup-health episode, a convergence root) slings its work children and
// refuses each owned one on its own, untouched, with sling's force too.
func TestDoSlingBatchRefusesAControllerOwnedChild(t *testing.T) {
	for _, force := range []bool{false, true} {
		runner := newFakeRunner()
		deps := testDeps(&config.City{Workspace: config.Workspace{Name: "test"}}, runtime.NewFake(), runner.run)
		store := deps.Store
		convoy, err := store.Create(beads.Bead{Title: "convoy", Type: "convoy"})
		if err != nil {
			t.Fatal(err)
		}
		work, err := store.Create(beads.Bead{Title: "work", Type: "task", Status: "open"})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.DepAdd(convoy.ID, work.ID, "tracks"); err != nil {
			t.Fatal(err)
		}
		owned := map[string]beads.Bead{}
		for _, typ := range []string{"startup-health-episode", "Convergence", "task", "message", "order-run"} {
			b, err := store.Create(beads.Bead{Title: typ, Type: "task", Status: "open"})
			if err != nil {
				t.Fatal(err)
			}
			switch typ {
			case "task":
				if err := store.Update(b.ID, beads.UpdateOpts{Labels: []string{"gc:session"}}); err != nil {
					t.Fatal(err)
				}
			case "order-run": // a coordination row by its run label (the #7766 review)
				if err := store.Update(b.ID, beads.UpdateOpts{Labels: []string{"order-run:digest"}}); err != nil {
					t.Fatal(err)
				}
			default:
				if err := store.Update(b.ID, beads.UpdateOpts{Type: &typ}); err != nil {
					t.Fatal(err)
				}
			}
			if err := store.DepAdd(convoy.ID, b.ID, "tracks"); err != nil {
				t.Fatal(err)
			}
			owned[b.ID], _ = store.Get(b.ID)
		}
		a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}
		result, err := DoSlingBatch(SlingOpts{Target: a, BeadOrFormula: convoy.ID, Force: force}, deps, store)
		if err != nil && result.Routed == 0 {
			t.Fatalf("force %t: DoSlingBatch: %v", force, err)
		}
		if result.Routed != 1 || result.Failed != len(owned) {
			t.Fatalf("force %t: routed %d failed %d, want 1 and %d", force, result.Routed, result.Failed, len(owned))
		}
		for _, c := range result.Children {
			if before, ok := owned[c.BeadID]; ok {
				if !c.Failed || !strings.Contains(c.FailReason, "is owned") {
					t.Fatalf("force %t: owned child %+v, want refused", force, c)
				}
				if got, _ := store.Get(c.BeadID); got.Assignee != before.Assignee || got.Status != before.Status || !maps.Equal(got.Metadata, before.Metadata) {
					t.Fatalf("force %t: owned child %+v -> %+v, want untouched", force, before, got)
				}
			}
		}
	}
}

// TestCollectAttachedBeadsSkipsAControllerOwnedRow (the #7742 review): an
// owned row a parent's molecule_id or workflow_id names, or an owned child
// marked as an attached root, is no attachment: no autoclose, sling force,
// burn or GC that walks attachments sees it; a real attachment is returned.
func TestCollectAttachedBeadsSkipsAControllerOwnedRow(t *testing.T) {
	store := beads.NewMemStore()
	parent, err := store.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	ownedBy := func(typ string, labels []string, meta map[string]string, parentID string) beads.Bead {
		b, err := store.Create(beads.Bead{Title: "owned", Type: "task", Labels: labels, Metadata: meta, ParentID: parentID})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.Update(b.ID, beads.UpdateOpts{Type: &typ}); err != nil {
			t.Fatal(err)
		}
		return b
	}
	byMolecule := ownedBy("session", []string{"gc:session"}, nil, "")
	byWorkflow := ownedBy("startup-health-episode", nil, nil, "")
	ownedChild := ownedBy("convergence", nil, map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}, parent.ID)
	attached, err := store.Create(beads.Bead{Title: "wf", Type: "task", ParentID: parent.ID, Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SetMetadataBatch(parent.ID, map[string]string{beadmeta.MoleculeIDMetadataKey: byMolecule.ID, "workflow_id": byWorkflow.ID}); err != nil {
		t.Fatal(err)
	}
	parent, _ = store.Get(parent.ID)
	got, err := CollectAttachedBeads(parent, store, store)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != attached.ID {
		ids := make([]string, 0, len(got))
		for _, b := range got {
			ids = append(ids, b.ID)
		}
		t.Fatalf("attachments %v, want only %s (owned %s %s %s skipped)", ids, attached.ID, byMolecule.ID, byWorkflow.ID, ownedChild.ID)
	}
}
