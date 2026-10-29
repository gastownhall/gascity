package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
)

// gateLaneCity is a split city for the gate lane: a real SQLite binding, a
// strict work store, and the rigs given, each a counting strict store.
type gateLaneCity struct {
	cr      *CityRuntime
	binding beads.Store
	work    *countingRouteStore
	rigs    map[string]*countingRouteStore
	rec     *events.Fake
}

func newGateLaneCity(t *testing.T, rigNames ...string) *gateLaneCity {
	t.Helper()
	binding := newSplitEnvClassLeaf(t)
	work := &countingRouteStore{Store: splittest.NewWorkStore(t, "gc")}
	cfg := &config.City{Workspace: config.Workspace{Name: "city", Prefix: "gc"}}
	rigStores := map[string]beads.Store{}
	rigs := map[string]*countingRouteStore{}
	for i, name := range rigNames {
		prefix := "r" + strconv.Itoa(i)
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: name, Path: "rigs/" + name, Prefix: prefix})
		store := &countingRouteStore{Store: splittest.NewWorkStore(t, prefix)}
		rigs[name] = store
		rigStores[name] = store
	}
	rec := events.NewFake()
	cr := &CityRuntime{
		cityName:            "city",
		cityPath:            t.TempDir(),
		cfg:                 cfg,
		standaloneCityStore: work,
		standaloneRigStores: rigStores,
		storageRoutes:       splitEnvRoutes(binding),
		rec:                 rec,
		logPrefix:           "gc",
		stdout:              io.Discard,
		stderr:              io.Discard,
	}
	return &gateLaneCity{cr: cr, binding: binding, work: work, rigs: rigs, rec: rec}
}

// gate mints a drain control and its gate on blocker in the binding, plus an
// item step held behind the gate, and returns the gate and the item.
func (c *gateLaneCity) gate(t *testing.T, blocker string) (gate, item beads.Bead) {
	t.Helper()
	control, err := c.binding.Create(beads.Bead{Title: "drain", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gcg-root"}})
	if err != nil {
		t.Fatal(err)
	}
	gate, err = c.binding.Create(beads.Bead{
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
	item, err = c.binding.Create(beads.Bead{Title: "drain item step", Type: "task", Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: "gcg-item"}})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.binding.DepAdd(item.ID, gate.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	return gate, item
}

func (c *gateLaneCity) ready(t *testing.T, id string) bool {
	t.Helper()
	rows, err := c.binding.Ready(beads.ReadyQuery{TierMode: beads.TierBoth})
	if err != nil {
		t.Fatal(err)
	}
	return slices.ContainsFunc(rows, func(b beads.Bead) bool { return b.ID == id })
}

// TestCrossStoreGateLaneReleasesADrainItemOnceItsWorkBlockerCloses drives the
// off-tick pass over a split city: the item is held while its blocker is open
// in a rig store, and once the blocker closes the pass releases the gate and
// the item becomes ready in the binding's own Ready.
func TestCrossStoreGateLaneReleasesADrainItemOnceItsWorkBlockerCloses(t *testing.T) {
	c := newGateLaneCity(t, "myrig")
	blocker, err := c.rigs["myrig"].Create(beads.Bead{Title: "work blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	gate, item := c.gate(t, blocker.ID)

	c.cr.releaseCrossStoreGates()
	if c.ready(t, item.ID) {
		t.Fatalf("drain item %s is ready while its gate's blocker %s is open", item.ID, blocker.ID)
	}
	if err := c.rigs["myrig"].Close(blocker.ID); err != nil {
		t.Fatal(err)
	}
	report := c.cr.releaseCrossStoreGates()
	if !slices.Equal(report.Released, []string{gate.ID}) {
		t.Fatalf("report = %+v, want gate %s released", report, gate.ID)
	}
	if !c.ready(t, item.ID) {
		t.Fatalf("drain item %s is not ready after its gate was released", item.ID)
	}
}

// TestCrossStoreGateLaneReadsEachStoreOncePerPass bounds the pass's cost: ten
// gates over four rigs cost ONE batched read per work store, not one read per
// gate per store (the review measured 50 serial round trips, ~4.5 min at
// maintainer-city's 5.4 s ledger query).
func TestCrossStoreGateLaneReadsEachStoreOncePerPass(t *testing.T) {
	c := newGateLaneCity(t, "a", "b", "c", "d")
	for i := 0; i < 10; i++ {
		blocker, err := c.rigs["a"].Create(beads.Bead{Title: "blocker", Type: "task"})
		if err != nil {
			t.Fatal(err)
		}
		c.gate(t, blocker.ID)
	}
	c.cr.releaseCrossStoreGates()
	stores := map[string]*countingRouteStore{"city": c.work}
	for name, store := range c.rigs {
		stores[name] = store
	}
	for name, store := range stores {
		if got := store.reads(); got > 1 {
			t.Errorf("store %s was read %d time(s) in one pass, want at most 1 batched read", name, got)
		}
	}
}

// TestCrossStoreGateLaneKeepsAGateWhileAStoreIsUnreadable: a store whose read
// fails might hold an open copy of any blocker, so no gate is released and none
// is marked unresolved until the store can be read. The hold is not silent:
// every pass names the store it could not read, why, and the gates held only
// on it, on stderr.
func TestCrossStoreGateLaneKeepsAGateWhileAStoreIsUnreadable(t *testing.T) {
	c := newGateLaneCity(t, "myrig")
	blocker, err := c.work.Create(beads.Bead{Title: "blocker", Type: "task"})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.work.Close(blocker.ID); err != nil {
		t.Fatal(err)
	}
	gate, _ := c.gate(t, blocker.ID)
	ghost, _ := c.gate(t, "gc-404")
	c.cr.standaloneRigStores["myrig"] = unreadableGateLeg{Store: c.rigs["myrig"]}
	held := []string{gate.ID, ghost.ID}
	slices.Sort(held)

	for pass := 0; pass < 2; pass++ {
		var stderr bytes.Buffer
		c.cr.stderr = &stderr
		report := c.cr.releaseCrossStoreGates()
		if len(report.Released) != 0 || len(report.Unresolved) != 0 {
			t.Fatalf("pass %d: report = %+v; want both gates kept while the rig store is unreadable", pass, report)
		}
		for _, id := range held {
			got, err := c.binding.Get(id)
			if err != nil || got.Status == "closed" || got.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != "" {
				t.Fatalf("pass %d: gate %s = %+v, %v; want it open and unmarked", pass, id, got, err)
			}
		}
		if len(report.Unreadable) != 1 || !strings.Contains(report.Unreadable[0].Error(), "rig myrig") || !strings.Contains(report.Unreadable[0].Error(), "store unavailable") {
			t.Fatalf("pass %d: unreadable = %v; want rig myrig named with its read error", pass, report.Unreadable)
		}
		if !slices.Equal(report.Undecided, held) {
			t.Fatalf("pass %d: undecided = %v, want both gates %v held only on the unreadable store", pass, report.Undecided, held)
		}
		log := stderr.String()
		for _, want := range []string{"rig myrig", "store unavailable", "2 open gate(s)"} {
			if !strings.Contains(log, want) {
				t.Fatalf("pass %d: stderr = %q; want it to name %q: an unreadable store must not freeze gates silently", pass, log, want)
			}
		}
	}
}

type unreadableGateLeg struct{ beads.Store }

func (unreadableGateLeg) List(beads.ListQuery) ([]beads.Bead, error) {
	return nil, errors.New("store unavailable")
}

func (unreadableGateLeg) Get(string) (beads.Bead, error) {
	return beads.Bead{}, errors.New("store unavailable")
}

// TestCrossStoreGateLaneLeavesASuspendedRigsBlockersUndecided: a suspended
// rig's store is not read, so a blocker in its namespace keeps its gate, not
// marked unresolved.
func TestCrossStoreGateLaneLeavesASuspendedRigsBlockersUndecided(t *testing.T) {
	c := newGateLaneCity(t, "myrig")
	c.cr.cfg.Rigs[0].Suspended = true
	gate, _ := c.gate(t, "r0-1")
	report := c.cr.releaseCrossStoreGates()
	if len(report.Released)+len(report.Unresolved) != 0 {
		t.Fatalf("report = %+v; want the suspended rig's blocker undecided", report)
	}
	if c.rigs["myrig"].reads() != 0 {
		t.Fatalf("the suspended rig's store was read %d time(s)", c.rigs["myrig"].reads())
	}
	if got, _ := c.binding.Get(gate.ID); got.Metadata[beadmeta.CrossStoreGateStateMetadataKey] != "" {
		t.Fatalf("gate %s was marked %q", gate.ID, got.Metadata[beadmeta.CrossStoreGateStateMetadataKey])
	}
}

// TestCrossStoreGateLaneReportsAnUnresolvedControlOnce: gates whose blocker
// no store holds are marked unresolved and announced ONCE per drain control —
// one control.stalled for a control with two such gates, aligned with the
// control dispatcher's emitter (actor controller, error_class pending,
// first_seen the persisted anchor, the run id) — and not again next pass.
func TestCrossStoreGateLaneReportsAnUnresolvedControlOnce(t *testing.T) {
	c := newGateLaneCity(t)
	var stderr bytes.Buffer
	c.cr.stderr = &stderr
	gate, _ := c.gate(t, "gc-404")
	control := gate.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]
	second, err := c.binding.Create(beads.Bead{
		Title:  "waits for gc-405",
		Type:   "gate",
		Labels: []string{dispatch.CrossStoreGateLabel},
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:          "gcg-root",
			beadmeta.CrossStoreGateOwnerMetadataKey: control,
			beadmeta.CrossStoreBlockerMetadataKey:   "gc-405",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// The anchor is the persisted time the gates were first found unresolved:
	// pin it in the past, so it cannot be mistaken for the emission time.
	anchorAt := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	restore := dispatch.PinCrossStoreGateClockForTest(func() time.Time { return anchorAt })
	c.cr.releaseCrossStoreGates()
	restore()
	c.cr.releaseCrossStoreGates()
	var stalled []events.Event
	for _, evt := range c.rec.Events {
		if evt.Type == events.ControlStalled {
			stalled = append(stalled, evt)
		}
	}
	if len(stalled) != 1 {
		t.Fatalf("control.stalled events = %d, want exactly 1 for one control across two passes", len(stalled))
	}
	evt := stalled[0]
	if evt.Actor != "controller" || evt.Subject != control || evt.RunID != "gcg-root" || evt.Message == "" {
		t.Fatalf("event = %+v; want actor controller, subject %s, run gcg-root and a message", evt, control)
	}
	var payload events.ControlStalledPayload
	if err := json.Unmarshal(evt.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	anchor := mustBeadMeta(t, c.binding, gate.ID, beadmeta.CrossStoreGateUnresolvedAtMetadataKey)
	if anchor != anchorAt.Format(time.RFC3339) {
		t.Fatalf("persisted anchor = %q, want the pinned %q", anchor, anchorAt.Format(time.RFC3339))
	}
	if payload.BeadID != control || payload.ErrorClass != "pending" || payload.Attempts != 2 || payload.FirstSeen != anchor {
		t.Fatalf("payload = %+v; want control %s, pending, 2 gates, first_seen the persisted anchor %q", payload, control, anchor)
	}
	for _, id := range []string{gate.ID, second.ID} {
		if mustBeadMeta(t, c.binding, id, beadmeta.CrossStoreGateStateMetadataKey) != dispatch.CrossStoreGateStateUnresolved {
			t.Fatalf("gate %s is not marked unresolved", id)
		}
	}
}

// TestCrossStoreGateLaneReportsEveryUnresolvedGateOfAControl: a control that
// already waits on an unresolved gate and finds a second one unresolved is
// announced again, and that control.stalled covers every unresolved gate it
// waits on — attempts the count of them, first_seen the earliest anchor — as
// the per-control contract says, not just the gate that changed this pass.
func TestCrossStoreGateLaneReportsEveryUnresolvedGateOfAControl(t *testing.T) {
	c := newGateLaneCity(t)
	first, _ := c.gate(t, "gc-404")
	control := first.Metadata[beadmeta.CrossStoreGateOwnerMetadataKey]
	anchorAt := time.Date(2026, 10, 1, 8, 0, 0, 0, time.UTC)
	restore := dispatch.PinCrossStoreGateClockForTest(func() time.Time { return anchorAt })
	c.cr.releaseCrossStoreGates()
	restore()
	second, err := c.binding.Create(beads.Bead{
		Title:  "waits for gc-405",
		Type:   "gate",
		Labels: []string{dispatch.CrossStoreGateLabel},
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey:          "gcg-root",
			beadmeta.CrossStoreGateOwnerMetadataKey: control,
			beadmeta.CrossStoreBlockerMetadataKey:   "gc-405",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	c.cr.releaseCrossStoreGates()
	var stalled []events.Event
	for _, evt := range c.rec.Events {
		if evt.Type == events.ControlStalled {
			stalled = append(stalled, evt)
		}
	}
	if len(stalled) != 2 {
		t.Fatalf("control.stalled events = %d, want 2: one per pass that newly found an unresolved gate", len(stalled))
	}
	var payload events.ControlStalledPayload
	if err := json.Unmarshal(stalled[1].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.BeadID != control || payload.Attempts != 2 || payload.FirstSeen != anchorAt.Format(time.RFC3339) {
		t.Fatalf("second payload = %+v; want control %s, 2 gates, first_seen the first gate's anchor %s", payload, control, anchorAt.Format(time.RFC3339))
	}
	for _, id := range []string{first.ID, second.ID} {
		if !strings.Contains(payload.Error, id) {
			t.Fatalf("second event %q does not name gate %s; it must list every unresolved gate the control waits on", payload.Error, id)
		}
	}
}

func mustBeadMeta(t *testing.T, store beads.Store, id, key string) string {
	t.Helper()
	b, err := store.Get(id)
	if err != nil {
		t.Fatal(err)
	}
	return b.Metadata[key]
}

// TestCrossStoreGateLaneReadsNothingWhileTheCityIsQuiescent: a quiescent
// city's stores are not touched, gates included — whether the runtime serves a
// controller state or runs without one, on the runtime's own flag as every
// sibling off-tick lane reads it.
func TestCrossStoreGateLaneReadsNothingWhileTheCityIsQuiescent(t *testing.T) {
	for _, tc := range []struct {
		name    string
		quiesce func(c *gateLaneCity)
	}{
		{name: "with a controller state", quiesce: func(c *gateLaneCity) {
			quiescent := new(atomic.Bool)
			quiescent.Store(true)
			c.cr.cs = &controllerState{beadsQuiescent: quiescent, cityBeadStore: c.work, storageRoutes: c.cr.storageRoutes, beadStores: c.cr.standaloneRigStores}
		}},
		{name: "without a controller state", quiesce: func(c *gateLaneCity) { c.cr.beadsQuiescent.Store(true) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := newGateLaneCity(t, "myrig")
			blocker, err := c.work.Create(beads.Bead{Title: "blocker", Type: "task"})
			if err != nil {
				t.Fatal(err)
			}
			c.gate(t, blocker.ID)
			tc.quiesce(c)
			c.work.lists, c.work.gets, c.work.batches = 0, 0, 0
			report := c.cr.releaseCrossStoreGates()
			if report.Open != 0 || c.work.reads() != 0 || c.rigs["myrig"].reads() != 0 {
				t.Fatalf("quiescent pass: report %+v, work reads %d, rig reads %d; want nothing read", report, c.work.reads(), c.rigs["myrig"].reads())
			}
		})
	}
}

// TestCrossStoreGateLaneReadsNothingOnASingleStoreCity is the compatibility
// control: a city that relocates nothing never mints a gate, so the pass must
// not read a store at all.
func TestCrossStoreGateLaneReadsNothingOnASingleStoreCity(t *testing.T) {
	work := &countingRouteStore{Store: beads.NewMemStore()}
	cr := &CityRuntime{cityName: "city", standaloneCityStore: work, logPrefix: "gc", stdout: io.Discard, stderr: io.Discard}
	cr.releaseCrossStoreGates()
	if work.reads() != 0 {
		t.Fatalf("the gate pass issued %d read(s) on a single-store city, want 0", work.reads())
	}
}

// TestCrossStoreBlockerResolverMatchesASuspendedRigsNamespaceWhole: a
// suspended rig whose prefix is hyphenated ("gc-ops") makes its own ids
// undecided and leaves the city's "gc-" ids alone — both directions.
func TestCrossStoreBlockerResolverMatchesASuspendedRigsNamespaceWhole(t *testing.T) {
	cfg := &config.City{Rigs: []config.Rig{{Name: "ops", Path: "rigs/ops", Prefix: "gc-ops"}}}
	resolve := crossStoreBlockerResolver(beads.NewMemStore(), beads.NewMemStore(), cfg,
		map[string]beads.Store{"ops": beads.NewMemStore()}, map[string]bool{"rigs/ops": true})
	resolved, err := resolve([]string{"gc-ops-12", "gc-12"})
	if err != nil {
		t.Fatal(err)
	}
	if !resolved.Undecided["gc-ops-12"] {
		t.Error("gc-ops-12 is in suspended rig ops (prefix gc-ops) but reads as decided")
	}
	if resolved.Undecided["gc-12"] {
		t.Error("gc-12 is not in rig ops's namespace but reads as undecided")
	}
}
