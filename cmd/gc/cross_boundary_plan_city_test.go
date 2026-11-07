package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
)

// The city copy's cross-boundary rules (S5 §8.1, D9), each row on a real
// SQLite work store: an edge whose two ends land in different stores after
// the split, in both directions, is gated, refused by id, or dropped and
// recorded, and never written as a cross-store edge.
//
// Red on main: the copy dropped every edge onto work without a gate, a
// record or a refusal (`if !infraIDs[d.DependsOnID] { continue }`), and the
// clear re-added each edge from work onto a moved row.

// d9SQLiteSource is a real SQLite work store under the bead-policy wrapper
// every production open returns, holding rows with fixed ids.
func d9SQLiteSource(t *testing.T, rows []beads.Bead, edges []beads.Dep) beads.Store {
	t.Helper()
	opened, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeBeadStoreHandle(opened) })
	creator, ok := beads.As[beads.ForeignIDCreator](opened)
	if !ok {
		t.Fatal("the SQLite store creates no foreign ids")
	}
	for _, b := range rows {
		if _, err := creator.CreateWithForeignID(b); err != nil {
			t.Fatalf("creating %s: %v", b.ID, err)
		}
	}
	for _, e := range edges {
		if err := opened.DepAdd(e.IssueID, e.DependsOnID, e.Type); err != nil {
			t.Fatalf("adding %v: %v", e, err)
		}
	}
	return borrowedStoreWithBeadPolicies(opened, &config.City{})
}

var d9Created = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)

func d9Row(id string, infra bool, status string) beads.Bead {
	b := beads.Bead{ID: id, Title: id, Type: "task", Status: "open", CreatedAt: d9Created, Metadata: beads.StringMap{}}
	if infra {
		b.Metadata["gc.kind"] = "workflow"
	}
	if status == "closed" {
		b.Status = "closed"
	}
	return b
}

// d9Rig stubs the probe of the city's other stores with one rig store, a
// real SQLite store holding rows, or one that cannot be read.
func d9Rig(t *testing.T, rows []beads.Bead, unreadable bool) {
	t.Helper()
	var store beads.Store
	if !unreadable {
		store = d9SQLiteSource(t, rows, nil)
	}
	prevOpen, prevProbe := rigImportStoreOpener, infraCityBoundaryProbe
	t.Cleanup(func() { rigImportStoreOpener, infraCityBoundaryProbe = prevOpen, prevProbe })
	rigImportStoreOpener = func(string, config.Rig) (beads.Store, error) {
		if unreadable {
			return nil, errors.New("rig ga: dial tcp: connection refused")
		}
		return store, nil
	}
	infraCityBoundaryProbe = func(cityPath string, _ *config.City, ids []string, fromBackup bool) (map[string][]beads.Bead, []string) {
		cfg := &config.City{Rigs: []config.Rig{{Name: "ga", Path: "rigs/ga"}}}
		return probeElsewhere(rigImportEnv{CityPath: cityPath, Cfg: cfg}, "", resolveStoreScopeRoot(cityPath, cityPath), fromBackup, ids)
	}
}

func d9Records(t *testing.T, b beads.Bead) []rigCrossEdgeRecord {
	t.Helper()
	raw := b.Metadata[beadmeta.MigratedCrossEdgesMetadataKey]
	if raw == "" {
		return nil
	}
	var recs []rigCrossEdgeRecord
	if err := json.Unmarshal([]byte(raw), &recs); err != nil {
		t.Fatalf("%s's record does not parse: %v", b.ID, err)
	}
	return recs
}

func TestTheCityCopyPlansEveryBoundaryEdge(t *testing.T) {
	const d, w, x = "mc-d1", "mc-w1", "ga-x1"
	for _, tc := range []struct {
		name string
		// dir is "out" (moved D depends on X) or "in" (work W depends on D).
		dir, kind  string
		dStatus    string
		far        string // "work", "rig", "rig-unreadable", "absent", "external"
		farStatus  string
		refuse     bool
		refusedIDs []string
		action     string // the record's action on D when not refused
		gate       bool
	}{
		{name: "D blocks on open work: gated", dir: "out", kind: "blocks", far: "work", gate: true, action: rigEdgeGated},
		{name: "D blocks on finished work: dropped", dir: "out", kind: "blocks", far: "work", farStatus: "closed", action: rigEdgeDropped},
		{name: "a closed D blocks on open work: dropped", dir: "out", kind: "blocks", dStatus: "closed", far: "work", action: rigEdgeDropped},
		{name: "D waits-for open work: refused", dir: "out", kind: "waits-for", far: "work", refuse: true, refusedIDs: []string{d, w}},
		{name: "D conditional-blocks on open work: refused", dir: "out", kind: "conditional-blocks", far: "work", refuse: true, refusedIDs: []string{d, w}},
		{name: "D tracks open work: refused", dir: "out", kind: "tracks", far: "work", refuse: true, refusedIDs: []string{d, w}},
		{name: "D parent-child of open work: refused", dir: "out", kind: "parent-child", far: "work", refuse: true, refusedIDs: []string{d, w}},
		{name: "D tracks closed work: dropped", dir: "out", kind: "tracks", far: "work", farStatus: "closed", action: rigEdgeDropped},
		{name: "D related to work: dropped", dir: "out", kind: "related", far: "work", action: rigEdgeDropped},
		{name: "D blocks on an external reference: copied", dir: "out", kind: "blocks", far: "external", action: rigEdgeExternal},
		{name: "D blocks on open rig work: gated", dir: "out", kind: "blocks", far: "rig", gate: true, action: rigEdgeGated},
		{name: "D blocks on finished rig work: dropped", dir: "out", kind: "blocks", far: "rig", farStatus: "closed", action: rigEdgeDropped},
		{name: "D waits-for open rig work: refused", dir: "out", kind: "waits-for", far: "rig", refuse: true, refusedIDs: []string{d, x}},
		{name: "D tracks open rig work: refused", dir: "out", kind: "tracks", far: "rig", refuse: true, refusedIDs: []string{d, x}},
		{name: "D blocks on a bead no store holds: dropped", dir: "out", kind: "blocks", far: "absent", action: rigEdgeDropped},
		{name: "D blocks on a bead an unreadable rig may hold: refused", dir: "out", kind: "blocks", far: "rig-unreadable", refuse: true, refusedIDs: []string{d, x}},
		{name: "open work blocked by open D: refused", dir: "in", kind: "blocks", far: "work", refuse: true, refusedIDs: []string{w, d}},
		{name: "open work waits-for open D: refused", dir: "in", kind: "waits-for", far: "work", refuse: true, refusedIDs: []string{w, d}},
		{name: "open work blocked by closed D: dropped", dir: "in", kind: "blocks", dStatus: "closed", far: "work", action: rigEdgeDropped},
		{name: "closed work blocked by open D: dropped", dir: "in", kind: "blocks", far: "work", farStatus: "closed", action: rigEdgeDropped},
		{name: "open work tracks open D: refused", dir: "in", kind: "tracks", far: "work", refuse: true, refusedIDs: []string{w, d}},
		{name: "open work parent-child of open D: refused", dir: "in", kind: "parent-child", far: "work", refuse: true, refusedIDs: []string{w, d}},
		{name: "work related to D: dropped", dir: "in", kind: "related", far: "work", action: rigEdgeDropped},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubInfraControllerPing(t, 0)
			rows := []beads.Bead{d9Row(d, true, tc.dStatus), d9Row("mc-s1", true, ""), d9Row("mc-w9", false, "")}
			edges := []beads.Dep{{IssueID: "mc-s1", DependsOnID: d, Type: "blocks"}}
			other := map[string]string{"work": w, "rig": x, "rig-unreadable": x, "absent": "ga-gone", "external": "external:ga-up"}[tc.far]
			if tc.far == "work" {
				rows = append(rows, d9Row(w, false, tc.farStatus))
			}
			d9Rig(t, []beads.Bead{d9Row(x, false, tc.farStatus)}, tc.far == "rig-unreadable")
			if tc.dir == "out" {
				edges = append(edges, beads.Dep{IssueID: d, DependsOnID: other, Type: tc.kind})
			} else {
				edges = append(edges, beads.Dep{IssueID: other, DependsOnID: d, Type: tc.kind})
			}
			source := d9SQLiteSource(t, rows, edges)
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
			t.Cleanup(func() { openInfraMigrationSource = prev })
			cityPath := t.TempDir()
			cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
			var log bytes.Buffer
			report := migrateInfraClasses(t, cityPath, cfg, &log)
			target := mustResolveInfraTarget(t, cityPath, cfg)

			if tc.refuse {
				var refusal *infraBoundaryRefusal
				if report.Outcome != infraMigrationUnconverged || !errors.As(report.Fault, &refusal) {
					t.Fatalf("outcome = %s (%v), want the copy refused: %s", report.Outcome, report.Fault, log.String())
				}
				if len(refusal.Refusals) != 1 || !slices.Equal(refusal.Refusals[0].IDs, tc.refusedIDs) || refusal.Refusals[0].Remedy == "" {
					t.Fatalf("refusals = %+v, want one naming %v with its fix", refusal.Refusals, tc.refusedIDs)
				}
				if count, known := infraBindingContents(t, target); !known || count != 0 {
					t.Fatalf("a refused copy left %d row(s) in the binding (known %v)", count, known)
				}
				if _, err := source.Get(d); err != nil {
					t.Fatalf("a refused copy removed %s: %v", d, err)
				}
				return
			}
			if report.Outcome != infraMigrationConverged {
				t.Fatalf("outcome = %s: %s", report.Outcome, log.String())
			}
			binding := openMigratedDestination(t, target)
			moved, err := binding.Get(d)
			if err != nil {
				t.Fatal(err)
			}
			var rec *rigCrossEdgeRecord
			for _, r := range d9Records(t, moved) {
				if r.Dir == tc.dir && r.Other == other {
					r := r
					rec = &r
				}
			}
			if rec == nil || rec.Action != tc.action || rec.Type != tc.kind {
				t.Fatalf("%s records %+v for the edge with %s, want dir %s action %s", d, d9Records(t, moved), other, tc.dir, tc.action)
			}
			gates, err := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
			if err != nil {
				t.Fatal(err)
			}
			deps, err := binding.DepList(d, "down")
			if err != nil {
				t.Fatal(err)
			}
			var held []string
			for _, dep := range deps {
				held = append(held, dep.DependsOnID+" ("+dep.Type+")")
			}
			switch {
			case tc.gate:
				if len(gates) != 1 || gates[0].Metadata[beadmeta.CrossStoreGateOwnerMetadataKey] != d || gates[0].Metadata[beadmeta.CrossStoreBlockerMetadataKey] != other || gates[0].Metadata[infraMigrationStampKey] != config.StorageWorkBinding {
					t.Fatalf("gates = %+v, want one owned by %s waiting on %s, stamped as the city copy's", gates, d, other)
				}
				if !slices.Equal(held, []string{gates[0].ID + " (blocks)"}) {
					t.Fatalf("%s's binding edges = %v, want only its gate", d, held)
				}
				if line := d + " waits on open work " + other + " through cross-store gate " + gates[0].ID; !strings.Contains(log.String(), line) {
					t.Fatalf("the migrate output does not say %q: %s", line, log.String())
				}
				manifest, _, err := readInfraCopyManifestRecord(target)
				if err != nil || !manifest.Gates[gates[0].ID] || manifest.IDs[gates[0].ID] {
					t.Fatalf("the manifest records gates %v and ids %d (%v), want the gate as a gate only", manifest.Gates, len(manifest.IDs), err)
				}
			case tc.action == rigEdgeExternal:
				if len(gates) != 0 || !slices.Equal(held, []string{other + " (" + tc.kind + ")"}) {
					t.Fatalf("gates %d, %s's binding edges %v, want the external reference copied as it is", len(gates), d, held)
				}
			default:
				if len(gates) != 0 || len(held) != 0 {
					t.Fatalf("gates %d, %s's binding edges %v, want neither", len(gates), d, held)
				}
			}
			// No edge crosses: the work store holds none onto D, and the
			// binding none onto work.
			if tc.far == "work" {
				if wdeps, err := source.DepList(w, "down"); err != nil || len(wdeps) != 0 {
					t.Fatalf("%s's work-store edges after the clear = %+v (%v), want none", w, wdeps, err)
				}
			}
			if strings.Contains(fmt.Sprint(held), w) {
				t.Fatalf("the binding holds an edge onto work %s: %v", w, held)
			}
		})
	}
}

// TestTheCityCopyResumesWithoutMintingAGateTwice: a copy killed after it
// minted its gate (at the gate batch boundary, or after the verify) keeps the
// gate and its owner (a gate's id is a function of its owner and blocker),
// and the resumed run mints none again.
func TestTheCityCopyResumesWithoutMintingAGateTwice(t *testing.T) {
	for _, point := range []string{"minting gates 1/1", "verify"} {
		t.Run(point, func(t *testing.T) {
			stubInfraControllerPing(t, 0)
			d9Rig(t, nil, false)
			source := d9SQLiteSource(t,
				[]beads.Bead{d9Row("mc-d1", true, ""), d9Row("mc-w1", false, "")},
				[]beads.Dep{{IssueID: "mc-d1", DependsOnID: "mc-w1", Type: "blocks"}})
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
			t.Cleanup(func() { openInfraMigrationSource = prev })
			cityPath := t.TempDir()
			cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
			prevPoint := infraEngineCrashPoint
			infraEngineCrashPoint = func(p string) error {
				if p == point {
					return errors.New("killed at " + point)
				}
				return nil
			}
			var log bytes.Buffer
			first := migrateInfraClasses(t, cityPath, cfg, &log)
			infraEngineCrashPoint = prevPoint
			if first.Outcome == infraMigrationConverged {
				t.Fatalf("the kill at %s did not stop the run: %s", point, log.String())
			}
			log.Reset()
			if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
				t.Fatalf("resume = %s: %s", got.Outcome, log.String())
			}
			if !strings.Contains(log.String(), "kept 1 of 1 stamped row(s)") {
				t.Fatalf("the resume did not keep the row and its gate: %s", log.String())
			}
			binding := openMigratedDestination(t, mustResolveInfraTarget(t, cityPath, cfg))
			gates, err := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
			if err != nil || len(gates) != 1 {
				t.Fatalf("gates after the resume = %+v (%v), want exactly one", gates, err)
			}
			deps, _ := binding.DepList("mc-d1", "down")
			if len(deps) != 1 || deps[0].DependsOnID != gates[0].ID {
				t.Fatalf("mc-d1's binding edges = %+v, want its gate", deps)
			}
		})
	}
}

// d9GatedCity is a city converged with its work-store copies retained,
// whose moved mc-d1 blocks on open work mc-w1 through one gate.
func d9GatedCity(t *testing.T) (beads.Store, infraBindingTarget) {
	t.Helper()
	stubInfraControllerPing(t, 0)
	d9Rig(t, nil, false)
	source := d9SQLiteSource(t,
		[]beads.Bead{d9Row("mc-d1", true, ""), d9Row("mc-w1", false, ""), d9Row("mc-w2", false, "")},
		[]beads.Dep{{IssueID: "mc-d1", DependsOnID: "mc-w1", Type: "blocks"}})
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	var log bytes.Buffer
	if got := migrateInfraClassesRetainingSource(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("cutover = %s: %s", got.Outcome, log.String())
	}
	return source, mustResolveInfraTarget(t, cityPath, cfg)
}

// TestTheEqualityStageHoldsTheBindingToThePlannedGates: the binding must hold
// exactly the gates the plan minted. One missing (the owner would run ahead of
// its blocker) or one the plan never minted is refused.
func TestTheEqualityStageHoldsTheBindingToThePlannedGates(t *testing.T) {
	source, target := d9GatedCity(t)
	plan := plannedInfraCopy(t, source)
	reopen := func() (beads.Store, error) { return openInfraDestination(target) }
	if _, gates, err := verifyInfraCopy(reopen, source, plan); err != nil || len(gates) != 1 {
		t.Fatalf("the faithful copy = gates %v, %v", gates, err)
	}

	binding := openMigratedDestination(t, target)
	extra := rigImportGate{Owner: plan.boundary.Gates[0].Owner, Blocker: "mc-w2"}
	minted, err := createRigImportGate(binding, config.StorageWorkBinding, "", extra)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyInfraCopy(reopen, source, plan); err == nil || !strings.Contains(err.Error(), "binding holds "+minted.ID+", which the work store's infrastructure slice does not") {
		t.Fatalf("a gate the plan never minted = %v, want it named", err)
	}
	if err := binding.Delete(minted.ID); err != nil {
		t.Fatal(err)
	}
	gates, err := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil || len(gates) != 1 {
		t.Fatalf("gates = %+v (%v)", gates, err)
	}
	if err := binding.Close(gates[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyInfraCopy(reopen, source, plan); err == nil || !strings.Contains(err.Error(), "is closed in the binding, want open") {
		t.Fatalf("a closed gate = %v, want it refused: it no longer holds its owner", err)
	}
	if err := binding.Delete(gates[0].ID); err != nil {
		t.Fatal(err)
	}
	if _, _, err := verifyInfraCopy(reopen, source, plan); err == nil || !strings.Contains(err.Error(), "missing the cross-store gate(s) the copy planned: mc-d1 on mc-w1") {
		t.Fatalf("a binding missing its gate = %v, want the gate named", err)
	}
}

// TestTheCityCopyDropsAGateItsPlanNoLongerWants: a copy killed after it
// minted a gate, whose blocker then finished, resumes with the gate gone: the
// plan no longer wants it, so prepare deletes it rather than keep it.
func TestTheCityCopyDropsAGateItsPlanNoLongerWants(t *testing.T) {
	stubInfraControllerPing(t, 0)
	d9Rig(t, nil, false)
	source := d9SQLiteSource(t,
		[]beads.Bead{d9Row("mc-d1", true, ""), d9Row("mc-w1", false, "")},
		[]beads.Dep{{IssueID: "mc-d1", DependsOnID: "mc-w1", Type: "blocks"}})
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	prevPoint := infraEngineCrashPoint
	t.Cleanup(func() { infraEngineCrashPoint = prevPoint })
	infraEngineCrashPoint = func(p string) error {
		if p == "verify" {
			return errors.New("killed after the gate")
		}
		return nil
	}
	var log bytes.Buffer
	if first := migrateInfraClasses(t, cityPath, cfg, &log); first.Outcome == infraMigrationConverged {
		t.Fatalf("the kill did not stop the run: %s", log.String())
	}
	infraEngineCrashPoint = prevPoint
	if err := source.Close("mc-w1"); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("resume = %s: %s", got.Outcome, log.String())
	}
	binding := openMigratedDestination(t, mustResolveInfraTarget(t, cityPath, cfg))
	gates, err := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil || len(gates) != 0 {
		t.Fatalf("gates after the blocker finished = %+v (%v), want none", gates, err)
	}
	if deps, _ := binding.DepList("mc-d1", "down"); len(deps) != 0 {
		t.Fatalf("mc-d1's binding edges = %+v, want none", deps)
	}
}

// TestTheCityCopyRemintsAGateClosedBetweenRuns: a copy killed after it minted
// a gate, whose gate is then closed in the binding while its blocker is
// still open, resumes: prepare drops the closed gate rather than keep it, and
// the import mints it again, open, under the same id. Kept as it was, the
// equality stage would refuse it on every run.
func TestTheCityCopyRemintsAGateClosedBetweenRuns(t *testing.T) {
	stubInfraControllerPing(t, 0)
	d9Rig(t, nil, false)
	source := d9SQLiteSource(t,
		[]beads.Bead{d9Row("mc-d1", true, ""), d9Row("mc-w1", false, "")},
		[]beads.Dep{{IssueID: "mc-d1", DependsOnID: "mc-w1", Type: "blocks"}})
	prev := openInfraMigrationSource
	openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
	t.Cleanup(func() { openInfraMigrationSource = prev })
	cityPath := t.TempDir()
	cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
	target := mustResolveInfraTarget(t, cityPath, cfg)
	prevPoint := infraEngineCrashPoint
	t.Cleanup(func() { infraEngineCrashPoint = prevPoint })
	infraEngineCrashPoint = func(p string) error {
		if p == "verify" {
			return errors.New("killed after the gate")
		}
		return nil
	}
	var log bytes.Buffer
	if first := migrateInfraClasses(t, cityPath, cfg, &log); first.Outcome == infraMigrationConverged {
		t.Fatalf("the kill did not stop the run: %s", log.String())
	}
	infraEngineCrashPoint = prevPoint
	binding := openMigratedDestination(t, target)
	gates, err := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil || len(gates) != 1 {
		t.Fatalf("gates after the kill = %+v (%v)", gates, err)
	}
	gateID := gates[0].ID
	if err := binding.Close(gateID); err != nil {
		t.Fatal(err)
	}
	_ = closeBeadStoreHandle(binding)

	log.Reset()
	if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
		t.Fatalf("resume = %s: %s", got.Outcome, log.String())
	}
	reopened := openMigratedDestination(t, target)
	gate, err := reopened.Get(gateID)
	if err != nil || gate.Status != "open" {
		t.Fatalf("gate %s after the resume = %+v (%v), want it minted again, open", gateID, gate, err)
	}
	if deps, _ := reopened.DepList("mc-d1", "down"); len(deps) != 1 || deps[0].DependsOnID != gateID {
		t.Fatalf("mc-d1's binding edges = %+v, want its gate", deps)
	}
}

// TestTheCityCopyRewritesAGateEdgeChangedBetweenRuns: a copy killed after it
// minted a gate, whose owner -> gate edge is then changed in the binding
// while the gate stays open, resumes: prepare keeps the gate and its owner,
// and the import writes the edge back as planned, blocks with no payload.
// Kept as it was, the equality stage would refuse it on every run.
func TestTheCityCopyRewritesAGateEdgeChangedBetweenRuns(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(binding beads.Store, gateID string) error
	}{
		{name: "to a nonblocking type", change: func(binding beads.Store, gateID string) error {
			return binding.DepAdd("mc-d1", gateID, "related")
		}},
		{name: "to carry a payload", change: func(binding beads.Store, gateID string) error {
			writer, ok := beads.As[beads.DepMetadataWriter](binding)
			if !ok {
				return errors.New("the binding writes no edge payload")
			}
			return writer.DepAddWithMetadata("mc-d1", gateID, "blocks", `{"gate":"any-children"}`)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stubInfraControllerPing(t, 0)
			d9Rig(t, nil, false)
			source := d9SQLiteSource(t,
				[]beads.Bead{d9Row("mc-d1", true, ""), d9Row("mc-w1", false, "")},
				[]beads.Dep{{IssueID: "mc-d1", DependsOnID: "mc-w1", Type: "blocks"}})
			prev := openInfraMigrationSource
			openInfraMigrationSource = func(string) (beads.Store, error) { return source, nil }
			t.Cleanup(func() { openInfraMigrationSource = prev })
			cityPath := t.TempDir()
			cfg := infraSplitConfig(filepath.Join(cityPath, ".gc", "store"))
			target := mustResolveInfraTarget(t, cityPath, cfg)
			prevPoint := infraEngineCrashPoint
			t.Cleanup(func() { infraEngineCrashPoint = prevPoint })
			infraEngineCrashPoint = func(p string) error {
				if p == "verify" {
					return errors.New("killed after the gate")
				}
				return nil
			}
			var log bytes.Buffer
			if first := migrateInfraClasses(t, cityPath, cfg, &log); first.Outcome == infraMigrationConverged {
				t.Fatalf("the kill did not stop the run: %s", log.String())
			}
			infraEngineCrashPoint = prevPoint
			binding := openMigratedDestination(t, target)
			gates, err := binding.List(beads.ListQuery{Label: dispatch.CrossStoreGateLabel, IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
			if err != nil || len(gates) != 1 {
				t.Fatalf("gates after the kill = %+v (%v)", gates, err)
			}
			gateID := gates[0].ID
			if err := tc.change(binding, gateID); err != nil {
				t.Fatal(err)
			}
			_ = closeBeadStoreHandle(binding)

			log.Reset()
			if got := migrateInfraClasses(t, cityPath, cfg, &log); got.Outcome != infraMigrationConverged {
				t.Fatalf("resume = %s: %s", got.Outcome, log.String())
			}
			if !strings.Contains(log.String(), "kept 1 of 1 stamped row(s)") {
				t.Fatalf("the resume did not keep the row and its gate: %s", log.String())
			}
			reopened := openMigratedDestination(t, target)
			if deps, _ := reopened.DepList("mc-d1", "down"); len(deps) != 1 || deps[0].DependsOnID != gateID || deps[0].Type != "blocks" {
				t.Fatalf("mc-d1's binding edges = %+v, want blocks onto its gate %s", deps, gateID)
			}
			if payload, err := infraReadEdgePayload(reopened, "mc-d1", gateID); err != nil || payload.Carried {
				t.Fatalf("mc-d1's edge onto its gate carries %+v (%v), want no payload", payload, err)
			}
		})
	}
}

// TestEveryHoldIsNamedInOneRefusal: a city with an edge hold the plan refuses
// and a status hold the binding cannot record is refused once, naming both,
// by migrate and by preflight alike, so the operator fixes them in one pass.
func TestEveryHoldIsNamedInOneRefusal(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 0)
	source, err := openInfraMigrationSource(request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	waiting := mustCreateInfraBead(t, source, beads.Bead{Title: "waiting wisp", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	work := mustCreateInfraBead(t, source, beads.Bead{Title: "open work", Type: "task"})
	if err := source.DepAdd(waiting.ID, work.ID, "waits-for"); err != nil {
		t.Fatal(err)
	}
	parked := mustCreateInfraBead(t, source, beads.Bead{Title: "parked wisp", Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}, SourceStatus: "blocked"})

	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code == 0 {
		t.Fatalf("preflight cleared a city with two holds: %s", stdout.String())
	}
	var log bytes.Buffer
	report := migrateInfraClasses(t, request.CityPath, request.Cfg, &log)
	if report.Outcome == infraMigrationConverged {
		t.Fatalf("migrate converged over two holds: %s", log.String())
	}
	for name, out := range map[string]string{"preflight": stdout.String(), "migrate": log.String()} {
		for _, id := range []string{waiting.ID, parked.ID} {
			if !strings.Contains(out, id) {
				t.Errorf("%s's refusal does not name %s: %s", name, id, out)
			}
		}
	}
	var boundary *infraBoundaryRefusal
	var status *infraUncarriedHolds
	if !errors.As(report.Fault, &boundary) || !errors.As(report.Fault, &status) {
		t.Fatalf("the migrate refusal %v does not carry both holds", report.Fault)
	}
}

// TestPreflightNamesAMovedRowWaitingOnAnUnheldExternalBead: an external:<id>
// reference is carried as it is and enforced on the split city, so preflight
// names an open moved row whose external blocker no store holds. A held
// target, a capability reference, a closed row and a non-blocking edge are
// not named; nothing blocks.
func TestPreflightNamesAMovedRowWaitingOnAnUnheldExternalBead(t *testing.T) {
	request, _, _ := preflightReadyCity(t, 0)
	d9Rig(t, []beads.Bead{d9Row("ga-held", false, "")}, false)
	source, err := openInfraMigrationSource(request.CityPath)
	if err != nil {
		t.Fatal(err)
	}
	wisp := func(title string) beads.Bead {
		return mustCreateInfraBead(t, source, beads.Bead{Title: title, Type: "task", Metadata: beads.StringMap{"gc.kind": "workflow"}})
	}
	stranded, onRig, onCapability, related, closed := wisp("stranded"), wisp("on rig"), wisp("on capability"), wisp("related"), wisp("closed")
	for _, e := range []beads.Dep{
		{IssueID: stranded.ID, DependsOnID: "external:ga-gone", Type: "blocks"},
		{IssueID: onRig.ID, DependsOnID: "external:ga-held", Type: "blocks"},
		{IssueID: onCapability.ID, DependsOnID: "external:ga:deploy", Type: "blocks"},
		{IssueID: related.ID, DependsOnID: "external:ga-gone", Type: "related"},
		{IssueID: closed.ID, DependsOnID: "external:ga-gone", Type: "blocks"},
	} {
		if err := source.DepAdd(e.IssueID, e.DependsOnID, e.Type); err != nil {
			t.Fatal(err)
		}
	}
	if err := source.Close(closed.ID); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	if code := doStoragePreflight(request, &stdout, &stderr); code != 0 {
		t.Fatalf("preflight blocked on an external reference: %s", stdout.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "1 open bead(s) the copy moves wait on an external: reference whose bead no store holds") || !strings.Contains(out, stranded.ID+" -[blocks]-> external:ga-gone") {
		t.Fatalf("preflight does not name %s: %s", stranded.ID, out)
	}
	for _, id := range []string{onRig.ID, onCapability.ID, related.ID, closed.ID} {
		if strings.Contains(out, id+" -[") {
			t.Errorf("preflight names %s, whose external reference is held, a capability, not blocking, or on a closed row: %s", id, out)
		}
	}
}
