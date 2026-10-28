package dispatch

import (
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/rollout/gate"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// sqliteGraphStore opens a real SQLite binding minting gcg-.
func sqliteGraphStore(t *testing.T) beads.Store {
	t.Helper()
	store, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("opening the SQLite graph binding: %v", err)
	}
	t.Cleanup(func() { _ = store.(interface{ CloseStore() error }).CloseStore() })
	return store
}

func newGateControl(t *testing.T, graph beads.Store) beads.Bead {
	t.Helper()
	control, err := graph.Create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gcg-root"}})
	if err != nil {
		t.Fatal(err)
	}
	return control
}

// TestEnsureCrossStoreGateIsIdempotentUnderARace: two minters racing for the
// same (control, blocker) end up with one gate, on a real SQLite binding whose
// pinned-id duplicate check is what makes the create conditional.
func TestEnsureCrossStoreGateIsIdempotentUnderARace(t *testing.T) {
	graph := sqliteGraphStore(t)
	control := newGateControl(t, graph)
	const racers = 8
	ids := make([]string, racers)
	errs := make([]error, racers)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := range racers {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			ids[i], errs[i] = ensureCrossStoreGate(graph, control, "gc-7", true)
		}()
	}
	start.Done()
	done.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("racer %d: %v", i, err)
		}
		if ids[i] == "" || ids[i] != ids[0] {
			t.Fatalf("racers got gates %v, want one shared gate", ids)
		}
	}
	if gates := crossStoreGatesIn(t, graph); len(gates) != 1 {
		t.Fatalf("gates = %+v, want exactly one", gates)
	}
}

// gateStores are the stores a gate lives in: the two engines a binding is
// served from (SQLite; the native Dolt workspace engine, which refuses
// removing a metadata key in an update), both pinning the derived id; a store
// that mints its own id and is found by metadata (MemStore); and one whose
// conditional writes are resolved, so every gate write is a revision CAS.
func gateStores(t *testing.T) map[string]beads.Store {
	return map[string]beads.Store{
		"sqlite (pinned id)":              sqliteGraphStore(t),
		"native dolt (pinned id)":         splittest.NewNativeWorkStore(t, "gcw"),
		"memstore (minted ids)":           beads.NewMemStoreFrom(1000, nil, nil),
		"memstore (conditional, CAS)":     conditionalMemStore(t),
		"sqlite (stamped require, CAS)":   stampedGateStore(t, sqliteGraphStore(t), "SQLiteStore", gate.Require),
		"native dolt (stamped auto, CAS)": stampedGateStore(t, splittest.NewNativeWorkStore(t, "gcn"), "NativeWorkStore", gate.Auto),
	}
}

// stampedGateStore stamps a real engine's conditional-writes mode as the
// factory stamps a production binding, and checks that gate writes then take
// the revision-checked path.
func stampedGateStore(t *testing.T, store beads.Store, kind string, mode gate.Mode) beads.Store {
	t.Helper()
	if err := beads.StampOpenedStore(store, kind, mode, false, nil, nil); err != nil {
		t.Fatalf("stamping %s conditional writes %q: %v", kind, mode, err)
	}
	if w, _, err := beads.ResolveConditionalWriter(store); err != nil || w == nil {
		t.Fatalf("%s stamped %q resolves no conditional writer (%v): gate writes would not be revision-checked", kind, mode, err)
	}
	return store
}

// conditionalMemStore is a MemStore whose conditional writes resolve
// (beads.conditional_writes = require), so gate writes take the CAS path.
func conditionalMemStore(t *testing.T) beads.Store {
	t.Helper()
	store := beads.NewMemStoreFrom(1000, nil, nil)
	if err := beads.StampOpenedStore(store, "MemStore", gate.Require, false, nil, nil); err != nil {
		t.Fatalf("stamping conditional writes: %v", err)
	}
	return store
}

// TestEnsureCrossStoreGateReopensAGateTheControllerReleased: the drain asks for
// a gate only when it has just found the blocker open, so a gate the release
// pass closed (its blocker had closed) is REOPENED — the blocker opened again —
// and the item is held again, as a single-store city holds it behind the
// reopened blocker. No second gate is minted.
func TestEnsureCrossStoreGateReopensAGateTheControllerReleased(t *testing.T) {
	for name, graph := range gateStores(t) {
		t.Run(name, func(t *testing.T) {
			work := beads.NewMemStore()
			work.HonorExplicitIDs = true
			if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "blocker", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			control := newGateControl(t, graph)
			gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
			if err != nil || gateID == "" {
				t.Fatalf("ensureCrossStoreGate = %q, %v", gateID, err)
			}
			if err := work.Close("gc-7"); err != nil {
				t.Fatal(err)
			}
			if report, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil || len(report.Released) != 1 {
				t.Fatalf("release = %+v, %v", report, err)
			}
			again, err := ensureCrossStoreGate(graph, control, "gc-7", true)
			if err != nil || again != gateID {
				t.Fatalf("after release ensureCrossStoreGate = %q, %v; want the released gate %s reopened", again, err, gateID)
			}
			got := mustGetBead(t, graph, gateID)
			if got.Status == "closed" || got.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != "" {
				t.Fatalf("gate = %+v; want it open with its released marker cleared", got)
			}
			if gates := crossStoreGatesIn(t, graph); len(gates) != 1 {
				t.Fatalf("gates = %+v, want the one gate", gates)
			}
		})
	}
}

// TestEnsureCrossStoreGateKeepsAnOperatorOverride: a gate an operator closed
// (no released marker) is final. The drain records the override explicitly
// the first time it sees it, and never reopens or replaces the gate.
func TestEnsureCrossStoreGateKeepsAnOperatorOverride(t *testing.T) {
	for name, graph := range gateStores(t) {
		t.Run(name, func(t *testing.T) {
			control := newGateControl(t, graph)
			gateID, err := ensureCrossStoreGate(graph, control, "gc-404", true)
			if err != nil || gateID == "" {
				t.Fatalf("ensureCrossStoreGate = %q, %v", gateID, err)
			}
			if err := graph.Close(gateID); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 2; i++ {
				again, err := ensureCrossStoreGate(graph, control, "gc-404", true)
				if err != nil || again != "" {
					t.Fatalf("pass %d after the operator closed the gate: %q, %v; want no gate", i, again, err)
				}
			}
			got := mustGetBead(t, graph, gateID)
			if got.Status != "closed" || got.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateOverridden {
				t.Fatalf("gate = %+v; want it closed and recorded as overridden", got)
			}
			if gates := crossStoreGatesIn(t, graph); len(gates) != 1 {
				t.Fatalf("gates = %+v, want only the overridden one", gates)
			}
			// The release pass does not reopen an override either.
			if report, err := ReleaseCrossStoreGates(graph, storesResolver()); err != nil || len(report.Reopened) != 0 {
				t.Fatalf("release = %+v, %v; want no reopen", report, err)
			}
		})
	}
}

// TestReleaseCrossStoreGatesReopensAReleasedGateWhoseBlockerReopened: the
// release pass also follows a blocker that reopens after its gate was
// released, while the drain is live; once the drain's control is closed it
// leaves the gate alone.
func TestReleaseCrossStoreGatesReopensAReleasedGateWhoseBlockerReopened(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	work := beads.NewMemStore()
	work.HonorExplicitIDs = true
	if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "blocker", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	control := newGateControl(t, graph)
	gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := work.Close("gc-7"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil {
		t.Fatal(err)
	}
	if err := work.Reopen("gc-7"); err != nil {
		t.Fatal(err)
	}
	report, err := ReleaseCrossStoreGates(graph, storesResolver(work))
	if err != nil || !slices.Equal(report.Reopened, []string{gateID}) {
		t.Fatalf("release after the blocker reopened = %+v, %v; want gate %s reopened", report, err, gateID)
	}
	if got := mustGetBead(t, graph, gateID); got.Status == "closed" {
		t.Fatalf("gate %s is still closed", gateID)
	}

	if err := work.Close("gc-7"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil {
		t.Fatal(err)
	}
	if err := graph.Close(control.ID); err != nil {
		t.Fatal(err)
	}
	if err := work.Reopen("gc-7"); err != nil {
		t.Fatal(err)
	}
	if report, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil || len(report.Reopened) != 0 {
		t.Fatalf("release with the drain closed = %+v, %v; want no reopen", report, err)
	}
}

// TestEnsureCrossStoreGateGivesEachBlockerItsOwnGate: one drain waiting on
// two blockers holds its items behind two gates, on the pinned-id path as on
// the minted one.
func TestEnsureCrossStoreGateGivesEachBlockerItsOwnGate(t *testing.T) {
	for name, graph := range gateStores(t) {
		t.Run(name, func(t *testing.T) {
			control := newGateControl(t, graph)
			a, err := ensureCrossStoreGate(graph, control, "gc-7", true)
			if err != nil {
				t.Fatal(err)
			}
			b, err := ensureCrossStoreGate(graph, control, "gc-8", true)
			if err != nil {
				t.Fatal(err)
			}
			if a == "" || b == "" || a == b {
				t.Fatalf("gates for gc-7 and gc-8 = %q and %q, want two distinct gates", a, b)
			}
		})
	}
}

// TestReleaseCrossStoreGatesRequiresEveryCopyReleased pins the judge's half of
// "closed in every store": a blocker held closed in one store and open in
// another keeps its gate, and a blocker closed with a failed work outcome keeps
// its gate.
func TestReleaseCrossStoreGatesRequiresEveryCopyReleased(t *testing.T) {
	for _, tc := range []struct {
		name   string
		copies func(t *testing.T) []beads.Store
	}{
		{"a second copy is still open", func(t *testing.T) []beads.Store {
			closed := beads.NewMemStore()
			closed.HonorExplicitIDs = true
			open := beads.NewMemStore()
			open.HonorExplicitIDs = true
			if _, err := closed.Create(beads.Bead{ID: "gc-7", Title: "copy", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			if err := closed.Close("gc-7"); err != nil {
				t.Fatal(err)
			}
			if _, err := open.Create(beads.Bead{ID: "gc-7", Title: "copy", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			return []beads.Store{closed, open}
		}},
		{"closed with a failed outcome", func(t *testing.T) []beads.Store {
			work := beads.NewMemStore()
			work.HonorExplicitIDs = true
			if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "failed", Type: "task", Metadata: map[string]string{beadmeta.WorkOutcomeMetadataKey: beadmeta.WorkOutcomeBlocked}}); err != nil {
				t.Fatal(err)
			}
			if err := work.Close("gc-7"); err != nil {
				t.Fatal(err)
			}
			return []beads.Store{work}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			graph := beads.NewMemStoreFrom(1000, nil, nil)
			control := newGateControl(t, graph)
			gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
			if err != nil || gateID == "" {
				t.Fatalf("ensureCrossStoreGate = %q, %v", gateID, err)
			}
			report, err := ReleaseCrossStoreGates(graph, storesResolver(tc.copies(t)...))
			if err != nil || len(report.Released) != 0 {
				t.Fatalf("release = %+v, %v; want the gate kept: its blocker has not released everywhere", report, err)
			}
		})
	}
}

// TestReleaseCrossStoreGatesReadsEachBlockerOnceAndStripsExternal: the blockers
// of every open gate are resolved in ONE resolver call, each distinct blocker
// once, with an external: marker stripped.
func TestReleaseCrossStoreGatesReadsEachBlockerOnceAndStripsExternal(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	for i := 0; i < 3; i++ {
		control := newGateControl(t, graph)
		if _, err := ensureCrossStoreGate(graph, control, "gc-7", true); err != nil {
			t.Fatal(err)
		}
		if _, err := ensureCrossStoreGate(graph, control, "external:gc-8", true); err != nil {
			t.Fatal(err)
		}
	}
	var calls [][]string
	report, err := ReleaseCrossStoreGates(graph, func(ids []string) (CrossStoreBlockerResolution, error) {
		calls = append(calls, slices.Clone(ids))
		return CrossStoreBlockerResolution{}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || !slices.Equal(calls[0], []string{"gc-7", "gc-8"}) {
		t.Fatalf("resolver calls = %v, want one call naming gc-7 and gc-8 once each", calls)
	}
	if len(report.Unresolved) != 6 || len(report.NewlyUnresolved) != 6 {
		t.Fatalf("report = %+v, want 6 gates newly unresolved", report)
	}
	again, err := ReleaseCrossStoreGates(graph, storesResolver())
	if err != nil || len(again.NewlyUnresolved) != 0 || len(again.Unresolved) != 6 {
		t.Fatalf("second pass = %+v, %v; want the 6 still unresolved and none newly", again, err)
	}
}

// TestReleaseCrossStoreGatesKeepsAGateAnUnreadableStoreMayDecide: a blocker a
// store the resolver could not read may hold is undecided — its gate is neither
// released nor marked unresolved. The hold is reported, not silent: the pass
// carries the stores the resolver could not read, and the gates it holds only
// on them, into its report. A gate an open copy it did read holds is held by
// that copy, not by the store it could not read.
func TestReleaseCrossStoreGatesKeepsAGateAnUnreadableStoreMayDecide(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	var gateIDs []string
	for _, blocker := range []string{"mr-1", "gc-2", "gc-3"} {
		gateID, err := ensureCrossStoreGate(graph, newGateControl(t, graph), blocker, true)
		if err != nil {
			t.Fatal(err)
		}
		gateIDs = append(gateIDs, gateID)
	}
	unreadable := errors.New("rig mr: reading 3 blocker(s): store unavailable")
	report, err := ReleaseCrossStoreGates(graph, func([]string) (CrossStoreBlockerResolution, error) {
		return CrossStoreBlockerResolution{
			// gc-2 is open in a store the resolver read; gc-3 is closed there.
			Copies:     map[string][]beads.Bead{"gc-2": {{ID: "gc-2", Status: "open"}}, "gc-3": {{ID: "gc-3", Status: "closed"}}},
			Undecided:  map[string]bool{"mr-1": true, "gc-2": true, "gc-3": true},
			Unreadable: []error{unreadable},
		}, nil
	})
	if err != nil || len(report.Released)+len(report.Unresolved) != 0 {
		t.Fatalf("release = %+v, %v; want every gate left as it is", report, err)
	}
	for _, gateID := range gateIDs {
		if got := mustGetBead(t, graph, gateID); got.Status == "closed" || got.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != "" {
			t.Fatalf("gate = %+v; want it open and unmarked", got)
		}
	}
	want := []string{gateIDs[0], gateIDs[2]}
	slices.Sort(want)
	if !slices.Equal(report.Undecided, want) {
		t.Fatalf("undecided = %v, want %v: the gates on mr-1 (no copy read) and gc-3 (every copy read releases it), not the one gc-2's open copy holds", report.Undecided, want)
	}
	if len(report.Unreadable) != 1 || !errors.Is(report.Unreadable[0], unreadable) {
		t.Fatalf("unreadable = %v, want the resolver's %v", report.Unreadable, unreadable)
	}
}

// TestReleaseCrossStoreGatesReleasesAGateWhoseControlIsGone: a deleted drain
// control releases its gates.
func TestReleaseCrossStoreGatesReleasesAGateWhoseControlIsGone(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	control := newGateControl(t, graph)
	gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := graph.Delete(control.ID); err != nil {
		t.Fatal(err)
	}
	report, err := ReleaseCrossStoreGates(graph, storesResolver())
	if err != nil || !slices.Equal(report.Retired, []string{gateID}) || len(report.Released) != 0 {
		t.Fatalf("release = %+v, %v; want gate %s retired (its drain control is gone)", report, err, gateID)
	}
	if state := mustGetBead(t, graph, gateID).Metadata[beadmeta.CrossStoreGateStateMetadataKey]; state != CrossStoreGateStateRetired {
		t.Fatalf("gate state = %q, want retired", state)
	}
}

// TestOpenCrossStoreGatesNamingFindsTheGatesADeleteWouldStrand is the helper
// the delete guard calls (S2): it names the open gates waiting on a bead.
func TestOpenCrossStoreGatesNamingFindsTheGatesADeleteWouldStrand(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	gateID, err := ensureCrossStoreGate(graph, newGateControl(t, graph), "gc-7", true)
	if err != nil {
		t.Fatal(err)
	}
	gates, err := OpenCrossStoreGatesNaming(graph, "gc-7")
	if err != nil || len(gates) != 1 || gates[0].ID != gateID {
		t.Fatalf("OpenCrossStoreGatesNaming = %+v, %v; want gate %s", gates, err, gateID)
	}
	if err := graph.Close(gateID); err != nil {
		t.Fatal(err)
	}
	if gates, err := OpenCrossStoreGatesNaming(graph, "gc-7"); err != nil || len(gates) != 0 {
		t.Fatalf("after release = %+v, %v; want none", gates, err)
	}
}

// releasedGate mints a gate on gc-7 under a fresh control and releases it
// through the pass (gc-7 closed in work), returning the control and gate id.
func releasedGate(t *testing.T, graph beads.Store, work *beads.MemStore) (beads.Bead, string) {
	t.Helper()
	if _, err := work.Get("gc-7"); err != nil {
		if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "blocker", Type: "task"}); err != nil {
			t.Fatal(err)
		}
	}
	control := newGateControl(t, graph)
	gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
	if err != nil || gateID == "" {
		t.Fatalf("ensureCrossStoreGate = %q, %v", gateID, err)
	}
	if b, _ := work.Get("gc-7"); b.Status != "closed" {
		if err := work.Close("gc-7"); err != nil {
			t.Fatal(err)
		}
	}
	if report, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil || !slices.Contains(report.Released, gateID) {
		t.Fatalf("release = %+v, %v; want %s released", report, err, gateID)
	}
	return control, gateID
}

func honoringMem() *beads.MemStore {
	m := beads.NewMemStore()
	m.HonorExplicitIDs = true
	return m
}

// TestEnsureCrossStoreGateLeavesAReleasedGateWhenTheBlockerIsNotFound: the
// drain reopens a controller-closed gate only when it FOUND the blocker open.
// A blocker its narrower residency frame cannot find (another rig, which the
// release pass reads) leaves the gate released — no release/reopen flap.
func TestEnsureCrossStoreGateLeavesAReleasedGateWhenTheBlockerIsNotFound(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	work := honoringMem()
	control, gateID := releasedGate(t, graph, work)
	got, err := ensureCrossStoreGate(graph, control, "gc-7", false)
	if err != nil || got != "" {
		t.Fatalf("ensureCrossStoreGate(not found) = %q, %v; want no gate (left released)", got, err)
	}
	if g := mustGetBead(t, graph, gateID); g.Status != "closed" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateReleased {
		t.Fatalf("gate = %+v; want it still closed and released", g)
	}
}

// TestReleaseCrossStoreGatesKeepsAReleasedGateWhileItsBlockerStaysClosed: a
// pass over a released gate whose blocker is still closed reopens nothing.
func TestReleaseCrossStoreGatesKeepsAReleasedGateWhileItsBlockerStaysClosed(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	work := honoringMem()
	_, gateID := releasedGate(t, graph, work)
	for i := 0; i < 2; i++ {
		report, err := ReleaseCrossStoreGates(graph, storesResolver(work))
		if err != nil || len(report.Reopened) != 0 {
			t.Fatalf("pass %d = %+v, %v; want no reopen while gc-7 is closed", i, report, err)
		}
	}
	if g := mustGetBead(t, graph, gateID); g.Status != "closed" {
		t.Fatalf("gate %s reopened", gateID)
	}
}

// TestReleaseCrossStoreGatesDoesNotReopenOnAPurgedOrUnreadableBlocker: a
// released gate reopens only when some store holds an unreleased copy. A
// blocker that is gone (purged after closing) is not one, and neither is a
// blocker that an unreadable store may hold.
func TestReleaseCrossStoreGatesDoesNotReopenOnAPurgedOrUnreadableBlocker(t *testing.T) {
	t.Run("purged", func(t *testing.T) {
		graph := beads.NewMemStoreFrom(1000, nil, nil)
		work := honoringMem()
		_, gateID := releasedGate(t, graph, work)
		if err := work.Delete("gc-7"); err != nil {
			t.Fatal(err)
		}
		report, err := ReleaseCrossStoreGates(graph, storesResolver(work))
		if err != nil || len(report.Reopened) != 0 || mustGetBead(t, graph, gateID).Status != "closed" {
			t.Fatalf("after purge = %+v, %v; want the gate left released", report, err)
		}
	})
	t.Run("unreadable store", func(t *testing.T) {
		graph := beads.NewMemStoreFrom(1000, nil, nil)
		work := honoringMem()
		_, gateID := releasedGate(t, graph, work)
		if err := work.Reopen("gc-7"); err != nil {
			t.Fatal(err)
		}
		open, _ := work.Get("gc-7")
		report, err := ReleaseCrossStoreGates(graph, func([]string) (CrossStoreBlockerResolution, error) {
			// The open copy is visible, but another store that may hold a
			// copy could not be read: undecided.
			return CrossStoreBlockerResolution{
				Copies:    map[string][]beads.Bead{"gc-7": {open}},
				Undecided: map[string]bool{"gc-7": true},
			}, nil
		})
		if err != nil || len(report.Reopened) != 0 || mustGetBead(t, graph, gateID).Status != "closed" {
			t.Fatalf("with gc-7 undecided = %+v, %v; want the gate left released", report, err)
		}
	})
}

// failingUpdates fails every Update on one id.
type failingUpdates struct {
	beads.Store
	failID string
}

func (s failingUpdates) Update(id string, opts beads.UpdateOpts) error {
	if id == s.failID {
		return errors.New("injected: write failed")
	}
	return s.Store.Update(id, opts)
}

// TestReopenIsOneWriteSoAFailureLeavesNoHalfState: a reopen that fails leaves
// the gate exactly as it was (closed, released) — never open with its released
// marker, whose later operator close would read as a controller release.
func TestReopenIsOneWriteSoAFailureLeavesNoHalfState(t *testing.T) {
	inner := beads.NewMemStoreFrom(1000, nil, nil)
	work := honoringMem()
	_, gateID := releasedGate(t, inner, work)
	if err := work.Reopen("gc-7"); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseCrossStoreGates(failingUpdates{Store: inner, failID: gateID}, storesResolver(work)); err == nil {
		t.Fatal("the reopen's failure was not reported")
	}
	g := mustGetBead(t, inner, gateID)
	if g.Status != "closed" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateReleased {
		t.Fatalf("gate after a failed reopen = %s/%q; want closed/released, untouched", g.Status, g.Metadata[beadmeta.CrossStoreGateStateMetadataKey])
	}
	// The next pass reopens it, and clears the marker and the close reason
	// (empty or absent: some engines refuse removing a key in an update, so
	// the reopen sets them empty in the same write).
	if _, err := ReleaseCrossStoreGates(inner, storesResolver(work)); err != nil {
		t.Fatal(err)
	}
	g = mustGetBead(t, inner, gateID)
	if g.Status == "closed" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != "" || g.Metadata["close_reason"] != "" {
		t.Fatalf("gate after the reopen = %s with metadata %v; want open with no state and no close reason", g.Status, g.Metadata)
	}
}

// listCounting counts a gate store's List calls and id-batched reads.
type listCounting struct {
	beads.Store
	lists, idReads int
}

func (s *listCounting) List(q beads.ListQuery) ([]beads.Bead, error) {
	s.lists++
	if len(q.IDs) > 0 {
		s.idReads++
	}
	return s.Store.List(q)
}

// TestReleaseCrossStoreGatesRetiresFinishedDrainsAndThenReadsOnlyItsListings:
// a released gate whose drain is closed is retired once; after that a pass
// with nothing gated reads the two gate listings and nothing else — no
// control, no resolver call.
func TestReleaseCrossStoreGatesRetiresFinishedDrainsAndThenReadsOnlyItsListings(t *testing.T) {
	inner := beads.NewMemStoreFrom(1000, nil, nil)
	work := honoringMem()
	control, gateID := releasedGate(t, inner, work)
	if err := inner.Close(control.ID); err != nil {
		t.Fatal(err)
	}
	report, err := ReleaseCrossStoreGates(inner, storesResolver(work))
	if err != nil || !slices.Equal(report.Retired, []string{gateID}) {
		t.Fatalf("pass = %+v, %v; want gate %s retired", report, err, gateID)
	}
	counting := &listCounting{Store: inner}
	resolved := 0
	report, err = ReleaseCrossStoreGates(counting, func([]string) (CrossStoreBlockerResolution, error) {
		resolved++
		return CrossStoreBlockerResolution{}, nil
	})
	if err != nil || report.Open != 0 || len(report.Retired) != 0 {
		t.Fatalf("idle pass = %+v, %v", report, err)
	}
	if counting.lists != 2 || counting.idReads != 0 || resolved != 0 {
		t.Fatalf("idle pass read %d list(s), %d id batch(es), %d resolver call(s); want the two gate listings only", counting.lists, counting.idReads, resolved)
	}
}

// racingStore hides the pinned gate from the up-front reads, as if a racing
// minter created it between them and this caller's Create.
type racingStore struct {
	beads.Store
	pinned string
	hidden bool
}

func (s *racingStore) Get(id string) (beads.Bead, error) {
	if id == s.pinned && s.hidden {
		s.hidden = false
		return beads.Bead{}, beads.ErrNotFound
	}
	return s.Store.Get(id)
}

func (s *racingStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	if q.Label == CrossStoreGateLabel && len(q.Metadata) > 0 {
		return nil, nil
	}
	return s.Store.List(q)
}

// IDPrefix forwards the binding's mint prefix, so the gate id is pinned.
func (s *racingStore) IDPrefix() string { return "gcg" }

// TestEnsureCrossStoreGateSettlesARacingMintersGateLikeAnyOther: a gate a
// racing minter won is settled exactly like one found up front — a released
// one is not handed back as holding when the blocker was not found open.
func TestEnsureCrossStoreGateSettlesARacingMintersGateLikeAnyOther(t *testing.T) {
	graph := sqliteGraphStore(t)
	work := honoringMem()
	control, gateID := releasedGate(t, graph, work)
	race := &racingStore{Store: graph, pinned: gateID, hidden: true}
	got, err := ensureCrossStoreGate(race, control, "gc-7", false)
	if err != nil || got != "" {
		t.Fatalf("racing ensureCrossStoreGate(not found open) = %q, %v; want no gate: the raced gate is released", got, err)
	}
}

// TestEnsureCrossStoreGateTreatsATeardownCloseAsTheControllers: a workflow
// teardown (CloseSubtree) closes the gate with a stamped close_reason and no
// state marker. That is the controller's close, not an operator override: a
// retried drain that finds the blocker open reopens it.
func TestEnsureCrossStoreGateTreatsATeardownCloseAsTheControllers(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	control := newGateControl(t, graph)
	gateID, err := ensureCrossStoreGate(graph, control, "gc-7", true)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := graph.CloseAll([]string{gateID}, map[string]string{"close_reason": molecule.SubtreeClosedReason}); err != nil {
		t.Fatal(err)
	}
	// The pass never judged the blocker released, so even a drain that does
	// not find the blocker hands the gate back reopened, for re-judgment.
	if got, err := ensureCrossStoreGate(graph, control, "gc-7", false); err != nil || got != gateID {
		t.Fatalf("not found open: %q, %v; want the torn-down gate %s reopened for the pass to judge", got, err, gateID)
	}
	g := mustGetBead(t, graph, gateID)
	if g.Status != "open" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != "" || g.Metadata["close_reason"] != "" {
		t.Fatalf("gate = %s %v; want open with its state and close reason cleared (never an operator override)", g.Status, g.Metadata)
	}
	if got, err := ensureCrossStoreGate(graph, control, "gc-7", true); err != nil || got != gateID {
		t.Fatalf("found open: %q, %v; want gate %s holding", got, err, gateID)
	}
}

// TestReleaseCrossStoreGatesStampsWhenAGateWasFirstFoundUnresolved: the
// first_seen anchor control.stalled reports is the persisted time the gate
// was first found unresolved, and a later pass does not move it.
func TestReleaseCrossStoreGatesStampsWhenAGateWasFirstFoundUnresolved(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	gateID, err := ensureCrossStoreGate(graph, newGateControl(t, graph), "gc-404", true)
	if err != nil {
		t.Fatal(err)
	}
	prev := crossStoreGateNow
	t.Cleanup(func() { crossStoreGateNow = prev })
	first := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	crossStoreGateNow = func() time.Time { return first }
	if _, err := ReleaseCrossStoreGates(graph, storesResolver()); err != nil {
		t.Fatal(err)
	}
	crossStoreGateNow = func() time.Time { return first.Add(time.Hour) }
	if _, err := ReleaseCrossStoreGates(graph, storesResolver()); err != nil {
		t.Fatal(err)
	}
	if got := mustGetBead(t, graph, gateID).Metadata[beadmeta.CrossStoreGateUnresolvedAtMetadataKey]; got != first.Format(time.RFC3339) {
		t.Fatalf("unresolved_at = %q, want the first pass's %q", got, first.Format(time.RFC3339))
	}
}

// TestReleaseCrossStoreGatesClearsAnUnresolvedMarkOnceItsBlockerIsBack: a gate
// marked unresolved whose blocker is restored is an ordinary open gate again,
// held by its blocker — so gc doctor stops recommending an override close — in
// one write. A blocker that then disappears again is a new unresolved
// episode, reported afresh with its own anchor.
func TestReleaseCrossStoreGatesClearsAnUnresolvedMarkOnceItsBlockerIsBack(t *testing.T) {
	const (
		stateKey = beadmeta.CrossStoreGateStateMetadataKey
		atKey    = beadmeta.CrossStoreGateUnresolvedAtMetadataKey
	)
	inner := beads.NewMemStoreFrom(1000, nil, nil)
	rig := honoringMem()
	gateID, err := ensureCrossStoreGate(inner, newGateControl(t, inner), "rg-9", false)
	if err != nil {
		t.Fatal(err)
	}
	prev := crossStoreGateNow
	t.Cleanup(func() { crossStoreGateNow = prev })
	first := time.Date(2026, 10, 27, 9, 0, 0, 0, time.UTC)
	crossStoreGateNow = func() time.Time { return first }
	if report, err := ReleaseCrossStoreGates(inner, storesResolver(rig)); err != nil || len(report.NewlyUnresolved) != 1 {
		t.Fatalf("pass with rg-9 nowhere = %+v, %v; want the gate newly unresolved", report, err)
	}

	if _, err := rig.Create(beads.Bead{ID: "rg-9", Title: "restored blocker", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	counting := &writeCounting{Store: inner, id: gateID}
	report, err := ReleaseCrossStoreGates(counting, storesResolver(rig))
	if err != nil || len(report.Unresolved) != 0 || len(report.Released) != 0 {
		t.Fatalf("pass with rg-9 restored open = %+v, %v; want the gate held by its blocker, no longer unresolved", report, err)
	}
	if g := mustGetBead(t, inner, gateID); g.Status == "closed" || g.Metadata[stateKey] != "" || g.Metadata[atKey] != "" {
		t.Fatalf("gate = %s %v; want it open with its unresolved mark and anchor cleared", g.Status, g.Metadata)
	}
	if counting.writes != 1 {
		t.Fatalf("clearing the mark took %d writes to the gate, want exactly 1", counting.writes)
	}

	if err := rig.Delete("rg-9"); err != nil {
		t.Fatal(err)
	}
	later := first.Add(24 * time.Hour)
	crossStoreGateNow = func() time.Time { return later }
	report, err = ReleaseCrossStoreGates(inner, storesResolver(rig))
	if err != nil || len(report.NewlyUnresolved) != 1 || report.NewlyUnresolved[0].Metadata[atKey] != later.Format(time.RFC3339) {
		t.Fatalf("pass after rg-9 vanished again = %+v, %v; want the gate newly unresolved, anchored at %s", report, err, later.Format(time.RFC3339))
	}
}

// TestCrossStoreGateLifecycleOnEveryGateStore runs the whole gate cycle on
// every store a gate lives in: mint, release when the blocker closes, reopen
// when it reopens (with the marker and close reason cleared), retire when the
// drain finishes.
func TestCrossStoreGateLifecycleOnEveryGateStore(t *testing.T) {
	for name, graph := range gateStores(t) {
		t.Run(name, func(t *testing.T) {
			work := honoringMem()
			control, gateID := releasedGate(t, graph, work)
			if err := work.Reopen("gc-7"); err != nil {
				t.Fatal(err)
			}
			report, err := ReleaseCrossStoreGates(graph, storesResolver(work))
			if err != nil || !slices.Equal(report.Reopened, []string{gateID}) {
				t.Fatalf("reopen pass = %+v, %v; want gate %s reopened", report, err, gateID)
			}
			g := mustGetBead(t, graph, gateID)
			if g.Status == "closed" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != "" || g.Metadata["close_reason"] != "" {
				t.Fatalf("reopened gate = %s %v; want open with no state and no close reason", g.Status, g.Metadata)
			}
			if err := work.Close("gc-7"); err != nil {
				t.Fatal(err)
			}
			if report, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil || !slices.Equal(report.Released, []string{gateID}) {
				t.Fatalf("release pass = %+v, %v", report, err)
			}
			if err := graph.Close(control.ID); err != nil {
				t.Fatal(err)
			}
			if report, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil || !slices.Equal(report.Retired, []string{gateID}) {
				t.Fatalf("retire pass = %+v, %v", report, err)
			}
		})
	}
}

// TestEnsureCrossStoreGateReopensARetiredGateFoundOpen: a retired gate is the
// controller's close, so a retried drain that FINDS the blocker open reopens
// it rather than reading it as an operator override.
func TestEnsureCrossStoreGateReopensARetiredGateFoundOpen(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	work := honoringMem()
	control, gateID := releasedGate(t, graph, work)
	if err := graph.Close(control.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil {
		t.Fatal(err)
	}
	if err := graph.Reopen(control.ID); err != nil {
		t.Fatal(err)
	}
	got, err := ensureCrossStoreGate(graph, control, "gc-7", true)
	if err != nil || got != gateID {
		t.Fatalf("ensureCrossStoreGate(found open) on a retired gate = %q, %v; want %s reopened", got, err, gateID)
	}
}

// TestEnsureCrossStoreGateHandsARetiredGateBackToTheReleasePass: a retired
// gate met by a live drain that does not find the blocker is marked released
// again, so the release pass — which reads every store — re-judges it.
func TestEnsureCrossStoreGateHandsARetiredGateBackToTheReleasePass(t *testing.T) {
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	work := honoringMem()
	control, gateID := releasedGate(t, graph, work)
	if err := graph.Close(control.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil {
		t.Fatal(err)
	}
	if err := graph.Reopen(control.ID); err != nil {
		t.Fatal(err)
	}
	if got, err := ensureCrossStoreGate(graph, control, "gc-7", false); err != nil || got != "" {
		t.Fatalf("ensureCrossStoreGate(not found) = %q, %v; want no gate yet", got, err)
	}
	if state := mustGetBead(t, graph, gateID).Metadata[beadmeta.CrossStoreGateStateMetadataKey]; state != CrossStoreGateStateReleased {
		t.Fatalf("gate state = %q, want released (handed back to the pass)", state)
	}
	if err := work.Reopen("gc-7"); err != nil {
		t.Fatal(err)
	}
	if report, err := ReleaseCrossStoreGates(graph, storesResolver(work)); err != nil || !slices.Equal(report.Reopened, []string{gateID}) {
		t.Fatalf("pass = %+v, %v; want the handed-back gate reopened (its blocker is open)", report, err)
	}
}

// writeCounting counts the writes made to one bead id.
type writeCounting struct {
	beads.Store
	id     string
	writes int
}

func (s *writeCounting) Update(id string, opts beads.UpdateOpts) error {
	if id == s.id {
		s.writes++
	}
	return s.Store.Update(id, opts)
}

func (s *writeCounting) Close(id string) error {
	if id == s.id {
		s.writes++
	}
	return s.Store.Close(id)
}

func (s *writeCounting) SetMetadata(id, k, v string) error {
	if id == s.id {
		s.writes++
	}
	return s.Store.SetMetadata(id, k, v)
}

func (s *writeCounting) SetMetadataBatch(id string, kvs map[string]string) error {
	if id == s.id {
		s.writes++
	}
	return s.Store.SetMetadataBatch(id, kvs)
}

// TestReleaseIsOneWrite: the release closes the gate and records its state
// and reason in a single write.
func TestReleaseIsOneWrite(t *testing.T) {
	inner := beads.NewMemStoreFrom(1000, nil, nil)
	work := honoringMem()
	if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "blocker", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	gateID, err := ensureCrossStoreGate(inner, newGateControl(t, inner), "gc-7", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := work.Close("gc-7"); err != nil {
		t.Fatal(err)
	}
	counting := &writeCounting{Store: inner, id: gateID}
	if report, err := ReleaseCrossStoreGates(counting, storesResolver(work)); err != nil || len(report.Released) != 1 {
		t.Fatalf("release = %+v, %v", report, err)
	}
	if counting.writes != 1 {
		t.Fatalf("the release made %d writes to the gate, want exactly 1", counting.writes)
	}
}

// operatorClosesOnList closes the gate (an operator's close) right after the
// pass lists the open gates, before the pass writes.
type operatorClosesOnList struct {
	beads.Store
	gateID string
	done   bool
}

func (s *operatorClosesOnList) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.Store.List(q)
	if !s.done && q.Label == CrossStoreGateLabel && q.Status == "open" {
		s.done = true
		if cerr := s.Close(s.gateID); cerr != nil {
			return nil, cerr
		}
	}
	return rows, err
}

// TestReleaseNeverOverwritesAnOperatorCloseThatRacedThePass: an operator
// closes the gate between the pass's listing and its write. The pass re-reads
// the gate live and leaves the operator's close alone — it is not stamped as a
// controller release, so the drain still reads it as an override.
func TestReleaseNeverOverwritesAnOperatorCloseThatRacedThePass(t *testing.T) {
	for name, mk := range map[string]func() beads.Store{
		"live re-check":    func() beads.Store { return beads.NewMemStoreFrom(1000, nil, nil) },
		"conditional, CAS": func() beads.Store { return conditionalMemStore(t) },
	} {
		t.Run(name, func(t *testing.T) {
			inner := mk()
			work := honoringMem()
			if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "blocker", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			control := newGateControl(t, inner)
			gateID, err := ensureCrossStoreGate(inner, control, "gc-7", true)
			if err != nil {
				t.Fatal(err)
			}
			if err := work.Close("gc-7"); err != nil {
				t.Fatal(err)
			}
			race := &operatorClosesOnList{Store: inner, gateID: gateID}
			report, err := ReleaseCrossStoreGates(race, storesResolver(work))
			if err != nil || len(report.Released) != 0 {
				t.Fatalf("pass = %+v, %v; want nothing released over the operator's close", report, err)
			}
			if state := mustGetBead(t, inner, gateID).Metadata[beadmeta.CrossStoreGateStateMetadataKey]; state != "" {
				t.Fatalf("gate state = %q; the operator's close was stamped by the controller", state)
			}
			if got, err := ensureCrossStoreGate(inner, control, "gc-7", true); err != nil || got != "" {
				t.Fatalf("drain after the race = %q, %v; want the operator's override honored", got, err)
			}
		})
	}
}

// casRace serves one stale read of a gate: the live re-read returns the gate
// open, and an operator closes it right after, before the pass's write. Only
// the write's revision check can see that.
type casRace struct {
	*beads.MemStore
	id    string
	fired bool
}

func (s *casRace) Get(id string) (beads.Bead, error) {
	b, err := s.MemStore.Get(id)
	if err == nil && id == s.id && !s.fired && b.Status != "closed" {
		s.fired = true
		if cerr := s.Close(id); cerr != nil {
			return beads.Bead{}, cerr
		}
	}
	return b, err
}

// TestReleaseCASRefusesAWriteOverAnOperatorClose: with conditional writes
// resolved, an operator close that lands between the pass's live re-read and
// its write fails the write's revision check; the pass re-reads, sees the
// operator's close, and leaves it alone.
func TestReleaseCASRefusesAWriteOverAnOperatorClose(t *testing.T) {
	mem := beads.NewMemStoreFrom(1000, nil, nil)
	if err := beads.StampOpenedStore(mem, "MemStore", gate.Require, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	work := honoringMem()
	if _, err := work.Create(beads.Bead{ID: "gc-7", Title: "blocker", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	gateID, err := ensureCrossStoreGate(mem, newGateControl(t, mem), "gc-7", true)
	if err != nil {
		t.Fatal(err)
	}
	if err := work.Close("gc-7"); err != nil {
		t.Fatal(err)
	}
	race := &casRace{MemStore: mem, id: gateID}
	if writer, _, err := beads.ResolveConditionalWriter(race); err != nil || writer == nil {
		t.Fatalf("fixture: conditional writes do not resolve (%v, %v)", writer, err)
	}
	report, err := ReleaseCrossStoreGates(race, storesResolver(work))
	if err != nil || len(report.Released) != 0 {
		t.Fatalf("pass = %+v, %v; want the CAS to refuse the release", report, err)
	}
	if !race.fired {
		t.Fatal("fixture: the stale read never happened")
	}
	if state := mustGetBead(t, mem, gateID).Metadata[beadmeta.CrossStoreGateStateMetadataKey]; state != "" {
		t.Fatalf("gate state = %q; a release was written over the operator's close", state)
	}
}

// undecidedResolver answers every blocker undecided: a store that may hold it
// could not be read (or is suspended).
func undecidedResolver(ids []string) (CrossStoreBlockerResolution, error) {
	undecided := make(map[string]bool, len(ids))
	for _, id := range ids {
		undecided[id] = true
	}
	return CrossStoreBlockerResolution{Undecided: undecided}, nil
}

// TestAReopenedDrainHoldsAGateTheReleasePassNeverReleased: a gate the pass
// never released — its blocker held by no store (unresolved), or by a store
// the pass cannot read (undecided) — is retired when its drain control
// closes. The control is reopened: the drain hands the gate back by reopening
// it, so the item is held again and the pass judges the gate afresh. Marking
// it released would let the item run on a judgement the pass never made.
func TestAReopenedDrainHoldsAGateTheReleasePassNeverReleased(t *testing.T) {
	for _, tc := range []struct {
		name           string
		resolver       CrossStoreBlockerResolver
		wantUnresolved bool
	}{
		{name: "blocker no store holds (unresolved)", resolver: storesResolver(beads.NewMemStore()), wantUnresolved: true},
		{name: "blocker in an unreadable or suspended store (undecided)", resolver: undecidedResolver},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for name, graph := range gateStores(t) {
				t.Run(name, func(t *testing.T) {
					control := newGateControl(t, graph)
					gateID, err := ensureCrossStoreGate(graph, control, "rg-9", false)
					if err != nil || gateID == "" {
						t.Fatalf("mint: %q, %v", gateID, err)
					}
					if _, err := ReleaseCrossStoreGates(graph, tc.resolver); err != nil {
						t.Fatal(err)
					}
					if err := graph.Close(control.ID); err != nil {
						t.Fatal(err)
					}
					if report, err := ReleaseCrossStoreGates(graph, tc.resolver); err != nil || !slices.Equal(report.Retired, []string{gateID}) {
						t.Fatalf("control closed: %+v, %v; want the gate retired", report, err)
					}
					if g := mustGetBead(t, graph, gateID); g.Metadata[beadmeta.CrossStoreGateRetiredFromMetadataKey] != "open" {
						t.Fatalf("retired gate metadata = %v; want it to record it was retired from open", g.Metadata)
					}
					if err := graph.Reopen(control.ID); err != nil {
						t.Fatal(err)
					}
					for tick := 0; tick < 3; tick++ {
						got, err := ensureCrossStoreGate(graph, mustGetBead(t, graph, control.ID), "rg-9", false)
						if err != nil || got != gateID {
							t.Fatalf("tick %d: drain = %q, %v; want the gate %s holding the item", tick, got, err, gateID)
						}
						report, err := ReleaseCrossStoreGates(graph, tc.resolver)
						if err != nil {
							t.Fatal(err)
						}
						if len(report.Released) != 0 || len(report.Retired) != 0 {
							t.Fatalf("tick %d: pass = %+v; want the gate left holding", tick, report)
						}
						if got := slices.ContainsFunc(report.Unresolved, func(g beads.Bead) bool { return g.ID == gateID }); got != tc.wantUnresolved {
							t.Fatalf("tick %d: unresolved = %v; want %v", tick, report.Unresolved, tc.wantUnresolved)
						}
					}
					g := mustGetBead(t, graph, gateID)
					if g.Status != "open" || g.Metadata[beadmeta.CrossStoreGateRetiredFromMetadataKey] != "" || g.Metadata["close_reason"] != "" {
						t.Fatalf("gate = %s %v; want open with its retirement cleared", g.Status, g.Metadata)
					}
				})
			}
		})
	}
}

// TestAReopenedDrainHandsAGateRetiredFromReleasedBackAsReleased: a gate the
// pass had released (its blocker closed in a rig the drain cannot see) and
// then retired goes back to released, with a close_reason that says so, in one
// write; it stays released tick after tick (no write per tick).
func TestAReopenedDrainHandsAGateRetiredFromReleasedBackAsReleased(t *testing.T) {
	for name, graph := range gateStores(t) {
		t.Run(name, func(t *testing.T) {
			rig := beads.NewMemStore()
			rig.HonorExplicitIDs = true
			if _, err := rig.Create(beads.Bead{ID: "rg-9", Title: "rig blocker", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			control := newGateControl(t, graph)
			gateID, err := ensureCrossStoreGate(graph, control, "rg-9", false)
			if err != nil {
				t.Fatal(err)
			}
			if err := rig.Close("rg-9"); err != nil {
				t.Fatal(err)
			}
			if report, err := ReleaseCrossStoreGates(graph, storesResolver(rig)); err != nil || len(report.Released) != 1 {
				t.Fatalf("release: %+v, %v", report, err)
			}
			if err := graph.Close(control.ID); err != nil {
				t.Fatal(err)
			}
			if report, err := ReleaseCrossStoreGates(graph, storesResolver(rig)); err != nil || len(report.Retired) != 1 {
				t.Fatalf("retire: %+v, %v", report, err)
			}
			if g := mustGetBead(t, graph, gateID); g.Metadata[beadmeta.CrossStoreGateRetiredFromMetadataKey] != CrossStoreGateStateReleased {
				t.Fatalf("retired gate metadata = %v; want retired_from released", g.Metadata)
			}
			if err := graph.Reopen(control.ID); err != nil {
				t.Fatal(err)
			}
			var revs []int64
			for tick := 0; tick < 3; tick++ {
				got, err := ensureCrossStoreGate(graph, mustGetBead(t, graph, control.ID), "rg-9", false)
				if err != nil || got != "" {
					t.Fatalf("tick %d: drain = %q, %v; want the released gate left released", tick, got, err)
				}
				if _, err := ReleaseCrossStoreGates(graph, storesResolver(rig)); err != nil {
					t.Fatal(err)
				}
				revs = append(revs, mustGetBead(t, graph, gateID).Revision)
			}
			g := mustGetBead(t, graph, gateID)
			if g.Status != "closed" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateReleased ||
				g.Metadata[beadmeta.CrossStoreGateRetiredFromMetadataKey] != "" || !strings.Contains(g.Metadata["close_reason"], "handed back") {
				t.Fatalf("gate = %s %v; want released, retirement cleared, close_reason restamped", g.Status, g.Metadata)
			}
			if revs[0] != revs[2] {
				t.Fatalf("gate revisions per tick = %v; want no write once handed back", revs)
			}
			// The blocker reopens: the pass reopens the handed-back gate.
			if err := rig.Reopen("rg-9"); err != nil {
				t.Fatal(err)
			}
			if report, err := ReleaseCrossStoreGates(graph, storesResolver(rig)); err != nil || !slices.Equal(report.Reopened, []string{gateID}) {
				t.Fatalf("blocker reopened: %+v, %v; want the gate reopened", report, err)
			}
		})
	}
}

// TestEveryControllerSubtreeCloseReadsAsAControllerClose runs each real close
// path that reaches a gate (a gate carries its drain's root, so every subtree
// close of that root closes it) and checks the gate reads as closed by the
// controller — a drain that finds its blocker open reopens it — never as an
// operator's final override. Paths outside this package call the same
// function with the same reason; wisp GC and the run-cancel handler are also
// pinned in their own packages.
func TestEveryControllerSubtreeCloseReadsAsAControllerClose(t *testing.T) {
	finalize := func(cancel bool) func(t *testing.T, store beads.Store, rootID string) {
		return func(t *testing.T, store beads.Store, rootID string) {
			t.Helper()
			if cancel {
				if err := store.SetMetadata(rootID, beadmeta.CancelRequestedMetadataKey, "true"); err != nil {
					t.Fatal(err)
				}
			}
			finalizer := mustCreateWorkflowBead(t, store, beads.Bead{
				Title:    "Finalize workflow",
				Type:     "task",
				Metadata: map[string]string{"gc.kind": "workflow-finalize", "gc.root_bead_id": rootID},
			})
			mustDepAdd(t, store, rootID, finalizer.ID, "blocks")
			if _, err := ProcessControl(store, finalizer, ProcessOptions{}); err != nil {
				t.Fatalf("ProcessControl(workflow-finalize): %v", err)
			}
		}
	}
	for _, tc := range []struct {
		name  string
		close func(t *testing.T, store beads.Store, rootID string)
	}{
		{"sling attachment of a molecule (molecule.CloseSubtree)", func(t *testing.T, store beads.Store, rootID string) {
			if _, err := molecule.CloseSubtree(store, rootID); err != nil {
				t.Fatal(err)
			}
		}},
		{"sling, gc formula, drain teardown, workflow attachment (sourceworkflow.CloseWorkflowSubtree)", func(t *testing.T, store beads.Store, rootID string) {
			if _, err := sourceworkflow.CloseWorkflowSubtree(store, rootID); err != nil {
				t.Fatal(err)
			}
		}},
		{"wisp GC repair of a closed root", func(t *testing.T, store beads.Store, rootID string) {
			if _, err := molecule.CloseSubtreeWithMetadata(store, rootID, map[string]string{
				beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
				"close_reason":              sourceworkflow.WorkflowSkippedCloseReason,
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"workflow finalizer, pass", finalize(false)},
		{"workflow finalizer, canceled run", finalize(true)},
		{"gc hook --claim skip of a done workflow (closes by gc.root_bead_id)", closeByRootWith(sourceworkflow.HookSkipDoneWorkflowCloseReason)},
		{"order-tracking sweep of a stale order wisp subtree", closeByRootWith(sourceworkflow.StaleOrderWispCloseReason)},
		{"POST /runs/{id}/cancel", func(t *testing.T, store beads.Store, rootID string) {
			if _, err := sourceworkflow.CloseWorkflowSubtreeAsExcept(store, rootID, beadmeta.OutcomeCanceled,
				sourceworkflow.RunCanceledCloseReason, map[string]string{beadmeta.CancelRequestedMetadataKey: "true"}, nil); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := beads.NewMemStore()
			root := mustCreateWorkflowBead(t, store, beads.Bead{
				Title:    "workflow",
				Type:     "task",
				Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
			})
			control := mustCreateWorkflowBead(t, store, beads.Bead{
				Title:    "drain",
				Type:     "task",
				Metadata: map[string]string{"gc.kind": "drain", beadmeta.RootBeadIDMetadataKey: root.ID},
			})
			gateID, err := ensureCrossStoreGate(store, control, "rg-9", true)
			if err != nil || gateID == "" {
				t.Fatalf("mint: %q, %v", gateID, err)
			}
			tc.close(t, store, root.ID)
			g := mustGetBead(t, store, gateID)
			if g.Status != "closed" {
				t.Fatalf("gate %s after the close = %s %v; the path does not reach gates", gateID, g.Status, g.Metadata)
			}
			if !IsControllerClosedCrossStoreGate(g) {
				t.Fatalf("gate closed with close_reason %q reads as an operator close (final)", g.Metadata["close_reason"])
			}
			if got, err := ensureCrossStoreGate(store, mustGetBead(t, store, control.ID), "rg-9", true); err != nil || got != gateID {
				t.Fatalf("drain finding the blocker open = %q, %v; want the gate reopened", got, err)
			}
		})
	}
}

// raceOnGet runs race just before the on-th live read of id, so the write
// that read precedes sees the gate as the race left it.
type raceOnGet struct {
	*beads.MemStore
	id   string
	on   int
	n    int
	race func()
}

func (s *raceOnGet) Get(id string) (beads.Bead, error) {
	if id == s.id {
		s.n++
		if s.n == s.on && s.race != nil {
			s.race()
		}
	}
	return s.MemStore.Get(id)
}

// gateInState mints a gate on a fresh MemStore and leaves it closed (or open)
// with meta, returning the store, the control and the gate as read.
func gateInState(t *testing.T, closed bool, meta map[string]string) (*beads.MemStore, beads.Bead, beads.Bead) {
	t.Helper()
	mem := beads.NewMemStore()
	control := newGateControl(t, mem)
	gateID, err := ensureCrossStoreGate(mem, control, "rg-9", false)
	if err != nil {
		t.Fatal(err)
	}
	if closed {
		if _, err := mem.CloseAll([]string{gateID}, meta); err != nil {
			t.Fatal(err)
		}
	} else if err := mem.SetMetadataBatch(gateID, meta); err != nil {
		t.Fatal(err)
	}
	return mem, control, mustGetBead(t, mem, gateID)
}

func setGateMeta(t *testing.T, mem *beads.MemStore, id string, meta map[string]string) func() {
	return func() {
		if err := mem.SetMetadataBatch(id, meta); err != nil {
			t.Error(err)
		}
	}
}

func reopenGateRaw(t *testing.T, mem *beads.MemStore, id string) func() {
	return func() {
		open := "open"
		if err := mem.Update(id, beads.UpdateOpts{Status: &open, Metadata: map[string]string{beadmeta.CrossStoreGateStateMetadataKey: ""}}); err != nil {
			t.Error(err)
		}
	}
}

// TestGateWritesReCheckTheirPremiseOnTheLiveRow: each drain-side gate write
// re-reads the gate and writes only while its premise still holds; when it no
// longer does, the drain answers from the live row.
func TestGateWritesReCheckTheirPremiseOnTheLiveRow(t *testing.T) {
	const (
		stateKey = beadmeta.CrossStoreGateStateMetadataKey
		fromKey  = beadmeta.CrossStoreGateRetiredFromMetadataKey
	)
	overridden := map[string]string{stateKey: CrossStoreGateStateOverridden}
	for _, tc := range []struct {
		name      string
		gateMeta  map[string]string
		foundOpen bool
		race      func(t *testing.T, mem *beads.MemStore, id string) func()
		wantID    bool
		want      func(g beads.Bead) bool
	}{
		{
			name:     "hand-back of a gate retired from released: another hand-back reopened it",
			gateMeta: map[string]string{stateKey: CrossStoreGateStateRetired, fromKey: CrossStoreGateStateReleased},
			race:     reopenGateRaw,
			wantID:   true,
			want:     func(g beads.Bead) bool { return g.Status == "open" && g.Metadata[stateKey] == "" },
		},
		{
			name:     "hand-back of a gate retired from open: an operator overrode it",
			gateMeta: map[string]string{stateKey: CrossStoreGateStateRetired, fromKey: "open"},
			race: func(t *testing.T, mem *beads.MemStore, id string) func() {
				return setGateMeta(t, mem, id, overridden)
			},
			want: func(g beads.Bead) bool {
				return g.Status == "closed" && g.Metadata[stateKey] == CrossStoreGateStateOverridden
			},
		},
		{
			name:     "hand-back reopen of a gate retired from open: it was retired from released meanwhile",
			gateMeta: map[string]string{stateKey: CrossStoreGateStateRetired, fromKey: "open"},
			race: func(t *testing.T, mem *beads.MemStore, id string) func() {
				return setGateMeta(t, mem, id, map[string]string{fromKey: CrossStoreGateStateReleased})
			},
			want: func(g beads.Bead) bool {
				return g.Status == "closed" && g.Metadata[stateKey] == CrossStoreGateStateRetired && g.Metadata[fromKey] == CrossStoreGateStateReleased
			},
		},
		{
			name:     "hand-back of a gate retired from released: an operator reopened it meanwhile",
			gateMeta: map[string]string{stateKey: CrossStoreGateStateRetired, fromKey: CrossStoreGateStateReleased},
			race: func(t *testing.T, mem *beads.MemStore, id string) func() {
				return func() {
					if err := mem.Reopen(id); err != nil {
						t.Error(err)
					}
				}
			},
			wantID: true,
			want: func(g beads.Bead) bool {
				// Never an "open but released" gate, whose later operator
				// close would read as the controller's.
				return g.Status == "open" && g.Metadata[stateKey] != CrossStoreGateStateReleased
			},
		},
		{
			name:     "hand-back of a gate retired from released: it was retired from open meanwhile",
			gateMeta: map[string]string{stateKey: CrossStoreGateStateRetired, fromKey: CrossStoreGateStateReleased},
			race: func(t *testing.T, mem *beads.MemStore, id string) func() {
				return setGateMeta(t, mem, id, map[string]string{fromKey: "open"})
			},
			want: func(g beads.Bead) bool {
				return g.Status == "closed" && g.Metadata[stateKey] == CrossStoreGateStateRetired && g.Metadata[fromKey] == "open"
			},
		},
		{
			name:      "reopen on a found-open blocker: an operator closed it with a reason",
			gateMeta:  map[string]string{stateKey: CrossStoreGateStateReleased, "close_reason": "blocker rg-9 closed"},
			foundOpen: true,
			race: func(t *testing.T, mem *beads.MemStore, id string) func() {
				return setGateMeta(t, mem, id, map[string]string{stateKey: "", "close_reason": "operator: not needed"})
			},
			want: func(g beads.Bead) bool {
				return g.Status == "closed" && g.Metadata["close_reason"] == "operator: not needed"
			},
		},
		{
			name:      "reopen on a found-open blocker: the release pass reopened it first",
			gateMeta:  map[string]string{stateKey: CrossStoreGateStateReleased, "close_reason": "blocker rg-9 closed"},
			foundOpen: true,
			race:      reopenGateRaw,
			wantID:    true,
			want:      func(g beads.Bead) bool { return g.Status == "open" },
		},
		{
			name:     "recording an override: the gate became controller-closed",
			gateMeta: map[string]string{"close_reason": "operator: not needed"},
			race: func(t *testing.T, mem *beads.MemStore, id string) func() {
				return setGateMeta(t, mem, id, map[string]string{stateKey: CrossStoreGateStateReleased})
			},
			want: func(g beads.Bead) bool { return g.Metadata[stateKey] == CrossStoreGateStateReleased },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem, _, gate := gateInState(t, true, tc.gateMeta)
			store := &raceOnGet{MemStore: mem, id: gate.ID, on: 1, race: tc.race(t, mem, gate.ID)}
			got, err := gateAnswer(store, gate, tc.foundOpen)
			if err != nil {
				t.Fatal(err)
			}
			if (got != "") != tc.wantID {
				t.Fatalf("answer = %q; want holding=%v from the live row", got, tc.wantID)
			}
			if g := mustGetBead(t, mem, gate.ID); !tc.want(g) {
				t.Fatalf("gate = %s %v; the write ignored the race", g.Status, g.Metadata)
			}
		})
	}
}

// TestReleasePassWritesReCheckTheirPremiseAndReportOnlyWhatTheyWrote: the
// pass's retire and reopen of a released gate write only while the gate is
// still released, and report only the gates they wrote.
func TestReleasePassWritesReCheckTheirPremiseAndReportOnlyWhatTheyWrote(t *testing.T) {
	released := map[string]string{beadmeta.CrossStoreGateStateMetadataKey: CrossStoreGateStateReleased, "close_reason": "blocker rg-9 closed"}
	overridden := map[string]string{beadmeta.CrossStoreGateStateMetadataKey: CrossStoreGateStateOverridden}
	for _, tc := range []struct {
		name        string
		closeDrain  bool
		wantNothing func(r CrossStoreGateReport) bool
	}{
		{name: "retire of a finished drain's released gate", closeDrain: true, wantNothing: func(r CrossStoreGateReport) bool { return len(r.Retired) == 0 }},
		{name: "reopen of a live drain's released gate", wantNothing: func(r CrossStoreGateReport) bool { return len(r.Reopened) == 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem, control, gate := gateInState(t, true, released)
			if tc.closeDrain {
				if err := mem.Close(control.ID); err != nil {
					t.Fatal(err)
				}
			}
			rig := beads.NewMemStore()
			rig.HonorExplicitIDs = true
			if _, err := rig.Create(beads.Bead{ID: "rg-9", Title: "open blocker", Type: "task"}); err != nil {
				t.Fatal(err)
			}
			store := &raceOnGet{MemStore: mem, id: gate.ID, on: 1, race: setGateMeta(t, mem, gate.ID, overridden)}
			report, err := ReleaseCrossStoreGates(store, storesResolver(rig))
			if err != nil {
				t.Fatal(err)
			}
			if !tc.wantNothing(report) {
				t.Fatalf("report = %+v; it lists a write that never happened", report)
			}
			if g := mustGetBead(t, mem, gate.ID); g.Status != "closed" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateOverridden {
				t.Fatalf("gate = %s %v; the pass wrote over the operator's override", g.Status, g.Metadata)
			}
		})
	}
}

// casBump moves the gate's revision (an unrelated metadata write) right after
// the first live read, so the revision-checked write that read feeds loses.
type casBump struct {
	*beads.MemStore
	id    string
	fired bool
}

func (s *casBump) Get(id string) (beads.Bead, error) {
	b, err := s.MemStore.Get(id)
	if err == nil && id == s.id && !s.fired {
		s.fired = true
		if serr := s.SetMetadata(id, "note", "touched"); serr != nil {
			return beads.Bead{}, serr
		}
	}
	return b, err
}

// TestReleaseRetriesALostCASWhosePremiseStillHolds: a revision-checked write
// that loses to an unrelated write re-reads the gate and, its premise still
// holding, writes in the same pass.
func TestReleaseRetriesALostCASWhosePremiseStillHolds(t *testing.T) {
	mem := beads.NewMemStoreFrom(1000, nil, nil)
	if err := beads.StampOpenedStore(mem, "MemStore", gate.Require, false, nil, nil); err != nil {
		t.Fatal(err)
	}
	control := newGateControl(t, mem)
	gateID, err := ensureCrossStoreGate(mem, control, "rg-9", true)
	if err != nil {
		t.Fatal(err)
	}
	rig := beads.NewMemStore()
	rig.HonorExplicitIDs = true
	if _, err := rig.Create(beads.Bead{ID: "rg-9", Title: "blocker", Type: "task", Status: "closed"}); err != nil {
		t.Fatal(err)
	}
	if err := rig.Close("rg-9"); err != nil {
		t.Fatal(err)
	}
	store := &casBump{MemStore: mem, id: gateID}
	report, err := ReleaseCrossStoreGates(store, storesResolver(rig))
	if err != nil {
		t.Fatal(err)
	}
	if !store.fired || !slices.Equal(report.Released, []string{gateID}) {
		t.Fatalf("fired=%v report=%+v; want the lost CAS retried and the gate released this pass", store.fired, report)
	}
	if g := mustGetBead(t, mem, gateID); g.Status != "closed" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateReleased {
		t.Fatalf("gate = %s %v", g.Status, g.Metadata)
	}
}

// alwaysLosesCAS refuses every revision-checked write to one bead, as if
// another writer moved its revision each time, so a gate write through it
// loses both of its attempts.
type alwaysLosesCAS struct {
	*beads.MemStore
	id string
}

func (s *alwaysLosesCAS) UpdateIfMatch(id string, expectedRevision int64, opts beads.UpdateOpts) error {
	if id == s.id {
		return &beads.PreconditionFailedError{ID: id, Expected: expectedRevision}
	}
	return s.MemStore.UpdateIfMatch(id, expectedRevision, opts)
}

// requireConditionalWrites stamps mem as a binding with beads.conditional_writes
// = require, so every gate write is a revision CAS.
func requireConditionalWrites(t *testing.T, mem *beads.MemStore) {
	t.Helper()
	if err := beads.StampOpenedStore(mem, "MemStore", gate.Require, false, nil, nil); err != nil {
		t.Fatalf("stamping conditional writes: %v", err)
	}
}

// TestReleaseCrossStoreGatesJudgesTheLiveRowWhenItsClearingWriteLoses: before
// it judges an open gate the pass clears a leftover closed-gate marker (an
// operator reopened a gate that was overridden, released or retired). When
// that conditional write does not land, the pass judges the gate's LIVE row,
// never its own copy with the marker deleted. A gate an operator closed
// between the listing and the write is no longer open: it is neither counted
// nor judged — never marked unresolved over the operator's close — and its
// override stands. A gate whose revision checks were both lost is judged as it
// is, still carrying the marker a later pass clears.
func TestReleaseCrossStoreGatesJudgesTheLiveRowWhenItsClearingWriteLoses(t *testing.T) {
	const (
		stateKey = beadmeta.CrossStoreGateStateMetadataKey
		fromKey  = beadmeta.CrossStoreGateRetiredFromMetadataKey
	)
	t.Run("an operator closed it between the listing and the write", func(t *testing.T) {
		for name, conditional := range map[string]bool{"live re-check": false, "conditional, CAS": true} {
			t.Run(name, func(t *testing.T) {
				mem, _, reopened := gateInState(t, false, map[string]string{stateKey: CrossStoreGateStateOverridden})
				if conditional {
					requireConditionalWrites(t, mem)
				}
				race := &operatorClosesOnList{Store: mem, gateID: reopened.ID}
				report, err := ReleaseCrossStoreGates(race, storesResolver())
				if err != nil {
					t.Fatal(err)
				}
				if !race.done {
					t.Fatal("fixture: the operator's close never happened")
				}
				if report.Open != 0 || len(report.Unresolved) != 0 || len(report.NewlyUnresolved) != 0 {
					t.Fatalf("report = %+v; want the gate the operator closed neither counted nor judged", report)
				}
				if g := mustGetBead(t, mem, reopened.ID); g.Status != "closed" || g.Metadata[stateKey] != CrossStoreGateStateOverridden {
					t.Fatalf("gate = %s %v; want the operator's close standing, still recorded as overridden", g.Status, g.Metadata)
				}
			})
		}
	})
	t.Run("both revision checks were lost", func(t *testing.T) {
		mem, _, reopened := gateInState(t, false, map[string]string{stateKey: CrossStoreGateStateRetired, fromKey: CrossStoreGateStateReleased})
		requireConditionalWrites(t, mem)
		report, err := ReleaseCrossStoreGates(&alwaysLosesCAS{MemStore: mem, id: reopened.ID}, storesResolver())
		if err != nil {
			t.Fatal(err)
		}
		if len(report.NewlyUnresolved) != 1 || report.Open != 1 {
			t.Fatalf("report = %+v; want the open gate judged (its blocker no store holds)", report)
		}
		live := mustGetBead(t, mem, reopened.ID)
		if live.Status == "closed" || live.Metadata[fromKey] != CrossStoreGateStateReleased {
			t.Fatalf("gate = %s %v; want it open, still carrying the leftover marker the lost write never cleared", live.Status, live.Metadata)
		}
		if judged := report.NewlyUnresolved[0]; judged.Metadata[fromKey] != live.Metadata[fromKey] {
			t.Fatalf("the pass judged a gate with metadata %v while the store holds %v; it must judge the live row, not its own copy with the marker deleted", judged.Metadata, live.Metadata)
		}
		// Once a write can land, a later pass clears the marker.
		if _, err := ReleaseCrossStoreGates(mem, storesResolver()); err != nil {
			t.Fatal(err)
		}
		if g := mustGetBead(t, mem, reopened.ID); g.Status == "closed" || g.Metadata[fromKey] != "" {
			t.Fatalf("gate after a pass whose write lands = %s %v; want the leftover marker cleared", g.Status, g.Metadata)
		}
	})
}

// TestAnOpenGateThatClosesUnderItsClearingWriteIsSettledAsClosed: the drain
// clears a leftover marker from an open gate before letting it hold the item.
// When the gate closes between the drain's read and that write, the drain
// settles the gate's live, closed row, never answering "held" from its stale
// open copy: an operator's close is the override it is and holds nothing, and
// a release the pass made meanwhile is reopened on a blocker the drain found
// holding.
func TestAnOpenGateThatClosesUnderItsClearingWriteIsSettledAsClosed(t *testing.T) {
	const stateKey = beadmeta.CrossStoreGateStateMetadataKey
	for _, tc := range []struct {
		name      string
		closing   map[string]string
		foundOpen bool
		wantID    bool
		want      func(g beads.Bead) bool
	}{
		{
			name:    "an operator closed it",
			closing: map[string]string{"close_reason": "operator: not needed"},
			want: func(g beads.Bead) bool {
				return g.Status == "closed" && g.Metadata[stateKey] == CrossStoreGateStateOverridden
			},
		},
		{
			name:      "the release pass released it, and the drain found its blocker holding",
			closing:   map[string]string{stateKey: CrossStoreGateStateReleased, "close_reason": "blocker rg-9 closed"},
			foundOpen: true,
			wantID:    true,
			want:      func(g beads.Bead) bool { return g.Status == "open" && g.Metadata[stateKey] == "" },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mem, _, reopened := gateInState(t, false, map[string]string{stateKey: CrossStoreGateStateOverridden})
			store := &raceOnGet{MemStore: mem, id: reopened.ID, on: 1, race: func() {
				if _, err := mem.CloseAll([]string{reopened.ID}, tc.closing); err != nil {
					t.Error(err)
				}
			}}
			got, err := gateAnswer(store, reopened, tc.foundOpen)
			if err != nil {
				t.Fatal(err)
			}
			if (got != "") != tc.wantID {
				t.Fatalf("answer = %q; want holding=%v from the gate's live row", got, tc.wantID)
			}
			if g := mustGetBead(t, mem, reopened.ID); !tc.want(g) {
				t.Fatalf("gate = %s %v; the drain settled its stale open copy, not the closed live row", g.Status, g.Metadata)
			}
		})
	}
}

// closeByRootWith closes every open bead carrying rootID as its root, and the
// root, with reason — the shape of the cmd/gc closes that select by
// gc.root_bead_id (gc hook --claim's skip, the stale order wisp sweep).
func closeByRootWith(reason string) func(t *testing.T, store beads.Store, rootID string) {
	return func(t *testing.T, store beads.Store, rootID string) {
		t.Helper()
		members, err := store.List(beads.ListQuery{Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: rootID}, TierMode: beads.TierBoth})
		if err != nil {
			t.Fatal(err)
		}
		ids := []string{}
		for _, m := range members {
			if m.Status != "closed" {
				ids = append(ids, m.ID)
			}
		}
		if _, err := store.CloseAll(append(ids, rootID), map[string]string{
			beadmeta.OutcomeMetadataKey: beadmeta.OutcomeSkipped,
			"close_reason":              reason,
		}); err != nil {
			t.Fatal(err)
		}
	}
}

// TestATeardownClosedGateIsJudgedAfreshWhenItsDrainIsRevived: a gate held for
// a blocker no store holds (unresolved) or a store the pass cannot read
// (undecided) is closed by a controller teardown — no state marker, a
// controller close reason — with its control. The release pass lists only
// open and released gates, so it never sees it. The control is reopened and
// the drain does not find the blocker: it reopens the gate for the pass to
// judge, and the item is held tick after tick.
func TestATeardownClosedGateIsJudgedAfreshWhenItsDrainIsRevived(t *testing.T) {
	run := func(t *testing.T, graph beads.Store, resolver CrossStoreBlockerResolver, reason string) {
		t.Helper()
		control := newGateControl(t, graph)
		gateID, err := ensureCrossStoreGate(graph, control, "rg-9", false)
		if err != nil || gateID == "" {
			t.Fatalf("mint: %q, %v", gateID, err)
		}
		if _, err := ReleaseCrossStoreGates(graph, resolver); err != nil {
			t.Fatal(err)
		}
		if _, err := molecule.CloseMembersWithMetadataExcept(graph, []beads.Bead{mustGetBead(t, graph, gateID)}, map[string]string{
			beadmeta.OutcomeMetadataKey: beadmeta.OutcomeCanceled,
			"close_reason":              reason,
		}, nil); err != nil {
			t.Fatal(err)
		}
		if err := graph.Close(control.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := ReleaseCrossStoreGates(graph, resolver); err != nil {
			t.Fatal(err)
		}
		if err := graph.Reopen(control.ID); err != nil {
			t.Fatal(err)
		}
		for tick := 0; tick < 3; tick++ {
			got, err := ensureCrossStoreGate(graph, mustGetBead(t, graph, control.ID), "rg-9", false)
			if err != nil || got != gateID {
				t.Fatalf("tick %d: drain = %q, %v; want gate %s holding the item", tick, got, err, gateID)
			}
			if _, err := ReleaseCrossStoreGates(graph, resolver); err != nil {
				t.Fatal(err)
			}
		}
		if g := mustGetBead(t, graph, gateID); g.Status != "open" {
			t.Fatalf("gate = %s %v; want open", g.Status, g.Metadata)
		}
	}
	resolvers := map[string]CrossStoreBlockerResolver{
		"unresolved": storesResolver(beads.NewMemStore()),
		"undecided":  undecidedResolver,
	}
	for rname, resolver := range resolvers {
		t.Run(rname, func(t *testing.T) {
			// Every engine, with the run cancel's reason.
			for name, graph := range gateStores(t) {
				t.Run(name, func(t *testing.T) { run(t, graph, resolver, sourceworkflow.RunCanceledCloseReason) })
			}
			// Every controller reason, on the SQLite binding.
			for reason := range controllerCloseReasons {
				t.Run(reason, func(t *testing.T) { run(t, sqliteGraphStore(t), resolver, reason) })
			}
		})
	}
}

// TestARevivedCanceledRunHoldsItsItemBehindAnOpenCrossRigBlocker: end to end.
// The blocker is open in a rig the drain cannot read; the run is canceled
// (POST /runs/{id}/cancel closes the gate with the controller's reason); the
// operator revives the root, the control and the item workflow. The item
// must wait for the blocker again.
func TestARevivedCanceledRunHoldsItsItemBehindAnOpenCrossRigBlocker(t *testing.T) {
	work := beads.NewMemStore()
	rig := beads.NewMemStore()
	rig.HonorExplicitIDs = true
	if _, err := rig.Create(beads.Bead{ID: "rg-77", Title: "rig blocker (open)", Type: "task"}); err != nil {
		t.Fatal(err)
	}
	graph, drain, memberIDs, dir := splitSQLiteDrain(t, work)
	mustDepAdd(t, work, memberIDs[0], "rg-77", "blocks")
	opts := ProcessOptions{FormulaSearchPaths: []string{dir}, MemberStores: []beads.Store{work}}
	if _, err := ProcessControl(graph, drain, opts); err != nil && !errors.Is(err, ErrControlPending) {
		t.Fatal(err)
	}
	if _, err := ReleaseCrossStoreGates(graph, storesResolver(work, rig)); err != nil {
		t.Fatal(err)
	}
	if gates := crossStoreGatesIn(t, graph); len(gates) != 1 || drainRowRunnable(t, graph, drain.ID) {
		t.Fatalf("setup: gates = %d, runnable = %v", len(gates), drainRowRunnable(t, graph, drain.ID))
	}
	rootID := drain.Metadata[beadmeta.RootBeadIDMetadataKey]
	manifest := mustDrainManifest(t, mustGetBead(t, graph, drain.ID))
	itemIDs := itemWorkflowBeadIDs(t, graph, manifest.Rows[0].ItemRootID)
	if _, err := sourceworkflow.CloseWorkflowSubtreeAsExcept(graph, rootID, beadmeta.OutcomeCanceled, sourceworkflow.RunCanceledCloseReason,
		map[string]string{beadmeta.CancelRequestedMetadataKey: "true"}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := ReleaseCrossStoreGates(graph, storesResolver(work, rig)); err != nil {
		t.Fatal(err)
	}
	for _, id := range append([]string{rootID, drain.ID, manifest.Rows[0].ItemRootID}, itemIDs...) {
		_ = graph.Reopen(id)
	}
	if err := graph.SetMetadata(rootID, beadmeta.CancelRequestedMetadataKey, ""); err != nil {
		t.Fatal(err)
	}
	for tick := 0; tick < 3; tick++ {
		if _, err := ProcessControl(graph, mustGetBead(t, graph, drain.ID), opts); err != nil && !errors.Is(err, ErrControlPending) {
			t.Fatal(err)
		}
		if _, err := ReleaseCrossStoreGates(graph, storesResolver(work, rig)); err != nil {
			t.Fatal(err)
		}
		if drainRowRunnable(t, graph, drain.ID) {
			t.Fatalf("tick %d: the revived run's item is runnable while its cross-rig blocker rg-77 is open", tick)
		}
	}
}

// TestAFailedCrossStoreGateHandBackWriteIsAnError: a hand-back write that fails is returned
// to the drain, never read as "no gate holds the item".
func TestAFailedCrossStoreGateHandBackWriteIsAnError(t *testing.T) {
	for name, meta := range map[string]map[string]string{
		"back to released":    {beadmeta.CrossStoreGateStateMetadataKey: CrossStoreGateStateRetired, beadmeta.CrossStoreGateRetiredFromMetadataKey: CrossStoreGateStateReleased},
		"reopen for judgment": {beadmeta.CrossStoreGateStateMetadataKey: CrossStoreGateStateRetired, beadmeta.CrossStoreGateRetiredFromMetadataKey: "open"},
	} {
		t.Run(name, func(t *testing.T) {
			mem, _, gate := gateInState(t, true, meta)
			if got, err := gateAnswer(failingUpdates{Store: mem, failID: gate.ID}, gate, false); err == nil {
				t.Fatalf("answer = %q with no error although the hand-back write failed", got)
			}
		})
	}
}

// TestIsControllerClosedCrossStoreGateIsFalseForAnOpenGate: an open gate is
// not closed at all, whatever its markers say.
func TestIsControllerClosedCrossStoreGateIsFalseForAnOpenGate(t *testing.T) {
	g := beads.Bead{ID: "gcg-xsg-1", Status: "open", Metadata: map[string]string{
		beadmeta.CrossStoreGateStateMetadataKey: CrossStoreGateStateReleased,
		"close_reason":                          molecule.SubtreeClosedReason,
	}}
	if IsControllerClosedCrossStoreGate(g) {
		t.Fatal("an open gate reads as controller-closed")
	}
	g.Status = "closed"
	if !IsControllerClosedCrossStoreGate(g) {
		t.Fatal("a released gate does not read as controller-closed")
	}
}

// TestAnOperatorReopenedOverriddenGateIsJudgedAfreshAfterATeardown: an
// operator overrides a gate (closes it; the drain records overridden), then
// reopens it — `bd reopen` keeps the gc.* marker. Whichever sees the open gate
// first, the drain or the release pass, clears the leftover marker in one
// write. A later controller teardown and revival of the drain then reads as
// the controller's close, so the gate holds the item until the pass judges
// it, instead of reading as the old operator override and letting it run.
func TestAnOperatorReopenedOverriddenGateIsJudgedAfreshAfterATeardown(t *testing.T) {
	for _, seenBy := range []string{"drain", "release pass"} {
		t.Run("seen first by the "+seenBy, func(t *testing.T) {
			for name, graph := range gateStores(t) {
				t.Run(name, func(t *testing.T) {
					resolver := storesResolver(beads.NewMemStore())
					control := newGateControl(t, graph)
					gateID, err := ensureCrossStoreGate(graph, control, "rg-9", false)
					if err != nil || gateID == "" {
						t.Fatalf("mint: %q, %v", gateID, err)
					}
					if _, err := graph.CloseAll([]string{gateID}, map[string]string{"close_reason": "operator: ship it"}); err != nil {
						t.Fatal(err)
					}
					for tick, found := range []bool{false, true} {
						if got, err := ensureCrossStoreGate(graph, mustGetBead(t, graph, control.ID), "rg-9", found); err != nil || got != "" {
							t.Fatalf("override tick %d: drain = %q, %v; an overridden gate never holds", tick, got, err)
						}
						if _, err := ReleaseCrossStoreGates(graph, resolver); err != nil {
							t.Fatal(err)
						}
					}
					if g := mustGetBead(t, graph, gateID); g.Status != "closed" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != CrossStoreGateStateOverridden {
						t.Fatalf("gate = %s %v; want closed and overridden", g.Status, g.Metadata)
					}
					if err := graph.Reopen(gateID); err != nil {
						t.Fatal(err)
					}
					if seenBy == "drain" {
						if got, err := ensureCrossStoreGate(graph, mustGetBead(t, graph, control.ID), "rg-9", false); err != nil || got != gateID {
							t.Fatalf("drain on the reopened gate = %q, %v; want it holding", got, err)
						}
					} else if _, err := ReleaseCrossStoreGates(graph, undecidedResolver); err != nil {
						// Undecided: the pass leaves the gate's judgement as
						// it is, so only the clearing writes it.
						t.Fatal(err)
					}
					g := mustGetBead(t, graph, gateID)
					if g.Status != "open" || g.Metadata[beadmeta.CrossStoreGateStateMetadataKey] == CrossStoreGateStateOverridden || g.Metadata["close_reason"] != "" {
						t.Fatalf("reopened gate = %s %v; want the leftover override and its reason cleared", g.Status, g.Metadata)
					}
					if _, err := molecule.CloseMembersWithMetadataExcept(graph, []beads.Bead{g}, map[string]string{
						beadmeta.OutcomeMetadataKey: beadmeta.OutcomeCanceled,
						"close_reason":              sourceworkflow.RunCanceledCloseReason,
					}, nil); err != nil {
						t.Fatal(err)
					}
					if err := graph.Close(control.ID); err != nil {
						t.Fatal(err)
					}
					if _, err := ReleaseCrossStoreGates(graph, resolver); err != nil {
						t.Fatal(err)
					}
					if err := graph.Reopen(control.ID); err != nil {
						t.Fatal(err)
					}
					if got, err := ensureCrossStoreGate(graph, mustGetBead(t, graph, control.ID), "rg-9", false); err != nil || got != gateID {
						t.Fatalf("revived drain = %q, %v; want the torn-down gate reopened for judgment, not read as the old override", got, err)
					}
					g = mustGetBead(t, graph, gateID)
					if g.Status != "open" || g.Metadata[beadmeta.OutcomeMetadataKey] != "" {
						t.Fatalf("gate after revival = %s %v; want open with gc.outcome cleared", g.Status, g.Metadata)
					}
				})
			}
		})
	}
}
