package main

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// TestDanglingBlockingEdgesCheckFindsAndRemovesOnlyDanglingEdges: on a split
// city the check reports the open and in-progress rows whose blocking edge
// names a bead no store holds — the maintainer-city shape is a blocks edge to a
// malformed "discovered-from:<id>" literal — and --fix removes exactly those
// edges. A cross-store edge whose target a store holds, a capability ref and a
// closed row are left alone, and nothing is written but the removals.
func TestDanglingBlockingEdgesCheckFindsAndRemovesOnlyDanglingEdges(t *testing.T) {
	cityPath := t.TempDir()
	binding := sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
	step, err := binding.Create(beads.Bead{Title: "graph step", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	mk := func(title, status string, targets ...string) beads.Bead {
		t.Helper()
		b, err := work.Create(beads.Bead{Title: title, Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		for _, target := range targets {
			if err := work.DepAdd(b.ID, target, "blocks"); err != nil {
				t.Fatal(err)
			}
		}
		if status != "" && status != "open" {
			s := status
			if err := work.Update(b.ID, beads.UpdateOpts{Status: &s}); err != nil {
				t.Fatal(err)
			}
		}
		return b
	}
	malformed := mk("malformed literal", "open", "discovered-from:gc-404")
	gone := mk("external to nothing", "in_progress", "external:gc-405")
	mk("cross-store, held", "open", step.ID)
	mk("capability", "open", "external:rig:some-capability")
	mk("closed row", "closed", "discovered-from:gc-406")

	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	edges, skipped := check.collect()
	if len(skipped) != 0 {
		t.Fatalf("skipped = %v", skipped)
	}
	var got []string
	for _, e := range edges {
		got = append(got, e.rowID+"->"+e.target)
	}
	want := []string{malformed.ID + "->discovered-from:gc-404", gone.ID + "->external:gc-405"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("dangling edges = %v, want %v", got, want)
	}
	if res := check.Run(nil); res.Status != doctor.StatusWarning || !strings.Contains(res.Message, "2 blocking edge") {
		t.Fatalf("Run = %+v, want a warning naming 2 edges", res)
	}
	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	if edges, _ := check.collect(); len(edges) != 0 {
		t.Fatalf("after --fix dangling edges = %+v, want none", edges)
	}
	if res := check.Run(nil); res.Status != doctor.StatusOK {
		t.Fatalf("Run after --fix = %+v, want OK", res)
	}
	deps, err := work.DepList(malformed.ID, "down")
	if err != nil || len(deps) != 0 {
		t.Fatalf("deps of %s after --fix = %+v, %v; want the dangling edge removed and nothing written", malformed.ID, deps, err)
	}
}

// TestDanglingBlockingEdgesCheckIsQuietOnASingleStoreCity: a city with no
// class binding keeps bd's answer, so the check reads nothing.
func TestDanglingBlockingEdgesCheckIsQuietOnASingleStoreCity(t *testing.T) {
	cityPath := t.TempDir()
	seedCLIStorageRoutes(t, cityPath, nil)
	opened := 0
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) {
		opened++
		return beads.NewMemStore(), nil
	})
	if res := check.Run(nil); res.Status != doctor.StatusOK || opened != 0 {
		t.Fatalf("Run = %+v with %d store open(s), want OK and none", res, opened)
	}
}

// TestCrossStoreGatesCheckReportsUnresolvedGates: an open gate whose blocker no
// store holds is a doctor finding for as long as it stays open.
func TestCrossStoreGatesCheckReportsUnresolvedGates(t *testing.T) {
	cityPath := t.TempDir()
	binding := sessionSplitRoutes(t, cityPath)
	check := newCrossStoreGatesCheck(nil, cityPath, nil)
	if res := check.Run(nil); res.Status != doctor.StatusOK {
		t.Fatalf("Run with no gates = %+v, want OK", res)
	}
	gate, err := binding.Create(beads.Bead{
		Title:  "waits for gc-404",
		Type:   "gate",
		Labels: []string{dispatch.CrossStoreGateLabel},
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:          "gcg-root",
			beadmeta.CrossStoreGateOwnerMetadataKey: "gcg-drain",
			beadmeta.CrossStoreBlockerMetadataKey:   "gc-404",
			beadmeta.CrossStoreGateStateMetadataKey: dispatch.CrossStoreGateStateUnresolved,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	res := check.Run(nil)
	if res.Status != doctor.StatusWarning || len(res.Details) != 1 || !strings.Contains(res.Details[0], gate.ID) {
		t.Fatalf("Run = %+v, want a warning naming gate %s", res, gate.ID)
	}
}

// TestDanglingBlockingEdgesFixSparesBlockersInUnreadRigs: --fix acts only on
// targets proven absent from EVERY store. A blocker in the namespace of a
// suspended rig, or of a rig whose store will not open, is undecided — even a
// hyphenated rig prefix ("gc-ops") is matched whole — and while any store was
// skipped --fix touches nothing at all.
func TestDanglingBlockingEdgesFixSparesBlockersInUnreadRigs(t *testing.T) {
	for _, tc := range []struct {
		name      string
		prefix    string
		suspended bool
		openFails bool
	}{
		{name: "suspended rig", prefix: "rg", suspended: true},
		{name: "rig store fails to open", prefix: "rg", openFails: true},
		{name: "suspended rig with a hyphenated prefix", prefix: "gc-ops", suspended: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			sessionSplitRoutes(t, cityPath)
			work := beads.NewMemStore()
			rig := beads.NewMemStore()
			rig.HonorExplicitIDs = true
			rb, err := rig.Create(beads.Bead{ID: tc.prefix + "-77", Title: "open rig blocker", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			row, err := work.Create(beads.Bead{Title: "row", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			if err := work.DepAdd(row.ID, rb.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			cfg := &config.City{Rigs: []config.Rig{{Name: "rg", Path: "rigs/rg", Prefix: tc.prefix, Suspended: tc.suspended}}}
			check := newDanglingBlockingEdgesCheck(cfg, cityPath, func(p string) (beads.Store, error) {
				if p == "rigs/rg" {
					if tc.openFails {
						return nil, errors.New("dolt server restarting")
					}
					return rig, nil
				}
				return work, nil
			})
			if edges, _ := check.collect(); len(edges) != 0 {
				t.Fatalf("dangling edges = %+v; %s is in an unread rig's namespace and is undecided", edges, rb.ID)
			}
			ferr := check.Fix(nil)
			if tc.openFails && ferr == nil {
				t.Fatal("Fix succeeded although a rig store could not be read")
			}
			if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 {
				t.Fatalf("deps of %s after --fix = %+v; the edge to %s, held by an unread rig, was touched", row.ID, deps, rb.ID)
			}
		})
	}
}

// TestDanglingBlockingEdgesFixRewritesAMalformedDiscoveredFromLiteral is the
// maintainer-city shape (ga-26jnu, ga-0wmnm, ga-dsazz): a blocks edge to the
// literal "discovered-from:<id>" whose <id> the row's own store holds. --fix
// rewrites it as the real discovered-from edge in that store; nothing blocks
// afterwards. A literal whose <id> only another store holds is left for the
// operator, because rewriting it would write a cross-store edge.
func TestDanglingBlockingEdgesFixRewritesAMalformedDiscoveredFromLiteral(t *testing.T) {
	cityPath := t.TempDir()
	binding := sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
	origin, err := work.Create(beads.Bead{Title: "where it was discovered", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := binding.Create(beads.Bead{Title: "graph-side origin", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	row, err := work.Create(beads.Bead{Title: "discovered work", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	cross, err := work.Create(beads.Bead{Title: "discovered from the graph", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(row.ID, "discovered-from:"+origin.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(cross.ID, "discovered-from:"+elsewhere.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	edges, skipped := check.collect()
	if len(skipped) != 0 || len(edges) != 2 {
		t.Fatalf("edges = %+v, skipped = %v; want the two malformed literals", edges, skipped)
	}
	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	deps, err := work.DepList(row.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != origin.ID || deps[0].Type != "discovered-from" {
		t.Fatalf("deps of %s after --fix = %+v; want one discovered-from edge to %s and no blocks edge", row.ID, deps, origin.ID)
	}
	crossDeps, err := work.DepList(cross.ID, "down")
	if err != nil {
		t.Fatal(err)
	}
	if len(crossDeps) != 1 || crossDeps[0].DependsOnID != "discovered-from:"+elsewhere.ID {
		t.Fatalf("deps of %s after --fix = %+v; want the literal left for the operator (its id lives in another store)", cross.ID, crossDeps)
	}
}

// rewriteStores are the two engines whose DepAdd RETYPES an existing edge in
// place, which is what makes a careless rewrite downgrade a blocks edge.
func rewriteStores(t *testing.T) map[string]func() beads.Store {
	return map[string]func() beads.Store{
		"memstore": func() beads.Store { return beads.NewMemStore() },
		"sqlite": func() beads.Store {
			s, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gc"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = s.(interface{ CloseStore() error }).CloseStore() })
			return s
		},
	}
}

// TestDanglingBlockingEdgesFixKeepsAnExistingRealBlocksEdge: a row that has a
// real blocks edge to X AND a malformed "discovered-from:X" literal keeps
// waiting on X after --fix: the literal is removed, the blocks edge is never
// retyped.
func TestDanglingBlockingEdgesFixKeepsAnExistingRealBlocksEdge(t *testing.T) {
	for name, mk := range rewriteStores(t) {
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			sessionSplitRoutes(t, cityPath)
			work := mk()
			origin, _ := work.Create(beads.Bead{Title: "open blocker and origin", Type: "task"})
			row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
			if err := work.DepAdd(row.ID, origin.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			if err := work.DepAdd(row.ID, "discovered-from:"+origin.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
			if err := check.Fix(nil); err != nil {
				t.Fatalf("Fix: %v", err)
			}
			deps, _ := work.DepList(row.ID, "down")
			if len(deps) != 1 || deps[0].DependsOnID != origin.ID || deps[0].Type != "blocks" {
				t.Fatalf("deps of %s after --fix = %+v; want only the real blocks edge to %s", row.ID, deps, origin.ID)
			}
		})
	}
}

type failDepAdd struct{ beads.Store }

func (failDepAdd) DepAdd(string, string, string) error { return errors.New("injected: write failed") }

// TestDanglingBlockingEdgesFixKeepsTheLiteralWhenTheAddFails: the typed edge
// is added before the literal is removed, so a failed add loses nothing; the
// fix is idempotent once it succeeds.
func TestDanglingBlockingEdgesFixKeepsTheLiteralWhenTheAddFails(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	inner := beads.NewMemStore()
	origin, _ := inner.Create(beads.Bead{Title: "origin", Type: "task"})
	row, _ := inner.Create(beads.Bead{Title: "row", Type: "task"})
	if err := inner.DepAdd(row.ID, "discovered-from:"+origin.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	failing := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return failDepAdd{inner}, nil })
	if err := failing.Fix(nil); err == nil {
		t.Fatal("Fix reported no error although the add failed")
	}
	if deps, _ := inner.DepList(row.ID, "down"); len(deps) != 1 || deps[0].DependsOnID != "discovered-from:"+origin.ID {
		t.Fatalf("deps after a failed add = %+v; want the literal kept", deps)
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return inner, nil })
	for i := 0; i < 3; i++ {
		if err := check.Fix(nil); err != nil {
			t.Fatalf("Fix %d: %v", i, err)
		}
	}
	if deps, _ := inner.DepList(row.ID, "down"); len(deps) != 1 || deps[0].DependsOnID != origin.ID || deps[0].Type != "discovered-from" {
		t.Fatalf("deps after --fix = %+v; want one discovered-from edge", deps)
	}
}

// TestDanglingBlockingEdgesFixOnRealBd runs both rewrite cases on a real
// bd/Dolt work store: a plain literal becomes a discovered-from edge, and a
// literal beside a real blocks edge is removed with the blocks edge kept.
func TestDanglingBlockingEdgesFixOnRealBd(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	work := splittest.NewNativeWorkStore(t, "ga")
	origin, _ := work.Create(beads.Bead{Title: "origin", Type: "task"})
	plain, _ := work.Create(beads.Bead{Title: "plain literal row", Type: "task"})
	both, _ := work.Create(beads.Bead{Title: "literal and real blocks", Type: "task"})
	for _, e := range [][3]string{
		{plain.ID, "discovered-from:" + origin.ID, "blocks"},
		{both.ID, origin.ID, "blocks"},
		{both.ID, "discovered-from:" + origin.ID, "blocks"},
	} {
		if err := work.DepAdd(e[0], e[1], e[2]); err != nil {
			t.Fatalf("seed %v: %v", e, err)
		}
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	for i := 0; i < 2; i++ {
		if err := check.Fix(nil); err != nil {
			t.Fatalf("Fix %d: %v", i, err)
		}
	}
	if deps, _ := work.DepList(plain.ID, "down"); len(deps) != 1 || deps[0].DependsOnID != origin.ID || deps[0].Type != "discovered-from" {
		t.Fatalf("plain row deps = %+v; want one discovered-from edge to %s", deps, origin.ID)
	}
	if deps, _ := work.DepList(both.ID, "down"); len(deps) != 1 || deps[0].DependsOnID != origin.ID || deps[0].Type != "blocks" {
		t.Fatalf("both row deps = %+v; want only the real blocks edge to %s", deps, origin.ID)
	}
}

// TestDanglingBlockingEdgesHonorsRuntimeSuspension is the maintainer-city
// shape: a rig suspended at RUNTIME (the suspension state) whose city.toml
// says it is not suspended. Its store is not read and its ids are undecided.
func TestDanglingBlockingEdgesHonorsRuntimeSuspension(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	yes := true
	if err := suspensionstate.SetRigSuspended(fsys.OSFS{}, cityPath, "rg", &yes); err != nil {
		t.Fatal(err)
	}
	work := beads.NewMemStore()
	row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
	if err := work.DepAdd(row.ID, "rg-77", "blocks"); err != nil {
		t.Fatal(err)
	}
	opened := false
	cfg := &config.City{Rigs: []config.Rig{{Name: "rg", Path: "rigs/rg", Prefix: "rg"}}}
	check := newDanglingBlockingEdgesCheck(cfg, cityPath, func(p string) (beads.Store, error) {
		if p == "rigs/rg" {
			opened = true
		}
		return work, nil
	})
	if edges, _ := check.collect(); len(edges) != 0 || opened {
		t.Fatalf("edges = %+v, rig store opened = %v; want the runtime-suspended rig unread and rg-77 undecided", edges, opened)
	}
	if err := check.Fix(nil); err != nil {
		t.Fatal(err)
	}
	if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 {
		t.Fatalf("deps after --fix = %+v; the edge into the suspended rig was touched", deps)
	}
}

// batchReadFailing fails the batched id read the scan resolves targets with.
type batchReadFailing struct{ beads.Store }

func (s batchReadFailing) List(q beads.ListQuery) ([]beads.Bead, error) {
	if len(q.IDs) > 0 {
		return nil, errors.New("injected: batch read failed")
	}
	return s.Store.List(q)
}

// TestDanglingBlockingEdgesReportsNothingWhenABatchReadFails: a store whose
// blocker read fails may hold any target, so the scan reports no dangling
// edge, says what it skipped, and --fix touches nothing.
func TestDanglingBlockingEdgesReportsNothingWhenABatchReadFails(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
	row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
	if err := work.DepAdd(row.ID, "discovered-from:gc-404", "blocks"); err != nil {
		t.Fatal(err)
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return batchReadFailing{work}, nil })
	edges, skipped := check.collect()
	if len(edges) != 0 || len(skipped) == 0 {
		t.Fatalf("edges = %+v, skipped = %v; want nothing reported and the failure named", edges, skipped)
	}
	if err := check.Fix(nil); err == nil {
		t.Fatal("Fix succeeded although a store's blocker read failed")
	}
	if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 {
		t.Fatalf("deps after --fix = %+v; want nothing touched", deps)
	}
}

// TestDanglingBlockingEdgesLeavesALiteralWhoseIDAnUnreadRigMayHold: a
// "discovered-from:<id>" literal whose <id> is in a suspended rig's namespace
// is the operator's, never removed or rewritten.
func TestDanglingBlockingEdgesLeavesALiteralWhoseIDAnUnreadRigMayHold(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
	row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
	if err := work.DepAdd(row.ID, "discovered-from:rg-77", "blocks"); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "rg", Path: "rigs/rg", Prefix: "rg", Suspended: true}}}
	check := newDanglingBlockingEdgesCheck(cfg, cityPath, func(string) (beads.Store, error) { return work, nil })
	if err := check.Fix(nil); err != nil {
		t.Fatal(err)
	}
	if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 || deps[0].DependsOnID != "discovered-from:rg-77" {
		t.Fatalf("deps after --fix = %+v; want the literal left for the operator", deps)
	}
}

// TestDanglingBlockingEdgesNeverRewritesABlockingLiteral: a "blocks:<id>"
// literal is not a typed edge to rewrite — rewriting it would make the row
// wait on something it did not wait on. It is removed as any dangling edge.
func TestDanglingBlockingEdgesNeverRewritesABlockingLiteral(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
	other, _ := work.Create(beads.Bead{Title: "other", Type: "task"})
	row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
	if err := work.DepAdd(row.ID, "blocks:"+other.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	if err := check.Fix(nil); err != nil {
		t.Fatal(err)
	}
	if deps, _ := work.DepList(row.ID, "down"); len(deps) != 0 {
		t.Fatalf("deps after --fix = %+v; want the blocking literal removed, not rewritten into a blocks edge", deps)
	}
}

// TestInUnreadNamespaceMatchesWholeNamespaces pins both directions: a
// hyphenated rig prefix owns its own ids and not its parent prefix's.
func TestInUnreadNamespaceMatchesWholeNamespaces(t *testing.T) {
	unread := []string{"gc-ops"}
	if !inUnreadNamespace("gc-ops-12", unread) || !inUnreadNamespace("GC-OPS-12", unread) {
		t.Error("gc-ops-12 is in the gc-ops namespace")
	}
	if inUnreadNamespace("gc-12", unread) || inUnreadNamespace("gc-opsx-1", unread) {
		t.Error("gc-12 and gc-opsx-1 are not in the gc-ops namespace")
	}
}

// TestDanglingBlockingEdgesFixReChecksTheRowBeforeWriting: the fix re-reads
// the row right before it writes. If an agent added a real blocks edge to the
// literal's id after the scan, the fix only removes the literal (it never
// retypes that edge); if the literal is already gone, it writes nothing.
func TestDanglingBlockingEdgesFixReChecksTheRowBeforeWriting(t *testing.T) {
	for name, mk := range rewriteStores(t) {
		t.Run(name, func(t *testing.T) {
			cityPath := t.TempDir()
			sessionSplitRoutes(t, cityPath)
			work := mk()
			origin, _ := work.Create(beads.Bead{Title: "origin", Type: "task"})
			raced, _ := work.Create(beads.Bead{Title: "an agent adds a real blocks edge", Type: "task"})
			gone, _ := work.Create(beads.Bead{Title: "an agent removes the literal", Type: "task"})
			for _, id := range []string{raced.ID, gone.ID} {
				if err := work.DepAdd(id, "discovered-from:"+origin.ID, "blocks"); err != nil {
					t.Fatal(err)
				}
			}
			check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
			edges, skipped := check.collect()
			if len(skipped) != 0 || len(edges) != 2 {
				t.Fatalf("edges = %+v, skipped = %v", edges, skipped)
			}
			// The rows change between the scan and the fix.
			if err := work.DepAdd(raced.ID, origin.ID, "blocks"); err != nil {
				t.Fatal(err)
			}
			if err := work.DepRemove(gone.ID, "discovered-from:"+origin.ID); err != nil {
				t.Fatal(err)
			}
			for _, e := range edges {
				if err := fixDanglingEdge(e, check.stillAbsent); err != nil {
					t.Fatalf("fix %s: %v", e.rowID, err)
				}
			}
			if deps, _ := work.DepList(raced.ID, "down"); len(deps) != 1 || deps[0].DependsOnID != origin.ID || deps[0].Type != "blocks" {
				t.Fatalf("%s deps = %+v; want only the real blocks edge the agent added", raced.ID, deps)
			}
			if deps, _ := work.DepList(gone.ID, "down"); len(deps) != 0 {
				t.Fatalf("%s deps = %+v; want nothing written for a literal already gone", gone.ID, deps)
			}
		})
	}
}

// failDepAddFor fails DepAdd for one row only.
type failDepAddFor struct {
	beads.Store
	row string
}

func (s failDepAddFor) DepAdd(issue, target, typ string) error {
	if issue == s.row {
		return errors.New("injected: write failed")
	}
	return s.Store.DepAdd(issue, target, typ)
}

// TestDanglingBlockingEdgesFixContinuesPastAFailingRow: one row whose rewrite
// fails is reported, and the next row is still fixed.
func TestDanglingBlockingEdgesFixContinuesPastAFailingRow(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	inner := beads.NewMemStore()
	origin, _ := inner.Create(beads.Bead{Title: "origin", Type: "task"})
	first, _ := inner.Create(beads.Bead{Title: "first row (fails)", Type: "task"})
	second, _ := inner.Create(beads.Bead{Title: "second row", Type: "task"})
	for _, id := range []string{first.ID, second.ID} {
		if err := inner.DepAdd(id, "discovered-from:"+origin.ID, "blocks"); err != nil {
			t.Fatal(err)
		}
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return failDepAddFor{Store: inner, row: first.ID}, nil })
	if err := check.Fix(nil); err == nil {
		t.Fatal("Fix reported no error although the first row's add failed")
	}
	if deps, _ := inner.DepList(second.ID, "down"); len(deps) != 1 || deps[0].DependsOnID != origin.ID || deps[0].Type != "discovered-from" {
		t.Fatalf("second row deps = %+v; want it fixed despite the first row's failure", deps)
	}
}

// TestDanglingBlockingEdgesRewriteKeepsTheEdgePayload: on a store whose
// edges carry a payload (SQLite), the rewritten edge carries the literal
// edge's payload.
func TestDanglingBlockingEdgesRewriteKeepsTheEdgePayload(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	work, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gc"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = work.(interface{ CloseStore() error }).CloseStore() })
	origin, _ := work.Create(beads.Bead{Title: "origin", Type: "task"})
	row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
	const payload = `{"note":"found while reviewing"}`
	if err := work.(beads.DepMetadataWriter).DepAddWithMetadata(row.ID, "discovered-from:"+origin.ID, "blocks", payload); err != nil {
		t.Fatal(err)
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	got, carries, err := work.(beads.DepMetadataReader).DepMetadata(row.ID, origin.ID)
	if err != nil || !carries || got != payload {
		t.Fatalf("rewritten edge payload = %q (carries %v, %v); want %q", got, carries, err, payload)
	}
}

// TestDanglingBlockingEdgesFixLeavesALiteralRetypedSinceTheScan: an agent
// retypes the literal edge to a non-blocking type after the scan; it is no
// longer a dangling BLOCKING edge, and the fix leaves it.
func TestDanglingBlockingEdgesFixLeavesALiteralRetypedSinceTheScan(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
	row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
	if err := work.DepAdd(row.ID, "gc-404", "blocks"); err != nil {
		t.Fatal(err)
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	edges, _ := check.collect()
	if len(edges) != 1 {
		t.Fatalf("edges = %+v", edges)
	}
	if err := work.DepRemove(row.ID, "gc-404"); err != nil {
		t.Fatal(err)
	}
	if err := work.DepAdd(row.ID, "gc-404", "related"); err != nil {
		t.Fatal(err)
	}
	if err := fixDanglingEdge(edges[0], check.stillAbsent); err != nil {
		t.Fatal(err)
	}
	if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 || deps[0].Type != "related" {
		t.Fatalf("deps = %+v; want the retyped related edge untouched", deps)
	}
}

// TestDanglingBlockingEdgesFixKeepsAnEdgeWhoseTargetAppeared: the target of a
// dangling edge is created (or restored) after the scan; the fix re-checks
// that it is still absent and keeps the edge.
func TestDanglingBlockingEdgesFixKeepsAnEdgeWhoseTargetAppeared(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
	work.HonorExplicitIDs = true
	row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
	if err := work.DepAdd(row.ID, "gc-404", "blocks"); err != nil {
		t.Fatal(err)
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	edges, _ := check.collect()
	if len(edges) != 1 {
		t.Fatalf("edges = %+v", edges)
	}
	if _, err := work.Create(beads.Bead{ID: "gc-404", Title: "restored", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	if err := fixDanglingEdge(edges[0], check.stillAbsent); err != nil {
		t.Fatal(err)
	}
	if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 {
		t.Fatalf("deps = %+v; want the edge to the restored gc-404 kept", deps)
	}
}

// TestDanglingBlockingEdgesFixKeepsTheLiteralWhenTheRewriteTargetWentAway:
// the bead a discovered-from literal names is deleted after the scan; the
// rewrite would add an edge to nothing, so the fix keeps the literal.
func TestDanglingBlockingEdgesFixKeepsTheLiteralWhenTheRewriteTargetWentAway(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
	origin, _ := work.Create(beads.Bead{Title: "origin", Type: "task"})
	row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
	literal := "discovered-from:" + origin.ID
	if err := work.DepAdd(row.ID, literal, "blocks"); err != nil {
		t.Fatal(err)
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	edges, _ := check.collect()
	if len(edges) != 1 {
		t.Fatalf("edges = %+v", edges)
	}
	if err := work.Delete(origin.ID); err != nil {
		t.Fatal(err)
	}
	if err := fixDanglingEdge(edges[0], check.stillAbsent); err != nil {
		t.Fatal(err)
	}
	if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 || deps[0].DependsOnID != literal {
		t.Fatalf("deps = %+v; want the literal kept since %s is gone", deps, origin.ID)
	}
}

// failingIDReads fails every by-id read once armed (the read stillAbsent
// makes), leaving edge reads and writes alone.
type failingIDReads struct {
	beads.Store
	armed bool
}

func (s *failingIDReads) List(q beads.ListQuery) ([]beads.Bead, error) {
	if s.armed && len(q.IDs) > 0 {
		return nil, errors.New("injected: store unreachable")
	}
	return s.Store.List(q)
}

func (s *failingIDReads) Get(id string) (beads.Bead, error) {
	if s.armed {
		return beads.Bead{}, errors.New("injected: store unreachable")
	}
	return s.Store.Get(id)
}

// TestDanglingBlockingEdgesFixKeepsTheEdgeWhileAStoreCannotBeReRead: the
// re-check before a removal cannot read a store; that store may hold the
// target now, so the edge is kept and the failure reported.
func TestDanglingBlockingEdgesFixKeepsTheEdgeWhileAStoreCannotBeReRead(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
	row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
	if err := work.DepAdd(row.ID, "gc-404", "blocks"); err != nil {
		t.Fatal(err)
	}
	rig := &failingIDReads{Store: beads.NewMemStore()}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	edges, _ := check.collect()
	if len(edges) != 1 {
		t.Fatalf("edges = %+v", edges)
	}
	check.scopes = append(check.scopes, doctorScope{label: "rig", store: rig})
	rig.armed = true
	if err := fixDanglingEdge(edges[0], check.stillAbsent); err == nil {
		t.Fatal("fix reported no error although a store could not be re-read")
	}
	if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 {
		t.Fatalf("deps = %+v; want the edge kept while a store is unreadable", deps)
	}
}

// TestDanglingBlockingEdgesFixRewritesALiteralWhoseBeadAppeared: a typed
// literal whose bead no store held at the scan would be removed; if the row's
// own store holds the bead by the time of the fix, it is rewritten into the
// typed edge instead, and if only another store holds it, it is left for the
// operator.
func TestDanglingBlockingEdgesFixRewritesALiteralWhoseBeadAppeared(t *testing.T) {
	for _, inRowStore := range []bool{true, false} {
		t.Run(map[bool]string{true: "appeared in the row's store", false: "appeared in another store"}[inRowStore], func(t *testing.T) {
			cityPath := t.TempDir()
			sessionSplitRoutes(t, cityPath)
			work := beads.NewMemStore()
			work.HonorExplicitIDs = true
			other := beads.NewMemStore()
			other.HonorExplicitIDs = true
			row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
			const literal = "discovered-from:gc-77"
			if err := work.DepAdd(row.ID, literal, "blocks"); err != nil {
				t.Fatal(err)
			}
			check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
			edges, _ := check.collect()
			if len(edges) != 1 || edges[0].action != danglingRemove {
				t.Fatalf("edges = %+v; want one removal", edges)
			}
			check.scopes = append(check.scopes, doctorScope{label: "rig", store: other})
			holder := other
			if inRowStore {
				holder = work
			}
			if _, err := holder.Create(beads.Bead{ID: "gc-77", Title: "origin", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			if err := fixDanglingEdge(edges[0], check.stillAbsent); err != nil {
				t.Fatal(err)
			}
			deps, _ := work.DepList(row.ID, "down")
			if inRowStore {
				if len(deps) != 1 || deps[0].DependsOnID != "gc-77" || deps[0].Type != "discovered-from" {
					t.Fatalf("deps = %+v; want the literal rewritten as a discovered-from edge to gc-77", deps)
				}
				return
			}
			if len(deps) != 1 || deps[0].DependsOnID != literal {
				t.Fatalf("deps = %+v; want the literal left for the operator", deps)
			}
		})
	}
}

// failingGets fails single-bead reads once armed.
type failingGets struct {
	beads.Store
	armed bool
}

func (s *failingGets) Get(id string) (beads.Bead, error) {
	if s.armed {
		return beads.Bead{}, errors.New("injected: read failed")
	}
	return s.Store.Get(id)
}

// TestDanglingBlockingEdgesFixKeepsALiteralWhenItsBeadCannotBeReRead: the
// re-read of a typed literal's bead in the row's own store fails; the bead may
// be there now, so the literal is kept and the failure reported.
func TestDanglingBlockingEdgesFixKeepsALiteralWhenItsBeadCannotBeReRead(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	work := &failingGets{Store: beads.NewMemStore()}
	row, _ := work.Create(beads.Bead{Title: "row", Type: "task"})
	const literal = "discovered-from:gc-77"
	if err := work.DepAdd(row.ID, literal, "blocks"); err != nil {
		t.Fatal(err)
	}
	check := newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return work, nil })
	edges, _ := check.collect()
	if len(edges) != 1 {
		t.Fatalf("edges = %+v", edges)
	}
	work.armed = true
	if err := fixDanglingEdge(edges[0], check.stillAbsent); err == nil {
		t.Fatal("fix reported no error although the literal's bead could not be re-read")
	}
	if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 || deps[0].DependsOnID != literal {
		t.Fatalf("deps = %+v; want the literal kept", deps)
	}
}
