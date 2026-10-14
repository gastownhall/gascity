package main

import (
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/session"
)

// TestWispAutocloseNeverClosesAControllerOwnedRow (mc-dkblm, architecture B,
// the #7742 review): a work bead whose molecule_id or workflow_id names a
// controller-owned row (written past the API), or whose child is one, closes
// without taking the row with it: CollectAttachedBeads never returns one and
// molecule.CloseMembersWithMetadataExcept never closes one, the chain sling's
// force, dispatch finalization and wisp GC share.
func TestWispAutocloseNeverClosesAControllerOwnedRow(t *testing.T) {
	beads.SetSessionWriteHook(nil)
	beads.SetSessionKeyHook(nil)
	t.Cleanup(func() {
		session.GuardSessionKeys(func(msg string) { panic(msg) })
		session.GuardDispositionWrites(func(msg string) { panic(msg) })
	})
	for _, kind := range []struct {
		name, typ string
		labels    []string
	}{
		{"session", session.BeadType, []string{session.LabelSession}},
		{"crash-damaged session", "", []string{session.LabelSession}},
		{"task carrying the session label", "task", []string{session.LabelSession}},
		{"startup-health episode", session.StartupHealthEpisodeType, nil},
		{"convergence root", "Convergence", nil},
	} {
		for _, link := range []string{"molecule_id", "workflow_id", "child", "member"} {
			t.Run(kind.name+"/"+link, func(t *testing.T) {
				store := beads.NewMemStore()
				work, err := store.Create(beads.Bead{Title: "w", Type: "task"})
				if err != nil {
					t.Fatal(err)
				}
				row, err := store.Create(beads.Bead{Title: "owned", Type: "task", Labels: kind.labels, Metadata: map[string]string{"state": "active"}})
				if err != nil {
					t.Fatal(err)
				}
				typ := kind.typ
				if err := store.Update(row.ID, beads.UpdateOpts{Type: &typ}); err != nil {
					t.Fatal(err)
				}
				switch link {
				case "child": // an attached root by parentage
					parent := work.ID
					if err := store.Update(row.ID, beads.UpdateOpts{ParentID: &parent, Metadata: map[string]string{"gc.kind": "workflow"}}); err != nil {
						t.Fatal(err)
					}
				case "member": // a leftover member of a terminal attached molecule, which the autoclose reaps
					mol, err := store.Create(beads.Bead{Title: "mol", Type: "molecule"})
					if err != nil {
						t.Fatal(err)
					}
					step, err := store.Create(beads.Bead{Title: "step", Type: "task", ParentID: mol.ID})
					if err != nil {
						t.Fatal(err)
					}
					parent := mol.ID
					if err := store.Update(row.ID, beads.UpdateOpts{ParentID: &parent}); err != nil {
						t.Fatal(err)
					}
					if err := store.SetMetadata(work.ID, "molecule_id", mol.ID); err != nil {
						t.Fatal(err)
					}
					if err := store.Close(mol.ID); err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if got, _ := store.Get(step.ID); got.Status != "closed" {
							t.Errorf("the molecule's leftover step %s, want it reaped", got.Status)
						}
					})
				default:
					if err := store.SetMetadata(work.ID, link, row.ID); err != nil {
						t.Fatal(err)
					}
				}
				before, _ := store.Get(row.ID)
				if err := store.Close(work.ID); err != nil {
					t.Fatal(err)
				}
				doWispAutocloseWith(store, work.ID, io.Discard, beads.GraphStore{Store: store})
				if got, _ := store.Get(row.ID); got.Status != before.Status {
					t.Fatalf("owned row %s -> %s after the work bead's autoclose, want it as it was", before.Status, got.Status)
				}
			})
		}
	}
}
