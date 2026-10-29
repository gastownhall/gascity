package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// Autoclose of a work-class parent across stores (S3c, site 1).
//
// A graph root poured under a work bead (molecule.Options.ParentID: the root
// carries the attach target as its legacy parent) lives in the class binding,
// while the parent, a convoy, is work class and stays on the work ledger. The
// controller's autoclose read the parent through the store of the row that
// closed, the binding, missed it, and the convoy never autoclosed.
//
// Both topologies: the single-store leg is the legacy control (one store, the
// same answer as before); the split leg is the repo's two-store fixture (a
// strict bd-semantics work store and a real SQLite binding).

// convoyOverGraphRoot seeds an open convoy on the work ledger with one closed
// work child, and an open graph root in the graph store whose legacy parent is
// the convoy.
func convoyOverGraphRoot(t *testing.T, e splitEnv) (convoy, root beads.Bead) {
	t.Helper()
	convoy, err := e.work.Create(beads.Bead{Title: "release convoy", Type: "convoy"})
	if err != nil {
		t.Fatal(err)
	}
	done, err := e.work.Create(beads.Bead{Title: "finished work", Type: "task", ParentID: convoy.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := e.work.Close(done.ID); err != nil {
		t.Fatal(err)
	}
	root, err = e.graphStore().Create(beads.Bead{Title: "graph root", Type: "molecule", ParentID: convoy.ID, Metadata: map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWisp,
	}})
	if err != nil {
		t.Fatal(err)
	}
	return convoy, root
}

func autocloseControllerFor(e splitEnv) *controllerState {
	return &controllerState{cfg: e.cfg, cityPath: e.cityPath, cityBeadStore: e.work, storageRoutes: e.routes}
}

func TestAutocloseClosesAWorkConvoyWhenItsGraphChildCloses(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		convoy, root := convoyOverGraphRoot(t, e)
		graph := e.graphStore()
		if err := graph.Close(root.ID); err != nil {
			t.Fatal(err)
		}
		cs := autocloseControllerFor(e)
		if finished := cs.beadCloseAutoclose(root.ID, graph, "")(); !finished {
			t.Errorf("autoclose run for %s did not finish", root.ID)
		}
		got, err := e.work.Get(convoy.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("work convoy %s status=%q after its last child (graph root %s) closed, want closed", convoy.ID, got.Status, root.ID)
		}
	})
}

// The convoy's children are counted in both stores: an open graph child keeps
// the work convoy open when a work sibling closes.
func TestAutocloseKeepsAWorkConvoyOpenWhileItsGraphChildIsOpen(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		convoy, root := convoyOverGraphRoot(t, e)
		graph := e.graphStore()
		other, err := graph.Create(beads.Bead{Title: "second graph root", Type: "molecule", ParentID: convoy.ID, Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWisp,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := graph.Close(root.ID); err != nil {
			t.Fatal(err)
		}
		cs := autocloseControllerFor(e)
		cs.beadCloseAutoclose(root.ID, graph, "")()
		got, err := e.work.Get(convoy.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "closed" {
			t.Fatalf("work convoy %s closed while its graph child %s is still open", convoy.ID, other.ID)
		}
	})
}
