package main

import (
	"bytes"
	"errors"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// Autoclose of a work-class convoy across stores (S3c, site 1).
//
// A graph root poured under a work bead (molecule.Options.ParentID: the root
// carries the attach target as its legacy parent) lives in the class binding,
// while the parent, a convoy, is work class and stays on the work ledger. Two
// defects followed on a split city:
//
//   - When the graph child closed last, the controller's autoclose read the
//     parent through the binding, missed it, and the convoy never autoclosed.
//   - When a WORK child closed last, the convoy's children were listed on the
//     work ledger only, so it closed while its graph child was still open.
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
	return &controllerState{cfg: e.cfg, cityPath: e.cityPath, cityBeadStore: e.work, beadStores: e.rigStores, storageRoutes: e.routes}
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

// A work sibling that closes last does not close the convoy while its graph
// child is open; once the graph child is closed too, it does.
func TestAutocloseOfAWorkSiblingCountsTheConvoysGraphChildren(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		convoy, root := convoyOverGraphRoot(t, e)
		last, err := e.work.Create(beads.Bead{Title: "last work child", Type: "task", ParentID: convoy.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.work.Close(last.ID); err != nil {
			t.Fatal(err)
		}
		cs := autocloseControllerFor(e)
		if finished := cs.beadCloseAutoclose(last.ID, e.work, "")(); !finished {
			t.Errorf("autoclose run for %s did not finish", last.ID)
		}
		got, err := e.work.Get(convoy.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status == "closed" {
			t.Fatalf("work convoy %s closed when its last work child %s closed, while its graph child %s is still open", convoy.ID, last.ID, root.ID)
		}

		if err := e.graphStore().Close(root.ID); err != nil {
			t.Fatal(err)
		}
		cs.beadCloseAutoclose(last.ID, e.work, "")()
		if got, err = e.work.Get(convoy.ID); err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("work convoy %s status=%q with every child closed in both stores, want closed", convoy.ID, got.Status)
		}
	})
}

// An open graph child on the binding's ephemeral tier keeps a work convoy
// open, and its close closes the convoy. The binding is the raw engine, with
// no policy layer to widen a list to that tier, so its children are listed at
// beads.FederatedReadTier. Split only: on a single-store city the children are
// listed on the work ledger, whose policy layer has always read both tiers.
func TestAutocloseCountsAWorkConvoysEphemeralGraphChild(t *testing.T) {
	e := newSplitEnv(t, true)
	convoy, root := convoyOverGraphRoot(t, e)
	wisp := mintEphemeralGraphChild(t, e, "ephemeral graph child", convoy.ID)
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
	if got.Status == "closed" {
		t.Fatalf("work convoy %s closed while its ephemeral graph child %s is still open", convoy.ID, wisp.ID)
	}

	if err := graph.Close(wisp.ID); err != nil {
		t.Fatal(err)
	}
	cs.beadCloseAutoclose(wisp.ID, graph, "")()
	if got, err = e.work.Get(convoy.ID); err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Fatalf("work convoy %s status=%q after its ephemeral graph child %s closed last, want closed", convoy.ID, got.Status, wisp.ID)
	}
}

// A graph root poured under a rig convoy closes the convoy where it lives:
// the binding row's work home is the rig its parent's id prefix names. On a
// single-store city the root is poured beside the convoy, in the rig's store.
func TestAutocloseClosesARigConvoyWhenItsGraphChildCloses(t *testing.T) {
	forEachTopologyWithRig(t, func(t *testing.T, e splitEnv) {
		convoy, err := e.rig.Create(beads.Bead{Title: "rig convoy", Type: "convoy"})
		if err != nil {
			t.Fatal(err)
		}
		done, err := e.rig.Create(beads.Bead{Title: "finished rig work", Type: "task", ParentID: convoy.ID})
		if err != nil {
			t.Fatal(err)
		}
		if err := e.rig.Close(done.ID); err != nil {
			t.Fatal(err)
		}
		pour := e.rig
		if e.split {
			pour = e.graphStore()
		}
		root, err := pour.Create(beads.Bead{Title: "graph root", Type: "molecule", ParentID: convoy.ID, Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWisp,
		}})
		if err != nil {
			t.Fatal(err)
		}
		if err := pour.Close(root.ID); err != nil {
			t.Fatal(err)
		}
		cs := autocloseControllerFor(e)
		if finished := cs.beadCloseAutoclose(root.ID, pour, "")(); !finished {
			t.Errorf("autoclose run for %s did not finish", root.ID)
		}
		got, err := e.rig.Get(convoy.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != "closed" {
			t.Fatalf("rig convoy %s status=%q after its last child (graph root %s) closed, want closed", convoy.ID, got.Status, root.ID)
		}
	})
}

// The hourly backstop (gc convoy check, which the core pack's
// convoy-autoclose-backstop order runs) counts a work convoy's children in
// the class binding too: it leaves the convoy open while its graph child is
// open, and closes it once that child has closed.
func TestConvoyCheckCountsAWorkConvoysGraphChildren(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		convoy, root := convoyOverGraphRoot(t, e)
		seedCLIStorageRoutes(t, e.cityPath, e.routes)
		views, err := convoyStoreViewsWithBinding(e.cityPath, []convoyStoreView{{path: e.cityPath, store: e.work}})
		if err != nil {
			t.Fatalf("federating the check's views with the class binding: %v", err)
		}
		check := func() string {
			t.Helper()
			var stdout, stderr bytes.Buffer
			if code := doConvoyCheckAcrossStoresJSON(views, 0, events.Discard, false, &stdout, &stderr); code != 0 {
				t.Fatalf("gc convoy check = %d, want 0:\nstdout=%s\nstderr=%s", code, stdout.String(), stderr.String())
			}
			got, err := e.work.Get(convoy.ID)
			if err != nil {
				t.Fatal(err)
			}
			return got.Status
		}
		if got := check(); got == "closed" {
			t.Fatalf("gc convoy check closed work convoy %s while its graph child %s is still open", convoy.ID, root.ID)
		}
		if err := e.graphStore().Close(root.ID); err != nil {
			t.Fatal(err)
		}
		if got := check(); got != "closed" {
			t.Fatalf("work convoy %s status=%q after gc convoy check with every child closed in both stores, want closed", convoy.ID, got)
		}
	})
}

// A store that cannot answer is no proof: the convoy stays open and the run
// is reported unfinished, so the sweep retries it.
func TestConvoyAutocloseAcrossStoresFailsClosedOnAnUnreadableStore(t *testing.T) {
	down := errors.New("store down")
	newFixture := func(t *testing.T) (work, binding beads.Store, convoy, workChild, graphChild beads.Bead) {
		t.Helper()
		work = beads.NewMemStore()
		binding = &beads.MemStore{IDPrefix: "gcg"}
		var err error
		if convoy, err = work.Create(beads.Bead{Title: "convoy", Type: "convoy"}); err != nil {
			t.Fatal(err)
		}
		if workChild, err = work.Create(beads.Bead{Title: "work child", Type: "task", ParentID: convoy.ID}); err != nil {
			t.Fatal(err)
		}
		if err := work.Close(workChild.ID); err != nil {
			t.Fatal(err)
		}
		if graphChild, err = binding.Create(beads.Bead{Title: "graph child", Type: "molecule", ParentID: convoy.ID, Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}}); err != nil {
			t.Fatal(err)
		}
		if err := binding.Close(graphChild.ID); err != nil {
			t.Fatal(err)
		}
		return work, binding, convoy, workChild, graphChild
	}
	t.Run("binding list fails", func(t *testing.T) {
		work, binding, convoy, workChild, _ := newFixture(t)
		run := doConvoyAutocloseWith(work, convoyAutocloseHomes{children: &listErrStore{Store: binding, err: down}}, events.Discard, workChild.ID, io.Discard, io.Discard)
		if run.finished() {
			t.Error("run finished although the binding's children could not be listed")
		}
		if got, _ := work.Get(convoy.ID); got.Status == "closed" {
			t.Fatalf("convoy %s closed without its binding children read", convoy.ID)
		}
	})
	t.Run("work parent read fails", func(t *testing.T) {
		work, binding, convoy, _, graphChild := newFixture(t)
		unreadable := &getErrStore{Store: work, err: down}
		homes := convoyAutocloseHomes{parent: func(string) beads.Store { return unreadable }}
		run := doConvoyAutocloseWith(binding, homes, events.Discard, graphChild.ID, io.Discard, io.Discard)
		if run.finished() {
			t.Error("run finished although the parent could not be read")
		}
		if got, _ := work.Get(convoy.ID); got.Status == "closed" {
			t.Fatalf("convoy %s closed although its read failed", convoy.ID)
		}
	})
}

// A binding row reads its parent from its own store first, whether it is a
// relic (a work-prefixed id migrated into the binding) or was minted there
// since (a retry copies its predecessor's parent): a work bead that reuses the
// parent's id must not answer for the parent the row's own store holds.
func TestConvoyAutocloseReadsABindingRowsParentFromItsOwnStoreFirst(t *testing.T) {
	for _, child := range []beads.Bead{
		{ID: "gc-9", Title: "relic step", Type: "step"},
		{ID: "gcg-3", Title: "retry minted after the split", Type: "task", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}},
	} {
		t.Run(child.ID, func(t *testing.T) {
			work := beads.NewMemStore()
			work.HonorExplicitIDs = true
			binding := beads.NewMemStore()
			binding.HonorExplicitIDs = true
			if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "unrelated convoy reusing the id", Type: "convoy"}); err != nil {
				t.Fatal(err)
			}
			if _, err := work.Create(beads.Bead{ID: "gc-8", Title: "its finished child", Type: "task", ParentID: "gc-7"}); err != nil {
				t.Fatal(err)
			}
			if _, err := binding.Create(beads.Bead{ID: "gc-7", Title: "migrated root", Type: "molecule", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}}); err != nil {
				t.Fatal(err)
			}
			child.ParentID = "gc-7"
			if _, err := binding.Create(child); err != nil {
				t.Fatal(err)
			}
			// Create mints open rows, so both children are closed explicitly:
			// with every child terminal, only the parent read decides whether
			// the convoy closes.
			if err := work.Close("gc-8"); err != nil {
				t.Fatal(err)
			}
			if err := binding.Close(child.ID); err != nil {
				t.Fatal(err)
			}
			homes := convoyAutocloseHomes{parent: func(string) beads.Store { return work }}
			doConvoyAutocloseWith(binding, homes, events.Discard, child.ID, io.Discard, io.Discard)
			if got, _ := work.Get("gc-7"); got.Status == "closed" {
				t.Fatalf("closing %s autoclosed an unrelated work convoy that reuses its parent's id", child.ID)
			}
		})
	}
}
