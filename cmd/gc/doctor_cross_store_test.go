package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
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

// TestCrossStoreGatesCheckReportsGatesHeldOnlyByAnUnreadableStore: a store the
// release pass cannot read may hold an open copy of any blocker, so the pass
// leaves every gate it cannot judge exactly as it is — and gc doctor says so.
// It names each open gate held only because a store could not be read, with
// the store and the error, whether the store would not open or its read
// failed. A gate an open copy in a readable store holds is held by that copy
// and is not reported, and once the store reads again neither is.
func TestCrossStoreGatesCheckReportsGatesHeldOnlyByAnUnreadableStore(t *testing.T) {
	for _, tc := range []struct {
		name    string
		openRig func(rig beads.Store) (beads.Store, error)
		wantErr string
	}{
		{
			name:    "rig store would not open",
			openRig: func(beads.Store) (beads.Store, error) { return nil, errors.New("dolt server restarting") },
			wantErr: "dolt server restarting",
		},
		{
			name:    "rig store read fails",
			openRig: func(rig beads.Store) (beads.Store, error) { return unreadableGateLeg{Store: rig}, nil },
			wantErr: "store unavailable",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityPath := t.TempDir()
			binding := sessionSplitRoutes(t, cityPath)
			work := beads.NewMemStore()
			rig := beads.NewMemStore()
			closedBlocker, err := work.Create(beads.Bead{Title: "closed blocker", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			if err := work.Close(closedBlocker.ID); err != nil {
				t.Fatal(err)
			}
			openBlocker, err := work.Create(beads.Bead{Title: "open blocker", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			control, err := binding.Create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gcg-root"}})
			if err != nil {
				t.Fatal(err)
			}
			gateOn := func(blocker string) beads.Bead {
				t.Helper()
				gate, err := binding.Create(beads.Bead{
					Title:  "waits for " + blocker,
					Type:   "gate",
					Labels: []string{dispatch.CrossStoreGateLabel},
					Metadata: map[string]string{
						beadmeta.RootBeadIDMetadataKey:          "gcg-root",
						beadmeta.CrossStoreGateOwnerMetadataKey: control.ID,
						beadmeta.CrossStoreBlockerMetadataKey:   blocker,
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				return gate
			}
			heldByStore := gateOn(closedBlocker.ID)
			heldByCopy := gateOn(openBlocker.ID)
			cfg := &config.City{Rigs: []config.Rig{{Name: "rg", Path: "rigs/rg", Prefix: "rg"}}}
			readable := false
			check := newCrossStoreGatesCheck(cfg, cityPath, func(path string) (beads.Store, error) {
				if path == "rigs/rg" {
					if readable {
						return rig, nil
					}
					return tc.openRig(rig)
				}
				return work, nil
			})

			res := check.Run(nil)
			if res.Status != doctor.StatusWarning {
				t.Fatalf("Run = %+v, want a warning: gate %s is held only because rig rg could not be read", res, heldByStore.ID)
			}
			var named []string
			for _, detail := range res.Details {
				if strings.Contains(detail, "gate "+heldByCopy.ID+" ") {
					t.Fatalf("detail %q reports gate %s, which the open copy of %s holds", detail, heldByCopy.ID, openBlocker.ID)
				}
				if strings.Contains(detail, "gate "+heldByStore.ID+" ") {
					named = append(named, detail)
				}
			}
			if len(named) != 1 || !strings.Contains(named[0], "rig rg") || !strings.Contains(named[0], tc.wantErr) {
				t.Fatalf("details = %q; want gate %s named once, with rig rg and %q", res.Details, heldByStore.ID, tc.wantErr)
			}

			readable = true
			if res := check.Run(nil); res.Status != doctor.StatusOK {
				t.Fatalf("Run with every store readable = %+v, want OK: no gate is held only by an unreadable store", res)
			}
		})
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

// batchReadFailing fails the batched id read the scan resolves targets with:
// beads.GetMany reads a store by id through its exact batch read, which every
// production work store has (bd, native Dolt).
type batchReadFailing struct{ beads.Store }

func (s batchReadFailing) GetExactBatch([]string) (map[string]beads.Bead, []string, error) {
	return nil, nil, errors.New("injected: batch read failed")
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

// failingIDReadOf fails a by-id read of id, once armed. It has no exact batch
// read, so beads.GetMany reads it with one Get per id.
type failingIDReadOf struct {
	beads.Store
	id    string
	armed bool
}

func (s *failingIDReadOf) Get(id string) (beads.Bead, error) {
	if s.armed && id == s.id {
		return beads.Bead{}, errors.New("injected: store unreachable")
	}
	return s.Store.Get(id)
}

// TestDanglingBlockingEdgesFixReportsAFailedReCheckOfALiteralsBead: before a
// typed literal is removed, the stores are asked whether its bead appeared;
// a store that cannot answer may hold it, so the literal is kept and the
// failure reported.
func TestDanglingBlockingEdgesFixReportsAFailedReCheckOfALiteralsBead(t *testing.T) {
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	work := beads.NewMemStore()
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
	rig := &failingIDReadOf{Store: beads.NewMemStore(), id: "gc-77"}
	check.scopes = append(check.scopes, doctorScope{label: "rig", store: rig})
	rig.armed = true
	if err := fixDanglingEdge(edges[0], check.stillAbsent); err == nil {
		t.Fatal("fix reported no error although a store could not be asked for gc-77")
	}
	if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 || deps[0].DependsOnID != literal {
		t.Fatalf("deps = %+v; want the literal kept", deps)
	}
}

// exactBatchMisses is a store whose exact batch read leaves every id
// unresolved, the answer beads.ExactBatchGetter permits for an id it does not
// answer exactly, while its Get finds the bead.
type exactBatchMisses struct{ beads.Store }

var _ beads.ExactBatchGetter = exactBatchMisses{}

func (exactBatchMisses) GetExactBatch(ids []string) (map[string]beads.Bead, []string, error) {
	return map[string]beads.Bead{}, slices.Clone(ids), nil
}

// TestDanglingBlockingEdgesFixConfirmsAnUnresolvedTargetWithGet: the re-check
// before a removal reads the target with beads.GetMany, which takes an id an
// exact batch read left unresolved as absent, where the batch contract leaves
// that id for Get to settle. A store whose batch leaves the target unresolved
// while its Get finds it holds the target, and the edge is kept.
func TestDanglingBlockingEdgesFixConfirmsAnUnresolvedTargetWithGet(t *testing.T) {
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
	rig := beads.NewMemStore()
	rig.HonorExplicitIDs = true
	if _, err := rig.Create(beads.Bead{ID: "gc-404", Title: "held", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	check.scopes = append(check.scopes, doctorScope{label: "rig", store: exactBatchMisses{rig}})
	if err := fixDanglingEdge(edges[0], check.stillAbsent); err != nil {
		t.Fatal(err)
	}
	if deps, _ := work.DepList(row.ID, "down"); len(deps) != 1 {
		t.Fatalf("deps = %+v; want the edge to gc-404 kept: the rig's Get holds it", deps)
	}
}

// danglingEdgeBd is a bd CLI double that drives the dangling-edge check
// through a real *beads.BdStore. A beadstest.FakeBd answers the bead rows
// (list, query and a one-id show); the double owns the dependency table, so it
// can hold an edge whose target has no row, and answers the edge commands as
// bd v1.3 does:
//
//   - `bd dep list <id> --json` prints the depended-on beads' rows, a join
//     that leaves out an edge whose target has no row;
//   - `bd dep list <id> <id>... --json` prints the raw dependency records, an
//     anchor's records once per time it is named;
//   - `bd dep add` and `bd dep remove` take an "external:" target only as
//     external:<project>:<capability>; remove takes an edge by its exact
//     target, and add a target bd holds;
//   - `bd show --json <ids...>` prints the rows it holds and skips the rest,
//     failing only when it holds none.
type danglingEdgeBd struct {
	rows  *beadstest.FakeBd // answers every command but the edge commands and show
	mu    sync.Mutex        // a two-tier List runs its two bd reads at once
	edges []beads.Dep
	calls []string
	// batchErr, when set, is how a multi-id `bd dep list` fails.
	batchErr error
	// removeErr, when set, is how every `bd dep remove` fails.
	removeErr error
}

func newDanglingEdgeBd() *danglingEdgeBd {
	return &danglingEdgeBd{rows: beadstest.NewFakeBd()}
}

// seed stores an open bead titled title.
func (f *danglingEdgeBd) seed(t *testing.T, title string) beads.Bead {
	t.Helper()
	b, err := f.rows.Seed(beads.Bead{Title: title, Type: "task"})
	if err != nil {
		t.Fatalf("seed %q: %v", title, err)
	}
	return b
}

// seedBlocks stores issue's blocks edge to target unvalidated, whatever its
// target, as a create-time dependency or an import writes one.
func (f *danglingEdgeBd) seedBlocks(issue, target string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.edges = append(f.edges, beads.Dep{IssueID: issue, DependsOnID: target, Type: "blocks"})
}

// stored lists issue's edges as the double stores them, "target:type": the
// table itself, not what bd's one-id dep list shows of it.
func (f *danglingEdgeBd) stored(issue string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, e := range f.edges {
		if e.IssueID == issue {
			out = append(out, e.DependsOnID+":"+e.Type)
		}
	}
	slices.Sort(out)
	return out
}

// ran returns the bd commands run so far, each as its space-joined argv.
func (f *danglingEdgeBd) ran() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *danglingEdgeBd) run(dir, name string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, strings.Join(args, " "))
	if len(args) >= 2 && args[0] == "dep" {
		switch args[1] {
		case "list":
			return f.depList(args[2:])
		case "add":
			return f.depAdd(args[2:])
		case "remove":
			return f.depRemove(args[2:])
		}
	}
	if len(args) > 0 && args[0] == "show" {
		return f.show(dir, args[1:])
	}
	return f.rows.Run(dir, name, args...)
}

func (f *danglingEdgeBd) depList(args []string) ([]byte, error) {
	var ids []string
	for _, arg := range args {
		switch {
		case arg == "--json":
		case strings.HasPrefix(arg, "-"):
			return nil, fmt.Errorf("danglingEdgeBd: dep list %s is not modeled", arg)
		default:
			ids = append(ids, arg)
		}
	}
	if len(ids) == 1 {
		rows := []map[string]string{}
		for _, e := range f.edges {
			if e.IssueID != ids[0] {
				continue
			}
			target, err := f.rows.Bead(e.DependsOnID)
			if err != nil {
				continue // the join: an edge whose target has no row is not printed
			}
			rows = append(rows, map[string]string{"id": target.ID, "title": target.Title, "status": target.Status, "dependency_type": e.Type})
		}
		return json.Marshal(rows)
	}
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	records := []map[string]string{}
	for _, id := range ids {
		for _, e := range f.edges {
			if e.IssueID == id {
				records = append(records, map[string]string{"issue_id": e.IssueID, "depends_on_id": e.DependsOnID, "type": e.Type})
			}
		}
	}
	return json.Marshal(records)
}

// edgeArgs splits `bd dep add|remove` arguments into the two ids and --type.
func edgeArgs(args []string) (issue, target, depType string, err error) {
	var ids []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--type" && i+1 < len(args):
			depType = args[i+1]
			i++
		case strings.HasPrefix(args[i], "-"):
			return "", "", "", fmt.Errorf("danglingEdgeBd: flag %s is not modeled", args[i])
		default:
			ids = append(ids, args[i])
		}
	}
	if len(ids) != 2 {
		return "", "", "", fmt.Errorf("danglingEdgeBd: want two ids, got %v", ids)
	}
	return ids[0], ids[1], depType, nil
}

// bdExternalRef is bd's validateExternalRef: an "external:" target must be
// external:<project>:<capability>.
func bdExternalRef(target string) error {
	parts := strings.SplitN(target, ":", 3)
	if len(parts) != 3 || parts[1] == "" || parts[2] == "" {
		return fmt.Errorf("invalid external reference format: expected 'external:<project>:<capability>', got '%s'", target)
	}
	return nil
}

func (f *danglingEdgeBd) depAdd(args []string) ([]byte, error) {
	issue, target, depType, err := edgeArgs(args)
	if err != nil {
		return nil, err
	}
	if _, err := f.rows.Bead(issue); err != nil {
		return nil, fmt.Errorf("resolving %s: no issue found matching %q", issue, issue)
	}
	if strings.HasPrefix(target, "external:") {
		if err := bdExternalRef(target); err != nil {
			return nil, err
		}
	} else if _, err := f.rows.Bead(target); err != nil {
		return nil, fmt.Errorf("resolving dependency ID %s: no issue found matching %q", target, target)
	}
	for _, e := range f.edges {
		if e.IssueID == issue && e.DependsOnID == target {
			return nil, fmt.Errorf("dependency %s -> %s already exists", issue, target)
		}
	}
	f.edges = append(f.edges, beads.Dep{IssueID: issue, DependsOnID: target, Type: depType})
	return []byte(`{"status":"added"}`), nil
}

func (f *danglingEdgeBd) depRemove(args []string) ([]byte, error) {
	issue, target, _, err := edgeArgs(args)
	if err != nil {
		return nil, err
	}
	if f.removeErr != nil {
		return nil, f.removeErr
	}
	if strings.HasPrefix(target, "external:") {
		if err := bdExternalRef(target); err != nil {
			return nil, err
		}
	}
	for i, e := range f.edges {
		if e.IssueID == issue && e.DependsOnID == target {
			f.edges = slices.Delete(f.edges, i, i+1)
			return []byte(`{"status":"removed"}`), nil
		}
	}
	return nil, fmt.Errorf("resolving dependency ID %s: no issue found matching %q", target, target)
}

func (f *danglingEdgeBd) show(dir string, args []string) ([]byte, error) {
	rows := []json.RawMessage{}
	for _, id := range args {
		if strings.HasPrefix(id, "-") {
			continue
		}
		out, err := f.rows.Run(dir, "bd", "show", "--json", id)
		if err != nil {
			continue // bd notes "Issue <id> not found" on stderr and goes on
		}
		var one []json.RawMessage
		if err := json.Unmarshal(out, &one); err != nil {
			return nil, err
		}
		rows = append(rows, one...)
	}
	if len(rows) == 0 {
		return nil, errors.New("no issues found matching the provided IDs")
	}
	return json.Marshal(rows)
}

// bdDoctorCity returns the dangling-edge check of a split city whose work
// store is a *beads.BdStore over bd, and that store.
func bdDoctorCity(t *testing.T, bd *danglingEdgeBd) (*danglingBlockingEdgesCheck, *beads.BdStore) {
	t.Helper()
	cityPath := t.TempDir()
	sessionSplitRoutes(t, cityPath)
	store := beads.NewBdStore("/fake-bd", bd.run)
	return newDanglingBlockingEdgesCheck(nil, cityPath, func(string) (beads.Store, error) { return store, nil }), store
}

// TestDanglingBlockingEdgesReadsALoneBdRowsRawEdges is a bd work store with
// ONE open row. bd's one-row `bd dep list` joins the row's edges onto the
// beads they name, so it never shows the edge this check exists for; the
// check reads bd's raw dependency records, for the scan and again right before
// it writes. A literal whose bead no store holds is removed, and one whose
// bead the row's store holds (closed, so not a row) is rewritten as the typed
// edge.
func TestDanglingBlockingEdgesReadsALoneBdRowsRawEdges(t *testing.T) {
	for _, tc := range []struct {
		name       string
		heldOrigin bool
	}{
		{name: "a literal whose bead no store holds is removed"},
		{name: "a literal whose bead the store holds is rewritten", heldOrigin: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bd := newDanglingEdgeBd()
			originID, action, wantStored := "gc-404", danglingRemove, []string(nil)
			if tc.heldOrigin {
				origin := bd.seed(t, "origin")
				if _, err := bd.rows.Run("", "bd", "close", origin.ID); err != nil {
					t.Fatal(err)
				}
				originID, action, wantStored = origin.ID, danglingRewrite, []string{origin.ID + ":discovered-from"}
			}
			row := bd.seed(t, "lone row")
			literal := "discovered-from:" + originID
			bd.seedBlocks(row.ID, literal)
			check, store := bdDoctorCity(t, bd)
			if deps, err := store.DepList(row.ID, "down"); err != nil || len(deps) != 0 {
				t.Fatalf("bd's one-row dep list = (%v, %v); the double must leave out the edge to %s, as bd does", deps, err, literal)
			}
			edges, skipped := check.collect()
			if len(skipped) != 0 || len(edges) != 1 || edges[0].rowID != row.ID || edges[0].target != literal || edges[0].action != action {
				t.Fatalf("edges = %+v, skipped = %v; want %s -> %s reported with action %d", edges, skipped, row.ID, literal, action)
			}
			if err := check.Fix(nil); err != nil {
				t.Fatalf("Fix: %v", err)
			}
			if got := bd.stored(row.ID); !slices.Equal(got, wantStored) {
				t.Fatalf("edges bd stores for %s after --fix = %v, want %v", row.ID, got, wantStored)
			}
			if res := check.Run(nil); res.Status != doctor.StatusOK {
				t.Fatalf("Run after --fix = %+v, want OK", res)
			}
		})
	}
}

// TestDanglingBlockingEdgesFixOnABdStore runs both rewrite cases and a removal
// through a *beads.BdStore: a plain literal becomes the discovered-from edge, a
// literal beside a real blocks edge is removed with the blocks edge kept, and
// an edge to a bead no store holds is removed. bd is asked to remove each
// literal by its exact target, and never the real edge.
func TestDanglingBlockingEdgesFixOnABdStore(t *testing.T) {
	bd := newDanglingEdgeBd()
	origin := bd.seed(t, "origin")
	plain := bd.seed(t, "plain literal row")
	both := bd.seed(t, "literal and real blocks")
	gone := bd.seed(t, "edge to nothing")
	bd.seedBlocks(plain.ID, "discovered-from:"+origin.ID)
	bd.seedBlocks(both.ID, origin.ID)
	bd.seedBlocks(both.ID, "discovered-from:"+origin.ID)
	bd.seedBlocks(gone.ID, "gc-404")
	check, _ := bdDoctorCity(t, bd)
	if edges, skipped := check.collect(); len(skipped) != 0 || len(edges) != 3 {
		t.Fatalf("edges = %+v, skipped = %v; want the two literals and the edge to gc-404", edges, skipped)
	}
	for i := 0; i < 2; i++ {
		if err := check.Fix(nil); err != nil {
			t.Fatalf("Fix %d: %v", i, err)
		}
	}
	for _, want := range []struct {
		row   string
		edges []string
	}{
		{plain.ID, []string{origin.ID + ":discovered-from"}},
		{both.ID, []string{origin.ID + ":blocks"}},
		{gone.ID, nil},
	} {
		if got := bd.stored(want.row); !slices.Equal(got, want.edges) {
			t.Errorf("edges bd stores for %s after --fix = %v, want %v", want.row, got, want.edges)
		}
	}
	if slices.Contains(bd.ran(), "dep remove "+both.ID+" "+origin.ID) {
		t.Errorf("bd was asked to remove the real blocks edge %s -> %s (commands %q)", both.ID, origin.ID, bd.ran())
	}
	if res := check.Run(nil); res.Status != doctor.StatusOK {
		t.Fatalf("Run after --fix = %+v, want OK", res)
	}
}

// TestDanglingBlockingEdgesSkipsABdScopeWhoseRawEdgesCannotBeRead: when bd's
// batched dep read fails, or the bd backend refuses it, the scope's edges
// cannot be read as stored. Reading the rows one by one instead would hide
// every dangling edge and call the scope clean, so the scope is reported as
// skipped, the check warns, and --fix refuses to act.
func TestDanglingBlockingEdgesSkipsABdScopeWhoseRawEdgesCannotBeRead(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
	}{
		{name: "the batched read fails", err: errors.New("exit status 1: Error: dolt: connection refused")},
		{name: "the backend refuses the batched read", err: errors.New(`exit status 1: Error: operation "IssueRelations" not supported by the postgres backend`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bd := newDanglingEdgeBd()
			row := bd.seed(t, "row")
			const literal = "discovered-from:gc-404"
			bd.seedBlocks(row.ID, literal)
			bd.batchErr = tc.err
			check, _ := bdDoctorCity(t, bd)
			edges, skipped := check.collect()
			if len(edges) != 0 || len(skipped) != 1 || !strings.Contains(skipped[0], "city skipped") {
				t.Fatalf("edges = %+v, skipped = %q; want the city scope reported as skipped, not read row by row", edges, skipped)
			}
			if res := check.Run(nil); res.Status != doctor.StatusWarning {
				t.Fatalf("Run = %+v, want a warning: the city's edges could not be read", res)
			}
			if err := check.Fix(nil); err == nil {
				t.Fatal("Fix succeeded although the city's edges could not be read")
			}
			if got := bd.stored(row.ID); !slices.Equal(got, []string{literal + ":blocks"}) {
				t.Fatalf("edges bd stores for %s = %v, want the literal untouched", row.ID, got)
			}
		})
	}
}

// TestDanglingBlockingEdgesLeavesATwoPartExternalRefInABdStore: bd's `dep
// remove` takes an "external:" target only as external:<project>:<capability>,
// so a dangling "external:<id>" edge in a bd store cannot be removed through
// bd. The check reports it for the operator, saying why, and --fix does not
// ask bd for a removal it refuses. (A store that can remove it does:
// TestDanglingBlockingEdgesCheckFindsAndRemovesOnlyDanglingEdges.)
func TestDanglingBlockingEdgesLeavesATwoPartExternalRefInABdStore(t *testing.T) {
	bd := newDanglingEdgeBd()
	row := bd.seed(t, "row")
	const ref = "external:gc-405"
	bd.seedBlocks(row.ID, ref)
	check, _ := bdDoctorCity(t, bd)
	res := check.Run(nil)
	if res.Status != doctor.StatusWarning || len(res.Details) != 1 || !strings.Contains(res.Details[0], ref) || !strings.Contains(res.Details[0], "external:<project>:<capability>") {
		t.Fatalf("Run = %+v; want %s reported for the operator, with the reason bd will not remove it", res, ref)
	}
	if err := check.Fix(nil); err != nil {
		t.Fatalf("Fix: %v", err)
	}
	for _, call := range bd.ran() {
		if strings.HasPrefix(call, "dep remove ") {
			t.Fatalf("Fix ran bd %s, a removal bd refuses", call)
		}
	}
	if got := bd.stored(row.ID); !slices.Equal(got, []string{ref + ":blocks"}) {
		t.Fatalf("edges bd stores for %s = %v, want %s left for the operator", row.ID, got, ref)
	}
}

// TestDanglingBlockingEdgesFixReportsARemovalBdRefuses: bd before v1.3 refuses
// to remove an edge to a same-prefix bead it cannot resolve. The refusal is
// --fix's error, naming the row and the edge, and the edge is kept.
func TestDanglingBlockingEdgesFixReportsARemovalBdRefuses(t *testing.T) {
	bd := newDanglingEdgeBd()
	row := bd.seed(t, "row")
	bd.seedBlocks(row.ID, "gc-404")
	bd.removeErr = errors.New("exit status 1: Error: resolving the dependency ID: no issue found")
	check, _ := bdDoctorCity(t, bd)
	err := check.Fix(nil)
	if err == nil || !strings.Contains(err.Error(), row.ID) || !strings.Contains(err.Error(), `"gc-404"`) {
		t.Fatalf("Fix = %v; want bd's refusal, naming row %s and the edge to gc-404", err, row.ID)
	}
	if got := bd.stored(row.ID); !slices.Equal(got, []string{"gc-404:blocks"}) {
		t.Fatalf("edges bd stores for %s = %v, want the edge kept", row.ID, got)
	}
}
